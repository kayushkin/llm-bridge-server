package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The stubs below stand in for kanban-store and llm-bridge-server. Each fault
// field is a status code: zero means "answer normally", non-zero means answer
// with that status, which is how every failure this suite is about arrives in
// production — the board or the bridge answering, just not with 200.

// stubCard is one In Progress card as the reviewer meets it: a placement, the
// noteboard item behind it, and the session link that says what to review.
type stubCard struct {
	cardID    string
	title     string
	body      string
	noItem    bool     // the placement resolves to no noteboard item
	sessionID string   // "" attaches no session link at all
	tags      []string // entity tags already on that session
}

type stubKanban struct {
	t          *testing.T
	boardName  string
	cards      []stubCard
	noBoard    bool
	failLinks  int // status to answer GET /api/cards/{id}/links with
	failTags   int // status to answer GET /api/entities/session/{id}/tags with
	failAddTag int // status to answer POST /api/entities/session/{id}/tags with
	// failSettleTag answers only the review-settled write with this status, so a
	// test can let the pass tag land and refuse the settle that follows it.
	failSettleTag int
	failPatch     int // status to answer PATCH /api/cards/{id} with

	added   map[string][]string // session id -> tags added this run
	patched map[string]string   // card id -> body it was patched with
	server  *httptest.Server
}

const (
	stubBoardID   = "board-1"
	stubColInProg = "col-inprogress"
)

func newStubKanban(t *testing.T, cards ...stubCard) *stubKanban {
	k := &stubKanban{
		t: t, boardName: defaultBoardName, cards: cards,
		added:   map[string][]string{},
		patched: map[string]string{},
	}
	k.server = httptest.NewServer(http.HandlerFunc(k.serve))
	t.Cleanup(k.server.Close)
	return k
}

func (k *stubKanban) URL() string { return k.server.URL }

func (k *stubKanban) card(id string) *stubCard {
	for i := range k.cards {
		if k.cards[i].cardID == id {
			return &k.cards[i]
		}
	}
	return nil
}

func (k *stubKanban) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/api/boards" && r.Method == "GET":
		if k.noBoard {
			writeStubJSON(w, 200, []map[string]string{})
			return
		}
		writeStubJSON(w, 200, []map[string]string{{"id": stubBoardID, "name": k.boardName}})

	case p == "/api/boards/"+stubBoardID+"/cards" && r.Method == "GET":
		writeStubJSON(w, 200, k.boardView())

	case strings.HasPrefix(p, "/api/entities/session/") && strings.HasSuffix(p, "/tags") && r.Method == "GET":
		if k.failTags != 0 {
			http.Error(w, "tag store unavailable", k.failTags)
			return
		}
		ref := strings.TrimSuffix(strings.TrimPrefix(p, "/api/entities/session/"), "/tags")
		var tags []map[string]string
		for _, c := range k.cards {
			if c.sessionID == ref {
				for _, t := range c.tags {
					tags = append(tags, map[string]string{"entity_type": "session", "entity_ref": ref, "tag": t})
				}
			}
		}
		for _, t := range k.added[ref] {
			tags = append(tags, map[string]string{"entity_type": "session", "entity_ref": ref, "tag": t})
		}
		writeStubJSON(w, 200, tags)

	case strings.HasPrefix(p, "/api/entities/session/") && strings.HasSuffix(p, "/tags") && r.Method == "POST":
		if k.failAddTag != 0 {
			http.Error(w, "tag refused", k.failAddTag)
			return
		}
		ref := strings.TrimSuffix(strings.TrimPrefix(p, "/api/entities/session/"), "/tags")
		var req struct {
			Tag string `json:"tag"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if k.failSettleTag != 0 && req.Tag == tagSettled {
			http.Error(w, "settle refused", k.failSettleTag)
			return
		}
		k.added[ref] = append(k.added[ref], req.Tag)
		writeStubJSON(w, 201, map[string]string{"status": "ok"})

	case strings.HasSuffix(p, "/links") && r.Method == "GET":
		if k.failLinks != 0 {
			http.Error(w, "links unavailable", k.failLinks)
			return
		}
		cardID := strings.TrimSuffix(strings.TrimPrefix(p, "/api/cards/"), "/links")
		var links []map[string]any
		if c := k.card(cardID); c != nil && c.sessionID != "" {
			links = append(links, map[string]any{
				"card_id": cardID, "entity_type": "session", "entity_ref": c.sessionID,
			})
		}
		writeStubJSON(w, 200, links)

	case strings.HasPrefix(p, "/api/cards/") && r.Method == "PATCH":
		if k.failPatch != 0 {
			http.Error(w, "patch refused", k.failPatch)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		body, _ := req["body"].(string)
		k.patched[strings.TrimPrefix(p, "/api/cards/")] = body
		writeStubJSON(w, 200, map[string]string{"status": "ok"})

	default:
		k.t.Errorf("stub kanban-store: unexpected %s %s", r.Method, p)
		http.Error(w, "unexpected", 500)
	}
}

// boardView renders In Progress with every stub card in it. The reviewer reads
// no other column.
func (k *stubKanban) boardView() map[string]any {
	var cards []map[string]any
	for _, c := range k.cards {
		cv := map[string]any{
			"placement": map[string]any{
				"card_id": c.cardID, "board_id": stubBoardID, "column_id": stubColInProg,
			},
		}
		if !c.noItem {
			cv["item"] = map[string]any{"id": c.cardID, "title": c.title, "body": c.body}
		}
		cards = append(cards, cv)
	}
	return map[string]any{
		"board": map[string]any{"id": stubBoardID, "name": k.boardName},
		"columns": []map[string]any{{
			"column": map[string]any{"id": stubColInProg, "board_id": stubBoardID, "name": columnInProgress},
			"cards":  cards,
		}},
	}
}

// stubBridge answers GET /sessions/{id} and POST /sessions/{id}/send.
type stubBridge struct {
	t        *testing.T
	states   map[string]string // session id -> state; a missing id answers 404
	purposes map[string]string // session id -> purpose; unset answers "interactive"
	failRead int               // status to answer every GET with (not 404)
	failSend int               // status to answer every send with
	sent     map[string]int    // session id -> nudges delivered
	server   *httptest.Server
}

func newStubBridge(t *testing.T) *stubBridge {
	b := &stubBridge{t: t, states: map[string]string{}, purposes: map[string]string{}, sent: map[string]int{}}
	b.server = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.server.Close)
	return b
}

func (b *stubBridge) URL() string { return b.server.URL }

func (b *stubBridge) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/sessions/") {
		b.t.Errorf("stub bridge: unexpected %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", 500)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/send") && r.Method == "POST" {
		if b.failSend != 0 {
			http.Error(w, "send refused", b.failSend)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/send")
		b.sent[id]++
		writeStubJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if b.failRead != 0 {
		http.Error(w, "bridge unavailable", b.failRead)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/sessions/")
	state, ok := b.states[id]
	if !ok {
		http.Error(w, "no such session", 404)
		return
	}
	// llm-bridge-server always names a purpose; a session the test did not mark
	// as autoworker's is an interactive one.
	purpose := b.purposes[id]
	if purpose == "" {
		purpose = "interactive"
	}
	writeStubJSON(w, 200, map[string]string{"session_id": id, "state": state, "updated_at": "2026-08-29T00:00:00Z", "purpose": purpose})
}

func writeStubJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// shortfallLine returns the single `run degraded:` line out of a captured log,
// and fails the test if there is not exactly one.
//
// It exists because asserting with strings.Contains over the WHOLE log is
// answered by whatever else happened to print — and this command logs a line
// naming the same card immediately before every Record it makes, so a shortfall
// report that dropped the card id entirely would stay green. That trap was
// measured on cmd/spend-ceiling-guard and again on cmd/kanban-curator on
// 2026-08-29; here it would fire on eight of the eight Record sites.
func shortfallLine(t *testing.T, out string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "run degraded:") {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one `run degraded:` line, got %d in:\n%s", len(found), out)
	}
	return found[0]
}

func requireNoShortfall(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "run degraded:") {
		t.Fatalf("a deliberate skip was reported as a shortfall:\n%s", out)
	}
}
