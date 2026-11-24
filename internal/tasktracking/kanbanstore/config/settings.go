package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "kanban-store"

// OwnedEnvironmentVariablePrefixes are the prefixes of the variables that are
// this service's alone. A set variable carrying one that SettingDefinitions
// does not declare stops the service from starting: it is a misspelling or a
// leftover, and either way someone believes it does something.
//
// They stop short of "KANBAN_" on purpose. KANBAN_STORE_URL is how every other
// service finds this one, KANBAN_URL is how dash does, and the scheduler's
// kanban jobs read KANBAN_CLASSIFIER_MODEL and its neighbors. One shared
// environment file carrying any of them would stop the service over a variable
// that was never meant for it. So KANBAN_PORTS, KANBAN_DB_PATH and
// KANBAN_STORE_SERVICE_TOKENS are refused, and KANBAN_STORE_URL is not.
var OwnedEnvironmentVariablePrefixes = []string{
	"KANBAN_PORT",
	"KANBAN_LISTEN",
	"KANBAN_DB",
	"KANBAN_NOTEBOARD",
	"KANBAN_STORE_SERVICE_TOKEN",
}

// Keys of the settings, as GET /settings names them.
const (
	SettingListenHost             = "listen_host"
	SettingListenPort             = "listen_port"
	SettingDatabasePath           = "database_path"
	SettingNoteboardURL           = "noteboard_url"
	SettingPrincipalStoreURL      = "principal_store_url"
	SettingLLMBridgeServerURL     = "llm_bridge_server_url"
	SettingBundleStoreURL         = "bundle_store_url"
	SettingFileStoreURL           = "file_store_url"
	SettingFileStoreServiceToken  = "file_store_service_token"
	SettingMultichatURL           = "multichat_url"
	SettingAuthStoreURL           = "auth_store_url"
	SettingAuthStoreToken         = "auth_store_token"
	SettingServiceToken           = "service_token"
	SettingGrantStoreURL          = "grant_store_url"
	SettingGrantStoreServiceToken = "grant_store_service_token"
	SettingReferenceStoreURL      = "reference_store_url"
)

// The values in force with nothing set. The shipped systemd unit sets the
// addresses explicitly; the defaults exist so a bare `make run` on the same
// host works.
const (
	// DefaultListenHost is loopback: every caller (the gateway, dash,
	// grant-store, the scheduler's jobs) is on this host.
	DefaultListenHost         = "127.0.0.1"
	DefaultListenPort         = 8305
	DefaultNoteboardURL       = "http://localhost:8191"
	DefaultPrincipalStoreURL  = "http://127.0.0.1:8314"
	DefaultReferenceStoreURL  = "http://127.0.0.1:8343"
	DefaultLLMBridgeServerURL = "http://127.0.0.1:8160"
	DefaultBundleStoreURL     = "http://127.0.0.1:8307"
	DefaultAuthStoreURL       = "http://127.0.0.1:8303"
)

// MinimumServiceTokenLength is the shortest service token the store accepts,
// its own or grant-store's: a short or empty token would match requests that
// should not be unrestricted.
const MinimumServiceTokenLength = 32

// DefaultDatabasePath is the database the service opens with KANBAN_DB unset:
// ~/.kanban-store/kanban-store.db. It is empty when the home directory is
// unknown, which leaves the setting unset, and CheckRequired then refuses to
// start rather than open .kanban-store/kanban-store.db under the working
// directory.
func DefaultDatabasePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kanban-store", "kanban-store.db")
}

// SettingDefinitions declares every environment variable this process reads.
//
// Nothing here is Editable: the service stores no setting of its own, so only
// GET /settings is mounted.
//
// Every secret stays a string. servicesettings.New quotes a value it cannot
// parse, so a secret of any other type could reach a log through its own error.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenHost, EnvironmentVariable: "KANBAN_LISTEN_HOST", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenHost,
			Description: "The interface the HTTP server listens on. The default is loopback, since every caller is on this host; an empty value or 0.0.0.0 listens on every interface."},
		{Key: SettingListenPort, EnvironmentVariable: "KANBAN_PORT", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeInteger, Default: fmt.Sprint(DefaultListenPort),
			Description: "The port the HTTP server listens on, on the interface KANBAN_LISTEN_HOST names. Changing it moves the service, so every KANBAN_STORE_URL (the gateway, dash, grant-store, producer, the scheduler's kanban jobs) must be told the new address."},
		{Key: SettingDatabasePath, EnvironmentVariable: "KANBAN_DB", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDatabasePath(), Required: true,
			Description: "The SQLite file that holds boards, columns, card placement, links, notes and triggers. Changing it starts the service on whatever database is there, or a new empty one; the old rows stay where they were."},
		{Key: SettingNoteboardURL, EnvironmentVariable: "KANBAN_NOTEBOARD_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultNoteboardURL,
			Description: "Where noteboard answers. A card's title, body, tags and status are a noteboard item, so a wrong address leaves every board with cards it cannot describe."},
		{Key: SettingPrincipalStoreURL, EnvironmentVariable: "PRINCIPAL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultPrincipalStoreURL,
			Description: "Where principal-store answers. The calling principal is read there on every request, and a principal is checked there before it is assigned to a card; a wrong address answers every principal's request 502."},
		{Key: SettingReferenceStoreURL, EnvironmentVariable: "REFERENCE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultReferenceStoreURL,
			Description: "Where reference-store answers. It owns the list of reference types, which GET /api/entity-types serves and a ticket's source_entity_type is checked against; a wrong address answers both 502."},
		{Key: SettingLLMBridgeServerURL, EnvironmentVariable: "LLM_BRIDGE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultLLMBridgeServerURL,
			Description: "Where llm-bridge-server answers. A board's default agent and default instance are checked there before either is written. It is the name grant-store, llm-bridge-adapter and dash already read, so one host has one name for the owner."},
		{Key: SettingBundleStoreURL, EnvironmentVariable: "BUNDLE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultBundleStoreURL,
			Description: "Where bundle-store answers. A board's default bundle is checked there before it is written."},
		{Key: SettingFileStoreURL, EnvironmentVariable: "FILE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "Where file-store answers; it keeps a card's attachments. Unset is a real answer: the store then has no attachments, and every attachment route says so with a 503 rather than report that a card has none."},
		{Key: SettingFileStoreServiceToken, EnvironmentVariable: "FILE_STORE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The token file-store asks of every caller. The service refuses to start when FILE_STORE_URL is set and this is not. It reads every file, so it reaches the unit through a host-local drop-in from ~/.config/file-store-tokens.env, never the tracked unit and never the shared principal-gating file, whose variables every agent session inherits."},
		{Key: SettingMultichatURL, EnvironmentVariable: "MULTICHAT_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "Where multichat answers; message triggers send through it. Unset is a real answer: triggers still match and render, and each delivery is recorded as not_configured, so a trigger is seen firing into nothing rather than silently doing nothing."},
		{Key: SettingAuthStoreURL, EnvironmentVariable: "AUTH_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultAuthStoreURL,
			Description: "Where auth-store answers. multichat's API token is resolved there (provider \"multichat\") when a message trigger sends; nothing else reads it."},
		{Key: SettingAuthStoreToken, EnvironmentVariable: "AUTH_STORE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The bearer token auth-store takes. The service refuses to start when MULTICHAT_URL is set and this is not."},
		{Key: SettingServiceToken, EnvironmentVariable: "KANBAN_STORE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "The token internal services present in X-Kanban-Store-Service-Token to be past every board check. At least 32 characters, or the service refuses to start. There is no off switch: a store that cannot authorize a caller must not serve boards."},
		{Key: SettingGrantStoreURL, EnvironmentVariable: "GRANT_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "Where grant-store answers. Board access is read from it on every principal's request. It has no default: a guessed one would start a store that believes it is checking callers and is not."},
		{Key: SettingGrantStoreServiceToken, EnvironmentVariable: "GRANT_STORE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "The token grant-store asks of every caller. At least 32 characters, or the service refuses to start; without it every board read is a 502."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a set variable under an owned prefix that nobody declared, and on a port
// that is not a whole number.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, OwnedEnvironmentVariablePrefixes, SettingDefinitions(), environment)
}

// Config is what the command starts the service with.
type Config struct {
	ListenHost   string
	ListenPort   int
	DatabasePath string

	NoteboardURL       string
	PrincipalStoreURL  string
	ReferenceStoreURL  string
	LLMBridgeServerURL string
	BundleStoreURL     string

	// FileStoreURL is empty when the store has no attachments. When it is set,
	// FileStoreServiceToken is too.
	FileStoreURL          string
	FileStoreServiceToken string

	// MultichatURL is empty when message triggers send nowhere. When it is
	// set, AuthStoreToken is too.
	MultichatURL   string
	AuthStoreURL   string
	AuthStoreToken string

	// What every kanban-store needs before it can answer a request: the token
	// internal services present, where board grants are read, and the token
	// grant-store itself wants.
	ServiceToken           string
	GrantStoreURL          string
	GrantStoreServiceToken string
}

// Load reads the command's configuration from registry. It fails on a service
// token under MinimumServiceTokenLength, and on an address set without the
// token its owner asks for. An error names the variable and never its value.
func Load(registry *servicesettings.Registry) (Config, error) {
	loaded := Config{
		ListenHost:             registry.String(SettingListenHost),
		ListenPort:             registry.Integer(SettingListenPort),
		DatabasePath:           registry.String(SettingDatabasePath),
		NoteboardURL:           registry.String(SettingNoteboardURL),
		PrincipalStoreURL:      registry.String(SettingPrincipalStoreURL),
		ReferenceStoreURL:      registry.String(SettingReferenceStoreURL),
		LLMBridgeServerURL:     registry.String(SettingLLMBridgeServerURL),
		BundleStoreURL:         registry.String(SettingBundleStoreURL),
		FileStoreURL:           registry.String(SettingFileStoreURL),
		FileStoreServiceToken:  registry.String(SettingFileStoreServiceToken),
		MultichatURL:           registry.String(SettingMultichatURL),
		AuthStoreURL:           registry.String(SettingAuthStoreURL),
		AuthStoreToken:         registry.String(SettingAuthStoreToken),
		ServiceToken:           registry.String(SettingServiceToken),
		GrantStoreURL:          registry.String(SettingGrantStoreURL),
		GrantStoreServiceToken: registry.String(SettingGrantStoreServiceToken),
	}
	if len(loaded.ServiceToken) < MinimumServiceTokenLength {
		return Config{}, fmt.Errorf("KANBAN_STORE_SERVICE_TOKEN must be at least %d characters: internal services present it, and without it every request that omits the header would be unrestricted", MinimumServiceTokenLength)
	}
	if loaded.GrantStoreURL == "" {
		return Config{}, fmt.Errorf("GRANT_STORE_URL is required: board access is read from grant-store on every request")
	}
	if len(loaded.GrantStoreServiceToken) < MinimumServiceTokenLength {
		return Config{}, fmt.Errorf("GRANT_STORE_SERVICE_TOKEN must be at least %d characters: grant-store gates its own routes and answers a call without it 401", MinimumServiceTokenLength)
	}
	if loaded.FileStoreURL != "" && loaded.FileStoreServiceToken == "" {
		return Config{}, fmt.Errorf("FILE_STORE_URL is set but FILE_STORE_SERVICE_TOKEN is not: file-store takes no call without it")
	}
	if loaded.MultichatURL != "" && loaded.AuthStoreToken == "" {
		return Config{}, fmt.Errorf("MULTICHAT_URL is set but AUTH_STORE_TOKEN is not: message triggers resolve multichat's API token from auth-store provider \"multichat\"")
	}
	return loaded, nil
}
