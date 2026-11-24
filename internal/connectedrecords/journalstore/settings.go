package journalstore

import (
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "journal-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting: it is a misspelling or a leftover,
// and either way someone believes it does something.
const OwnedEnvironmentVariablePrefix = "JOURNAL_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress     = "listen_address"
	SettingDataDirectory     = "data_directory"
	SettingNoteboardURL      = "noteboard_url"
	SettingReferenceStoreURL = "reference_store_url"
)

// DefaultListenAddress is where the service listens with nothing set. Loopback,
// deliberately: this is the user's private writing and the service has no auth
// of its own. A wildcard bind would put every journal entry on the network.
const DefaultListenAddress = "127.0.0.1:8322"

func wiring(key, variable, defaultValue, description string) servicesettings.Definition {
	return servicesettings.Definition{Key: key, EnvironmentVariable: variable, Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: defaultValue, Description: description}
}

// SettingDefinitions declares every environment variable this process reads,
// once. The command reads its configuration from it; GET /settings describes
// the service from it; and a test holds every os.Getenv in the repo to it.
//
// Nothing here is Editable, and nothing may become so while the service has no
// operator gate: GET /settings is as open as every other route.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		wiring(SettingListenAddress, "JOURNAL_STORE_ADDR", DefaultListenAddress,
			"The address the HTTP server listens on. Changing it moves the service, so everything that calls it must be told the new address. Keep it on loopback: the service has no auth."),
		{Key: SettingDataDirectory, EnvironmentVariable: "JOURNAL_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds journal-store.db. Changing it starts the service on whatever database is there, or an empty one; the old entries stay where they were."},
		wiring(SettingNoteboardURL, "JOURNAL_STORE_NOTEBOARD_URL", "http://localhost:8191",
			"noteboard. POST /entries/{id}/promote-todo creates the todo there."),
		wiring(SettingReferenceStoreURL, "JOURNAL_STORE_REFERENCE_STORE_URL", "http://127.0.0.1:8343",
			"reference-store. Its list of reference types (GET /types) says which type a ref: link's id is."),
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a value that does not parse, on a missing required secret, and on a set
// JOURNAL_STORE_ variable nobody declared.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, []string{OwnedEnvironmentVariablePrefix}, SettingDefinitions(), environment)
}

// RegisterSettingsHandler serves the registry at GET /settings. PUT
// /settings/{key} is not mounted: no setting is Editable.
func RegisterSettingsHandler(mux *http.ServeMux, registry *servicesettings.Registry) {
	mux.Handle("GET /settings", servicesettings.Handler(registry, "/settings"))
}
