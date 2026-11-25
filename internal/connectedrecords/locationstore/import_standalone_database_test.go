package locationstore

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"

	_ "modernc.org/sqlite"
)

// newStandaloneDatabase writes a made-up standalone location-store file: the
// live standalone schema, then rows. Each statement runs in order.
func newStandaloneDatabase(t *testing.T, statements ...string) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "location-store.db")
	database, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(string(schema)); err != nil {
		t.Fatalf("applying the standalone schema: %v", err)
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("writing the standalone rows: %v\n%s", err, statement)
		}
	}
	return path
}

// standaloneRows is a small made-up history on the test grid: two imports, a
// place merged into a later one, a visit at the kept place, a movement inside
// a later journey with a two-point path, three points, revisions with gaps in
// their numbers, a home period, what the first import's file holds, a
// calendar link, a series place and a place suggestion.
var standaloneRows = []string{
	`INSERT INTO imports (id, seq, kind, sha256, source_machine, source_path, imported_at, finished_at, counts, snapshot, snapshot_changes, recorded_by)
	 VALUES ('import_000001', 1, 'google_timeline', '` + strings.Repeat("a", 64) + `', 'laptop', 'exports/Timeline.json', 1700000000, 1700000100,
	 '{"points_added":2}', '{"covers_from":1000,"covers_to":9000,"raw_signals_oldest_at":0,"raw_signals_newest_at":0}', '{"visits_superseded":0,"trips_superseded":0,"visits_restored":0,"trips_restored":0,"kept_edited_ids":[]}', '')`,
	`INSERT INTO imports (id, seq, kind, sha256, source_machine, source_path, imported_at, finished_at, counts, recorded_by)
	 VALUES ('import_000004', 4, 'phone_live', '` + strings.Repeat("b", 64) + `', 'example-phone', '', 1700000200, 1700000300, '{"points_added":1}', 'operator on dash')`,
	`INSERT INTO places (id, seq, name, latitude, longitude, radius_meters, address, kind, google_place_id, description, described_by, tags, source, import_id,
	 created_at, updated_at, deleted_at, locality, merged_into_place_id)
	 VALUES ('place_000002', 2, '', 10.001, 20.001, 0, '', 'other', 'example-cafe-old', '', '', '[]', 'google_timeline', 'import_000001', 1700000000, 1700000400, 1700000400, NULL, 'place_000007')`,
	`INSERT INTO places (id, seq, name, latitude, longitude, radius_meters, address, kind, google_place_id, description, described_by, tags, source, import_id,
	 created_at, updated_at, deleted_at, locality, merged_into_place_id)
	 VALUES ('place_000007', 7, 'Example Lake Cafe', 10.0012, 20.0012, 40, '1 Fake Road', 'venue', 'example-cafe', 'Good tea', 'hand', '["coffee"]', 'manual', NULL,
	 1700000300, 1700000300, 0, '{"city":"Exampletown","region":"","country":"Exampleland","country_code":"EX","time_zone":"UTC","geonames_id":1,"distance_meters":10,"described_by":"test"}', NULL)`,
	`INSERT INTO visits (id, seq, place_id, started_at, ended_at, latitude, longitude, probability, source_semantic_type, description, described_by, tags, source,
	 source_ref, import_id, created_at, updated_at, deleted_at, locality, superseded_by_import_id)
	 VALUES ('visit_000003', 3, 'place_000007', 1000, 2000, 10.001, 20.001, 0.9, 'UNKNOWN', '', '', '[]', 'google_timeline', '1000-2000', 'import_000001',
	 1700000000, 1700000400, 0, NULL, NULL)`,
	`INSERT INTO trips (id, seq, parent_trip_id, started_at, ended_at, start_latitude, start_longitude, end_latitude, end_longitude, start_place_id, end_place_id,
	 mode, source_mode, distance_meters, probability, title, description, described_by, tags, source, source_ref, import_id, created_at, updated_at, deleted_at,
	 start_locality, end_locality, kind, title_by, superseded_by_import_id, parent_set_by)
	 VALUES ('trip_000001', 1, 'trip_000005', 2000, 3000, 10.001, 20.001, 10.05, 20.05, 'place_000007', NULL, 'driving', 'IN_PASSENGER_VEHICLE', 7000, 0.8,
	 '', '', '', '[]', 'google_timeline', '2000-3000', 'import_000001', 1700000000, 1700000000, 0, NULL, NULL, 'movement', '', NULL, 'hand')`,
	`INSERT INTO trips (id, seq, parent_trip_id, started_at, ended_at, mode, title, source, created_at, updated_at, kind, title_by)
	 VALUES ('trip_000005', 5, NULL, 500, 9000, 'driving', 'Lake week', 'manual', 1700000500, 1700000500, 'journey', 'hand')`,
	`INSERT INTO trip_path_points (trip_id, position, latitude, longitude, timestamp) VALUES ('trip_000001', 0, 10.01, 20.01, 2100), ('trip_000001', 1, 10.03, 20.03, NULL)`,
	`INSERT INTO points (timestamp, latitude, longitude, accuracy_meters, source, source_ref, import_id) VALUES
	 (1500, 10.001, 20.001, 12, 'google_timeline_raw', '', 'import_000001'),
	 (2500, 10.02, 20.02, NULL, 'google_timeline_raw', '', 'import_000001'),
	 (1767225600, 10.0001, 20.0, 15, 'phone', '', 'import_000004')`,
	`INSERT INTO revisions (revision_id, entity_type, entity_id, reason, replaced_at, row_json)
	 VALUES (3, 'place', 'place_000002', 'merge', 1700000400, '{"id":"place_000002","name":"","tags":[]}')`,
	`INSERT INTO revisions (revision_id, entity_type, entity_id, reason, replaced_at, row_json)
	 VALUES (8, 'trip', 'trip_000001', 'attach', 1700000500, '{"id":"trip_000001","parent_trip_id":null,"tags":[]}')`,
	`INSERT INTO home_periods (id, seq, place_id, latitude, longitude, locality, started_at, ended_at, nights_known, nights_at_home, described_by, created_at, updated_at, deleted_at)
	 VALUES ('home_period_000002', 2, NULL, 10.0, 20.0, NULL, 100, 0, 30, 28, 'location-group-journeys', 1700000600, 1700000600, 0)`,
	`INSERT INTO import_records (import_id, entity_type, entity_id) VALUES ('import_000001', 'visit', 'visit_000003'), ('import_000001', 'trip', 'trip_000001')`,
	`INSERT INTO calendar_event_links (id, seq, calendar_id, event_id, recurring_event_id, summary, location, started_at, ended_at, all_day, target_type, target_id,
	 rule, confidence, reason, matched_by, created_at, updated_at, deleted_at, deleted_by)
	 VALUES ('calendar_event_link_000006', 6, 'calendar-a', 'event-a', '', 'Tea', 'Example Lake Cafe', 1000, 2000, 1, 'visit', 'visit_000003',
	 'hand', 1, '', 'hand', 1700000700, 1700000700, 0, '')`,
	`INSERT INTO calendar_series_places (id, seq, calendar_id, recurring_event_id, summary, place_id, spot_place_ids, instances, instances_out, instances_at_place,
	 agreement, confidence, outcome, first_instance_at, last_instance_at, window_from, window_to, matched_by, created_at, updated_at)
	 VALUES ('calendar_series_place_000001', 1, 'calendar-a', 'series-a', 'Class', 'place_000007', '["place_000007"]', 5, 4, 3, 0.75, 0.7, 'linked',
	 1000, 9000, 0, 10000, 'location-match-calendar', 1700000800, 1700000800)`,
	`INSERT INTO place_suggestions (id, seq, place_id, source, source_ref, name, address, kind, distance_meters, reason, evidence, status, proposed_by,
	 decided_by, decided_at, decision_note, created_at, updated_at, deleted_at)
	 VALUES ('place_suggestion_000003', 3, 'place_000002', 'osm_poi', 'node/1', 'Example Kiosk', '', 'venue', 12, 'near', '{}', 'rejected', 'location-suggest-place-names',
	 'hand', 1700000900, 'wrong', 1700000850, 1700000900, 0)`,
}

func TestImportingAStandaloneDatabaseKeepsEveryIDAndOwnsEveryRecord(t *testing.T) {
	service := newTestService(t)
	ctx := context.Background()
	path := newStandaloneDatabase(t, standaloneRows...)
	counts, err := ImportStandaloneDatabase(ctx, service.store.db, path, alicePrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	want := StandaloneDatabaseImportCounts{Imports: 2, Places: 2, Visits: 1, Trips: 2, TripPathPoints: 2, Points: 3, Revisions: 2, HomePeriods: 1,
		ImportRecords: 2, CalendarEventLinks: 1, CalendarSeriesPlaces: 1, PlaceSuggestions: 1}
	if counts != want {
		t.Fatalf("counts = %s, want %s", counts.Summary(), want.Summary())
	}
	if summary := counts.Summary(); !strings.HasPrefix(summary, "imports=2 places=2 visits=1 trips=2 trip_path_points=2 points=3") {
		t.Errorf("summary = %q", summary)
	}

	// Every row is Alice's, and Bob reaches none of them.
	for _, table := range []string{"imports", "places", "visits", "trips", "trip_path_points", "points", "revisions", "home_periods", "import_records",
		"calendar_event_links", "calendar_series_places", "place_suggestions"} {
		var others int
		if err := service.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE owner_principal_id <> $1`, alicePrincipalID).Scan(&others); err != nil {
			t.Fatal(err)
		}
		if others != 0 {
			t.Errorf("%s holds %d rows not owned by the import's owner", table, others)
		}
	}
	var bobsHealth Health
	service.as(bob).must(http.MethodGet, "/health", nil, &bobsHealth, http.StatusOK)
	if bobsHealth.Counts != (Counts{}) {
		t.Errorf("Bob reaches %+v of the imported records", bobsHealth.Counts)
	}

	// The ids, links and values came through as they were.
	var kept Place
	service.must(http.MethodGet, "/places/place_000007", nil, &kept, http.StatusOK)
	if kept.Name != "Example Lake Cafe" || kept.RadiusMeters != 40 || kept.Locality == nil || kept.Locality.City != "Exampletown" ||
		len(kept.Tags) != 1 || kept.Tags[0] != "coffee" || kept.OwnerPrincipalID != alicePrincipalID {
		t.Errorf("place_000007 = %+v", kept)
	}
	var merged Place
	service.must(http.MethodGet, "/places/place_000002?include_deleted=true", nil, &merged, http.StatusOK)
	if merged.MergedIntoPlaceID == nil || *merged.MergedIntoPlaceID != "place_000007" || merged.DeletedAt == 0 {
		t.Errorf("place_000002 = %+v, want deleted and merged into place_000007", merged)
	}
	var visit Visit
	service.must(http.MethodGet, "/visits/visit_000003", nil, &visit, http.StatusOK)
	if visit.PlaceName != "Example Lake Cafe" || len(visit.CalendarEventLinks) != 1 || !visit.CalendarEventLinks[0].AllDay ||
		visit.CalendarEventLinks[0].ID != "calendar_event_link_000006" || visit.Probability == nil || *visit.Probability != 0.9 {
		t.Errorf("visit_000003 = %+v", visit)
	}
	var movement Trip
	service.must(http.MethodGet, "/trips/trip_000001", nil, &movement, http.StatusOK)
	if movement.ParentTripID == nil || *movement.ParentTripID != "trip_000005" || movement.PathPoints != 2 || movement.ParentSetBy != SetByHand {
		t.Errorf("trip_000001 = %+v", movement)
	}
	var tripPath []PathPoint
	service.must(http.MethodGet, "/trips/trip_000001/path", nil, &tripPath, http.StatusOK)
	if len(tripPath) != 2 || tripPath[0].Latitude != 10.01 || tripPath[0].Timestamp == nil || *tripPath[0].Timestamp != 2100 || tripPath[1].Timestamp != nil {
		t.Errorf("trip_000001's path = %+v", tripPath)
	}
	var places []Place
	service.must(http.MethodGet, "/places?q=lake+cafe", nil, &places, http.StatusOK)
	if len(places) != 1 || places[0].ID != "place_000007" {
		t.Errorf("q=lake cafe found %d places, want place_000007", len(places))
	}
	var points []Point
	service.must(http.MethodGet, "/points", nil, &points, http.StatusOK)
	if len(points) != 3 || points[0].Timestamp != 1500 || points[0].AccuracyMeters == nil || points[1].AccuracyMeters != nil || points[2].ImportID != "import_000004" {
		t.Errorf("points = %+v", points)
	}
	var revisions []Revision
	service.must(http.MethodGet, "/trips/trip_000001/revisions", nil, &revisions, http.StatusOK)
	if len(revisions) != 1 || revisions[0].RevisionID != 8 || revisions[0].Reason != "attach" || revisions[0].OwnerPrincipalID != alicePrincipalID {
		t.Errorf("trip_000001's revisions = %+v", revisions)
	} else if row, isObject := revisions[0].Row.(map[string]any); !isObject || row["owner_principal_id"] != alicePrincipalID || row["id"] != "trip_000001" {
		t.Errorf("the revision's row = %v, want the record as it was with its owner", revisions[0].Row)
	}
	var record Import
	service.must(http.MethodGet, "/imports/import_000001", nil, &record, http.StatusOK)
	if record.Snapshot == nil || record.Snapshot.CoversTo != 9000 || record.Counts.PointsAdded != 2 || record.SnapshotChanges == nil {
		t.Errorf("import_000001 = %+v", record)
	}
	var series []CalendarSeriesPlace
	service.must(http.MethodGet, "/calendar-series-places?place_id=place_000007", nil, &series, http.StatusOK)
	if len(series) != 1 || series[0].ID != "calendar_series_place_000001" {
		t.Errorf("series places at place_000007 = %+v", series)
	}
	var suggestion PlaceSuggestion
	service.must(http.MethodGet, "/place-suggestions/place_suggestion_000003", nil, &suggestion, http.StatusOK)
	if suggestion.Status != PlaceSuggestionRejected || suggestion.OwnerPrincipalID != alicePrincipalID {
		t.Errorf("place_suggestion_000003 = %+v", suggestion)
	}
	var period HomePeriod
	service.must(http.MethodGet, "/home-periods/home_period_000002", nil, &period, http.StatusOK)
	if period.NightsAtHome != 28 {
		t.Errorf("home_period_000002 = %+v", period)
	}
	// The same file again is the same import: the copy kept the hash.
	again := service.startImport(ImportKindGoogleTimeline, strings.Repeat("a", 64))
	if again.Import.ID != "import_000001" || !again.AlreadyFinished {
		t.Errorf("the imported file again = %+v, want import_000001, finished", again)
	}

	// The next record of each kind takes the number after the highest copied.
	var place Place
	service.must(http.MethodPost, "/places", PlaceCreate{Name: "New", Latitude: float(10.2), Longitude: float(20.2), Kind: PlaceKindOther}, &place, http.StatusCreated)
	var newVisit Visit
	service.must(http.MethodPost, "/visits", VisitCreate{PlaceID: &place.ID, StartedAt: 100, EndedAt: 200}, &newVisit, http.StatusCreated)
	var trip Trip
	service.must(http.MethodPost, "/trips", TripCreate{Title: "New", StartedAt: 100, EndedAt: 200, Mode: ModeWalking}, &trip, http.StatusCreated)
	started := service.startImport(ImportKindGoogleTimeline, strings.Repeat("c", 64))
	var link CalendarEventLink
	service.must(http.MethodPost, "/calendar-event-links", CalendarEventLinkCreate{CalendarID: "calendar-b", EventID: "event-b", StartedAt: 100, EndedAt: 200,
		TargetType: EntityPlace, TargetID: place.ID}, &link, http.StatusCreated)
	service.must(http.MethodPatch, "/places/"+place.ID, map[string]any{"name": "Newer"}, nil, http.StatusOK)
	service.must(http.MethodGet, "/places/"+place.ID+"/revisions", nil, &revisions, http.StatusOK)
	for got, want := range map[string]string{place.ID: "place_000008", newVisit.ID: "visit_000004", trip.ID: "trip_000006",
		started.Import.ID: "import_000005", link.ID: "calendar_event_link_000007"} {
		if got != want {
			t.Errorf("the next record is %s, want %s", got, want)
		}
	}
	if len(revisions) != 1 || revisions[0].RevisionID != 9 {
		t.Errorf("the next revision = %+v, want revision 9", revisions)
	}
	err = postgresstorage.InTransaction(ctx, service.store.db, func(tx *sql.Tx) error {
		for table, want := range map[string]string{"home_periods": "home_period_000003", "calendar_series_places": "calendar_series_place_000002",
			"place_suggestions": "place_suggestion_000004"} {
			id, _, err := nextID(ctx, tx, table, standaloneIDPrefixes[table])
			if err != nil {
				return err
			}
			if id != want {
				t.Errorf("the next %s id is %s, want %s", table, id, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestImportingAStandaloneDatabaseRefusesWhatItCannotCopyWhole(t *testing.T) {
	service := newTestService(t)
	ctx := context.Background()
	path := newStandaloneDatabase(t, standaloneRows...)
	for _, owner := range []string{"", "alice", "principal_1"} {
		if _, err := ImportStandaloneDatabase(ctx, service.store.db, path, owner); err == nil {
			t.Errorf("owner %q was accepted", owner)
		}
	}

	extraColumn := newStandaloneDatabase(t, `ALTER TABLE visits ADD COLUMN mood TEXT`)
	if _, err := ImportStandaloneDatabase(ctx, service.store.db, extraColumn, alicePrincipalID); err == nil || !strings.Contains(err.Error(), "mood") {
		t.Errorf("a column the import does not know = %v, want a refusal naming it", err)
	}

	badID := newStandaloneDatabase(t, `INSERT INTO imports (id, seq, kind, sha256, imported_at) VALUES ('import_000009', 2, 'google_timeline', 'x', 1)`)
	if _, err := ImportStandaloneDatabase(ctx, service.store.db, badID, alicePrincipalID); err == nil || !strings.Contains(err.Error(), "import_000009") {
		t.Errorf("an id its number does not make = %v, want a refusal", err)
	}

	dangling := newStandaloneDatabase(t, `INSERT INTO visits (id, seq, place_id, started_at, ended_at, source, created_at, updated_at)
		VALUES ('visit_000001', 1, 'place_000099', 1, 2, 'manual', 1, 1)`)
	if _, err := ImportStandaloneDatabase(ctx, service.store.db, dangling, alicePrincipalID); err == nil {
		t.Error("a visit at a place the file does not hold was copied")
	}
	var held int
	if err := service.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM imports`).Scan(&held); err != nil || held != 0 {
		t.Fatalf("refused imports left %d rows, %v", held, err)
	}

	if _, err := ImportStandaloneDatabase(ctx, service.store.db, path, alicePrincipalID); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportStandaloneDatabase(ctx, service.store.db, path, bobPrincipalID); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("a second import into a module that holds rows = %v, want a refusal", err)
	}
	if places, err := service.store.ListPlaces(ctx, ownership.OwnedBy(bobPrincipalID), PlaceFilter{IncludeDeleted: true}); err != nil || len(places) != 0 {
		t.Errorf("the refused second import gave Bob %d places, %v", len(places), err)
	}
}
