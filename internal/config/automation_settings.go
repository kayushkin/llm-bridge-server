package config

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Keys of the scheduler's and rule-store's settings in this server. Both keep
// their records in their modules of database.postgres_url and are served
// under /scheduler/ and /rules/ whatever these say; the switches below
// say only whether this process acts on them. Each is read once, at start.
const (
	SettingSchedulerRunsEnabled = "scheduler.runs.enabled"
	SettingSchedulerNATSURL     = "scheduler.nats_url"
	SettingSchedulerNotesURL    = "scheduler.notes_url"
	// SettingSchedulerAgentJobPrincipalID is the principal every session an
	// agent job starts runs as.
	SettingSchedulerAgentJobPrincipalID = "scheduler.agent_job_principal_id"
	SettingRulesReactorEnabled          = "rules.reactor.enabled"
	SettingRulesSweepInterval           = "rules.sweep_interval"
)

// automationSettingDefinitions declares the settings above.
func automationSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingSchedulerRunsEnabled, EnvironmentVariable: "LLMBRIDGE_SCHEDULER_RUNS_ENABLED", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "false",
			Description: "Whether this server runs scheduler jobs: the cron ticker, run now, the bus commands and the runs event triggers start. Off, the jobs, holds, runs and stored model roles are still read and written at /scheduler/, no job runs, and run now answers 503. Turn it on only after the standalone scheduler has stopped: two processes holding the same jobs would run every shell command, agent session and reminder twice. Read at start."},
		{Key: SettingSchedulerNATSURL, EnvironmentVariable: "LLMBRIDGE_SCHEDULER_NATS_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The NATS server the scheduler's bus commands answer on, its event triggers read, and its run log goes to. Read only while scheduler.runs.enabled is on. Empty answers no bus command and fires no event trigger. Agent jobs do not use it: since 2026-11-27 they start their sessions through this server's session core, not on bridge.inbound."},
		{Key: SettingSchedulerNotesURL, EnvironmentVariable: "LLMBRIDGE_SCHEDULER_NOTES_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The address of notes (this server's /notes, as the agent reaches it, such as http://127.0.0.1:8160/notes) written into an agent job's prompt, for its agent to read and rewrite the job's workspace at <address>/items/<id>. It was scheduler.noteboard_url until 2026-11-27, when workspaces stayed in notes and noteboard's todos became issues. Required while scheduler.runs.enabled is on."},
		{Key: SettingSchedulerAgentJobPrincipalID, EnvironmentVariable: "LLMBRIDGE_SCHEDULER_AGENT_JOB_PRINCIPAL_ID", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The principal (a principal-store id, such as principal_000001) every session an agent job starts runs as, so its agent gets LLM_BRIDGE_PRINCIPAL_TOKEN and LLM_BRIDGE_GATEWAY_URL and reaches what that principal may: the job's workspace in notes and the issues its prompt calls are operator routes, so this is an administrator, normally the operator. The session is created and checked as POST /sessions creates one naming that principal, and its grants decide where it may run. Empty starts sessions as no principal, whose agents hold no token: a job with a workspace then fails its run, naming this setting. Read only while scheduler.runs.enabled is on, at start."},
		{Key: SettingRulesReactorEnabled, EnvironmentVariable: "LLMBRIDGE_RULES_REACTOR_ENABLED", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "false",
			Description: "Whether this server runs rule-store's reactor: it follows this server's sessions, checks every active rule against each change and on the sweep, records each decision and, for a live rule, makes its calls. Off, rules, revisions, activations and runs are still read and written at /rules/, nothing is decided and activate's background check is skipped. Turn it on only after the standalone rule-store has stopped: two reactors would decide every rule twice and a live rule would act twice. Read at start."},
		{Key: SettingRulesSweepInterval, EnvironmentVariable: "LLMBRIDGE_RULES_SWEEP_INTERVAL", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "1m",
			Description: "How often the reactor checks every active rule with a time condition against every session rule-store holds. Read only while rules.reactor.enabled is on."},
	}
}
