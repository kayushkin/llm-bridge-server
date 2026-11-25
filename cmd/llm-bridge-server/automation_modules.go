package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	natsbus "github.com/kayushkin/bus"

	"github.com/kayushkin/llm-bridge-server/internal/automation/rulestore"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/api"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/bus"
	schedulerconfig "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/config"
	schedulercron "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/cron"
	schedulerdb "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/eventtriggerconsumer"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/executor"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/logging"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/modelstoreroles"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/runstatus"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/sessionmodelrole"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/server"
)

// schedulerNATSClientName is how the scheduler names itself on NATS.
const schedulerNATSClientName = "llm-bridge-server scheduler"

func openSchedulerModule(ctx context.Context, url string) (*sql.DB, error) {
	module, err := schedulerdb.Module()
	if err != nil {
		return nil, err
	}
	return openPostgresModule(ctx, url, module)
}

// enableAutomation opens the scheduler's and rule-store's modules in Postgres
// and serves them from srv at /scheduler/ and /rule-store/. Neither acts on
// its own unless its setting says so: scheduler.runs.enabled starts the cron
// ticker, run now, the bus commands and event triggers, and
// rule_store.reactor.enabled starts the reactor. While the standalone
// scheduler and rule-store run, a second process acting on the same jobs and
// rules would run every job and decide every rule twice. The returned
// function stops both and closes the pools.
func enableAutomation(ctx context.Context, cfg *config.Config, srv *server.Server) (func(), error) {
	closeScheduler, err := enableScheduler(ctx, cfg, srv)
	if err != nil {
		return nil, err
	}
	closeRuleStore, err := enableRuleStore(ctx, cfg, srv)
	if err != nil {
		closeScheduler()
		return nil, err
	}
	return func() {
		closeRuleStore()
		closeScheduler()
	}, nil
}

func enableScheduler(ctx context.Context, cfg *config.Config, srv *server.Server) (func(), error) {
	settings := cfg.Settings
	runsEnabled := settings.Boolean(config.SettingSchedulerRunsEnabled)
	natsURL := settings.String(config.SettingSchedulerNATSURL)
	noteboardURL := settings.String(config.SettingSchedulerNoteboardURL)
	if runsEnabled && natsURL != "" && noteboardURL == "" {
		return nil, fmt.Errorf("scheduler: %s is on and %s is set, so agent jobs run, but %s is not set: their prompts would name no workspace address",
			config.SettingSchedulerRunsEnabled, config.SettingSchedulerNATSURL, config.SettingSchedulerNoteboardURL)
	}

	pool, err := openSchedulerModule(ctx, cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("scheduler: %w", err)
	}
	store := schedulerdb.New(pool)
	var closers []func()
	closeAll := func() {
		for index := len(closers) - 1; index >= 0; index-- {
			closers[index]()
		}
		if err := pool.Close(); err != nil {
			log.Printf("postgres: closing the scheduler module: %v", err)
		}
	}
	fail := func(err error) (func(), error) {
		closeAll()
		return nil, fmt.Errorf("scheduler: %w", err)
	}

	// Model roles are checked with model-store, and agent jobs read sessions,
	// instances and harnesses, by direct calls into this process.
	roles := modelstoreroles.Checker{Roles: srv.SchedulerModelStoreRoles()}

	storedSettings, err := schedulerconfig.NewStoredSettingsRegistry()
	if err != nil {
		return fail(err)
	}
	if err := storedSettings.AttachStoredValues(store.ServiceSettingValues(), nil); err != nil {
		return fail(fmt.Errorf("stored settings: %w", err))
	}
	for _, key := range schedulerconfig.ModelRoleSettingKeys {
		storedSettings.SetValidator(key, roles.CheckRole)
	}

	scheduler := schedulercron.New(store)
	logger := logging.New(nil)
	if !runsEnabled {
		scheduler.SwitchRunsOff(config.SettingSchedulerRunsEnabled + " is off on this server")
	} else {
		// Any run still marked running belongs to a process that is gone: this
		// one has not started a job yet.
		var natsClient *natsbus.Client
		if natsURL != "" {
			if natsClient, err = natsbus.Connect(natsbus.Options{URL: natsURL, Name: schedulerNATSClientName}); err != nil {
				return fail(fmt.Errorf("%s: %w", config.SettingSchedulerNATSURL, err))
			}
			closers = append(closers, natsClient.Close)
			logger = logging.New(natsClient)
		}
		interrupted, err := store.ReconcileOrphanedRuns()
		if err != nil {
			return fail(fmt.Errorf("closing the runs the last process left going: %w", err))
		}
		for _, run := range interrupted {
			logger.LogJobFinish(run.JobID, run.JobName, run.Schedule, run.RunID, runstatus.Interrupted, run.Output, run.FinishedAt.Sub(run.StartedAt))
		}
		log.Printf("scheduler: runs are on; %d runs were going when the last process stopped, and are interrupted", len(interrupted))
		scheduler.SetLogger(logger)
		if natsClient != nil {
			agents := executor.NewAgentExecutor(natsClient, srv.SchedulerBridge(), noteboardURL,
				func() string { return storedSettings.ModelRole(schedulerconfig.SettingAgentJobModelRole) },
				sessionmodelrole.SourcesChecker{Sources: srv.SchedulerModelRoleSources()})
			scheduler.SetAgentExecutor(agents)
			busHandler, err := bus.NewHandler(natsURL, store, scheduler, logger, roles.CheckRole)
			if err != nil {
				return fail(fmt.Errorf("bus commands: %w", err))
			}
			closers = append(closers, busHandler.Close)
			scheduler.SetBusHandler(busHandler)
			if err := busHandler.Start(); err != nil {
				return fail(fmt.Errorf("bus commands: %w", err))
			}
			consumer := &eventtriggerconsumer.Consumer{Store: store, Starter: scheduler}
			if err := consumer.Subscribe(natsClient); err != nil {
				return fail(fmt.Errorf("event triggers: %w", err))
			}
		} else {
			log.Printf("scheduler: %s is not set, so only shell jobs run: an agent job's run ends in error, and no event trigger or bus command is answered", config.SettingSchedulerNATSURL)
		}
	}
	if err := scheduler.Start(); err != nil {
		return fail(err)
	}
	closers = append(closers, scheduler.Stop)

	routes := api.New(store, scheduler, "", storedSettings, roles.CheckRole)
	routes.SetLogger(logger)
	srv.EnableScheduler(routes)
	srv.EnableSchedulerJobReferences(routes)
	if runsEnabled {
		log.Printf("postgres: scheduler open, served at /scheduler/; jobs run here")
	} else {
		log.Printf("postgres: scheduler open, served at /scheduler/; %s is off, so no job runs here", config.SettingSchedulerRunsEnabled)
	}
	return closeAll, nil
}

// importSchedulerCmd copies a standalone scheduler's SQLite file into the
// scheduler module, keeping every job id, hold and run it copies, and prints
// how many rows it copied.
func importSchedulerCmd(args []string) {
	flags := flag.NewFlagSet("import-scheduler", flag.ExitOnError)
	sqlitePath := flags.String("sqlite", "", "the standalone scheduler's scheduler.db to copy (a copy of it, made with sqlite3 .backup, while the scheduler runs)")
	newestRunsPerJob := flags.Int("newest-runs-per-job", 0, "copy only each job's newest runs, this many; 0 copies every run. 100 or more answers every route as the standalone scheduler did")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if *sqlitePath == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: llm-bridge-server import-scheduler -sqlite <file> [-newest-runs-per-job <n>]")
		os.Exit(2)
	}
	ctx := context.Background()
	pool, err := openSchedulerModule(ctx, importPostgresURL())
	if err != nil {
		importFailed(err)
	}
	defer pool.Close()
	counts, err := schedulerdb.ImportStandaloneDatabase(ctx, pool, *sqlitePath, schedulerdb.ImportOptions{NewestRunsPerJob: *newestRunsPerJob})
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	fmt.Printf("jobs:                     %d\n", counts.Jobs)
	fmt.Printf("runs:                     %d (left behind: %d)\n", counts.Runs, counts.RunsLeftBehind)
	fmt.Printf("service_settings:         %d\n", counts.ServiceSettings)
	fmt.Printf("event_trigger_deliveries: %d\n", counts.EventTriggerDeliveries)
	fmt.Printf("numbering goes on after: job %d, run %d\n", counts.LastJobNumber, counts.LastRunNumber)
}

// ruleStoreEffectCallTimeout bounds one call a live rule makes to an owner.
const ruleStoreEffectCallTimeout = 30 * time.Second

func openRuleStoreModule(ctx context.Context, url string) (*sql.DB, error) {
	module, err := rulestore.Module()
	if err != nil {
		return nil, err
	}
	return openPostgresModule(ctx, url, module)
}

// enableRuleStore opens rule-store's module and serves it at /rule-store/. It
// reads sessions from this server and cards and entity tags from kanban-store,
// both in this process as the internal service, as the standalone rule-store
// read them with the two service tokens; so it runs after kanban-store is
// enabled. The reactor runs only while rule_store.reactor.enabled is on.
func enableRuleStore(ctx context.Context, cfg *config.Config, srv *server.Server) (func(), error) {
	settings := cfg.Settings
	reactorEnabled := settings.Boolean(config.SettingRuleStoreReactorEnabled)
	sweepInterval := settings.Duration(config.SettingRuleStoreSweepInterval)
	if reactorEnabled && sweepInterval <= 0 {
		return nil, fmt.Errorf("rule-store: %s is %s; it must be longer than nothing", config.SettingRuleStoreSweepInterval, sweepInterval)
	}
	kanbanClient, err := srv.KanbanStoreHTTPClientAsInternalService()
	if err != nil {
		return nil, fmt.Errorf("rule-store: %w", err)
	}
	pool, err := openRuleStoreModule(ctx, cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("rule-store: %w", err)
	}
	store := rulestore.New(pool)

	// Sessions, cards and entity tags are read through the stores' Go
	// methods. The session feed is this server's /session-events, a stream
	// read through its own handler, and a live rule's calls are HTTP-shaped
	// by design (each names a method, a path and a body, and its run records
	// the answer's status), so both still go through this server's and
	// kanban-store's handlers in this process, as the internal service.
	bridgeBaseURL, _ := srv.HTTPClientToThisServer(0)
	_, bridgeStream := srv.HTTPClientToThisServer(0)
	feed := &rulestore.LLMBridge{BaseURL: bridgeBaseURL, Stream: bridgeStream}
	sessions := srv.RuleStoreSessions(feed)
	kanban := srv.RuleStoreKanban()
	_, effectsOnThisServer := srv.HTTPClientToThisServer(0)
	owners := rulestore.NewOwnerCalls(feed, &rulestore.KanbanStore{BaseURL: server.KanbanStoreInProcessBaseURL})
	owners.Client = &http.Client{Timeout: ruleStoreEffectCallTimeout, Transport: transportByHost{
		hostOf(bridgeBaseURL):                      effectsOnThisServer.Transport,
		hostOf(server.KanbanStoreInProcessBaseURL): kanbanClient.Transport,
	}}
	reactor := rulestore.NewReactor(store, sessions, kanban, owners, sweepInterval)

	lifetime, stop := context.WithCancel(ctx)
	if reactorEnabled {
		// A live run left applying by the last process stopped mid-way. Its
		// calls are not made again: the run says which answered.
		interrupted, err := store.MarkInterruptedRuns()
		if err != nil {
			stop()
			pool.Close()
			return nil, fmt.Errorf("rule-store: marking the runs the last process left applying: %w", err)
		}
		go reactor.Run(lifetime)
		log.Printf("rule-store: the reactor runs here, sweeping every %s; %d live runs were applying when the last process stopped, and are interrupted", sweepInterval, interrupted)
	} else {
		reactor.SwitchOff(config.SettingRuleStoreReactorEnabled + " is off on this server")
	}
	routes := http.NewServeMux()
	rulestore.RegisterHandlers(lifetime, routes, store, reactor, kanban)
	srv.EnableRuleStore(routes)
	if reactorEnabled {
		log.Printf("postgres: rule-store open, served at /rule-store/; its reactor runs here")
	} else {
		log.Printf("postgres: rule-store open, served at /rule-store/; %s is off, so no rule decides here", config.SettingRuleStoreReactorEnabled)
	}
	return func() {
		stop()
		if err := pool.Close(); err != nil {
			log.Printf("postgres: closing the rule-store module: %v", err)
		}
	}, nil
}

// transportByHost carries each request with the transport of its URL's host,
// so one client reaches both owners a live rule calls, each in this process.
type transportByHost map[string]http.RoundTripper

func (transports transportByHost) RoundTrip(request *http.Request) (*http.Response, error) {
	transport, known := transports[request.URL.Host]
	if !known {
		return nil, fmt.Errorf("rule-store: no owner in this process answers %s", request.URL.Host)
	}
	return transport.RoundTrip(request)
}

func hostOf(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		panic(fmt.Sprintf("the in-process base URL %q does not parse: %v", baseURL, err))
	}
	return parsed.Host
}

// importRuleStoreCmd copies a standalone rule-store's SQLite file into its
// module, keeping every id, and prints how many rows it copied.
func importRuleStoreCmd(args []string) {
	path := importArguments("import-rule-store", "rule-store", args)
	ctx := context.Background()
	pool, err := openRuleStoreModule(ctx, importPostgresURL())
	if err != nil {
		importFailed(err)
	}
	defer pool.Close()
	counts, err := rulestore.ImportStandaloneDatabase(ctx, pool, path)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	fmt.Printf("rules:            %d\n", counts.Rules)
	fmt.Printf("rule_revisions:   %d\n", counts.Revisions)
	fmt.Printf("rule_activations: %d\n", counts.Activations)
	fmt.Printf("subjects:         %d\n", counts.Subjects)
	fmt.Printf("rule_runs:        %d\n", counts.Runs)
}
