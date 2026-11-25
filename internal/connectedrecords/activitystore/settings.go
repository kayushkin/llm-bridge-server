package activitystore

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "activity-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting.
const OwnedEnvironmentVariablePrefix = "ACTIVITY_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress = "listen_address"
	SettingDataDirectory = "data_directory"
)

// DefaultListenAddress is loopback, deliberately: this is what the user did
// online, and the service has no auth of its own. dash is the front door.
const DefaultListenAddress = "127.0.0.1:8336"

// DefaultDataDir is where the database and attachments live when the
// environment says nothing.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "activity-store")
}

// SettingDefinitions declares every environment variable the service reads,
// once. Nothing is Editable: GET /settings is as open as every other route.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "ACTIVITY_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Keep it on loopback: the service has no auth."},
		{Key: SettingDataDirectory, EnvironmentVariable: "ACTIVITY_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds activity-store.db and the attachments/ files. Changing it starts the service on whatever is there, or nothing."},
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

// SettingActivityStoreURL is the key of the importer's one setting.
const SettingActivityStoreURL = "activity_store_url"

// ImportGoogleSettingDefinitions declares every environment variable
// activity-import-google reads.
func ImportGoogleSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingActivityStoreURL, EnvironmentVariable: "ACTIVITY_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8336",
			Description: "Where activity-store answers: the command posts there."},
	}
}

// NewImportGoogleSettingsRegistry reads activity-import-google's settings. It
// owns no prefix: ACTIVITY_STORE_ belongs to the service.
func NewImportGoogleSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("activity-import-google", nil, ImportGoogleSettingDefinitions(), environment)
}
