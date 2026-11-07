package timeledgerstore

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description.
const ServiceName = "time-ledger-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting.
const OwnedEnvironmentVariablePrefix = "TIME_LEDGER_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress = "listen_address"
	SettingDataDirectory = "data_directory"
)

// DefaultListenAddress is loopback, deliberately: what the operator did is
// private and the service has no auth of its own.
const DefaultListenAddress = "127.0.0.1:8344"

// DefaultDataDir is where the database lives when the environment says nothing.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "time-ledger-store")
}

// SettingDefinitions declares every environment variable the service reads.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "TIME_LEDGER_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Keep it on loopback: the service has no auth."},
		{Key: SettingDataDirectory, EnvironmentVariable: "TIME_LEDGER_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds time-ledger.db."},
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

// Keys of time-ledger-review's settings.
const (
	SettingBridgeURL          = "bridge_url"
	SettingBridgeServiceToken = "bridge_service_token"
	SettingModelStoreURL      = "model_store_url"
)

// ReviewName is time-ledger-review's name in its settings.
const ReviewName = "time-ledger-review"

// ReviewSettingDefinitions declares every environment variable
// time-ledger-review reads. Models are asked through llm-bridge-server's
// POST /oneshot, and their list prices come from model-store.
func ReviewSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingBridgeURL, EnvironmentVariable: "LLMBRIDGE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://localhost:8160",
			Description: "Where llm-bridge-server answers POST /oneshot."},
		{Key: SettingBridgeServiceToken, EnvironmentVariable: "LLMBRIDGE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "llm-bridge-server's service token, sent only to LLMBRIDGE_URL and never printed."},
		{Key: SettingModelStoreURL, EnvironmentVariable: "MODEL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://localhost:8155",
			Description: "Where model-store answers GET /api/models, for each model's list price."},
	}
}

// NewReviewSettingsRegistry reads time-ledger-review's settings. It owns no
// prefix: LLMBRIDGE_ and MODEL_STORE_ belong to other services.
func NewReviewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ReviewName, nil, ReviewSettingDefinitions(), environment)
}
