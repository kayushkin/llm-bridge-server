package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/model"
)

// The double in noteboard_query_test.go answers 200 `[]` to any method on any
// path. That is right for what it pins — escaping is a property of the request
// line — but it leaves every other claim ItemsClient makes about the item stores
// with no fixture behind it, and a claim with no fixture cannot be disagreed
// with at all.
//
// Every status and body below was measured against the RUNNING noteboard on
// :8191 on 2026-09-07, with a throwaway item, not read off its source:
//
//	GET    /api/items          (no match)          200 []            not null
//	POST   /api/items                              201 <the item, with a top-level id>
//	POST   /api/items          (no title)          400 {"error":"title is required"}
//	PATCH  /api/items/{id}                         200 <the whole item>
//	PATCH  /api/items/{unknown}                    404 {"error":"not found"}
//	GET    /api/items?parent_id={unknown}          200 []
//	DELETE /api/items          (wrong verb)        405 {"error":"method not allowed"}
//	GET    /api/nosuchroute                        404 404 page not found   (PLAIN TEXT)
//
// and two behaviors that are characterised rather than fixed, each with its own
// test saying so out loud.
//
// The same item routes now answer inside llm-bridge-server, with /api gone:
// todos at /issues/items and notes at /notes/items. The stub serves both.

// noteboardStub routes through a real http.ServeMux carrying noteboard's own
// routes, so a wrong verb is 405 here because it is 405 there, and a path the
// client did not mean to build is 404 rather than a cheerful empty list. A
// stand-in that answers everything identically cannot notice a wrong URL.
type noteboardStub struct {
	// items is what a list route answers with.
	items []map[string]any
	// knownID is the only id the single-item routes recognize; anything else 404s.
	knownID string
	// lastQuery is the raw query of the last list request, for the caller to read.
	lastQuery string
	// listRequests counts GETs on the list route, so a test can see the paging.
	listRequests int
	// lastPatch is the decoded body of the last PATCH on the known item.
	lastPatch map[string]any
	// stores is "<method> <store prefix>" for every request, in order, so a
	// test can see which store a call reached.
	stores []string
}

func (n *noteboardStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for _, store := range []string{"/issues", "/notes"} {
		n.registerItemRoutes(mux, store)
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// registerItemRoutes serves the item routes one store answers under its
// prefix in the bridge, recording which store each request reached.
func (n *noteboardStub) registerItemRoutes(mux *http.ServeMux, store string) {
	mux.HandleFunc(store+"/items", func(w http.ResponseWriter, r *http.Request) {
		n.stores = append(n.stores, r.Method+" "+store)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			n.lastQuery = r.URL.RawQuery
			n.listRequests++
			body := n.items
			if body == nil {
				body = []map[string]any{}
			}
			// noteboard caps a page at `limit` and skips `offset` rows first, and
			// says nothing about what it left behind; the stub does the same so a
			// client that stops after one page is caught here.
			if offset, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil {
				body = body[min(offset, len(body)):]
			}
			if limit, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
				body = body[:min(limit, len(body))]
			}
			json.NewEncoder(w).Encode(body)
		case http.MethodPost:
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			if title, _ := req["title"].(string); title == "" {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"title is required"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			req["id"] = "created-item-id"
			json.NewEncoder(w).Encode(req)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			w.Write([]byte(`{"error":"method not allowed"}`))
		}
	})

	mux.HandleFunc(store+"/items/", func(w http.ResponseWriter, r *http.Request) {
		n.stores = append(n.stores, r.Method+" "+store)
		w.Header().Set("Content-Type", "application/json")
		id := strings.TrimPrefix(r.URL.Path, store+"/items/")
		if id != n.knownID {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"not found"}`))
			return
		}
		if r.Method == http.MethodPatch {
			n.lastPatch = map[string]any{}
			json.NewDecoder(r.Body).Decode(&n.lastPatch)
		}
		switch r.Method {
		case http.MethodGet, http.MethodPatch, http.MethodDelete:
			// noteboard echoes the WHOLE item back on a PATCH. The client passes
			// out=nil and discards it — pinned below, because that is what makes a
			// dropped field silent.
			json.NewEncoder(w).Encode(map[string]any{"id": id, "type": "todo", "title": "probe"})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			w.Write([]byte(`{"error":"method not allowed"}`))
		}
	})
}

// itemsClientFor is the client as newCoordinator builds it for a bridge at
// bridgeURL: todos under /issues, notes under /notes.
func itemsClientFor(bridgeURL string) *ItemsClient {
	return &ItemsClient{IssuesURL: bridgeURL + "/issues", NotesURL: bridgeURL + "/notes"}
}

// ---------------------------------------------------------------- the refusals

// The client's only error channel is `status >= 300`. If the status does not
// reach the message, a refused read is indistinguishable from a malformed one,
// and the coordinator's log says the wrong thing about a live outage.
func TestANoteboardRefusalReachesTheCallerAsItsStatus(t *testing.T) {
	stub := &noteboardStub{knownID: "the-item"}
	client := itemsClientFor(stub.server(t).URL)

	err := client.PatchTodo("some-other-item", map[string]any{"status": "done"})
	if err == nil {
		t.Fatal("a PATCH against an id noteboard does not have was reported as success")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("the status did not reach the message: %v", err)
	}
}

// noteboard's own router answers an unrouted path with net/http's plain-text
// "404 page not found", not with a JSON error body. A client that assumed every
// refusal is JSON would report a wrong URL as a decode failure.
func TestAnUnroutedPathIsReportedAsARefusalAndNotAsABadBody(t *testing.T) {
	stub := &noteboardStub{knownID: "the-item"}
	client := itemsClientFor(stub.server(t).URL)

	err := client.do("GET", client.IssuesURL, "/nosuchroute", nil, &[]map[string]any{})
	if err == nil {
		t.Fatal("a 404 on an unrouted path was read as an empty result")
	}
	if strings.Contains(err.Error(), "invalid character") {
		t.Errorf("a plain-text refusal was reported as a JSON decode failure: %v", err)
	}
}

// ---------------------------------------------------------------- the agreements

// noteboard answers 201, not 200, on a create. The client's only success test is
// `status < 300`, so this passes — but nothing said so until now, and narrowing
// that check to `== 200` is a one-character edit that would stop every recurring
// occurrence from being materialized.
func TestCreateItemAcceptsThe201NoteboardAnswersWith(t *testing.T) {
	stub := &noteboardStub{knownID: "the-item"}
	client := itemsClientFor(stub.server(t).URL)

	title := "nightly probe"
	created, err := client.CreateItem(&model.CreateItemRequest{
		Type: model.TypeTodo, Title: title, Tags: []string{"personal"},
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if created.ID == "" {
		t.Error("the created item carried no id, so the caller cannot link to it")
	}
}

// An empty result is `[]` and not `null`. Both decode into a nil-or-empty slice
// without error, so this is an agreement rather than a risk — recorded because
// the next reader should not have to re-probe to learn it.
func TestAnEmptyListIsAnEmptyArray(t *testing.T) {
	stub := &noteboardStub{knownID: "the-item"}
	client := itemsClientFor(stub.server(t).URL)

	items, err := client.ListOpen("a-tag-nothing-carries")
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected no items, got %d", len(items))
	}
}

// ---------------------------------------------------------------- characterised

// noteboard caps a page at the `limit` asked for and says nothing about what it
// left behind — measured on :8191, where limit=500 returned exactly 500 open
// todos and limit=5000 returned 1114. The list methods used to ask for a fixed
// 500, so a full page and a truncated one were the same value. They page now,
// and this is the 1114 that was measured.
func TestListOpenReadsPastTheFirstFullPage(t *testing.T) {
	stub := &noteboardStub{items: numberedItems(1114), knownID: "the-item"}
	client := itemsClientFor(stub.server(t).URL)

	items, err := client.ListOpen("personal")
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(items) != 1114 {
		t.Fatalf("got %d items, want all 1114 — the read stopped at a page boundary", len(items))
	}
	if stub.listRequests != 3 {
		t.Errorf("made %d list requests, want 3 pages of 500 for 1114 items", stub.listRequests)
	}
}

// ListChildren is the list that grows by itself: an instance-mode template keeps
// one child per occurrence. hasChildDueAt reads it to avoid a duplicate, so a
// child past the first page must still be in the answer.
func TestListChildrenReadsPastTheFirstFullPage(t *testing.T) {
	stub := &noteboardStub{items: numberedItems(501), knownID: "the-item"}
	client := itemsClientFor(stub.server(t).URL)

	items, err := client.ListChildren("the-template")
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(items) != 501 || items[500].ID != "item-00500" {
		t.Fatalf("got %d children, want 501 ending in item-00500", len(items))
	}
}

// A list that exactly fills its last page costs one more request, which comes
// back empty. That empty page is what separates "500 of 500" from "500 of more".
func TestAnExactlyFullPageIsFollowedByAnEmptyOne(t *testing.T) {
	stub := &noteboardStub{items: numberedItems(500), knownID: "the-item"}
	client := itemsClientFor(stub.server(t).URL)

	items, err := client.ListOpen("personal")
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(items) != 500 || stub.listRequests != 2 {
		t.Fatalf("got %d items in %d requests, want 500 in 2", len(items), stub.listRequests)
	}
}

// The page bound is the one place the read can still stop early, so it must stop
// loudly: a partial list returned as a whole one is the defect paging replaced.
func TestAListLongerThanThePageBoundIsAnError(t *testing.T) {
	stub := &noteboardStub{items: numberedItems(itemsPageSize*itemsMaxPages + 1), knownID: "the-item"}
	client := itemsClientFor(stub.server(t).URL)

	items, err := client.ListOpen("personal")
	if err == nil {
		t.Fatalf("got %d items and no error from a list longer than the page bound", len(items))
	}
	if !strings.Contains(err.Error(), "partial list") {
		t.Errorf("error does not say the list was partial: %v", err)
	}
}

func numberedItems(count int) []map[string]any {
	items := make([]map[string]any, count)
	for i := range items {
		items[i] = map[string]any{"id": fmt.Sprintf("item-%05d", i), "type": "todo", "title": "x"}
	}
	return items
}

// ⚠️ Also a characterisation. noteboard answers 200 and echoes the whole item for
// a PATCH field it does not recognize — measured: {"due_date":...} returned 200
// and the item came back without it. The client passes out=nil and discards the
// body, so `err == nil` is not evidence that anything was written.
//
// Every field name the coordinator actually sends was probed against :8191 on
// 2026-09-07 and all five land: status, due_at, body, links, schedule. So this
// records a hazard, not a live bug.
func TestAPatchReturnsNilForAFieldNoteboardDropped(t *testing.T) {
	stub := &noteboardStub{knownID: "the-item"}
	client := itemsClientFor(stub.server(t).URL)

	if err := client.PatchTodo("the-item", map[string]any{"due_date": "2027-01-02T03:04:05Z"}); err != nil {
		t.Fatalf("expected the misspelled field to be accepted silently, got %v", err)
	}
	t.Log("characterised: a misspelled PATCH field is a 200 and a no-op — the only " +
		"evidence a write landed is reading the value back")
}

// ---------------------------------------------------------------- what the client keeps and writes

// ListScheduled is the roll and materialize steps' whole view of the board: an
// item it drops is never rolled, and an item it wrongly keeps reaches code that
// reads its rule.
func TestListScheduledKeepsOnlyItemsCarryingARule(t *testing.T) {
	stub := &noteboardStub{knownID: "the-item", items: []map[string]any{
		{"id": "with-rule", "type": "todo", "title": "x", "schedule": map[string]any{"rrule": "FREQ=WEEKLY", "tzid": "UTC"}},
		{"id": "without-rule", "type": "todo", "title": "x"},
	}}
	client := itemsClientFor(stub.server(t).URL)

	items, err := client.ListScheduled("personal")
	if err != nil {
		t.Fatalf("ListScheduled: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("ListScheduled dropped the item that carries a rule")
	}
	for _, item := range items {
		if item.ID != "with-rule" {
			t.Errorf("ListScheduled kept %q, which carries no rule", item.ID)
		}
	}
}

// noteboard answers newest first, so the first match is the newest item with the
// tag, and that is the one FindByTag's callers rewrite.
func TestFindByTagReturnsTheFirstMatchNoteboardAnswers(t *testing.T) {
	stub := &noteboardStub{knownID: "the-item", items: []map[string]any{
		{"id": "newest", "type": "note", "title": "x"},
		{"id": "older", "type": "note", "title": "x"},
	}}
	client := itemsClientFor(stub.server(t).URL)

	item, err := client.FindByTag(model.TypeNote, "personal-digest")
	if err != nil {
		t.Fatalf("FindByTag: %v", err)
	}
	if item == nil || item.ID != "newest" {
		t.Fatalf("FindByTag returned %+v, want the first item noteboard answered, %q", item, "newest")
	}
}

// noteboard drops a PATCH key it does not know and still answers 200 (see
// TestAPatchReturnsNilForAFieldNoteboardDropped), so a misspelled key loses the
// coordinator's bookkeeping with no error anywhere. The key must be `schedule`.
func TestSetScheduleWritesTheScheduleKey(t *testing.T) {
	stub := &noteboardStub{knownID: "the-item"}
	client := itemsClientFor(stub.server(t).URL)

	if err := client.SetSchedule("the-item", &model.Schedule{RRule: "FREQ=DAILY", TZID: "UTC"}, false); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	if _, found := stub.lastPatch["schedule"]; !found || len(stub.lastPatch) != 1 {
		t.Fatalf("PATCH body was %v, want exactly the key \"schedule\"", stub.lastPatch)
	}
}

// ---------------------------------------------------------------- which store a call reaches

// Todos live in issues and the digest note in notes. A todo's write that
// reached notes, or the note's that reached issues, is refused there with a
// 400 at best; at worst it lands on whatever that store holds under the id.
// Each call must reach the store that holds its type.
func TestEachCallReachesTheStoreThatHoldsItsType(t *testing.T) {
	stub := &noteboardStub{knownID: "the-item"}
	client := itemsClientFor(stub.server(t).URL)

	calls := []struct {
		name string
		call func() error
		want string
	}{
		{"ListOpen", func() error { _, err := client.ListOpen("personal"); return err }, "GET /issues"},
		{"ListChildren", func() error { _, err := client.ListChildren("the-template"); return err }, "GET /issues"},
		{"FindByTag todo", func() error { _, err := client.FindByTag(model.TypeTodo, "personal"); return err }, "GET /issues"},
		{"FindByTag note", func() error { _, err := client.FindByTag(model.TypeNote, digestTag); return err }, "GET /notes"},
		{"CreateItem todo", func() error {
			_, err := client.CreateItem(&model.CreateItemRequest{Type: model.TypeTodo, Title: "occurrence"})
			return err
		}, "POST /issues"},
		{"CreateItem note", func() error {
			_, err := client.CreateItem(&model.CreateItemRequest{Type: model.TypeNote, Title: digestTitle})
			return err
		}, "POST /notes"},
		{"PatchTodo", func() error { return client.PatchTodo("the-item", map[string]any{"status": "done"}) }, "PATCH /issues"},
		{"SetSchedule", func() error {
			return client.SetSchedule("the-item", &model.Schedule{RRule: "FREQ=DAILY", TZID: "UTC"}, false)
		}, "PATCH /issues"},
		{"PatchNote", func() error { return client.PatchNote("the-item", map[string]any{"body": "today"}) }, "PATCH /notes"},
	}
	for _, c := range calls {
		stub.stores = nil
		if err := c.call(); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(stub.stores) != 1 || stub.stores[0] != c.want {
			t.Errorf("%s reached %v, want exactly %q", c.name, stub.stores, c.want)
		}
	}
}

// A type neither store holds the coordinator's way is refused before any
// request, rather than sent to whichever store a guess picks.
func TestATypeTheCoordinatorDoesNotRouteIsRefused(t *testing.T) {
	stub := &noteboardStub{knownID: "the-item"}
	client := itemsClientFor(stub.server(t).URL)

	if _, err := client.FindByTag(model.TypeWorkspace, "personal"); err == nil {
		t.Error("FindByTag of a workspace was sent somewhere")
	}
	if _, err := client.CreateItem(&model.CreateItemRequest{Type: "", Title: "untyped"}); err == nil {
		t.Error("CreateItem with no type was sent somewhere")
	}
	if len(stub.stores) != 0 {
		t.Errorf("refused calls still reached %v", stub.stores)
	}
}
