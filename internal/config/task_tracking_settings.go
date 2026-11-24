package config

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Keys of the settings of the task_tracking modules this server runs beside
// prediction-store: noteboard needs none of its own, and project-store's are
// these. Each is read once, at start.
const (
	SettingProjectStoreSuggestionModelRole                        = "project_store.suggestion_model_role"
	SettingProjectStoreSessionFilerEnabled                        = "project_store.session_filer.enabled"
	SettingProjectStoreSessionFilerInterval                       = "project_store.session_filer.interval"
	SettingProjectStoreSessionFilerModelRole                      = "project_store.session_filer.model_role"
	SettingProjectStoreSessionFilerMinimumConfidence              = "project_store.session_filer.minimum_confidence"
	SettingProjectStoreSessionFilerQuietBeforeFilingWithoutABrief = "project_store.session_filer.quiet_before_filing_without_a_brief"
	SettingProjectStoreSessionFilerWindow                         = "project_store.session_filer.window"
	SettingProjectStoreSessionFilerSessionsPerPass                = "project_store.session_filer.sessions_per_pass"
)

// taskTrackingSettingDefinitions declares the settings above.
func taskTrackingSettingDefinitions() []servicesettings.Definition {
	const (
		wiring   = msg.ServiceSettingKindWiring
		behavior = msg.ServiceSettingKindBehavior
		text     = msg.ServiceSettingValueTypeString
	)
	return []servicesettings.Definition{
		{Key: SettingProjectStoreSuggestionModelRole, EnvironmentVariable: "LLMBRIDGE_PROJECT_STORE_SUGGESTION_MODEL_ROLE", Kind: behavior, ValueType: text, Default: "balanced",
			Description: "The model-store role this server's one-shot router asks for a project's suggested next steps (POST /project-store/projects/{id}/suggestions). Empty refuses suggestions. Read at start."},
		{Key: SettingProjectStoreSessionFilerEnabled, EnvironmentVariable: "LLMBRIDGE_PROJECT_STORE_SESSION_FILER_ENABLED", Kind: behavior, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "false",
			Description: "Run project-store's session filer in this server: a filing pass every project_store.session_filer.interval, each filing chats under projects as POST /project-store/session-filings/run does. Off by default, while the standalone project-store's scheduler job still runs the passes; the passes belong to bridge-worker once it exists. Read at start."},
		{Key: SettingProjectStoreSessionFilerInterval, EnvironmentVariable: "LLMBRIDGE_PROJECT_STORE_SESSION_FILER_INTERVAL", Kind: behavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "5m",
			Description: "How long the session filer waits between passes when project_store.session_filer.enabled is on, counted from the end of the last one. Read at start."},
		{Key: SettingProjectStoreSessionFilerModelRole, EnvironmentVariable: "LLMBRIDGE_PROJECT_STORE_SESSION_FILER_MODEL_ROLE", Kind: behavior, ValueType: text, Default: "balanced",
			Description: "The model-store role this server's one-shot router asks which project a chat session belongs to. Empty refuses every filing pass. Read at start."},
		{Key: SettingProjectStoreSessionFilerMinimumConfidence, EnvironmentVariable: "LLMBRIDGE_PROJECT_STORE_SESSION_FILER_MINIMUM_CONFIDENCE", Kind: behavior, ValueType: msg.ServiceSettingValueTypeDecimal, Default: "0.85",
			Description: "The lowest odds, above 0 and at most 1, at which the session filer files a session under the project the model picked. Below it the pick waits as a proposal for a person. Read at start."},
		{Key: SettingProjectStoreSessionFilerQuietBeforeFilingWithoutABrief, EnvironmentVariable: "LLMBRIDGE_PROJECT_STORE_SESSION_FILER_QUIET_BEFORE_FILING_WITHOUT_A_BRIEF", Kind: behavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "1h",
			Description: "How long a chat with no brief must have been unchanged before the session filer judges it from its name alone. A chat gets its brief when its first turn ends. Read at start."},
		{Key: SettingProjectStoreSessionFilerWindow, EnvironmentVariable: "LLMBRIDGE_PROJECT_STORE_SESSION_FILER_WINDOW", Kind: behavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "48h",
			Description: "How far back a filing pass looks for changed chats when its caller names no window. It must be longer than the longest time the passes may stop, or chats changed in the gap are never judged. Read at start."},
		{Key: SettingProjectStoreSessionFilerSessionsPerPass, EnvironmentVariable: "LLMBRIDGE_PROJECT_STORE_SESSION_FILER_SESSIONS_PER_PASS", Kind: behavior, ValueType: msg.ServiceSettingValueTypeInteger, Default: "20",
			Description: "The most sessions one filing pass judges, each with one model call, when its caller names no limit. Read at start."},
	}
}
