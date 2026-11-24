package peoplestore

import (
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "people-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting: it is a misspelling or a leftover,
// and either way someone believes it does something.
const OwnedEnvironmentVariablePrefix = "PEOPLE_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress     = "listen_address"
	SettingDataDirectory     = "data_directory"
	SettingPrincipalStoreURL = "principal_store_url"
)

// DefaultListenAddress is where the service listens with nothing set.
// Loopback, deliberately: this service has no auth of its own, so a wildcard
// bind would put the operator's address book on the network.
const DefaultListenAddress = "127.0.0.1:8321"

// SettingDefinitions declares every environment variable the service reads,
// once. The command reads its configuration from it; GET /settings describes
// the service from it; and a test holds every os.Getenv in the repo to it.
//
// Nothing here is Editable, and nothing may become so while the service has no
// operator gate: GET /settings is as open as every other route.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "PEOPLE_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Changing it moves the service, so everything that calls it must be told the new address."},
		{Key: SettingDataDirectory, EnvironmentVariable: "PEOPLE_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds people-store.db. Changing it starts the service on whatever database is there, or an empty one; the old people stay where they were."},
		{Key: SettingPrincipalStoreURL, EnvironmentVariable: "PEOPLE_STORE_PRINCIPAL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "Where principal-store answers. Every principal_id written to a person is checked there with GET /principals/{id}; when it cannot answer, the write is refused with a 502."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a value that does not parse and on a set PEOPLE_STORE_ variable nobody
// declared.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, []string{OwnedEnvironmentVariablePrefix}, SettingDefinitions(), environment)
}

// RegisterSettingsHandler serves the registry at GET /settings. PUT
// /settings/{key} is not mounted: no setting is Editable.
func RegisterSettingsHandler(mux *http.ServeMux, registry *servicesettings.Registry) {
	mux.Handle("GET /settings", servicesettings.Handler(registry, "/settings"))
}

// The multichat importer, cmd/people-import-multichat, runs from a shell or a
// scheduler job whose environment carries many other services' variables, so
// its registry owns no prefix and fails only on what it cannot run without.

// ImporterName is the importer's name in its settings errors.
const ImporterName = "people-import-multichat"

// Keys of the importer's settings.
const (
	ImporterSettingPeopleStoreURL = "people_store_url"
	ImporterSettingMultichatURL   = "multichat_url"
	ImporterSettingAuthStoreURL   = "auth_store_url"
	ImporterSettingAuthStoreToken = "auth_store_token"
)

// ImporterSettingDefinitions declares every environment variable the importer
// reads.
func ImporterSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: ImporterSettingPeopleStoreURL, EnvironmentVariable: "PEOPLE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "Where people-store answers; suggestions are written through its POST /suggestions."},
		{Key: ImporterSettingMultichatURL, EnvironmentVariable: "MULTICHAT_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "Where the multichat server answers; contacts are read from its GET /api/contacts/unified."},
		{Key: ImporterSettingAuthStoreURL, EnvironmentVariable: "AUTH_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "Where auth-store answers. multichat's API token is resolved there from provider \"multichat\"."},
		{Key: ImporterSettingAuthStoreToken, EnvironmentVariable: "AUTH_STORE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "The bearer token auth-store takes."},
	}
}

// NewImporterSettingsRegistry reads the importer's settings from environment.
func NewImporterSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ImporterName, nil, ImporterSettingDefinitions(), environment)
}

// The Google Contacts importer, cmd/people-import-contacts, reads vCard files
// named on its command line and writes only to people-store, so it needs one
// setting. Like the multichat importer it owns no prefix.

// ContactsImporterName is the Google Contacts importer's name in its settings
// errors.
const ContactsImporterName = "people-import-contacts"

// ContactsImporterSettingDefinitions declares every environment variable the
// Google Contacts importer reads.
func ContactsImporterSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: ImporterSettingPeopleStoreURL, EnvironmentVariable: "PEOPLE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "Where people-store answers; persons, identities and suggestions are read there, and suggestions written through its POST /suggestions."},
	}
}

// NewContactsImporterSettingsRegistry reads the Google Contacts importer's
// settings from environment.
func NewContactsImporterSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ContactsImporterName, nil, ContactsImporterSettingDefinitions(), environment)
}
