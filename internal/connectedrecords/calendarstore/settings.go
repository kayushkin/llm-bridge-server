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

// Keys of google sync's settings.
const (
	SettingDashURL   = "dash_url"
	SettingDashToken = "dash_token"
)

// GoogleSyncName is calendar-store-sync-google's name in its settings.
const GoogleSyncName = "calendar-store-sync-google"

// GoogleSyncSettingDefinitions declares every environment variable
// calendar-store-sync-google reads. dash is the one way to Google Calendar on
// a host: the command reads a calendar there and hands the events to the
// store, so the service never holds the token.
func GoogleSyncSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingDashURL, EnvironmentVariable: "DASH_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://localhost:8101",
			Description: "Where dash answers: the calendar's events are read from its GET /api/calendar/google."},
		{Key: SettingDashToken, EnvironmentVariable: "DASH_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "dash's bearer token, sent only to DASH_URL and never printed."},
	}
}

// NewGoogleSyncSettingsRegistry reads calendar-store-sync-google's settings.
// It owns no prefix: DASH_ belongs to dash, CALENDAR_STORE_ to the service.
func NewGoogleSyncSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(GoogleSyncName, nil, GoogleSyncSettingDefinitions(), environment)
}
