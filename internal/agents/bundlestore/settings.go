package bundlestore

import (
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "bundle-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting: it is a misspelling or a leftover,
// and either way someone believes it does something.
//
// The whole prefix is owned, not a pair of narrow ones, because nothing else
// under it should reach this process. BUNDLE_STORE_URL, which callers read to
// find this service, belongs in the callers' own environments, never in this
// service's unit or in the shell that runs deploy.sh.
const OwnedEnvironmentVariablePrefix = "BUNDLE_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress = "listen_address"
	SettingDataDirectory = "data_directory"
	SettingSkillStoreURL = "skill_store_url"
	SettingToolStoreURL  = "tool_store_url"
)

// DefaultListenAddress is where the service listens with nothing set.
const DefaultListenAddress = "127.0.0.1:8307"

// DefaultSkillStoreURL and DefaultToolStoreURL are where the two stores that
// own bundle members answer. They are the defaults of the skill_store_url and
// tool_store_url settings, which the command hands to the member lookup; no
// code reads the environment for them.
const (
	DefaultSkillStoreURL = "http://localhost:8301"
	DefaultToolStoreURL  = "http://localhost:8302"
)

// SettingDefinitions declares every environment variable this process reads,
// once. The command reads its configuration from it; GET /settings describes
// the service from it; and a test holds every os.Getenv in the repo to it, so
// a variable cannot be read without being declared here.
//
// Nothing here is Editable, and nothing may become so while the service has no
// operator gate: GET /settings is as open as every other route.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "BUNDLE_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Changing it moves the service, so everything that resolves a session's bundle must be told the new address."},
		{Key: SettingDataDirectory, EnvironmentVariable: "BUNDLE_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds bundle-store.db. Changing it starts the service on whatever database is there, or on an empty one with no bundles; the bundles in the old directory stay where they were."},
		{Key: SettingSkillStoreURL, EnvironmentVariable: "SKILL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultSkillStoreURL,
			Description: "Where skill-store answers. Only the -check-members pre-flight reads it, to confirm every stored skill member id still exists; the server itself never calls skill-store."},
		{Key: SettingToolStoreURL, EnvironmentVariable: "TOOL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultToolStoreURL,
			Description: "Where tool-store answers. Only the -check-members pre-flight reads it, to confirm every stored tool member id still exists; the server itself never calls tool-store."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a value that does not parse and on a set BUNDLE_STORE_ variable nobody
// declared.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, []string{OwnedEnvironmentVariablePrefix}, SettingDefinitions(), environment)
}

// RegisterSettingsHandler serves the registry at GET /settings, the way every
// service serves its settings. PUT /settings/{key} is not mounted: no setting
// is Editable, so there is nothing a write could change.
func RegisterSettingsHandler(mux *http.ServeMux, registry *servicesettings.Registry) {
	mux.Handle("GET /settings", servicesettings.Handler(registry, "/settings"))
}
