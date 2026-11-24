package repostore

import (
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "repo-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting: it is a misspelling or a leftover,
// and either way someone believes it does something.
const OwnedEnvironmentVariablePrefix = "REPO_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress            = "listen_address"
	SettingDataDirectory            = "data_directory"
	SettingPullRequestSyncInterval  = "pull_request_sync_interval"
	SettingGitHubCommandLineProgram = "github_command_line_program"
)

// DefaultListenAddress is where the service listens with nothing set.
const DefaultListenAddress = "127.0.0.1:8306"

// DefaultPullRequestSyncInterval is how often the service asks GitHub for the
// pull requests of every repo with a github_url, with nothing set.
const DefaultPullRequestSyncInterval = "10m"

// DefaultGitHubCommandLineProgram is the gh program the service runs to reach
// GitHub, with nothing set: whatever "gh" is on PATH. A systemd user unit's PATH
// often lacks the directory gh is installed in, so the unit sets the full path.
const DefaultGitHubCommandLineProgram = "gh"

// SettingDefinitions declares every environment variable this process reads,
// once. The command reads its configuration from it; GET /settings describes
// the service from it; and a test holds every os.Getenv in the repo to it, so
// a variable cannot be read without being declared here.
//
// Nothing here is Editable, and nothing may become so while the service has no
// operator gate: GET /settings is as open as every other route.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "REPO_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Changing it moves the service, so everything that calls it must be told the new address."},
		{Key: SettingDataDirectory, EnvironmentVariable: "REPO_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds repo-store.db. Changing it starts the service on whatever database is there, or an empty one; the old repos stay where they were."},
		{Key: SettingPullRequestSyncInterval, EnvironmentVariable: "REPO_STORE_PULL_REQUEST_SYNC_INTERVAL", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: DefaultPullRequestSyncInterval,
			Description: "How long the service waits between asking GitHub for the pull requests of every repo with a github_url. Shorter makes GET /pull-requests fresher and spends more of the GitHub account's hourly request allowance: each pass makes at least one request per repo."},
		{Key: SettingGitHubCommandLineProgram, EnvironmentVariable: "REPO_STORE_GITHUB_CLI", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultGitHubCommandLineProgram,
			Description: "The gh program the service runs (`gh api …`) to read pull requests from GitHub, as the account gh is logged in to under the service's HOME. The service will not start if it cannot find it. Changing it changes which gh, and so possibly which GitHub account, the service reads as."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a value that does not parse and on a set REPO_STORE_ variable nobody
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
