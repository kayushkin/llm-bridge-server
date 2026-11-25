package locationstore

import (
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The store runs inside llm-bridge-server, which declares its settings (the
// database, and the GeoNames directory it hands to New). What is declared
// here are the commands' own settings: they are HTTP clients of the store's
// routes, and each reads its variables from its own environment.

// Keys of the import commands' settings. The commands read them from the
// environment; the store itself reads none.
const (
	SettingLocationStoreURL = "location_store_url"
	SettingPhotoStoreURL    = "photo_store_url"
	SettingActivityStoreURL = "activity_store_url"
)

func locationStoreURLDefinition() servicesettings.Definition {
	return servicesettings.Definition{Key: SettingLocationStoreURL, EnvironmentVariable: "LOCATION_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8333",
		Description: "Where location-store answers: the command posts there."}
}

// ImportGoogleSettingDefinitions declares every environment variable
// location-import-google reads.
func ImportGoogleSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{locationStoreURLDefinition()}
}

// NewImportGoogleSettingsRegistry reads location-import-google's settings. It
// owns no prefix, so only CheckRequired can refuse.
func NewImportGoogleSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("location-import-google", nil, ImportGoogleSettingDefinitions(), environment)
}

// ImportPhotosSettingDefinitions declares every environment variable
// location-import-photos reads.
func ImportPhotosSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		locationStoreURLDefinition(),
		{Key: SettingPhotoStoreURL, EnvironmentVariable: "PHOTO_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8329",
			Description: "Where photo-store answers: the photos are read from there."},
	}
}

// NewImportPhotosSettingsRegistry reads location-import-photos' settings.
func NewImportPhotosSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("location-import-photos", nil, ImportPhotosSettingDefinitions(), environment)
}

// ImportPhotoSidecarsSettingDefinitions declares every environment variable
// location-import-photo-sidecars reads: the same as location-import-photos.
func ImportPhotoSidecarsSettingDefinitions() []servicesettings.Definition {
	return ImportPhotosSettingDefinitions()
}

// NewImportPhotoSidecarsSettingsRegistry reads
// location-import-photo-sidecars' settings.
func NewImportPhotoSidecarsSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("location-import-photo-sidecars", nil, ImportPhotoSidecarsSettingDefinitions(), environment)
}

// ImportActivitySettingDefinitions declares every environment variable
// location-import-activity reads.
func ImportActivitySettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		locationStoreURLDefinition(),
		{Key: SettingActivityStoreURL, EnvironmentVariable: "ACTIVITY_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8336",
			Description: "Where activity-store answers: the activities with coordinates are read from there."},
	}
}

// NewImportActivitySettingsRegistry reads location-import-activity's
// settings. It owns no prefix: ACTIVITY_STORE_ belongs to activity-store.
func NewImportActivitySettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("location-import-activity", nil, ImportActivitySettingDefinitions(), environment)
}

// SettingGeonamesDirectory is the key of location-name-localities' GeoNames
// directory setting.
const SettingGeonamesDirectory = "geonames_directory"

// LocalityNamingSettingDefinitions declares every environment variable
// location-name-localities reads. The GeoNames directory is where it
// downloads the town dump, and must be the directory the server's
// location_store.geonames_directory names, which the store reads: the store
// no longer answers where its data is.
func LocalityNamingSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		locationStoreURLDefinition(),
		{Key: SettingGeonamesDirectory, EnvironmentVariable: "LOCATION_STORE_GEONAMES_DIRECTORY", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "Where the GeoNames town files are downloaded: the directory llm-bridge-server's location_store.geonames_directory names, which location-store reads them from."},
	}
}

// NewLocalityNamingSettingsRegistry reads location-name-localities' settings.
func NewLocalityNamingSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("location-name-localities", nil, LocalityNamingSettingDefinitions(), environment)
}

// JourneyGroupingSettingDefinitions declares every environment variable
// location-group-journeys reads.
func JourneyGroupingSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{locationStoreURLDefinition()}
}

// PhoneSegmentingSettingDefinitions declares every environment variable
// location-segment-phone-points reads.
func PhoneSegmentingSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{locationStoreURLDefinition()}
}

// NewPhoneSegmentingSettingsRegistry reads location-segment-phone-points' settings.
func NewPhoneSegmentingSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("location-segment-phone-points", nil, PhoneSegmentingSettingDefinitions(), environment)
}

// NewJourneyGroupingSettingsRegistry reads location-group-journeys' settings.
func NewJourneyGroupingSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New("location-group-journeys", nil, JourneyGroupingSettingDefinitions(), environment)
}

// Keys of location-match-calendar's settings.
const (
	SettingDashURL   = "dash_url"
	SettingDashToken = "dash_token"
)

// CalendarMatchingSettingDefinitions declares every environment variable
// location-match-calendar reads. dash is the one way to Google Calendar: the
// command reads the events there and hands them to location-store, which
// decides, so the service itself never holds the token.
func CalendarMatchingSettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		locationStoreURLDefinition(),
		{Key: SettingDashURL, EnvironmentVariable: "DASH_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://localhost:8101",
			Description: "Where dash answers: the calendars and their events are read from its /api/calendar/google routes."},
		{Key: SettingDashToken, EnvironmentVariable: "DASH_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "dash's bearer token, sent only to DASH_URL and never printed."},
	}
}

// NewCalendarMatchingSettingsRegistry reads location-match-calendar's
// settings. It owns no prefix: DASH_ belongs to dash.
func NewCalendarMatchingSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(SetByCalendarMatching, nil, CalendarMatchingSettingDefinitions(), environment)
}

// SettingOpenStreetMapExtracts is the key of location-suggest-place-names'
// extracts setting.
const SettingOpenStreetMapExtracts = "openstreetmap_extracts"

// DefaultOpenStreetMapExtractDirectory is where the suggester looks for
// extracts when the environment names none: every .osm.pbf file in it.
func DefaultOpenStreetMapExtractDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".cache", "osm")
}

// PlaceNameSuggestingSettingDefinitions declares every environment variable
// location-suggest-place-names reads. The extracts are files on disk
// (Geofabrik's regional .osm.pbf downloads): nothing about a place is ever
// sent to an online service. dash is read for the calendar events whose
// addresses are matched against the extracts.
func PlaceNameSuggestingSettingDefinitions() []servicesettings.Definition {
	calendar := CalendarMatchingSettingDefinitions()
	return append(calendar, servicesettings.Definition{Key: SettingOpenStreetMapExtracts, EnvironmentVariable: "LOCATION_STORE_OSM_EXTRACTS",
		Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeStringList, Default: DefaultOpenStreetMapExtractDirectory(),
		Description: "OpenStreetMap extracts (.osm.pbf) to read, comma-separated: files, or directories whose every .osm.pbf is read. One per region, so a second state is one more file."})
}

// NewPlaceNameSuggestingSettingsRegistry reads location-suggest-place-names'
// settings. It owns no prefix: the other commands read LOCATION_STORE_
// variables too.
func NewPlaceNameSuggestingSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(SetByPlaceNameSuggesting, nil, PlaceNameSuggestingSettingDefinitions(), environment)
}
