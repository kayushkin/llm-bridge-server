package onlineactivity

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

func TestARequestWithNoCallerIsRefused(t *testing.T) {
	ts := newTestServer(t).as(nil)
	for _, route := range [][2]string{
		{"GET", "/health"}, {"GET", "/vocabularies"}, {"GET", "/stats"}, {"GET", "/activities"},
		{"GET", "/activities/activity_000001"}, {"DELETE", "/activities/activity_000001"},
		{"POST", "/activities/activity_000001/restore"}, {"POST", "/activities/activity_000001/purge"},
		{"GET", "/activities/activity_000001/revisions"}, {"GET", "/attachments/attachment_000001"},
		{"GET", "/days/2026-01-05?tz=UTC"}, {"GET", "/imports"}, {"POST", "/imports"}, {"GET", "/imports/import_000001"},
		{"POST", "/imports/import_000001/batches"}, {"POST", "/imports/import_000001/finish"},
	} {
		if code := ts.raw(route[0], route[1], map[string]any{}).Code; code != http.StatusUnauthorized {
			t.Errorf("%s %s with no caller = %d", route[0], route[1], code)
		}
	}
	// This server's own service names nobody, so it cannot own an import.
	ts.as(thisServer).do("POST", "/imports", ImportStart{Kind: ImportKindMyActivityHTML, Sha256: sha("a")}, http.StatusForbidden, nil)
}

// Alice and Bob each import the same file holding the same cards; neither
// reaches the other's records, and nothing of one is folded into the other's.
func TestOwnersReachOnlyTheirOwnRecords(t *testing.T) {
	ts := newTestServer(t)
	aliceServer, bobServer := ts.as(alice), ts.as(bob)
	aliceImport, counts := aliceServer.importFile(ImportKindMyActivityHTML, sha("a"), search(), watch(SourceGoogleMyActivity, "ref-watch"))
	if counts.Created != 2 || counts.AttachmentsStored != 1 {
		t.Fatalf("alice's import = %+v", counts)
	}
	// The same file, the same cards: for Bob they are new, not known or folded.
	_, counts = bobServer.importFile(ImportKindMyActivityHTML, sha("a"), search(), watch(SourceGoogleMyActivity, "ref-watch"))
	if counts.Created != 2 || counts.Known != 0 || counts.Folded != 0 || counts.AttachmentsStored != 1 {
		t.Fatalf("bob's import of alice's file = %+v, want two new activities of his own", counts)
	}
	// YouTube's history of the watch folds into Bob's own watch only.
	_, counts = bobServer.importFile(ImportKindYouTubeHistoryHTML, sha("b"), watch(SourceYouTubeHistory, "ref-history"))
	if counts.Folded != 1 {
		t.Fatalf("bob's history = %+v", counts)
	}

	var alicePage, bobPage ActivityPage
	aliceServer.do("GET", "/activities?product=Search", nil, 200, &alicePage)
	bobServer.do("GET", "/activities?product=YouTube", nil, 200, &bobPage)
	aliceSearch := alicePage.Activities[0]
	bobWatch := bobPage.Activities[0]
	if aliceSearch.OwnerPrincipalID != aliceID || bobWatch.OwnerPrincipalID != bobID || len(bobWatch.Sightings) != 2 {
		t.Fatalf("alice's search %+v, bob's watch %+v", aliceSearch, bobWatch)
	}
	aliceServer.do("GET", "/activities?product=YouTube", nil, 200, &alicePage)
	if len(alicePage.Activities[0].Sightings) != 1 {
		t.Errorf("bob's history was folded into alice's watch: %+v", alicePage.Activities[0].Sightings)
	}

	// Bob cannot read or change alice's activity, attachment or import: each
	// answers as a missing one.
	for _, route := range [][2]string{
		{"GET", "/activities/" + aliceSearch.ID}, {"GET", "/activities/" + aliceSearch.ID + "?include_deleted=true"},
		{"DELETE", "/activities/" + aliceSearch.ID}, {"POST", "/activities/" + aliceSearch.ID + "/restore"},
		{"POST", "/activities/" + aliceSearch.ID + "/purge"}, {"GET", "/activities/" + aliceSearch.ID + "/revisions"},
		{"GET", "/attachments/" + aliceSearch.Attachments[0].ID}, {"GET", "/imports/" + aliceImport},
		{"POST", "/imports/" + aliceImport + "/finish"},
	} {
		if recorder := bobServer.raw(route[0], route[1], nil); recorder.Code != http.StatusNotFound {
			t.Errorf("bob %s %s = %d", route[0], route[1], recorder.Code)
		}
	}
	if recorder := bobServer.raw("POST", "/imports/"+aliceImport+"/batches", ImportBatch{Activities: []ImportActivity{search()}}); recorder.Code != http.StatusNotFound {
		t.Errorf("bob sending a batch into alice's import = %d", recorder.Code)
	}
	aliceServer.do("GET", "/activities/"+aliceSearch.ID, nil, 200, nil)
	if response := aliceServer.raw("GET", "/attachments/"+aliceSearch.Attachments[0].ID, nil); response.Code != 200 {
		t.Errorf("alice's own attachment = %d", response.Code)
	}

	// Lists, stats, days, imports and health hold only the caller's own.
	bobServer.do("GET", "/activities?q=sourdough", nil, 200, &bobPage)
	if bobPage.Total != 1 || bobPage.Activities[0].OwnerPrincipalID != bobID {
		t.Errorf("bob's search = %+v", bobPage)
	}
	var stats Stats
	bobServer.do("GET", "/stats", nil, 200, &stats)
	if stats.Total != 2 {
		t.Errorf("bob's stats = %+v", stats)
	}
	var day Day
	bobServer.do("GET", "/days/2026-01-05?tz=America/Los_Angeles", nil, 200, &day)
	if day.Total != 2 {
		t.Errorf("bob's day = %+v", day)
	}
	var imports []Import
	bobServer.do("GET", "/imports", nil, 200, &imports)
	if len(imports) != 2 || imports[0].OwnerPrincipalID != bobID {
		t.Errorf("bob's imports = %+v", imports)
	}
	var health Health
	bobServer.do("GET", "/health", nil, 200, &health)
	if health.Counts != (HealthCounts{Activities: 2, Sightings: 3, Attachments: 1, Imports: 2}) {
		t.Errorf("bob's health = %+v", health.Counts)
	}
	bobServer.do("GET", "/activities?owner_principal_id="+aliceID, nil, http.StatusForbidden, nil)

	// An administrator sees both, and narrows to one owner when asked.
	admin := ts.as(administrator)
	var page ActivityPage
	admin.do("GET", "/activities", nil, 200, &page)
	if page.Total != 4 {
		t.Errorf("the administrator's activities = %d, want both owners'", page.Total)
	}
	admin.do("GET", "/activities?owner_principal_id="+aliceID, nil, 200, &page)
	if page.Total != 2 || page.Activities[0].OwnerPrincipalID != aliceID {
		t.Errorf("the administrator narrowed to alice = %+v", page)
	}
	admin.do("GET", "/health", nil, 200, &health)
	if health.Counts.Activities != 4 || health.Counts.Imports != 3 {
		t.Errorf("the administrator's health = %+v", health.Counts)
	}
	admin.do("GET", "/activities/"+aliceSearch.ID, nil, 200, nil)
	// This server's own service, acting as nobody, reads every owner's too.
	ts.as(thisServer).do("GET", "/stats", nil, 200, &stats)
	if stats.Total != 4 {
		t.Errorf("the service's stats = %+v", stats)
	}

	// Purging bob's search destroys his file and leaves alice's.
	bobServer.do("GET", "/activities?product=Search", nil, 200, &bobPage)
	before := ts.files.held()
	bobServer.do("POST", "/activities/"+bobPage.Activities[0].ID+"/purge", nil, 200, nil)
	if after := ts.files.held(); after != before-1 {
		t.Errorf("file-store held %d files before bob's purge and %d after", before, after)
	}
	if response := aliceServer.raw("GET", "/attachments/"+aliceSearch.Attachments[0].ID, nil); response.Code != 200 || response.Body.String() != "not really audio" {
		t.Errorf("alice's attachment after bob's purge = %d", response.Code)
	}
	if told := ts.files.told["file_000001"]; told.ActivityID != aliceSearch.ID || told.ContentType != "audio/mpeg" || !strings.HasSuffix(told.FileName, ".mp3") {
		t.Errorf("file-store was told %+v for alice's attachment", told)
	}
}

// A batch whose file cannot be kept writes nothing, and a batch that fails
// after a file was kept destroys that file again.
func TestAFailedBatchLeavesNoFileBehind(t *testing.T) {
	ts := newTestServer(t)
	var started ImportStarted
	ts.do("POST", "/imports", ImportStart{Kind: ImportKindMyActivityHTML, Sha256: sha("a")}, 200, &started)
	first := search()
	ts.do("POST", "/imports/"+started.Import.ID+"/batches", ImportBatch{Activities: []ImportActivity{first}}, 200, nil)
	if held := ts.files.held(); held != 1 {
		t.Fatalf("file-store holds %d files after one attachment", held)
	}
	second := search()
	second.SourceRef, second.OccurredAt = "ref-second", eveningSearchAt+3600
	second.Attachments = append(second.Attachments, ImportAttachment{FileName: "second.mp3", ContentType: "audio/mpeg",
		ContentBase64: "c2Vjb25kIGZpbGU="})
	third := search()
	third.SourceRef, third.OccurredAt = "ref-third", eveningSearchAt+7200
	third.Attachments[0].ContentBase64 = "dGhpcmQgZmlsZQ=="
	// The second record's two files are kept; file-store then refuses the
	// third record's file, so the batch fails and the two kept ones go.
	ts.files.failAfter(2)
	if recorder := ts.raw("POST", "/imports/"+started.Import.ID+"/batches", ImportBatch{Activities: []ImportActivity{second, third}}); recorder.Code != http.StatusInternalServerError {
		t.Fatalf("a batch whose file cannot be kept = %d %s", recorder.Code, recorder.Body.String())
	}
	if held := ts.files.held(); held != 1 {
		t.Errorf("file-store holds %d files after the failed batch, want only the first batch's", held)
	}
	var health Health
	ts.do("GET", "/health", nil, 200, &health)
	if health.Counts.Activities != 1 || health.Counts.Attachments != 1 {
		t.Errorf("a failed batch wrote rows: %+v", health.Counts)
	}
}

func TestAZeroScopeReachesNothing(t *testing.T) {
	store, _ := testStore(t)
	if _, err := store.ListActivities(testContext, ownership.Scope{}, ActivityFilter{Limit: 10}); err == nil {
		t.Error("a scope that was never set listed activities")
	}
	if _, err := store.Counts(testContext, ownership.Scope{}); err == nil {
		t.Error("a scope that was never set counted activities")
	}
	if _, err := store.StartImport(testContext, "", ImportStart{Kind: ImportKindMyActivityHTML, Sha256: sha("a")}); err == nil {
		t.Error("an import with no owner was started")
	}
}
