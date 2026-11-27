package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/agents/evaluations"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models"
	"github.com/kayushkin/llm-bridge-server/internal/agents/usage"
	"github.com/kayushkin/llm-bridge-server/internal/agents/usage/anthropic"
	"github.com/kayushkin/llm-bridge-server/internal/agents/usage/codex"
	usagestorehttpapi "github.com/kayushkin/llm-bridge-server/internal/agents/usage/httpapi"
	"github.com/kayushkin/llm-bridge-server/internal/agents/usage/limitsrefresh"
	"github.com/kayushkin/llm-bridge-server/internal/agents/usage/spendrefresh"
	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/server"
)

// limitsStaleAfterIdleIntervals is how many idle intervals old a limits
// snapshot may be before the routes mark it stale: three missed reads in a
// row, as the standalone usage-store had it.
const limitsStaleAfterIdleIntervals = 3

// limitsRefreshCheckEvery is how often the limits refresher looks for a
// provider whose next read is due, as the standalone usage-store did.
const limitsRefreshCheckEvery = 15 * time.Second

// modelStoreTokenUsage answers usage-store's GET /api/usage from model-store's
// old daily usage rows.
type modelStoreTokenUsage struct {
	models *models.Store
}

func (reader modelStoreTokenUsage) TokenUsageOverTheLastDays(days int) ([]usagestorehttpapi.TokenUsageTotals, error) {
	totals, err := reader.models.TokenUsageOverTheLastDays(days)
	if err != nil {
		return nil, err
	}
	answered := make([]usagestorehttpapi.TokenUsageTotals, len(totals))
	for index, row := range totals {
		answered[index] = usagestorehttpapi.TokenUsageTotals{Agent: row.Agent, Orchestrator: row.Orchestrator, Model: row.Model,
			Messages: int(row.Requests), InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
			TotalTokens: row.InputTokens + row.OutputTokens, CostUSD: row.CostUSD}
	}
	return answered, nil
}

// enableUsageAndEvalStores opens usage-store and eval-store in Postgres and
// serves them from srv. usage-store's limits refresher and spend refresher run
// only when usage_store.limits_refresh.enabled and
// usage_store.spend_refresh.enabled are on, and eval-store carries runs out
// only when eval_store.runs.enabled is on: while the standalone services still
// run, two processes reading the same providers would spend their rate limits
// twice, and two runners would run one run twice. The returned function stops
// the loops and closes the pools.
func enableUsageAndEvalStores(ctx context.Context, cfg *config.Config, srv *server.Server, models *models.Store) (func(), error) {
	settings := cfg.Settings
	usageDB, err := openAgentsModule(ctx, cfg.PostgresURL, usage.Module)
	if err != nil {
		return nil, fmt.Errorf("usage-store: %w", err)
	}
	evalDB, err := openAgentsModule(ctx, cfg.PostgresURL, evaluations.Module)
	if err != nil {
		usageDB.Close()
		return nil, fmt.Errorf("eval-store: %w", err)
	}
	loops, stopLoops := context.WithCancel(ctx)
	var runner *evaluations.Runner
	closeAll := func() {
		stopLoops()
		if runner != nil {
			runner.Close()
		}
		if err := errors.Join(evalDB.Close(), usageDB.Close()); err != nil {
			log.Printf("postgres: closing usage-store and eval-store: %v", err)
		}
	}
	fail := func(err error) (func(), error) {
		closeAll()
		return nil, err
	}

	usageStore := usage.New(usageDB)
	idleInterval := settings.Duration(config.SettingUsageStoreLimitsRefreshIdleInterval)
	watchedInterval := settings.Duration(config.SettingUsageStoreLimitsRefreshWatchedInterval)
	watcherExpiry := settings.Duration(config.SettingUsageStoreLimitsRefreshWatcherExpiry)
	for key, interval := range map[string]time.Duration{config.SettingUsageStoreLimitsRefreshIdleInterval: idleInterval,
		config.SettingUsageStoreLimitsRefreshWatchedInterval: watchedInterval, config.SettingUsageStoreLimitsRefreshWatcherExpiry: watcherExpiry} {
		if interval <= 0 {
			return fail(fmt.Errorf("usage-store: %s is %s; it must be longer than nothing", key, interval))
		}
	}

	anthropicLimits := anthropic.New()
	var codexLimits *codex.Reader
	if command := settings.String(config.SettingUsageStoreCodexCommand); command == "" {
		log.Printf("usage-store: codex limits are off: %s is not set", config.SettingUsageStoreCodexCommand)
	} else if codexLimits, err = codex.New(command, settings.Duration(config.SettingUsageStoreCodexTimeout)); err != nil {
		log.Printf("usage-store: ERROR: codex limits are off: %v", err)
		codexLimits = nil
	}
	providers := []limitsrefresh.Provider{{
		Name:  "anthropic",
		Fetch: func(context.Context) (*usage.ProviderLimits, []byte, error) { return anthropicLimits.Fetch() },
	}}
	if codexLimits != nil {
		providers = append(providers, limitsrefresh.Provider{Name: "codex", Fetch: codexLimits.Read})
	}
	limitsRefresher := limitsrefresh.New(limitsrefresh.Intervals{Idle: idleInterval, Watched: watchedInterval, WatcherExpiry: watcherExpiry},
		usageStore.SaveLimits, providers)

	var collectors usagestorehttpapi.SpendCollectors
	spendRefreshEnabled := settings.Boolean(config.SettingUsageStoreSpendRefreshEnabled)
	spendInterval := settings.Duration(config.SettingUsageStoreSpendRefreshInterval)
	if spendRefreshEnabled {
		if spendInterval <= 0 {
			return fail(fmt.Errorf("usage-store: %s is %s; it must be longer than nothing", config.SettingUsageStoreSpendRefreshInterval, spendInterval))
		}
		if cfg.AuthStoreToken == "" {
			return fail(fmt.Errorf("usage-store: %s is on, and auth_store.token is not set, so the admin key cannot be found", config.SettingUsageStoreSpendRefreshEnabled))
		}
		collector, err := spendrefresh.DiscoverAnthropicCollector(ctx, authstoreclient.New(cfg.AuthStoreURL, cfg.AuthStoreToken, "usage-store"))
		switch {
		case err != nil:
			log.Printf("usage-store: ERROR: spend collection is off: %v", err)
		case collector == nil:
			log.Printf("usage-store: ERROR: spend collection is off: auth-store holds no Anthropic admin key")
		default:
			collectors.Anthropic = collector
		}
	}

	usageHandlers := usagestorehttpapi.New(usageStore, modelStoreTokenUsage{models: models}, anthropicLimits, codexLimits, limitsRefresher,
		time.Duration(limitsStaleAfterIdleIntervals)*idleInterval, collectors)
	srv.EnableUsageStore(usageHandlers)
	if settings.Boolean(config.SettingUsageStoreLimitsRefreshEnabled) {
		go limitsRefresher.Run(loops, limitsRefreshCheckEvery)
		log.Printf("usage-store: reading limits every %s, every %s while the usage page is open (codex: %t)", idleInterval, watchedInterval, codexLimits != nil)
	} else {
		log.Printf("usage-store: %s is off, so this server reads no limits and they go stale", config.SettingUsageStoreLimitsRefreshEnabled)
	}
	if collectors.Anthropic != nil {
		go spendrefresh.Run(loops, usageStore, usageHandlers, collectors.Anthropic, spendInterval)
		log.Printf("usage-store: reading per-key spend every %s", spendInterval)
	}

	defaultLimits, err := evaluations.ParseBudgetLimits(settings.StringMap(config.SettingEvalStoreDefaultBudgetLimits))
	if err != nil {
		return fail(fmt.Errorf("eval-store: %s: %w", config.SettingEvalStoreDefaultBudgetLimits, err))
	}
	evalStore := evaluations.New(evalDB)
	runsEnabled := settings.Boolean(config.SettingEvalStoreRunsEnabled)
	runner, err = srv.EnableEvalStore(evalStore, server.EvalStoreOptions{
		RunsEnabled:              runsEnabled,
		OperationPollingInterval: settings.Duration(config.SettingEvalStoreOperationPollingInterval),
		Defaults: evaluations.RunDefaults{
			Concurrency:           settings.Integer(config.SettingEvalStoreDefaultConcurrency),
			BudgetLimits:          defaultLimits,
			BudgetCheckEveryCalls: settings.Integer(config.SettingEvalStoreBudgetCheckEveryCalls),
			AnswerTimeout:         settings.Duration(config.SettingEvalStoreAnswerTimeout),
			JudgeTimeout:          settings.Duration(config.SettingEvalStoreJudgeTimeout),
			OrganizationID:        settings.String(config.SettingEvalStoreOrganizationID),
		},
	})
	if err != nil {
		return fail(err)
	}
	if runsEnabled {
		interrupted, err := evalStore.MarkInterruptedRuns()
		if err != nil {
			return fail(fmt.Errorf("eval-store: marking the runs the last process left going: %w", err))
		}
		log.Printf("eval-store: runs are on; %d runs were going when this server last stopped, and are interrupted and can be resumed", interrupted)
	} else {
		log.Printf("eval-store: %s is off, so runs cannot start or resume here", config.SettingEvalStoreRunsEnabled)
	}
	log.Printf("postgres: usage-store and eval-store open, served at /usage-store/ and /eval-store/")
	return closeAll, nil
}

func importUsageStoreCmd(args []string) {
	importAgentsStoreCmd("import-usage-store", args, usage.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
		return usage.ImportStandaloneDatabase(ctx, db, path)
	})
}

// importEvalStoreCmd copies a standalone eval-store's file into its module.
// Its cases hold real mail; the command prints only how many rows it copied.
func importEvalStoreCmd(args []string) {
	importAgentsStoreCmd("import-eval-store", args, evaluations.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
		return evaluations.ImportStandaloneDatabase(ctx, db, path)
	})
}
