package kanbanclient

// Protocol tests for the kanban-store client: does it speak the protocol
// kanban-store actually serves?
//
// id_path_test.go already pins the path SEGMENTS of every call. It does not pin
// the verb, the request field names, or which status the client reads as
// success, and nothing else here does either: every caller fakes this client at
// its own boundary. The instrument below is a stand-in kanban-store that
// records the wire traffic, judged against ~/repos/kanban-store's real handlers.
//
// Ported from 622572ea (test/kanban-clients-speak-the-real-protocol) and
// re-judged against kanban-store internal/api/api.go, ticket.go and
// tag_rules.go as of dbd1328:
//
//	GET    /api/boards                                  200
//	GET    /api/boards/{b}                              200, 404
//	GET    /api/boards/{b}/columns                      200
//	GET    /api/boards/{b}/cards                        200
//	POST   /api/boards/{b}/cards                        201
//	DELETE /api/boards/{b}/cards/{c}                    200
//	GET    /api/boards/{b}/cards/{c}/effective-defaults 200, 404
//	POST   /api/cards/{c}/move                          200, 409 on WIP
//	PATCH  /api/cards/{c}                               200
//	POST   /api/cards/{c}/hold                          200
//	PUT    /api/cards/{c}/ticket                        200
//	GET    /api/cards/{c}/placements                    200
//	GET    /api/cards/{c}/links                         200
//	POST   /api/cards/{c}/links                         201
//	DELETE /api/links/{l}                               200
//	GET    /api/entities/{t}/{r}/tags                   200
//	POST   /api/entities/{t}/{r}/tags                   201
//	DELETE /api/entities/{t}/{r}/tags/{g}               200
//	GET    /api/entities/{t}/{r}/cards                  200
//
// MoveCard's auto-status and WIP cases live in move_card_test.go, and the
// entity type sent verbatim is covered by id_path_test.go's entityType probe.
//
// Every path assertion reads r.RequestURI, never r.URL.Path: net/http decodes
// URL.Path before a handler sees it, so only RequestURI is the bytes that went
// over the wire.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// recordedWireRequest is one call as the upstream saw it.
type recordedWireRequest struct {
	Method     string
	RequestURI string
	Body       string
}

// wireRecordingStore is a stand-in kanban-store. It records every request and
// answers each with one scripted status and body, so a case can assert on what
// the client SENT as well as on what it did with the answer.
type wireRecordingStore struct {
	server   *httptest.Server
	requests []recordedWireRequest
}

func newWireRecordingStore(t *testing.T, status int, body string) *wireRecordingStore {
	t.Helper()
	store := &wireRecordingStore{}
	store.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		store.requests = append(store.requests, recordedWireRequest{
			Method:     r.Method,
			RequestURI: r.RequestURI,
			Body:       string(raw),
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(store.server.Close)
	return store
}

// client returns a client already bound to board b1 with a Todo column, so
// CreateCard can run without the EnsureBoard round trips.
func (store *wireRecordingStore) client() *Client {
	client := New(store.server.URL, "")
	client.BoardID = "b1"
	client.ColumnsByName = map[string]string{"Todo": "col-todo"}
	return client
}

func (store *wireRecordingStore) onlyRequest(t *testing.T) recordedWireRequest {
	t.Helper()
	if len(store.requests) != 1 {
		t.Fatalf("expected exactly 1 upstream request, got %d: %+v", len(store.requests), store.requests)
	}
	return store.requests[0]
}

func (store *wireRecordingStore) onlyRequestBody(t *testing.T) map[string]any {
	t.Helper()
	var sent map[string]any
	if err := json.Unmarshal([]byte(store.onlyRequest(t).Body), &sent); err != nil {
		t.Fatalf("request body is not a JSON object: %v", err)
	}
	return sent
}

// protocolCall is one client call with kanban-store's answer to it on the
// happy path.
type protocolCall struct {
	name          string
	invoke        func(c *Client) error
	successStatus int
	successBody   string
	wantMethod    string
	wantURI       string
}

func ignoreResult[T any](_ T, err error) error { return err }

// everyCall lists each exported call that makes exactly one request. The
// success bodies are the smallest the client decodes without complaint.
var everyCall = []protocolCall{
	{"ListBoards", func(c *Client) error { return ignoreResult(c.ListBoards()) },
		200, `[]`, "GET", "/api/boards"},
	{"GetBoard", func(c *Client) error { return ignoreResult(c.GetBoard("b1")) },
		200, `{"id":"b1"}`, "GET", "/api/boards/b1"},
	{"ListColumns", func(c *Client) error { return ignoreResult(c.ListColumns("b1")) },
		200, `[]`, "GET", "/api/boards/b1/columns"},
	{"GetBoardView", func(c *Client) error { return ignoreResult(c.GetBoardView("b1")) },
		200, `{}`, "GET", "/api/boards/b1/cards"},
	{"CreateCard", func(c *Client) error {
		return ignoreResult(c.CreateCard(CreateCardArgs{ColumnName: "Todo", Title: "t"}))
	}, 201, `{"item":{"id":"card-1"}}`, "POST", "/api/boards/b1/cards"},
	{"DetachCardFromBoard", func(c *Client) error { return c.DetachCardFromBoard("b1", "card-1") },
		200, `{"status":"ok"}`, "DELETE", "/api/boards/b1/cards/card-1"},
	{"GetCardEffectiveDefaults", func(c *Client) error {
		return ignoreResult(c.GetCardEffectiveDefaults("b1", "card-1"))
	}, 200, `{"board_id":"b1","card_id":"card-1"}`, "GET", "/api/boards/b1/cards/card-1/effective-defaults"},
	{"MoveCard", func(c *Client) error { return ignoreResult(c.MoveCard("card-1", "b1", "col-1", 3)) },
		200, `{"placement":{}}`, "POST", "/api/cards/card-1/move"},
	{"PatchCard", func(c *Client) error { return c.PatchCard("card-1", map[string]any{"title": "t"}) },
		200, `{}`, "PATCH", "/api/cards/card-1"},
	{"PatchCardBody", func(c *Client) error { return c.PatchCardBody("card-1", "hello") },
		200, `{}`, "PATCH", "/api/cards/card-1"},
	{"HoldCard", func(c *Client) error { return c.HoldCard("card-1", "parked") },
		200, `{}`, "POST", "/api/cards/card-1/hold"},
	{"PutTicket", func(c *Client) error {
		return c.PutTicket("card-1", "principal_000001", "email", "email", "msg-1")
	}, 200, `{}`, "PUT", "/api/cards/card-1/ticket"},
	{"ListCardPlacements", func(c *Client) error { return ignoreResult(c.ListCardPlacements("card-1")) },
		200, `[]`, "GET", "/api/cards/card-1/placements"},
	{"ListCardLinks", func(c *Client) error { return ignoreResult(c.ListCardLinks("card-1")) },
		200, `[]`, "GET", "/api/cards/card-1/links"},
	// Shares its path with ListCardLinks and is told apart only by the verb.
	{"AddCardLink", func(c *Client) error { return c.AddCardLink("card-1", "session", "sess-1", "label") },
		201, `{}`, "POST", "/api/cards/card-1/links"},
	{"DeleteCardLink", func(c *Client) error { return c.DeleteCardLink("link-1") },
		200, `{"status":"ok"}`, "DELETE", "/api/links/link-1"},
	{"ListEntityTags", func(c *Client) error { return ignoreResult(c.ListEntityTags("session", "sess-1")) },
		200, `[]`, "GET", "/api/entities/session/sess-1/tags"},
	{"AddEntityTag", func(c *Client) error { return c.AddEntityTag("session", "sess-1", "scoped") },
		201, `{}`, "POST", "/api/entities/session/sess-1/tags"},
	{"DeleteEntityTag", func(c *Client) error { return c.DeleteEntityTag("session", "sess-1", "scoped") },
		200, `{"status":"ok"}`, "DELETE", "/api/entities/session/sess-1/tags/scoped"},
	{"ListCardsForEntity", func(c *Client) error {
		return ignoreResult(c.ListCardsForEntity("parent_card", "card-9"))
	}, 200, `[]`, "GET", "/api/entities/parent_card/card-9/cards"},
}

// ============================ method and path ============================

// A wrong verb is a 405 and a wrong path is a 404, and neither reddens any
// other test in this repo, because every caller fakes the client.
func TestEachCallSpeaksItsRouteVerbatim(t *testing.T) {
	for _, call := range everyCall {
		t.Run(call.name, func(t *testing.T) {
			store := newWireRecordingStore(t, call.successStatus, call.successBody)
			if err := call.invoke(store.client()); err != nil {
				t.Fatalf("call returned an error on a scripted success: %v", err)
			}
			got := store.onlyRequest(t)
			if got.Method != call.wantMethod {
				t.Errorf("method = %q, kanban-store routes %q", got.Method, call.wantMethod)
			}
			if got.RequestURI != call.wantURI {
				t.Errorf("RequestURI = %q, kanban-store routes %q", got.RequestURI, call.wantURI)
			}
		})
	}
}

// ============================ the success/failure status split ============================

// A failed call must not look like a successful one. The failure carries the
// route's own success body on purpose: an error object cannot decode into a
// slice, so a list call would fail on the decode even with its status check
// deleted, and the case would pass while pinning nothing. Only a body that
// decodes cleanly shows whether the status was consulted at all — and `[]` is
// also what a proxy or a rewritten route in front of kanban-store tends to
// answer with.
func TestEachCallRefusesAServerFaultWhoseBodyDecodes(t *testing.T) {
	for _, call := range everyCall {
		t.Run(call.name, func(t *testing.T) {
			store := newWireRecordingStore(t, http.StatusInternalServerError, call.successBody)
			if err := call.invoke(store.client()); err == nil {
				t.Errorf("HTTP 500 carrying %s was read as success, which a caller cannot "+
					"tell from an empty answer", call.successBody)
			}
		})
	}
}

// entityTagsList POST and cardLinks POST write 201 and the rest write 200, so a
// client that accepted "any 2xx" would hide a route that had changed shape.
// Each write must accept exactly what kanban-store writes and refuse its
// neighbor.
//
// PutTicket is left out: kanban-store writes 200 but the client accepts any
// 2xx, deliberately or not. That is a behavior question for the client, not
// something a protocol port should settle.
func TestEachWriteAcceptsExactlyTheStatusTheServerWrites(t *testing.T) {
	neighbor := map[int]int{200: 201, 201: 200}
	for _, call := range everyCall {
		if call.wantMethod == "GET" || call.name == "PutTicket" {
			continue
		}
		t.Run(call.name+"/refuses a neighboring 2xx", func(t *testing.T) {
			rejected := neighbor[call.successStatus]
			store := newWireRecordingStore(t, rejected, call.successBody)
			if err := call.invoke(store.client()); err == nil {
				t.Errorf("HTTP %d is not what kanban-store writes for this route (%d), "+
					"but the client read it as success", rejected, call.successStatus)
			}
		})
	}
}

// ============================ request JSON field names ============================

// The request bodies against kanban-store's model structs. board_id and
// column_id are required by MoveCardRequest.Validate, so their misspelling is
// a 400; position has no such guard, so a misspelling there would be silent.
func TestWriteRequestsNameTheFieldsTheServerDecodes(t *testing.T) {
	expectFields := func(t *testing.T, sent map[string]any, want map[string]any) {
		t.Helper()
		for field, value := range want {
			if sent[field] != value {
				t.Errorf("request[%q] = %#v, kanban-store decodes %#v", field, sent[field], value)
			}
		}
	}

	t.Run("MoveCard", func(t *testing.T) {
		store := newWireRecordingStore(t, 200, `{"placement":{}}`)
		if _, err := store.client().MoveCard("card-1", "board-1", "col-1", 7.5); err != nil {
			t.Fatalf("MoveCard: %v", err)
		}
		// model.MoveCardRequest's json tags.
		expectFields(t, store.onlyRequestBody(t), map[string]any{
			"board_id": "board-1", "column_id": "col-1", "position": 7.5,
		})
	})

	t.Run("PatchCardBody patches body alone", func(t *testing.T) {
		store := newWireRecordingStore(t, 200, `{}`)
		if err := store.client().PatchCardBody("card-1", "the new body"); err != nil {
			t.Fatalf("PatchCardBody: %v", err)
		}
		sent := store.onlyRequestBody(t)
		expectFields(t, sent, map[string]any{"body": "the new body"})
		// kanban-store forwards this map to noteboard unchanged, so any other
		// key would edit a field the caller never asked to edit.
		if len(sent) != 1 {
			t.Errorf("PatchCardBody sent %d fields %v; it must patch body alone", len(sent), sent)
		}
	})

	t.Run("HoldCard", func(t *testing.T) {
		store := newWireRecordingStore(t, 200, `{}`)
		if err := store.client().HoldCard("card-1", "waiting on the operator"); err != nil {
			t.Fatalf("HoldCard: %v", err)
		}
		// holdCard decodes an anonymous struct with Reason `json:"reason"`.
		expectFields(t, store.onlyRequestBody(t), map[string]any{"reason": "waiting on the operator"})
	})

	t.Run("PutTicket", func(t *testing.T) {
		store := newWireRecordingStore(t, 200, `{}`)
		if err := store.client().PutTicket("card-1", "principal_000001", "email", "email", "msg-1"); err != nil {
			t.Fatalf("PutTicket: %v", err)
		}
		// model.TicketWriteRequest, which putCardTicket decodes with
		// DisallowUnknownFields: an extra key is a 400, a misspelled one too.
		sent := store.onlyRequestBody(t)
		want := map[string]any{
			"requester_principal_id": "principal_000001",
			"channel":                "email",
			"source_entity_type":     "email",
			"source_entity_ref":      "msg-1",
		}
		expectFields(t, sent, want)
		for field := range sent {
			if _, known := want[field]; !known {
				t.Errorf("request carries %q, which TicketWriteRequest does not declare and the server refuses", field)
			}
		}
	})

	t.Run("CreateCard", func(t *testing.T) {
		store := newWireRecordingStore(t, 201, `{"item":{"id":"card-1"}}`)
		_, err := store.client().CreateCard(CreateCardArgs{
			ColumnName: "Todo", Title: "a title", Body: "a body", Tags: []string{"x"},
			Priority: 3, Hold: true, HoldReason: "owned by a pipeline",
		})
		if err != nil {
			t.Fatalf("CreateCard: %v", err)
		}
		// model.CreateCardRequest's json tags.
		expectFields(t, store.onlyRequestBody(t), map[string]any{
			"title": "a title", "body": "a body", "column_id": "col-todo",
			"priority": float64(3), "hold": true, "hold_reason": "owned by a pipeline",
		})
	})

	t.Run("AddCardLink", func(t *testing.T) {
		store := newWireRecordingStore(t, 201, `{}`)
		if err := store.client().AddCardLink("card-1", "session", "sess-1", "classifier"); err != nil {
			t.Fatalf("AddCardLink: %v", err)
		}
		// model.CreateCardLinkRequest's json tags.
		expectFields(t, store.onlyRequestBody(t), map[string]any{
			"entity_type": "session", "entity_ref": "sess-1", "label": "classifier",
		})
	})

	t.Run("AddCardLink omits an empty label rather than sending it blank", func(t *testing.T) {
		store := newWireRecordingStore(t, 201, `{}`)
		if err := store.client().AddCardLink("card-1", "session", "sess-1", ""); err != nil {
			t.Fatalf("AddCardLink: %v", err)
		}
		// Label is a *string on the server: a blank one is stored as a blank
		// label, which is not the same row as no label.
		if label, present := store.onlyRequestBody(t)["label"]; present {
			t.Errorf("an empty label was sent as %#v; the client must omit the key", label)
		}
	})

	t.Run("AddEntityTag", func(t *testing.T) {
		store := newWireRecordingStore(t, 201, `{}`)
		if err := store.client().AddEntityTag("session", "sess-1", "review-settled"); err != nil {
			t.Fatalf("AddEntityTag: %v", err)
		}
		// model.CreateEntityTagRequest.Tag.
		expectFields(t, store.onlyRequestBody(t), map[string]any{"tag": "review-settled"})
	})
}

// ============================ response decoding ============================

// kanban-store answers this route with card_id/item pairs "so callers can spot
// orphans (item==null)". A card whose noteboard item was deleted comes back
// with a null item, and dropping that row turns a dangling reference into a
// card that is simply not there. The dispatcher counts these rows when it
// decides a parent's children are all settled.
func TestListCardsForEntityKeepsAnOrphanRow(t *testing.T) {
	store := newWireRecordingStore(t, 200, `[
		{"card_id":"card-live","item":{"title":"a real one"}},
		{"card_id":"card-orphan","item":null}
	]`)

	entries, err := store.client().ListCardsForEntity("parent_card", "card-9")
	if err != nil {
		t.Fatalf("ListCardsForEntity: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 — the orphan row was dropped", len(entries))
	}
	if entries[1].CardID != "card-orphan" {
		t.Errorf("entries[1].CardID = %q, want %q", entries[1].CardID, "card-orphan")
	}
	if entries[1].Item != nil {
		t.Errorf("entries[1].Item = %#v, want nil so the caller can spot the orphan", entries[1].Item)
	}
}

// A json tag that disagrees with the server decodes to an empty string, not an
// error. These are the fields callers filter on.
func TestReadsDecodeTheServersFieldNames(t *testing.T) {
	t.Run("ListEntityTags against model.EntityTag", func(t *testing.T) {
		store := newWireRecordingStore(t, 200,
			`[{"entity_type":"session","entity_ref":"sess-1","tag":"review-settled"}]`)
		tags, err := store.client().ListEntityTags("session", "sess-1")
		if err != nil {
			t.Fatalf("ListEntityTags: %v", err)
		}
		if len(tags) != 1 || tags[0] != (EntityTag{EntityType: "session", EntityRef: "sess-1", Tag: "review-settled"}) {
			t.Errorf("tags = %+v, want one row session/sess-1/review-settled", tags)
		}
	})

	t.Run("ListCardLinks against model.CardLink", func(t *testing.T) {
		store := newWireRecordingStore(t, 200,
			`[{"id":"link-1","card_id":"card-1","entity_type":"session","entity_ref":"sess-1","label":"tcl-dispatcher"}]`)
		links, err := store.client().ListCardLinks("card-1")
		if err != nil {
			t.Fatalf("ListCardLinks: %v", err)
		}
		if len(links) != 1 {
			t.Fatalf("got %d links, want 1", len(links))
		}
		// The dispatcher selects on EntityType and reads EntityRef as a session
		// id; empty ones would read as "this card has no session".
		got := links[0]
		if got.ID != "link-1" || got.CardID != "card-1" || got.EntityType != "session" ||
			got.EntityRef != "sess-1" || got.Label != "tcl-dispatcher" {
			t.Errorf("link = %+v, want every field carried through", got)
		}
	})

	t.Run("ListCardPlacements against model.Placement", func(t *testing.T) {
		store := newWireRecordingStore(t, 200,
			`[{"card_id":"card-1","board_id":"b1","column_id":"col-doing","position":2.5}]`)
		placements, err := store.client().ListCardPlacements("card-1")
		if err != nil {
			t.Fatalf("ListCardPlacements: %v", err)
		}
		want := CardPlacement{CardID: "card-1", BoardID: "b1", ColumnID: "col-doing", Position: 2.5}
		if len(placements) != 1 || placements[0] != want {
			t.Errorf("placements = %+v, want [%+v]", placements, want)
		}
	})
}
