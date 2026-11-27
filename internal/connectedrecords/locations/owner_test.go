package locations

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// aliceRecords is what Alice keeps in a fresh service: an import with a place,
// a visit, a movement, two points and a finished snapshot; a journey made by
// hand with the movement attached; a place and a visit by hand; and a hand
// link from a calendar event to the visit.
type aliceRecords struct {
	importID, placeID, visitID, movementID, journeyID, handPlaceID, linkID string
}

func makeAliceRecords(t *testing.T, service *testService) aliceRecords {
	t.Helper()
	started := service.startImport(ImportKindGoogleTimeline, hashA)
	service.must(http.MethodPost, "/imports/"+started.Import.ID+"/batches", sampleBatch(), nil, http.StatusOK)
	var journey, movement Trip
	service.must(http.MethodPost, "/trips", TripCreate{Title: "Lake week", StartedAt: 500, EndedAt: 9000, Mode: ModeDriving}, &journey, http.StatusCreated)
	service.must(http.MethodPost, "/trips/"+journey.ID+"/children", AttachChildren{TripIDs: []string{"trip_000001"}}, &movement, http.StatusOK)
	var place Place
	service.must(http.MethodPost, "/places", PlaceCreate{Name: "Example Hall", Latitude: float(10.06), Longitude: float(20.06), Kind: PlaceKindVenue}, &place, http.StatusCreated)
	var link CalendarEventLink
	service.must(http.MethodPost, "/calendar-event-links", CalendarEventLinkCreate{CalendarID: "calendar-a", EventID: "event-a", StartedAt: 1000, EndedAt: 2000,
		TargetType: EntityVisit, TargetID: "visit_000001"}, &link, http.StatusCreated)
	if place.OwnerPrincipalID != alicePrincipalID || journey.OwnerPrincipalID != alicePrincipalID || link.OwnerPrincipalID != alicePrincipalID ||
		started.Import.OwnerPrincipalID != alicePrincipalID {
		t.Fatalf("owners: place=%s journey=%s link=%s import=%s, want %s on each", place.OwnerPrincipalID, journey.OwnerPrincipalID,
			link.OwnerPrincipalID, started.Import.OwnerPrincipalID, alicePrincipalID)
	}
	return aliceRecords{importID: started.Import.ID, placeID: "place_000001", visitID: "visit_000001", movementID: "trip_000001",
		journeyID: journey.ID, handPlaceID: place.ID, linkID: link.ID}
}

func TestAnotherOwnersRecordsAreNotFoundForReadOrWrite(t *testing.T) {
	service := newTestService(t)
	records := makeAliceRecords(t, service)
	asBob := service.as(bob)

	for _, request := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/places/" + records.placeID, nil},
		{http.MethodPatch, "/places/" + records.placeID, map[string]any{"name": "Bob was here"}},
		{http.MethodDelete, "/places/" + records.placeID, nil},
		{http.MethodPost, "/places/" + records.placeID + "/restore", nil},
		{http.MethodGet, "/places/" + records.placeID + "/revisions", nil},
		{http.MethodPost, "/places/" + records.handPlaceID + "/merge", PlaceMergeRequest{Into: records.placeID}},
		{http.MethodGet, "/visits/" + records.visitID, nil},
		{http.MethodPatch, "/visits/" + records.visitID, map[string]any{"description": "Bob was here", "described_by": "bob"}},
		{http.MethodDelete, "/visits/" + records.visitID, nil},
		{http.MethodGet, "/trips/" + records.movementID, nil},
		{http.MethodGet, "/trips/" + records.movementID + "/path", nil},
		{http.MethodPatch, "/trips/" + records.movementID, map[string]any{"title": "Bob was here"}},
		{http.MethodPost, "/trips/" + records.journeyID + "/children", AttachChildren{TripIDs: []string{records.movementID}}},
		{http.MethodGet, "/journeys/" + records.journeyID, nil},
		{http.MethodGet, "/imports/" + records.importID, nil},
		{http.MethodPost, "/imports/" + records.importID + "/batches", sampleBatch()},
		{http.MethodPost, "/imports/" + records.importID + "/finish", nil},
		{http.MethodGet, "/calendar-event-links/" + records.linkID, nil},
		{http.MethodDelete, "/calendar-event-links/" + records.linkID, nil},
		{http.MethodGet, "/places/not_an_id", nil},
	} {
		if status, _ := asBob.call(request.method, request.path, request.body, nil); status != http.StatusNotFound {
			t.Errorf("Bob: %s %s = %d, want 404, the answer for a missing record", request.method, request.path, status)
		}
	}

	var place Place
	service.must(http.MethodGet, "/places/"+records.placeID, nil, &place, http.StatusOK)
	var visit Visit
	service.must(http.MethodGet, "/visits/"+records.visitID, nil, &visit, http.StatusOK)
	var link CalendarEventLink
	service.must(http.MethodGet, "/calendar-event-links/"+records.linkID, nil, &link, http.StatusOK)
	var record Import
	service.must(http.MethodGet, "/imports/"+records.importID, nil, &record, http.StatusOK)
	if place.Name != "" || place.DeletedAt != 0 || visit.Description != "" || visit.DeletedAt != 0 || link.DeletedAt != 0 || record.FinishedAt != 0 {
		t.Errorf("Bob's refused writes changed Alice's records")
	}
	var revisions []Revision
	service.must(http.MethodGet, "/places/"+records.placeID+"/revisions", nil, &revisions, http.StatusOK)
	if len(revisions) != 0 {
		t.Errorf("Bob's refused writes left %d revisions", len(revisions))
	}
}

func TestAListHoldsOnlyTheCallersOwnRecords(t *testing.T) {
	service := newTestService(t)
	makeAliceRecords(t, service)
	asBob := service.as(bob)

	for _, path := range []string{"/places", "/places?q=hall", "/visits", "/trips", "/journeys", "/timeline", "/points", "/imports", "/home-periods",
		"/calendar-event-links", "/calendar-series-places", "/place-suggestions", "/unnamed-places"} {
		var found []map[string]any
		if status, answer := asBob.call(http.MethodGet, path, nil, &found); status != http.StatusOK || len(found) != 0 {
			t.Errorf("Bob: GET %s = %d with %d records, want none of Alice's: %.200s", path, status, len(found), answer)
		}
	}
	var bobsHealth, alicesHealth Health
	asBob.must(http.MethodGet, "/health", nil, &bobsHealth, http.StatusOK)
	if bobsHealth.Counts != (Counts{}) {
		t.Errorf("Bob's health counts %+v, want none of Alice's records", bobsHealth.Counts)
	}
	service.must(http.MethodGet, "/health", nil, &alicesHealth, http.StatusOK)
	if alicesHealth.Counts.Places != 2 || alicesHealth.Counts.Trips != 2 || alicesHealth.Counts.Points != 2 || alicesHealth.Counts.Imports != 1 {
		t.Errorf("Alice's health counts %+v", alicesHealth.Counts)
	}
	if status, _ := asBob.call(http.MethodGet, "/places?owner_principal_id="+alicePrincipalID, nil, nil); status != http.StatusForbidden {
		t.Errorf("Bob naming Alice as the owner = %d, want 403", status)
	}
	if status, _ := asBob.call(http.MethodGet, "/places?owner_principal_id="+bobPrincipalID, nil, nil); status != http.StatusOK {
		t.Errorf("Bob naming himself as the owner = %d, want 200", status)
	}
}

func TestAnAdministratorSeesEveryOwnerAndNarrowsToOne(t *testing.T) {
	service := newTestService(t)
	records := makeAliceRecords(t, service)
	asBob := service.as(bob)
	var bobsPlace Place
	asBob.must(http.MethodPost, "/places", PlaceCreate{Name: "Bob's shed", Latitude: float(10.3), Longitude: float(20.3), Kind: PlaceKindOther}, &bobsPlace, http.StatusCreated)
	if bobsPlace.OwnerPrincipalID != bobPrincipalID {
		t.Fatalf("Bob's place is owned by %s", bobsPlace.OwnerPrincipalID)
	}

	administrator := service.as(aliceAdministrator)
	var every, bobs []Place
	administrator.must(http.MethodGet, "/places", nil, &every, http.StatusOK)
	if len(every) != 3 {
		t.Errorf("the administrator lists %d places, want both owners' 3", len(every))
	}
	administrator.must(http.MethodGet, "/places?owner_principal_id="+bobPrincipalID, nil, &bobs, http.StatusOK)
	if len(bobs) != 1 || bobs[0].ID != bobsPlace.ID {
		t.Errorf("narrowed to Bob, the administrator lists %+v", bobs)
	}
	administrator.must(http.MethodGet, "/places/"+bobsPlace.ID, nil, nil, http.StatusOK)
	var health Health
	administrator.must(http.MethodGet, "/health", nil, &health, http.StatusOK)
	if health.Counts.Places != 3 {
		t.Errorf("the administrator's health counts %d places, want 3", health.Counts.Places)
	}

	// Records of two owners are never joined, even by someone who sees both.
	for _, request := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/places/" + bobsPlace.ID + "/merge", PlaceMergeRequest{Into: records.placeID}},
		{http.MethodPatch, "/visits/" + records.visitID, map[string]any{"place_id": bobsPlace.ID}},
		{http.MethodPost, "/visits", VisitCreate{PlaceID: &bobsPlace.ID, StartedAt: 100, EndedAt: 200}},
	} {
		if status, answer := administrator.call(request.method, request.path, request.body, nil); status != http.StatusBadRequest {
			t.Errorf("the administrator: %s %s across owners = %d %s, want 400", request.method, request.path, status, answer)
		}
	}
	// Bob cannot reach Alice's place at all, so for him it is missing: a
	// visit at it is the 400 a missing place always was.
	if status, _ := asBob.call(http.MethodPost, "/visits", VisitCreate{PlaceID: &records.placeID, StartedAt: 100, EndedAt: 200}, nil); status != http.StatusBadRequest {
		t.Errorf("Bob: a visit at Alice's place = %d, want 400", status)
	}
	var bobsTrip Trip
	asBob.must(http.MethodPost, "/trips", TripCreate{Title: "Bob's walk", StartedAt: 100, EndedAt: 200, Mode: ModeWalking, Kind: TripKindMovement}, &bobsTrip, http.StatusCreated)
	if status, _ := asBob.call(http.MethodPatch, "/trips/"+bobsTrip.ID, map[string]any{"parent_trip_id": records.journeyID}, nil); status != http.StatusBadRequest {
		t.Errorf("Bob: his trip under Alice's journey = %d, want 400", status)
	}
	if status, _ := administrator.call(http.MethodPost, "/trips/"+records.journeyID+"/children", AttachChildren{TripIDs: []string{bobsTrip.ID}}, nil); status != http.StatusBadRequest {
		t.Errorf("the administrator: Bob's trip under Alice's journey = %d, want 400", status)
	}
	if status, _ := service.call(http.MethodPost, "/calendar-event-links", CalendarEventLinkCreate{CalendarID: "c", EventID: "e", StartedAt: 1, EndedAt: 2,
		TargetType: EntityPlace, TargetID: bobsPlace.ID}, nil); status != http.StatusBadRequest {
		t.Errorf("Alice: a link to Bob's place = %d, want 400", status)
	}
}

func TestTwoOwnersMayHoldTheSameFileKeysAndFixes(t *testing.T) {
	service := newTestService(t)
	asBob := service.as(bob)
	alices := service.startImport(ImportKindGoogleTimeline, hashA)
	bobs := asBob.startImport(ImportKindGoogleTimeline, hashA)
	if alices.Import.ID == bobs.Import.ID || bobs.AlreadyFinished || bobs.Import.OwnerPrincipalID != bobPrincipalID {
		t.Fatalf("the same file for two owners = %+v and %+v, want an import each", alices.Import, bobs.Import)
	}
	var aliceCounts, bobCounts ImportCounts
	service.must(http.MethodPost, "/imports/"+alices.Import.ID+"/batches", sampleBatch(), &aliceCounts, http.StatusOK)
	asBob.must(http.MethodPost, "/imports/"+bobs.Import.ID+"/batches", sampleBatch(), &bobCounts, http.StatusOK)
	if bobCounts != aliceCounts || bobCounts.PlacesCreated != 1 || bobCounts.PointsAdded != 2 {
		t.Errorf("Bob's copy of the batch = %+v, want what Alice's did, %+v: one owner's records never stand for another's", bobCounts, aliceCounts)
	}
	var places []Place
	asBob.must(http.MethodGet, "/places?google_place_id=place-one", nil, &places, http.StatusOK)
	if len(places) != 1 || places[0].OwnerPrincipalID != bobPrincipalID {
		t.Errorf("Bob's place-one = %+v", places)
	}

	// Each owner's phone has its own day import, and a pass reads one owner's fixes.
	live := LivePoints{Device: "example-phone", RecordedBy: "test", Points: phoneDay()}
	var aliceLive, bobLive LivePointsRecorded
	service.must(http.MethodPost, "/points/live", live, &aliceLive, http.StatusOK)
	asBob.must(http.MethodPost, "/points/live", LivePoints{Device: "example-phone", RecordedBy: "test", Points: phoneDay()[:3]}, &bobLive, http.StatusOK)
	if aliceLive.ImportIDs[0] == bobLive.ImportIDs[0] || bobLive.PointsAdded != 3 {
		t.Errorf("live points: Alice %+v, Bob %+v, want a day import each", aliceLive, bobLive)
	}
	var bobsRun PhoneSegmentation
	asBob.must(http.MethodPost, "/phone-points/segment", PhoneSegmentRequest{From: minutes(-60), To: minutes(24 * 60)}, &bobsRun, http.StatusOK)
	// Bob's three fixes are twenty minutes at home: one stay, his only visit.
	if bobsRun.PointsRead != 3 || len(bobsRun.ActionCounts) != 1 || bobsRun.ActionCounts["create"] != 1 {
		t.Errorf("Bob's segmenting read %d points and did %+v, want his own 3 made into one visit", bobsRun.PointsRead, bobsRun.ActionCounts)
	}
	administrator := service.as(aliceAdministrator)
	var forAlice PhoneSegmentation
	administrator.must(http.MethodPost, "/phone-points/segment?owner_principal_id="+alicePrincipalID, PhoneSegmentRequest{From: minutes(-60), To: minutes(24 * 60)}, &forAlice, http.StatusOK)
	if forAlice.PointsRead != len(live.Points) || forAlice.ActionCounts["create"] != 3 {
		t.Errorf("the administrator's run for Alice = %d points, %+v", forAlice.PointsRead, forAlice.ActionCounts)
	}
	var bobsVisits []Visit
	asBob.must(http.MethodGet, "/visits?source=phone_points", nil, &bobsVisits, http.StatusOK)
	if len(bobsVisits) != 1 || bobsVisits[0].OwnerPrincipalID != bobPrincipalID {
		t.Errorf("after the run for Alice Bob has %d visits, want only his own one", len(bobsVisits))
	}
	if status, _ := service.as(ownership.Caller{SeesEveryOwner: true}).call(http.MethodPost, "/phone-points/segment",
		PhoneSegmentRequest{From: minutes(-60), To: minutes(24 * 60)}, nil); status != http.StatusForbidden {
		t.Errorf("a pass by a caller who acts for nobody = %d, want 403", status)
	}
}

// examplePaths is one call of every route, in RegisterHandlers' order.
var examplePaths = []struct{ method, path string }{
	{"GET", "/health"}, {"GET", "/vocabularies"},
	{"GET", "/places"}, {"POST", "/places"}, {"GET", "/places/place_000001"}, {"PATCH", "/places/place_000001"}, {"POST", "/places/place_000001/merge"},
	{"GET", "/visits"}, {"POST", "/visits"}, {"GET", "/visits/visit_000001"}, {"PATCH", "/visits/visit_000001"},
	{"GET", "/trips"}, {"POST", "/trips"}, {"GET", "/trips/trip_000001"}, {"PATCH", "/trips/trip_000001"}, {"GET", "/trips/trip_000001/path"},
	{"POST", "/trips/trip_000001/children"},
	{"DELETE", "/places/place_000001"}, {"POST", "/places/place_000001/restore"}, {"GET", "/places/place_000001/revisions"},
	{"DELETE", "/visits/visit_000001"}, {"POST", "/visits/visit_000001/restore"}, {"GET", "/visits/visit_000001/revisions"},
	{"DELETE", "/trips/trip_000001"}, {"POST", "/trips/trip_000001/restore"}, {"GET", "/trips/trip_000001/revisions"},
	{"POST", "/localities/fill"}, {"GET", "/home-periods"}, {"GET", "/home-periods/home_period_000001"}, {"POST", "/journeys/group"},
	{"GET", "/journeys"}, {"GET", "/journeys/trip_000001"},
	{"POST", "/calendar/match"}, {"GET", "/calendar-event-links"}, {"POST", "/calendar-event-links"},
	{"GET", "/calendar-event-links/calendar_event_link_000001"}, {"DELETE", "/calendar-event-links/calendar_event_link_000001"},
	{"GET", "/calendar-series-places"}, {"GET", "/calendar-series-places/calendar_series_place_000001"},
	{"GET", "/unnamed-places"}, {"POST", "/place-suggestions/proposals"}, {"GET", "/place-suggestions"},
	{"GET", "/place-suggestions/place_suggestion_000001"}, {"POST", "/place-suggestions/place_suggestion_000001/accept"},
	{"POST", "/place-suggestions/place_suggestion_000001/reject"},
	{"GET", "/timeline"}, {"GET", "/points"}, {"POST", "/points/live"}, {"POST", "/phone-points/segment"},
	{"GET", "/imports"}, {"POST", "/imports"}, {"GET", "/imports/import_000001"}, {"POST", "/imports/import_000001/batches"},
	{"POST", "/imports/import_000001/finish"},
}

func TestEveryRouteRefusesARequestWithNoCaller(t *testing.T) {
	service := newTestService(t)
	makeAliceRecords(t, service)
	nobody := service.withoutCaller()
	for _, route := range examplePaths {
		var answer map[string]string
		if status, _ := nobody.call(route.method, route.path, "{}", &answer); status != http.StatusUnauthorized || answer["error"] == "" {
			t.Errorf("%s %s with no caller = %d %v, want 401 with an error", route.method, route.path, status, answer)
		}
	}
	// The vocabularies answer any caller who is someone.
	service.as(bob).must(http.MethodGet, "/vocabularies", nil, nil, http.StatusOK)
}

func TestAWriteThatCreatesNeedsACallerWhoIsSomeone(t *testing.T) {
	service := newTestService(t)
	nobody := service.as(ownership.Caller{SeesEveryOwner: true})
	for _, request := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/places", PlaceCreate{Name: "x", Latitude: float(10), Longitude: float(20), Kind: PlaceKindOther}},
		{http.MethodPost, "/imports", ImportStart{Kind: ImportKindGoogleTimeline, Sha256: hashA}},
		{http.MethodPost, "/points/live", LivePoints{Device: "example-phone", RecordedBy: "test", Points: phoneDay()}},
	} {
		if status, _ := nobody.call(request.method, request.path, request.body, nil); status != http.StatusForbidden {
			t.Errorf("%s %s acting as nobody = %d, want 403", request.method, request.path, status)
		}
	}
	// Reading as nobody who sees every owner reads every owner's.
	nobody.must(http.MethodGet, "/places", nil, nil, http.StatusOK)
}

func TestTheStoreRefusesTheZeroScope(t *testing.T) {
	service := newTestService(t)
	if _, err := service.store.ListPlaces(context.Background(), ownership.Scope{}, PlaceFilter{}); err == nil {
		t.Error("ListPlaces with the zero scope answered; it reaches nothing and must be refused")
	}
}

// The schema's record_id, which holds each stored id to its number, writes
// what postgresstorage.FormatRecordID writes, past six digits too.
func TestTheSchemasRecordIDIsFormatRecordID(t *testing.T) {
	service := newTestService(t)
	for _, number := range []int64{1, 42, 999999, 1000000, 12345678} {
		var written string
		if err := service.store.db.QueryRowContext(context.Background(), `SELECT record_id('place', $1)`, number).Scan(&written); err != nil {
			t.Fatal(err)
		}
		if want := postgresstorage.FormatRecordID("place", number); written != want {
			t.Errorf("record_id('place', %d) = %q, want %q", number, written, want)
		}
	}
}

// q matches text anywhere, ignores the case of ASCII letters only, as
// SQLite's LIKE did, and reads % and _ as themselves.
func TestPlaceSearchKeepsTheLikeItReplaced(t *testing.T) {
	service := newTestService(t)
	for _, name := range []string{"Ärzte Lake House", "100% Juice_Bar"} {
		service.must(http.MethodPost, "/places", PlaceCreate{Name: name, Latitude: float(10.1), Longitude: float(20.1), Kind: PlaceKindVenue,
			Tags: []string{"Example-Tag"}}, nil, http.StatusCreated)
	}
	for query, want := range map[string]int{
		"lake": 1, "LAKE": 1, "ärzte": 0, "Ärzte": 1, "lake house": 1, "lake juice": 0, "%": 1, "e_b": 1, "e_h": 0, "example-tag": 2,
	} {
		var places []Place
		service.must(http.MethodGet, "/places?q="+strings.NewReplacer("%", "%25", " ", "+").Replace(query), nil, &places, http.StatusOK)
		if len(places) != want {
			t.Errorf("q=%q found %d places, want %d", query, len(places), want)
		}
	}
}
