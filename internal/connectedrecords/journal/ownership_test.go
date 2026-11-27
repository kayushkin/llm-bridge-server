package journal

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// examplePathOf fills a route pattern's wildcards so it can be called.
func examplePathOf(pattern string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	path = strings.ReplaceAll(path, "{id}", "entry_000001")
	path = strings.ReplaceAll(path, "{slug}", "hello")
	return method, path
}

func TestEveryRouteRefusesACallerWithNoPrincipalAndARequestWithNoCaller(t *testing.T) {
	service := newTestService(t)
	service.mustCreate(EntryInput{Kind: "timeline", Title: "A trip"})
	routes := (&handler{}).routes()
	if len(routes) < 20 {
		t.Fatalf("the route list holds %d routes; it is not the list RegisterHandlers mounts", len(routes))
	}
	nobody := service.as(ownership.Caller{SeesEveryOwner: true})
	for _, route := range routes {
		method, path := examplePathOf(route.pattern)
		var answer map[string]string
		if status := nobody.call(method, path, nil, &answer); status != http.StatusForbidden || answer["error"] != refusalOfEmptyPrincipal {
			t.Errorf("%s as a caller with no principal = %d %q, want 403 %q", route.pattern, status, answer["error"], refusalOfEmptyPrincipal)
		}
		if status := service.withoutCaller().call(method, path, nil, nil); status != http.StatusUnauthorized {
			t.Errorf("%s with no caller = %d, want 401", route.pattern, status)
		}
	}
}

func TestAnotherOwnersRecordsAreNotFoundForReadOrWrite(t *testing.T) {
	service := newTestService(t)
	asBob := service.as(bob)
	entry := service.mustCreate(EntryInput{Kind: "journal", Body: "- [ ] water plants\n[Carol](ref:person_000012)"})
	timeline := service.mustCreate(EntryInput{Kind: "timeline", Title: "A trip"})
	item := service.mustAddItems(timeline.ID, TimelineItemInput{OccurredAt: 100, Precision: "day", Title: "left"})[0]
	gone := service.mustCreate(EntryInput{Kind: "note", Title: "gone"})
	service.call(http.MethodDelete, "/entries/"+gone.ID, nil, nil)

	for _, request := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/entries/" + entry.ID, nil},
		{http.MethodGet, "/entries/" + gone.ID + "?include_deleted=true", nil},
		{http.MethodPatch, "/entries/" + entry.ID, map[string]any{"body": "Bob was here"}},
		{http.MethodDelete, "/entries/" + entry.ID, nil},
		{http.MethodPost, "/entries/" + gone.ID + "/restore", nil},
		{http.MethodGet, "/entries/" + entry.ID + "/revisions", nil},
		{http.MethodPost, "/entries/" + entry.ID + "/promote-todo", map[string]int{"line": 1}},
		{http.MethodGet, "/timelines/" + timeline.ID + "/items", nil},
		{http.MethodPost, "/timelines/" + timeline.ID + "/items", TimelineItemsRequest{Items: []TimelineItemInput{{OccurredAt: 1, Precision: "day", Title: "x"}}}},
		{http.MethodGet, "/timeline-items?timeline=" + timeline.ID, nil},
		{http.MethodGet, "/timeline-items/" + item.ID, nil},
		{http.MethodPatch, "/timeline-items/" + item.ID, map[string]any{"title": "Bob was here"}},
		{http.MethodDelete, "/timeline-items/" + item.ID, nil},
		{http.MethodPost, "/timeline-items/" + item.ID + "/restore", nil},
		{http.MethodGet, "/timeline-items/" + item.ID + "/revisions", nil},
	} {
		if status := asBob.call(request.method, request.path, request.body, nil); status != http.StatusNotFound {
			t.Errorf("Bob: %s %s = %d, want 404, the answer for a missing record", request.method, request.path, status)
		}
	}

	var unchanged Entry
	service.call(http.MethodGet, "/entries/"+entry.ID, nil, &unchanged)
	if unchanged.DeletedAt != 0 || strings.Contains(unchanged.Body, "Bob") {
		t.Errorf("Bob's refused writes changed Alice's entry")
	}
	var revisions []Revision
	service.call(http.MethodGet, "/entries/"+entry.ID+"/revisions", nil, &revisions)
	if len(revisions) != 0 {
		t.Errorf("Bob's refused writes left %d revisions", len(revisions))
	}
	if len(service.noteboard.received) != 0 {
		t.Errorf("Bob's refused promote-todo reached noteboard")
	}
}

func TestAListHoldsOnlyTheCallersOwnRecords(t *testing.T) {
	service := newTestService(t)
	asBob := service.as(bob)
	mine := service.mustCreate(EntryInput{Kind: "post", Title: "Alice's post", Body: "[Carol](ref:person_000012) tea", Visibility: "published"})
	theirs := asBob.mustCreate(EntryInput{Kind: "post", Title: "Bob's post", Body: "[Carol](ref:person_000012) tea", Visibility: "published"})
	if mine.OwnerPrincipalID != alicePrincipalID || theirs.OwnerPrincipalID != bobPrincipalID {
		t.Fatalf("owners = %q and %q, want each the caller that created it", mine.OwnerPrincipalID, theirs.OwnerPrincipalID)
	}

	for _, path := range []string{"/entries", "/entries?q=tea", "/published"} {
		var entries []Entry
		service.call(http.MethodGet, path, nil, &entries)
		if len(entries) != 1 || entries[0].ID != mine.ID {
			t.Errorf("Alice: GET %s = %d entries, want only her own %s", path, len(entries), mine.ID)
		}
	}
	var backlinks []Backlink
	asBob.call(http.MethodGet, "/mentions?ref=person_000012", nil, &backlinks)
	if len(backlinks) != 1 || backlinks[0].Entry.ID != theirs.ID {
		t.Errorf("Bob: backlinks = %d, want only his own entry", len(backlinks))
	}
	var health Health
	asBob.call(http.MethodGet, "/health", nil, &health)
	if health.Counts.Entries != 1 || health.Counts.Mentions != 1 || health.Counts.Published != 1 {
		t.Errorf("Bob's health counts %+v, want only his own records", health.Counts)
	}
	if status := asBob.call(http.MethodGet, "/entries?owner_principal_id="+alicePrincipalID, nil, nil); status != http.StatusForbidden {
		t.Errorf("Bob naming Alice as the owner = %d, want 403", status)
	}
	if status := asBob.call(http.MethodGet, "/entries?owner_principal_id="+bobPrincipalID, nil, nil); status != http.StatusOK {
		t.Errorf("Bob naming himself as the owner = %d, want 200", status)
	}
}

func TestAnAdministratorSeesEveryOwnerAndCanNarrowToOne(t *testing.T) {
	service := newTestService(t)
	service.mustCreate(EntryInput{Kind: "note", Title: "Alice's"})
	theirs := service.as(bob).mustCreate(EntryInput{Kind: "note", Title: "Bob's"})
	administrator := service.as(aliceAdministrator)

	var entries []Entry
	administrator.call(http.MethodGet, "/entries", nil, &entries)
	if len(entries) != 2 {
		t.Fatalf("an administrator lists %d entries, want both owners' 2", len(entries))
	}
	administrator.call(http.MethodGet, "/entries?owner_principal_id="+bobPrincipalID, nil, &entries)
	if len(entries) != 1 || entries[0].ID != theirs.ID {
		t.Errorf("narrowed to Bob: %d entries, want only %s", len(entries), theirs.ID)
	}
	var health Health
	administrator.call(http.MethodGet, "/health?owner_principal_id="+alicePrincipalID, nil, &health)
	if health.Counts.Entries != 1 {
		t.Errorf("health narrowed to Alice counts %d entries, want 1", health.Counts.Entries)
	}

	// A write to Bob's entry by the administrator: what it creates under it,
	// the revision and the mentions, is Bob's, not the administrator's.
	var patched Entry
	if status := administrator.call(http.MethodPatch, "/entries/"+theirs.ID, map[string]any{"body": "[Carol](ref:person_000012)"}, &patched); status != http.StatusOK {
		t.Fatalf("an administrator's PATCH of Bob's entry = %d", status)
	}
	if patched.OwnerPrincipalID != bobPrincipalID {
		t.Errorf("after an administrator's PATCH the entry is owned by %q, want Bob still", patched.OwnerPrincipalID)
	}
	var revisions []Revision
	administrator.call(http.MethodGet, "/entries/"+theirs.ID+"/revisions", nil, &revisions)
	if len(revisions) != 1 || revisions[0].OwnerPrincipalID != bobPrincipalID || revisions[0].Entry.OwnerPrincipalID != bobPrincipalID {
		t.Errorf("the revision's owner = %+v, want Bob's", revisions)
	}
	var bobsHealth Health
	service.as(bob).call(http.MethodGet, "/health", nil, &bobsHealth)
	if bobsHealth.Counts.Mentions != 1 || bobsHealth.Counts.Revisions != 1 {
		t.Errorf("Bob's counts after the administrator's write = %+v, want the mention and revision his", bobsHealth.Counts)
	}

	// Items an administrator adds to Bob's timeline are Bob's.
	timeline := service.as(bob).mustCreate(EntryInput{Kind: "timeline", Title: "Bob's trip"})
	items := administrator.mustAddItems(timeline.ID, TimelineItemInput{OccurredAt: 100, Precision: "day", Title: "left"})
	if items[0].OwnerPrincipalID != bobPrincipalID {
		t.Errorf("an item on Bob's timeline is owned by %q, want Bob", items[0].OwnerPrincipalID)
	}
	if status := service.as(bob).call(http.MethodGet, "/timeline-items/"+items[0].ID, nil, nil); status != http.StatusOK {
		t.Errorf("Bob reading the item on his own timeline = %d, want 200", status)
	}
}

func TestTheDatabaseRefusesAChildUnderAnotherOwnersRecord(t *testing.T) {
	service := newTestService(t)
	entry := service.mustCreate(EntryInput{Kind: "timeline", Title: "Alice's trip"})
	seq := mustNumber(entry.ID)
	for name, statement := range map[string]string{
		"mention":       `INSERT INTO mentions (owner_principal_id, entry_seq, position, line, entity_ref) VALUES ($1, $2, 0, 1, 'person_000012')`,
		"revision":      `INSERT INTO entry_revisions (owner_principal_id, entry_seq, reason, replaced_at, kind, title, body, written_at, visibility, published_at, slug, tags, about_ref, source, source_ref, source_hash, created_at, updated_at, deleted_at) VALUES ($1, $2, 'update', 1, 'note', '', '', 1, 'private', 0, '', '[]', '', '', '', '', 1, 1, 0)`,
		"timeline item": `INSERT INTO timeline_items (owner_principal_id, timeline_seq, occurred_at, precision, title, created_at, updated_at) VALUES ($1, $2, 1, 'day', 'x', 1, 1)`,
	} {
		if _, err := service.store.db.ExecContext(context.Background(), statement, bobPrincipalID, seq); err == nil || !strings.Contains(err.Error(), "foreign key") {
			t.Errorf("a %s owned by Bob under Alice's entry was stored (err %v); the composite key should refuse it", name, err)
		}
	}
	if _, err := service.store.db.ExecContext(context.Background(), `INSERT INTO entries (owner_principal_id, kind, written_at, visibility, created_at, updated_at) VALUES ('alice', 'note', 1, 'private', 1, 1)`); err == nil {
		t.Errorf("an entry whose owner is not a principal id was stored")
	}
}

func TestTwoOwnersMayHoldTheSameImportAndTheSameSlug(t *testing.T) {
	service := newTestService(t)
	asBob := service.as(bob)
	var aliceImport, bobImport ImportResult
	if status := service.call(http.MethodPost, "/imports", importRequest(samsungNote("a.txt", "h1", "Alice's")), &aliceImport); status != http.StatusOK || aliceImport.Created != 1 {
		t.Fatalf("Alice's import = %d %+v", status, aliceImport)
	}
	if status := asBob.call(http.MethodPost, "/imports", importRequest(samsungNote("a.txt", "h1", "Bob's")), &bobImport); status != http.StatusOK || bobImport.Created != 1 {
		t.Fatalf("Bob's import of the same source pair = %d %+v, want it created as his own", status, bobImport)
	}
	if aliceImport.Outcomes[0].EntryID == bobImport.Outcomes[0].EntryID {
		t.Fatalf("the two imports landed on one entry")
	}
	// Alice's re-import with a new hash updates only her own entry.
	var again ImportResult
	service.call(http.MethodPost, "/imports", importRequest(samsungNote("a.txt", "h2", "Alice's, edited")), &again)
	if again.Updated != 1 || again.Outcomes[0].EntryID != aliceImport.Outcomes[0].EntryID {
		t.Errorf("Alice's re-import = %+v, want her own entry updated", again)
	}
	var bobsEntry Entry
	asBob.call(http.MethodGet, "/entries/"+bobImport.Outcomes[0].EntryID, nil, &bobsEntry)
	if bobsEntry.Body != "Bob's" {
		t.Errorf("Alice's re-import changed Bob's entry")
	}

	alicePost := service.mustCreate(EntryInput{Kind: "post", Title: "Hello", Visibility: "published"})
	bobPost := asBob.mustCreate(EntryInput{Kind: "post", Title: "Hello", Visibility: "published"})
	if alicePost.Slug != "hello" || bobPost.Slug != "hello" {
		t.Fatalf("slugs = %q and %q, want both hello", alicePost.Slug, bobPost.Slug)
	}
	if status := service.call(http.MethodPost, "/entries", EntryInput{Kind: "post", Title: "Hello", Visibility: "published"}, nil); status != http.StatusConflict {
		t.Errorf("Alice publishing a second hello = %d, want 409", status)
	}
	var bySlug Entry
	asBob.call(http.MethodGet, "/published/hello", nil, &bySlug)
	if bySlug.ID != bobPost.ID {
		t.Errorf("Bob's /published/hello = %s, want his own %s", bySlug.ID, bobPost.ID)
	}
	administrator := service.as(aliceAdministrator)
	if status := administrator.call(http.MethodGet, "/published/hello", nil, nil); status != http.StatusBadRequest {
		t.Errorf("an administrator's /published/hello, which two owners publish = %d, want 400 asking which owner", status)
	}
	if status := administrator.call(http.MethodGet, "/published/hello?owner_principal_id="+alicePrincipalID, nil, &bySlug); status != http.StatusOK || bySlug.ID != alicePost.ID {
		t.Errorf("narrowed to Alice = %d %s, want %s", status, bySlug.ID, alicePost.ID)
	}
}

func TestOnlyAnAdministratorMayPromoteATodoAndOnlyOfTheirOwnEntry(t *testing.T) {
	service := newTestService(t)
	mine := service.mustCreate(EntryInput{Kind: "journal", Body: "- [ ] water plants"})
	theirs := service.as(bob).mustCreate(EntryInput{Kind: "journal", Body: "- [ ] water plants"})

	var answer map[string]string
	if status := service.call(http.MethodPost, "/entries/"+mine.ID+"/promote-todo", `{"line":1}`, &answer); status != http.StatusForbidden || !strings.Contains(answer["error"], "noteboard") {
		t.Errorf("the owner who is not an administrator = %d %q, want 403 saying why", status, answer["error"])
	}
	administrator := service.as(aliceAdministrator)
	if status := administrator.call(http.MethodPost, "/entries/"+theirs.ID+"/promote-todo", `{"line":1}`, &answer); status != http.StatusForbidden || !strings.Contains(answer["error"], "owner") {
		t.Errorf("an administrator on Bob's entry = %d %q, want 403 saying only the owner may", status, answer["error"])
	}
	if len(service.noteboard.received) != 0 {
		t.Fatalf("a refused promotion reached noteboard")
	}
	if status := administrator.call(http.MethodPost, "/entries/"+mine.ID+"/promote-todo", `{"line":1}`, nil); status != http.StatusCreated {
		t.Errorf("an administrator on their own entry = %d, want 201", status)
	}
}

func TestTheOverlayAndBacklinksReadInScope(t *testing.T) {
	service := newTestService(t)
	asBob := service.as(bob)
	aliceTimeline := service.mustCreate(EntryInput{Kind: "timeline", Title: "Alice's"})
	bobTimeline := asBob.mustCreate(EntryInput{Kind: "timeline", Title: "Bob's"})
	service.mustAddItems(aliceTimeline.ID, TimelineItemInput{OccurredAt: 100, Precision: "day", Title: "a", Sources: []string{"entry_000042"}})
	asBob.mustAddItems(bobTimeline.ID, TimelineItemInput{OccurredAt: 200, Precision: "day", Title: "b", Sources: []string{"entry_000042"}})

	if status := asBob.call(http.MethodGet, "/timeline-items?timeline="+bobTimeline.ID+"&timeline="+aliceTimeline.ID, nil, nil); status != http.StatusNotFound {
		t.Errorf("Bob's overlay naming Alice's timeline = %d, want 404 for her timeline", status)
	}
	var merged []TimelineItem
	if status := service.as(aliceAdministrator).call(http.MethodGet, "/timeline-items?timeline="+bobTimeline.ID+"&timeline="+aliceTimeline.ID, nil, &merged); status != http.StatusOK || len(merged) != 2 {
		t.Errorf("an administrator's overlay of both = %d with %d items, want 200 with 2", status, len(merged))
	}
	var backlinks []Backlink
	asBob.call(http.MethodGet, "/mentions?ref=entry_000042", nil, &backlinks)
	if len(backlinks) != 1 || backlinks[0].Entry.ID != bobTimeline.ID {
		t.Errorf("Bob's backlinks through timeline items = %d, want only his own timeline", len(backlinks))
	}
}
