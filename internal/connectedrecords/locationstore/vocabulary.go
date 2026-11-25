package locationstore

import "strings"

// The words a record's fields may take. GET /vocabularies serves them all.

// What a place is to the user.
const (
	PlaceKindHome  = "home"
	PlaceKindWork  = "work"
	PlaceKindVenue = "venue" // a named business or landmark
	PlaceKindSaved = "saved" // an address the user saved, searched for or gave a name
	PlaceKindOther = "other" // not known; an import may later say more
)

// PlaceKinds is every kind a place can have.
var PlaceKinds = []string{PlaceKindHome, PlaceKindWork, PlaceKindVenue, PlaceKindSaved, PlaceKindOther}

// How a trip moved.
const (
	ModeDriving      = "driving"
	ModeMotorcycling = "motorcycling"
	ModeWalking      = "walking"
	ModeRunning      = "running"
	ModeHiking       = "hiking"
	ModeCycling      = "cycling"
	ModeBus          = "bus"
	ModeTrain        = "train"
	ModeTram         = "tram"
	ModeSubway       = "subway"
	ModeFerry        = "ferry"
	ModeBoating      = "boating"
	ModeFlying       = "flying"
	ModeSkiing       = "skiing"
	// ModeMixed is a trip made of others that moved in more than one way: a
	// holiday with flights and drives.
	ModeMixed   = "mixed"
	ModeUnknown = "unknown"
)

// TripModes is every mode a trip can have.
var TripModes = []string{ModeDriving, ModeMotorcycling, ModeWalking, ModeRunning, ModeHiking, ModeCycling, ModeBus,
	ModeTrain, ModeTram, ModeSubway, ModeFerry, ModeBoating, ModeFlying, ModeSkiing, ModeMixed, ModeUnknown}

// Where a place, visit or trip came from.
const (
	// SourceGoogleTimeline is the on-device Timeline export (Timeline.json,
	// 2024 on).
	SourceGoogleTimeline = "google_timeline"
	// SourceGoogleSemantic is the legacy Takeout Semantic Location History.
	SourceGoogleSemantic = "google_semantic"
	// SourceManual is made by hand, by a person or an agent.
	SourceManual = "manual"
)

// SourceGenerated is made by one of this repo's commands from other records:
// a journey grouped by location-group-journeys.
const SourceGenerated = "generated"

// SourcePhonePoints is a visit or movement that POST /phone-points/segment
// made from the points a phone sent live (PointSourcePhone).
const SourcePhonePoints = "phone_points"

// RecordSources is every source a place, visit or trip can have.
var RecordSources = []string{SourceGoogleTimeline, SourceGoogleSemantic, SourceManual, SourceGenerated, SourcePhonePoints}

// What a trip is.
const (
	// TripKindMovement is one movement a source recorded: a drive, a flight.
	TripKindMovement = "movement"
	// TripKindJourney is a span away from home made of movements: a road trip
	// or a holiday. Made by hand, or by location-group-journeys.
	TripKindJourney = "journey"
)

// TripKinds is every kind a trip can have.
var TripKinds = []string{TripKindMovement, TripKindJourney}

// Who set a field the routes and the commands both write.
const (
	// SetByHand is a person or an agent, through the routes.
	SetByHand = "hand"
	// SetByJourneyGrouping is location-group-journeys.
	SetByJourneyGrouping = "location-group-journeys"
	// SetByLocalityNaming is location-name-localities.
	SetByLocalityNaming = "location-name-localities"
	// SetByCalendarMatching is location-match-calendar.
	SetByCalendarMatching = "location-match-calendar"
)

// How a calendar event was matched to where the user was.
const (
	// CalendarRulePlace: the event's location names a known place, by name or
	// address, and a visit near that place overlaps the event.
	CalendarRulePlace = "place"
	// CalendarRuleSeriesPlace: the event is one of a recurring series whose
	// location names a town, and at three or more of its times the user was at
	// the same place in that town, not home.
	CalendarRuleSeriesPlace = "series_place"
	// CalendarRuleUnlocatedSeriesPlace: the event is one of a recurring
	// series with no location, and of the times of the series the user was
	// out (not home, not away on a journey), most found them at one spot: at
	// least three times, and at least 60% of them. Times they were elsewhere
	// link nothing.
	CalendarRuleUnlocatedSeriesPlace = "unlocated_series_place"
	// CalendarRuleTownVisit: a single event whose location names a town, and
	// the user arrived at a place in that town, not home, within an hour of
	// its start.
	CalendarRuleTownVisit = "town_visit"
	// CalendarRuleTownJourney: a single event whose location names a town
	// away from home, during a journey that was in that town while the event
	// ran.
	CalendarRuleTownJourney = "town_journey"
	// CalendarRuleFlight: a "Flight to X (AA 123)" event and the flight
	// recorded at its time.
	CalendarRuleFlight = "flight"
	// CalendarRuleFlightJourney: the journey a flight event's flight is part
	// of, or that the event falls in when no flight was recorded.
	CalendarRuleFlightJourney = "flight_journey"
	// CalendarRuleJourneyTime: a single event with no location, on a calendar
	// the user owns, during a journey. Time alone: the weakest rule.
	CalendarRuleJourneyTime = "journey_time"
	// CalendarRuleHand: made by a person or agent through the routes.
	CalendarRuleHand = "hand"
)

// CalendarMatchRules is every rule a calendar event link can have.
var CalendarMatchRules = []string{CalendarRulePlace, CalendarRuleSeriesPlace, CalendarRuleUnlocatedSeriesPlace, CalendarRuleTownVisit, CalendarRuleTownJourney,
	CalendarRuleFlight, CalendarRuleFlightJourney, CalendarRuleJourneyTime, CalendarRuleHand}

// What an unlocated recurring series came to, as calendar_series_places
// records it.
const (
	// SeriesOutcomeLinked: one spot held enough of the times the user was
	// out; those times link to the visits there.
	SeriesOutcomeLinked = "linked"
	// SeriesOutcomeBelowAgreement: the leading spot held too small a share
	// of the times the user was out.
	SeriesOutcomeBelowAgreement = "below_agreement"
	// SeriesOutcomeTooFewTimes: the user was out at its times, but the
	// leading spot, if any, held fewer than three of them.
	SeriesOutcomeTooFewTimes = "too_few_times"
	// SeriesOutcomeNeverOut: at none of its times was the user out at a
	// place that could hold it.
	SeriesOutcomeNeverOut = "never_out"
)

// SeriesOutcomes is every outcome a calendar series place can have.
var SeriesOutcomes = []string{SeriesOutcomeLinked, SeriesOutcomeBelowAgreement, SeriesOutcomeTooFewTimes, SeriesOutcomeNeverOut}

// What a place name suggestion is in.
const (
	PlaceSuggestionProposed = "proposed"
	PlaceSuggestionAccepted = "accepted"
	PlaceSuggestionRejected = "rejected"
)

// PlaceSuggestionStatuses is every status a place name suggestion can have.
var PlaceSuggestionStatuses = []string{PlaceSuggestionProposed, PlaceSuggestionAccepted, PlaceSuggestionRejected}

// Where a place name suggestion came from.
const (
	// PlaceSuggestionSourcePOI: a named OpenStreetMap feature near the place.
	PlaceSuggestionSourcePOI = "osm_poi"
	// PlaceSuggestionSourceAddress: the nearest OpenStreetMap address, for a
	// place no feature names.
	PlaceSuggestionSourceAddress = "osm_address"
	// PlaceSuggestionSourceCalendar: calendar events whose street address,
	// found in OpenStreetMap, lies at the place.
	PlaceSuggestionSourceCalendar = "calendar_address"
)

// PlaceSuggestionSources is every source a place name suggestion can have.
var PlaceSuggestionSources = []string{PlaceSuggestionSourcePOI, PlaceSuggestionSourceAddress, PlaceSuggestionSourceCalendar}

// SetByPlaceNameSuggesting is location-suggest-place-names.
const SetByPlaceNameSuggesting = "location-suggest-place-names"

// CalendarLinkTargetTypes is every kind of record a calendar event links to.
var CalendarLinkTargetTypes = []string{EntityVisit, EntityTrip, EntityPlace}

// Where a raw point came from.
const (
	PointSourceGoogleRecords      = "google_records"       // legacy Takeout Records.json
	PointSourceGoogleTimelineRaw  = "google_timeline_raw"  // Timeline.json rawSignals positions
	PointSourceGoogleTimelinePath = "google_timeline_path" // Timeline.json timelinePath points
	PointSourceGoogleSemanticPath = "google_semantic_path" // legacy Semantic Location History simplifiedRawPath points
	PointSourcePhoto              = "photo"                // a photo's GPS, from photo-store; source_ref is the photo id
	// PointSourceGoogleActivity is where the user's device was when they did
	// something Google recorded (a search, a Maps lookup), from
	// activity-store; source_ref is the activity id.
	PointSourceGoogleActivity = "google_activity"
	// PointSourceGooglePhotosSidecar is the location Google Photos kept for a
	// photo whose file has no GPS, from the photo's Takeout sidecar
	// (geoData); source_ref is the photo-store photo id.
	PointSourceGooglePhotosSidecar = "google_photos_sidecar"
	// PointSourcePhone is a fix a phone sent as it was made, through
	// POST /points/live.
	PointSourcePhone = "phone"
)

// PointSources is every source a point can have.
var PointSources = []string{PointSourceGoogleRecords, PointSourceGoogleTimelineRaw, PointSourceGoogleTimelinePath,
	PointSourceGoogleSemanticPath, PointSourcePhoto, PointSourceGoogleActivity, PointSourceGooglePhotosSidecar, PointSourcePhone}

// What kind of file an import read.
const (
	ImportKindGoogleTimeline = "google_timeline" // on-device Timeline.json
	ImportKindGoogleRecords  = "google_records"  // legacy Takeout Records.json
	ImportKindGoogleSemantic = "google_semantic" // one legacy Semantic Location History month file
	ImportKindPhotoStore     = "photo_store"     // photos with GPS, read from photo-store
	// ImportKindActivityStore is activities with coordinates, read from
	// activity-store.
	ImportKindActivityStore = "activity_store"
	// ImportKindGooglePhotosSidecars is Google Takeout photo sidecars whose
	// geoData places a photo that photo-store has no GPS for.
	ImportKindGooglePhotosSidecars = "google_photos_sidecars"
	// ImportKindPhoneLive is the points one phone sent live on one UTC day,
	// through POST /points/live.
	ImportKindPhoneLive = "phone_live"
)

// ImportKinds is every kind an import can have.
var ImportKinds = []string{ImportKindGoogleTimeline, ImportKindGoogleRecords, ImportKindGoogleSemantic, ImportKindPhotoStore,
	ImportKindActivityStore, ImportKindGooglePhotosSidecars, ImportKindPhoneLive}

// Vocabularies is the answer to GET /vocabularies.
type Vocabularies struct {
	PlaceKinds    []string `json:"place_kinds"`
	TripKinds     []string `json:"trip_kinds"`
	TripModes     []string `json:"trip_modes"`
	RecordSources []string `json:"record_sources"`
	PointSources  []string `json:"point_sources"`
	ImportKinds   []string `json:"import_kinds"`
	// CalendarMatchRules are the rules a calendar event link names.
	CalendarMatchRules []string `json:"calendar_match_rules"`
	// CalendarLinkTargetTypes are what a calendar event links to.
	CalendarLinkTargetTypes []string `json:"calendar_link_target_types"`
	// SeriesOutcomes are what a recurring series with no location came to.
	SeriesOutcomes []string `json:"series_outcomes"`
	// PlaceSuggestionStatuses are the states of a place name suggestion.
	PlaceSuggestionStatuses []string `json:"place_suggestion_statuses"`
	// PlaceSuggestionSources are where a place name suggestion came from.
	PlaceSuggestionSources []string `json:"place_suggestion_sources"`
}

func knownWord(words []string, word string) bool {
	for _, known := range words {
		if word == known {
			return true
		}
	}
	return false
}

// googleActivityModes maps Google's activity types, in both the on-device
// export and legacy Takeout, to a trip mode. A type not here is unknown, and
// the trip keeps Google's word in source_mode.
var googleActivityModes = map[string]string{
	"IN_PASSENGER_VEHICLE": ModeDriving,
	"IN_VEHICLE":           ModeDriving,
	"IN_CAR":               ModeDriving,
	"IN_TAXI":              ModeDriving,
	"DRIVING":              ModeDriving,
	"MOTORCYCLING":         ModeMotorcycling,
	"WALKING":              ModeWalking,
	"ON_FOOT":              ModeWalking,
	"RUNNING":              ModeRunning,
	"HIKING":               ModeHiking,
	"CYCLING":              ModeCycling,
	"ON_BICYCLE":           ModeCycling,
	"IN_BUS":               ModeBus,
	"IN_TRAIN":             ModeTrain,
	"IN_TRAM":              ModeTram,
	"IN_SUBWAY":            ModeSubway,
	"IN_FERRY":             ModeFerry,
	"SAILING":              ModeBoating,
	"BOATING":              ModeBoating,
	"KAYAKING":             ModeBoating,
	"ROWING":               ModeBoating,
	"FLYING":               ModeFlying,
	"SKIING":               ModeSkiing,
}

// ModeForGoogleActivityType is the trip mode for one of Google's activity
// types (IN_PASSENGER_VEHICLE), or unknown.
func ModeForGoogleActivityType(activityType string) string {
	if mode, found := googleActivityModes[strings.ToUpper(strings.TrimSpace(activityType))]; found {
		return mode
	}
	return ModeUnknown
}

// PlaceKindForGoogleSemanticType is the place kind for one of Google's
// semantic types: HOME and INFERRED_HOME in the on-device export, TYPE_HOME in
// legacy Takeout, and the like. named says the source gave the place a name,
// which makes a place of no stated type a venue.
func PlaceKindForGoogleSemanticType(semanticType string, named bool) string {
	switch strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(semanticType)), "TYPE_") {
	case "HOME", "INFERRED_HOME":
		return PlaceKindHome
	case "WORK", "INFERRED_WORK":
		return PlaceKindWork
	case "SEARCHED_ADDRESS", "ALIASED_LOCATION":
		return PlaceKindSaved
	}
	if named {
		return PlaceKindVenue
	}
	return PlaceKindOther
}
