package issuesclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/boardvocabulary"
)

// Ported from 8ff8f0b (sweep/board-vocabulary-in-ask-and-spend-ceiling-guard).
// Its companion test for a card with no session is covered on main by
// TestAnEmptyEntityRefIsSkippedRatherThanFailed.

// TestSessionCardLinkSurvivesTheWriterToReaderRoundTrip pins the one wire value
// on the spend path.
//
// CreateCard writes a card link naming the session that did the work, and three
// separate binaries read that link back to decide what a task has cost:
// cmd/spend-ceiling-guard, cmd/autoworker/spend.go and cmd/autoworker-reviewer.
// Every one of them selects links with the filter reproduced below.
//
// The failure this guards is silent and expensive. If the writer's entity type
// and the readers' filter ever drift apart, ListCardLinks still returns rows,
// the filter still runs, and it simply matches nothing. Zero session links
// reads as zero sessions, zero sessions reads as zero dollars spent, and a
// spend ceiling that is never reached never holds anything — so a task with a
// $5 cap keeps dispatching. There is no error and no empty result that looks
// wrong; the guard just reports "0 held" forever, which is what it reports on a
// healthy box too.
//
// The four sites share a constant today, so this test asserts the thing a
// shared constant cannot: the literal that actually crosses the wire, and that
// the readers' filter selects it. Re-hardcoding either side fails here.
func TestSessionCardLinkSurvivesTheWriterToReaderRoundTrip(t *testing.T) {
	const (
		cardID    = "card-1"
		sessionID = "autoworker-nightly-1754640000000000000"
	)

	// The store, reduced to the three routes CreateCard touches. Links are kept
	// exactly as they arrived so the read path sees the bytes the write path
	// sent, rather than a struct the test filled in itself.
	var stored []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/boards/board-1/cards", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"item": map[string]any{"id": cardID}})
	})
	mux.HandleFunc("/cards/"+cardID+"/links", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			var link map[string]any
			if err := json.Unmarshal(body, &link); err != nil {
				t.Errorf("card link body is not JSON: %v", err)
			}
			stored = append(stored, link)
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			out := make([]CardLink, 0, len(stored))
			for _, l := range stored {
				et, _ := l["entity_type"].(string)
				ref, _ := l["entity_ref"].(string)
				label, _ := l["label"].(string)
				out = append(out, CardLink{CardID: cardID, EntityType: et, EntityRef: ref, Label: label})
			}
			_ = json.NewEncoder(w).Encode(out)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL)
	c.BoardID = "board-1"
	c.ColumnsByName = map[string]string{boardvocabulary.ColumnInProgress: "col-1"}

	got, err := c.CreateCard(CreateCardArgs{
		ColumnName: boardvocabulary.ColumnInProgress,
		Title:      "a task that costs money",
		SessionID:  sessionID,
		Provider:   "claude-code",
	})
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if got != cardID {
		t.Fatalf("CreateCard returned %q, want %q", got, cardID)
	}

	// 1. The literal that crossed the wire. Spelled out rather than compared to
	//    the constant: if someone changes boardvocabulary.LinkTypeSession, existing
	//    rows in kanban-store still say "session" and this must fail.
	var sessionLinks int
	for _, l := range stored {
		if l["entity_type"] == "session" {
			sessionLinks++
			if l["entity_ref"] != sessionID {
				t.Errorf("session link points at %q, want %q", l["entity_ref"], sessionID)
			}
		}
	}
	if sessionLinks != 1 {
		t.Fatalf("wrote %d links with entity_type \"session\", want exactly 1 — spend attribution reads this row", sessionLinks)
	}

	// 2. The readers' filter, reproduced verbatim from cmd/spend-ceiling-guard,
	//    cmd/autoworker/spend.go and cmd/autoworker-reviewer.
	links, err := c.ListCardLinks(cardID)
	if err != nil {
		t.Fatalf("ListCardLinks: %v", err)
	}
	var sessions []string
	for _, l := range links {
		if l.EntityType == boardvocabulary.LinkTypeSession {
			sessions = append(sessions, l.EntityRef)
		}
	}
	if len(sessions) != 1 || sessions[0] != sessionID {
		t.Fatalf("the spend readers' filter selected %v, want exactly [%s] — a ceiling cannot fire on a session it cannot see", sessions, sessionID)
	}
}
