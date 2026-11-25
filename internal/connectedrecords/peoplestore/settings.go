package peoplestore

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// people-store runs inside llm-bridge-server, whose settings it shares, so it
// declares none of its own. What is declared here is the settings of its two
// importers, which are HTTP clients of its routes and run on their own.

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
