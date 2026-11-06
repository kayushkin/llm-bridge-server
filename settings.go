package calendarstore

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description.
const ServiceName = "calendar-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting.
const OwnedEnvironmentVariablePrefix = "CALENDAR_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress  = "listen_address"
	SettingDataDirectory  = "data_directory"
	SettingPeopleStoreURL = "people_store_url"
)

// DefaultListenAddress is loopback, deliberately: a calendar is private and
// the service has no auth of its own.
const DefaultListenAddress = "127.0.0.1:8342"

// DefaultDataDir is where the database lives when the environment says nothing.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "calendar-store")
}

// SettingDefinitions declares every environment variable the service reads.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "CALENDAR_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Keep it on loopback: the service has no auth."},
		{Key: SettingDataDirectory, EnvironmentVariable: "CALENDAR_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds calendar-store.db."},
		{Key: SettingPeopleStoreURL, EnvironmentVariable: "CALENDAR_STORE_PEOPLE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8321",
			Description: "Where people-store answers: an import asks it whose each attendee's address is, and a link to a person is checked against it."},
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
