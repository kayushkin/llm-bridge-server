package journalstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testRegistryRows is a cut of reference-store's list of types: enough to show a
// prefixed id typed, a uuid typed, and two patterns that both claim an id.
var testRegistryRows = []EntityTypeRow{
	{Type: "person", Service: "people-store", Get: "/people/{id}", IDPatterns: []string{`person_\d{6,}`}},
	{Type: "principal", Service: "principal-store", Get: "/principals/{id}", IDPatterns: []string{`principal_\d{6,}`}},
	{Type: "note", Service: "noteboard", Get: "/api/items/{id}", IDPatterns: []string{`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`}},
	{Type: "entry", Service: "journal-store", Get: "/entries/{id}", IDPatterns: []string{`entry_\d{6,}`}},
	{Type: "both-a", Service: "x", Get: "/a/{id}", IDPatterns: []string{`twin_\d+`}},
	{Type: "both-b", Service: "y", Get: "/b/{id}", IDPatterns: []string{`twin_\d+`}},
}

// fakeNoteboard records every todo it is asked to create and answers with a
// fixed uuid, or with failWith when set.
type fakeNoteboard struct {
	mutex    sync.Mutex
	received []noteboardTodo
	failWith int
	server   *httptest.Server
}

const fakeTodoID = "3f2a9c1e-7b4d-4e8a-9c6f-0a1b2c3d4e5f"

func newFakeNoteboard(t *testing.T) *fakeNoteboard {
	fake := &fakeNoteboard{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/items" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		var todo noteboardTodo
		if err := json.NewDecoder(r.Body).Decode(&todo); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fake.mutex.Lock()
		defer fake.mutex.Unlock()
		if fake.failWith != 0 {
			http.Error(w, "noteboard is down", fake.failWith)
			return
		}
		fake.received = append(fake.received, todo)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":%q,"type":"todo","title":%q}`, fakeTodoID, todo.Title)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

type testService struct {
	t         *testing.T
	store     *Store
	server    *httptest.Server
	noteboard *fakeNoteboard
}

func newTestServiceWithRegistry(t *testing.T, registry *EntityTypeRegistry) *testService {
	t.Helper()
	noteboard := newFakeNoteboard(t)
	store, err := Open(t.TempDir(), registry, NewNoteboardClient(noteboard.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	mux := http.NewServeMux()
	RegisterHandlers(mux, store)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &testService{t: t, store: store, server: server, noteboard: noteboard}
}

func newTestService(t *testing.T) *testService {
	t.Helper()
	registry := NewStaticEntityTypeRegistry(testRegistryRows)
	if err := registry.Refresh(); err != nil {
		t.Fatal(err)
	}
	return newTestServiceWithRegistry(t, registry)
}

// call sends a request and decodes a JSON answer into out when out is not nil.
func (s *testService) call(method, path string, body any, out any) int {
	s.t.Helper()
	var reader io.Reader
	switch typed := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			s.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, s.server.URL+path, reader)
	if err != nil {
		s.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	if out != nil && len(answer) > 0 {
		if err := json.Unmarshal(answer, out); err != nil {
			s.t.Fatalf("%s %s = %d, answer not JSON for %T: %v: %s", method, path, response.StatusCode, out, err, answer)
		}
	}
	return response.StatusCode
}

func (s *testService) mustCreate(input EntryInput) Entry {
	s.t.Helper()
	var created Entry
	if status := s.call(http.MethodPost, "/entries", input, &created); status != http.StatusCreated {
		s.t.Fatalf("POST /entries = %d: %+v", status, created)
	}
	return created
}

func TestIdsAreEntryAndASixDigitSequence(t *testing.T) {
	service := newTestService(t)
	first := service.mustCreate(EntryInput{Kind: "journal", Body: "first"})
	second := service.mustCreate(EntryInput{Kind: "note", Title: "Second", Body: "second"})
	if first.ID != "entry_000001" || second.ID != "entry_000002" {
		t.Fatalf("ids = %s, %s; want entry_000001, entry_000002", first.ID, second.ID)
	}
	if formatID(1234567) != "entry_1234567" {
		t.Errorf("formatID(1234567) = %s: a seventh digit must widen the id, not wrap it", formatID(1234567))
	}
}

func TestCreateDefaultsWrittenAtOnlyWhenNoneIsSentAndVisibilityToPrivate(t *testing.T) {
	service := newTestService(t)
	before := time.Now().Unix()
	unset := service.mustCreate(EntryInput{Kind: "journal", Body: "today"})
	if unset.WrittenAt < before || unset.WrittenAt > time.Now().Unix() {
		t.Errorf("written_at with none sent = %d, want now", unset.WrittenAt)
	}
	if unset.Visibility != VisibilityPrivate {
		t.Errorf("visibility with none sent = %q, want private", unset.Visibility)
	}
	sent := service.mustCreate(EntryInput{Kind: "journal", Body: "old", WrittenAt: 1500000000})
	if sent.WrittenAt != 1500000000 {
		t.Errorf("written_at sent as 1500000000 stored as %d", sent.WrittenAt)
	}
}

func TestCreateRefusesWhatAnEntryCannotBe(t *testing.T) {
	service := newTestService(t)
	for name, body := range map[string]string{
		"unknown kind":                 `{"kind":"diary","body":"x"}`,
		"unknown visibility":           `{"kind":"journal","visibility":"public","body":"x"}`,
		"a note with no title":         `{"kind":"note","body":"x"}`,
		"about with no about_ref":      `{"kind":"about","title":"Carol","body":"x"}`,
		"about_ref on a journal entry": `{"kind":"journal","about_ref":"person_000001","body":"x"}`,
		"a source field":               `{"kind":"note","title":"t","source":"samsung-notes"}`,
		"a misspelt field":             `{"kind":"note","title":"t","bdy":"x"}`,
		"an uppercase slug":            `{"kind":"post","title":"t","slug":"Hello"}`,
		"an empty tag":                 `{"kind":"note","title":"t","tags":[" "]}`,
	} {
		var answer map[string]string
		if status := service.call(http.MethodPost, "/entries", body, &answer); status != http.StatusBadRequest {
			t.Errorf("%s: POST /entries = %d (%v), want 400", name, status, answer)
		}
	}
}

func TestVocabulariesAreServed(t *testing.T) {
	service := newTestService(t)
	var kinds, visibilities []string
	service.call(http.MethodGet, "/entry-kinds", nil, &kinds)
	service.call(http.MethodGet, "/visibilities", nil, &visibilities)
	if strings.Join(kinds, ",") != "journal,post,note,about,timeline,transcript" {
		t.Errorf("GET /entry-kinds = %v", kinds)
	}
	if strings.Join(visibilities, ",") != "private,draft,published" {
		t.Errorf("GET /visibilities = %v", visibilities)
	}
}

func TestExtractMentionsReadsOnlyRefLinksOutsideCode(t *testing.T) {
	body := strings.Join([]string{
		"Lunch with [Carol](ref:person_000012) and [a site](https://carol.example).",    // 1
		"See [that todo](ref:3f2a9c1e-7b4d-4e8a-9c6f-0a1b2c3d4e5f), [plain](notes.md).", // 2
		"Inline `[not this](ref:person_000099)` is code.",                               // 3
		"```",                           // 4
		"[nor this](ref:person_000098)", // 5
		"```",                           // 6
		`[a \[bracketed\] label](ref:principal_000003) and [Carol again](ref:person_000012)`, // 7
		"![a picture](ref:entry_000004)",                           // 8
		"[spaced](ref: person_000001) [empty]() ref:person_000002", // 9
	}, "\n")
	got := ExtractMentions(body)
	want := []Mention{
		{Position: 0, Line: 1, EntityRef: "person_000012", Label: "Carol"},
		{Position: 1, Line: 2, EntityRef: "3f2a9c1e-7b4d-4e8a-9c6f-0a1b2c3d4e5f", Label: "that todo"},
		{Position: 2, Line: 7, EntityRef: "principal_000003", Label: "a [bracketed] label"},
		{Position: 3, Line: 7, EntityRef: "person_000012", Label: "Carol again"},
		{Position: 4, Line: 8, EntityRef: "entry_000004", Label: "a picture"},
	}
	if len(got) != len(want) {
		t.Fatalf("ExtractMentions found %d mentions, want %d: %+v", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("mention %d = %+v, want %+v", index, got[index], want[index])
		}
	}
}

func TestMentionsAreTypedFromTheRegistryAndRewrittenOnEveryWrite(t *testing.T) {
	service := newTestService(t)
	entry := service.mustCreate(EntryInput{Kind: "journal", Body: "[Carol](ref:person_000012), [todo](ref:3F2A9C1E-7B4D-4E8A-9C6F-0A1B2C3D4E5F), [?](ref:thing_1), [twin](ref:twin_7)"})
	types := map[string]string{}
	for _, mention := range entry.Mentions {
		types[mention.EntityRef] = mention.EntityType
	}
	want := map[string]string{
		"person_000012":                        "person",
		"3F2A9C1E-7B4D-4E8A-9C6F-0A1B2C3D4E5F": "note", // patterns match case-insensitively, as dash's resolver does
		"thing_1":                              "",     // no registry type claims it
		"twin_7":                               "",     // two types claim it; the reader decides, not this store
	}
	for ref, wantType := range want {
		if got, found := types[ref]; !found || got != wantType {
			t.Errorf("mention %s typed %q (found %v), want %q", ref, got, found, wantType)
		}
	}

	var updated Entry
	service.call(http.MethodPatch, "/entries/"+entry.ID, map[string]string{"body": "only [Bob](ref:person_000013)"}, &updated)
	if len(updated.Mentions) != 1 || updated.Mentions[0].EntityRef != "person_000013" {
		t.Fatalf("after the body changed, mentions = %+v", updated.Mentions)
	}
	var backlinks []Backlink
	service.call(http.MethodGet, "/mentions?ref=person_000012", nil, &backlinks)
	if len(backlinks) != 0 {
		t.Errorf("person_000012 is no longer in the body, and still has backlinks: %+v", backlinks)
	}
}

func TestBacklinksListLiveEntriesMentioningARef(t *testing.T) {
	service := newTestService(t)
	older := service.mustCreate(EntryInput{Kind: "journal", Body: "met [Carol](ref:person_000012)", WrittenAt: 1700000000})
	newer := service.mustCreate(EntryInput{Kind: "journal", Body: "[A.](ref:person_000012) again, and [Carol](ref:person_000012)", WrittenAt: 1800000000})
	gone := service.mustCreate(EntryInput{Kind: "journal", Body: "[Carol](ref:person_000012)"})
	service.mustCreate(EntryInput{Kind: "journal", Body: "[Bob](ref:person_000013)"})
	service.call(http.MethodDelete, "/entries/"+gone.ID, nil, nil)

	var backlinks []Backlink
	if status := service.call(http.MethodGet, "/mentions?ref=person_000012", nil, &backlinks); status != http.StatusOK {
		t.Fatalf("GET /mentions = %d", status)
	}
	if len(backlinks) != 2 || backlinks[0].Entry.ID != newer.ID || backlinks[1].Entry.ID != older.ID {
		t.Fatalf("backlinks = %+v, want %s then %s", backlinks, newer.ID, older.ID)
	}
	if len(backlinks[0].Mentions) != 2 {
		t.Errorf("the newer entry mentions person_000012 twice; got %+v", backlinks[0].Mentions)
	}
	if status := service.call(http.MethodGet, "/mentions", nil, nil); status != http.StatusBadRequest {
		t.Errorf("GET /mentions with no ref = %d, want 400", status)
	}
}

func TestMentionsStoredBeforeTheRegistryCouldBeReadAreTypedAfterward(t *testing.T) {
	failing := true
	registry := &EntityTypeRegistry{fetch: func() ([]EntityTypeRow, error) {
		if failing {
			return nil, errors.New("reference-store unreachable")
		}
		return testRegistryRows, nil
	}}
	if err := registry.Refresh(); err == nil {
		t.Fatal("the failing registry read succeeded")
	}
	service := newTestServiceWithRegistry(t, registry)
	var health Health
	service.call(http.MethodGet, "/health", nil, &health)
	if health.Status != "degraded" || !strings.Contains(health.EntityRegistry.Error, "unreachable") {
		t.Errorf("health with no registry = %+v, want degraded with the read's error", health)
	}

	entry := service.mustCreate(EntryInput{Kind: "journal", Body: "[Carol](ref:person_000012)"})
	if entry.Mentions[0].EntityType != "" {
		t.Fatalf("with no registry the mention was typed %q", entry.Mentions[0].EntityType)
	}
	failing = false
	if err := registry.Refresh(); err != nil {
		t.Fatal(err)
	}
	if typed, err := service.store.BackfillMentionTypes(); err != nil || typed != 1 {
		t.Fatalf("BackfillMentionTypes = %d, %v; want 1", typed, err)
	}
	var fetched Entry
	service.call(http.MethodGet, "/entries/"+entry.ID, nil, &fetched)
	if fetched.Mentions[0].EntityType != "person" {
		t.Errorf("after the backfill the mention is typed %q, want person", fetched.Mentions[0].EntityType)
	}
	service.call(http.MethodGet, "/health", nil, &health)
	if health.Status != "ok" || health.EntityRegistry.Types != len(testRegistryRows) {
		t.Errorf("health after a good read = %+v", health)
	}
}

func TestARegistryWithAPatternThatDoesNotCompileIsRefusedWhole(t *testing.T) {
	registry := NewStaticEntityTypeRegistry([]EntityTypeRow{{Type: "bad", IDPatterns: []string{`(`}}})
	if err := registry.Refresh(); err == nil || !strings.Contains(err.Error(), "does not compile") {
		t.Fatalf("Refresh = %v, want a refusal naming the pattern", err)
	}
}

func TestPatchRefusesUnknownAndUnpatchableFields(t *testing.T) {
	service := newTestService(t)
	entry := service.mustCreate(EntryInput{Kind: "journal", Body: "x"})
	for body, wantInError := range map[string]string{
		`{"bdy":"y"}`:                      `unknown field "bdy"`,
		`{"source":"samsung-notes"}`:       `"source" cannot be patched`,
		`{"published_at":1}`:               `"published_at" cannot be patched`,
		`{"deleted_at":1}`:                 `"deleted_at" cannot be patched`,
		`{"title":null}`:                   `"title" is null`,
		`{"body":"y","visiblity":"draft"}`: `unknown field "visiblity"`,
	} {
		var answer map[string]string
		status := service.call(http.MethodPatch, "/entries/"+entry.ID, body, &answer)
		if status != http.StatusBadRequest || !strings.Contains(answer["error"], wantInError) {
			t.Errorf("PATCH %s = %d %q, want 400 containing %q", body, status, answer["error"], wantInError)
		}
	}
	var unchanged Entry
	service.call(http.MethodGet, "/entries/"+entry.ID, nil, &unchanged)
	if unchanged.Body != "x" {
		t.Errorf("a refused PATCH changed the body to %q", unchanged.Body)
	}
	var revisions []Revision
	service.call(http.MethodGet, "/entries/"+entry.ID+"/revisions", nil, &revisions)
	if len(revisions) != 0 {
		t.Errorf("refused PATCHes left %d revisions", len(revisions))
	}
	if status := service.call(http.MethodPatch, "/entries/entry_999999", `{"body":"y"}`, nil); status != http.StatusNotFound {
		t.Errorf("PATCH of an unknown entry = %d, want 404", status)
	}
}

func TestDeleteIsSoftAndRestoreBringsTheEntryBackWithAFullHistory(t *testing.T) {
	service := newTestService(t)
	entry := service.mustCreate(EntryInput{Kind: "note", Title: "Plan", Body: "v1", Tags: []string{"a"}, WrittenAt: 1600000000})
	service.call(http.MethodPatch, "/entries/"+entry.ID, map[string]any{"body": "v2", "tags": []string{"b"}}, nil)

	if status := service.call(http.MethodDelete, "/entries/"+entry.ID, nil, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d", status)
	}
	if status := service.call(http.MethodGet, "/entries/"+entry.ID, nil, nil); status != http.StatusNotFound {
		t.Errorf("GET of a deleted entry = %d, want 404", status)
	}
	var deleted Entry
	if status := service.call(http.MethodGet, "/entries/"+entry.ID+"?include_deleted=true", nil, &deleted); status != http.StatusOK || deleted.DeletedAt == 0 {
		t.Errorf("GET ?include_deleted=true = %d, deleted_at %d", status, deleted.DeletedAt)
	}
	var listed []Entry
	service.call(http.MethodGet, "/entries", nil, &listed)
	if len(listed) != 0 {
		t.Errorf("GET /entries lists a deleted entry: %+v", listed)
	}
	if status := service.call(http.MethodPatch, "/entries/"+entry.ID, `{"body":"v3"}`, nil); status != http.StatusNotFound {
		t.Errorf("PATCH of a deleted entry = %d, want 404", status)
	}
	if status := service.call(http.MethodDelete, "/entries/"+entry.ID, nil, nil); status != http.StatusNoContent {
		t.Errorf("a second DELETE = %d, want 204", status)
	}

	var restored Entry
	if status := service.call(http.MethodPost, "/entries/"+entry.ID+"/restore", nil, &restored); status != http.StatusOK {
		t.Fatalf("restore = %d", status)
	}
	if restored.DeletedAt != 0 || restored.Body != "v2" || restored.Tags[0] != "b" {
		t.Errorf("restored = %+v, want the entry as it was deleted", restored)
	}

	var revisions []Revision
	service.call(http.MethodGet, "/entries/"+entry.ID+"/revisions", nil, &revisions)
	reasons := []string{}
	for _, revision := range revisions {
		reasons = append(reasons, revision.Reason)
	}
	if strings.Join(reasons, ",") != "restore,delete,update" {
		t.Fatalf("revision reasons newest first = %v, want restore,delete,update (the second delete changed nothing)", reasons)
	}
	original := revisions[2].Entry
	if original.ID != entry.ID || original.Kind != "note" || original.Title != "Plan" || original.Body != "v1" ||
		original.Tags[0] != "a" || original.WrittenAt != 1600000000 || original.Visibility != "private" || original.CreatedAt == 0 {
		t.Errorf("the update's revision does not hold the whole prior row: %+v", original)
	}
	if revisions[0].Entry.DeletedAt == 0 {
		t.Errorf("the restore's revision does not hold the deleted_at it replaced")
	}
	var paged []Revision
	service.call(http.MethodGet, "/entries/"+entry.ID+"/revisions?limit=1&offset=1", nil, &paged)
	if len(paged) != 1 || paged[0].Reason != "delete" {
		t.Errorf("revisions?limit=1&offset=1 = %+v, want the delete", paged)
	}
}

func TestPublishingDerivesAUniqueSlugAndKeepsTheFirstPublicationDate(t *testing.T) {
	service := newTestService(t)
	post := service.mustCreate(EntryInput{Kind: "post", Title: "Hello, World! Again", Body: "hi"})
	if post.Slug != "" || post.PublishedAt != 0 {
		t.Fatalf("a private post has slug %q, published_at %d", post.Slug, post.PublishedAt)
	}
	var published Entry
	if status := service.call(http.MethodPatch, "/entries/"+post.ID, `{"visibility":"published"}`, &published); status != http.StatusOK {
		t.Fatalf("publish = %d", status)
	}
	if published.Slug != "hello-world-again" || published.PublishedAt == 0 {
		t.Fatalf("published: slug %q, published_at %d", published.Slug, published.PublishedAt)
	}

	russian := service.mustCreate(EntryInput{Kind: "post", Title: "Привет, мир", Visibility: "published"})
	if russian.Slug != "привет-мир" {
		t.Errorf("a Russian title gave slug %q, want привет-мир", russian.Slug)
	}

	// The same title again: the derived slug is taken, and the caller is told
	// to pick one rather than handed hello-world-again-2 unasked.
	twin := service.mustCreate(EntryInput{Kind: "post", Title: "Hello world again"})
	var conflict map[string]string
	if status := service.call(http.MethodPatch, "/entries/"+twin.ID, `{"visibility":"published"}`, &conflict); status != http.StatusConflict {
		t.Fatalf("publishing a taken slug = %d %v, want 409", status, conflict)
	}
	if status := service.call(http.MethodPatch, "/entries/"+twin.ID, `{"visibility":"published","slug":"hello-twin"}`, nil); status != http.StatusOK {
		t.Errorf("publishing under an edited slug = %d", status)
	}
	// A draft may hold a slug a published entry has.
	if status := service.call(http.MethodPost, "/entries", `{"kind":"post","title":"x","visibility":"draft","slug":"hello-world-again"}`, nil); status != http.StatusCreated {
		t.Errorf("a draft holding a published slug = %d, want 201", status)
	}
	// A journal entry with no title has nothing to derive from.
	untitled := service.mustCreate(EntryInput{Kind: "journal", Body: "x"})
	if status := service.call(http.MethodPatch, "/entries/"+untitled.ID, `{"visibility":"published"}`, nil); status != http.StatusBadRequest {
		t.Errorf("publishing an untitled entry with no slug = %d, want 400", status)
	}

	// Back to draft for a fix and out again: same date, same slug.
	firstPublishedAt := published.PublishedAt
	time.Sleep(1100 * time.Millisecond)
	service.call(http.MethodPatch, "/entries/"+post.ID, `{"visibility":"draft"}`, nil)
	var listed []Entry
	service.call(http.MethodGet, "/published", nil, &listed)
	for _, entry := range listed {
		if entry.ID == post.ID {
			t.Errorf("GET /published lists a draft")
		}
	}
	if status := service.call(http.MethodGet, "/published/hello-world-again", nil, nil); status != http.StatusNotFound {
		t.Errorf("GET /published/{slug} of a draft = %d, want 404", status)
	}
	service.call(http.MethodPatch, "/entries/"+post.ID, `{"visibility":"published"}`, &published)
	if published.PublishedAt != firstPublishedAt || published.Slug != "hello-world-again" {
		t.Errorf("republished: published_at %d (was %d), slug %q", published.PublishedAt, firstPublishedAt, published.Slug)
	}
	var bySlug Entry
	if status := service.call(http.MethodGet, "/published/hello-world-again", nil, &bySlug); status != http.StatusOK || bySlug.ID != post.ID {
		t.Errorf("GET /published/hello-world-again = %d %s", status, bySlug.ID)
	}

	// Deleting frees the slug; restoring after someone else took it is a conflict.
	service.call(http.MethodDelete, "/entries/"+post.ID, nil, nil)
	taker := service.mustCreate(EntryInput{Kind: "post", Title: "Hello world again", Visibility: "published"})
	if taker.Slug != "hello-world-again" {
		t.Fatalf("the freed slug was not reusable: %q", taker.Slug)
	}
	if status := service.call(http.MethodPost, "/entries/"+post.ID+"/restore", nil, nil); status != http.StatusConflict {
		t.Errorf("restoring onto a taken slug = %d, want 409", status)
	}
}

func TestListFiltersAndSearch(t *testing.T) {
	service := newTestService(t)
	service.mustCreate(EntryInput{Kind: "journal", Body: "rain all day", WrittenAt: 1700000000, Tags: []string{"weather"}})
	middle := service.mustCreate(EntryInput{Kind: "note", Title: "Well-being plan", Body: "sleep", WrittenAt: 1710000000})
	about := service.mustCreate(EntryInput{Kind: "about", Title: "Carol", Body: "likes tea", AboutRef: "person_000012", WrittenAt: 1720000000})

	check := func(query string, wantIDs ...string) {
		t.Helper()
		var entries []Entry
		if status := service.call(http.MethodGet, "/entries"+query, nil, &entries); status != http.StatusOK {
			t.Fatalf("GET /entries%s = %d", query, status)
		}
		got := []string{}
		for _, entry := range entries {
			got = append(got, entry.ID)
		}
		if strings.Join(got, ",") != strings.Join(wantIDs, ",") {
			t.Errorf("GET /entries%s = %v, want %v", query, got, wantIDs)
		}
	}
	check("", about.ID, middle.ID, "entry_000001")
	check("?kind=note", middle.ID)
	check("?exclude_kind=note&exclude_kind=about", "entry_000001")
	check("?tag=weather", "entry_000001")
	check("?q=weath", "entry_000001") // a prefix of a tag
	check("?q=well-being", middle.ID) // a hyphen is text, not a 500
	check(`?q=%22tea`, about.ID)      // a stray quote too
	check("?about_ref=person_000012", about.ID)
	check("?from=1705000000&to=1720000000", middle.ID) // to is exclusive
	check("?limit=1&offset=1", middle.ID)
	check("?visibility=private", about.ID, middle.ID, "entry_000001")

	for _, query := range []string{"?from=2024-01-01", "?kind=diary", "?exclude_kind=diary", "?visibility=public", "?limit=x"} {
		if status := service.call(http.MethodGet, "/entries"+query, nil, nil); status != http.StatusBadRequest {
			t.Errorf("GET /entries%s = %d, want 400", query, status)
		}
	}
}

func importRequest(entries ...ImportEntry) ImportRequest { return ImportRequest{Entries: entries} }

func samsungNote(ref, hash, body string) ImportEntry {
	return ImportEntry{Source: "samsung-notes", SourceRef: ref, SourceHash: hash, Kind: "note", Title: ref, Body: body,
		WrittenAt: 1600000000, Tags: []string{"samsung-notes"}}
}

func TestListBySourceShowsOnlyThatImportersEntries(t *testing.T) {
	service := newTestService(t)
	other := samsungNote("c.txt", "h3", "three")
	other.Source = "skype"
	service.call(http.MethodPost, "/imports", importRequest(samsungNote("a.txt", "h1", "one"), other), nil)
	var entries []Entry
	if status := service.call(http.MethodGet, "/entries?source=skype", nil, &entries); status != http.StatusOK {
		t.Fatalf("GET ?source= = %d", status)
	}
	if len(entries) != 1 || entries[0].Source != "skype" || entries[0].SourceRef != "c.txt" {
		t.Errorf("?source=skype = %+v", entries)
	}
}

func TestImportIsIdempotentBySourceAndSourceRef(t *testing.T) {
	service := newTestService(t)
	var result ImportResult
	if status := service.call(http.MethodPost, "/imports", importRequest(samsungNote("a.txt", "h1", "one"), samsungNote("b.txt", "h2", "two")), &result); status != http.StatusOK {
		t.Fatalf("first import = %d", status)
	}
	if result.Created != 2 || result.Updated != 0 || result.Skipped != 0 {
		t.Fatalf("first import = %+v", result)
	}
	imported := result.Outcomes[0].EntryID
	var entry Entry
	service.call(http.MethodGet, "/entries/"+imported, nil, &entry)
	if entry.Source != "samsung-notes" || entry.SourceRef != "a.txt" || entry.SourceHash != "h1" || entry.WrittenAt != 1600000000 || entry.Visibility != "private" {
		t.Errorf("imported entry = %+v", entry)
	}

	// The user publishes one in the meantime.
	service.call(http.MethodPatch, "/entries/"+imported, `{"visibility":"draft","slug":"mine"}`, nil)

	service.call(http.MethodPost, "/imports", importRequest(samsungNote("a.txt", "h1-changed", "one, edited"), samsungNote("b.txt", "h2", "two")), &result)
	if result.Created != 0 || result.Updated != 1 || result.Skipped != 1 {
		t.Fatalf("second import = %+v, want 1 updated and 1 skipped", result)
	}
	service.call(http.MethodGet, "/entries/"+imported, nil, &entry)
	if entry.Body != "one, edited" || entry.SourceHash != "h1-changed" {
		t.Errorf("updated import = %+v", entry)
	}
	if entry.Visibility != "draft" || entry.Slug != "mine" {
		t.Errorf("a re-import overwrote the user's visibility/slug: %q %q", entry.Visibility, entry.Slug)
	}
	var revisions []Revision
	service.call(http.MethodGet, "/entries/"+imported+"/revisions", nil, &revisions)
	if len(revisions) != 2 || revisions[0].Reason != "import" || revisions[0].Entry.Body != "one" {
		t.Errorf("revisions after the re-import = %+v", revisions)
	}

	// A dry run says what would happen and writes nothing.
	service.call(http.MethodPost, "/imports?dry_run=true", importRequest(samsungNote("a.txt", "h1-again", "x"), samsungNote("c.txt", "h3", "three")), &result)
	if result.Created != 1 || result.Updated != 1 {
		t.Errorf("dry run = %+v", result)
	}
	var counts Health
	service.call(http.MethodGet, "/health", nil, &counts)
	if counts.Counts.Entries != 2 {
		t.Errorf("a dry run wrote: %d entries", counts.Counts.Entries)
	}

	for name, request := range map[string]ImportRequest{
		"no written_at":  importRequest(ImportEntry{Source: "s", SourceRef: "r", SourceHash: "h", Kind: "note", Title: "t"}),
		"no source_hash": importRequest(ImportEntry{Source: "s", SourceRef: "r", Kind: "note", Title: "t", WrittenAt: 1}),
		"a pair twice":   importRequest(samsungNote("d.txt", "h", "x"), samsungNote("d.txt", "h", "x")),
		"one bad of two": importRequest(samsungNote("e.txt", "h", "x"), ImportEntry{Source: "samsung-notes", SourceRef: "f.txt", SourceHash: "h", Kind: "bogus", Title: "t", WrittenAt: 1}),
		"no entries":     importRequest(),
	} {
		if status := service.call(http.MethodPost, "/imports", request, nil); status != http.StatusBadRequest {
			t.Errorf("%s: POST /imports = %d, want 400", name, status)
		}
	}
	service.call(http.MethodGet, "/health", nil, &counts)
	if counts.Counts.Entries != 2 {
		t.Errorf("a refused import wrote part of itself: %d entries", counts.Counts.Entries)
	}
}

func TestPromoteTodoCreatesANoteboardTodoAndLinksTheLine(t *testing.T) {
	service := newTestService(t)
	entry := service.mustCreate(EntryInput{Kind: "journal", Body: "Today\n\n  * [ ] call [the] plumber\n- [x] done already\n- [ ] [linked](ref:" + fakeTodoID + ")\nplain line"})

	var result PromoteTodoResult
	if status := service.call(http.MethodPost, "/entries/"+entry.ID+"/promote-todo", `{"line":3}`, &result); status != http.StatusCreated {
		t.Fatalf("promote-todo = %d %+v", status, result)
	}
	if result.TodoID != fakeTodoID {
		t.Errorf("todo_id = %q", result.TodoID)
	}
	lines := strings.Split(result.Entry.Body, "\n")
	if want := `  * [ ] [call \[the\] plumber](ref:` + fakeTodoID + `)`; lines[2] != want {
		t.Errorf("line 3 = %q, want %q", lines[2], want)
	}
	if len(service.noteboard.received) != 1 {
		t.Fatalf("noteboard received %d todos", len(service.noteboard.received))
	}
	todo := service.noteboard.received[0]
	if todo.Type != "todo" || todo.Title != "call [the] plumber" || len(todo.Links) != 1 || todo.Links[0] != entry.ID ||
		!strings.Contains(todo.Body, entry.ID) {
		t.Errorf("noteboard was sent %+v", todo)
	}
	found := false
	for _, mention := range result.Entry.Mentions {
		if mention.EntityRef == fakeTodoID && mention.Line == 3 && mention.EntityType == "note" && mention.Label == "call [the] plumber" {
			found = true
		}
	}
	if !found {
		t.Errorf("the promoted line is not a typed mention: %+v", result.Entry.Mentions)
	}
	var revisions []Revision
	service.call(http.MethodGet, "/entries/"+entry.ID+"/revisions", nil, &revisions)
	if len(revisions) != 1 || revisions[0].Reason != "promote-todo" {
		t.Errorf("revisions = %+v", revisions)
	}

	// A ticked box is promoted ticked.
	service.call(http.MethodPost, "/entries/"+entry.ID+"/promote-todo", `{"line":4}`, &result)
	if got := strings.Split(result.Entry.Body, "\n")[3]; got != "- [x] [done already](ref:"+fakeTodoID+")" {
		t.Errorf("line 4 = %q", got)
	}

	for body, wantStatus := range map[string]int{
		`{"line":5}`:  http.StatusConflict,   // already linked
		`{"line":6}`:  http.StatusBadRequest, // not a checklist item
		`{"line":0}`:  http.StatusBadRequest,
		`{"line":99}`: http.StatusBadRequest,
		`{"lines":1}`: http.StatusBadRequest,
	} {
		if status := service.call(http.MethodPost, "/entries/"+entry.ID+"/promote-todo", body, nil); status != wantStatus {
			t.Errorf("promote-todo %s = %d, want %d", body, status, wantStatus)
		}
	}
	if len(service.noteboard.received) != 2 {
		t.Errorf("refused promotions reached noteboard: %d todos", len(service.noteboard.received))
	}
}

func TestPromoteTodoLeavesTheEntryAloneWhenNoteboardFails(t *testing.T) {
	service := newTestService(t)
	entry := service.mustCreate(EntryInput{Kind: "journal", Body: "- [ ] water plants"})
	service.noteboard.failWith = http.StatusInternalServerError
	var answer map[string]string
	if status := service.call(http.MethodPost, "/entries/"+entry.ID+"/promote-todo", `{"line":1}`, &answer); status != http.StatusBadGateway {
		t.Fatalf("promote-todo with noteboard down = %d %v, want 502", status, answer)
	}
	var unchanged Entry
	service.call(http.MethodGet, "/entries/"+entry.ID, nil, &unchanged)
	if unchanged.Body != "- [ ] water plants" {
		t.Errorf("the body changed although no todo was made: %q", unchanged.Body)
	}
}

func TestHealthCountsAndUnknownEntries(t *testing.T) {
	service := newTestService(t)
	service.mustCreate(EntryInput{Kind: "post", Title: "p", Visibility: "published", Body: "[x](ref:person_000001)"})
	gone := service.mustCreate(EntryInput{Kind: "journal", Body: "y"})
	service.call(http.MethodDelete, "/entries/"+gone.ID, nil, nil)
	var health Health
	service.call(http.MethodGet, "/health", nil, &health)
	want := Counts{Entries: 1, Deleted: 1, Published: 1, Mentions: 1, Revisions: 1}
	if health.Counts != want || health.Status != "ok" {
		t.Errorf("health = %+v, want counts %+v", health, want)
	}
	for _, path := range []string{"/entries/entry_999999", "/entries/entry_999999/revisions", "/published/nothing"} {
		if status := service.call(http.MethodGet, path, nil, nil); status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, status)
		}
	}
}
