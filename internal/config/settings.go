// Package config declares every environment variable the scheduler service
// (cmd/scheduler) reads, once, with llm-bridge servicesettings. The command
// reads its configuration from the registry built here, GET /settings describes
// the service from it, and a test holds every os.Getenv in the service's code
// to it.
//
// It covers the service and the packages it imports, not the job binaries in
// cmd/: each of those starts in a job's shell, reads its own variables, and is
// converted separately.
package config

import (
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "scheduler"

// OwnedEnvironmentVariablePrefixes are the prefixes of the variables that are
// this service's alone. A set variable carrying one that SettingDefinitions
// does not declare stops the service from starting: it is a misspelling or a
// leftover, and either way someone believes it does something.
//
// They are three full variable names, not "SCHEDULER_". SCHEDULER_URL is how
// every job and agent finds the scheduler, and every job is a child of this
// process; owning the whole prefix would stop the scheduler the day its own
// unit set SCHEDULER_URL for its jobs. So SCHEDULER_DB_PATH and SCHEDULER_PORTS
// are refused, and SCHEDULER_URL is not.
var OwnedEnvironmentVariablePrefixes = []string{"SCHEDULER_PORT", "SCHEDULER_DB", "SCHEDULER_TOKEN"}

// Keys of the settings, as GET /settings names them.
const (
	SettingPort         = "port"
	SettingDatabasePath = "database_path"
	SettingToken        = "token"
	SettingNATSURL      = "nats_url"
	SettingBridgeURL    = "llm_bridge_url"
	SettingNoteboardURL = "noteboard_url"
)

// The values in force with nothing set.
const (
	DefaultPort         = "8092"
	DefaultNATSURL      = "nats://localhost:4222"
	DefaultBridgeURL    = "http://localhost:8160"
	DefaultNoteboardURL = "http://localhost:8191"
)

// Config is the configuration the service runs on, read from a Registry.
type Config struct {
	Port         int
	DatabasePath string
	Token        string
	NATSURL      string
	BridgeURL    string
	NoteboardURL string
}

// DefaultDatabasePath is the database the service opens with SCHEDULER_DB
// unset: ~/.scheduler/scheduler.db. It is empty when the home directory is
// unknown, which leaves the setting unset, and CheckRequired then refuses to
// start rather than open .scheduler/scheduler.db in the working directory.
func DefaultDatabasePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".scheduler", "scheduler.db")
}

// SettingDefinitions declares every environment variable the service reads.
//
// Nothing here is Editable: GET /settings sits behind the same bearer as
// /api/*, which is no gate at all while SCHEDULER_TOKEN is unset.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingPort, EnvironmentVariable: "SCHEDULER_PORT", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeInteger, Default: DefaultPort,
			Description: "The port the HTTP API listens on, on every interface. Changing it moves the service: every SCHEDULER_URL, dash's scheduler proxy and healthcheck must be told the new port."},
		{Key: SettingDatabasePath, EnvironmentVariable: "SCHEDULER_DB", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDatabasePath(), Required: true,
			Description: "The SQLite file that holds every job and every run. Changing it starts the scheduler on whatever jobs that file holds, or none; the old jobs stop firing."},
		{Key: SettingToken, EnvironmentVariable: "SCHEDULER_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The bearer every route but /health requires. Unset, the API and this page answer anyone who can reach the port; set, every caller must send it."},
		{Key: SettingNATSURL, EnvironmentVariable: "NATS_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultNATSURL,
			Description: "The NATS server for the bus commands, agent jobs and logging. The service does not start when it cannot reach it. Jobs inherit this variable."},
		{Key: SettingBridgeURL, EnvironmentVariable: "LLMBRIDGE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultBridgeURL,
			Description: "Where llm-bridge-server answers. An agent job asks it for the harness instance to start its first turn on; a wrong address fails every new agent job."},
		{Key: SettingNoteboardURL, EnvironmentVariable: "NOTEBOARD_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultNoteboardURL,
			Description: "The noteboard address written into an agent job's prompt, for reading and rewriting its workspace. A wrong address sends every such agent to a memory it cannot reach. Jobs inherit this variable."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a set variable under an owned prefix that nobody declared.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, OwnedEnvironmentVariablePrefixes, SettingDefinitions(), environment)
}

// Load reads the service's configuration from registry.
func Load(registry *servicesettings.Registry) Config {
	return Config{
		Port:         registry.Integer(SettingPort),
		DatabasePath: registry.String(SettingDatabasePath),
		Token:        registry.String(SettingToken),
		NATSURL:      registry.String(SettingNATSURL),
		BridgeURL:    registry.String(SettingBridgeURL),
		NoteboardURL: registry.String(SettingNoteboardURL),
	}
}
