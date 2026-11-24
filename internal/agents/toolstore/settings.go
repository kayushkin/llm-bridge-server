package toolstore

import (
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/agents/toolstore/tools"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "tool-store"

// OwnedEnvironmentVariableNames are the variables that are this service's
// alone. servicesettings treats each as a prefix, so a set TOOL_STORE_ADDRESS
// or TOOL_STORE_DATA_DIRECTORY that SettingDefinitions does not declare stops
// the service from starting: it is a misspelling, and someone believes it does
// something.
//
// The whole TOOL_STORE_ prefix is not owned. TOOL_STORE_URL is what other
// services read to find this one, and a shell that carries it — an agent's, or
// one that runs scripts/e2e-smoke.sh — must still be able to start the binary.
var OwnedEnvironmentVariableNames = []string{"TOOL_STORE_ADDR", "TOOL_STORE_DATA_DIR"}

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress  = "listen_address"
	SettingDataDirectory  = "data_directory"
	SettingAuthStoreURL   = "auth_store_url"
	SettingAuthStoreToken = "auth_store_token"
	SettingPinchtabURL    = "pinchtab_url"
	SettingPinchtabToken  = "pinchtab_token"
	SettingBraveAPIKey    = "brave_api_key"
	SettingSchedulerURL   = "scheduler_url"
	SettingSchedulerToken = "scheduler_token"
)

// DefaultListenAddress is where the service listens with nothing set: loopback,
// because the service has no auth of its own.
const DefaultListenAddress = "127.0.0.1:8302"

// DefaultAuthStoreURL is where auth-store is looked for with nothing set.
const DefaultAuthStoreURL = "http://127.0.0.1:8303"

// SettingDefinitions declares every environment variable this process reads as
// configuration, once. The command reads its configuration from it and hands
// the tools theirs; GET /settings describes the service from it; and a test
// holds every os.Getenv in the repo to it, so a variable cannot be read without
// being declared here.
//
// Every secret is a string: servicesettings quotes a value that does not parse
// as its type, and a string always parses.
//
// Nothing here is Editable, and nothing may become so while the service has no
// operator gate: GET /settings is as open as every other route.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "TOOL_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Changing it moves the service, so everything that calls it must be told the new address."},
		{Key: SettingDataDirectory, EnvironmentVariable: "TOOL_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds tool-store.db. Changing it starts the service on whatever database is there, or an empty one; the old tools, instances and opt-ins stay where they were."},
		{Key: SettingAuthStoreURL, EnvironmentVariable: "AUTH_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultAuthStoreURL,
			Description: "Where auth-store answers. POST /provision resolves each credential an MCP tool names there; a wrong address makes provisioning a tool with credentials fail and leaves the rest of the service working."},
		{Key: SettingAuthStoreToken, EnvironmentVariable: "AUTH_STORE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The bearer token presented to auth-store. It must match auth-store's own AUTHSTORE_TOKEN; without it an auth-store that has a token answers 401 and no credential resolves."},
		{Key: SettingPinchtabURL, EnvironmentVariable: "PINCHTAB_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: tools.DefaultPinchtabURL,
			Description: "Where PinchTab answers. The in-process browser tool sends every action there; a wrong address makes each browser call report a connection error."},
		{Key: SettingPinchtabToken, EnvironmentVariable: "PINCHTAB_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The bearer token the browser tool presents to PinchTab. Unset, the tool sends no Authorization header."},
		{Key: SettingBraveAPIKey, EnvironmentVariable: "BRAVE_API_KEY", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The Brave Search API key the in-process web_search tool sends. Unset, every web_search call answers that the key is not set. It is not the key an MCP brave-search server gets: that one is resolved from auth-store at provision time."},
		{Key: SettingSchedulerURL, EnvironmentVariable: "SCHEDULER_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: tools.DefaultSchedulerURL,
			Description: "Where the scheduler answers. The in-process scheduler tool lists, creates and changes cron jobs there."},
		{Key: SettingSchedulerToken, EnvironmentVariable: "SCHEDULER_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The bearer token the scheduler tool presents to the scheduler. Unset, the tool sends no Authorization header."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a set variable that begins with one of OwnedEnvironmentVariableNames and
// that nobody declared.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, OwnedEnvironmentVariableNames, SettingDefinitions(), environment)
}

// OutsideServiceConnectionsFrom reads from registry what the three in-process
// tools that reach an outside service are built with.
func OutsideServiceConnectionsFrom(registry *servicesettings.Registry) tools.OutsideServiceConnections {
	return tools.OutsideServiceConnections{
		Pinchtab:    tools.PinchtabConnection{BaseURL: registry.String(SettingPinchtabURL), Token: registry.String(SettingPinchtabToken)},
		BraveAPIKey: registry.String(SettingBraveAPIKey),
		Scheduler:   tools.SchedulerConnection{BaseURL: registry.String(SettingSchedulerURL), Token: registry.String(SettingSchedulerToken)},
	}
}

// RegisterSettingsHandler serves the registry at GET /settings, the way every
// service serves its settings. PUT /settings/{key} is not mounted: no setting
// is Editable, so there is nothing a write could change.
func RegisterSettingsHandler(mux *http.ServeMux, registry *servicesettings.Registry) {
	mux.Handle("GET /settings", servicesettings.Handler(registry, "/settings"))
}
