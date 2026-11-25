package locationstore

// Wire types: what the routes take and answer. ./generate-ts.sh renders this
// file and vocabulary.go to ts/model.ts. Timestamps are epoch seconds; 0
// means unset. A nullable field is null when no source said.

// Place is a named spot.
type Place struct {
	// ID is place_000001: what anything else joins on.
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	// RadiusMeters is how far the place reaches; 0 when not given.
	RadiusMeters float64 `json:"radius_meters"`
	Address      string  `json:"address"`
	// Kind is one of GET /vocabularies place_kinds.
	Kind string `json:"kind"`
	// GooglePlaceID is Google's id for the place, when a source gave one; an
	// import finds the place again by it.
	GooglePlaceID *string `json:"google_place_id"`
	// Description is markdown; [text](ref:<id>) links name people and events.
	Description string   `json:"description"`
	DescribedBy string   `json:"described_by"`
	Tags        []string `json:"tags"`
	Source      string   `json:"source"`
	// ImportID is the import that created the place; null when made by hand.
	ImportID *string `json:"import_id"`
	// Locality is the town the place is in; null until
	// location-name-localities names it.
	Locality  *Locality `json:"locality"`
	CreatedAt int64     `json:"created_at"`
	UpdatedAt int64     `json:"updated_at"`
	DeletedAt int64     `json:"deleted_at"`
	// MergedIntoPlaceID is the place this one was merged into (POST
	// /places/{id}/merge), which then holds everything that pointed here;
	// null for a place never merged.
	MergedIntoPlaceID *string `json:"merged_into_place_id"`
}

// PlaceMergeRequest is the body of POST /places/{id}/merge.
type PlaceMergeRequest struct {
	Into string `json:"into"`
}

// PlaceMerge answers POST /places/{id}/merge: the place kept, and how many
// records were moved from the merged place to it.
type PlaceMerge struct {
	Place                  Place  `json:"place"`
	MergedPlaceID          string `json:"merged_place_id"`
	VisitsMoved            int    `json:"visits_moved"`
	TripStartsMoved        int    `json:"trip_starts_moved"`
	TripEndsMoved          int    `json:"trip_ends_moved"`
	CalendarLinksMoved     int    `json:"calendar_links_moved"`
	CalendarLinksDuplicate int    `json:"calendar_links_duplicate"`
	CalendarSeriesMoved    int    `json:"calendar_series_moved"`
	HomePeriodsMoved       int    `json:"home_periods_moved"`
	PlacesRepointed        int    `json:"places_repointed"`
}

// Visit is being somewhere from StartedAt to EndedAt.
type Visit struct {
	// ID is visit_000001.
	ID      string  `json:"id"`
	PlaceID *string `json:"place_id"`
	// PlaceName is the place's name when the visit was read, for display only.
	PlaceName string   `json:"place_name"`
	StartedAt int64    `json:"started_at"`
	EndedAt   int64    `json:"ended_at"`
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
	// Probability is 0..1: how sure the source is of the place.
	Probability *float64 `json:"probability"`
	// SourceSemanticType is the source's own word for the place (HOME).
	SourceSemanticType string   `json:"source_semantic_type"`
	Description        string   `json:"description"`
	DescribedBy        string   `json:"described_by"`
	Tags               []string `json:"tags"`
	Source             string   `json:"source"`
	// SourceRef is what an import finds the visit again by; "" when made by
	// hand.
	SourceRef string  `json:"source_ref"`
	ImportID  *string `json:"import_id"`
	// SupersededByImportID is the newer export that no longer holds this
	// visit, when that is why it is deleted.
	SupersededByImportID *string `json:"superseded_by_import_id"`
	// Locality is the town the visit was in: the visit's own coordinate, or
	// its place's. Null until location-name-localities names it.
	Locality *Locality `json:"locality"`
	// CalendarEventLinks are the live calendar events linked to the visit.
	// Only GET /visits/{id} fills them; lists leave them out.
	CalendarEventLinks []CalendarEventLink `json:"calendar_event_links,omitempty"`
	CreatedAt          int64               `json:"created_at"`
	UpdatedAt          int64               `json:"updated_at"`
	DeletedAt          int64               `json:"deleted_at"`
}

// Trip is a movement from one place to another, or, with children, a road
// trip or holiday made of them.
type Trip struct {
	// ID is trip_000001.
	ID string `json:"id"`
	// Kind is one of GET /vocabularies trip_kinds: a movement a source
	// recorded, or a journey made of them.
	Kind string `json:"kind"`
	// ParentTripID is the trip this one is part of; never set by an import.
	ParentTripID *string `json:"parent_trip_id"`
	// ParentSetBy says who set or cleared ParentTripID: "" nobody yet,
	// "hand" a person or agent through the routes, or the command that
	// grouped it (location-group-journeys). The command moves only trips it
	// placed itself or that nobody has placed.
	ParentSetBy    string   `json:"parent_set_by"`
	StartedAt      int64    `json:"started_at"`
	EndedAt        int64    `json:"ended_at"`
	StartLatitude  *float64 `json:"start_latitude"`
	StartLongitude *float64 `json:"start_longitude"`
	EndLatitude    *float64 `json:"end_latitude"`
	EndLongitude   *float64 `json:"end_longitude"`
	StartPlaceID   *string  `json:"start_place_id"`
	// StartPlaceName and EndPlaceName are the places' names when the trip was
	// read, for display only.
	StartPlaceName string  `json:"start_place_name"`
	EndPlaceID     *string `json:"end_place_id"`
	EndPlaceName   string  `json:"end_place_name"`
	// Mode is one of GET /vocabularies trip_modes.
	Mode string `json:"mode"`
	// SourceMode is the source's own word for the mode (IN_PASSENGER_VEHICLE).
	SourceMode     string   `json:"source_mode"`
	DistanceMeters *float64 `json:"distance_meters"`
	// Probability is 0..1: how sure the source is of the mode.
	Probability *float64 `json:"probability"`
	Title       string   `json:"title"`
	// TitleBy says who wrote the title: "hand" through the routes, or the
	// command that generated it. A command rewrites only its own titles.
	TitleBy     string   `json:"title_by"`
	Description string   `json:"description"`
	DescribedBy string   `json:"described_by"`
	Tags        []string `json:"tags"`
	Source      string   `json:"source"`
	SourceRef   string   `json:"source_ref"`
	ImportID    *string  `json:"import_id"`
	// SupersededByImportID is the newer export that no longer holds this
	// trip, when that is why it is deleted.
	SupersededByImportID *string `json:"superseded_by_import_id"`
	// StartLocality and EndLocality are the towns the trip started and ended
	// in; null until location-name-localities names them. A journey's are
	// those of its first and last trip.
	StartLocality *Locality `json:"start_locality"`
	EndLocality   *Locality `json:"end_locality"`
	// PathPoints is how many points GET /trips/{id}/path answers.
	PathPoints int `json:"path_points"`
	// ChildTrips is how many live trips name this one as their parent.
	ChildTrips int `json:"child_trips"`
	// CalendarEventLinks are the live calendar events linked to the trip.
	// Only GET /trips/{id} fills them; lists leave them out.
	CalendarEventLinks []CalendarEventLink `json:"calendar_event_links,omitempty"`
	CreatedAt          int64               `json:"created_at"`
	UpdatedAt          int64               `json:"updated_at"`
	DeletedAt          int64               `json:"deleted_at"`
}

// PathPoint is one point of a trip's path, in order.
type PathPoint struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	// Timestamp is null when the source gave the point no time.
	Timestamp *int64 `json:"timestamp"`
}

// Point is one raw location fix.
type Point struct {
	Timestamp      int64    `json:"timestamp"`
	Latitude       float64  `json:"latitude"`
	Longitude      float64  `json:"longitude"`
	AccuracyMeters *float64 `json:"accuracy_meters"`
	// Source is one of GET /vocabularies point_sources.
	Source string `json:"source"`
	// SourceRef names the record the point came from (a photo id), or "".
	SourceRef string `json:"source_ref"`
	ImportID  string `json:"import_id"`
}

// TimelineItem is one visit or one trip, in GET /timeline.
type TimelineItem struct {
	// Kind is "visit" or "trip"; the other field is null.
	Kind  string `json:"kind"`
	Visit *Visit `json:"visit"`
	Trip  *Trip  `json:"trip"`
}

// ImportCounts says what an import, or one batch of it, did.
type ImportCounts struct {
	PointsAdded     int `json:"points_added"`
	PointsUnchanged int `json:"points_unchanged"`
	PlacesCreated   int `json:"places_created"`
	PlacesUpdated   int `json:"places_updated"`
	VisitsCreated   int `json:"visits_created"`
	VisitsUpdated   int `json:"visits_updated"`
	TripsCreated    int `json:"trips_created"`
	TripsUpdated    int `json:"trips_updated"`
	// RecordsUnchanged counts places, visits and trips already as the import
	// says, or deleted by someone, so left alone.
	RecordsUnchanged int `json:"records_unchanged"`
}

// Import is one imported file.
type Import struct {
	// ID is import_000001.
	ID string `json:"id"`
	// Kind is one of GET /vocabularies import_kinds.
	Kind          string `json:"kind"`
	Sha256        string `json:"sha256"`
	SourceMachine string `json:"source_machine"`
	SourcePath    string `json:"source_path"`
	// RecordedBy is who sent a live import's points (POST /points/live), as
	// the front door signed it; "" for an import of a file.
	RecordedBy string `json:"recorded_by"`
	ImportedAt int64  `json:"imported_at"`
	// FinishedAt is 0 while the import is still sending.
	FinishedAt int64        `json:"finished_at"`
	Counts     ImportCounts `json:"counts"`
	// Snapshot is the span a full-snapshot export covers, and what says how
	// new it is; null for any other import.
	Snapshot *ImportSnapshot `json:"snapshot"`
	// SnapshotChanges is what reconciling the snapshots did when this import
	// last finished; null when it did not run.
	SnapshotChanges *SnapshotChanges `json:"snapshot_changes"`
}

// ImportSnapshot describes an on-device Timeline export, which holds every
// visit and activity of the span it covers: an export newer than another
// supersedes the older one's records inside its own span.
type ImportSnapshot struct {
	// CoversFrom and CoversTo are the earliest start and the latest end of
	// the file's semantic segments.
	CoversFrom int64 `json:"covers_from"`
	CoversTo   int64 `json:"covers_to"`
	// RawSignalsOldestAt and RawSignalsNewestAt are the oldest and newest
	// raw position the file holds; 0 when it holds none. The phone keeps raw
	// positions in a rolling buffer, so a later export has a later newest
	// position, or the same newest and a later oldest.
	RawSignalsOldestAt int64 `json:"raw_signals_oldest_at"`
	RawSignalsNewestAt int64 `json:"raw_signals_newest_at"`
}

// SnapshotChanges is what reconciling the snapshot imports did: records an
// older export held that the newest export covering them does not, set
// aside, and records set aside before that a newer export holds again,
// brought back.
type SnapshotChanges struct {
	VisitsSuperseded int `json:"visits_superseded"`
	TripsSuperseded  int `json:"trips_superseded"`
	VisitsRestored   int `json:"visits_restored"`
	TripsRestored    int `json:"trips_restored"`
	// KeptEditedIDs are records the newest export no longer holds, kept
	// because someone edited them.
	KeptEditedIDs []string `json:"kept_edited_ids"`
}

// ImportFinish is the body of POST /imports/{id}/finish; it may be empty.
type ImportFinish struct {
	// Snapshot, for an on-device Timeline export, makes the import a full
	// snapshot of its span.
	Snapshot *ImportSnapshot `json:"snapshot"`
}

// ImportStart is the body of POST /imports.
type ImportStart struct {
	Kind          string `json:"kind"`
	Sha256        string `json:"sha256"`
	SourceMachine string `json:"source_machine"`
	SourcePath    string `json:"source_path"`
	// Reapply opens a finished import of the same file again, so the file is
	// sent once more: to record which records it holds, for an import made
	// before the store kept that.
	Reapply bool `json:"reapply"`
}

// ImportStarted answers POST /imports. AlreadyFinished means the same file was
// imported before and there is nothing to send.
type ImportStarted struct {
	Import          Import `json:"import"`
	AlreadyFinished bool   `json:"already_finished"`
}

// ImportPlace is a place in an import batch, found again by GooglePlaceID.
type ImportPlace struct {
	GooglePlaceID string  `json:"google_place_id"`
	Name          string  `json:"name"`
	Latitude      float64 `json:"latitude"`
	Longitude     float64 `json:"longitude"`
	Address       string  `json:"address"`
	Kind          string  `json:"kind"`
	Source        string  `json:"source"`
}

// ImportVisit is a visit in an import batch, found again by Source and
// SourceRef. GooglePlaceID names a place in this batch or an earlier one.
type ImportVisit struct {
	Source             string   `json:"source"`
	SourceRef          string   `json:"source_ref"`
	StartedAt          int64    `json:"started_at"`
	EndedAt            int64    `json:"ended_at"`
	Latitude           *float64 `json:"latitude"`
	Longitude          *float64 `json:"longitude"`
	GooglePlaceID      string   `json:"google_place_id"`
	Probability        *float64 `json:"probability"`
	SourceSemanticType string   `json:"source_semantic_type"`
}

// ImportTrip is a trip in an import batch, found again by Source and
// SourceRef.
type ImportTrip struct {
	Source         string   `json:"source"`
	SourceRef      string   `json:"source_ref"`
	StartedAt      int64    `json:"started_at"`
	EndedAt        int64    `json:"ended_at"`
	StartLatitude  *float64 `json:"start_latitude"`
	StartLongitude *float64 `json:"start_longitude"`
	EndLatitude    *float64 `json:"end_latitude"`
	EndLongitude   *float64 `json:"end_longitude"`
	// StartGooglePlaceID and EndGooglePlaceID link the trip's ends to places
	// already known by those ids; an unknown id links nothing.
	StartGooglePlaceID string      `json:"start_google_place_id"`
	EndGooglePlaceID   string      `json:"end_google_place_id"`
	Mode               string      `json:"mode"`
	SourceMode         string      `json:"source_mode"`
	DistanceMeters     *float64    `json:"distance_meters"`
	Probability        *float64    `json:"probability"`
	Path               []PathPoint `json:"path"`
}

// ImportPoint is a raw point in an import batch.
type ImportPoint struct {
	Timestamp      int64    `json:"timestamp"`
	Latitude       float64  `json:"latitude"`
	Longitude      float64  `json:"longitude"`
	AccuracyMeters *float64 `json:"accuracy_meters"`
	Source         string   `json:"source"`
	SourceRef      string   `json:"source_ref"`
}

// ImportBatch is the body of POST /imports/{id}/batches. Places are written
// first, so a visit may name a place in the same batch.
type ImportBatch struct {
	Places []ImportPlace `json:"places"`
	Visits []ImportVisit `json:"visits"`
	Trips  []ImportTrip  `json:"trips"`
	Points []ImportPoint `json:"points"`
}

// PlaceCreate is the body of POST /places.
type PlaceCreate struct {
	Name         string   `json:"name"`
	Latitude     *float64 `json:"latitude"`
	Longitude    *float64 `json:"longitude"`
	RadiusMeters float64  `json:"radius_meters"`
	Address      string   `json:"address"`
	Kind         string   `json:"kind"`
	Description  string   `json:"description"`
	DescribedBy  string   `json:"described_by"`
	Tags         []string `json:"tags"`
}

// VisitCreate is the body of POST /visits.
type VisitCreate struct {
	PlaceID     *string  `json:"place_id"`
	StartedAt   int64    `json:"started_at"`
	EndedAt     int64    `json:"ended_at"`
	Latitude    *float64 `json:"latitude"`
	Longitude   *float64 `json:"longitude"`
	Description string   `json:"description"`
	DescribedBy string   `json:"described_by"`
	Tags        []string `json:"tags"`
}

// TripCreate is the body of POST /trips: usually a road trip or holiday that
// imported drives are then attached to.
type TripCreate struct {
	// Kind is journey when left out.
	Kind           string   `json:"kind"`
	ParentTripID   *string  `json:"parent_trip_id"`
	StartedAt      int64    `json:"started_at"`
	EndedAt        int64    `json:"ended_at"`
	StartLatitude  *float64 `json:"start_latitude"`
	StartLongitude *float64 `json:"start_longitude"`
	EndLatitude    *float64 `json:"end_latitude"`
	EndLongitude   *float64 `json:"end_longitude"`
	StartPlaceID   *string  `json:"start_place_id"`
	EndPlaceID     *string  `json:"end_place_id"`
	Mode           string   `json:"mode"`
	DistanceMeters *float64 `json:"distance_meters"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	DescribedBy    string   `json:"described_by"`
	Tags           []string `json:"tags"`
}

// AttachChildren is the body of POST /trips/{id}/children.
type AttachChildren struct {
	TripIDs []string `json:"trip_ids"`
}

// Revision is the whole state of a place, visit or trip just before one
// change: Row is that record as GET answered it then.
type Revision struct {
	RevisionID int64  `json:"revision_id"`
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Reason     string `json:"reason"`
	ReplacedAt int64  `json:"replaced_at"`
	Row        any    `json:"row"`
}

// Counts is what GET /health counts.
type Counts struct {
	Places    int `json:"places"`
	Visits    int `json:"visits"`
	Trips     int `json:"trips"`
	Points    int `json:"points"`
	Imports   int `json:"imports"`
	Deleted   int `json:"deleted"`
	Revisions int `json:"revisions"`
}

// Health is the answer to GET /health.
type Health struct {
	Status string `json:"status"`
	Counts Counts `json:"counts"`
}

// Locality is the town a coordinate is in, named offline from GeoNames'
// towns of 1000 people or more: of the towns whose reach covers the
// coordinate, the most populous, else the nearest.
type Locality struct {
	City string `json:"city"`
	// Region is the first-level division: a state, province or county.
	Region      string `json:"region"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	// TimeZone is the town's IANA time zone (Africa/Ndjamena).
	TimeZone   string `json:"time_zone"`
	GeonamesID int64  `json:"geonames_id"`
	// DistanceMeters is from the coordinate to the town's center.
	DistanceMeters float64 `json:"distance_meters"`
	// DescribedBy says who named it: the command and the data it read.
	DescribedBy string `json:"described_by"`
}

// LocalityFillRequest is the body of POST /localities/fill.
type LocalityFillRequest struct {
	DryRun bool `json:"dry_run"`
}

// LocalityFill answers POST /localities/fill: how many empty localities it
// filled (or, in a dry run, would fill), and how many it could not name.
type LocalityFill struct {
	DryRun          bool `json:"dry_run"`
	Places          int  `json:"places"`
	Visits          int  `json:"visits"`
	TripStarts      int  `json:"trip_starts"`
	TripEnds        int  `json:"trip_ends"`
	AlreadyNamed    int  `json:"already_named"`
	NoTownNearby    int  `json:"no_town_nearby"`
	NoCoordinate    int  `json:"no_coordinate"`
	DistinctTowns   int  `json:"distinct_towns"`
	GeonamesTownsIn int  `json:"geonames_towns_in"`
}

// HomePeriod is a stretch of time with one home base: the place the user
// slept most nights. EndedAt 0 means it runs on past the newest data.
type HomePeriod struct {
	// ID is home_period_000001.
	ID string `json:"id"`
	// PlaceID is the home place; null when the nights there named no place.
	PlaceID   *string   `json:"place_id"`
	PlaceName string    `json:"place_name"`
	Latitude  float64   `json:"latitude"`
	Longitude float64   `json:"longitude"`
	Locality  *Locality `json:"locality"`
	StartedAt int64     `json:"started_at"`
	EndedAt   int64     `json:"ended_at"`
	// NightsKnown is how many nights of the period the data places anywhere;
	// NightsAtHome how many of those were at the home.
	NightsKnown  int    `json:"nights_known"`
	NightsAtHome int    `json:"nights_at_home"`
	DescribedBy  string `json:"described_by"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
	DeletedAt    int64  `json:"deleted_at"`
}

// JourneyGroupRequest is the body of POST /journeys/group. A zero threshold
// takes its default.
type JourneyGroupRequest struct {
	DryRun bool `json:"dry_run"`
	// AwayDistanceMeters is how far from home counts as away (80 km).
	AwayDistanceMeters float64 `json:"away_distance_meters"`
	// MinimumAwaySeconds is how long a span away without a night away must
	// last to be a journey (6 h).
	MinimumAwaySeconds int64 `json:"minimum_away_seconds"`
	// HomeChangeDays is how many days a new night place must lead before it
	// becomes the home (21).
	HomeChangeDays int `json:"home_change_days"`
	// ReturnBreakSeconds is how long back near home, without a night at
	// home, ends a journey (12 h).
	ReturnBreakSeconds int64 `json:"return_break_seconds"`
}

// PlannedJourney is one journey a grouping found, and what it did or would do.
type PlannedJourney struct {
	// Action is create, update, unchanged or skip.
	Action string `json:"action"`
	// TripID is the journey's trip; "" for one a dry run would create.
	TripID    string `json:"trip_id"`
	Title     string `json:"title"`
	StartedAt int64  `json:"started_at"`
	EndedAt   int64  `json:"ended_at"`
	Mode      string `json:"mode"`
	// Nights is how many nights (03:00 local) fall in the journey.
	Nights int `json:"nights"`
	// FarthestMeters is the farthest the journey got from home.
	FarthestMeters float64  `json:"farthest_meters"`
	ChildTripIDs   []string `json:"child_trip_ids"`
	// ChildrenAttached counts trips this run made (or would make) part of it;
	// ChildrenDetached trips it took back out; ChildrenKeptElsewhere trips in
	// its span whose parent someone set by hand, left where they are.
	ChildrenAttached      int `json:"children_attached"`
	ChildrenDetached      int `json:"children_detached"`
	ChildrenKeptElsewhere int `json:"children_kept_elsewhere"`
	// Notes says why it was skipped, or what looks doubtful.
	Notes []string `json:"notes"`
}

// JourneyGrouping answers POST /journeys/group.
type JourneyGrouping struct {
	DryRun      bool                `json:"dry_run"`
	Request     JourneyGroupRequest `json:"request"`
	HomePeriods []HomePeriod        `json:"home_periods"`
	Journeys    []PlannedJourney    `json:"journeys"`
	// DeletedJourneyIDs are journeys an earlier run made that the data no
	// longer supports; soft-deleted, so they can be restored.
	DeletedJourneyIDs []string `json:"deleted_journey_ids"`
	// HomePlacesMarked are places whose kind this run set to home.
	HomePlacesMarked []string `json:"home_places_marked"`
	Notes            []string `json:"notes"`
}

// JourneyLocality is one town a journey stayed in, in order of arrival.
type JourneyLocality struct {
	Locality Locality `json:"locality"`
	// Seconds is the time the journey's visits spent there.
	Seconds   int64 `json:"seconds"`
	ArrivedAt int64 `json:"arrived_at"`
}

// JourneySummary answers GET /journeys/{id}: the journey's own trip, the
// trips that make it up, the visits in its span and the towns it stayed in.
type JourneySummary struct {
	Trip       Trip              `json:"trip"`
	Children   []Trip            `json:"children"`
	Visits     []Visit           `json:"visits"`
	Localities []JourneyLocality `json:"localities"`
	// CalendarEventLinks are the live calendar events linked to the
	// journey, to its trips or to its visits.
	CalendarEventLinks []CalendarEventLink `json:"calendar_event_links"`
}

// CalendarEventLink ties one calendar event to where the user was for it: a
// visit, a trip (a flight, or a journey) or a place. location-store owns the
// link; the event belongs to Google Calendar and is found again by
// CalendarID and EventID, never by its summary.
type CalendarEventLink struct {
	// ID is calendar_event_link_000001.
	ID string `json:"id"`
	// CalendarID and EventID are Google Calendar's ids for the event: what
	// the link is found again by.
	CalendarID string `json:"calendar_id"`
	EventID    string `json:"event_id"`
	// RecurringEventID is the series the event is one instance of; "" for a
	// single event.
	RecurringEventID string `json:"recurring_event_id"`
	// Summary and Location are the event's as last read, for display only.
	Summary  string `json:"summary"`
	Location string `json:"location"`
	// StartedAt and EndedAt are the event's times; an all-day event's are
	// its local days.
	StartedAt int64 `json:"started_at"`
	EndedAt   int64 `json:"ended_at"`
	AllDay    bool  `json:"all_day"`
	// TargetType is visit, trip or place; TargetID that record's id.
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	// Rule is how it matched: one of GET /vocabularies calendar_match_rules.
	Rule string `json:"rule"`
	// Confidence is 0..1: how likely the event is what the user was there
	// for.
	Confidence float64 `json:"confidence"`
	// Reason says, in words, what agreed.
	Reason string `json:"reason"`
	// MatchedBy is who made the link: location-match-calendar, or hand.
	MatchedBy string `json:"matched_by"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	DeletedAt int64  `json:"deleted_at"`
	// DeletedBy is who deleted it. A link a person deleted stays deleted: a
	// later matching run does not make it again.
	DeletedBy string `json:"deleted_by"`
}

// CalendarEventLinkCreate is the body of POST /calendar-event-links: a link
// a person or agent makes by hand.
type CalendarEventLinkCreate struct {
	CalendarID       string `json:"calendar_id"`
	EventID          string `json:"event_id"`
	RecurringEventID string `json:"recurring_event_id"`
	Summary          string `json:"summary"`
	Location         string `json:"location"`
	StartedAt        int64  `json:"started_at"`
	EndedAt          int64  `json:"ended_at"`
	AllDay           bool   `json:"all_day"`
	TargetType       string `json:"target_type"`
	TargetID         string `json:"target_id"`
	Reason           string `json:"reason"`
}

// GoogleCalendar is the part of a Google Calendar list entry the matching
// reads.
type GoogleCalendar struct {
	ID              string `json:"id"`
	Summary         string `json:"summary"`
	SummaryOverride string `json:"summaryOverride"`
	// AccessRole is owner, writer, reader or freeBusyReader. Only events on
	// a calendar the user owns or writes count without a location: a
	// calendar they only read holds other people's days off and meetings.
	AccessRole string `json:"accessRole"`
	// TimeZone places an all-day event that names no zone of its own.
	TimeZone string `json:"timeZone"`
}

// GoogleEventTime is a Google Calendar event's start or end: DateTime for a
// timed event, Date (YYYY-MM-DD) for an all-day one.
type GoogleEventTime struct {
	DateTime string `json:"dateTime"`
	Date     string `json:"date"`
	TimeZone string `json:"timeZone"`
}

// GoogleCalendarEvent is the part of a Google Calendar event the matching
// reads, with Google's own field names.
type GoogleCalendarEvent struct {
	ID               string          `json:"id"`
	Status           string          `json:"status"`
	EventType        string          `json:"eventType"`
	Summary          string          `json:"summary"`
	Location         string          `json:"location"`
	RecurringEventID string          `json:"recurringEventId"`
	HangoutLink      string          `json:"hangoutLink"`
	Start            GoogleEventTime `json:"start"`
	End              GoogleEventTime `json:"end"`
}

// CalendarEvents is one calendar and its events in a window.
type CalendarEvents struct {
	Calendar GoogleCalendar        `json:"calendar"`
	Events   []GoogleCalendarEvent `json:"events"`
}

// CalendarMatchRequest is the body of POST /calendar/match: the events of
// each calendar read for the window From to To.
type CalendarMatchRequest struct {
	DryRun bool `json:"dry_run"`
	// From and To are the window the events were read for. The run settles
	// its own links of these calendars' events in it, and no others.
	From      int64            `json:"from"`
	To        int64            `json:"to"`
	Calendars []CalendarEvents `json:"calendars"`
}

// PlannedCalendarEventLink is one link a matching run found, or one of its
// own it no longer finds, and what it did or would do.
type PlannedCalendarEventLink struct {
	// Action is create, update, unchanged, restore, delete, or
	// kept_deleted (a person deleted the link; it stays deleted).
	Action string            `json:"action"`
	Link   CalendarEventLink `json:"link"`
}

// CalendarMatching answers POST /calendar/match.
type CalendarMatching struct {
	DryRun bool  `json:"dry_run"`
	From   int64 `json:"from"`
	To     int64 `json:"to"`
	// EventsRead is how many events the request carried.
	EventsRead int `json:"events_read"`
	// LinksByRule counts the links found, by rule.
	LinksByRule map[string]int `json:"links_by_rule"`
	// Unlinked counts the events that found nothing, by why.
	Unlinked map[string]int `json:"unlinked"`
	// Actions counts the links by action.
	Actions map[string]int             `json:"actions"`
	Links   []PlannedCalendarEventLink `json:"links"`
	// SeriesPlaces is what the run found for each recurring series with no
	// location: the spot the user was at for most of its times out, and
	// whether that was enough to link.
	SeriesPlaces []CalendarSeriesPlace `json:"series_places"`
	Notes        []string              `json:"notes"`
}

// CalendarSeriesPlace is where a recurring series with no location seems to
// be held: of the times of the series the user was out (at a place not home,
// and not away on a journey), how many found them at one spot. Served by GET
// /calendar-series-places; location-match-calendar rewrites it each run that
// reads the series.
type CalendarSeriesPlace struct {
	// ID is calendar_series_place_000001.
	ID string `json:"id"`
	// CalendarID and RecurringEventID are Google's ids for the series: what
	// the row is found again by.
	CalendarID       string `json:"calendar_id"`
	RecurringEventID string `json:"recurring_event_id"`
	// Summary is the series' as last read, for display only.
	Summary string `json:"summary"`
	// PlaceID is the place of the leading spot, nil when the user was never
	// out at its times; PlaceName is read with it, for display.
	PlaceID   *string `json:"place_id"`
	PlaceName string  `json:"place_name"`
	// SpotPlaceIDs are the places within reach of PlaceID that count as the
	// same spot: two Google place ids for one building.
	SpotPlaceIDs []string `json:"spot_place_ids"`
	// Instances is how many timed times of the series the window held;
	// InstancesOut, at how many of them the user was out; InstancesAtPlace,
	// how many of those found them at the spot.
	Instances        int `json:"instances"`
	InstancesOut     int `json:"instances_out"`
	InstancesAtPlace int `json:"instances_at_place"`
	// Agreement is InstancesAtPlace / InstancesOut.
	Agreement float64 `json:"agreement"`
	// Confidence is what the series' links carry; 0 unless Outcome is linked.
	Confidence float64 `json:"confidence"`
	// Outcome is one of GET /vocabularies series_outcomes.
	Outcome         string `json:"outcome"`
	FirstInstanceAt int64  `json:"first_instance_at"`
	LastInstanceAt  int64  `json:"last_instance_at"`
	// WindowFrom and WindowTo are the window of the run that wrote it.
	WindowFrom int64  `json:"window_from"`
	WindowTo   int64  `json:"window_to"`
	MatchedBy  string `json:"matched_by"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// PlaceVisitPattern is when the user goes to a place: how often, how long,
// and the weekday and hour they usually arrive, in the place's local time.
type PlaceVisitPattern struct {
	Visits int `json:"visits"`
	// Days is how many local days hold a visit.
	Days  int     `json:"days"`
	Hours float64 `json:"hours"`
	// FirstVisitAt and LastVisitAt are the first visit's start and the last
	// one's end.
	FirstVisitAt int64 `json:"first_visit_at"`
	LastVisitAt  int64 `json:"last_visit_at"`
	// TimeZone is the zone the weekdays and hours are read in: the place's
	// town's, else UTC.
	TimeZone string `json:"time_zone"`
	// UsualWeekday is the weekday most visits began on (Monday), and
	// UsualWeekdayVisits how many did.
	UsualWeekday       string `json:"usual_weekday"`
	UsualWeekdayVisits int    `json:"usual_weekday_visits"`
	// UsualArrivalHour is the local hour most visits began in (0..23).
	UsualArrivalHour       int `json:"usual_arrival_hour"`
	UsualArrivalHourVisits int `json:"usual_arrival_hour_visits"`
	// ArrivalsByWeekday counts visits by the weekday they began, Sunday
	// first; ArrivalsByHour by the local hour.
	ArrivalsByWeekday []int `json:"arrivals_by_weekday"`
	ArrivalsByHour    []int `json:"arrivals_by_hour"`
	// MedianVisitMinutes is how long a visit there usually lasts.
	MedianVisitMinutes float64 `json:"median_visit_minutes"`
}

// UnnamedPlace is a place with no name the user has been to, and when: GET
// /unnamed-places ranks them by days visited, home left out.
type UnnamedPlace struct {
	Place   Place             `json:"place"`
	Pattern PlaceVisitPattern `json:"pattern"`
}

// OSMFeature is a named point of interest from an OpenStreetMap extract,
// read offline, near a place.
type OSMFeature struct {
	// OSMRef is OpenStreetMap's id for it: node/123, way/456.
	OSMRef    string            `json:"osm_ref"`
	Name      string            `json:"name"`
	Tags      map[string]string `json:"tags"`
	Latitude  float64           `json:"latitude"`
	Longitude float64           `json:"longitude"`
	// Address is the feature's own addr:* tags written out, "" when it has
	// none.
	Address        string  `json:"address"`
	DistanceMeters float64 `json:"distance_meters"`
}

// OSMAddress is an address from an OpenStreetMap extract: a node or building
// with addr:housenumber and addr:street.
type OSMAddress struct {
	OSMRef         string  `json:"osm_ref"`
	Address        string  `json:"address"`
	Latitude       float64 `json:"latitude"`
	Longitude      float64 `json:"longitude"`
	DistanceMeters float64 `json:"distance_meters"`
}

// GeocodedCalendarEvent is a calendar event whose location's street address
// was found in an OpenStreetMap extract, offline, near a place.
type GeocodedCalendarEvent struct {
	CalendarID string `json:"calendar_id"`
	EventID    string `json:"event_id"`
	// RecurringEventID is the series, "" for a single event.
	RecurringEventID string `json:"recurring_event_id"`
	Summary          string `json:"summary"`
	Location         string `json:"location"`
	StartedAt        int64  `json:"started_at"`
	EndedAt          int64  `json:"ended_at"`
	// Venue is the part of the location before its street address, "" when
	// the location starts with the address.
	Venue string `json:"venue"`
	// Address is the OpenStreetMap address the location matched.
	Address        OSMAddress `json:"address"`
	DistanceMeters float64    `json:"distance_meters"`
	// VisitOverlapped says a visit at the place overlapped the event: the
	// user was there while it ran. Set by location-store, not the sender.
	VisitOverlapped bool `json:"visit_overlapped"`
}

// PlaceProposalEvidence is what the suggester found near one unnamed place.
type PlaceProposalEvidence struct {
	PlaceID string `json:"place_id"`
	// NearbyPOIs are named features within reach, nearest first.
	NearbyPOIs []OSMFeature `json:"nearby_pois"`
	// NearestAddress is the nearest address within reach; nil when none.
	NearestAddress *OSMAddress `json:"nearest_address"`
	// CalendarEvents are events whose address lies within reach.
	CalendarEvents []GeocodedCalendarEvent `json:"calendar_events"`
}

// PlaceSuggestionProposals is the body of POST /place-suggestions/proposals:
// what location-suggest-place-names found offline for each unnamed place.
// The store decides the suggestions from it.
type PlaceSuggestionProposals struct {
	DryRun bool `json:"dry_run"`
	// ProposedBy is who found it: location-suggest-place-names.
	ProposedBy string `json:"proposed_by"`
	// Sources names the data read: each extract's file name and date.
	Sources []string                `json:"sources"`
	Places  []PlaceProposalEvidence `json:"places"`
}

// PlaceSuggestionEvidence is everything a suggestion rests on: the place's
// visit pattern and all that was found near it, so a person deciding sees
// the rivals too.
type PlaceSuggestionEvidence struct {
	Pattern        PlaceVisitPattern       `json:"pattern"`
	NearbyPOIs     []OSMFeature            `json:"nearby_pois"`
	NearestAddress *OSMAddress             `json:"nearest_address"`
	CalendarEvents []GeocodedCalendarEvent `json:"calendar_events"`
	// POIsWithin25Meters counts the named features within 25 m: one alone
	// is a strong suggestion, several are rivals.
	POIsWithin25Meters int      `json:"pois_within_25_meters"`
	Sources            []string `json:"sources"`
}

// PlaceSuggestion is one name a place might have, never applied until a
// person or agent accepts it.
type PlaceSuggestion struct {
	// ID is place_suggestion_000001.
	ID      string `json:"id"`
	PlaceID string `json:"place_id"`
	// Source is one of GET /vocabularies place_suggestion_sources; SourceRef
	// is the OpenStreetMap id it came from (node/123), or for a calendar
	// address, calendar:<the address's OpenStreetMap id>.
	Source    string `json:"source"`
	SourceRef string `json:"source_ref"`
	// Name, Address and Kind are what accepting it writes onto the place
	// (each only where the place has none); Kind "" changes nothing.
	Name           string  `json:"name"`
	Address        string  `json:"address"`
	Kind           string  `json:"kind"`
	DistanceMeters float64 `json:"distance_meters"`
	// Reason says, in words, why it was suggested.
	Reason   string                  `json:"reason"`
	Evidence PlaceSuggestionEvidence `json:"evidence"`
	// Status is proposed, accepted or rejected.
	Status     string `json:"status"`
	ProposedBy string `json:"proposed_by"`
	// DecidedBy, DecidedAt and DecisionNote say who accepted or rejected it
	// and why.
	DecidedBy    string `json:"decided_by"`
	DecidedAt    int64  `json:"decided_at"`
	DecisionNote string `json:"decision_note"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
	// DeletedAt is set when a later run no longer found a proposed one.
	DeletedAt int64 `json:"deleted_at"`
}

// PlaceSuggestionDecision is the body of POST /place-suggestions/{id}/accept
// and /reject. On accept, Name, Address and Kind replace the suggestion's
// own when set: a cleaner spelling of the same thing.
type PlaceSuggestionDecision struct {
	DecidedBy string `json:"decided_by"`
	Note      string `json:"note"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	Kind      string `json:"kind"`
}

// PlaceSuggestionsForPlace is one place's suggestions, as POST
// /place-suggestions/proposals answers them.
type PlaceSuggestionsForPlace struct {
	Place       Place             `json:"place"`
	Suggestions []PlaceSuggestion `json:"suggestions"`
}

// PlaceSuggesting answers POST /place-suggestions/proposals.
type PlaceSuggesting struct {
	DryRun bool `json:"dry_run"`
	// Places are the places sent, with their live suggestions, most days
	// visited first.
	Places []PlaceSuggestionsForPlace `json:"places"`
	// Actions counts the suggestions by what the run did: create, update,
	// unchanged, kept_decided (accepted or rejected before), delete.
	Actions map[string]int `json:"actions"`
}

// LivePoints is the body of POST /points/live: fixes one phone made, sent as
// it made them. RecordedBy is who sent them, written by the front door.
type LivePoints struct {
	Device     string      `json:"device"`
	RecordedBy string      `json:"recorded_by"`
	Points     []LivePoint `json:"points"`
}

// LivePoint is one fix: epoch seconds, and accuracy in meters when the phone
// gave one.
type LivePoint struct {
	Timestamp      int64    `json:"timestamp"`
	Latitude       float64  `json:"latitude"`
	Longitude      float64  `json:"longitude"`
	AccuracyMeters *float64 `json:"accuracy_meters"`
}

// LivePointsRecorded answers POST /points/live: the imports (one per device
// and UTC day) the points went into, and what was new.
type LivePointsRecorded struct {
	ImportIDs       []string `json:"import_ids"`
	PointsAdded     int      `json:"points_added"`
	PointsUnchanged int      `json:"points_unchanged"`
}

// PhoneSegmentRequest is the body of POST /phone-points/segment: the span of
// phone points to make into visits and movements. A dry run writes nothing.
type PhoneSegmentRequest struct {
	DryRun bool  `json:"dry_run"`
	From   int64 `json:"from"`
	To     int64 `json:"to"`
}

// PhoneSegmentAction is what a segmenting run did to one visit or movement.
// Action is create, update, unchanged, delete, kept_edited, kept_deleted or
// skip_other_source.
type PhoneSegmentAction struct {
	Action         string   `json:"action"`
	EntityType     string   `json:"entity_type"`
	ID             string   `json:"id"`
	StartedAt      int64    `json:"started_at"`
	EndedAt        int64    `json:"ended_at"`
	PlaceID        *string  `json:"place_id,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	DistanceMeters *float64 `json:"distance_meters,omitempty"`
	Points         int      `json:"points"`
	Note           string   `json:"note,omitempty"`
}

// PhoneSegmentation answers POST /phone-points/segment. From is where the run
// began reading, which is earlier than asked when a stored phone visit or
// movement runs across the start.
type PhoneSegmentation struct {
	DryRun         bool                 `json:"dry_run"`
	From           int64                `json:"from"`
	To             int64                `json:"to"`
	PointsRead     int                  `json:"points_read"`
	PointsTooRough int                  `json:"points_too_rough"`
	PlacesCreated  []string             `json:"places_created"`
	Actions        []PhoneSegmentAction `json:"actions"`
	ActionCounts   map[string]int       `json:"action_counts"`
}
