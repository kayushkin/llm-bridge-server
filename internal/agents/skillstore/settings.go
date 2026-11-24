package skillstore

import (
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "skill-store"

// OwnedEnvironmentVariablePrefixes are the prefixes of the variables that are
// this service's alone. A set variable carrying one of them that
// SettingDefinitions does not declare stops the process from starting: it is a
// misspelling or a leftover, and either way someone believes it does something.
//
// These are the three full variable names, not the shared prefix SKILL_STORE_,
// and the reason was measured on 2026-09-20. SKILL_STORE_URL is how the rest of
// the fleet finds this service: 62 references across the repos, and grant-store's
// unit sets it with Environment=. It is not in the server's own environment
// today, but cmd/seed builds this same registry and runs from an operator or
// agent shell, which inherits llm-bridge-server's environment — and
// llm-bridge-server declares SKILL_STORE_URL as one of its own settings, so
// setting it there is a supported act that would then stop cmd/seed. Owning the
// full names instead costs nothing that matters: servicesettings matches a
// prefix with HasPrefix, so SKILL_STORE_ADDRESS, SKILL_STORE_DATA_DIRECTORY and
// SKILL_STORE_INSTALL_DIRS are all still refused, while SKILL_STORE_URL passes.
// A misspelling that shortens a name, such as SKILL_STORE_ADR, is not caught.
var OwnedEnvironmentVariablePrefixes = []string{
	"SKILL_STORE_ADDR",
	"SKILL_STORE_DATA_DIR",
	"SKILL_STORE_INSTALL_DIR",
}

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress    = "listen_address"
	SettingDataDirectory    = "data_directory"
	SettingInstallDirectory = "install_directory"
)

// DefaultListenAddress is where the service listens with nothing set: loopback,
// because the service has no auth of its own.
const DefaultListenAddress = "127.0.0.1:8301"

// SettingDefinitions declares every environment variable this process reads,
// once. The two commands read their configuration from it; GET /settings
// describes the service from it; and a test holds every os.Getenv in the repo to
// it, so a variable cannot be read without being declared here.
//
// Nothing here is Editable, and nothing may become so while the service has no
// operator gate: GET /settings is as open as every other route.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "SKILL_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Changing it moves the service, so everything that calls it must be told the new address."},
		{Key: SettingDataDirectory, EnvironmentVariable: "SKILL_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds skill-store.db, the skill payloads under skills/ and the cloned source repos under repos/. Changing it starts the service on whatever database is there, or an empty one; the old sources and skills stay where they were."},
		{Key: SettingInstallDirectory, EnvironmentVariable: "SKILL_STORE_INSTALL_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultInstallDir(),
			Description: "The directory an installed skill is copied into, and the only place an uninstall will erase from. Changing it does not move what is already installed: those files stay where they were written, and the installation rows still point at them."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a value that does not parse and on a set variable under an owned prefix
// that nobody declared.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, OwnedEnvironmentVariablePrefixes, SettingDefinitions(), environment)
}

// RegisterSettingsHandler serves the registry at GET /settings, the way every
// service serves its settings. PUT /settings/{key} is not mounted: no setting
// is Editable, so there is nothing a write could change.
func RegisterSettingsHandler(mux *http.ServeMux, registry *servicesettings.Registry) {
	mux.Handle("GET /settings", servicesettings.Handler(registry, "/settings"))
}
