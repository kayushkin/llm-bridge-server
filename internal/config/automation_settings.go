package config

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Keys of the scheduler's and rule-store's settings in this server. Both keep
// their records in their modules of database.postgres_url and are served
// under /scheduler/ and /rule-store/ whatever these say; the switches below
// say only whether this process acts on them. Each is read once, at start.
const (
	SettingSchedulerRunsEnabled    = "scheduler.runs.enabled"
	SettingSchedulerNATSURL        = "scheduler.nats_url"
	SettingSchedulerNoteboardURL   = "scheduler.noteboard_url"
	SettingRuleStoreReactorEnabled = "rule_store.reactor.enabled"
	SettingRuleStoreSweepInterval  = "rule_store.sweep_interval"
)

// automationSettingDefinitions declares the settings above.
func automationSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingSchedulerRunsEnabled, EnvironmentVariable: "LLMBRIDGE_SCHEDULER_RUNS_ENABLED", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "false",
			Description: "Whether this server runs scheduler jobs: the cron ticker, run now, the bus commands and the runs event triggers start. Off, the jobs, holds, runs and stored model roles are still read and written at /scheduler/, no job runs, and run now answers 503. Turn it on only after the standalone scheduler has stopped: two processes holding the same jobs would run every shell command, agent session and reminder twice. Read at start."},
		{Key: SettingSchedulerNATSURL, EnvironmentVariable: "LLMBRIDGE_SCHEDULER_NATS_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The NATS server the scheduler's agent jobs publish their first turn on (bridge.inbound), its bus commands answer on, its event triggers read, and its run log goes to. Read only while scheduler.runs.enabled is on. Empty runs shell jobs only: an agent job's run ends in error, no event trigger fires and no bus command is answered."},
		{Key: SettingSchedulerNoteboardURL, EnvironmentVariable: "LLMBRIDGE_SCHEDULER_NOTEBOARD_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The noteboard address written into an agent job's prompt, for its agent to read and rewrite the job's workspace. Required while scheduler.runs.enabled is on and scheduler.nats_url is set."},
		{Key: SettingRuleStoreReactorEnabled, EnvironmentVariable: "LLMBRIDGE_RULE_STORE_REACTOR_ENABLED", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "false",
			Description: "Whether this server runs rule-store's reactor: it follows this server's sessions, checks every active rule against each change and on the sweep, records each decision and, for a live rule, makes its calls. Off, rules, revisions, activations and runs are still read and written at /rule-store/, nothing is decided and activate's background check is skipped. Turn it on only after the standalone rule-store has stopped: two reactors would decide every rule twice and a live rule would act twice. Read at start."},
		{Key: SettingRuleStoreSweepInterval, EnvironmentVariable: "LLMBRIDGE_RULE_STORE_SWEEP_INTERVAL", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "1m",
			Description: "How often the reactor checks every active rule with a time condition against every session rule-store holds. Read only while rule_store.reactor.enabled is on."},
	}
}
