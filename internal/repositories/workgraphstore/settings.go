package workgraphstore

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "work-graph-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting.
const OwnedEnvironmentVariablePrefix = "WORK_GRAPH_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress = "listen_address"
	SettingDataDirectory = "data_directory"
	SettingSpoolPath     = "spool_path"
	SettingRepoStoreURL  = "repo_store_url"
)

// DefaultListenAddress is where the service listens with nothing set.
const DefaultListenAddress = "127.0.0.1:8319"

// DefaultRepoStoreURL is repo-store as its unit sets it.
const DefaultRepoStoreURL = "http://localhost:8306"

// DefaultDataDir is where the database lives with nothing set.
func DefaultDataDir() string {
	return filepath.Join(homeDirectory(), ".local", "share", "work-graph-store")
}

// DefaultSpoolPath is the file the git hook appends to and the service reads.
// The hook reads no settings, so this function is the one place both agree on.
func DefaultSpoolPath() string {
	return filepath.Join(homeDirectory(), ".local", "state", "work-graph-store", "ref-updates.jsonl")
}

func homeDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}

// SettingDefinitions declares every environment variable this process reads,
// once. Nothing is Editable: the service has no operator gate.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "WORK_GRAPH_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Changing it moves the service, so everything that calls it must be told the new address."},
		{Key: SettingDataDirectory, EnvironmentVariable: "WORK_GRAPH_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds work-graph-store.db. Changing it starts the service on whatever database is there, or an empty one."},
		{Key: SettingSpoolPath, EnvironmentVariable: "WORK_GRAPH_STORE_SPOOL_PATH", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultSpoolPath(),
			Description: "The file the git hook appends ref updates to. The hook always writes to the default, so changing this makes the service read a file nobody writes."},
		{Key: SettingRepoStoreURL, EnvironmentVariable: "WORK_GRAPH_STORE_REPO_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultRepoStoreURL,
			Description: "repo-store's base URL. The service asks it which repo a git directory belongs to, and stores repo-store's id."},
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
