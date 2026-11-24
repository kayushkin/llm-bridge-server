// Package settings declares every environment variable noteboard reads, once,
// with llm-bridge servicesettings. The command reads its configuration from
// the registry built here, GET /settings describes the service from it, and a
// test holds every os.Getenv in the repo to it.
package settings

import (
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "noteboard"

// OwnedEnvironmentVariablePrefixes are the prefixes of the variables that are
// this service's alone. A set variable carrying one that Definitions does not
// declare stops the service from starting: it is a misspelling or a leftover,
// and either way someone believes it does something.
//
// They are the two declared names and not "NOTEBOARD_", because NOTEBOARD_URL
// is what three dozen other programs on this host read to find noteboard. It
// is theirs, noteboard never reads it, and a shared environment file that
// carries it must not stop noteboard from starting. So NOTEBOARD_DB_PATH and
// NOTEBOARD_PORTS are refused, and NOTEBOARD_URL is not.
var OwnedEnvironmentVariablePrefixes = []string{"NOTEBOARD_PORT", "NOTEBOARD_LISTEN", "NOTEBOARD_DB"}

// Keys of the settings, as GET /settings names them.
const (
	ListenHost   = "listen_host"
	ListenPort   = "listen_port"
	DatabasePath = "database_path"
)

// DefaultListenHost is the interface the service listens on with nothing set:
// loopback only. No route here asks for credentials, so every interface would
// open the notes to anyone who can reach the host; callers use localhost.
const DefaultListenHost = "127.0.0.1"

// DefaultListenPort is the port the service listens on with nothing set.
const DefaultListenPort = "8191"

// DefaultDatabasePath is the database the service opens with NOTEBOARD_DB
// unset: ~/.noteboard/noteboard.db. It is empty when the home directory is
// unknown, which leaves the setting unset, and CheckRequired then refuses to
// start rather than open a database relative to wherever the process runs.
func DefaultDatabasePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".noteboard", "noteboard.db")
}

// Definitions declares every environment variable this process reads.
//
// Nothing here is Editable, and nothing may become so while the service has no
// operator gate: GET /settings is as open as every other route.
func Definitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: ListenHost, EnvironmentVariable: "NOTEBOARD_LISTEN_HOST", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenHost,
			Description: "The interface the HTTP server listens on. The default is loopback, because no route asks for credentials; an empty value or 0.0.0.0 opens every route to every interface."},
		{Key: ListenPort, EnvironmentVariable: "NOTEBOARD_PORT", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeInteger, Default: DefaultListenPort,
			Description: "The port the HTTP server listens on, on the interface NOTEBOARD_LISTEN_HOST names. Changing it moves the service, so everything that reads NOTEBOARD_URL must be told the new address."},
		{Key: DatabasePath, EnvironmentVariable: "NOTEBOARD_DB", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDatabasePath(), Required: true,
			Description: "The SQLite file that holds every note, todo and list. Changing it starts the service on whatever database is there, or a new empty one; the old items stay where they were."},
	}
}

// NewRegistry reads this service's settings from environment. It fails on a
// port that is not a whole number and on a set variable under an owned prefix
// that nobody declared.
func NewRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, OwnedEnvironmentVariablePrefixes, Definitions(), environment)
}
