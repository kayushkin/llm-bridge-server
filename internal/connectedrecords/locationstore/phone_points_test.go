package locationstore

import (
	"net/http"
	"testing"
)

// Every coordinate here is invented: a grid near 10°N 20°E.

const phoneBase = int64(1767225600) // 2026-01-01 00:00 UTC

func minutes(m int64) int64 { return phoneBase + m*60 }

// phoneDay is an hour at home (a fix every 10 minutes), a 20-minute drive of
// about 11 km north with three fixes on the way, and an hour at a cafe.
func phoneDay() []LivePoint {
	points := []LivePoint{}
	for m := int64(0); m <= 60; m += 10 {
		points = append(points, LivePoint{Timestamp: minutes(m), Latitude: 10.0001, Longitude: 20.0, AccuracyMeters: float(12)})
	}
	for step, latitude := range []float64{10.025, 10.05, 10.075} {
		points = append(points, LivePoint{Timestamp: minutes(65 + int64(step)*5), Latitude: latitude, Longitude: 20.0, AccuracyMeters: float(20)})
	}
	for m := int64(80); m <= 140; m += 10 {
		points = append(points, LivePoint{Timestamp: minutes(m), Latitude: 10.1, Longitude: 20.0005, AccuracyMeters: float(15)})
	}
	return points
}

func (s *testService) segment(dryRun bool) PhoneSegmentation {
	s.t.Helper()
	var segmentation PhoneSegmentation
	s.must(http.MethodPost, "/phone-points/segment", PhoneSegmentRequest{DryRun: dryRun, From: minutes(-60), To: minutes(24 * 60)}, &segmentation, http.StatusOK)
	return segmentation
}

func TestLivePointsAreStoredOncePerFixAndSignedBySender(t *testing.T) {
	s := newTestService(t)
	live := LivePoints{Device: "example-phone", RecordedBy: "operator on dash (principal_000001)", Points: phoneDay()}
	var first, again LivePointsRecorded
	s.must(http.MethodPost, "/points/live", live, &first, http.StatusOK)
	if first.PointsAdded != len(live.Points) || first.PointsUnchanged != 0 || len(first.ImportIDs) != 1 {
		t.Fatalf("first send = %+v, want %d added in one import", first, len(live.Points))
	}
	s.must(http.MethodPost, "/points/live", live, &again, http.StatusOK)
	if again.PointsAdded != 0 || again.PointsUnchanged != len(live.Points) || again.ImportIDs[0] != first.ImportIDs[0] {
		t.Fatalf("second send = %+v, want every point unchanged in %s", again, first.ImportIDs[0])
	}
	var record Import
	s.must(http.MethodGet, "/imports/"+first.ImportIDs[0], nil, &record, http.StatusOK)
	if record.Kind != ImportKindPhoneLive || record.SourceMachine != "example-phone" || record.RecordedBy != live.RecordedBy ||
		record.Counts.PointsAdded != len(live.Points) || record.FinishedAt == 0 {
		t.Fatalf("import = %+v", record)
	}
	var points []Point
	s.must(http.MethodGet, "/points?source=phone&limit=100", nil, &points, http.StatusOK)
	if len(points) != len(live.Points) {
		t.Fatalf("%d phone points stored, want %d", len(points), len(live.Points))
	}
}

func TestLivePointsRefuseWhatNoPhoneCouldSend(t *testing.T) {
	s := newTestService(t)
	good := LivePoint{Timestamp: minutes(0), Latitude: 10, Longitude: 20}
	for name, live := range map[string]LivePoints{
		"no device":       {RecordedBy: "someone", Points: []LivePoint{good}},
		"bad device":      {Device: "a phone", RecordedBy: "someone", Points: []LivePoint{good}},
		"no sender":       {Device: "example-phone", Points: []LivePoint{good}},
		"no points":       {Device: "example-phone", RecordedBy: "someone"},
		"bad latitude":    {Device: "example-phone", RecordedBy: "someone", Points: []LivePoint{{Timestamp: minutes(0), Latitude: 91, Longitude: 20}}},
		"from the future": {Device: "example-phone", RecordedBy: "someone", Points: []LivePoint{{Timestamp: now() + 3600, Latitude: 10, Longitude: 20}}},
		"negative error":  {Device: "example-phone", RecordedBy: "someone", Points: []LivePoint{{Timestamp: minutes(0), Latitude: 10, Longitude: 20, AccuracyMeters: float(-1)}}},
	} {
		if status, answer := s.call(http.MethodPost, "/points/live", live, nil); status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, status, answer)
		}
	}
}

func TestSegmentingMakesTwoVisitsAndTheDriveBetween(t *testing.T) {
	s := newTestService(t)
	s.must(http.MethodPost, "/points/live", LivePoints{Device: "example-phone", RecordedBy: "someone", Points: phoneDay()}, nil, http.StatusOK)

	dry := s.segment(true)
	if dry.ActionCounts["create"] != 3 {
		t.Fatalf("dry run = %+v, want three creates", dry.ActionCounts)
	}
	var visits []Visit
	s.must(http.MethodGet, "/visits", nil, &visits, http.StatusOK)
	if len(visits) != 0 {
		t.Fatalf("a dry run wrote %d visits", len(visits))
	}

	run := s.segment(false)
	if run.ActionCounts["create"] != 3 || len(run.PlacesCreated) != 2 {
		t.Fatalf("run = %+v, places %v; want 3 creates and 2 places", run.ActionCounts, run.PlacesCreated)
	}
	s.must(http.MethodGet, "/visits", nil, &visits, http.StatusOK)
	if len(visits) != 2 || visits[0].StartedAt != minutes(0) || visits[0].EndedAt != minutes(60) ||
		visits[1].StartedAt != minutes(80) || visits[1].EndedAt != minutes(140) || visits[0].Source != SourcePhonePoints {
		t.Fatalf("visits = %+v", visits)
	}
	var trips []Trip
	s.must(http.MethodGet, "/trips?kind=movement", nil, &trips, http.StatusOK)
	if len(trips) != 1 || trips[0].StartedAt != minutes(60) || trips[0].EndedAt != minutes(80) || trips[0].Mode != ModeDriving ||
		trips[0].PathPoints != 5 || trips[0].StartPlaceID == nil || *trips[0].StartPlaceID != *visits[0].PlaceID {
		t.Fatalf("trips = %+v", trips)
	}

	again := s.segment(false)
	if again.ActionCounts["unchanged"] != 3 || len(again.ActionCounts) != 1 || len(again.PlacesCreated) != 0 {
		t.Fatalf("a second run = %+v, places %v; want 3 unchanged and nothing else", again.ActionCounts, again.PlacesCreated)
	}
}

func TestSegmentingExtendsAStayAsPointsArrive(t *testing.T) {
	s := newTestService(t)
	day := phoneDay()
	s.must(http.MethodPost, "/points/live", LivePoints{Device: "example-phone", RecordedBy: "someone", Points: day[:len(day)-3]}, nil, http.StatusOK)
	s.segment(false)
	s.must(http.MethodPost, "/points/live", LivePoints{Device: "example-phone", RecordedBy: "someone", Points: day[len(day)-3:]}, nil, http.StatusOK)
	run := s.segment(false)
	if run.ActionCounts["update"] != 1 || run.ActionCounts["create"] != 0 || run.ActionCounts["delete"] != 0 {
		t.Fatalf("run = %+v, want the cafe visit updated", run.ActionCounts)
	}
	var visits []Visit
	s.must(http.MethodGet, "/visits", nil, &visits, http.StatusOK)
	if len(visits) != 2 || visits[1].EndedAt != minutes(140) {
		t.Fatalf("visits = %+v", visits)
	}
}

func TestSegmentingLeavesAVisitSomeoneEditedOrDeleted(t *testing.T) {
	s := newTestService(t)
	s.must(http.MethodPost, "/points/live", LivePoints{Device: "example-phone", RecordedBy: "someone", Points: phoneDay()}, nil, http.StatusOK)
	s.segment(false)
	var visits []Visit
	s.must(http.MethodGet, "/visits", nil, &visits, http.StatusOK)
	s.must(http.MethodPatch, "/visits/"+visits[0].ID, map[string]any{"description": "breakfast", "described_by": "hand"}, nil, http.StatusOK)
	s.must(http.MethodDelete, "/visits/"+visits[1].ID, nil, nil, http.StatusNoContent)

	run := s.segment(false)
	if run.ActionCounts["kept_edited"] != 1 || run.ActionCounts["kept_deleted"] != 1 || run.ActionCounts["create"] != 0 {
		t.Fatalf("run = %+v", run.ActionCounts)
	}
	var live []Visit
	s.must(http.MethodGet, "/visits", nil, &live, http.StatusOK)
	if len(live) != 1 || live[0].ID != visits[0].ID || live[0].Description != "breakfast" {
		t.Fatalf("live visits = %+v", live)
	}
}

func TestSegmentingSkipsWhatAnotherSourceCovers(t *testing.T) {
	s := newTestService(t)
	s.must(http.MethodPost, "/visits", map[string]any{"latitude": 10.0001, "longitude": 20.0, "started_at": minutes(-30), "ended_at": minutes(30)}, nil, http.StatusCreated)
	s.must(http.MethodPost, "/points/live", LivePoints{Device: "example-phone", RecordedBy: "someone", Points: phoneDay()}, nil, http.StatusOK)
	run := s.segment(false)
	if run.ActionCounts["skip_other_source"] != 1 || run.ActionCounts["create"] != 2 {
		t.Fatalf("run = %+v, want the home visit skipped", run.ActionCounts)
	}
}

func TestPhoneMovementModes(t *testing.T) {
	for _, c := range []struct {
		straight, path float64
		seconds        int64
		want           string
	}{
		{1_000_000, 1_000_000, 2 * 3600, ModeFlying},
		{11_000, 12_000, 20 * 60, ModeDriving},
		{1_000, 1_200, 15 * 60, ModeWalking},
		{4_000, 4_000, 20 * 60, ModeUnknown},
		{4_000, 4_000, 0, ModeUnknown},
	} {
		if got := phoneMovementMode(c.straight, c.path, c.seconds); got != c.want {
			t.Errorf("%.0f m straight, %.0f m path in %d s = %s, want %s", c.straight, c.path, c.seconds, got, c.want)
		}
	}
}

func TestSegmentingKeepsAVisitsPlaceWhenANearerDuplicateAppears(t *testing.T) {
	s := newTestService(t)
	s.must(http.MethodPost, "/points/live", LivePoints{Device: "example-phone", RecordedBy: "someone", Points: phoneDay()}, nil, http.StatusOK)
	s.segment(false)
	var visits []Visit
	s.must(http.MethodGet, "/visits", nil, &visits, http.StatusOK)
	firstPlace := *visits[0].PlaceID
	// The first place sits 44 m off the stay's center; a second record of the
	// same spot sits on it.
	s.must(http.MethodPatch, "/places/"+firstPlace, map[string]any{"latitude": 10.0005}, nil, http.StatusOK)
	s.must(http.MethodPost, "/places", map[string]any{"name": "duplicate", "latitude": 10.0001, "longitude": 20.0, "kind": "other"}, nil, http.StatusCreated)
	run := s.segment(false)
	if run.ActionCounts["unchanged"] != 3 || len(run.ActionCounts) != 1 {
		t.Fatalf("run = %+v, want everything unchanged", run.ActionCounts)
	}
	s.must(http.MethodGet, "/visits", nil, &visits, http.StatusOK)
	if *visits[0].PlaceID != firstPlace {
		t.Fatalf("visit moved to %s from %s", *visits[0].PlaceID, firstPlace)
	}
	var trips []Trip
	s.must(http.MethodGet, "/trips?kind=movement", nil, &trips, http.StatusOK)
	if *trips[0].StartPlaceID != firstPlace || *trips[0].EndPlaceID != *visits[1].PlaceID {
		t.Fatalf("trip ends %s → %s, want the visits' places %s → %s", *trips[0].StartPlaceID, *trips[0].EndPlaceID, firstPlace, *visits[1].PlaceID)
	}
}

func TestSegmentingKeepsAVisitOnThePlaceItWasMergedInto(t *testing.T) {
	s := newTestService(t)
	s.must(http.MethodPost, "/points/live", LivePoints{Device: "example-phone", RecordedBy: "someone", Points: phoneDay()}, nil, http.StatusOK)
	s.segment(false)
	var visits []Visit
	s.must(http.MethodGet, "/visits", nil, &visits, http.StatusOK)
	var kept Place
	s.must(http.MethodPost, "/places", map[string]any{"name": "home", "latitude": 10.0004, "longitude": 20.0, "kind": "home"}, &kept, http.StatusCreated)
	s.must(http.MethodPost, "/places/"+*visits[0].PlaceID+"/merge", PlaceMergeRequest{Into: kept.ID}, nil, http.StatusOK)
	run := s.segment(false)
	if run.ActionCounts["unchanged"] != 3 {
		t.Fatalf("run after a merge = %+v, want everything unchanged", run.ActionCounts)
	}
	s.must(http.MethodGet, "/visits", nil, &visits, http.StatusOK)
	if *visits[0].PlaceID != kept.ID {
		t.Fatalf("visit is at %s, want the kept place %s", *visits[0].PlaceID, kept.ID)
	}
}
