package locationstore

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every coordinate here is invented: a grid near 10°N 20°E.

type testService struct {
	t      *testing.T
	store  *Store
	server *httptest.Server
}

func newTestService(t *testing.T) *testService {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	mux := http.NewServeMux()
	RegisterHandlers(mux, store)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &testService{t: t, store: store, server: server}
}

func (s *testService) call(method, path string, body any, out any) (int, []byte) {
	s.t.Helper()
	var reader io.Reader
	switch typed := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(typed)
	default:
		encoded, _ := json.Marshal(typed)
		reader = bytes.NewReader(encoded)
	}
	request, _ := http.NewRequest(method, s.server.URL+path, reader)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	if out != nil && len(answer) > 0 && strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(answer, out); err != nil {
			s.t.Fatalf("%s %s: %v: %s", method, path, err, answer)
		}
	}
	return response.StatusCode, answer
}

func (s *testService) must(method, path string, body any, out any, want int) {
	s.t.Helper()
	if status, answer := s.call(method, path, body, out); status != want {
		s.t.Fatalf("%s %s = %d %s, want %d", method, path, status, answer, want)
	}
}

func float(value float64) *float64 { return &value }

const hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func (s *testService) startImport(kind, hash string) ImportStarted {
	var started ImportStarted
	s.must(http.MethodPost, "/imports", ImportStart{Kind: kind, Sha256: hash, SourceMachine: "laptop", SourcePath: "exports/Timeline.json"}, &started, http.StatusOK)
	return started
}

func sampleBatch() ImportBatch {
	return ImportBatch{
		Places: []ImportPlace{{GooglePlaceID: "place-one", Latitude: 10.001, Longitude: 20.001, Kind: PlaceKindOther, Source: SourceGoogleTimeline}},
		Visits: []ImportVisit{{Source: SourceGoogleTimeline, SourceRef: "1000-2000", StartedAt: 1000, EndedAt: 2000,
			Latitude: float(10.001), Longitude: float(20.001), GooglePlaceID: "place-one", Probability: float(0.9), SourceSemanticType: "UNKNOWN"}},
		Trips: []ImportTrip{{Source: SourceGoogleTimeline, SourceRef: "2000-3000", StartedAt: 2000, EndedAt: 3000,
			StartLatitude: float(10.001), StartLongitude: float(20.001), EndLatitude: float(10.05), EndLongitude: float(20.05),
			Mode: ModeDriving, SourceMode: "IN_PASSENGER_VEHICLE", DistanceMeters: float(7000), Probability: float(0.8),
			Path: []PathPoint{{Latitude: 10.01, Longitude: 20.01}, {Latitude: 10.03, Longitude: 20.03}}}},
		Points: []ImportPoint{{Timestamp: 1500, Latitude: 10.001, Longitude: 20.001, AccuracyMeters: float(12), Source: PointSourceGoogleTimelineRaw},
			{Timestamp: 2500, Latitude: 10.02, Longitude: 20.02, Source: PointSourceGoogleTimelineRaw}},
	}
}

func TestAnImportCreatesThenChangesNothingAndKeepsWhatPeopleWrote(t *testing.T) {
	service := newTestService(t)
	started := service.startImport(ImportKindGoogleTimeline, hashA)
	if started.Import.ID != "import_000001" || started.AlreadyFinished {
		t.Fatalf("start = %+v", started)
	}
	var counts ImportCounts
	service.must(http.MethodPost, "/imports/import_000001/batches", sampleBatch(), &counts, http.StatusOK)
	if counts.PlacesCreated != 1 || counts.VisitsCreated != 1 || counts.TripsCreated != 1 || counts.PointsAdded != 2 {
		t.Fatalf("first batch = %+v", counts)
	}
	var visit Visit
	service.must(http.MethodGet, "/visits/visit_000001", nil, &visit, http.StatusOK)
	if visit.PlaceID == nil || *visit.PlaceID != "place_000001" {
		t.Fatalf("visit = %+v: it should name the place the batch made", visit)
	}

	service.must(http.MethodPatch, "/trips/trip_000001", map[string]any{"title": "To the lake", "description": "Windy", "described_by": "test",
		"tags": []string{"lake"}, "mode": ModeBus}, nil, http.StatusOK)
	service.must(http.MethodPatch, "/places/place_000001", map[string]any{"name": "The lake house"}, nil, http.StatusOK)

	// The same batch again changes nothing.
	service.must(http.MethodPost, "/imports/import_000001/batches", sampleBatch(), &counts, http.StatusOK)
	if counts != (ImportCounts{PointsUnchanged: 2, RecordsUnchanged: 3}) {
		t.Fatalf("second batch = %+v, want nothing changed", counts)
	}

	// The source moves the trip's end: the facts follow, what people wrote stays.
	changed := sampleBatch()
	changed.Trips[0].EndedAt = 3100
	changed.Places[0].Name = "Google's name"
	changed.Places[0].Kind = PlaceKindHome
	service.must(http.MethodPost, "/imports/import_000001/batches", changed, &counts, http.StatusOK)
	if counts.TripsUpdated != 1 || counts.PlacesUpdated != 1 {
		t.Fatalf("third batch = %+v", counts)
	}
	var trip Trip
	service.must(http.MethodGet, "/trips/trip_000001", nil, &trip, http.StatusOK)
	if trip.EndedAt != 3100 || trip.Title != "To the lake" || trip.Description != "Windy" || trip.Mode != ModeBus || len(trip.Tags) != 1 {
		t.Errorf("trip after a re-import = %+v: the end moves; title, description, tags and the corrected mode stay", trip)
	}
	if trip.PathPoints != 2 {
		t.Errorf("trip path points = %d, want 2", trip.PathPoints)
	}
	var place Place
	service.must(http.MethodGet, "/places/place_000001", nil, &place, http.StatusOK)
	if place.Name != "The lake house" || place.Kind != PlaceKindHome {
		t.Errorf("place = %+v: the name someone wrote stays, kind other becomes what the source says", place)
	}
	var revisions []Revision
	service.must(http.MethodGet, "/trips/trip_000001/revisions", nil, &revisions, http.StatusOK)
	if len(revisions) != 2 || revisions[0].Reason != "import" || revisions[1].Reason != "update" {
		t.Errorf("revisions = %+v, want import then update, newest first", revisions)
	}

	var finished Import
	service.must(http.MethodPost, "/imports/import_000001/finish", nil, &finished, http.StatusOK)
	if finished.FinishedAt == 0 || finished.Counts.TripsCreated != 1 || finished.Counts.PointsAdded != 2 {
		t.Errorf("finished import = %+v", finished)
	}
	again := service.startImport(ImportKindGoogleTimeline, hashA)
	if !again.AlreadyFinished || again.Import.ID != "import_000001" {
		t.Errorf("the same file again = %+v, want already finished", again)
	}
	service.must(http.MethodPost, "/imports/import_000001/batches", sampleBatch(), nil, http.StatusConflict)
}

func TestABatchWithOneBadRecordWritesNothing(t *testing.T) {
	service := newTestService(t)
	service.startImport(ImportKindGoogleTimeline, hashA)
	for name, spoil := range map[string]func(*ImportBatch){
		"place with no google id": func(b *ImportBatch) { b.Places[0].GooglePlaceID = "" },
		"manual source":           func(b *ImportBatch) { b.Visits[0].Source = SourceManual },
		"visit ends before start": func(b *ImportBatch) { b.Visits[0].EndedAt = 10 },
		"trip mode unknown word":  func(b *ImportBatch) { b.Trips[0].Mode = "teleport" },
		"half a start":            func(b *ImportBatch) { b.Trips[0].StartLongitude = nil },
		"point off the globe":     func(b *ImportBatch) { b.Points[1].Latitude = 91 },
		"probability above one":   func(b *ImportBatch) { b.Visits[0].Probability = float(80) },
	} {
		batch := sampleBatch()
		spoil(&batch)
		if status, body := service.call(http.MethodPost, "/imports/import_000001/batches", batch, nil); status != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, status, body)
		}
	}
	var health Health
	service.must(http.MethodGet, "/health", nil, &health, http.StatusOK)
	if health.Counts.Places+health.Counts.Visits+health.Counts.Trips+health.Counts.Points != 0 || health.Counts.Imports != 1 {
		t.Errorf("refused batches wrote %+v", health.Counts)
	}
}

func TestAPointWithASourceRefMovesWhenItsSourceDoes(t *testing.T) {
	service := newTestService(t)
	service.startImport(ImportKindPhotoStore, hashA)
	photo := ImportPoint{Timestamp: 5000, Latitude: 10.2, Longitude: 20.2, Source: PointSourcePhoto, SourceRef: "photo_000001"}
	var counts ImportCounts
	service.must(http.MethodPost, "/imports/import_000001/batches", ImportBatch{Points: []ImportPoint{photo}}, &counts, http.StatusOK)
	photo.Latitude = 10.3
	service.must(http.MethodPost, "/imports/import_000001/batches", ImportBatch{Points: []ImportPoint{photo}}, &counts, http.StatusOK)
	var points []Point
	service.must(http.MethodGet, "/points?source=photo", nil, &points, http.StatusOK)
	if len(points) != 1 || points[0].Latitude != 10.3 || points[0].SourceRef != "photo_000001" {
		t.Errorf("points = %+v, want the one photo point, moved", points)
	}
	service.must(http.MethodGet, "/points?source=nowhere", nil, nil, http.StatusBadRequest)
}

func TestPointsTimelineAndTripFilters(t *testing.T) {
	service := newTestService(t)
	service.startImport(ImportKindGoogleTimeline, hashA)
	batch := sampleBatch()
	batch.Trips = append(batch.Trips, ImportTrip{Source: SourceGoogleTimeline, SourceRef: "3000-3600", StartedAt: 3000, EndedAt: 3600,
		Mode: ModeWalking, SourceMode: "WALKING", DistanceMeters: float(800)})
	batch.Visits = append(batch.Visits, ImportVisit{Source: SourceGoogleTimeline, SourceRef: "9000-9900", StartedAt: 9000, EndedAt: 9900,
		Latitude: float(10.3), Longitude: float(20.3)})
	service.must(http.MethodPost, "/imports/import_000001/batches", batch, nil, http.StatusOK)

	var points []Point
	service.must(http.MethodGet, "/points?from=2000&to=3000", nil, &points, http.StatusOK)
	if len(points) != 1 || points[0].Timestamp != 2500 || points[0].ImportID != "import_000001" {
		t.Errorf("points 2000–3000 = %+v", points)
	}

	var timeline []TimelineItem
	service.must(http.MethodGet, "/timeline?from=0&to=5000", nil, &timeline, http.StatusOK)
	var kinds []string
	for _, item := range timeline {
		kinds = append(kinds, item.Kind)
	}
	if strings.Join(kinds, ",") != "visit,trip,trip" {
		t.Errorf("timeline kinds = %v, want visit,trip,trip in time order", kinds)
	}

	var trips []Trip
	service.must(http.MethodGet, "/trips?min_distance_meters=1000", nil, &trips, http.StatusOK)
	if len(trips) != 1 || trips[0].ID != "trip_000001" {
		t.Errorf("trips of at least 1 km = %+v", trips)
	}
	service.must(http.MethodGet, "/trips?mode=walking", nil, &trips, http.StatusOK)
	if len(trips) != 1 || trips[0].ID != "trip_000002" {
		t.Errorf("walking trips = %+v", trips)
	}
	service.must(http.MethodGet, "/trips?mode=teleport", nil, nil, http.StatusBadRequest)

	// A road trip made by hand, with both drives attached.
	var parent Trip
	service.must(http.MethodPost, "/trips", TripCreate{Title: "Lake weekend", StartedAt: 1000, EndedAt: 4000, Mode: ModeMixed}, &parent, http.StatusCreated)
	if parent.ID != "trip_000003" || parent.Source != SourceManual {
		t.Fatalf("parent = %+v", parent)
	}
	service.must(http.MethodPost, "/trips/trip_000003/children", AttachChildren{TripIDs: []string{"trip_000001", "trip_000002"}}, &parent, http.StatusOK)
	if parent.ChildTrips != 2 {
		t.Errorf("parent child_trips = %d, want 2", parent.ChildTrips)
	}
	service.must(http.MethodGet, "/trips?parent_trip_id=trip_000003", nil, &trips, http.StatusOK)
	if len(trips) != 2 {
		t.Errorf("children of trip_000003 = %d, want 2", len(trips))
	}
	service.must(http.MethodGet, "/trips?parent_trip_id=none", nil, &trips, http.StatusOK)
	if len(trips) != 1 || trips[0].ID != "trip_000003" {
		t.Errorf("top-level trips = %+v", trips)
	}
	service.must(http.MethodGet, "/timeline?to=5000", nil, &timeline, http.StatusOK)
	if len(timeline) != 3 {
		t.Errorf("timeline = %d items, want the parent left out", len(timeline))
	}
	service.must(http.MethodGet, "/timeline?to=5000&include_parent_trips=true", nil, &timeline, http.StatusOK)
	if len(timeline) != 4 {
		t.Errorf("timeline with parents = %d items, want 4", len(timeline))
	}
	// A loop is refused, and null detaches.
	service.must(http.MethodPatch, "/trips/trip_000003", map[string]any{"parent_trip_id": "trip_000001"}, nil, http.StatusBadRequest)
	var child Trip
	service.must(http.MethodPatch, "/trips/trip_000002", map[string]any{"parent_trip_id": nil}, &child, http.StatusOK)
	if child.ParentTripID != nil {
		t.Errorf("detached child = %+v", child)
	}
}

func TestHandMadeRecordsPatchDeleteAndRestore(t *testing.T) {
	service := newTestService(t)
	var place Place
	service.must(http.MethodPost, "/places", PlaceCreate{Name: "Corner bakery", Latitude: float(10.5), Longitude: float(20.5), Kind: PlaceKindVenue,
		RadiusMeters: 30, Tags: []string{"food"}}, &place, http.StatusCreated)
	if place.ID != "place_000001" || place.Source != SourceManual || place.ImportID != nil {
		t.Fatalf("place = %+v", place)
	}
	service.must(http.MethodPost, "/places", PlaceCreate{Name: "Nowhere", Latitude: float(10.5), Longitude: float(20.5), Kind: "castle"}, nil, http.StatusBadRequest)
	service.must(http.MethodPost, "/places", map[string]any{"name": "x", "latitude": 1, "longitude": 1, "kind": "venue", "colour": "red"}, nil, http.StatusBadRequest)

	var visit Visit
	service.must(http.MethodPost, "/visits", VisitCreate{PlaceID: &place.ID, StartedAt: 100, EndedAt: 200}, &visit, http.StatusCreated)
	if visit.PlaceName != "Corner bakery" {
		t.Errorf("visit place_name = %q", visit.PlaceName)
	}
	missing := "place_999999"
	service.must(http.MethodPost, "/visits", VisitCreate{PlaceID: &missing, StartedAt: 100, EndedAt: 200}, nil, http.StatusBadRequest)

	service.must(http.MethodPatch, "/places/place_000001", map[string]any{"description": "Good bread"}, nil, http.StatusBadRequest)
	service.must(http.MethodPatch, "/places/place_000001", map[string]any{"nmae": "typo"}, nil, http.StatusBadRequest)
	service.must(http.MethodPatch, "/places/place_000001", map[string]any{"name": nil}, nil, http.StatusBadRequest)
	service.must(http.MethodPatch, "/visits/visit_000001", map[string]any{"place_id": nil, "tags": []string{"morning"}}, &visit, http.StatusOK)
	if visit.PlaceID != nil || visit.Tags[0] != "morning" {
		t.Errorf("visit after patch = %+v", visit)
	}

	service.must(http.MethodDelete, "/places/place_000001", nil, nil, http.StatusNoContent)
	service.must(http.MethodGet, "/places/place_000001", nil, nil, http.StatusNotFound)
	service.must(http.MethodGet, "/places/place_000001?include_deleted=true", nil, &place, http.StatusOK)
	if place.DeletedAt == 0 {
		t.Error("a deleted place says deleted_at 0")
	}
	var places []Place
	service.must(http.MethodGet, "/places?q=bakery", nil, &places, http.StatusOK)
	if len(places) != 0 {
		t.Errorf("a deleted place is listed: %+v", places)
	}
	service.must(http.MethodPost, "/places/place_000001/restore", nil, &place, http.StatusOK)
	service.must(http.MethodGet, "/places?q=bakery&kind=venue&tag=food", nil, &places, http.StatusOK)
	if len(places) != 1 {
		t.Errorf("places after restore = %+v", places)
	}
	var revisions []Revision
	service.must(http.MethodGet, "/places/place_000001/revisions", nil, &revisions, http.StatusOK)
	if len(revisions) != 2 || revisions[0].Reason != "restore" || revisions[1].Reason != "delete" {
		t.Errorf("revisions = %+v", revisions)
	}

	var vocabularies Vocabularies
	service.must(http.MethodGet, "/vocabularies", nil, &vocabularies, http.StatusOK)
	if len(vocabularies.TripModes) != len(TripModes) || len(vocabularies.PlaceKinds) != len(PlaceKinds) {
		t.Errorf("vocabularies = %+v", vocabularies)
	}
}

func TestGoogleWordsMapToModesAndKinds(t *testing.T) {
	for word, want := range map[string]string{"IN_PASSENGER_VEHICLE": ModeDriving, "walking": ModeWalking, "FLYING": ModeFlying,
		"IN_SUBWAY": ModeSubway, "UNKNOWN_ACTIVITY_TYPE": ModeUnknown, "": ModeUnknown} {
		if got := ModeForGoogleActivityType(word); got != want {
			t.Errorf("mode of %q = %q, want %q", word, got, want)
		}
	}
	for _, check := range []struct {
		word  string
		named bool
		want  string
	}{{"HOME", false, PlaceKindHome}, {"TYPE_WORK", false, PlaceKindWork}, {"INFERRED_HOME", false, PlaceKindHome},
		{"TYPE_ALIASED_LOCATION", false, PlaceKindSaved}, {"UNKNOWN", true, PlaceKindVenue}, {"UNKNOWN", false, PlaceKindOther}} {
		if got := PlaceKindForGoogleSemanticType(check.word, check.named); got != check.want {
			t.Errorf("kind of %q named=%v = %q, want %q", check.word, check.named, got, check.want)
		}
	}
}
