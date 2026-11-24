package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/kanban-store/internal/api"
	"github.com/kayushkin/kanban-store/internal/bundlestore"
	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/grantstore"
	"github.com/kayushkin/kanban-store/internal/llmbridge"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/noteboard"
	"github.com/kayushkin/kanban-store/internal/principalstore"
)

// ============================ Test harness ============================

// fakeNoteboard is an in-memory stand-in for the upstream noteboard service.
// kanban-store's API treats noteboard as the source of truth for card content,
// so the tests need a stateful backend that creates items, applies PATCHes
// (this is how auto_status is observed), serves GETs for board-view assembly,
// and 404s on missing items. We mount it on an httptest.Server and point the
// real noteboard.Client at it — no network, no real noteboard required.
type fakeNoteboard struct {
	mu    sync.Mutex
	items map[string]map[string]any
	seq   int
	// patchStatus, when non-zero, makes every PATCH answer that status instead
	// of applying the change. noteboard being reachable for the create and
	// unreachable a millisecond later is the ordinary case, not an exotic one.
	patchStatus int
	// failStatus, when non-zero, makes every request answer that status: the
	// noteboard that is up for one call and failing for the next.
	failStatus int
	// queries counts POST /api/items/query calls, so a test can tell a read
	// that asked noteboard to filter from one that had no need to.
	queries int
	// itemReads counts GET /api/items/{id} calls, so a test can tell a read
	// that fetched items one by one from one that asked once.
	itemReads int
	// versions counts writes. An item's updated_at is its version, and the ids
	// above must not move when a PATCH takes one.
	versions int
}

// nextVersion is an updated_at no earlier write was given. Call it with mu held.
func (f *fakeNoteboard) nextVersion() string {
	f.versions++
	return fmt.Sprintf("2026-09-18T00:00:00.%09dZ", f.versions)
}

func newFakeNoteboard() *fakeNoteboard {
	return &fakeNoteboard{items: map[string]map[string]any{}}
}

// setHold parks or releases an item the way noteboard does: held_at is a
// timestamp that is either there or not, and it is deliberately not the item's
// status — held work is still open work, it just is not cleared to run.
func (f *fakeNoteboard) setHold(w http.ResponseWriter, id string, held bool, reason string) {
	f.mu.Lock()
	it, ok := f.items[id]
	var out map[string]any
	if ok {
		if held {
			it["held_at"] = time.Now().UTC().Format(time.RFC3339)
			it["hold_reason"] = reason
		} else {
			delete(it, "held_at")
			delete(it, "hold_reason")
		}
		out = clone(it)
	}
	f.mu.Unlock()
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, 200, out)
}

func (f *fakeNoteboard) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/items", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		f.mu.Lock()
		f.seq++
		id := "nb-" + itoa(f.seq)
		payload["id"] = id
		payload["updated_at"] = f.nextVersion()
		payload["created_at"] = payload["updated_at"]
		if _, ok := payload["status"]; !ok {
			payload["status"] = "open"
		}
		f.items[id] = payload
		out := clone(payload)
		f.mu.Unlock()
		writeJSON(w, 201, out)
	})

	mux.HandleFunc("POST /api/items/batch", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			IDs []string `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		items := make([]map[string]any, len(request.IDs))
		for i, id := range request.IDs {
			if item, ok := f.items[id]; ok {
				if _, deleted := item["deleted_at"]; !deleted {
					items[i] = clone(item)
				}
			}
		}
		writeJSON(w, 200, map[string]any{"items": items})
	})

	mux.HandleFunc("GET /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		includeDeleted := r.URL.Query().Get("include_deleted") == "true"
		f.mu.Lock()
		f.itemReads++
		it, ok := f.items[id]
		if ok && !includeDeleted {
			// A deleted item is not found unless asked for by name.
			if _, deleted := it["deleted_at"]; deleted {
				ok = false
			}
		}
		var out map[string]any
		if ok {
			out = clone(it)
		}
		f.mu.Unlock()
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, out)
	})

	// POST /api/items/query, as noteboard answers it (noteboard model/items_query.go
	// and internal/db/items_query.go at 41d488b, measured live 2026-09-20): an
	// unknown field or sort is a 400; an item must carry ALL the tags and ANY of
	// the priorities and statuses; a sort's ties fall back to the order the ids
	// were sent in; the page is cut after the filter; total counts the matches;
	// missing_ids are the ids naming no live item, whatever the filter.
	mux.HandleFunc("POST /api/items/query", func(w http.ResponseWriter, r *http.Request) {
		var query struct {
			Q            string   `json:"q"`
			CreatedFrom  string   `json:"created_from"`
			CreatedTo    string   `json:"created_to"`
			IDs          []string `json:"ids"`
			Tags         []string `json:"tags"`
			Priorities   []int    `json:"priorities"`
			Statuses     []string `json:"statuses"`
			DueBefore    string   `json:"due_before"`
			Sort         string   `json:"sort"`
			Limit        int      `json:"limit"`
			Offset       int      `json:"offset"`
			IncludeItems bool     `json:"include_items"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&query); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		sorts := map[string]bool{"": true, "given": true, "priority": true, "due_at": true, "updated_at": true, "created_at": true, "updated_asc": true, "title": true}
		if !sorts[query.Sort] {
			writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("sort %q is not one of [given priority due_at updated_at created_at title] (GET /api/items/query-options)", query.Sort)})
			return
		}
		f.mu.Lock()
		f.queries++
		type match struct {
			position int
			item     map[string]any
		}
		matches := []match{}
		missing := []string{}
		for position, id := range query.IDs {
			item, held := f.items[id]
			if _, deleted := item["deleted_at"]; !held || deleted {
				missing = append(missing, id)
				continue
			}
			keep := true
			if query.Q != "" {
				keep = strings.Contains(strings.ToLower(fmt.Sprint(item["title"])+"\n"+fmt.Sprint(item["body"])+"\n"+id), strings.ToLower(query.Q))
			}
			created, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(item["created_at"]))
			if query.CreatedFrom != "" {
				bound, err := time.Parse(time.RFC3339Nano, query.CreatedFrom)
				keep = keep && err == nil && !created.Before(bound)
			}
			if query.CreatedTo != "" {
				bound, err := time.Parse(time.RFC3339Nano, query.CreatedTo)
				keep = keep && err == nil && !created.After(bound)
			}
			carried := map[string]bool{}
			if tags, ok := item["tags"].([]any); ok {
				for _, tag := range tags {
					carried[fmt.Sprint(tag)] = true
				}
			}
			for _, tag := range query.Tags {
				keep = keep && carried[tag]
			}
			priority := 0
			if number, ok := item["priority"].(float64); ok {
				priority = int(number)
			}
			if len(query.Priorities) > 0 {
				any := false
				for _, wanted := range query.Priorities {
					any = any || wanted == priority
				}
				keep = keep && any
			}
			if len(query.Statuses) > 0 {
				any := false
				for _, wanted := range query.Statuses {
					any = any || wanted == item["status"]
				}
				keep = keep && any
			}
			if keep {
				matches = append(matches, match{position, clone(item)})
			}
		}
		f.mu.Unlock()
		priorityOf := func(item map[string]any) float64 { number, _ := item["priority"].(float64); return number }
		sort.SliceStable(matches, func(i, j int) bool {
			switch query.Sort {
			case "updated_at", "created_at", "updated_asc":
				key := query.Sort
				if key == "updated_asc" {
					key = "updated_at"
				}
				left, right := fmt.Sprint(matches[i].item[key]), fmt.Sprint(matches[j].item[key])
				if left != right {
					if query.Sort == "updated_asc" {
						return left < right
					}
					return left > right
				}
			case "priority":
				if priorityOf(matches[i].item) != priorityOf(matches[j].item) {
					return priorityOf(matches[i].item) > priorityOf(matches[j].item)
				}
			case "title":
				left, right := strings.ToLower(fmt.Sprint(matches[i].item["title"])), strings.ToLower(fmt.Sprint(matches[j].item["title"]))
				if left != right {
					return left < right
				}
			}
			return matches[i].position < matches[j].position
		})
		total := len(matches)
		if query.Offset > len(matches) {
			query.Offset = len(matches)
		}
		matches = matches[query.Offset:]
		if query.Limit > 0 && query.Limit < len(matches) {
			matches = matches[:query.Limit]
		}
		answer := map[string]any{"total": total, "ids": []string{}, "missing_ids": missing}
		ids, items := []string{}, []map[string]any{}
		for _, kept := range matches {
			ids = append(ids, fmt.Sprint(kept.item["id"]))
			items = append(items, kept.item)
		}
		answer["ids"] = ids
		if query.IncludeItems {
			answer["items"] = items
		}
		writeJSON(w, 200, answer)
	})

	mux.HandleFunc("PATCH /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var patch map[string]any
		json.NewDecoder(r.Body).Decode(&patch)
		f.mu.Lock()
		if st := f.patchStatus; st != 0 {
			f.mu.Unlock()
			writeJSON(w, st, map[string]string{"error": "upstream refused the patch"})
			return
		}
		it, ok := f.items[id]
		var out map[string]any
		// If-Match, as noteboard reads it (noteboard internal/api/api.go,
		// expectedUpdatedAtFromIfMatch; measured on the live service 2026-09-18):
		// the quoted updated_at the patch was made against. A stored version that
		// differs is a 412 carrying the item as it is now, and nothing is written.
		if ifMatch := r.Header.Get("If-Match"); ok && ifMatch != "" && ifMatch != "*" {
			if stored, _ := it["updated_at"].(string); ifMatch != `"`+stored+`"` {
				current := clone(it)
				f.mu.Unlock()
				w.Header().Set("ETag", `"`+stored+`"`)
				writeJSON(w, 412, map[string]any{"error": "item " + id + " was changed after the version this update was made against; nothing was written", "current": current})
				return
			}
		}
		if ok {
			// Every applied PATCH moves updated_at, which is the item's version.
			it["updated_at"] = f.nextVersion()
			// noteboard decodes a PATCH into model.UpdateItemRequest NON-strictly:
			// a key that struct does not carry is dropped in silence and still
			// answered 200. Applying every key, as this fake used to, makes a
			// misspelled field look like it worked.
			for k, v := range patch {
				if noteboardUpdateFields[k] {
					it[k] = v
				}
			}
			out = clone(it)
		}
		f.mu.Unlock()
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, out)
	})

	// A reversible delete stamps deleted_at and leaves status ALONE, exactly as
	// noteboard does (internal/db/db.go DeleteItem, verified against a real
	// noteboard binary on 2026-08-10). The row stops being visible: a later GET
	// answers 404. This fake previously flipped status to "archived", which no
	// version of noteboard has ever done, and a green test asserted it.
	mux.HandleFunc("DELETE /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		hard := r.URL.Query().Get("hard") == "true"
		f.mu.Lock()
		it, ok := f.items[id]
		if ok {
			if hard {
				delete(f.items, id)
			} else {
				it["deleted_at"] = "2026-08-10T00:00:00Z"
			}
		}
		f.mu.Unlock()
		if !ok {
			// noteboard answers a delete of an id it cannot find with 404, like
			// its other missing-item routes (measured live 2026-10-05; it was a
			// 500 with the raw driver message until noteboard 23830ac).
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	// The hold gate. noteboard parks an item by stamping held_at and clears it
	// on unhold; kanban-store keeps no hold of its own, it forwards. So these
	// two endpoints are where the gate's two directions can be observed at all.
	mux.HandleFunc("POST /api/items/{id}/hold", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Reason string `json:"reason"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.setHold(w, r.PathValue("id"), true, req.Reason)
	})

	mux.HandleFunc("POST /api/items/{id}/unhold", func(w http.ResponseWriter, r *http.Request) {
		f.setHold(w, r.PathValue("id"), false, "")
	})

	mux.HandleFunc("GET /api/search", func(w http.ResponseWriter, r *http.Request) {
		q := strings.ToLower(r.URL.Query().Get("q"))
		if q == "" {
			writeJSON(w, 400, map[string]string{"error": "q parameter is required"})
			return
		}
		includeHeld := r.URL.Query().Get("include_held") == "true"
		f.mu.Lock()
		var out []map[string]any
		for _, it := range f.items {
			title, _ := it["title"].(string)
			if !strings.Contains(strings.ToLower(title), q) {
				continue
			}
			if _, deleted := it["deleted_at"]; deleted {
				continue
			}
			// Held items are withheld unless the caller asks for them — the board
			// asks, an agent's discovery query must not.
			if _, held := it["held_at"]; held && !includeHeld {
				continue
			}
			out = append(out, clone(it))
		}
		f.mu.Unlock()
		writeJSON(w, 200, out)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		failStatus := f.failStatus
		f.mu.Unlock()
		if failStatus != 0 {
			writeJSON(w, failStatus, map[string]string{"error": "noteboard is failing"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// heldAt returns the stored held_at for an item, or "" if it is not held. The
// hold lives on the noteboard item rather than on the board, so this is where
// holding has to be observed — the board carries nothing to assert against.
func (f *fakeNoteboard) heldAt(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it, ok := f.items[id]; ok {
		s, _ := it["held_at"].(string)
		return s
	}
	return ""
}

// holdReason returns the reason recorded with the hold, or "" if there is none.
func (f *fakeNoteboard) holdReason(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it, ok := f.items[id]; ok {
		s, _ := it["hold_reason"].(string)
		return s
	}
	return ""
}

// noteboardUpdateFields is the json tag set of noteboard's UpdateItemRequest,
// read from ~/repos/noteboard/model/model.go on 2026-08-10. A PATCH key outside
// this set is discarded by the real service without saying so.
var noteboardUpdateFields = map[string]bool{
	"title": true, "body": true, "tags": true, "priority": true, "rank": true,
	"status": true, "list_id": true, "due_at": true, "parent_id": true,
	"links": true, "schedule": true, "auto_hold_at_usd": true,
}

// status returns the stored status for an item, or "" if it does not exist.
// Used to assert auto_status was applied to the upstream noteboard item.
func (f *fakeNoteboard) status(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it, ok := f.items[id]; ok {
		s, _ := it["status"].(string)
		return s
	}
	return ""
}

// seedItem inserts an item directly, simulating a noteboard item that exists
// independently of kanban (used to test attaching an existing card).
func (f *fakeNoteboard) seedItem(title string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := "nb-" + itoa(f.seq)
	f.items[id] = map[string]any{"id": id, "title": title, "status": "open"}
	return id
}

func clone(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// setup builds an API backed by a fresh temp SQLite store, an in-memory fake
// noteboard and a stub principal-store (see assignments_test.go for what it
// knows). The returned cleanup tears all three down.
func setup(t *testing.T) (http.Handler, *fakeNoteboard, func()) {
	t.Helper()
	principals := httptest.NewServer(newFakePrincipalStore().handler())
	h, nb, cleanup := setupAgainstPrincipalStore(t, principals.URL)
	return h, nb, func() {
		cleanup()
		principals.Close()
	}
}

// setupAgainstPrincipalStore is setup with the principal-store URL chosen by
// the test — pointed at a closed port to exercise the unreachable path.
func setupAgainstPrincipalStore(t *testing.T, principalStoreURL string) (http.Handler, *fakeNoteboard, func()) {
	t.Helper()
	h, nb, _, cleanup := setupExposingStore(t, principalStoreURL)
	return h, nb, cleanup
}

// setupExposingStore also hands back the store itself, for the tests that need
// to build state no HTTP route will — a placement with no event behind it, the
// shape of every card that predates the event log.
func setupExposingStore(t *testing.T, principalStoreURL string) (http.Handler, *fakeNoteboard, *db.Store, func()) {
	t.Helper()
	bridge := httptest.NewServer(newFakeLLMBridgeServer().handler())
	t.Cleanup(bridge.Close)
	bundles := httptest.NewServer(newFakeBundleStore().handler())
	t.Cleanup(bundles.Close)
	return setupWithOwners(t, principalStoreURL, bridge.URL, bundles.URL)
}

// setupWithOwners is the whole harness with both owner URLs chosen by the
// test: principal-store for principals, llm-bridge-server for agents and
// instances. Point either at a closed port to exercise the unreachable path.
func setupWithOwners(t *testing.T, principalStoreURL, llmBridgeServerURL, bundleStoreURL string) (http.Handler, *fakeNoteboard, *db.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := db.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	nb := newFakeNoteboard()
	srv := httptest.NewServer(nb.handler())
	a := api.New(store, noteboard.New(srv.URL), principalstore.New(principalStoreURL), llmbridge.New(llmBridgeServerURL), bundlestore.New(bundleStoreURL), settingsRegistryForTests(t))
	a.SetReferenceStore(fakeReferenceStore(t))
	// Registered after t.TempDir, so it runs before the directory is removed —
	// and here rather than only in the returned cleanup, which some callers
	// discard.
	t.Cleanup(a.WaitForMessageTriggers)
	// Every request needs a credential now, so the shared harness acts as an
	// internal service: `do` below sends the service token. The per-principal
	// rules have their own harness in principal_access_test.go, which points
	// the grant-store client at a fake; a service-token request never reaches
	// grant-store, so an unused address is right here.
	a.SetPrincipalEnforcement(api.PrincipalEnforcement{
		ServiceToken: testServiceToken,
		Grants:       grantstore.New("http://127.0.0.1:1", ""),
	})
	return a.Handler(), nb, store, func() {
		a.WaitForMessageTriggers()
		srv.Close()
		store.Close()
	}
}

// do issues a request against the handler and returns the recorder. body may be
// nil (no request body) or any value (JSON-encoded).
func do(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set(api.ServiceTokenHeader, testServiceToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// decodeSuccessfulResponse reads a response body the server answered successfully.
//
// The status check is the point of it. A body is only a result once the status
// says so; an error body is JSON too, and decoding {"error":"..."} into a struct
// succeeds and leaves a zero value. The test then fails several assertions later
// on a missing field or a short slice and names the wrong subsystem -- or, when
// the zero value carries a nil pointer, panics and takes every other test in the
// package down with it. Failing here instead reports the request that actually
// failed, with its status and body.
func decodeSuccessfulResponse(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if w.Code < 200 || w.Code > 299 {
		t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
}

// mkBoard creates a board and returns its id.
func mkBoard(t *testing.T, h http.Handler, name string) string {
	t.Helper()
	w := do(t, h, "POST", "/api/boards", model.CreateBoardRequest{Name: name})
	if w.Code != 201 {
		t.Fatalf("create board: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var b model.Board
	decodeSuccessfulResponse(t, w, &b)
	return b.ID
}

// mkColumn creates a column on a board and returns its id. autoStatus "" leaves
// it unset.
func mkColumn(t *testing.T, h http.Handler, boardID, name, autoStatus string) string {
	t.Helper()
	req := model.CreateColumnRequest{Name: name}
	if autoStatus != "" {
		req.AutoStatus = &autoStatus
	}
	w := do(t, h, "POST", "/api/boards/"+boardID+"/columns", req)
	if w.Code != 201 {
		t.Fatalf("create column: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var c model.Column
	decodeSuccessfulResponse(t, w, &c)
	return c.ID
}

// ============================ Health ============================

func TestHealthEndpoint(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	w := do(t, h, "GET", "/health", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	decodeSuccessfulResponse(t, w, &resp)
	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
}

// ============================ Boards ============================

func TestBoardCRUD(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	// Create
	desc := "my board"
	w := do(t, h, "POST", "/api/boards", model.CreateBoardRequest{Name: "Board A", Description: &desc})
	if w.Code != 201 {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var b model.Board
	decodeSuccessfulResponse(t, w, &b)
	if b.Name != "Board A" || b.Description != "my board" {
		t.Fatalf("unexpected board: %+v", b)
	}

	// Get
	w = do(t, h, "GET", "/api/boards/"+b.ID, nil)
	if w.Code != 200 {
		t.Fatalf("get: expected 200, got %d", w.Code)
	}

	// List
	w = do(t, h, "GET", "/api/boards", nil)
	if w.Code != 200 {
		t.Fatalf("list: expected 200, got %d", w.Code)
	}
	var boards []model.Board
	decodeSuccessfulResponse(t, w, &boards)
	if len(boards) != 1 {
		t.Fatalf("expected 1 board, got %d", len(boards))
	}

	// Patch (rename + archive)
	newName := "Board A2"
	archived := true
	w = do(t, h, "PATCH", "/api/boards/"+b.ID, model.UpdateBoardRequest{Name: &newName, Archived: &archived})
	if w.Code != 200 {
		t.Fatalf("patch: expected 200, got %d", w.Code)
	}
	var updated model.Board
	decodeSuccessfulResponse(t, w, &updated)
	if updated.Name != "Board A2" || !updated.Archived {
		t.Fatalf("patch did not apply: %+v", updated)
	}

	// Archived board is excluded from the default list but included with include_archived.
	w = do(t, h, "GET", "/api/boards", nil)
	decodeSuccessfulResponse(t, w, &boards)
	if len(boards) != 0 {
		t.Fatalf("archived board should be hidden, got %d", len(boards))
	}
	w = do(t, h, "GET", "/api/boards?include_archived=true", nil)
	decodeSuccessfulResponse(t, w, &boards)
	if len(boards) != 1 {
		t.Fatalf("include_archived should show board, got %d", len(boards))
	}

	// Delete
	w = do(t, h, "DELETE", "/api/boards/"+b.ID, nil)
	if w.Code != 200 {
		t.Fatalf("delete: expected 200, got %d", w.Code)
	}
	w = do(t, h, "GET", "/api/boards/"+b.ID, nil)
	if w.Code != 404 {
		t.Fatalf("get after delete: expected 404, got %d", w.Code)
	}
}

func TestBoardErrors(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	// Missing name → 400
	if w := do(t, h, "POST", "/api/boards", model.CreateBoardRequest{}); w.Code != 400 {
		t.Fatalf("empty name: expected 400, got %d", w.Code)
	}
	// Invalid JSON → 400
	req := httptest.NewRequest("POST", "/api/boards", strings.NewReader("{not json"))
	req.Header.Set(api.ServiceTokenHeader, testServiceToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("bad json: expected 400, got %d", w.Code)
	}
	// Unknown board → 404
	if w := do(t, h, "GET", "/api/boards/nope", nil); w.Code != 404 {
		t.Fatalf("unknown board: expected 404, got %d", w.Code)
	}
	// Unsupported method → 405
	if w := do(t, h, "DELETE", "/api/boards", nil); w.Code != 405 {
		t.Fatalf("delete collection: expected 405, got %d", w.Code)
	}
}

// ============================ Columns ============================

func TestColumnCRUD(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")

	// Create
	w := do(t, h, "POST", "/api/boards/"+boardID+"/columns", model.CreateColumnRequest{Name: "Todo"})
	if w.Code != 201 {
		t.Fatalf("create column: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var c model.Column
	decodeSuccessfulResponse(t, w, &c)
	if c.Name != "Todo" || c.BoardID != boardID {
		t.Fatalf("unexpected column: %+v", c)
	}

	// List
	w = do(t, h, "GET", "/api/boards/"+boardID+"/columns", nil)
	var cols []model.Column
	decodeSuccessfulResponse(t, w, &cols)
	if len(cols) != 1 {
		t.Fatalf("expected 1 column, got %d", len(cols))
	}

	// Get
	if w := do(t, h, "GET", "/api/columns/"+c.ID, nil); w.Code != 200 {
		t.Fatalf("get column: expected 200, got %d", w.Code)
	}

	// Patch
	name := "In Progress"
	auto := "open"
	w = do(t, h, "PATCH", "/api/columns/"+c.ID, model.UpdateColumnRequest{Name: &name, AutoStatus: &auto})
	if w.Code != 200 {
		t.Fatalf("patch column: expected 200, got %d", w.Code)
	}
	decodeSuccessfulResponse(t, w, &c)
	if c.Name != "In Progress" || c.AutoStatus == nil || *c.AutoStatus != "open" {
		t.Fatalf("patch did not apply: %+v", c)
	}

	// Delete
	if w := do(t, h, "DELETE", "/api/columns/"+c.ID, nil); w.Code != 200 {
		t.Fatalf("delete column: expected 200, got %d", w.Code)
	}
	if w := do(t, h, "GET", "/api/columns/"+c.ID, nil); w.Code != 404 {
		t.Fatalf("get after delete: expected 404, got %d", w.Code)
	}
}

func TestColumnErrors(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")

	// Missing name → 400
	if w := do(t, h, "POST", "/api/boards/"+boardID+"/columns", model.CreateColumnRequest{}); w.Code != 400 {
		t.Fatalf("empty name: expected 400, got %d", w.Code)
	}
	// Invalid auto_status at create → 400
	bad := "nonsense"
	if w := do(t, h, "POST", "/api/boards/"+boardID+"/columns", model.CreateColumnRequest{Name: "X", AutoStatus: &bad}); w.Code != 400 {
		t.Fatalf("bad auto_status create: expected 400, got %d", w.Code)
	}
	// Invalid auto_status at patch → 400
	colID := mkColumn(t, h, boardID, "C", "")
	if w := do(t, h, "PATCH", "/api/columns/"+colID, model.UpdateColumnRequest{AutoStatus: &bad}); w.Code != 400 {
		t.Fatalf("bad auto_status patch: expected 400, got %d", w.Code)
	}
	// Column on a missing board → 404 (board lookup fails in store).
	if w := do(t, h, "POST", "/api/boards/missing/columns", model.CreateColumnRequest{Name: "X"}); w.Code != 404 {
		t.Fatalf("column on missing board: expected 404, got %d", w.Code)
	}
}

func TestReorderColumns(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")
	c1 := mkColumn(t, h, boardID, "A", "")
	c2 := mkColumn(t, h, boardID, "B", "")
	c3 := mkColumn(t, h, boardID, "C", "")

	// Reverse the order via explicit positions.
	body := map[string]any{"columns": []map[string]any{
		{"id": c1, "position": 3.0},
		{"id": c2, "position": 2.0},
		{"id": c3, "position": 1.0},
	}}
	w := do(t, h, "POST", "/api/boards/"+boardID+"/columns/reorder", body)
	if w.Code != 200 {
		t.Fatalf("reorder: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var cols []model.Column
	decodeSuccessfulResponse(t, w, &cols)
	if len(cols) != 3 {
		t.Fatalf("expected 3 columns, got %d", len(cols))
	}
	// ListColumns orders by position ASC → C, B, A.
	if cols[0].ID != c3 || cols[1].ID != c2 || cols[2].ID != c1 {
		t.Fatalf("unexpected order: %s, %s, %s", cols[0].Name, cols[1].Name, cols[2].Name)
	}
}

// ============================ Cards ============================

func TestCardCreateAndDelete(t *testing.T) {
	h, nb, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")
	colID := mkColumn(t, h, boardID, "Todo", "")

	// Create a card → creates a noteboard item, then attaches it.
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{
		Title: "First card", ColumnID: colID, Tags: []string{"x"},
	})
	if w.Code != 201 {
		t.Fatalf("create card: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)
	if cv.Placement == nil || cv.Placement.ColumnID != colID {
		t.Fatalf("placement not set: %+v", cv.Placement)
	}
	cardID := cv.Placement.CardID
	if nb.status(cardID) != "open" {
		t.Fatalf("expected noteboard status open, got %q", nb.status(cardID))
	}

	// Board view should now contain the card in its column.
	w = do(t, h, "GET", "/api/boards/"+boardID+"/cards", nil)
	if w.Code != 200 {
		t.Fatalf("board view: expected 200, got %d", w.Code)
	}
	var view model.BoardView
	decodeSuccessfulResponse(t, w, &view)
	if len(view.Columns) != 1 || len(view.Columns[0].Cards) != 1 {
		t.Fatalf("expected 1 card in 1 column, got %+v", view.Columns)
	}

	// A reversible delete of the card. This assertion used to read "→ noteboard
	// item flips to archived", and it passed, because the fake it ran against
	// had been written from this repo's comments instead of from noteboard's
	// routes. noteboard stamps deleted_at and never touches status.
	if w := do(t, h, "DELETE", "/api/cards/"+cardID, nil); w.Code != 200 {
		t.Fatalf("delete card: expected 200, got %d", w.Code)
	}
	if got := nb.status(cardID); got != "open" {
		t.Fatalf("a reversible delete must leave status alone, got %q — deleting is the "+
			"item being taken away, archiving is a state the user chose for a live "+
			"item, and a restore has to be able to tell them apart", got)
	}

	// And this is what a soft delete actually does to the board: the placement
	// survives, the item read 404s, so the card leaves its column and lands in
	// the orphan bucket. Nothing hides it as an archived card, because nothing
	// ever marks it archived.
	w = do(t, h, "GET", "/api/boards/"+boardID+"/cards", nil)
	if w.Code != 200 {
		t.Fatalf("board view: expected 200, got %d", w.Code)
	}
	view = model.BoardView{}
	decodeSuccessfulResponse(t, w, &view)
	if len(view.Columns) != 1 || len(view.Columns[0].Cards) != 0 {
		t.Fatalf("deleted card must leave its column, got %+v", view.Columns)
	}
	if len(view.Orphans) != 1 || view.Orphans[0].Placement.CardID != cardID {
		t.Fatalf("deleted card must surface as an orphan placement, got %+v", view.Orphans)
	}
}

// TestCardCreateAutoStatus verifies creating a card directly into a column with
// auto_status applies that status to the noteboard item (symmetry with move).
func TestCardCreateAutoStatus(t *testing.T) {
	h, nb, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")
	doneCol := mkColumn(t, h, boardID, "Done", "done")

	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{
		Title: "Born done", ColumnID: doneCol,
	})
	if w.Code != 201 {
		t.Fatalf("create card: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)
	if got := nb.status(cv.Placement.CardID); got != "done" {
		t.Fatalf("expected auto_status done on create, got %q", got)
	}
}

// TestCardMoveAutoStatus covers moving a card across columns and the auto_status
// PATCH to noteboard that a move into an auto_status column triggers.
func TestCardMoveAutoStatus(t *testing.T) {
	h, nb, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")
	todo := mkColumn(t, h, boardID, "Todo", "")
	done := mkColumn(t, h, boardID, "Done", "done")

	// Create a card in Todo.
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "Move me", ColumnID: todo})
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)
	cardID := cv.Placement.CardID
	if nb.status(cardID) != "open" {
		t.Fatalf("precondition: expected open, got %q", nb.status(cardID))
	}

	// Move it into Done.
	w = do(t, h, "POST", "/api/cards/"+cardID+"/move", model.MoveCardRequest{
		BoardID: boardID, ColumnID: done, Position: 1.0,
	})
	if w.Code != 200 {
		t.Fatalf("move: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	decodeSuccessfulResponse(t, w, &resp)
	if resp["auto_status_applied"] != "done" {
		t.Fatalf("expected auto_status_applied=done, got %v", resp["auto_status_applied"])
	}
	if nb.status(cardID) != "done" {
		t.Fatalf("expected noteboard status done after move, got %q", nb.status(cardID))
	}

	// Placements lookup should reflect the new column.
	w = do(t, h, "GET", "/api/cards/"+cardID+"/placements", nil)
	var ps []model.Placement
	decodeSuccessfulResponse(t, w, &ps)
	if len(ps) != 1 || ps[0].ColumnID != done {
		t.Fatalf("placement not moved: %+v", ps)
	}
}

func TestAttachExistingCard(t *testing.T) {
	h, nb, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")
	colID := mkColumn(t, h, boardID, "Todo", "")

	// A noteboard item that already exists, independent of kanban.
	existing := nb.seedItem("Pre-existing item")
	w := do(t, h, "PUT", "/api/boards/"+boardID+"/cards/"+existing, model.AttachCardRequest{ColumnID: colID})
	if w.Code != 201 {
		t.Fatalf("attach: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Attaching an item noteboard doesn't know about → 404.
	w = do(t, h, "PUT", "/api/boards/"+boardID+"/cards/ghost", model.AttachCardRequest{ColumnID: colID})
	if w.Code != 404 {
		t.Fatalf("attach unknown item: expected 404, got %d", w.Code)
	}

	// Detach.
	if w := do(t, h, "DELETE", "/api/boards/"+boardID+"/cards/"+existing, nil); w.Code != 200 {
		t.Fatalf("detach: expected 200, got %d", w.Code)
	}
}

func TestWIPLimit(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")

	// Column with wip_limit 1.
	limit := 1
	w := do(t, h, "POST", "/api/boards/"+boardID+"/columns", model.CreateColumnRequest{Name: "Tight", WIPLimit: &limit})
	var c model.Column
	decodeSuccessfulResponse(t, w, &c)

	// First card fits.
	if w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "one", ColumnID: c.ID}); w.Code != 201 {
		t.Fatalf("first card: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	// Second card exceeds the WIP limit → 409.
	if w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "two", ColumnID: c.ID}); w.Code != 409 {
		t.Fatalf("second card: expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCardErrors(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")
	colID := mkColumn(t, h, boardID, "Todo", "")

	// Missing title → 400.
	if w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{ColumnID: colID}); w.Code != 400 {
		t.Fatalf("missing title: expected 400, got %d", w.Code)
	}
	// Missing column_id → 400.
	if w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "x"}); w.Code != 400 {
		t.Fatalf("missing column_id: expected 400, got %d", w.Code)
	}
	// Column belonging to another board → 400.
	other := mkBoard(t, h, "Other")
	otherCol := mkColumn(t, h, other, "Elsewhere", "")
	if w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "x", ColumnID: otherCol}); w.Code != 400 {
		t.Fatalf("cross-board column: expected 400, got %d", w.Code)
	}
	// Move with missing board_id → 400.
	if w := do(t, h, "POST", "/api/cards/whatever/move", model.MoveCardRequest{ColumnID: colID, Position: 1}); w.Code != 400 {
		t.Fatalf("move missing board_id: expected 400, got %d", w.Code)
	}
	// Move a card with no placement → 404.
	if w := do(t, h, "POST", "/api/cards/ghost/move", model.MoveCardRequest{BoardID: boardID, ColumnID: colID, Position: 1}); w.Code != 404 {
		t.Fatalf("move nonexistent placement: expected 404, got %d", w.Code)
	}
}

// ============================ Card links ============================

func TestCardLinks(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")
	colID := mkColumn(t, h, boardID, "Todo", "")
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "linked", ColumnID: colID})
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)
	cardID := cv.Placement.CardID

	// Create a link.
	label := "primary session"
	w = do(t, h, "POST", "/api/cards/"+cardID+"/links", model.CreateCardLinkRequest{
		EntityType: "session", EntityRef: "sess-123", Label: &label,
	})
	if w.Code != 201 {
		t.Fatalf("create link: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var link model.CardLink
	decodeSuccessfulResponse(t, w, &link)
	if link.EntityType != "session" || link.EntityRef != "sess-123" {
		t.Fatalf("unexpected link: %+v", link)
	}

	// List links.
	w = do(t, h, "GET", "/api/cards/"+cardID+"/links", nil)
	var links []model.CardLink
	decodeSuccessfulResponse(t, w, &links)
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(links))
	}

	// Reverse lookup: cards by entity.
	w = do(t, h, "GET", "/api/entities/session/sess-123/cards", nil)
	if w.Code != 200 {
		t.Fatalf("entity cards: expected 200, got %d", w.Code)
	}
	var ec []map[string]any
	decodeSuccessfulResponse(t, w, &ec)
	if len(ec) != 1 || ec[0]["card_id"] != cardID {
		t.Fatalf("expected the linked card, got %+v", ec)
	}

	// Missing entity_type → 400.
	if w := do(t, h, "POST", "/api/cards/"+cardID+"/links", model.CreateCardLinkRequest{EntityRef: "x"}); w.Code != 400 {
		t.Fatalf("missing entity_type: expected 400, got %d", w.Code)
	}

	// Delete the link.
	if w := do(t, h, "DELETE", "/api/links/"+link.ID, nil); w.Code != 200 {
		t.Fatalf("delete link: expected 200, got %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/links/"+link.ID, nil); w.Code != 404 {
		t.Fatalf("delete missing link: expected 404, got %d", w.Code)
	}
}

// One entity can be linked to several cards — an agent session picks up a
// dispatch card and then has its work classified onto more. A caller that has
// to name a single card ("which todo is this session for?") reads the first
// one, so the route must hand back the store's order untouched.
//
// The store-level guarantee (oldest link first, whatever order the rows were
// written in) is pinned in internal/db; this checks the route does not
// re-sort on the way out.
func TestEntityCardsAreOldestLinkFirst(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")
	colID := mkColumn(t, h, boardID, "Todo", "")

	const sessionRef = "sess-ordered"
	var wantOrder []string
	for _, title := range []string{"dispatch", "classified once", "classified twice"} {
		w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: title, ColumnID: colID})
		var cv model.CardView
		decodeSuccessfulResponse(t, w, &cv)
		cardID := cv.Placement.CardID
		wantOrder = append(wantOrder, cardID)
		if w := do(t, h, "POST", "/api/cards/"+cardID+"/links", model.CreateCardLinkRequest{
			EntityType: "session", EntityRef: sessionRef,
		}); w.Code != 201 {
			t.Fatalf("link %q: expected 201, got %d: %s", title, w.Code, w.Body.String())
		}
	}

	w := do(t, h, "GET", "/api/entities/session/"+sessionRef+"/cards", nil)
	if w.Code != 200 {
		t.Fatalf("entity cards: expected 200, got %d", w.Code)
	}
	var got []map[string]any
	decodeSuccessfulResponse(t, w, &got)
	if len(got) != len(wantOrder) {
		t.Fatalf("expected %d cards, got %d: %+v", len(wantOrder), len(got), got)
	}
	for i, want := range wantOrder {
		if got[i]["card_id"] != want {
			t.Errorf("card %d = %v, want %v (order must be oldest link first)", i, got[i]["card_id"], want)
		}
	}
}

// ============================ Search ============================

func TestSearch(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")
	colID := mkColumn(t, h, boardID, "Todo", "")
	if w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "Findable card", ColumnID: colID}); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	// Plain search delegates to noteboard.
	w := do(t, h, "GET", "/api/search?q=Findable", nil)
	if w.Code != 200 {
		t.Fatalf("search: expected 200, got %d", w.Code)
	}
	var items []map[string]any
	decodeSuccessfulResponse(t, w, &items)
	if len(items) != 1 {
		t.Fatalf("expected 1 search result, got %d", len(items))
	}

	// on_board=true keeps only items with a placement (the created card qualifies).
	w = do(t, h, "GET", "/api/search?q=Findable&on_board=true", nil)
	decodeSuccessfulResponse(t, w, &items)
	if len(items) != 1 {
		t.Fatalf("on_board search: expected 1, got %d", len(items))
	}

	// Missing q → 400.
	if w := do(t, h, "GET", "/api/search", nil); w.Code != 400 {
		t.Fatalf("missing q: expected 400, got %d", w.Code)
	}
}

// A card created straight into a column with auto_status carries TWO writes: the
// noteboard create, then a PATCH that makes the item's status match the column.
// The second one can fail on its own, and when it does the card exists in
// noteboard reading "open" while sitting in a Done column — which is precisely
// the state the auto_status write exists to prevent.
//
// moveCard already reports that failure as auto_status_error. createCard used to
// drop it: the `if perr == nil` guard had no else, so the response was a plain
// 201 carrying the pre-patch item. status "open" is also exactly what a column
// with no auto_status returns, so the failure produced a value indistinguishable
// from a legitimate answer, and noteboard is the source of truth that agents
// read to decide what work is still open.
func TestCardCreateReportsAFailedAutoStatus(t *testing.T) {
	h, nb, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Board")
	colID := mkColumn(t, h, boardID, "Done", "done")

	nb.patchStatus = 503

	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{
		Title: "lands in Done", ColumnID: colID,
	})
	if w.Code != 201 {
		t.Fatalf("create card: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)

	if cv.AutoStatusError == "" {
		t.Fatalf("a failed auto_status write must be reported: the card is in a column "+
			"whose auto_status is %q and its noteboard item still reads %q, and the "+
			"response says nothing — got %s", "done", nb.status(cv.Placement.CardID),
			w.Body.String())
	}
	if cv.AutoStatusApplied != "" {
		t.Errorf("auto_status was not applied, so it must not be reported as applied: %q",
			cv.AutoStatusApplied)
	}
}

// The other side of the same contract: when the write succeeds, say so, and say
// it with the same field moveCard uses.
func TestCardCreateReportsAnAppliedAutoStatus(t *testing.T) {
	h, nb, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Board")
	colID := mkColumn(t, h, boardID, "Done", "done")

	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{
		Title: "lands in Done", ColumnID: colID,
	})
	if w.Code != 201 {
		t.Fatalf("create card: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)

	if cv.AutoStatusApplied != "done" {
		t.Errorf("auto_status_applied: got %q, want done", cv.AutoStatusApplied)
	}
	if cv.AutoStatusError != "" {
		t.Errorf("auto_status succeeded, so no error must be reported: %q", cv.AutoStatusError)
	}
	if got := nb.status(cv.Placement.CardID); got != "done" {
		t.Errorf("noteboard status: got %q, want done", got)
	}
}

// A column with no auto_status must stay silent on both fields — otherwise the
// "applied" field cannot be read as evidence that anything happened.
func TestCardCreateWithoutAutoStatusReportsNeither(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Board")
	colID := mkColumn(t, h, boardID, "Todo", "")

	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{
		Title: "plain", ColumnID: colID,
	})
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)
	if cv.AutoStatusApplied != "" || cv.AutoStatusError != "" {
		t.Errorf("no auto_status column must report neither field, got applied=%q error=%q",
			cv.AutoStatusApplied, cv.AutoStatusError)
	}
}
