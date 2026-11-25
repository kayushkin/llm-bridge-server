package messagestore

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "message-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting.
const OwnedEnvironmentVariablePrefix = "MESSAGE_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress = "listen_address"
	SettingDataDirectory = "data_directory"
	SettingStoreURL      = "message_store_url"
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

// DefaultListenAddress is loopback, deliberately: these are private messages,
// and the service has no auth of its own. dash is the front door.
const DefaultListenAddress = "127.0.0.1:8337"

// DefaultDataDir is where the database lives when the environment says
// nothing.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "message-store")
}

// SettingDefinitions declares every environment variable the service reads,
// once. Nothing is Editable: GET /settings is as open as every other route.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "MESSAGE_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Keep it on loopback: the service has no auth."},
		{Key: SettingDataDirectory, EnvironmentVariable: "MESSAGE_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds message-store.db. Changing it starts the service on whatever is there, or nothing."},
	}
}

// NewSettingsRegistry reads this service's settings from environment.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, []string{OwnedEnvironmentVariablePrefix}, SettingDefinitions(), environment)
}

// RegisterSettingsHandler serves the registry at GET /settings.
func RegisterSettingsHandler(mux *http.ServeMux, registry *servicesettings.Registry) {
	mux.Handle("GET /settings", servicesettings.Handler(registry, "/settings"))
}

// StoreURLDefinition is the one setting every importer shares: where to post.
func StoreURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingStoreURL, EnvironmentVariable: "MESSAGE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8337",
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
// prefix: MESSAGE_STORE_ belongs to the service, and the service's registry
// would refuse MESSAGE_STORE_URL, so the importers never build that one.
func NewImporterSettingsRegistry(importer string, environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(importer, nil, ImporterSettingDefinitions(), environment)
}
