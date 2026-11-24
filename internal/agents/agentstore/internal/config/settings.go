// Package config declares every environment variable agent-store's commands
// read, once, with llm-bridge's servicesettings. Each command reads its
// configuration from its own registry; the server serves its registry at
// GET /settings; and a test holds every os.Getenv in the repo to these lists.
//
// The library packages read no environment at all: llm-bridge-server and inber
// embed them, and a read there would be configuration of those processes that
// no declaration here could describe.
package config

import (
	"os"
	"path/filepath"

	agentstore "github.com/kayushkin/agent-store"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServerName is the standalone server's name in its own settings description.
const ServerName = "agent-store"

// SeedCommandName and MigrateInberCommandName name the two operator commands'
// registries, which only ever appear in their refusals.
const (
	SeedCommandName         = "agent-store seed"
	MigrateInberCommandName = "agent-store migrate-inber"
)

// OwnedEnvironmentVariablePrefixes are the prefixes of the variables that are
// agent-store's alone. A set variable carrying one that a command does not
// declare stops that command: it is a misspelling or a leftover, and either
// way someone believes it does something.
//
// Each is a full variable name, not "AGENT_STORE_". AGENT_STORE_URL is how
// fourteen callers find the server and AGENT_STORE_PATH is inber's, so one
// shared environment file carrying either would stop the server over a
// variable that was never meant for it. INBER_PATH and INBER_ROOT are declared
// and not owned: inber reads the same names.
var OwnedEnvironmentVariablePrefixes = []string{
	"AGENT_STORE_ADDR",
	"AGENT_STORE_DB",
	"AGENT_STORE_SCAN_INTERVAL_SECS",
}

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress             = "listen_address"
	SettingDatabasePath              = "database_path"
	SettingAutoScanIntervalInSeconds = "auto_scan_interval_seconds"
	SettingInberRepositoryPath       = "inber_repository_path"
)

// DefaultListenAddress binds the standalone server to loopback. Every caller
// on this host finds it at http://localhost:8300, and nothing off the host
// has a reason to reach an unauthenticated prompt editor.
const DefaultListenAddress = "127.0.0.1:8300"

// DefaultDatabasePath is the database every command opens with AGENT_STORE_DB
// unset. It is the library's own default, not a copy of it.
func DefaultDatabasePath() string {
	return agentstore.DefaultPath()
}

// DefaultInberRepositoryPath is ~/repos/inber, where both operator commands
// look for inber's agent definitions with nothing set.
func DefaultInberRepositoryPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "repos", "inber")
}

func databasePathDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingDatabasePath, EnvironmentVariable: "AGENT_STORE_DB", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDatabasePath(),
		Description: "The SQLite file that holds agents, tracked files and prompt sections. Changing it opens whatever database is there, or an empty one; the old records stay where they were. llm-bridge-server opens its own copy of this library and does not read it."}
}

// ServerSettingDefinitions declares every environment variable cmd/server
// reads.
//
// Nothing here is Editable, and nothing may become so while the standalone
// server has no operator gate: GET /settings is as open as every other route.
func ServerSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "AGENT_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The host:port the standalone server listens on. The default binds loopback only, because no route here asks for credentials and PUT /prompt-sections/{id} rewrites the prompt every agent receives; an address such as :8300 opens those routes to every interface. Changing it moves the server for everything that finds it through AGENT_STORE_URL."},
		databasePathDefinition(),
		{Key: SettingAutoScanIntervalInSeconds, EnvironmentVariable: "AGENT_STORE_SCAN_INTERVAL_SECS", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeInteger, Default: "900",
			Description: "Seconds between the server's scans of $HOME for edits to tracked files; 0 turns the scan off. A negative value stops the server at start. Changing it changes how soon an edit to a rendered prompt file is carried back."},
	}
}

// SeedCommandSettingDefinitions declares every environment variable cmd/seed
// reads.
func SeedCommandSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		databasePathDefinition(),
		{Key: SettingInberRepositoryPath, EnvironmentVariable: "INBER_ROOT", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultInberRepositoryPath(),
			Description: "The inber checkout whose agent files the seed reads. Changing it seeds from that checkout."},
	}
}

// MigrateInberCommandSettingDefinitions declares every environment variable
// cmd/migrate-inber reads. It opens the library's default database and reads
// no AGENT_STORE_DB.
func MigrateInberCommandSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingInberRepositoryPath, EnvironmentVariable: "INBER_PATH", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultInberRepositoryPath(),
			Description: "The inber checkout whose agents.json the migration reads. Changing it migrates from that checkout."},
	}
}

// NewServerSettingsRegistry reads the server's settings from environment. It
// fails on a value that does not parse and on a set owned variable nobody
// declared.
func NewServerSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServerName, OwnedEnvironmentVariablePrefixes, ServerSettingDefinitions(), environment)
}

// NewSeedCommandSettingsRegistry reads cmd/seed's settings from environment.
// It owns no prefix: a server variable in the operator's shell is not seed's
// to refuse.
func NewSeedCommandSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(SeedCommandName, nil, SeedCommandSettingDefinitions(), environment)
}

// NewMigrateInberCommandSettingsRegistry reads cmd/migrate-inber's settings
// from environment. It owns no prefix, as seed does not.
func NewMigrateInberCommandSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(MigrateInberCommandName, nil, MigrateInberCommandSettingDefinitions(), environment)
}
