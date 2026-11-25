package messagestore

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The store runs inside llm-bridge-server, whose settings are its own. These
// are the settings of the importer commands, clients of the store over HTTP.

// Keys of the importers' settings.
const (
	SettingStoreURL = "message_store_url"
	// SettingPhotoStoreURL is where the importers look up a chat's pictures.
	SettingPhotoStoreURL = "photo_store_url"
	// SettingJournalStoreURL is where the journal link command reads
	// transcripts.
	SettingJournalStoreURL = "journal_store_url"
	// SettingPeopleStoreURL is where the importers name a handle's person.
	SettingPeopleStoreURL = "people_store_url"
	// SettingMultichatURL and SettingMultichatToken are where
	// message-import-multichat reads multichat's message log, and the bearer
	// token it sends.
	SettingMultichatURL   = "multichat_url"
	SettingMultichatToken = "multichat_token"
	// SettingAuthStoreURL and SettingAuthStoreToken are where
	// message-import-discord-bot reads a bot's Discord token, and the bearer
	// token it sends there.
	SettingAuthStoreURL   = "auth_store_url"
	SettingAuthStoreToken = "auth_store_token"
)

// defaultStoreURL is where the standalone message-store answered. The
// variable names the store's address wherever it runs now.
const defaultStoreURL = "http://127.0.0.1:8337"

// StoreURLDefinition is the one setting every importer shares: where to post.
func StoreURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingStoreURL, EnvironmentVariable: "MESSAGE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: defaultStoreURL,
		Description: "Where message-store answers: the command posts there."}
}

// ImporterSettingDefinitions declares every variable an importer command
// reads. An importer that reads more adds its own definitions here, so the
// settings test sees them.
func ImporterSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{StoreURLDefinition(), PhotoStoreURLDefinition(), JournalStoreURLDefinition(),
		PeopleStoreURLDefinition(), MultichatURLDefinition(), MultichatTokenDefinition(), AuthStoreURLDefinition(), AuthStoreTokenDefinition()}
}

// AuthStoreURLDefinition is where auth-store answers: message-import-discord-bot
// reads a bot's Discord token there at the start of each run.
func AuthStoreURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingAuthStoreURL, EnvironmentVariable: "AUTH_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8303",
		Description: "Where auth-store answers: message-import-discord-bot reads the bot's Discord token there (provider discord)."}
}

// AuthStoreTokenDefinition is the bearer token auth-store asks for. Only
// message-import-discord-bot needs it, and it refuses to start without one.
func AuthStoreTokenDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingAuthStoreToken, EnvironmentVariable: "AUTH_STORE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
		Description: "The bearer token auth-store asks for. message-import-discord-bot sends it and never prints it."}
}

// PhotoStoreURLDefinition is where photo-store answers: the chat importers
// find a sent picture's photo id there by the file's sha256.
func PhotoStoreURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingPhotoStoreURL, EnvironmentVariable: "PHOTO_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8329",
		Description: "Where photo-store answers: an importer looks up each sent picture there by the original file's sha256."}
}

// JournalStoreURLDefinition is where journal-store answers: the link command
// reads its transcript entries there.
func JournalStoreURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingJournalStoreURL, EnvironmentVariable: "JOURNAL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8322",
		Description: "Where journal-store answers: message-link-journal-transcripts reads transcript entries there."}
}

// PeopleStoreURLDefinition is where people-store answers: an importer asks
// it whose a phone number or chat account is.
func PeopleStoreURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingPeopleStoreURL, EnvironmentVariable: "PEOPLE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8321",
		Description: "Where people-store answers: an importer asks GET /identities/resolve whose each handle is."}
}

// MultichatURLDefinition is where multichat answers, its routes under
// /api, as multichat-send reads the same variable.
func MultichatURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingMultichatURL, EnvironmentVariable: "MULTICHAT_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8402",
		Description: "Where multichat answers: message-import-multichat reads /api/message-log and /api/conversations there."}
}

// MultichatTokenDefinition is the bearer token multichat asks for. Only
// message-import-multichat needs it, and it refuses to start without one.
func MultichatTokenDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingMultichatToken, EnvironmentVariable: "MULTICHAT_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
		Description: "The bearer token multichat's API asks for. message-import-multichat sends it and never prints it."}
}

// NewImporterSettingsRegistry reads an importer's settings. It owns no
// prefix: the variables it reads are other services' too.
func NewImporterSettingsRegistry(importer string, environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(importer, nil, ImporterSettingDefinitions(), environment)
}
