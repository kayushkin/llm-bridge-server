package config

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The model-store role each model-calling job binary asks for is a behaviour
// setting of the service, not of the binary. The binaries are short-lived
// processes started by jobs, with no store and no HTTP of their own, so a
// setting they declared could only come from the environment and could not be
// changed from a settings page. The service stores these in its database
// (AttachStoredValues), serves them at GET /settings, takes a new value at
// PUT /settings/{key}, and each binary reads its role from there when it
// starts (internal/schedulersettings). That record is the only source: no
// binary reads a model or a role from its environment or a flag.
//
// The environment variable of each is read once, to seed the record on the
// first start after the setting exists; after that it is ignored and GET
// /settings says so.

// Keys of the stored model-role settings, as GET /settings names them.
const (
	SettingEmailClassifierModelRole      = "email_classifier_model_role"
	SettingEmailClassifierMergeModelRole = "email_classifier_merge_model_role"
	SettingKanbanClassifierModelRole     = "kanban_classifier_model_role"
	SettingKanbanScoperModelRole         = "kanban_scoper_model_role"
	SettingDemoMailGeneratorModelRole    = "demo_mail_generator_model_role"
	SettingAutoworkerAnthropicModelRole  = "autoworker_anthropic_model_role"
	SettingAutoworkerCodexModelRole      = "autoworker_codex_model_role"
	SettingAgentJobModelRole             = "agent_job_model_role"
	SettingHeraldModelRole               = "herald_model_role"
	SettingKanbanDispatcherModelRole     = "kanban_dispatcher_model_role"
	SettingDemoWorkerModelRole           = "demo_worker_model_role"
	SettingModelStoreURL                 = "model_store_url"
)

// DefaultModelStoreURL is where model-store answers with MODEL_STORE_URL unset.
const DefaultModelStoreURL = "http://localhost:8155"

// The roles the record is seeded with on first start. Each is the role whose
// models are closest to the model the binary asked for before it named a role;
// see the README's settings table for what each replaced.
const (
	// email-classifier filed with claude-sonnet-4-6, deliberately a better
	// model than its siblings': 44-50% of the mail that reaches the model
	// becomes a new card, and telling a task from a record is the judgement
	// the cheap model got wrong. default is model-store's everyday tier.
	DefaultEmailClassifierModelRole = string(msg.ModelRoleDefault)
	// Not efficient: on 2026-09-24 the efficient tier answered fifty-card
	// batches with an empty list and found one group where balanced found four.
	DefaultEmailClassifierMergeModelRole = string(msg.ModelRoleBalanced)
	// kanban-classifier and kanban-scoper ran on claude-haiku-4-5, the cheap
	// tier.
	DefaultKanbanClassifierModelRole = string(msg.ModelRoleEfficient)
	DefaultKanbanScoperModelRole     = string(msg.ModelRoleEfficient)
	// demo-mail-generator wrote with claude-sonnet-4-6.
	DefaultDemoMailGeneratorModelRole = string(msg.ModelRoleDefault)

	// The sessions below named no model and ran on the bridge's per-harness
	// default: claude-opus-5-5 on claude_code and gpt-5.6-terra on codex on
	// 2026-09-30, the first models of default and balanced.
	DefaultAutoworkerAnthropicModelRole = string(msg.ModelRoleDefault)
	DefaultAutoworkerCodexModelRole     = string(msg.ModelRoleBalanced)
	DefaultAgentJobModelRole            = string(msg.ModelRoleDefault)
	DefaultHeraldModelRole              = string(msg.ModelRoleDefault)
	// kanban-dispatcher and demo-worker take their instance and bundle from
	// the board; both boards used inst-cc-local (claude_code) and no bundle
	// named a model, so they too ran on the claude_code default.
	DefaultKanbanDispatcherModelRole = string(msg.ModelRoleDefault)
	DefaultDemoWorkerModelRole       = string(msg.ModelRoleDefault)
)

// ModelRoleSettingKeys lists every stored model-role setting, for the
// service's validator.
var ModelRoleSettingKeys = []string{
	SettingEmailClassifierModelRole, SettingEmailClassifierMergeModelRole,
	SettingKanbanClassifierModelRole, SettingKanbanScoperModelRole, SettingDemoMailGeneratorModelRole,
	SettingAutoworkerAnthropicModelRole, SettingAutoworkerCodexModelRole, SettingAgentJobModelRole, SettingHeraldModelRole,
	SettingKanbanDispatcherModelRole, SettingDemoWorkerModelRole,
}

func modelRoleDefinition(key, variable, defaultRole, description string) servicesettings.Definition {
	return servicesettings.Definition{Key: key, EnvironmentVariable: variable, Kind: msg.ServiceSettingKindBehaviour, ValueType: msg.ServiceSettingValueTypeModelRole,
		Default: defaultRole, Required: true, Editable: true,
		Description: description + " A model-store role (best, default, balanced, efficient): llm-bridge-server tries the role's models in order. Read by the job binary each time it starts, so a change takes effect on the next run."}
}

// sessionModelRoleDefinition declares the role a session this repo starts runs
// on. A session is not a one-shot call: llm-bridge-server pins it to the role's
// FIRST model and never tries the rest, and the session's harness must be able
// to run that model, which is checked before the session is created.
func sessionModelRoleDefinition(key, variable, defaultRole, description string) servicesettings.Definition {
	return servicesettings.Definition{Key: key, EnvironmentVariable: variable, Kind: msg.ServiceSettingKindBehaviour, ValueType: msg.ServiceSettingValueTypeModelRole,
		Default: defaultRole, Required: true, Editable: true,
		Description: description + " A model-store role (best, default, balanced, efficient): the session runs on the role's first model, with no fallback to the rest, and is not started when its harness cannot run that model. Read when each session is started, so a change applies to the next one."}
}

func modelRoleDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		modelRoleDefinition(SettingEmailClassifierModelRole, "SCHEDULER_EMAIL_CLASSIFIER_MODEL_ROLE", DefaultEmailClassifierModelRole,
			"The role email-classifier's filing pass and deadline backfill ask for. It reads each batch of new mail and decides which card it joins or whether it starts one."),
		modelRoleDefinition(SettingEmailClassifierMergeModelRole, "SCHEDULER_EMAIL_CLASSIFIER_MERGE_MODEL_ROLE", DefaultEmailClassifierMergeModelRole,
			"The role email-classifier's card-merge pass asks for. It makes one call per fifty cards, so it is where a cheaper role pays most."),
		modelRoleDefinition(SettingKanbanClassifierModelRole, "SCHEDULER_KANBAN_CLASSIFIER_MODEL_ROLE", DefaultKanbanClassifierModelRole,
			"The role kanban-classifier asks for when it files an agent session onto the board."),
		modelRoleDefinition(SettingKanbanScoperModelRole, "SCHEDULER_KANBAN_SCOPER_MODEL_ROLE", DefaultKanbanScoperModelRole,
			"The role kanban-scoper asks for when it splits a goal card into sub-cards."),
		modelRoleDefinition(SettingDemoMailGeneratorModelRole, "SCHEDULER_DEMO_MAIL_GENERATOR_MODEL_ROLE", DefaultDemoMailGeneratorModelRole,
			"The role demo-mail-generator asks for when it writes the demo mailbox's messages."),
		sessionModelRoleDefinition(SettingAutoworkerAnthropicModelRole, "SCHEDULER_AUTOWORKER_ANTHROPIC_MODEL_ROLE", DefaultAutoworkerAnthropicModelRole,
			"The role autoworker's anthropic lane starts its worker sessions on (claude_code harness, autoworker-anthropic-… sessions)."),
		sessionModelRoleDefinition(SettingAutoworkerCodexModelRole, "SCHEDULER_AUTOWORKER_CODEX_MODEL_ROLE", DefaultAutoworkerCodexModelRole,
			"The role autoworker's codex lane starts its worker sessions on (codex harness, autoworker-codex-… sessions). The codex harness runs only openai models."),
		sessionModelRoleDefinition(SettingAgentJobModelRole, "SCHEDULER_AGENT_JOB_MODEL_ROLE", DefaultAgentJobModelRole,
			"The role an agent-type job starts its session on when the job names no model_role of its own."),
		sessionModelRoleDefinition(SettingHeraldModelRole, "SCHEDULER_HERALD_MODEL_ROLE", DefaultHeraldModelRole,
			"The role ask starts a herald-… session on, to relay an alert or question to the user."),
		sessionModelRoleDefinition(SettingKanbanDispatcherModelRole, "SCHEDULER_KANBAN_DISPATCHER_MODEL_ROLE", DefaultKanbanDispatcherModelRole,
			"The role kanban-dispatcher starts a fresh tcl-… session on for a board's Backlog card. It outranks the model of the bundle the board gives the card; a card whose instance's harness cannot run the role's first model is not dispatched."),
		sessionModelRoleDefinition(SettingDemoWorkerModelRole, "SCHEDULER_DEMO_WORKER_MODEL_ROLE", DefaultDemoWorkerModelRole,
			"The role demo-worker starts its demo-… dispatch and review sessions on. It outranks the model of the bundle the board names."),
		{Key: SettingModelStoreURL, EnvironmentVariable: "MODEL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultModelStoreURL,
			Description: "Where model-store answers. A new model role is checked against its GET /api/roles before it is stored, and an agent job's session role against GET /api/roles/{role} before the session starts; while it is down no role can be changed and no agent job starts."},
	}
}
