package onlineactivity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

var testContext = context.Background()

// Two made-up owners, an administrator and this server's own service.
const (
	aliceID = "principal_000101"
	bobID   = "principal_000102"
)

var (
	alice         = &ownership.Caller{PrincipalID: aliceID}
	bob           = &ownership.Caller{PrincipalID: bobID}
	administrator = &ownership.Caller{PrincipalID: "principal_000001", SeesEveryOwner: true}
	thisServer    = &ownership.Caller{SeesEveryOwner: true}
)

// fakeFiles is file-store in memory: each Put is a file of its own, as
// file-store's are.
type fakeFiles struct {
	mutex   sync.Mutex
	next    int
	content map[string][]byte
	told    map[string]AttachmentFile
	// putsBeforeFailing, when not negative, is how many more Puts succeed
	// before file-store refuses every one.
	putsBeforeFailing int
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{content: map[string][]byte{}, told: map[string]AttachmentFile{}, putsBeforeFailing: -1}
}

func (f *fakeFiles) Put(_ context.Context, content io.Reader, file AttachmentFile) (string, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.putsBeforeFailing == 0 {
		return "", fmt.Errorf("file-store is down")
	}
	if f.putsBeforeFailing > 0 {
		f.putsBeforeFailing--
	}
	bytes, err := io.ReadAll(content)
	if err != nil {
		return "", err
	}
	f.next++
	id := fmt.Sprintf("file_%06d", f.next)
	f.content[id] = bytes
	f.told[id] = file
	return id, nil
}

func (f *fakeFiles) Open(_ context.Context, fileID string) (io.ReadCloser, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	content, found := f.content[fileID]
	if !found {
		return nil, fmt.Errorf("no file %s", fileID)
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (f *fakeFiles) Purge(_ context.Context, fileID string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if _, found := f.content[fileID]; !found {
		return fmt.Errorf("no file %s", fileID)
	}
	delete(f.content, fileID)
	return nil
}

func (f *fakeFiles) failAfter(puts int) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.putsBeforeFailing = puts
}

func (f *fakeFiles) held() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return len(f.content)
}

func testStore(t *testing.T) (*Store, *fakeFiles) {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	database := postgrestest.NewDatabase(t, module)
	files := newFakeFiles()
	return New(database.Open(module), files), files
}

type testServer struct {
	t      *testing.T
	store  *Store
	files  *fakeFiles
	mux    *http.ServeMux
	caller *ownership.Caller
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	store, files := testStore(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, store)
	return &testServer{t: t, store: store, files: files, mux: mux, caller: alice}
}

// as is the same server, called as caller (nil for none).
func (ts *testServer) as(caller *ownership.Caller) *testServer {
	copied := *ts
	copied.caller = caller
	return &copied
}

func (ts *testServer) raw(method, path string, body any) *httptest.ResponseRecorder {
	ts.t.Helper()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			ts.t.Fatal(err)
		}
		reader = bytes.NewReader(payload)
	}
	request := httptest.NewRequest(method, path, reader)
	if ts.caller != nil {
		request = request.WithContext(ownership.ContextWithCaller(request.Context(), *ts.caller))
	}
	recorder := httptest.NewRecorder()
	ts.mux.ServeHTTP(recorder, request)
	return recorder
}

func (ts *testServer) do(method, path string, body any, want int, into any) {
	ts.t.Helper()
	recorder := ts.raw(method, path, body)
	if recorder.Code != want {
		ts.t.Fatalf("%s %s = %d, want %d: %s", method, path, recorder.Code, want, recorder.Body.String())
	}
	if into != nil {
		if err := json.Unmarshal(recorder.Body.Bytes(), into); err != nil {
			ts.t.Fatalf("%s %s: %v: %s", method, path, err, recorder.Body.String())
		}
	}
}

// importFile runs one whole import and answers its id and counts.
func (ts *testServer) importFile(kind, sha string, records ...ImportActivity) (string, ImportCounts) {
	ts.t.Helper()
	var started ImportStarted
	ts.do("POST", "/imports", ImportStart{Kind: kind, Sha256: sha, SourceMachine: "laptop", SourcePath: "Takeout/" + kind}, 200, &started)
	if started.AlreadyFinished {
		return started.Import.ID, ImportCounts{}
	}
	var counts ImportCounts
	ts.do("POST", "/imports/"+started.Import.ID+"/batches", ImportBatch{Activities: records}, 200, &counts)
	ts.do("POST", "/imports/"+started.Import.ID+"/finish", struct{}{}, 200, nil)
	return started.Import.ID, counts
}

func sha(letter string) string { return strings.Repeat(letter, 64) }

func number(value float64) *float64 { return &value }

// 2026-01-06 02:01:10 UTC is the evening of 2026-01-05 in Los Angeles.
const eveningSearchAt = 1767664870

func search() ImportActivity {
	return ImportActivity{Product: "Search", Action: ActionSearched, ActionText: "Searched for", Title: "sourdough starter",
		URL: "https://www.google.com/search?q=sourdough+starter", OccurredAt: eveningSearchAt, Latitude: number(10.0001), Longitude: number(20.0002),
		Details: Details{Products: []string{"Search"}, Locations: []Location{{Name: "Example Town", Latitude: number(10.0001), Longitude: number(20.0002), Basis: "From your device"}}},
		Source:  SourceGoogleMyActivity, SourceRef: "ref-search",
		Attachments: []ImportAttachment{{FileName: "2026-01-06_02_01_10_000_UTC.mp3", ContentType: "audio/mpeg",
			ContentBase64: base64.StdEncoding.EncodeToString([]byte("not really audio"))}}}
}

func watch(source, ref string) ImportActivity {
	return ImportActivity{Product: "YouTube", Action: ActionWatched, ActionText: "Watched", Title: "How to fold a map",
		URL: "https://www.youtube.com/watch?v=abc123", OccurredAt: eveningSearchAt + 600,
		Details: Details{Channel: &Link{Name: "Example Channel", URL: "https://www.youtube.com/channel/UC000"}}, Source: source, SourceRef: ref}
}

func TestAnImportIsIdempotentAndFoldsTheSameWatchFromTwoSources(t *testing.T) {
	ts := newTestServer(t)
	_, counts := ts.importFile(ImportKindMyActivityHTML, sha("a"), search(), watch(SourceGoogleMyActivity, "ref-watch"))
	if counts.Created != 2 || counts.AttachmentsStored != 1 {
		t.Fatalf("first import = %+v", counts)
	}
	// YouTube's own history has the same watch: url and second match.
	_, counts = ts.importFile(ImportKindYouTubeHistoryHTML, sha("b"), watch(SourceYouTubeHistory, "ref-history"))
	if counts.Folded != 1 || counts.Created != 0 {
		t.Fatalf("the same watch from YouTube history = %+v, want it folded", counts)
	}
	// The same file again changes nothing.
	var started ImportStarted
	ts.do("POST", "/imports", ImportStart{Kind: ImportKindMyActivityHTML, Sha256: sha("a")}, 200, &started)
	if !started.AlreadyFinished {
		t.Fatal("the same file was not recognized")
	}
	// A later export holding the same cards: known by reference.
	_, counts = ts.importFile(ImportKindMyActivityHTML, sha("c"), search(), watch(SourceGoogleMyActivity, "ref-watch"))
	if counts.Known != 2 || counts.Created != 0 {
		t.Fatalf("a later export = %+v, want both known", counts)
	}
	var health Health
	ts.do("GET", "/health", nil, 200, &health)
	if health.Counts.Activities != 2 || health.Counts.Sightings != 3 || health.Counts.Attachments != 1 || health.Counts.Imports != 3 {
		t.Errorf("health = %+v", health.Counts)
	}
	var page ActivityPage
	ts.do("GET", "/activities?product=YouTube", nil, 200, &page)
	if page.Total != 1 || len(page.Activities[0].Sightings) != 2 || page.Activities[0].Sightings[1].Source != SourceYouTubeHistory {
		t.Errorf("the watch = %+v", page)
	}
	ts.do("GET", "/activities?source=youtube_history", nil, 200, &page)
	if page.Total != 1 {
		t.Errorf("source filter found %d, want the folded watch", page.Total)
	}
}

// Two products of one source with the same url and second are two records
// Google keeps apart (a search result clicked, and the page Chrome loaded), so
// they stay two activities; and two cards in one file are never folded.
func TestTheSameSourceIsNotFoldedAcrossProductsOrWithinAFile(t *testing.T) {
	ts := newTestServer(t)
	visit := ImportActivity{Product: "Search", Action: ActionVisited, Title: "Example", URL: "https://example.com/", OccurredAt: 1700000000,
		Source: SourceGoogleMyActivity, SourceRef: "search-visit"}
	_, counts := ts.importFile(ImportKindMyActivityHTML, sha("a"), visit)
	chrome := visit
	chrome.Product, chrome.SourceRef = "Chrome", "chrome-visit"
	twice := chrome
	twice.SourceRef = "chrome-visit#2"
	_, counts = ts.importFile(ImportKindMyActivityHTML, sha("b"), chrome, twice)
	if counts.Created != 2 || counts.Folded != 0 {
		t.Fatalf("Chrome's two cards = %+v, want two new activities", counts)
	}
	history := chrome
	history.Source, history.SourceRef = SourceChromeHistory, "1700000000000000-x"
	_, counts = ts.importFile(ImportKindChromeHistoryJSON, sha("c"), history)
	if counts.Folded != 1 {
		t.Fatalf("Chrome's own history = %+v, want it folded into one", counts)
	}
}

func TestOneBadRecordWritesNothing(t *testing.T) {
	ts := newTestServer(t)
	var started ImportStarted
	ts.do("POST", "/imports", ImportStart{Kind: ImportKindMyActivityHTML, Sha256: sha("a")}, 200, &started)
	bad := search()
	bad.Action = "browsed"
	ts.do("POST", "/imports/"+started.Import.ID+"/batches", ImportBatch{Activities: []ImportActivity{search(), bad}}, 400, nil)
	var health Health
	ts.do("GET", "/health", nil, 200, &health)
	if health.Counts.Activities != 0 {
		t.Errorf("a refused batch wrote %d activities", health.Counts.Activities)
	}
	ts.do("POST", "/imports", ImportStart{Kind: "takeout_zip", Sha256: sha("b")}, 400, nil)
	ts.do("POST", "/imports", map[string]string{"kind": ImportKindMyActivityHTML, "sha256": sha("b"), "note": "x"}, 400, nil)
}

func TestListSearchesEveryWordAndRefusesAnUnknownParameter(t *testing.T) {
	ts := newTestServer(t)
	ts.importFile(ImportKindMyActivityHTML, sha("a"), search(), watch(SourceGoogleMyActivity, "ref-watch"))
	var page ActivityPage
	ts.do("GET", "/activities?q=sourdough", nil, 200, &page)
	if page.Total != 1 || page.Activities[0].Title != "sourdough starter" {
		t.Errorf("q=sourdough = %+v", page)
	}
	ts.do("GET", "/activities?q=example+channel", nil, 200, &page)
	if page.Total != 1 || page.Activities[0].Product != "YouTube" {
		t.Errorf("a channel name is searched: %+v", page)
	}
	ts.do("GET", "/activities?q=sourdough+channel", nil, 200, &page)
	if page.Total != 0 {
		t.Errorf("every word must match: %+v", page)
	}
	ts.do("GET", `/activities?q=%22unbalanced+OR`, nil, 200, &page)
	ts.do("GET", "/activities?order=desc&limit=1", nil, 200, &page)
	if page.Total != 2 || len(page.Activities) != 1 || page.Activities[0].Product != "YouTube" {
		t.Errorf("newest first = %+v", page)
	}
	ts.do("GET", "/activities?products=Search", nil, 400, nil)
	ts.do("GET", "/activities?action=browsed", nil, 400, nil)
	ts.do("GET", "/activities?limit=5000", nil, 400, nil)
}

func TestADaySummarizesByProductInTheAskedZone(t *testing.T) {
	ts := newTestServer(t)
	ts.importFile(ImportKindMyActivityHTML, sha("a"), search(), watch(SourceGoogleMyActivity, "ref-watch"))
	var day Day
	ts.do("GET", "/days/2026-01-05?tz=America/Los_Angeles", nil, 200, &day)
	if day.Total != 2 || len(day.Products) != 2 {
		t.Fatalf("the evening of the 5th in Los Angeles = %+v", day)
	}
	for _, product := range day.Products {
		if product.Count != 1 || len(product.First) != 1 || len(product.Actions) != 1 {
			t.Errorf("product %+v", product)
		}
	}
	ts.do("GET", "/days/2026-01-05?tz=UTC", nil, 200, &day)
	if day.Total != 0 {
		t.Errorf("in UTC the search is on the 6th, but the 5th holds %d", day.Total)
	}
	ts.do("GET", "/days/2026-01-06?tz=UTC&first=0", nil, 200, &day)
	if day.Total != 2 || len(day.Products[0].First) != 0 || day.To-day.From != 86400 {
		t.Errorf("the 6th in UTC = %+v", day)
	}
	// A day with a clock change is 23 hours.
	ts.do("GET", "/days/2026-03-08?tz=America/Los_Angeles", nil, 200, &day)
	if day.To-day.From != 23*3600 {
		t.Errorf("the spring-forward day is %d seconds", day.To-day.From)
	}
	ts.do("GET", "/days/2026-01-05", nil, 400, nil)
	ts.do("GET", "/days/2026-01-05?tz=Mars/Olympus", nil, 400, nil)
	ts.do("GET", "/days/5-1-2026?tz=UTC", nil, 400, nil)
}

func TestDeleteRestoreAndPurge(t *testing.T) {
	ts := newTestServer(t)
	ts.importFile(ImportKindMyActivityHTML, sha("a"), search(), watch(SourceGoogleMyActivity, "ref-watch"))
	var page ActivityPage
	ts.do("GET", "/activities?product=Search", nil, 200, &page)
	searched := page.Activities[0]
	if !strings.HasPrefix(searched.ID, "activity_0000") || len(searched.Attachments) != 1 {
		t.Fatalf("activity = %+v", searched)
	}
	attachment := searched.Attachments[0]
	response := ts.raw("GET", "/attachments/"+attachment.ID, nil)
	if response.Code != 200 || response.Body.String() != "not really audio" || response.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("attachment = %d %q %q", response.Code, response.Body.String(), response.Header().Get("Content-Type"))
	}

	ts.do("DELETE", "/activities/"+searched.ID, nil, 204, nil)
	ts.do("GET", "/activities/"+searched.ID, nil, 404, nil)
	ts.do("GET", "/activities/"+searched.ID+"?include_deleted=true", nil, 200, nil)
	ts.do("GET", "/activities?q=sourdough", nil, 200, &page)
	if page.Total != 0 {
		t.Error("a deleted activity is still listed")
	}
	var restored Activity
	ts.do("POST", "/activities/"+searched.ID+"/restore", nil, 200, &restored)
	if restored.DeletedAt != 0 {
		t.Error("restore did not restore")
	}

	var purged Activity
	ts.do("POST", "/activities/"+searched.ID+"/purge", nil, 200, &purged)
	if purged.PurgedAt == 0 || purged.DeletedAt == 0 || purged.Title != "" || purged.URL != "" || purged.Latitude != nil ||
		len(purged.Details.Locations) != 0 || purged.Attachments[0].PurgedAt == 0 {
		t.Errorf("purged = %+v", purged)
	}
	var revisions []Revision
	ts.do("GET", "/activities/"+searched.ID+"/revisions", nil, 200, &revisions)
	if len(revisions) != 3 {
		t.Fatalf("revisions = %d, want delete, restore and purge", len(revisions))
	}
	for _, revision := range revisions {
		if revision.Activity.Title != "" || revision.Activity.URL != "" || revision.Activity.Latitude != nil {
			t.Errorf("revision %d still holds what was purged: %+v", revision.ID, revision.Activity)
		}
	}
	ts.do("GET", "/attachments/"+attachment.ID, nil, 410, nil)
	if held := ts.files.held(); held != 0 {
		t.Errorf("file-store still holds %d files after the only attachment was purged", held)
	}
	ts.do("GET", "/activities?q=sourdough&include_deleted=true", nil, 200, &page)
	if page.Total != 0 {
		t.Error("a purged activity is still in the search index")
	}
	ts.do("POST", "/activities/"+searched.ID+"/restore", nil, 409, nil)
	ts.do("POST", "/activities/"+searched.ID+"/purge", nil, 200, nil)

	// A later export from another source brings nothing back.
	again := search()
	again.Source, again.SourceRef = SourceChromeHistory, "later"
	_, counts := ts.importFile(ImportKindChromeHistoryJSON, sha("d"), again)
	if counts.Folded != 1 || counts.Created != 0 || counts.AttachmentsStored != 0 {
		t.Errorf("a later import of the purged activity = %+v, want it folded into the purged row", counts)
	}
	ts.do("GET", "/activities/activity_999999", nil, 404, nil)
}

func TestVocabulariesAndStats(t *testing.T) {
	ts := newTestServer(t)
	ts.importFile(ImportKindMyActivityHTML, sha("a"), search(), watch(SourceGoogleMyActivity, "ref-watch"))
	var vocabularies Vocabularies
	ts.do("GET", "/vocabularies", nil, 200, &vocabularies)
	for _, phrase := range vocabularies.ActionPhrases {
		if !knownWord(vocabularies.Actions, phrase.Action) {
			t.Errorf("phrase %q maps to %q, which is not an action", phrase.Phrase, phrase.Action)
		}
	}
	var stats Stats
	ts.do("GET", "/stats", nil, 200, &stats)
	if stats.Total != 2 || len(stats.ByProduct) != 2 || stats.FirstAt != eveningSearchAt || stats.LastAt != eveningSearchAt+600 {
		t.Errorf("stats = %+v", stats)
	}
}
