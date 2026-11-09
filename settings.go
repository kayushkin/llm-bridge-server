package budgetstore

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description.
const ServiceName = "budget-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting.
const OwnedEnvironmentVariablePrefix = "BUDGET_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress    = "listen_address"
	SettingDataDirectory    = "data_directory"
	SettingAuthStoreURL     = "auth_store_url"
	SettingAuthStoreToken   = "auth_store_token"
	SettingPlaidEnvironment = "plaid_environment"
)

// DefaultListenAddress is where the service listens with nothing set.
// Loopback, deliberately: this service has no auth of its own and holds the
// operator's finances.
const DefaultListenAddress = "127.0.0.1:8340"

// DefaultDataDir is where the database lives when nothing says otherwise.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "share", "budget-store")
}

// SettingDefinitions declares every environment variable the service reads.
// Nothing is Editable: GET /settings is as open as every other route.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "BUDGET_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Changing it moves the service, so everything that calls it must be told."},
		{Key: SettingDataDirectory, EnvironmentVariable: "BUDGET_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds budget-store.db. Changing it starts the service on whatever database is there, or an empty one."},
		{Key: SettingAuthStoreURL, EnvironmentVariable: "BUDGET_STORE_AUTH_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "Where auth-store answers. It holds the Plaid keys and every bank's Plaid access token; with it unset the Plaid routes answer 503."},
		{Key: SettingAuthStoreToken, EnvironmentVariable: "BUDGET_STORE_AUTH_STORE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "auth-store's bearer token, for reading the Plaid keys and saving and reading each bank's access token. Set it in a host-local drop-in, never the tracked unit."},
		{Key: SettingPlaidEnvironment, EnvironmentVariable: "BUDGET_STORE_PLAID_ENVIRONMENT", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeString,
			Description: "Which Plaid keys to use: the auth-store credential of provider plaid whose account is this name (sandbox for test data, production for real banks). Empty turns the Plaid routes off (503). The credential's base_url says where that Plaid answers."},
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
