package principalstore

import (
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "principal-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting: it is a misspelling or a leftover,
// and either way someone believes it does something.
const OwnedEnvironmentVariablePrefix = "PRINCIPAL_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress = "listen_address"
	SettingDataDirectory = "data_directory"
)

// DefaultListenAddress is where the service listens with nothing set. Loopback,
// deliberately — not ":8314" like the older siblings. This service has no auth
// of its own; dash is the front door that adds it. A wildcard bind would put
// principal editing on the network for anything that can route to this host.
const DefaultListenAddress = "127.0.0.1:8314"

// SettingDefinitions declares every environment variable this process reads,
// once. The command reads its configuration from it; GET /settings describes
// the service from it; and a test holds every os.Getenv in the repo to it, so
// a variable cannot be read without being declared here.
//
// Nothing here is Editable, and nothing may become so while the service has no
// operator gate: GET /settings is as open as every other route.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "PRINCIPAL_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Changing it moves the service, so everything that calls it must be told the new address."},
		{Key: SettingDataDirectory, EnvironmentVariable: "PRINCIPAL_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds principal-store.db. Changing it starts the service on whatever database is there, or an empty one; the old principals stay where they were."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a value that does not parse and on a set PRINCIPAL_STORE_ variable nobody
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
