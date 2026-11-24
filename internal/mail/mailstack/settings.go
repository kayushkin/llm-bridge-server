package mailstack

import (
	"os"
	"strconv"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/accounts"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/localmail"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "mailstack"

// OwnedEnvironmentVariablePrefixes are the variables that are this service's
// alone. A set variable carrying one of them that SettingDefinitions does not
// declare stops the service from starting: it is a misspelling or a leftover,
// and either way someone believes it does something.
//
// They are the four full names, not MAILSTACK_. MAILSTACK_URL is what callers
// read to find this service, and 49 places across the fleet name it (measured
// 2026-09-20); one shared environment file carrying it would stop the mail
// service. The cost is that a misspelling such as MAILSTACK_ADDR is not
// caught, while MAILSTACK_DB_PATH and MAILSTACK_TOKENS still are.
var OwnedEnvironmentVariablePrefixes = []string{"MAILSTACK_PORT", "MAILSTACK_DB", "MAILSTACK_LOCAL_DB", "MAILSTACK_TOKEN"}

// Keys of the settings, as GET /settings names them.
const (
	SettingListenPort            = "listen_port"
	SettingAccountsDatabasePath  = "accounts_database_path"
	SettingLocalMailDatabasePath = "local_mail_database_path"
	SettingBearerToken           = "bearer_token"
	SettingAuthStoreURL          = "auth_store_url"
	SettingAuthStoreBearerToken  = "auth_store_bearer_token"
	SettingFileStoreURL          = "file_store_url"
	SettingFileStoreServiceToken = "file_store_service_token"
	// The three stores a person's access is read from. See principal_access.go.
	SettingPrincipalStoreURL       = "principal_store_url"
	SettingGrantStoreURL           = "grant_store_url"
	SettingGrantStoreServiceToken  = "grant_store_service_token"
	SettingKanbanStoreURL          = "kanban_store_url"
	SettingKanbanStoreServiceToken = "kanban_store_service_token"
)

// DefaultListenPort is the loopback port the service listens on with nothing
// set.
const DefaultListenPort = 8195

// DefaultAuthStoreURL is where auth-store answers with AUTH_STORE_URL unset.
const DefaultAuthStoreURL = "http://127.0.0.1:8303"

// defaultUnderHome is path when the home directory is known and empty when it
// is not. accounts.DefaultDBPath and localmail.DefaultDBPath drop that error
// and return a path relative to the working directory; an empty default leaves
// the setting unset instead, and the command refuses to start without a
// database path.
func defaultUnderHome(path string) string {
	if _, err := os.UserHomeDir(); err != nil {
		return ""
	}
	return path
}

// SettingDefinitions declares every environment variable this process reads,
// once. The command reads its configuration from it; GET /settings describes
// the service from it; and a test holds every os.Getenv in the repo to it, so
// a variable cannot be read without being declared here.
//
// The two tokens are strings on purpose: a string always parses, so no error
// from servicesettings.New can quote one. Nothing here is Editable: the service
// stores no setting, so there is nothing a write could change.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenPort, EnvironmentVariable: "MAILSTACK_PORT", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeInteger, Default: strconv.Itoa(DefaultListenPort),
			Description: "The loopback port the HTTP server listens on; the --port flag overrides it. Changing it moves the service, so everything that calls it must be told the new address."},
		{Key: SettingAccountsDatabasePath, EnvironmentVariable: "MAILSTACK_DB", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: defaultUnderHome(accounts.DefaultDBPath()),
			Description: "The accounts database: mail accounts, IMAP credentials and the message index. The --db flag overrides it. Changing it starts the service on whatever database is there, or an empty one; the old accounts and index stay where they were."},
		{Key: SettingLocalMailDatabasePath, EnvironmentVariable: "MAILSTACK_LOCAL_DB", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: defaultUnderHome(localmail.DefaultDBPath()),
			Description: "The database that holds synthetic mail for local accounts, created only when a local account is first used. The --local-db flag overrides it. Changing it leaves the old synthetic mail where it was."},
		{Key: SettingBearerToken, EnvironmentVariable: "MAILSTACK_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The bearer token every /api/* route and GET /settings demand. Unset leaves all of them open to any caller that can reach the port, and the service says so in its log at startup. Changing it locks out every caller still holding the old one."},
		{Key: SettingAuthStoreURL, EnvironmentVariable: "AUTH_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultAuthStoreURL,
			Description: "Where auth-store answers. Gmail and Outlook access tokens are fetched from it on every provider call; a wrong address fails every Gmail and Outlook request and leaves IMAP and local accounts working."},
		{Key: SettingAuthStoreBearerToken, EnvironmentVariable: "AUTH_STORE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The bearer token presented to auth-store. A wrong or missing one makes auth-store refuse every token fetch when it has a token of its own set, which fails every Gmail and Outlook request."},
		{Key: SettingFileStoreURL, EnvironmentVariable: "FILE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "Where file-store answers. Imported attachments keep their bytes there. Unset, an import that carries an attachment is refused with 503 and reading one answers 503; mail without attachments is unaffected."},
		{Key: SettingFileStoreServiceToken, EnvironmentVariable: "FILE_STORE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The token file-store asks of every caller. The service refuses to start when FILE_STORE_URL is set and this is not. It reads every file, so it reaches the unit through a host-local drop-in from ~/.config/file-store-tokens.env, never the tracked unit and never the shared principal-gating file, whose variables every agent session inherits."},
		{Key: SettingPrincipalStoreURL, EnvironmentVariable: "PRINCIPAL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8314",
			Description: "Where principal-store answers. A request that names a person in X-Principal-Id is checked there, on every call, for an active human and whether they are an administrator."},
		{Key: SettingGrantStoreURL, EnvironmentVariable: "GRANT_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8315",
			Description: "Where grant-store answers. A person's can_read_mail and can_import_mail grants on mail accounts are read there on every call."},
		{Key: SettingGrantStoreServiceToken, EnvironmentVariable: "GRANT_STORE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The token mailstack presents to grant-store. Unset, together with the kanban-store token, mailstack serves services only and refuses every request that names a person with 503."},
		{Key: SettingKanbanStoreURL, EnvironmentVariable: "KANBAN_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8305",
			Description: "Where kanban-store answers. It says which messages a person sees through a ticket they may view."},
		{Key: SettingKanbanStoreServiceToken, EnvironmentVariable: "KANBAN_STORE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The token mailstack presents to kanban-store. Unset, together with the grant-store token, mailstack serves services only and refuses every request that names a person with 503."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a port that is not a whole number and on a set variable under an owned
// name that nobody declared.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, OwnedEnvironmentVariablePrefixes, SettingDefinitions(), environment)
}
