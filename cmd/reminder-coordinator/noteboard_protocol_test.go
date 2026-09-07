package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/noteboard/model"
)

// The double in noteboard_query_test.go answers 200 `[]` to any method on any
// path. That is right for what it pins — escaping is a property of the request
// line — but it leaves every other claim NoteboardClient makes about noteboard
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
// and two behaviours that are characterised rather than fixed, each with its own
// test saying so out loud.

// noteboardStub routes through a real http.ServeMux carrying noteboard's own
// routes, so a wrong verb is 405 here because it is 405 there, and a path the
// client did not mean to build is 404 rather than a cheerful empty list. A
// stand-in that answers everything identically cannot notice a wrong URL.
type noteboardStub struct {
	// items is what a list route answers with.
	items []map[string]any
	// knownID is the only id the single-item routes recognise; anything else 404s.
	knownID string
	// lastQuery is the raw query of the last list request, for the caller to read.
	lastQuery string
}

func (n *noteboardStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			n.lastQuery = r.URL.RawQuery
			body := n.items
			if body == nil {
				body = []map[string]any{}
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

	mux.HandleFunc("/api/items/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id := strings.TrimPrefix(r.URL.Path, "/api/items/")
		if id != n.knownID {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"not found"}`))
			return
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

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// ---------------------------------------------------------------- the refusals

// The client's only error channel is `status >= 300`. If the status does not
// reach the message, a refused read is indistinguishable from a malformed one,
// and the coordinator's log says the wrong thing about a live outage.
func TestANoteboardRefusalReachesTheCallerAsItsStatus(t *testing.T) {
	stub := &noteboardStub{knownID: "the-item"}
	client := &NoteboardClient{BaseURL: stub.server(t).URL}

	err := client.Patch("some-other-item", map[string]any{"status": "done"})
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
	client := &NoteboardClient{BaseURL: stub.server(t).URL}

	err := client.do("GET", "/api/nosuchroute", nil, &[]map[string]any{})
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
	client := &NoteboardClient{BaseURL: stub.server(t).URL}

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
	client := &NoteboardClient{BaseURL: stub.server(t).URL}

	items, err := client.ListOpen("a-tag-nothing-carries")
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected no items, got %d", len(items))
	}
}

// ---------------------------------------------------------------- characterised

// ⚠️ This test asserts a DEFECT is present. It is here so that the day somebody
// repairs it, the suite says so instead of going quietly green.
//
// noteboard caps a page at the `limit` asked for and says nothing about what it
// left behind — measured on :8191, where limit=500 returned exactly 500 open
// todos and limit=5000 returned 1114. Every list method here asks for a fixed
// 500 and none checks whether it got exactly that many back, so a full page and
// a truncated one are the same value.
//
// Latent for the scope tag today (40 open `personal` todos), and NOT latent for
// ListChildren: an instance-mode template accumulates one child per occurrence
// for as long as it runs.
func TestAFullPageIsIndistinguishableFromATruncatedOne(t *testing.T) {
	full := make([]map[string]any, 500)
	for i := range full {
		full[i] = map[string]any{"id": fmt.Sprintf("item-%03d", i), "type": "todo", "title": "x"}
	}
	stub := &noteboardStub{items: full, knownID: "the-item"}
	client := &NoteboardClient{BaseURL: stub.server(t).URL}

	items, err := client.ListOpen("personal")
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(items) != 500 {
		t.Fatalf("stub returned %d items, want the 500 the client asked for", len(items))
	}
	if !strings.Contains(stub.lastQuery, "limit=500") {
		t.Fatalf("the client asked for a limit other than 500: %q", stub.lastQuery)
	}
	// The characterisation: a page that is exactly as long as the limit is
	// returned with a nil error and no signal of any kind.
	t.Log("characterised: 500 of 500 returned with no error and no short-read check — " +
		"see the card on b9b8dafc; the repair is a paging decision, not a patch")
}

// ⚠️ Also a characterisation. noteboard answers 200 and echoes the whole item for
// a PATCH field it does not recognise — measured: {"due_date":...} returned 200
// and the item came back without it. The client passes out=nil and discards the
// body, so `err == nil` is not evidence that anything was written.
//
// Every field name the coordinator actually sends was probed against :8191 on
// 2026-09-07 and all five land: status, due_at, body, links, schedule. So this
// records a hazard, not a live bug.
func TestAPatchReturnsNilForAFieldNoteboardDropped(t *testing.T) {
	stub := &noteboardStub{knownID: "the-item"}
	client := &NoteboardClient{BaseURL: stub.server(t).URL}

	if err := client.Patch("the-item", map[string]any{"due_date": "2027-01-02T03:04:05Z"}); err != nil {
		t.Fatalf("expected the misspelled field to be accepted silently, got %v", err)
	}
	t.Log("characterised: a misspelled PATCH field is a 200 and a no-op — the only " +
		"evidence a write landed is reading the value back")
}
