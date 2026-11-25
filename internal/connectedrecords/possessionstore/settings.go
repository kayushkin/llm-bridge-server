package possessionstore

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "possession-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting.
const OwnedEnvironmentVariablePrefix = "POSSESSION_STORE_"

// Keys of the settings, as GET /settings names them.
const (
	SettingListenAddress          = "listen_address"
	SettingDataDirectory          = "data_directory"
	SettingPhotoStoreURL          = "photo_store_url"
	SettingLocationStoreURL       = "location_store_url"
	SettingHarnessStoreDatabase   = "harness_store_database"
	SettingBridgeURL              = "bridge_url"
	SettingBridgeServiceToken     = "bridge_service_token"
	SettingModelStoreURL          = "model_store_url"
	SettingFirstReaderRole        = "first_reader_role"
	SettingSecondReaderRole       = "second_reader_role"
	SettingReviewWorkerEnabled    = "review_worker_enabled"
	SettingReviewImageLongestSide = "review_image_longest_side"
	SettingOwnerTimeZone          = "owner_time_zone"
	SettingArticleStoreURL        = "article_store_url"
	SettingMailstackURL           = "mailstack_url"
	SettingMailstackToken         = "mailstack_token"
	SettingBudgetStoreURL         = "budget_store_url"
)

// DefaultListenAddress is loopback, deliberately: the service has no auth of
// its own. dash is the front door.
const DefaultListenAddress = "127.0.0.1:8341"

func homeDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return home
}

// DefaultDataDir is where the database and the photo originals live when the
// environment says nothing.
func DefaultDataDir() string {
	return filepath.Join(homeDirectory(), ".config", "possession-store")
}

func photoStoreURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingPhotoStoreURL, EnvironmentVariable: "PHOTO_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8329",
		Description: "Where photo-store answers. Every photo a possession points at is checked there, and its thumbnail is read from there."}
}

func harnessStoreDatabaseDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingHarnessStoreDatabase, EnvironmentVariable: "POSSESSION_STORE_HARNESS_STORE_DATABASE", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString,
		Default:     filepath.Join(homeDirectory(), ".config", "harness-store", "harness.db"),
		Description: "harness-store's database, opened read-only. The service checks a computer's harness_machine_id against its machines table; possession-add-photos takes this machine's name from its one local machine."}
}

// SettingDefinitions declares every environment variable the service reads,
// once. Nothing is Editable: GET /settings is as open as every other route.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "POSSESSION_STORE_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. Keep it on loopback: the service has no auth."},
		{Key: SettingDataDirectory, EnvironmentVariable: "POSSESSION_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds possession-store.db and originals/, the full-size photos possession-add-photos brought in."},
		photoStoreURLDefinition(),
		{Key: SettingLocationStoreURL, EnvironmentVariable: "LOCATION_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8333",
			Description: "Where location-store answers. A possession's place_id is checked there."},
		harnessStoreDatabaseDefinition(),
		{Key: SettingBridgeURL, EnvironmentVariable: "LLMBRIDGE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://localhost:8160",
			Description: "Where llm-bridge-server answers. The photo readers are asked through its POST /oneshot."},
		{Key: SettingBridgeServiceToken, EnvironmentVariable: "LLMBRIDGE_SERVICE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "llm-bridge-server's service token, sent only to LLMBRIDGE_URL. Required while the review worker is on."},
		{Key: SettingModelStoreURL, EnvironmentVariable: "MODEL_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://localhost:8155",
			Description: "Where model-store answers. A reading's cost is priced from its list prices."},
		{Key: SettingFirstReaderRole, EnvironmentVariable: "POSSESSION_STORE_FIRST_READER_ROLE", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeModelRole, Default: "possession-photo-reader",
			Description: "The model-store role of the first photo reader. It also turns an answer the owner wrote in words into a decision."},
		{Key: SettingSecondReaderRole, EnvironmentVariable: "POSSESSION_STORE_SECOND_READER_ROLE", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeModelRole, Default: "possession-photo-second-reader",
			Description: "The model-store role of the second photo reader."},
		{Key: SettingReviewWorkerEnabled, EnvironmentVariable: "POSSESSION_STORE_REVIEW_WORKER_ENABLED", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "true",
			Description: "Whether the service reviews queued photos. Off, photos wait in the queue."},
		ownerTimeZoneDefinition(),
		{Key: SettingArticleStoreURL, EnvironmentVariable: "ARTICLE_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8318",
			Description: "Where article-store answers. A product's listing page is saved there, and its text read from there; a product's article_id is checked there."},
		{Key: SettingBudgetStoreURL, EnvironmentVariable: "BUDGET_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8340",
			Description: "Where budget-store answers. The eBay import records orders there; a possession's budget_transaction_id is checked there, and the order line it came from links to it there."},
		{Key: SettingMailstackURL, EnvironmentVariable: "MAILSTACK_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://localhost:8195",
			Description: "Where mailstack answers. The eBay import reads order confirmations there, read-only."},
		{Key: SettingMailstackToken, EnvironmentVariable: "MAILSTACK_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "mailstack's bearer token, sent only to MAILSTACK_URL; a host-local drop-in. Without it the eBay import answers an error."},
		{Key: SettingReviewImageLongestSide, EnvironmentVariable: "POSSESSION_STORE_REVIEW_IMAGE_LONGEST_SIDE", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeInteger, Default: "2048",
			Description: "The longest side, in pixels, of the photo the readers are shown when its original is in originals/. Without one they see photo-store's 1024-pixel thumbnail."},
	}
}

// OwnerTimeZoneDefinition is the zone a photo's time is read in to get the
// day an outfit was worn.
func ownerTimeZoneDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingOwnerTimeZone, EnvironmentVariable: "POSSESSION_STORE_OWNER_TIME_ZONE", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeString, Default: "UTC",
		Description: "The owner's IANA time zone. An outfit read from a photo is recorded as worn on the photo's day in this zone. Set it in a host-local drop-in."}
}

// NewSettingsRegistry reads this service's settings from environment.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, []string{OwnedEnvironmentVariablePrefix}, SettingDefinitions(), environment)
}

// RegisterSettingsHandler serves the registry at GET /settings.
func RegisterSettingsHandler(mux *http.ServeMux, registry *servicesettings.Registry) {
	mux.Handle("GET /settings", servicesettings.Handler(registry, "/settings"))
}

// Keys of possession-add-photos' settings.
const (
	SettingPossessionStoreURL = "possession_store_url"
)

// AddPhotosSettingDefinitions declares every environment variable
// possession-add-photos reads.
func AddPhotosSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingPossessionStoreURL, EnvironmentVariable: "POSSESSION_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://" + DefaultListenAddress,
			Description: "Where possession-store answers: the photos are queued for review there."},
		photoStoreURLDefinition(),
		{Key: SettingDataDirectory, EnvironmentVariable: "POSSESSION_STORE_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "possession-store's data directory: the originals are copied into its originals/."},
		harnessStoreDatabaseDefinition(),
	}
}

// NewAddPhotosSettingsRegistry reads possession-add-photos' settings. It owns
// no prefix: POSSESSION_STORE_ belongs to the service.
func NewAddPhotosSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("possession-add-photos", nil, AddPhotosSettingDefinitions(), environment)
}
