package projectstore

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description.
const ServiceName = "project-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting.
const OwnedEnvironmentVariablePrefix = "PROJECT_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress           = "listen_address"
	SettingDataDirectory           = "data_directory"
	SettingRepoStoreURL            = "repo_store_url"
	SettingKanbanStoreURL          = "kanban_store_url"
	SettingKanbanStoreServiceToken = "kanban_store_service_token"
	SettingSchedulerURL            = "scheduler_url"
	SettingHealthcheckURL          = "healthcheck_url"
	SettingPrincipalStoreURL       = "principal_store_url"
	SettingNoteboardURL            = "noteboard_url"
	SettingWorkGraphStoreURL       = "work_graph_store_url"
	SettingBridgeURL               = "bridge_url"
	SettingBridgeServiceToken      = "bridge_service_token"
	SettingSuggestionModelRole     = "suggestion_model_role"

	SettingSessionFilerModelRole                      = "session_filer_model_role"
	SettingSessionFilerMinimumConfidence              = "session_filer_minimum_confidence"
	SettingSessionFilerQuietBeforeFilingWithoutABrief = "session_filer_quiet_before_filing_without_a_brief"
	SettingSessionFilerWindow                         = "session_filer_window"
	SettingSessionFilerSessionsPerPass                = "session_filer_sessions_per_pass"
)

// DefaultListenAddress is where the service listens with nothing set.
const DefaultListenAddress = "127.0.0.1:8320"

// DefaultDataDir is where the database lives with nothing set.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "share", "project-store")
}

func wiring(key, variable, defaultValue, description string) servicesettings.Definition {
	return servicesettings.Definition{Key: key, EnvironmentVariable: variable, Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: defaultValue, Description: description}
}

// SettingDefinitions declares every environment variable this process reads,
// once. Nothing is Editable: the service has no operator gate.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		wiring(SettingListenAddress, "PROJECT_STORE_ADDR", DefaultListenAddress,
			"The address the HTTP server listens on. Changing it moves the service, so everything that calls it must be told the new address."),
		{Key: SettingDataDirectory, EnvironmentVariable: "PROJECT_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds project-store.db. Changing it starts the service on whatever database is there, or an empty one."},
		wiring(SettingRepoStoreURL, "PROJECT_STORE_REPO_STORE_URL", "http://localhost:8306",
			"repo-store. A repo linked to a project is checked there, and a project's deploys are read from its ledger."),
		wiring(SettingKanbanStoreURL, "PROJECT_STORE_KANBAN_STORE_URL", "http://localhost:8305",
			"kanban-store. A board linked to a project is checked there, and a project's cards are the cards linked to it there."),
		{Key: SettingKanbanStoreServiceToken, EnvironmentVariable: "KANBAN_STORE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "The token kanban-store takes from an internal service. Without it kanban-store answers every call 401, so the service refuses to start."},
		wiring(SettingSchedulerURL, "PROJECT_STORE_SCHEDULER_URL", "http://localhost:8092",
			"scheduler. A scheduler job linked to a project is checked there."),
		wiring(SettingHealthcheckURL, "PROJECT_STORE_HEALTHCHECK_URL", "http://localhost:8099",
			"healthcheck. A service linked to a project is checked against its inventory, and its state is read there."),
		wiring(SettingPrincipalStoreURL, "PROJECT_STORE_PRINCIPAL_STORE_URL", "http://127.0.0.1:8314",
			"principal-store. A person or group linked to a project is checked there."),
		wiring(SettingNoteboardURL, "PROJECT_STORE_NOTEBOARD_URL", "http://localhost:8191",
			"noteboard. A note linked to a project (a decision, a design) is checked there."),
		wiring(SettingWorkGraphStoreURL, "PROJECT_STORE_WORK_GRAPH_STORE_URL", "http://127.0.0.1:8319",
			"work-graph-store. A project's branches, and the sessions that moved them, are read there for its repos."),
		wiring(SettingBridgeURL, "PROJECT_STORE_BRIDGE_URL", "http://localhost:8160",
			"llm-bridge-server. A session filed under a project is checked there."),
		{Key: SettingBridgeServiceToken, EnvironmentVariable: "LLMBRIDGE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "The token llm-bridge-server takes from an internal service. Without it every session check is 401, so the service refuses to start."},
		{Key: SettingSuggestionModelRole, EnvironmentVariable: "PROJECT_STORE_SUGGESTION_MODEL_ROLE", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeString, Default: "balanced",
			Description: "The model-store role llm-bridge-server's one-shot router asks for a project's suggested next steps. Empty refuses suggestions."},
		{Key: SettingSessionFilerModelRole, EnvironmentVariable: "PROJECT_STORE_SESSION_FILER_MODEL_ROLE", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeString, Default: "balanced",
			Description: "The model-store role llm-bridge-server's one-shot router asks which project a chat session belongs to. Empty refuses every filing pass."},
		{Key: SettingSessionFilerMinimumConfidence, EnvironmentVariable: "PROJECT_STORE_SESSION_FILER_MINIMUM_CONFIDENCE", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDecimal, Default: "0.85",
			Description: "The lowest odds, from 0 to 1, at which the session filer files a session under the project the model picked. Below it the pick waits as a proposal for a person. Raising it files fewer sessions and proposes more."},
		{Key: SettingSessionFilerQuietBeforeFilingWithoutABrief, EnvironmentVariable: "PROJECT_STORE_SESSION_FILER_QUIET_BEFORE_FILING_WITHOUT_A_BRIEF", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "1h",
			Description: "How long a chat with no brief must have been unchanged before the session filer judges it from its name alone. A chat gets its brief when its first turn ends, so a shorter wait judges chats whose brief is about to be written."},
		{Key: SettingSessionFilerWindow, EnvironmentVariable: "PROJECT_STORE_SESSION_FILER_WINDOW", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "48h",
			Description: "How far back a filing pass looks for changed chats when its caller names no window. It must be longer than the longest time the scheduler job may be off, or chats changed in the gap are never judged."},
		{Key: SettingSessionFilerSessionsPerPass, EnvironmentVariable: "PROJECT_STORE_SESSION_FILER_SESSIONS_PER_PASS", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeInteger, Default: "20",
			Description: "The most sessions one filing pass judges, each with one model call, when its caller names no limit. The rest wait for the next pass."},
	}
}

// NewSettingsRegistry reads this service's settings from environment.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, []string{OwnedEnvironmentVariablePrefix}, SettingDefinitions(), environment)
}

// RegisterSettingsHandler serves the registry at GET /settings.
func RegisterSettingsHandler(mux *http.ServeMux, registry *servicesettings.Registry) {
	mux.Handle("GET /settings", servicesettings.Handler(registry, "/settings"))
}
