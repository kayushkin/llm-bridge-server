package config

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The settings of usage-store and eval-store, which run in this process from
// their Postgres modules. Read them through cfg.Settings with these keys. Each
// is read once, at start.
const (
	SettingUsageLimitsRefreshEnabled         = "usage.limits_refresh.enabled"
	SettingUsageLimitsRefreshIdleInterval    = "usage.limits_refresh.idle_interval"
	SettingUsageLimitsRefreshWatchedInterval = "usage.limits_refresh.watched_interval"
	SettingUsageLimitsRefreshWatcherExpiry   = "usage.limits_refresh.watcher_expiry"
	SettingUsageCodexCommand                 = "usage.codex_command"
	SettingUsageCodexTimeout                 = "usage.codex_timeout"
	SettingUsageSpendRefreshEnabled          = "usage.spend_refresh.enabled"
	SettingUsageSpendRefreshInterval         = "usage.spend_refresh.interval"

	SettingEvaluationsRunsEnabled              = "evaluations.runs.enabled"
	SettingEvaluationsOrganizationID           = "evaluations.organization_id"
	SettingEvaluationsDefaultConcurrency       = "evaluations.default_concurrency"
	SettingEvaluationsDefaultBudgetLimits      = "evaluations.default_budget_limits"
	SettingEvaluationsBudgetCheckEveryCalls    = "evaluations.budget_check_every_calls"
	SettingEvaluationsAnswerTimeout            = "evaluations.answer_timeout"
	SettingEvaluationsJudgeTimeout             = "evaluations.judge_timeout"
	SettingEvaluationsOperationPollingInterval = "evaluations.operation_polling_interval"
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
		{Key: SettingUsageLimitsRefreshEnabled, EnvironmentVariable: "LLMBRIDGE_USAGE_LIMITS_REFRESH_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Read every subscription's limits into usage-store in this server: Anthropic's OAuth usage with the login in this user's ~/.claude/.credentials.json, and Codex's rate limits from codex app-server when usage.codex_command is set. Off by default, while the standalone usage-store still reads them; the reads belong to bridge-worker once it exists. Off, the limits the one-shot router and the usage page see go stale. Read at start."},
		{Key: SettingUsageLimitsRefreshIdleInterval, EnvironmentVariable: "LLMBRIDGE_USAGE_LIMITS_REFRESH_IDLE_INTERVAL", Kind: behavior, ValueType: duration, Default: "15m",
			Description: "How long the limits refresher waits between reads of a provider while nobody has the usage page open. A snapshot is stale three of these after it was taken. Anthropic's usage endpoint shares its rate limit with every Claude Code process on the login, so never set it short. Read at start."},
		{Key: SettingUsageLimitsRefreshWatchedInterval, EnvironmentVariable: "LLMBRIDGE_USAGE_LIMITS_REFRESH_WATCHED_INTERVAL", Kind: behavior, ValueType: duration, Default: "2m",
			Description: "How long the limits refresher waits between reads while the usage page is open (POST /usage/api/usage/limits/watch). A failed read waits this long, doubling up to the idle interval. Read at start."},
		{Key: SettingUsageLimitsRefreshWatcherExpiry, EnvironmentVariable: "LLMBRIDGE_USAGE_LIMITS_REFRESH_WATCHER_EXPIRY", Kind: behavior, ValueType: duration, Default: "3m",
			Description: "How long one POST /usage/api/usage/limits/watch keeps the watched interval. Read at start."},
		{Key: SettingUsageCodexCommand, EnvironmentVariable: "LLMBRIDGE_USAGE_CODEX_COMMAND", Kind: wiring, ValueType: text,
			Description: "The Codex command usage-store starts as codex app-server to read Codex's rate limits: a path, or a name on PATH (the npm codex needs node on PATH too). Empty switches Codex limits off. Read at start."},
		{Key: SettingUsageCodexTimeout, EnvironmentVariable: "LLMBRIDGE_USAGE_CODEX_TIMEOUT", Kind: behavior, ValueType: duration, Default: "30s",
			Description: "The longest one codex app-server read of Codex's rate limits may take. Read at start."},
		{Key: SettingUsageSpendRefreshEnabled, EnvironmentVariable: "LLMBRIDGE_USAGE_SPEND_REFRESH_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Read what each Anthropic API key cost into usage-store in this server, every usage.spend_refresh.interval. It finds the admin key among auth-store's Anthropic credentials at start, with auth_store.url and auth_store.token, and every resolve shows in auth-store's audit trail. Off by default, while the standalone usage-store still reads it. Read at start."},
		{Key: SettingUsageSpendRefreshInterval, EnvironmentVariable: "LLMBRIDGE_USAGE_SPEND_REFRESH_INTERVAL", Kind: behavior, ValueType: duration, Default: "1h",
			Description: "How long the spend refresher waits between reads of per-key spend. Read at start."},

		{Key: SettingEvaluationsRunsEnabled, EnvironmentVariable: "LLMBRIDGE_EVALUATIONS_RUNS_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Carry out eval-store runs in this server: POST /evaluations/runs/{id}/start and /resume make their model calls as llm.completion operations here. Off, both answer 503 and every other eval-store route still answers. Off by default, while the standalone eval-store still runs them; runs belong to bridge-worker once it exists. On, a run this server left going when it last stopped is marked interrupted at start. Read at start."},
		{Key: SettingEvaluationsOrganizationID, EnvironmentVariable: "LLMBRIDGE_EVALUATIONS_ORGANIZATION_ID", Kind: wiring, ValueType: text,
			Description: "The principal-store group an eval-store run's calls are billed to when the run names none; its monthly operation budget pays for them. Empty, a run that names none refuses to start. Read at start."},
		{Key: SettingEvaluationsDefaultConcurrency, EnvironmentVariable: "LLMBRIDGE_EVALUATIONS_DEFAULT_CONCURRENCY", Kind: behavior, ValueType: integer, Default: "4",
			Description: "How many model calls an eval-store run makes at once when it names no concurrency. Read at start."},
		{Key: SettingEvaluationsDefaultBudgetLimits, EnvironmentVariable: "LLMBRIDGE_EVALUATIONS_DEFAULT_BUDGET_LIMITS", Kind: behavior, ValueType: msg.ServiceSettingValueTypeStringMap, Default: "codex.weekly:60,anthropic.seven_day:40",
			Description: "subscription.window:percent pairs an eval-store run gets when it names no budget limits. The run stops, resumable, once usage-store reports any of them above its percent. Read at start."},
		{Key: SettingEvaluationsBudgetCheckEveryCalls, EnvironmentVariable: "LLMBRIDGE_EVALUATIONS_BUDGET_CHECK_EVERY_CALLS", Kind: behavior, ValueType: integer, Default: "8",
			Description: "How many calls an eval-store run makes between budget readings. A run may overshoot a limit by this many calls. Read at start."},
		{Key: SettingEvaluationsAnswerTimeout, EnvironmentVariable: "LLMBRIDGE_EVALUATIONS_ANSWER_TIMEOUT", Kind: behavior, ValueType: duration, Default: "10m",
			Description: "How long a candidate's call may take before its operation is asked to stop and the answer is recorded as failed. Read at start."},
		{Key: SettingEvaluationsJudgeTimeout, EnvironmentVariable: "LLMBRIDGE_EVALUATIONS_JUDGE_TIMEOUT", Kind: behavior, ValueType: duration, Default: "15m",
			Description: "How long a judge's call may take before its operation is asked to stop and the verdict is recorded as failed. Read at start."},
		{Key: SettingEvaluationsOperationPollingInterval, EnvironmentVariable: "LLMBRIDGE_EVALUATIONS_OPERATION_POLLING_INTERVAL", Kind: behavior, ValueType: duration, Default: "2s",
			Description: "How often a waiting eval-store call reads its operation's receipt. Read at start."},
	}
}
