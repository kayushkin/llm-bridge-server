package activitystore

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The store runs inside llm-bridge-server and reads no settings of its own:
// its records are in the server's Postgres (database.postgres_url) and its
// attachment bytes in the server's file-store. What is left here are the
// settings of activity-import-google, which posts to the store over HTTP.

// SettingActivityStoreURL is the key of the importer's one setting.
const SettingActivityStoreURL = "activity_store_url"

// defaultActivityStoreURL is where the standalone activity-store answered.
// ACTIVITY_STORE_URL names the store's address wherever it runs now.
const defaultActivityStoreURL = "http://127.0.0.1:8336"

// ImportGoogleSettingDefinitions declares every environment variable
// activity-import-google reads.
func ImportGoogleSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingActivityStoreURL, EnvironmentVariable: "ACTIVITY_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: defaultActivityStoreURL,
			Description: "Where activity-store answers: the command posts there."},
	}
}

// NewImportGoogleSettingsRegistry reads activity-import-google's settings. It
// owns no prefix.
func NewImportGoogleSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("activity-import-google", nil, ImportGoogleSettingDefinitions(), environment)
}
