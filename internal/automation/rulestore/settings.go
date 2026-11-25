package rulestore

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description.
const ServiceName = "rule-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting.
const OwnedEnvironmentVariablePrefix = "RULE_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress           = "listen_address"
	SettingDataDirectory           = "data_directory"
	SettingLLMBridgeURL            = "llm_bridge_url"
	SettingLLMBridgeServiceToken   = "llm_bridge_service_token"
	SettingKanbanStoreURL          = "kanban_store_url"
	SettingKanbanStoreServiceToken = "kanban_store_service_token"
	SettingSweepInterval           = "sweep_interval"
)

// DefaultDataDir is where the database lives when nothing says otherwise.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "rule-store")
}

// SettingDefinitions declares every environment variable this process reads,
// once. Addresses have no default: the unit names them, so no port is written
// into the code. Nothing is Editable: there is no operator gate to put a write
// behind.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "RULE_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "The address the HTTP server listens on. The unit sets it, on loopback: the routes take no credential."},
		{Key: SettingDataDirectory, EnvironmentVariable: "RULE_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds rule-store.db."},
		{Key: SettingLLMBridgeURL, EnvironmentVariable: "RULE_STORE_LLM_BRIDGE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "llm-bridge-server's base URL. Sessions are read from GET /sessions and followed on GET /session-events."},
		{Key: SettingLLMBridgeServiceToken, EnvironmentVariable: "LLMBRIDGE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "llm-bridge-server's internal service token, sent alone, so rule-store reads every session and acts as no principal, as scheduler does. Loaded from the shared principal-gating tokens file."},
		{Key: SettingKanbanStoreURL, EnvironmentVariable: "RULE_STORE_KANBAN_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "kanban-store's base URL. A session's linked cards, their placements and the session's entity tags are read there."},
		{Key: SettingKanbanStoreServiceToken, EnvironmentVariable: "KANBAN_STORE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "kanban-store's service token. Entity tags answer 403 to a principal, so rule-store reads as the service."},
		{Key: SettingSweepInterval, EnvironmentVariable: "RULE_STORE_SWEEP_INTERVAL", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "1m",
			Description: "How often every active rule is checked against every session rule-store holds. The feed checks a session the moment it changes; the sweep is what notices that time has passed, for conditions such as updated_before."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a value that does not parse and on a set RULE_STORE_ variable nobody
// declared. Required settings are checked by serve.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, []string{OwnedEnvironmentVariablePrefix}, SettingDefinitions(), environment)
}

// RegisterSettingsHandler serves the registry at GET /settings.
func RegisterSettingsHandler(mux *http.ServeMux, registry *servicesettings.Registry) {
	mux.Handle("GET /settings", servicesettings.Handler(registry, "/settings"))
}
