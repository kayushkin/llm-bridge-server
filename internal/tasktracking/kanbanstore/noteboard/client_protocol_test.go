package noteboard

// Does this client speak the protocol noteboard actually serves?
//
// Every function in this package is a write or a read against another repo's
// HTTP surface, and until now the package had no test file at all. Coverage
// reports 0.0% and a mutation harness reports UNNOTICED for all of it, because
// a wrong verb or a misspelled query parameter reddens no test that never
// reaches the client. Neither instrument can see the only defect that matters
// here: a *disagreement* between what this client sends and what noteboard
// routes.
//
// So the assertions below are not written from this client's comments. They are
// written from noteboard's own source, and every one was re-checked against a
// real noteboard binary (`cmd/noteboard`) running on a throwaway database on
// 2026-08-10. Where the two disagreed, the disagreement is named in the test
// rather than smoothed over.
//
// Sources, both in ~/repos/noteboard:
//
//	internal/api/api.go   routes, methods, status codes
//	model/model.go        the request structs, which decode NON-STRICTLY
//
// The non-strict decode is why the field-name cases exist. noteboard drops a
// key its request struct does not carry and still answers 200, so a renamed or
// misspelled field is silently discarded and this client cannot tell.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// recorder captures one request's wire form so a test can judge it against
// noteboard's routes. Everything the server decides is settable per case.
type recorder struct {
	method      string
	path        string
	rawQuery    string
	contentType string
	body        []byte
	bodyRead    bool

	status int
	reply  string
}

func (r *recorder) server(t *testing.T) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.method = req.Method
		r.path = req.URL.Path
		r.rawQuery = req.URL.RawQuery
		r.contentType = req.Header.Get("Content-Type")
		b, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		r.body = b
		r.bodyRead = true
		status := r.status
		if status == 0 {
			status = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, r.reply)
	}))
	return New(srv.URL), srv.Close
}

func (r *recorder) decodedBody(t *testing.T) map[string]any {
	t.Helper()
	if len(r.body) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("request body is not a JSON object: %v — %s", err, string(r.body))
	}
	return out
}

// ---------------------------------------------------------------------------
// The other repo's accepted field names.
//
// Measured 2026-08-10 from ~/repos/noteboard/model/model.go. A key this client
// sends that is absent here is dropped by noteboard in silence and answered
// 200 — so these two sets are the whole point of the field-name cases below.
// If noteboard renames a field, these tests go red and that is the intent.
// ---------------------------------------------------------------------------

var noteboardCreateFields = map[string]bool{
	"type": true, "title": true, "body": true, "tags": true, "priority": true,
	"rank": true, "status": true, "list_id": true, "due_at": true,
	"parent_id": true, "links": true, "schedule": true, "created_by": true,
	"hold": true, "hold_reason": true, "auto_hold_at_usd": true,
}

var noteboardUpdateFields = map[string]bool{
	"title": true, "body": true, "tags": true, "priority": true, "rank": true,
	"status": true, "list_id": true, "due_at": true, "parent_id": true,
	"links": true, "schedule": true, "auto_hold_at_usd": true,
}

// ============================ CreateItem ============================

func TestCreateItemSpeaksTheCreateRoute(t *testing.T) {
	rec := &recorder{status: 201, reply: `{"id":"nb-1","status":"open"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	body := "b"
	prio := 2
	list := "L"
	due := "2026-08-10T09:00:00Z"
	parent := "p-1"
	ceiling := 5.0
	item, err := c.CreateItem(CreateItemPayload{
		Type: "todo", Title: "t", Body: &body, Tags: []string{"x"},
		Priority: &prio, ListID: &list, DueAt: &due, ParentID: &parent,
		Hold: true, HoldReason: "why", AutoHoldAtUSD: &ceiling,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if item["id"] != "nb-1" {
		t.Fatalf("item not passed through: %+v", item)
	}
	if rec.method != "POST" {
		t.Errorf("method: got %q, want POST", rec.method)
	}
	if rec.path != "/api/items" {
		t.Errorf("path: got %q, want /api/items", rec.path)
	}
	if rec.rawQuery != "" {
		t.Errorf("create must send no query string, got %q", rec.rawQuery)
	}
	if rec.contentType != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", rec.contentType)
	}
}

// noteboard answers 201 to a create, not 200. A client that accepted only 200
// would fail *after* the item was written, and a retry would write a second
// copy of the user's card.
func TestCreateItemRequiresTwoOhOne(t *testing.T) {
	rec := &recorder{status: 200, reply: `{"id":"nb-1"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	if _, err := c.CreateItem(CreateItemPayload{Type: "todo", Title: "t"}); err == nil {
		t.Fatal("a 200 from the create route must be an error: noteboard answers 201, " +
			"so a 200 means something other than noteboard replied")
	}
}

// Every key this client puts on the wire must be a key noteboard's
// CreateItemRequest carries. A key outside that set is dropped in silence.
func TestCreateItemSendsOnlyFieldsNoteboardAccepts(t *testing.T) {
	rec := &recorder{status: 201, reply: `{"id":"nb-1"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	body := "b"
	prio := 2
	list := "L"
	due := "2026-08-10T09:00:00Z"
	parent := "p-1"
	ceiling := 5.0
	if _, err := c.CreateItem(CreateItemPayload{
		Type: "todo", Title: "t", Body: &body, Tags: []string{"x"},
		Priority: &prio, ListID: &list, DueAt: &due, ParentID: &parent,
		Hold: true, HoldReason: "why", AutoHoldAtUSD: &ceiling,
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	for k := range rec.decodedBody(t) {
		if !noteboardCreateFields[k] {
			t.Errorf("field %q is not on noteboard's CreateItemRequest — it is dropped "+
				"in silence and answered 201, so this write does nothing", k)
		}
	}
}

// The hold gate and the spend ceiling are the two fields whose silent loss
// costs the most: a card meant to be created already parked would be created
// live, and a card meant to carry a dollar ceiling would carry none. Both are
// omitempty, so this pins that a set value actually reaches the wire.
func TestCreateItemPutsHoldAndCeilingOnTheWire(t *testing.T) {
	rec := &recorder{status: 201, reply: `{"id":"nb-1"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	ceiling := 0.0 // zero is a real instruction: stop before spending a cent
	if _, err := c.CreateItem(CreateItemPayload{
		Type: "todo", Title: "t", Hold: true, HoldReason: "sensitive",
		AutoHoldAtUSD: &ceiling,
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	got := rec.decodedBody(t)
	if got["hold"] != true {
		t.Errorf("hold missing from the wire: %s", string(rec.body))
	}
	if got["hold_reason"] != "sensitive" {
		t.Errorf("hold_reason missing from the wire: %s", string(rec.body))
	}
	v, ok := got["auto_hold_at_usd"]
	if !ok {
		t.Errorf("auto_hold_at_usd=0 was omitted — nil (no ceiling) and 0 (spend nothing) "+
			"are different instructions and must not collapse: %s", string(rec.body))
	} else if v != float64(0) {
		t.Errorf("auto_hold_at_usd: got %v, want 0", v)
	}
}

// DueAt is typed *string here and *time.Time on noteboard's side. A date-only
// string is therefore not a bad field, it is a whole request noteboard refuses
// to decode: measured, `{"due_at":"2026-08-10"}` answers 400 "invalid JSON".
// Pinned so the RFC3339 requirement is discoverable from this repo.
func TestCreateItemDueAtMustBeRFC3339(t *testing.T) {
	rec := &recorder{status: 400, reply: `{"error":"invalid JSON"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	due := "2026-08-10"
	_, err := c.CreateItem(CreateItemPayload{Type: "todo", Title: "t", DueAt: &due})
	if err == nil {
		t.Fatal("a 400 must surface as an error")
	}
	if !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("the server's reason must reach the caller, got: %v", err)
	}
}

// ============================ GetItem ============================

func TestGetItemSpeaksTheItemRoute(t *testing.T) {
	rec := &recorder{status: 200, reply: `{"id":"abc","status":"open"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	if _, err := c.GetItem("abc"); err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if rec.method != "GET" {
		t.Errorf("method: got %q, want GET", rec.method)
	}
	if rec.path != "/api/items/abc" {
		t.Errorf("path: got %q, want /api/items/abc", rec.path)
	}
	if len(rec.body) != 0 {
		t.Errorf("GET must send no body, got %q", string(rec.body))
	}
}

// noteboard 404s a missing item AND a soft-deleted one — a deleted row drops
// out of every read path. So notFound here means "not visible", which is not
// the same as "never existed"; GetItems turns it into a nil entry and the
// board reports that placement as an orphan.
func TestGetItemNotFoundIsTypedNotJustAnError(t *testing.T) {
	rec := &recorder{status: 404, reply: `{"error":"not found"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	_, err := c.GetItem("gone")
	if err == nil {
		t.Fatal("404 must be an error")
	}
	if !isNotFound(err) {
		t.Fatalf("404 must be a *notFoundError so GetItems can tell an orphan from a "+
			"real failure, got %T: %v", err, err)
	}
	var nf *notFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("errors.As must reach it, got %T", err)
	}
	if nf.id != "gone" {
		t.Errorf("the error must name the item id, got %q — a caller reading this "+
			"message needs the id, not the route", nf.id)
	}
	// The rendered message is what reaches a log or a 502 body, so it is part of
	// the contract too. It used to render the route ("/api/items/gone"), which
	// sends a reader looking for a routing fault instead of a missing card.
	if got := err.Error(); got != "noteboard item not found: gone" {
		t.Errorf("error message: got %q, want %q", got, "noteboard item not found: gone")
	}
}

func TestGetItemEscapesTheIDIntoThePath(t *testing.T) {
	rec := &recorder{status: 200, reply: `{"id":"x"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	if _, err := c.GetItem("a b"); err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	// The server decodes before routing, so the path it sees is the raw id.
	if rec.path != "/api/items/a b" {
		t.Errorf("path: got %q, want /api/items/a b", rec.path)
	}
}

// ============================ PatchItem ============================

func TestPatchItemSpeaksTheItemRoute(t *testing.T) {
	rec := &recorder{status: 200, reply: `{"id":"abc","status":"done"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	if _, err := c.PatchItem("abc", map[string]any{"status": "done"}); err != nil {
		t.Fatalf("PatchItem: %v", err)
	}
	if rec.method != "PATCH" {
		t.Errorf("method: got %q, want PATCH", rec.method)
	}
	if rec.path != "/api/items/abc" {
		t.Errorf("path: got %q, want /api/items/abc", rec.path)
	}
	if rec.contentType != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", rec.contentType)
	}
	if got := rec.decodedBody(t); got["status"] != "done" {
		t.Errorf("patch not forwarded verbatim: %s", string(rec.body))
	}
}

// This client forwards a caller's map unchanged, which is correct — layers are
// transparent. The cost lands on the caller, and it is worth stating once:
// noteboard decodes a PATCH non-strictly, so a key outside UpdateItemRequest is
// dropped and still answered 200. Nothing on this path can report that. The
// only keys kanban-store itself patches are checked here.
func TestPatchItemKeysKanbanStoreSendsAreAcceptedByNoteboard(t *testing.T) {
	// kanban-store patches exactly one key of its own: auto_status writes
	// "status" (internal/api/api.go, createCard and moveCard). Everything else
	// on this path is a caller's own JSON, forwarded whole.
	for _, key := range []string{"status"} {
		if !noteboardUpdateFields[key] {
			t.Errorf("kanban-store patches %q, which noteboard's UpdateItemRequest "+
				"does not carry — the write is dropped and answered 200", key)
		}
	}
}

// A whole-item round trip is the expensive mistake on this route: noteboard's
// revision snapshot covers 8 of 20 columns and NOT rank, due_at or schedule, so
// sending an item back where you meant to send one field destroys an rrule with
// no revision holding it. kanban-store does not do this today. Pinned so that
// staying true is checkable rather than assumed.
func TestPatchItemIsUsedAsAPartialUpdate(t *testing.T) {
	rec := &recorder{status: 200, reply: `{"id":"abc"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	if _, err := c.PatchItem("abc", map[string]any{"status": "done"}); err != nil {
		t.Fatalf("PatchItem: %v", err)
	}
	got := rec.decodedBody(t)
	if len(got) != 1 {
		t.Errorf("a status patch must put exactly one key on the wire, got %d: %s",
			len(got), string(rec.body))
	}
	for _, unsnapshotted := range []string{"rank", "due_at", "schedule"} {
		if _, present := got[unsnapshotted]; present {
			t.Errorf("patch carries %q, which noteboard does NOT snapshot into "+
				"item_revisions — overwriting it destroys the old value outright",
				unsnapshotted)
		}
	}
}

// ============================ Hold / Unhold ============================

func TestHoldItemSpeaksTheHoldAction(t *testing.T) {
	rec := &recorder{status: 200, reply: `{"id":"abc","held_at":"2026-08-10T00:00:00Z"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	if _, err := c.HoldItem("abc", "because"); err != nil {
		t.Fatalf("HoldItem: %v", err)
	}
	if rec.method != "POST" {
		t.Errorf("method: got %q, want POST", rec.method)
	}
	if rec.path != "/api/items/abc/hold" {
		t.Errorf("path: got %q, want /api/items/abc/hold", rec.path)
	}
	// noteboard's hold action decodes {"reason": "..."} and nothing else.
	got := rec.decodedBody(t)
	if got["reason"] != "because" {
		t.Errorf("reason must be spelled \"reason\": %s", string(rec.body))
	}
	if len(got) != 1 {
		t.Errorf("hold body carries one key, got %d: %s", len(got), string(rec.body))
	}
}

func TestUnholdItemSpeaksTheUnholdAction(t *testing.T) {
	rec := &recorder{status: 200, reply: `{"id":"abc"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	if _, err := c.UnholdItem("abc"); err != nil {
		t.Fatalf("UnholdItem: %v", err)
	}
	if rec.method != "POST" {
		t.Errorf("method: got %q, want POST", rec.method)
	}
	if rec.path != "/api/items/abc/unhold" {
		t.Errorf("path: got %q, want /api/items/abc/unhold", rec.path)
	}
	// noteboard's unhold reads no body at all. Sending none is correct and the
	// live server accepts it; this pins that no body is required to appear.
	if len(rec.body) != 0 {
		t.Errorf("unhold must send no body, got %q", string(rec.body))
	}
}

// ============================ DeleteItem ============================

func TestDeleteItemSpeaksTheItemRoute(t *testing.T) {
	rec := &recorder{status: 200, reply: `{"status":"ok"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	if err := c.DeleteItem("abc", false); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	if rec.method != "DELETE" {
		t.Errorf("method: got %q, want DELETE", rec.method)
	}
	if rec.path != "/api/items/abc" {
		t.Errorf("path: got %q, want /api/items/abc", rec.path)
	}
	if rec.rawQuery != "" {
		t.Errorf("a reversible delete must send no query, got %q", rec.rawQuery)
	}
}

// The purge is opt-in and its spelling is exact: noteboard tests
// `q.Get("hard") == "true"`, so "1", "yes" or "TRUE" all read as a reversible
// delete. Getting this wrong in either direction is unrecoverable in one
// direction and confusing in the other.
func TestDeleteItemHardUsesTheExactSpellingNoteboardTests(t *testing.T) {
	rec := &recorder{status: 200, reply: `{"status":"ok"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	if err := c.DeleteItem("abc", true); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	q, err := url.ParseQuery(rec.rawQuery)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if q.Get("hard") != "true" {
		t.Errorf("hard delete must send hard=true exactly, got %q", rec.rawQuery)
	}
}

// ⚠️ This was a recorded disagreement. It is now a recorded consequence.
//
// The 114th pass measured noteboard answering a DELETE of an id it cannot find
// with **500** and the raw driver message, and pinned that here. The 130th pass
// fixed noteboard: it answers **404** now, like every other missing-item route
// on that service (noteboard branch fix/delete-of-a-missing-item-is-404,
// commit 23830ac, card 5962e9ef).
//
// Two things the old note here got wrong, and they are worth keeping written
// down because they are what a reader would otherwise re-derive:
//
//  1. It said "a second delete of the same card takes this path: the first
//     stamps deleted_at, the second finds nothing and 500s." It does not.
//     noteboard's DeleteItem reads through GetItemIncludingDeleted, which still
//     finds the tombstone, so a second soft delete is a no-op answering 200.
//     The paths that really reach the missing-row branch are an id that never
//     existed and an id whose row was hard-purged.
//  2. It said this test was pinned "in both directions so a change on either
//     side is visible". It was not, and could not be: the recorder below is a
//     fabricated server, so the status it returns is whatever this file says it
//     is. No change to noteboard can redden it. Only a change to THIS client
//     can. That is a fine thing for a client test to be — it just is not a
//     tripwire on the other repo, and calling it one meant nobody looked.
//
// DeleteItem now tells the two apart, as every other call on this client does:
// a 404 is a typed notFoundError, anything else is a plain error. The handler
// in internal/api/api.go answers the first with 404 and the second with 502
// (card 66c85b68-a051-4e86-9ac4-b0ff7b39b109 asked which; this is its "yes").
func TestDeleteItemTellsAMissingItemFromAFailure(t *testing.T) {
	rec := &recorder{status: 404, reply: `{"error":"not found"}`}
	c, closeFn := rec.server(t)
	err := c.DeleteItem("gone", false)
	closeFn()
	if !isNotFound(err) {
		t.Errorf("404: want a notFoundError, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "gone") {
		t.Errorf("404: the error must name the id, got: %v", err)
	}

	rec = &recorder{status: 500, reply: `{"error":"whatever"}`}
	c, closeFn = rec.server(t)
	err = c.DeleteItem("gone", false)
	closeFn()
	if err == nil {
		t.Fatal("500: must be an error")
	}
	if isNotFound(err) {
		t.Errorf("500: a failure must not read as a missing item, got %v", err)
	}
}

// ============================ Search ============================

func TestSearchSpeaksTheSearchRoute(t *testing.T) {
	rec := &recorder{status: 200, reply: `[{"id":"a"},{"id":"b"}]`}
	c, closeFn := rec.server(t)
	defer closeFn()

	items, err := c.Search("plumber", 25)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if rec.method != "GET" {
		t.Errorf("method: got %q, want GET", rec.method)
	}
	if rec.path != "/api/search" {
		t.Errorf("path: got %q, want /api/search", rec.path)
	}
	q, err := url.ParseQuery(rec.rawQuery)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if q.Get("q") != "plumber" {
		t.Errorf("query parameter is spelled q, got %q", rec.rawQuery)
	}
	if q.Get("limit") != "25" {
		t.Errorf("limit: got %q, want 25", q.Get("limit"))
	}
}

// include_held is deliberate and load-bearing, not a copied flag. A kanban board
// is where a human goes to find work they parked and press play on it; noteboard
// withholds held items from the default read path, so without this the card a
// user parked would vanish from the board it is parked on and could never be
// un-parked. Pinned because this is exactly the parameter that gets copied into
// a context where it is wrong — an agent's own discovery query must NOT set it.
func TestSearchAsksForHeldItemsBecauseTheBoardMustShowParkedWork(t *testing.T) {
	rec := &recorder{status: 200, reply: `[]`}
	c, closeFn := rec.server(t)
	defer closeFn()

	if _, err := c.Search("x", 0); err != nil {
		t.Fatalf("Search: %v", err)
	}
	q, err := url.ParseQuery(rec.rawQuery)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if q.Get("include_held") != "true" {
		t.Errorf("the board's search must set include_held=true, got %q", rec.rawQuery)
	}
}

func TestSearchOmitsLimitWhenUnset(t *testing.T) {
	rec := &recorder{status: 200, reply: `[]`}
	c, closeFn := rec.server(t)
	defer closeFn()

	if _, err := c.Search("x", 0); err != nil {
		t.Fatalf("Search: %v", err)
	}
	q, err := url.ParseQuery(rec.rawQuery)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if _, present := q["limit"]; present {
		t.Errorf("limit=0 must be omitted so noteboard applies its own default, got %q",
			rec.rawQuery)
	}
}

func TestSearchEscapesTheQuery(t *testing.T) {
	rec := &recorder{status: 200, reply: `[]`}
	c, closeFn := rec.server(t)
	defer closeFn()

	if _, err := c.Search("a&b=c d", 0); err != nil {
		t.Fatalf("Search: %v", err)
	}
	q, err := url.ParseQuery(rec.rawQuery)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if q.Get("q") != "a&b=c d" {
		t.Errorf("query not escaped: got %q from %q", q.Get("q"), rec.rawQuery)
	}
	if q.Get("include_held") != "true" {
		t.Errorf("an unescaped query swallowed include_held: %q", rec.rawQuery)
	}
}

// noteboard requires q and answers 400 when it is empty. The board's own
// handler guards this first, so it should not reach the wire — but the client
// must still report it rather than answer an empty result set, which would read
// as "nothing matched".
func TestSearchEmptyQueryIsAnErrorNotAnEmptyResult(t *testing.T) {
	rec := &recorder{status: 400, reply: `{"error":"q parameter is required"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	items, err := c.Search("", 0)
	if err == nil {
		t.Fatal("400 must be an error — an empty slice would read as 'nothing matched'")
	}
	if items != nil {
		t.Errorf("a failed search must return no items, got %v", items)
	}
}

// ============================ doJSON, the chokepoint ============================

// Seven of the eight calls share this transport, so one wrong judgment here is
// seven wrong calls.
func TestDoJSONTreatsAnUnexpectedStatusAsAnError(t *testing.T) {
	rec := &recorder{status: 503, reply: `{"error":"upstream is down"}`}
	c, closeFn := rec.server(t)
	defer closeFn()

	_, err := c.GetItem("abc")
	if err == nil {
		t.Fatal("503 must be an error")
	}
	if isNotFound(err) {
		t.Error("503 is not a missing item")
	}
	// The server's own words have to survive: a bare status code sends the
	// reader to the wrong service.
	if !strings.Contains(err.Error(), "upstream is down") {
		t.Errorf("the response body must reach the caller, got: %v", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("the status must reach the caller, got: %v", err)
	}
}

// A 200 carrying something that is not an item is a failure, not an empty item.
// This is the shape that hides best: "does it parse" is not "did it succeed".
func TestDoJSONRejectsABodyThatIsNotAnObject(t *testing.T) {
	rec := &recorder{status: 200, reply: `<html>502 Bad Gateway</html>`}
	c, closeFn := rec.server(t)
	defer closeFn()

	item, err := c.GetItem("abc")
	if err == nil {
		t.Fatal("an unparseable body must be an error, not an empty item")
	}
	if item != nil {
		t.Errorf("a failed read must return no item, got %v", item)
	}
}

// ============================ GetItems ============================

func TestGetItemsReturnsNilForMissingAndKeepsInputOrder(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/api/items/batch" {
			t.Errorf("wrong batch route: %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"items":[{"id":"a"},null,{"id":"c"}]}`)
	}))
	t.Cleanup(func() {
		if calls != 1 {
			t.Errorf("expected one batch call, got %d", calls)
		}
	})
	defer srv.Close()

	c := New(srv.URL)
	ids := []string{"a", "missing", "c"}
	items, err := c.GetItems(ids)
	if err != nil {
		t.Fatalf("GetItems: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 slots, got %d", len(items))
	}
	if items[0] == nil || items[0]["id"] != "a" {
		t.Errorf("slot 0: got %v, want id a — results must stay in input order", items[0])
	}
	if items[1] != nil {
		t.Errorf("slot 1: a 404 must be a nil entry so the board can report an orphan, got %v",
			items[1])
	}
	if items[2] == nil || items[2]["id"] != "c" {
		t.Errorf("slot 2: got %v, want id c", items[2])
	}
}

// A real failure is not an orphan. A missing item means "this placement points
// at nothing"; a 500 means "we could not find out", and reporting the second as
// the first would quietly empty a board during a noteboard outage.
func TestGetItemsPropagatesARealFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		io.WriteString(w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	c := New(srv.URL)
	if _, err := c.GetItems([]string{"a", "b"}); err == nil {
		t.Fatal("a 500 from an item read must surface — an outage must not read as " +
			"a board full of orphans")
	}
}
