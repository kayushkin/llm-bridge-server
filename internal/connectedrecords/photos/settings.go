package photos

import (
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// DefaultListenAddress is where the standalone photo-store listened. The HTTP
// client commands (photo-import, photo-import-messages, photo-import-facebook,
// photo-match-people, photo-group-faces, photo-guess-face-groups) still take
// it as the default of their -store-url, so it is kept for them; the store
// itself now runs inside llm-bridge-server and listens on nothing of its own.
const DefaultListenAddress = "127.0.0.1:8329"

// Keys of photo-import-messages' settings: where multichat answers and its
// bearer token. The command reads them from the environment; the service
// itself reads neither.
const (
	SettingMultichatURL    = "multichat_url"
	SettingMultichatToken  = "multichat_token"
	SettingJournalStoreURL = "journal_store_url"
	SettingMessageStoreURL = "message_store_url"
)

// ImportMessagesSettingDefinitions declares every environment variable
// photo-import-messages reads.
func ImportMessagesSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingMultichatURL, EnvironmentVariable: "MULTICHAT_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8402",
			Description: "Where multichat answers: the chats and the message log the photos are read from."},
		{Key: SettingMultichatToken, EnvironmentVariable: "MULTICHAT_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "multichat's bearer token."},
		{Key: SettingJournalStoreURL, EnvironmentVariable: "JOURNAL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8322",
			Description: "Where journal-store answers: the transcripts a photo from a Messenger export is cited by."},
		{Key: SettingMessageStoreURL, EnvironmentVariable: "MESSAGE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8337",
			Description: "Where message-store answers: the chat message a photo from a fetched Discord DM was sent in, which the photo cites."},
	}
}

// ImportFacebookSettingDefinitions declares every environment variable
// photo-import-facebook reads.
func ImportFacebookSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingJournalStoreURL, EnvironmentVariable: "JOURNAL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8322",
			Description: "Where journal-store answers: the Facebook posts a photo is cited by."},
	}
}

// NewImportFacebookSettingsRegistry reads photo-import-facebook's settings.
func NewImportFacebookSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("photo-import-facebook", nil, ImportFacebookSettingDefinitions(), environment)
}

// Keys the commands that say who is in a photo read (photo-match-people and
// photo-guess-face-groups): where people-store answers, and where
// llm-bridge-server answers their model calls, with its token.
const (
	SettingPeopleStoreURL     = "people_store_url"
	SettingBridgeURL          = "llm_bridge_url"
	SettingBridgeServiceToken = "llm_bridge_service_token"
	SettingModelStoreURL      = "model_store_url"
)

// PeopleOnPhotosSettingDefinitions declares every environment variable
// photo-match-people and photo-guess-face-groups read.
func PeopleOnPhotosSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingMultichatURL, EnvironmentVariable: "MULTICHAT_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8402",
			Description: "Where multichat answers: the chat a photo was sent in, and its members."},
		{Key: SettingMultichatToken, EnvironmentVariable: "MULTICHAT_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "multichat's bearer token."},
		{Key: SettingJournalStoreURL, EnvironmentVariable: "JOURNAL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8322",
			Description: "Where journal-store answers: the transcripts that cite a photo, and the people they link."},
		{Key: SettingMessageStoreURL, EnvironmentVariable: "MESSAGE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8337",
			Description: "Where message-store answers: the chat message a photo cites, and the people in its conversation."},
		{Key: SettingPeopleStoreURL, EnvironmentVariable: "PEOPLE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8321",
			Description: "Where people-store answers: which person a chat handle belongs to."},
		{Key: SettingBridgeURL, EnvironmentVariable: "LLM_BRIDGE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8160",
			Description: "Where llm-bridge-server answers the model calls: the model check's POST /oneshot, and the face group guesses' POST /instances/{id}/oneshot and GET /settings."},
		{Key: SettingBridgeServiceToken, EnvironmentVariable: "LLMBRIDGE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "llm-bridge-server's service token; photo-match-people needs it only with -model-check, photo-guess-face-groups always."},
		{Key: SettingModelStoreURL, EnvironmentVariable: "MODEL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8155",
			Description: "Where model-store answers: the provider of each model photo-guess-face-groups asks, which picks the bridge instance that calls it."},
	}
}

// NewMatchPeopleSettingsRegistry reads photo-match-people's settings.
func NewMatchPeopleSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("photo-match-people", nil, PeopleOnPhotosSettingDefinitions(), environment)
}

// NewGuessFaceGroupsSettingsRegistry reads photo-guess-face-groups' settings.
func NewGuessFaceGroupsSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("photo-guess-face-groups", nil, PeopleOnPhotosSettingDefinitions(), environment)
}

// NewImportMessagesSettingsRegistry reads the command's settings. It owns no
// prefix, so only CheckRequired can refuse.
func NewImportMessagesSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("photo-import-messages", nil, ImportMessagesSettingDefinitions(), environment)
}
