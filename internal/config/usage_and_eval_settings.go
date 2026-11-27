package config

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The settings of usage-store and eval-store, which run in this process from
// their Postgres modules. Read them through cfg.Settings with these keys. Each
// is read once, at start.
const (
	SettingUsageStoreLimitsRefreshEnabled         = "usage_store.limits_refresh.enabled"
	SettingUsageStoreLimitsRefreshIdleInterval    = "usage_store.limits_refresh.idle_interval"
	SettingUsageStoreLimitsRefreshWatchedInterval = "usage_store.limits_refresh.watched_interval"
	SettingUsageStoreLimitsRefreshWatcherExpiry   = "usage_store.limits_refresh.watcher_expiry"
	SettingUsageStoreCodexCommand                 = "usage_store.codex_command"
	SettingUsageStoreCodexTimeout                 = "usage_store.codex_timeout"
	SettingUsageStoreSpendRefreshEnabled          = "usage_store.spend_refresh.enabled"
	SettingUsageStoreSpendRefreshInterval         = "usage_store.spend_refresh.interval"

	SettingEvalStoreRunsEnabled              = "eval_store.runs.enabled"
	SettingEvalStoreOrganizationID           = "eval_store.organization_id"
	SettingEvalStoreDefaultConcurrency       = "eval_store.default_concurrency"
	SettingEvalStoreDefaultBudgetLimits      = "eval_store.default_budget_limits"
	SettingEvalStoreBudgetCheckEveryCalls    = "eval_store.budget_check_every_calls"
	SettingEvalStoreAnswerTimeout            = "eval_store.answer_timeout"
	SettingEvalStoreJudgeTimeout             = "eval_store.judge_timeout"
	SettingEvalStoreOperationPollingInterval = "eval_store.operation_polling_interval"
)

// usageAndEvalSettingDefinitions declares the settings above.
func usageAndEvalSettingDefinitions() []servicesettings.Definition {
	const (
		wiring   = msg.ServiceSettingKindWiring
		behavior = msg.ServiceSettingKindBehavior
		text     = msg.ServiceSettingValueTypeString
		boolean  = msg.ServiceSettingValueTypeBoolean
		duration = msg.ServiceSettingValueTypeDuration
		integer  = msg.ServiceSettingValueTypeInteger
	)
	return []servicesettings.Definition{
		{Key: SettingUsageStoreLimitsRefreshEnabled, EnvironmentVariable: "LLMBRIDGE_USAGE_STORE_LIMITS_REFRESH_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Read every subscription's limits into usage-store in this server: Anthropic's OAuth usage with the login in this user's ~/.claude/.credentials.json, and Codex's rate limits from codex app-server when usage_store.codex_command is set. Off by default, while the standalone usage-store still reads them; the reads belong to bridge-worker once it exists. Off, the limits the one-shot router and the usage page see go stale. Read at start."},
		{Key: SettingUsageStoreLimitsRefreshIdleInterval, EnvironmentVariable: "LLMBRIDGE_USAGE_STORE_LIMITS_REFRESH_IDLE_INTERVAL", Kind: behavior, ValueType: duration, Default: "15m",
			Description: "How long the limits refresher waits between reads of a provider while nobody has the usage page open. A snapshot is stale three of these after it was taken. Anthropic's usage endpoint shares its rate limit with every Claude Code process on the login, so never set it short. Read at start."},
		{Key: SettingUsageStoreLimitsRefreshWatchedInterval, EnvironmentVariable: "LLMBRIDGE_USAGE_STORE_LIMITS_REFRESH_WATCHED_INTERVAL", Kind: behavior, ValueType: duration, Default: "2m",
			Description: "How long the limits refresher waits between reads while the usage page is open (POST /usage/api/usage/limits/watch). A failed read waits this long, doubling up to the idle interval. Read at start."},
		{Key: SettingUsageStoreLimitsRefreshWatcherExpiry, EnvironmentVariable: "LLMBRIDGE_USAGE_STORE_LIMITS_REFRESH_WATCHER_EXPIRY", Kind: behavior, ValueType: duration, Default: "3m",
			Description: "How long one POST /usage/api/usage/limits/watch keeps the watched interval. Read at start."},
		{Key: SettingUsageStoreCodexCommand, EnvironmentVariable: "LLMBRIDGE_USAGE_STORE_CODEX_COMMAND", Kind: wiring, ValueType: text,
			Description: "The Codex command usage-store starts as codex app-server to read Codex's rate limits: a path, or a name on PATH (the npm codex needs node on PATH too). Empty switches Codex limits off. Read at start."},
		{Key: SettingUsageStoreCodexTimeout, EnvironmentVariable: "LLMBRIDGE_USAGE_STORE_CODEX_TIMEOUT", Kind: behavior, ValueType: duration, Default: "30s",
			Description: "The longest one codex app-server read of Codex's rate limits may take. Read at start."},
		{Key: SettingUsageStoreSpendRefreshEnabled, EnvironmentVariable: "LLMBRIDGE_USAGE_STORE_SPEND_REFRESH_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Read what each Anthropic API key cost into usage-store in this server, every usage_store.spend_refresh.interval. It finds the admin key among auth-store's Anthropic credentials at start, with auth_store.url and auth_store.token, and every resolve shows in auth-store's audit trail. Off by default, while the standalone usage-store still reads it. Read at start."},
		{Key: SettingUsageStoreSpendRefreshInterval, EnvironmentVariable: "LLMBRIDGE_USAGE_STORE_SPEND_REFRESH_INTERVAL", Kind: behavior, ValueType: duration, Default: "1h",
			Description: "How long the spend refresher waits between reads of per-key spend. Read at start."},

		{Key: SettingEvalStoreRunsEnabled, EnvironmentVariable: "LLMBRIDGE_EVAL_STORE_RUNS_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Carry out eval-store runs in this server: POST /evaluations/runs/{id}/start and /resume make their model calls as llm.completion operations here. Off, both answer 503 and every other eval-store route still answers. Off by default, while the standalone eval-store still runs them; runs belong to bridge-worker once it exists. On, a run this server left going when it last stopped is marked interrupted at start. Read at start."},
		{Key: SettingEvalStoreOrganizationID, EnvironmentVariable: "LLMBRIDGE_EVAL_STORE_ORGANIZATION_ID", Kind: wiring, ValueType: text,
			Description: "The principal-store group an eval-store run's calls are billed to when the run names none; its monthly operation budget pays for them. Empty, a run that names none refuses to start. Read at start."},
		{Key: SettingEvalStoreDefaultConcurrency, EnvironmentVariable: "LLMBRIDGE_EVAL_STORE_DEFAULT_CONCURRENCY", Kind: behavior, ValueType: integer, Default: "4",
			Description: "How many model calls an eval-store run makes at once when it names no concurrency. Read at start."},
		{Key: SettingEvalStoreDefaultBudgetLimits, EnvironmentVariable: "LLMBRIDGE_EVAL_STORE_DEFAULT_BUDGET_LIMITS", Kind: behavior, ValueType: msg.ServiceSettingValueTypeStringMap, Default: "codex.weekly:60,anthropic.seven_day:40",
			Description: "subscription.window:percent pairs an eval-store run gets when it names no budget limits. The run stops, resumable, once usage-store reports any of them above its percent. Read at start."},
		{Key: SettingEvalStoreBudgetCheckEveryCalls, EnvironmentVariable: "LLMBRIDGE_EVAL_STORE_BUDGET_CHECK_EVERY_CALLS", Kind: behavior, ValueType: integer, Default: "8",
			Description: "How many calls an eval-store run makes between budget readings. A run may overshoot a limit by this many calls. Read at start."},
		{Key: SettingEvalStoreAnswerTimeout, EnvironmentVariable: "LLMBRIDGE_EVAL_STORE_ANSWER_TIMEOUT", Kind: behavior, ValueType: duration, Default: "10m",
			Description: "How long a candidate's call may take before its operation is asked to stop and the answer is recorded as failed. Read at start."},
		{Key: SettingEvalStoreJudgeTimeout, EnvironmentVariable: "LLMBRIDGE_EVAL_STORE_JUDGE_TIMEOUT", Kind: behavior, ValueType: duration, Default: "15m",
			Description: "How long a judge's call may take before its operation is asked to stop and the verdict is recorded as failed. Read at start."},
		{Key: SettingEvalStoreOperationPollingInterval, EnvironmentVariable: "LLMBRIDGE_EVAL_STORE_OPERATION_POLLING_INTERVAL", Kind: behavior, ValueType: duration, Default: "2s",
			Description: "How often a waiting eval-store call reads its operation's receipt. Read at start."},
	}
}
