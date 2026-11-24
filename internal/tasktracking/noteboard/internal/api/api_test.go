package api_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/kayushkin/noteboard/internal/api"
	"github.com/kayushkin/noteboard/internal/db"
	"github.com/kayushkin/noteboard/internal/settings"
	"github.com/kayushkin/noteboard/model"
	_ "modernc.org/sqlite"
)

func setup(t *testing.T) (*api.API, func()) {
	t.Helper()
	a, _, cleanup := setupWithPath(t)
	return a, cleanup
}

// setupWithPath also hands back the database file, for the one test that has to
// break the store from underneath to produce a failure that is not a missing
// row.
func setupWithPath(t *testing.T) (*api.API, string, func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	store, err := db.New(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(dir, 0755)
	registry, err := settings.NewRegistry(servicesettings.MapEnvironment(map[string]string{"NOTEBOARD_PORT": "18191", "NOTEBOARD_DB": path}))
	if err != nil {
		t.Fatal(err)
	}
	a := api.New(store, registry)
	return a, path, func() { store.Close() }
}

func TestGetSettingsDescribesTheServiceAndNothingCanBeWritten(t *testing.T) {
	a, path, cleanup := setupWithPath(t)
	defer cleanup()

	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d: %s", w.Code, w.Body)
	}
	var described msg.ServiceSettings
	if err := json.Unmarshal(w.Body.Bytes(), &described); err != nil {
		t.Fatal(err)
	}
	if described.Service != settings.ServiceName || len(described.Settings) != len(settings.Definitions()) {
		t.Fatalf("service=%q with %d settings, want %q with %d", described.Service, len(described.Settings), settings.ServiceName, len(settings.Definitions()))
	}
	for _, setting := range described.Settings {
		if setting.Editable {
			t.Errorf("%s is editable, and this service has no operator gate to put a write behind", setting.Key)
		}
		if setting.Key == settings.DatabasePath && (setting.Value != path || setting.Source != msg.ServiceSettingSourceEnvironment) {
			t.Errorf("database path served as %q from %q, want %q from the environment", setting.Value, setting.Source, path)
		}
	}

	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/settings/"+settings.ListenPort, strings.NewReader(`{"value":"1"}`)))
	if w.Code == http.StatusOK {
		t.Errorf("PUT /settings/%s = 200: a write route is mounted", settings.ListenPort)
	}
}

func TestNewRefusesAMissingSettingsRegistry(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("api.New(store, nil) did not panic: GET /settings would fail at its first read instead of at boot")
		}
	}()
	api.New(nil, nil)
}

func TestHealthEndpoint(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Fatalf("expected ok, got %v", resp["status"])
	}
}

func TestCRUD(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()

	// Create
	body, _ := json.Marshal(model.CreateItemRequest{Type: "note", Title: "Test Note"})
	req := httptest.NewRequest("POST", "/api/items", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var item model.Item
	json.NewDecoder(w.Body).Decode(&item)
	if item.Title != "Test Note" {
		t.Fatalf("expected title 'Test Note', got %q", item.Title)
	}

	// Get
	req = httptest.NewRequest("GET", "/api/items/"+item.ID, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("get: expected 200, got %d", w.Code)
	}

	// List
	req = httptest.NewRequest("GET", "/api/items", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("list: expected 200, got %d", w.Code)
	}
	var items []model.Item
	json.NewDecoder(w.Body).Decode(&items)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}

	// Update
	body, _ = json.Marshal(map[string]string{"title": "Updated"})
	req = httptest.NewRequest("PATCH", "/api/items/"+item.ID, bytes.NewReader(body))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("update: expected 200, got %d", w.Code)
	}

	// Delete (soft)
	req = httptest.NewRequest("DELETE", "/api/items/"+item.ID, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("delete: expected 200, got %d", w.Code)
	}
}

func TestSearch(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()

	body, _ := json.Marshal(model.CreateItemRequest{Type: "note", Title: "Searchable Thing"})
	req := httptest.NewRequest("POST", "/api/items", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	req = httptest.NewRequest("GET", "/api/search?q=Searchable", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("search: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var items []model.Item
	json.NewDecoder(w.Body).Decode(&items)
	if len(items) != 1 {
		t.Fatalf("expected 1 search result, got %d", len(items))
	}
}

func TestTags(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()

	tags := []string{"go", "backend"}
	body, _ := json.Marshal(map[string]interface{}{"type": "note", "title": "Tagged", "tags": tags})
	req := httptest.NewRequest("POST", "/api/items", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	req = httptest.NewRequest("GET", "/api/tags", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("tags: expected 200, got %d", w.Code)
	}
	var tagInfos []model.TagInfo
	json.NewDecoder(w.Body).Decode(&tagInfos)
	if len(tagInfos) != 2 {
		t.Fatalf("expected 2 tags, got %d", len(tagInfos))
	}
}

// TestExcludeTag covers the server-side exclude_tag list filter, which lets a
// consumer drop machine-generated noise (e.g. autoworker dispatch cards) from
// the open-todo query before the limit is applied.
func TestExcludeTag(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()

	mk := func(title string, tags []string) {
		body, _ := json.Marshal(map[string]interface{}{"type": "todo", "title": title, "tags": tags})
		req := httptest.NewRequest("POST", "/api/items", bytes.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 201 {
			t.Fatalf("create %q: expected 201, got %d: %s", title, w.Code, w.Body.String())
		}
	}
	mk("Real human todo", []string{"home"})
	mk("autoworker dispatch", []string{"autoworker", "anthropic"})
	mk("scheduler noise", []string{"autoworker", "scheduler"})

	list := func(query string) []model.Item {
		req := httptest.NewRequest("GET", "/api/items?type=todo&status=open"+query, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("list %q: expected 200, got %d", query, w.Code)
		}
		var items []model.Item
		json.NewDecoder(w.Body).Decode(&items)
		return items
	}

	if got := list(""); len(got) != 3 {
		t.Fatalf("no filter: expected 3 todos, got %d", len(got))
	}

	got := list("&exclude_tag=autoworker")
	if len(got) != 1 {
		t.Fatalf("exclude autoworker: expected 1 todo, got %d", len(got))
	}
	if got[0].Title != "Real human todo" {
		t.Fatalf("exclude autoworker: expected the human todo, got %q", got[0].Title)
	}

	// Multiple exclude_tag values are AND-combined (item must lack all of them).
	if got := list("&exclude_tag=autoworker&exclude_tag=home"); len(got) != 0 {
		t.Fatalf("exclude autoworker+home: expected 0 todos, got %d", len(got))
	}
}

// TestPostingAnUnknownParentIDIsABadRequest. The caller mistyped an id; the
// request will never succeed as written. A 500 would tell it to retry, and the
// child it wanted would sit outside the hold and the ceiling of the tree it
// meant to join.
func TestPostingAnUnknownParentIDIsABadRequest(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()

	missing := "a88bca06-3c94-4b7a-9a2d-59c2d1a3e9d1"
	body, _ := json.Marshal(model.CreateItemRequest{
		Type: "todo", Title: "child of nothing", ParentID: &missing,
	})
	req := httptest.NewRequest("POST", "/api/items", bytes.NewReader(body))
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("posting an unknown parent_id returned %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), missing) {
		t.Errorf("the error does not name the id that was wrong: %s", w.Body.String())
	}
}

// createAndRewrite makes an item and rewrites its body n times, leaving n
// revisions behind. It is the fixture for the paging tests below: they need a
// history longer than one page, and a one-revision history cannot tell a limit
// that works from a limit that is ignored.
func createAndRewrite(t *testing.T, h http.Handler, n int) string {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"type": "workspace", "title": "harness-watch memory", "body": "version 0",
	})
	req := httptest.NewRequest("POST", "/api/items", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var item model.Item
	json.NewDecoder(w.Body).Decode(&item)

	for i := 1; i <= n; i++ {
		body, _ = json.Marshal(map[string]string{"body": fmt.Sprintf("version %d", i)})
		req = httptest.NewRequest("PATCH", "/api/items/"+item.ID, bytes.NewReader(body))
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("update %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	return item.ID
}

func TestRevisionsEndpointServesAPage(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	id := createAndRewrite(t, h, 5)

	req := httptest.NewRequest("GET", "/api/items/"+id+"/revisions?limit=2", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("revisions: %d %s", w.Code, w.Body.String())
	}
	var page []model.Revision
	json.NewDecoder(w.Body).Decode(&page)
	if len(page) != 2 {
		t.Fatalf("?limit=2 returned %d revisions", len(page))
	}

	req = httptest.NewRequest("GET", "/api/items/"+id+"/revisions?limit=2&offset=2", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var second []model.Revision
	json.NewDecoder(w.Body).Decode(&second)
	if len(second) != 2 {
		t.Fatalf("second page returned %d revisions", len(second))
	}
	if second[0].ID == page[0].ID {
		t.Fatalf("offset did not move the window: both pages start at revision %d", page[0].ID)
	}
}

func TestRevisionsEndpointStillDefaultsToTheWholeHistory(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	id := createAndRewrite(t, h, 5)

	req := httptest.NewRequest("GET", "/api/items/"+id+"/revisions", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var all []model.Revision
	json.NewDecoder(w.Body).Decode(&all)
	if len(all) != 5 {
		t.Fatalf("unpaged request returned %d revisions, want all 5", len(all))
	}
}

// A limit that cannot be parsed must be refused, not swallowed. Swallowing it
// yields 0, and 0 on this endpoint means the whole history — so ?limit=abc
// would answer with the single most expensive response this service can produce
// while the caller believes it asked for a small one, and nothing in the reply
// would say otherwise.
func TestAnUnusableLimitIsRefusedRatherThanTreatedAsUnlimited(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	id := createAndRewrite(t, h, 5)

	for _, q := range []string{"?limit=abc", "?limit=-1", "?offset=abc", "?offset=-1", "?limit=2&offset=oops"} {
		req := httptest.NewRequest("GET", "/api/items/"+id+"/revisions"+q, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("GET /revisions%s = %d, want 400 (body %s)", q, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
}

// TestDeletingAnUnknownIDIsNotFound. Delete was the only missing-item route on
// this service that answered 500, and it leaked the driver's message doing it.
// A 500 tells the caller to retry a request that will never succeed as written,
// and it says the store failed when what actually happened is that the caller
// named a row that is not there.
func TestDeletingAnUnknownIDIsNotFound(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()

	req := httptest.NewRequest("DELETE", "/api/items/a88bca06-3c94-4b7a-9a2d-59c2d1a3e9d1", nil)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, req)

	if w.Code != 404 {
		t.Fatalf("deleting an unknown id returned %d, want 404: %s", w.Code, w.Body.String())
	}
	// The driver's message is not the caller's business, and it was the whole
	// of the old response body.
	if strings.Contains(w.Body.String(), "sql:") {
		t.Errorf("the response leaks the driver's error: %s", w.Body.String())
	}
}

// TestDeletingAnAlreadyPurgedIDIsNotFound pins the path that actually reaches
// the missing-row branch on an id the caller once held legitimately. A soft
// delete leaves the row, so it does NOT reach it (see the test below); a hard
// purge takes the row away, and every delete after that names nothing.
func TestDeletingAnAlreadyPurgedIDIsNotFound(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()

	body, _ := json.Marshal(model.CreateItemRequest{Type: "todo", Title: "purge me"})
	req := httptest.NewRequest("POST", "/api/items", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var item model.Item
	json.NewDecoder(w.Body).Decode(&item)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("DELETE", "/api/items/"+item.ID+"?hard=true", nil))
	if w.Code != 200 {
		t.Fatalf("purge returned %d, want 200: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("DELETE", "/api/items/"+item.ID, nil))
	if w.Code != 404 {
		t.Fatalf("deleting a purged id returned %d, want 404: %s", w.Code, w.Body.String())
	}
}

// TestDeletingATombstonedItemAgainStaysOK. A second soft delete is a no-op, not
// a missing item: the first stamps deleted_at and leaves the row, so the store
// still finds it and declines to move the tombstone's timestamp. This is the
// direction assertion for the two tests above — 404 must mean "no such row",
// not "this item is deleted", or a repeated delete would start reporting a
// failure for work that succeeded.
func TestDeletingATombstonedItemAgainStaysOK(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()

	body, _ := json.Marshal(model.CreateItemRequest{Type: "todo", Title: "delete me twice"})
	req := httptest.NewRequest("POST", "/api/items", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var item model.Item
	json.NewDecoder(w.Body).Decode(&item)

	for attempt := 1; attempt <= 2; attempt++ {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("DELETE", "/api/items/"+item.ID, nil))
		if w.Code != 200 {
			t.Fatalf("delete attempt %d returned %d, want 200: %s", attempt, w.Code, w.Body.String())
		}
	}
}

// TestEveryMissingItemRouteAnswersNotFound is the claim delete was the sole
// exception to. Asserting it over the whole set rather than on delete alone is
// what makes a future route that forgets it visible here.
func TestEveryMissingItemRouteAnswersNotFound(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()

	missing := "a88bca06-3c94-4b7a-9a2d-59c2d1a3e9d1"
	routes := []struct{ method, path string }{
		{"GET", "/api/items/" + missing},
		{"DELETE", "/api/items/" + missing},
		{"POST", "/api/items/" + missing + "/restore"},
		{"POST", "/api/items/" + missing + "/hold"},
		{"POST", "/api/items/" + missing + "/unhold"},
		{"GET", "/api/items/" + missing + "/occurrences"},
	}
	for _, route := range routes {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(route.method, route.path, nil))
		if w.Code != 404 {
			t.Errorf("%s %s returned %d, want 404: %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
}

// TestADeleteThatFailsForAnyOtherReasonIsStillAServerError is the direction
// assertion for the 404 above, and it is the one case the missing-row tests
// cannot make. Mapping every delete error to 404 passes all of them, and it
// would tell a caller "no such item" about an item that is plainly there —
// exactly what the PATCH handler already refuses to do.
//
// The failure is induced by taking away the table the snapshot writes to, so
// DeleteItem fails after it has already found the row.
func TestADeleteThatFailsForAnyOtherReasonIsStillAServerError(t *testing.T) {
	a, path, cleanup := setupWithPath(t)
	defer cleanup()
	h := a.Handler()

	body, _ := json.Marshal(model.CreateItemRequest{Type: "todo", Title: "present and correct"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/items", bytes.NewReader(body)))
	var item model.Item
	json.NewDecoder(w.Body).Decode(&item)

	sabotage, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sabotage.Exec("DROP TABLE item_revisions"); err != nil {
		t.Fatal(err)
	}
	sabotage.Close()

	// The row is still there, so this is not a missing item by any reading.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/items/"+item.ID, nil))
	if w.Code != 200 {
		t.Fatalf("the item under test is not readable, so the delete below proves nothing: %d", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("DELETE", "/api/items/"+item.ID, nil))
	if w.Code != 500 {
		t.Fatalf("a delete that failed for a reason other than a missing row returned %d, want 500: %s",
			w.Code, w.Body.String())
	}
}
