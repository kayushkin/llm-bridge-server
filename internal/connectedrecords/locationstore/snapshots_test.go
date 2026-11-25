package locationstore

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Every coordinate here is invented: a grid near 10°N 20°E.

func snapshotVisit(startedAt, endedAt int64) ImportVisit {
	return ImportVisit{Source: SourceGoogleTimeline, SourceRef: fmt.Sprintf("%d-%d", startedAt, endedAt), StartedAt: startedAt,
		EndedAt: endedAt, Latitude: float(10.001), Longitude: float(20.001)}
}

func snapshotTrip(startedAt, endedAt int64) ImportTrip {
	return ImportTrip{Source: SourceGoogleTimeline, SourceRef: fmt.Sprintf("%d-%d", startedAt, endedAt), StartedAt: startedAt,
		EndedAt: endedAt, Mode: ModeDriving, SourceMode: "IN_PASSENGER_VEHICLE"}
}

func (s *testService) snapshotImport(hash string, reapply bool, batch ImportBatch, snapshot ImportSnapshot) Import {
	s.t.Helper()
	var started ImportStarted
	s.must(http.MethodPost, "/imports", ImportStart{Kind: ImportKindGoogleTimeline, Sha256: hash, Reapply: reapply}, &started, http.StatusOK)
	s.must(http.MethodPost, "/imports/"+started.Import.ID+"/batches", batch, nil, http.StatusOK)
	var finished Import
	s.must(http.MethodPost, "/imports/"+started.Import.ID+"/finish", ImportFinish{Snapshot: &snapshot}, &finished, http.StatusOK)
	return finished
}

func liveRefs(t *testing.T, s *testService) string {
	t.Helper()
	var visits []Visit
	s.must(http.MethodGet, "/visits?limit=1000", nil, &visits, http.StatusOK)
	var trips []Trip
	s.must(http.MethodGet, "/trips?limit=1000", nil, &trips, http.StatusOK)
	var refs []string
	for _, visit := range visits {
		refs = append(refs, "v"+visit.SourceRef)
	}
	for _, trip := range trips {
		refs = append(refs, "t"+trip.SourceRef)
	}
	return strings.Join(refs, " ")
}

func TestANewerSnapshotSupersedesWhatTheOlderHeldInItsSpanOnly(t *testing.T) {
	service := newTestService(t)
	hashB := strings.Repeat("b", 64)
	hashC := strings.Repeat("c", 64)
	// The first export cuts the morning into one visit and one drive.
	first := ImportBatch{Visits: []ImportVisit{snapshotVisit(1000, 2000), snapshotVisit(5000, 6000)},
		Trips: []ImportTrip{snapshotTrip(2000, 3000)}}
	service.snapshotImport(hashA, false, first, ImportSnapshot{CoversFrom: 1000, CoversTo: 6000, RawSignalsOldestAt: 100, RawSignalsNewestAt: 5900})
	// Someone writes on the drive; a second drive they leave alone.
	service.must(http.MethodPatch, "/trips/trip_000001", map[string]any{"title": "To the lake"}, nil, http.StatusOK)

	// The second export, of the same moment, cuts it differently. Same raw
	// positions: the later import is the newer.
	second := ImportBatch{Visits: []ImportVisit{snapshotVisit(1000, 1500), snapshotVisit(5000, 6000)},
		Trips: []ImportTrip{snapshotTrip(1500, 3000)}}
	finished := service.snapshotImport(hashB, false, second, ImportSnapshot{CoversFrom: 1000, CoversTo: 6000, RawSignalsOldestAt: 100, RawSignalsNewestAt: 5900})
	changes := finished.SnapshotChanges
	if changes == nil || changes.VisitsSuperseded != 1 || changes.TripsSuperseded != 0 || len(changes.KeptEditedIDs) != 1 || changes.KeptEditedIDs[0] != "trip_000001" {
		t.Fatalf("snapshot changes = %+v: the old visit goes, the edited drive stays and is reported", changes)
	}
	if got := liveRefs(t, service); got != "v1000-1500 v5000-6000 t1500-3000 t2000-3000" {
		t.Errorf("live after the second export = %s", got)
	}
	var visit Visit
	service.must(http.MethodGet, "/visits/visit_000001?include_deleted=true", nil, &visit, http.StatusOK)
	if visit.SupersededByImportID == nil || *visit.SupersededByImportID != "import_000002" {
		t.Errorf("superseded visit = %+v, want it to name import_000002", visit)
	}
	var revisions []Revision
	service.must(http.MethodGet, "/visits/visit_000001/revisions", nil, &revisions, http.StatusOK)
	if len(revisions) == 0 || revisions[0].Reason != "superseded by import_000002" {
		t.Errorf("revisions = %+v", revisions)
	}

	// A third, narrower and newer export covers only the afternoon: it may
	// not touch the morning, and it re-cuts the afternoon visit.
	third := ImportBatch{Visits: []ImportVisit{snapshotVisit(5000, 5500)}}
	service.snapshotImport(hashC, false, third, ImportSnapshot{CoversFrom: 4000, CoversTo: 6000, RawSignalsOldestAt: 200, RawSignalsNewestAt: 5900})
	if got := liveRefs(t, service); got != "v1000-1500 v5000-5500 t1500-3000 t2000-3000" {
		t.Errorf("live after the narrower export = %s", got)
	}

	// The first export sent again: it is still the oldest, so it changes
	// nothing that stands, and its own superseded visit stays aside.
	service.snapshotImport(hashA, true, first, ImportSnapshot{CoversFrom: 1000, CoversTo: 6000, RawSignalsOldestAt: 100, RawSignalsNewestAt: 5900})
	if got := liveRefs(t, service); got != "v1000-1500 v5000-5500 t1500-3000 t2000-3000" {
		t.Errorf("live after re-sending the oldest export = %s", got)
	}
	var health Health
	service.must(http.MethodGet, "/health", nil, &health, http.StatusOK)
	if health.Counts.Visits != 2 || health.Counts.Trips != 2 {
		t.Errorf("health = %+v", health.Counts)
	}
}

func TestARecordANewerSnapshotHoldsAgainComesBack(t *testing.T) {
	service := newTestService(t)
	hashB := strings.Repeat("b", 64)
	service.snapshotImport(hashA, false, ImportBatch{Visits: []ImportVisit{snapshotVisit(1000, 2000)}},
		ImportSnapshot{CoversFrom: 1000, CoversTo: 2000, RawSignalsNewestAt: 100})
	// An export with an older raw buffer, imported later, is the older one:
	// it supersedes nothing the newer holds.
	service.snapshotImport(hashB, false, ImportBatch{Visits: []ImportVisit{snapshotVisit(1000, 1800)}},
		ImportSnapshot{CoversFrom: 1000, CoversTo: 2000, RawSignalsNewestAt: 50})
	if got := liveRefs(t, service); got != "v1000-2000" {
		t.Fatalf("live = %s, want the newer export's visit only", got)
	}
}
