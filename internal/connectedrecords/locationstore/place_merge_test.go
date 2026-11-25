package locationstore

import (
	"net/http"
	"testing"
)

// Every coordinate and place id here is invented: a grid near 10°N 20°E.

func TestMergingAPlaceMovesItsVisitsAndTripsAndImportsFollowIt(t *testing.T) {
	s := newTestService(t)
	var started ImportStarted
	s.must(http.MethodPost, "/imports", ImportStart{Kind: ImportKindGoogleTimeline, Sha256: "aa00000000000000000000000000000000000000000000000000000000000001"}, &started, http.StatusOK)
	visit := func(place string, from, to int64) ImportVisit {
		return ImportVisit{Source: SourceGoogleTimeline, SourceRef: phoneSourceRef(from, to), StartedAt: from, EndedAt: to,
			Latitude: float(10.0001), Longitude: float(20.0), GooglePlaceID: place}
	}
	batch := ImportBatch{
		Places: []ImportPlace{
			{GooglePlaceID: "example-home", Latitude: 10.0001, Longitude: 20.0, Kind: PlaceKindOther, Source: SourceGoogleTimeline},
			{GooglePlaceID: "example-home-label", Latitude: 10.0002, Longitude: 20.0, Kind: PlaceKindOther, Source: SourceGoogleTimeline},
		},
		Visits: []ImportVisit{visit("example-home", minutes(0), minutes(60)), visit("example-home-label", minutes(120), minutes(180))},
		Trips: []ImportTrip{{Source: SourceGoogleTimeline, SourceRef: phoneSourceRef(minutes(60), minutes(120)), StartedAt: minutes(60), EndedAt: minutes(120),
			StartLatitude: float(10.0001), StartLongitude: float(20.0), EndLatitude: float(10.0002), EndLongitude: float(20.0),
			StartGooglePlaceID: "example-home", EndGooglePlaceID: "example-home-label", SourceMode: "WALKING", Mode: ModeWalking}},
	}
	s.must(http.MethodPost, "/imports/"+started.Import.ID+"/batches", batch, nil, http.StatusOK)
	var places []Place
	s.must(http.MethodGet, "/places", nil, &places, http.StatusOK)
	keep, merged := places[0].ID, places[1].ID

	var merge PlaceMerge
	s.must(http.MethodPost, "/places/"+merged+"/merge", PlaceMergeRequest{Into: keep}, &merge, http.StatusOK)
	if merge.VisitsMoved != 1 || merge.TripEndsMoved != 1 || merge.TripStartsMoved != 0 || merge.Place.ID != keep {
		t.Fatalf("merge = %+v", merge)
	}
	var visits []Visit
	s.must(http.MethodGet, "/visits?place_id="+keep, nil, &visits, http.StatusOK)
	if len(visits) != 2 {
		t.Fatalf("%d visits at the kept place, want 2", len(visits))
	}
	var gone Place
	s.must(http.MethodGet, "/places/"+merged+"?include_deleted=true", nil, &gone, http.StatusOK)
	if gone.DeletedAt == 0 || gone.MergedIntoPlaceID == nil || *gone.MergedIntoPlaceID != keep {
		t.Fatalf("merged place = %+v", gone)
	}
	var revisions []Revision
	s.must(http.MethodGet, "/visits/"+visits[1].ID+"/revisions", nil, &revisions, http.StatusOK)
	if len(revisions) != 1 || revisions[0].Reason != reasonMerge {
		t.Fatalf("revisions of the moved visit = %+v, want one with reason merge", revisions)
	}

	// A later export naming the merged place by Google's id lands on the kept one.
	var later ImportStarted
	s.must(http.MethodPost, "/imports", ImportStart{Kind: ImportKindGoogleTimeline, Sha256: "aa00000000000000000000000000000000000000000000000000000000000002"}, &later, http.StatusOK)
	s.must(http.MethodPost, "/imports/"+later.Import.ID+"/batches", ImportBatch{Visits: []ImportVisit{visit("example-home-label", minutes(300), minutes(360))}}, nil, http.StatusOK)
	s.must(http.MethodGet, "/visits?place_id="+keep, nil, &visits, http.StatusOK)
	if len(visits) != 3 {
		t.Fatalf("%d visits at the kept place after a later import, want 3", len(visits))
	}
}

func TestMergeRefusesWhatCannotBeMerged(t *testing.T) {
	s := newTestService(t)
	var a, b Place
	s.must(http.MethodPost, "/places", map[string]any{"name": "A", "latitude": 10.0, "longitude": 20.0, "kind": "other"}, &a, http.StatusCreated)
	s.must(http.MethodPost, "/places", map[string]any{"name": "B", "latitude": 10.0, "longitude": 20.0, "kind": "other"}, &b, http.StatusCreated)
	for name, step := range map[string]struct {
		path string
		body any
		want int
	}{
		"no into":      {"/places/" + a.ID + "/merge", PlaceMergeRequest{}, http.StatusBadRequest},
		"into itself":  {"/places/" + a.ID + "/merge", PlaceMergeRequest{Into: a.ID}, http.StatusBadRequest},
		"no such into": {"/places/" + a.ID + "/merge", PlaceMergeRequest{Into: "place_999999"}, http.StatusNotFound},
		"unknown key":  {"/places/" + a.ID + "/merge", `{"into":"` + b.ID + `","name":"x"}`, http.StatusBadRequest},
	} {
		if status, answer := s.call(http.MethodPost, step.path, step.body, nil); status != step.want {
			t.Errorf("%s: %d %s, want %d", name, status, answer, step.want)
		}
	}
	s.must(http.MethodPost, "/places/"+a.ID+"/merge", PlaceMergeRequest{Into: b.ID}, nil, http.StatusOK)
	if status, _ := s.call(http.MethodPost, "/places/"+a.ID+"/merge", PlaceMergeRequest{Into: b.ID}, nil); status != http.StatusConflict {
		t.Errorf("merging a merged place again = %d, want 409", status)
	}
}
