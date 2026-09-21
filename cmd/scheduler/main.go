package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/slava/scheduler/internal/api"
	"github.com/slava/scheduler/internal/bus"
	"github.com/slava/scheduler/internal/config"
	"github.com/slava/scheduler/internal/cron"
	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/executor"
	"github.com/slava/scheduler/internal/logging"
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
	if n, err := store.ReconcileOrphanedRuns(); err != nil {
		logger.LogError("startup_error", fmt.Sprintf("Failed to reconcile orphaned runs: %v", err), nil)
		log.Fatalf("reconcile orphaned runs: %v", err)
	} else if n > 0 {
		log.Printf("reconciled %d orphaned run(s) left behind by a previous process", n)
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
