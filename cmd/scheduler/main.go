package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/slava/scheduler/internal/api"
	"github.com/slava/scheduler/internal/bus"
	"github.com/slava/scheduler/internal/config"
	"github.com/slava/scheduler/internal/cron"
	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/executor"
	"github.com/slava/scheduler/internal/logging"
	"github.com/slava/scheduler/internal/runstatus"
)

func main() {
	settings, err := config.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("settings: %v", err)
	}
	cfg := config.Load(settings)

	// A failed connect here is only a warning, but the bus handler below dials
	// the same address and stops the start when it cannot, so in effect the
	// scheduler does not run without NATS.
	var natsClient *natsbus.Client
	nc, err := natsbus.Connect(natsbus.Options{URL: cfg.NATSURL, Name: "scheduler"})
	if err != nil {
		log.Printf("warning: NATS connect failed: %v", err)
	} else {
		natsClient = nc
	}

	// Initialize logging (uses NATS client, nil-safe)
	logger := logging.New(natsClient)
	logger.LogSchedulerLifecycle("starting")

	// Initialize database
	store, err := db.New(cfg.DatabasePath)
	if err != nil {
		logger.LogError("startup_error", fmt.Sprintf("Failed to initialize database: %v", err), nil)
		log.Fatalf("db: %v", err)
	}
	defer store.Close()

	// Any run still marked "running" belongs to a process that is now gone —
	// this one has not started a job yet. Close them out here rather than in
	// Scheduler.Start, which Reload also calls, and which would therefore kill
	// live runs on every job edit.
	//
	// Each closed run is published as a finish, at the same level and in the
	// same shape as a run that ended on its own terms. Reconciliation does not
	// go through the cron loop, so this is the only place these finishes can be
	// reported, and until they were they existed solely as rows in SQLite.
	interruptedRuns, err := store.ReconcileOrphanedRuns()
	if err != nil {
		logger.LogError("startup_error", fmt.Sprintf("Failed to reconcile orphaned runs: %v", err), nil)
		log.Fatalf("reconcile orphaned runs: %v", err)
	}
	publishInterruptedRuns(logger, interruptedRuns)
	if len(interruptedRuns) > 0 {
		log.Printf("reconciled %d orphaned run(s) left behind by a previous process", len(interruptedRuns))
	}

	// Initialize scheduler
	scheduler := cron.New(store)
	scheduler.SetLogger(logger)

	// Wire up agent executor if NATS is available
	if natsClient != nil {
		agentExec := executor.NewAgentExecutor(natsClient, cfg.BridgeURL, cfg.NoteboardURL)
		scheduler.SetAgentExecutor(agentExec)
		log.Println("agent executor configured (NATS)")
	}

	// The bus handler opens a connection of its own to the same address.
	busHandler, err := bus.NewHandler(cfg.NATSURL, store, scheduler, logger)
	if err != nil {
		logger.LogError("startup_error", fmt.Sprintf("Failed to initialize bus handler: %v", err), nil)
		log.Fatalf("bus: %v", err)
	}
	scheduler.SetBusHandler(busHandler)

	// Start scheduler
	if err := scheduler.Start(); err != nil {
		logger.LogError("startup_error", fmt.Sprintf("Failed to start scheduler: %v", err), nil)
		log.Fatalf("cron: %v", err)
	}
	defer func() {
		scheduler.Stop()
		busHandler.Close()
		if natsClient != nil {
			natsClient.Close()
		}
		logger.LogSchedulerLifecycle("stopped")
	}()

	// Start bus handler
	if err := busHandler.Start(); err != nil {
		logger.LogError("startup_error", fmt.Sprintf("Failed to start bus handler: %v", err), nil)
		log.Printf("warning: bus handler failed to start: %v", err)
	}

	// Initialize API server
	srv := api.New(store, scheduler, cfg.Token, settings)
	srv.SetLogger(logger)

	// Start HTTP server
	go func() {
		addr := fmt.Sprintf(":%d", cfg.Port)
		log.Printf("scheduler listening on %s", addr)
		logger.LogSchedulerLifecycle("started", fmt.Sprintf("listening on %s", addr))
		if err := http.ListenAndServe(addr, srv); err != nil {
			logger.LogError("http_error", fmt.Sprintf("HTTP server error: %v", err), nil)
			log.Fatalf("http: %v", err)
		}
	}()

	// Wait for shutdown signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
	logger.LogSchedulerLifecycle("shutting_down")
}

// interruptedRunLogger is the part of *logging.Logger that reporting a
// reconciled run needs. It is named here so the reporting can be exercised
// without a NATS connection.
type interruptedRunLogger interface {
	LogJobFinish(jobID int64, jobName, schedule string, runID int64, status, output string, duration time.Duration)
}

// publishInterruptedRuns reports every run that startup reconciliation closed,
// one entry each, through the same call the cron loop uses for a run that ended
// on its own terms.
//
// One entry per run rather than a summary: LogJobFinish's metadata is per-run
// (job id, job name, run id, duration), so a summary would have to invent a
// different shape, and a consumer counting finishes would miss these entirely.
// Measured on the live database before choosing — steady-state reconciliation
// closes 2 runs, and the one large batch on record (510) was the one-off
// backfill when reconciliation first ran against years of accumulated rows.
//
// The status is runstatus.Interrupted, which TerminalStatusIsFailure reports as
// a failure, so these publish at error level: a run that was killed is not news
// filed at info.
func publishInterruptedRuns(logger interruptedRunLogger, runs []db.InterruptedRun) {
	for _, r := range runs {
		logger.LogJobFinish(r.JobID, r.JobName, r.Schedule, r.RunID, runstatus.Interrupted,
			r.Output, r.FinishedAt.Sub(r.StartedAt))
	}
}
