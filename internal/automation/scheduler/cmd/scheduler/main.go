// scheduler is the cron daemon itself: it opens the job database, builds the
// cron loop, serves the HTTP API and answers the bus, and shuts all four down
// on a signal.
//
// The whole binary is wiring, and wiring is where a daemon's real decisions
// live — which failures stop a boot and which are only a warning, what happens
// when the bus is down, and what runs before the first job can tick. Those
// decisions are pinned by tests, so the run is hoisted out of main() into
// runScheduler, which is a function of its arguments:
//
//   - cfg and settings are the resolved configuration, read once in main.
//   - deps carries the seams that reach outside the process. Anything that
//     opens a connection is injected as a FUNCTION rather than as a value, so a
//     path that is supposed to open none still opens none.
//
// main() keeps the same contract it always had: every failure runScheduler
// returns is fatal here. The one visible consequence of the hoist is that a
// failure now unwinds through the shutdown defers before the process exits,
// where log.Fatalf inside the serving goroutine used to skip them.
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/slava/scheduler/internal/api"
	"github.com/slava/scheduler/internal/bridgeauth"
	"github.com/slava/scheduler/internal/bus"
	"github.com/slava/scheduler/internal/config"
	cronpkg "github.com/slava/scheduler/internal/cron"
	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/eventtriggerconsumer"
	"github.com/slava/scheduler/internal/executor"
	"github.com/slava/scheduler/internal/logging"
	"github.com/slava/scheduler/internal/modelstoreroles"
	"github.com/slava/scheduler/internal/runstatus"
	"github.com/slava/scheduler/internal/sessionmodelrole"
)

// natsClientName is how this process identifies itself on the bus.
const natsClientName = "scheduler"

// busConnection is the process's one NATS connection. runScheduler holds it
// through this interface so a test can take the connected path without a live
// NATS server; in production it is a *natsbus.Client.
type busConnection interface {
	Close()
}

// busService is the bus request handler. It is both the cron loop's publisher
// and a subscriber with its own lifecycle.
type busService interface {
	cronpkg.BusHandler
	Start() error
	Close()
}

// deps are the seams that reach outside the process. Everything not listed here
// — the store's queries, the cron loop, the API server — is built from local
// state and is used directly.
type deps struct {
	// connectNATS opens the shared bus connection. A failure is only a warning
	// here; see runScheduler for why the boot stops anyway.
	connectNATS func(url, name string) (busConnection, error)
	// newLogger builds the event logger over whatever connectNATS left behind.
	// It is nil-safe on a bus-less process, so it is called either way.
	newLogger func() *logging.Logger
	// newAgentExecutor builds the executor an "agent" job runs through. It is
	// called only when the connection opened, which is the whole of that
	// decision — an agent job has nowhere to send its request otherwise.
	newAgentExecutor func() *executor.AgentExecutor
	// subscribeEventTriggers starts the consumers that run triggered jobs. It
	// is called only when the connection opened.
	subscribeEventTriggers func(store *db.Store, scheduler *cronpkg.Scheduler) error
	// openStore opens the job database.
	openStore func(path string) (*db.Store, error)
	// newBusHandler builds the bus request handler.
	newBusHandler func(natsURL string, store *db.Store, scheduler *cronpkg.Scheduler, logger *logging.Logger) (busService, error)
	// listenAndServe runs the HTTP API. It returns only when serving stops.
	listenAndServe func(addr string, handler http.Handler) error
	// signals delivers the shutdown signal.
	signals <-chan os.Signal
	// logf is the operator log — the plain lines a human reads in journalctl.
	logf func(format string, v ...any)
}

func main() {
	settings, err := config.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("settings: %v", err)
	}
	cfg := config.Load(settings)
	// Agent jobs call llm-bridge-server, which answers 401 to a request
	// without the service token.
	if err := bridgeauth.StampRequestsToBridge(cfg.BridgeURL, cfg.BridgeServiceToken); err != nil {
		log.Fatalf("bridge auth: %v", err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	if err := runScheduler(cfg, settings, productionDeps(cfg, settings, sig)); err != nil {
		log.Fatalf("%v", err)
	}
}

// productionDeps wires the real world. The NATS connection is captured here
// rather than passed back through the seams, so nothing downstream has to
// assert its way from the interface back to the concrete client.
func productionDeps(cfg config.Config, settings *servicesettings.Registry, signals <-chan os.Signal) deps {
	var client *natsbus.Client
	return deps{
		connectNATS: func(url, name string) (busConnection, error) {
			c, err := natsbus.Connect(natsbus.Options{URL: url, Name: name})
			if err != nil {
				return nil, err
			}
			client = c
			return c, nil
		},
		newLogger: func() *logging.Logger { return logging.New(client) },
		newAgentExecutor: func() *executor.AgentExecutor {
			return executor.NewAgentExecutor(client, cfg.BridgeURL, cfg.NoteboardURL,
				func() string { return settings.ModelRole(config.SettingAgentJobModelRole) },
				sessionmodelrole.New(cfg.ModelStoreURL, cfg.BridgeURL))
		},
		subscribeEventTriggers: func(store *db.Store, scheduler *cronpkg.Scheduler) error {
			consumer := &eventtriggerconsumer.Consumer{Store: store, Starter: scheduler}
			return consumer.Subscribe(client)
		},
		openStore: db.New,
		newBusHandler: func(natsURL string, store *db.Store, scheduler *cronpkg.Scheduler, logger *logging.Logger) (busService, error) {
			return bus.NewHandler(natsURL, store, scheduler, logger, modelstoreroles.New(cfg.ModelStoreURL).CheckRole)
		},
		listenAndServe: http.ListenAndServe,
		signals:        signals,
		logf:           log.Printf,
	}
}

// runScheduler boots the daemon and blocks until it is told to stop. It returns
// nil on an ordinary shutdown and an error on every failure that should end the
// process; main fatals on those.
func runScheduler(cfg config.Config, settings *servicesettings.Registry, d deps) error {
	// A failed connect here is only a warning, but the bus handler below dials
	// the same address and stops the start when it cannot, so in effect the
	// scheduler does not run without NATS.
	conn, err := d.connectNATS(cfg.NATSURL, natsClientName)
	if err != nil {
		d.logf("warning: NATS connect failed: %v", err)
		conn = nil
	}

	logger := d.newLogger()
	logger.LogSchedulerLifecycle("starting")

	store, err := d.openStore(cfg.DatabasePath)
	if err != nil {
		logger.LogError("startup_error", fmt.Sprintf("Failed to initialize database: %v", err), nil)
		return fmt.Errorf("db: %w", err)
	}
	defer store.Close()

	if err := attachStoredSettings(settings, store, modelstoreroles.New(cfg.ModelStoreURL)); err != nil {
		logger.LogError("startup_error", err.Error(), nil)
		return err
	}

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
		return fmt.Errorf("reconcile orphaned runs: %w", err)
	}
	publishInterruptedRuns(logger, interruptedRuns)
	if len(interruptedRuns) > 0 {
		d.logf("reconciled %d orphaned run(s) left behind by a previous process", len(interruptedRuns))
	}

	scheduler := cronpkg.New(store)
	scheduler.SetLogger(logger)

	if conn != nil {
		scheduler.SetAgentExecutor(d.newAgentExecutor())
		d.logf("agent executor configured (NATS)")
	}

	// The bus handler opens a connection of its own to the same address.
	busHandler, err := d.newBusHandler(cfg.NATSURL, store, scheduler, logger)
	if err != nil {
		logger.LogError("startup_error", fmt.Sprintf("Failed to initialize bus handler: %v", err), nil)
		return fmt.Errorf("bus: %w", err)
	}
	scheduler.SetBusHandler(busHandler)

	if err := scheduler.Start(); err != nil {
		logger.LogError("startup_error", fmt.Sprintf("Failed to start scheduler: %v", err), nil)
		return fmt.Errorf("cron: %w", err)
	}
	defer func() {
		scheduler.Stop()
		busHandler.Close()
		if conn != nil {
			conn.Close()
		}
		logger.LogSchedulerLifecycle("stopped")
	}()

	// Subscribing to the bus is not worth a boot. Jobs still tick and the API
	// still answers without it.
	if err := busHandler.Start(); err != nil {
		logger.LogError("startup_error", fmt.Sprintf("Failed to start bus handler: %v", err), nil)
		d.logf("warning: bus handler failed to start: %v", err)
	}
	// Nor are event triggers: without them no triggered job runs, but every
	// scheduled job still does.
	if conn != nil {
		if err := d.subscribeEventTriggers(store, scheduler); err != nil {
			logger.LogError("startup_error", fmt.Sprintf("Failed to subscribe event triggers; no triggered job will run: %v", err), nil)
			d.logf("warning: event triggers failed to subscribe; no triggered job will run: %v", err)
		}
	}

	srv := api.New(store, scheduler, cfg.Token, settings, modelstoreroles.New(cfg.ModelStoreURL).CheckRole)
	srv.SetLogger(logger)

	addr := net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.Port))
	served := make(chan error, 1)
	go func() {
		d.logf("scheduler listening on %s", addr)
		logger.LogSchedulerLifecycle("started", fmt.Sprintf("listening on %s", addr))
		served <- d.listenAndServe(addr, srv)
	}()

	select {
	case err := <-served:
		logger.LogError("http_error", fmt.Sprintf("HTTP server error: %v", err), nil)
		return fmt.Errorf("http: %w", err)
	case <-d.signals:
		d.logf("shutting down")
		logger.LogSchedulerLifecycle("shutting_down")
		return nil
	}
}

// attachStoredSettings makes the database the record of the service's editable
// settings — the model role each job binary asks for — and checks every new
// role with model-store before it is stored. A job binary reads its role from
// GET /settings when it starts, so a PUT takes effect on its next run.
func attachStoredSettings(settings *servicesettings.Registry, store *db.Store, roles modelstoreroles.Checker) error {
	if err := settings.AttachStoredValues(store.ServiceSettingValues(), nil); err != nil {
		return fmt.Errorf("stored settings: %w", err)
	}
	for _, key := range config.ModelRoleSettingKeys {
		settings.SetValidator(key, roles.CheckRole)
	}
	return nil
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
