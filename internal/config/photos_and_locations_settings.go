package config

import (
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photostore/facepasses"
	"github.com/kayushkin/llm-bridge-server/internal/productiondefaults"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Keys of the photo-store and location-store settings in this server. Both
// keep their records in their modules of database.postgres_url; photo-store
// keeps its thumbnails' bytes in the file-store module. Each is read once, at
// start.
//
// photo-store's three face passes (grouping faces, matching people on
// photos with a model's check, and asking models who a face group is) are
// background work that spends money on model calls and writes guesses about
// who is in someone's photos, so each is off until its own setting turns it
// on, and even then runs only for the owners named in
// photo_store.face_passes.owner_principal_ids.
const (
	SettingLocationStoreGeonamesDirectory           = "location_store.geonames_directory"
	SettingPhotoStoreFacePassesOwnerPrincipalIDs    = "photo_store.face_passes.owner_principal_ids"
	SettingPhotoStoreFacePassesInterval             = "photo_store.face_passes.interval"
	SettingPhotoStoreFacePassesMessageStoreURL      = "photo_store.face_passes.message_store_url"
	SettingPhotoStoreFaceGroupingEnabled            = "photo_store.face_grouping.enabled"
	SettingPhotoStorePeopleMatchingEnabled          = "photo_store.people_matching.enabled"
	SettingPhotoStorePeopleMatchingModelCheckLimit  = "photo_store.people_matching.model_check_limit"
	SettingPhotoStorePeopleMatchingModelCheckRole   = "photo_store.people_matching.model_check_role"
	SettingPhotoStoreFaceGroupGuessingEnabled       = "photo_store.face_group_guessing.enabled"
	SettingPhotoStoreFaceGroupGuessingModels        = "photo_store.face_group_guessing.models"
	SettingPhotoStoreFaceGroupGuessingContextModels = "photo_store.face_group_guessing.context_only_models"
	SettingPhotoStoreFaceGroupGuessingLimit         = "photo_store.face_group_guessing.limit"
)

// photosAndLocationsSettingDefinitions declares the settings above.
func photosAndLocationsSettingDefinitions() []servicesettings.Definition {
	const (
		wiring   = msg.ServiceSettingKindWiring
		path     = msg.ServiceSettingKindPath
		behavior = msg.ServiceSettingKindBehavior
		text     = msg.ServiceSettingValueTypeString
		list     = msg.ServiceSettingValueTypeStringList
		integer  = msg.ServiceSettingValueTypeInteger
		boolean  = msg.ServiceSettingValueTypeBoolean
	)
	return []servicesettings.Definition{
		{Key: SettingLocationStoreGeonamesDirectory, EnvironmentVariable: "LLMBRIDGE_LOCATION_STORE_GEONAMES_DIRECTORY", Kind: path, ValueType: text, Default: productiondefaults.LocationStoreGeonamesDirectory(),
			Description: "The directory holding GeoNames' public town dump (cities1000), which location-store reads to name the town of a place, visit or trip and to match calendar locations, offline. location-name-localities downloads it there when its LOCATION_STORE_GEONAMES_DIRECTORY names the same directory. With no dump there, filling localities answers that it is missing and calendar matching uses no town names. Read at start."},
		{Key: SettingPhotoStoreFacePassesOwnerPrincipalIDs, EnvironmentVariable: "LLMBRIDGE_PHOTO_STORE_FACE_PASSES_OWNER_PRINCIPAL_IDS", Kind: behavior, ValueType: list, Default: "",
			Description: "The principals whose photos photo-store's face passes run over, each pass once per owner and as that owner, so faces of two owners are never grouped or matched together. A pass that names people needs the owner's own person in their people-store records (the person whose principal_id is the owner); an owner without one is skipped and logged. Empty runs the passes for nobody. Read at start."},
		{Key: SettingPhotoStoreFacePassesInterval, EnvironmentVariable: "LLMBRIDGE_PHOTO_STORE_FACE_PASSES_INTERVAL", Kind: behavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "1h",
			Description: "How long the face passes wait after one round (every pass that is on, for every owner) before the next. Read at start."},
		{Key: SettingPhotoStoreFacePassesMessageStoreURL, EnvironmentVariable: "LLMBRIDGE_PHOTO_STORE_FACE_PASSES_MESSAGE_STORE_URL", Kind: wiring, ValueType: text, Default: "",
			Description: "Where message-store answers: people matching and face group guessing read the chat message a photo cites and the people in its conversation there. Required when either is on. Read at start."},
		{Key: SettingPhotoStoreFaceGroupingEnabled, EnvironmentVariable: "LLMBRIDGE_PHOTO_STORE_FACE_GROUPING_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Group the faces nobody has named into groups of one person each, and guess a group's person from the groups people decided, as photo-group-faces does. It reads and writes only photo-store and calls no model. Off by default, while the standalone photo-store's scheduler job still runs the command. Read at start."},
		{Key: SettingPhotoStorePeopleMatchingEnabled, EnvironmentVariable: "LLMBRIDGE_PHOTO_STORE_PEOPLE_MATCHING_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Propose who is in each photo from its faces and its context (the chat it was sent in, through multichat with auth-store's multichat token; message-store; journal-store and people-store in this server), as photo-match-people does, then show up to photo_store.people_matching.model_check_limit suggestions to a model that can see. Off by default. Read at start."},
		{Key: SettingPhotoStorePeopleMatchingModelCheckLimit, EnvironmentVariable: "LLMBRIDGE_PHOTO_STORE_PEOPLE_MATCHING_MODEL_CHECK_LIMIT", Kind: behavior, ValueType: integer, Default: "0",
			Description: "How many suggestions people matching shows a model per owner per round, surest first; each is one model call with images. 0 asks no model. Read at start."},
		{Key: SettingPhotoStorePeopleMatchingModelCheckRole, EnvironmentVariable: "LLMBRIDGE_PHOTO_STORE_PEOPLE_MATCHING_MODEL_CHECK_ROLE", Kind: behavior, ValueType: text, Default: facepasses.DefaultModelCheckRole,
			Description: "The model-store role the model check asks through this server's POST /oneshot; its models must see images. Read at start."},
		{Key: SettingPhotoStoreFaceGroupGuessingEnabled, EnvironmentVariable: "LLMBRIDGE_PHOTO_STORE_FACE_GROUP_GUESSING_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Ask each model in photo_store.face_group_guessing.models who an open face group is, as photo-guess-face-groups does, through this server's POST /instances/{id}/oneshot. Each answer is a guess a person decides on; it is never a link. Off by default. Read at start."},
		{Key: SettingPhotoStoreFaceGroupGuessingModels, EnvironmentVariable: "LLMBRIDGE_PHOTO_STORE_FACE_GROUP_GUESSING_MODELS", Kind: behavior, ValueType: list, Default: strings.Join(facepasses.DefaultGuessModels(), ","),
			Description: "The model-store ids face group guessing asks, each on its own. Read at start."},
		{Key: SettingPhotoStoreFaceGroupGuessingContextModels, EnvironmentVariable: "LLMBRIDGE_PHOTO_STORE_FACE_GROUP_GUESSING_CONTEXT_ONLY_MODELS", Kind: behavior, ValueType: list, Default: strings.Join(facepasses.DefaultContextOnlyModels(), ","),
			Description: "The models of photo_store.face_group_guessing.models that judge from the records and the whole photos only, with no faces to compare. Each must be in that list. Read at start."},
		{Key: SettingPhotoStoreFaceGroupGuessingLimit, EnvironmentVariable: "LLMBRIDGE_PHOTO_STORE_FACE_GROUP_GUESSING_LIMIT", Kind: behavior, ValueType: integer, Default: fmt.Sprint(facepasses.DefaultGuessLimit),
			Description: "How many open groups face group guessing asks about per owner per round, the most photos first. Read at start."},
	}
}
