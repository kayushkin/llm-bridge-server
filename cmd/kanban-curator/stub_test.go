package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The stubs below stand in for kanban-store and llm-bridge-server. Each fault
// field is a status code: zero means "answer normally", non-zero means answer
// with that status, which is how every failure this suite is about arrives in
// production — the board or the bridge answering, just not with 200.

// stubCard is one In Progress card as the curator meets it: a placement, the
// noteboard item behind it, and the session link that says what to close it
// against.
type stubCard struct {
	cardID    string
	title     string
	body      string
	noItem    bool   // the placement resolves to no noteboard item
	sessionID string // "" attaches no session link at all
}

type stubKanban struct {
	t         *testing.T
	boardName string
	cards     []stubCard
	settled   map[string]bool   // session id -> reviewer has signed off
	failLinks int               // status to answer GET /api/cards/{id}/links with
	failTags  int               // status to answer GET /api/entities/session/{id}/tags with
	failPatch int               // status to answer PATCH /api/cards/{id} with
	failMove  int               // status to answer POST /api/cards/{id}/move with
	moved     map[string]string // card id -> column id it was moved to
	patched   map[string]string // card id -> body it was patched with
	server    *httptest.Server
}

const (
	stubBoardID   = "board-1"
	stubColDone   = "col-done"
	stubColFailed = "col-failed"
	stubColInProg = "col-inprogress"
)

func newStubKanban(t *testing.T, boardName string, cards ...stubCard) *stubKanban {
	k := &stubKanban{
		t: t, boardName: boardName, cards: cards,
		settled: map[string]bool{},
		moved:   map[string]string{},
		patched: map[string]string{},
	}
	k.server = httptest.NewServer(http.HandlerFunc(k.serve))
	t.Cleanup(k.server.Close)
	return k
}

func (k *stubKanban) URL() string { return k.server.URL }

func (k *stubKanban) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/api/boards/"+stubBoardID && r.Method == "GET":
		writeStubJSON(w, 200, map[string]string{"id": stubBoardID, "name": k.boardName})

	case p == "/api/boards/"+stubBoardID+"/cards" && r.Method == "GET":
		writeStubJSON(w, 200, k.boardView())

	case strings.HasPrefix(p, "/api/entities/session/") && strings.HasSuffix(p, "/tags"):
		if k.failTags != 0 {
			http.Error(w, "tag store unavailable", k.failTags)
			return
		}
		ref := strings.TrimSuffix(strings.TrimPrefix(p, "/api/entities/session/"), "/tags")
		var tags []map[string]string
		if k.settled[ref] {
			tags = append(tags, map[string]string{"tag": tagReviewSettled})
		}
		writeStubJSON(w, 200, tags)

	case strings.HasSuffix(p, "/links") && r.Method == "GET":
		if k.failLinks != 0 {
			http.Error(w, "links unavailable", k.failLinks)
			return
		}
		cardID := strings.TrimSuffix(strings.TrimPrefix(p, "/api/cards/"), "/links")
		var links []map[string]any
		for _, c := range k.cards {
			if c.cardID == cardID && c.sessionID != "" {
				links = append(links, map[string]any{
					"card_id": cardID, "entity_type": "session", "entity_ref": c.sessionID,
				})
			}
		}
		writeStubJSON(w, 200, links)

	case strings.HasSuffix(p, "/move") && r.Method == "POST":
		if k.failMove != 0 {
			http.Error(w, "move refused", k.failMove)
			return
		}
		var req struct {
			ColumnID string `json:"column_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		k.moved[strings.TrimSuffix(strings.TrimPrefix(p, "/api/cards/"), "/move")] = req.ColumnID
		writeStubJSON(w, 200, map[string]string{"status": "ok"})

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

// boardView renders the three columns the curator needs, with every stub card in
// In Progress. Done and Failed are present but empty: the curator only reads
// their ids.
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
	col := func(id, name string, cs []map[string]any) map[string]any {
		return map[string]any{
			"column": map[string]any{"id": id, "board_id": stubBoardID, "name": name},
			"cards":  cs,
		}
	}
	return map[string]any{
		"board": map[string]any{"id": stubBoardID, "name": k.boardName},
		"columns": []map[string]any{
			col(stubColInProg, columnInProgress, cards),
			col(stubColDone, columnDone, nil),
			col(stubColFailed, columnFailed, nil),
		},
	}
}

// stubBridge answers GET /sessions/{id} for the states this suite needs.
type stubBridge struct {
	t         *testing.T
	states    map[string]string // session id -> state; a missing id answers 404
	purposes  map[string]string // session id -> purpose; purposeAutoworker gates the review wait
	updatedAt map[string]string // session id -> RFC3339 updated_at
	fail      int               // status to answer every read with (not 404)
	server    *httptest.Server
}

func newStubBridge(t *testing.T) *stubBridge {
	b := &stubBridge{t: t, states: map[string]string{}, purposes: map[string]string{}, updatedAt: map[string]string{}}
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
	if b.fail != 0 {
		http.Error(w, "bridge unavailable", b.fail)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/sessions/")
	state, ok := b.states[id]
	if !ok {
		http.Error(w, "no such session", 404)
		return
	}
	// llm-bridge-server always names a purpose, and the curator refuses a
	// session without one, so a session the test did not mark as autoworker's
	// is an interactive one.
	purpose := b.purposes[id]
	if purpose == "" {
		purpose = "interactive"
	}
	updated := b.updatedAt[id]
	if updated == "" {
		updated = time.Now().UTC().Format(time.RFC3339)
	}
	writeStubJSON(w, 200, map[string]string{"session_id": id, "state": state, "updated_at": updated, "purpose": purpose})
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
// measured on cmd/spend-ceiling-guard on 2026-08-29; here it would fire on
// six of the six Record sites.
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

func minutesAgo(n int) string {
	return time.Now().UTC().Add(-time.Duration(n) * time.Minute).Format(time.RFC3339)
}

var _ = fmt.Sprintf
