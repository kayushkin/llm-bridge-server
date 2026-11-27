package issuesclient

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/boardvocabulary"
)

// This file pins what scripts/sabotage-issuesclient.py found no other test in
// this package watching: board bootstrap (which board EnsureBoard binds, which
// columns it creates, and the auto_status it gives Done and Failed), the two
// CreateCard guards that run before any request, a create response carrying no
// id, and the timeout and trailing-slash trim New sets up.
//
// Ported 2026-09-26 from 021510ad on test/kanbanclient-has-a-sabotage-scorer,
// which pinned 25 mechanisms. Main has since grown client_protocol_test.go,
// which pins every route, status and field name, so the branch's tests for
// those were dropped as duplicates. Each test kept here closes a row the scorer
// reported UNNOTICED on main before the port: 22/33 before, 33/33 after.
//
// Every test below names the failure it stops rather than the line it reads,
// because none of these failures announce themselves. A dropped guard posts to
// a blank id and the store answers; a drifted auto_status is written once, when
// the column is created, and nothing downstream reads it back.
//
// # Why the fake store asserts on the request, not on a 404
//
// These tests answer every request with the canned response the call expects
// and then assert on the path and body the client actually sent. The assertion
// is the thing that fails. The fake is a plain handler and not a ServeMux:
// ServeMux cleans "//api/boards" to "/api/boards" and redirects, which would
// hide a base URL whose trailing slash stopped being trimmed.

// recordedRequest is one call the fake store saw.
type recordedRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

// recordingStore answers every request with the canned response the test gives
// it and records what it was asked for.
type recordingStore struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
}

func newRecordingStore(t *testing.T, respond http.HandlerFunc) *recordingStore {
	t.Helper()
	s := &recordingStore{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := recordedRequest{Method: r.Method, Path: r.URL.Path}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rec.Body); err != nil {
				t.Errorf("%s %s: request body is not a JSON object: %v", r.Method, r.URL.Path, err)
			}
		}
		s.mu.Lock()
		s.requests = append(s.requests, rec)
		s.mu.Unlock()

		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		respond(w, r)
	}))
	t.Cleanup(s.server.Close)
	return s
}

// only returns the single request the store saw, failing if it saw any other
// number — a call that made two requests, or none, is not the call under test.
func (s *recordingStore) only(t *testing.T) recordedRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) != 1 {
		t.Fatalf("store saw %d requests, want exactly 1: %+v", len(s.requests), s.requests)
	}
	return s.requests[0]
}

func (s *recordingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// client returns a client bound to this store with the board and column ids
// already resolved, so a test about card creation does not have to bootstrap a
// board first.
func (s *recordingStore) client() *Client {
	c := New(s.server.URL, "")
	c.BoardID = "board-1"
	c.ColumnsByName = map[string]string{boardvocabulary.ColumnInProgress: "col-1"}
	return c
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ============================ board bootstrap ============================

// fakeBoardStore is kanban-store reduced to the four routes EnsureBoard walks,
// with enough state to answer them consistently. It keeps the raw payloads it
// was posted, because what a column is CREATED with is the thing under test —
// EnsureBoard is the only writer of auto_status in this repo.
type fakeBoardStore struct {
	boards  []Board
	columns map[string][]Column

	createdBoards  []map[string]any
	createdColumns []map[string]any

	minted int
}

func (f *fakeBoardStore) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		decode := func() map[string]any {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("%s %s: body is not a JSON object: %v", r.Method, r.URL.Path, err)
			}
			return payload
		}

		switch {
		// GET/POST /api/boards
		case len(parts) == 2 && parts[1] == "boards":
			if r.Method == http.MethodGet {
				writeJSON(w, http.StatusOK, f.boards)
				return
			}
			payload := decode()
			f.createdBoards = append(f.createdBoards, payload)
			f.minted++
			name, _ := payload["name"].(string)
			board := Board{ID: fmt.Sprintf("board-%d", f.minted), Name: name}
			f.boards = append(f.boards, board)
			writeJSON(w, http.StatusCreated, board)

		// GET/POST /api/boards/{id}/columns
		case len(parts) == 4 && parts[1] == "boards" && parts[3] == "columns":
			boardID := parts[2]
			if r.Method == http.MethodGet {
				writeJSON(w, http.StatusOK, f.columns[boardID])
				return
			}
			payload := decode()
			f.createdColumns = append(f.createdColumns, payload)
			f.minted++
			name, _ := payload["name"].(string)
			autoStatus, _ := payload["auto_status"].(string)
			col := Column{ID: fmt.Sprintf("col-%d", f.minted), BoardID: boardID, Name: name, AutoStatus: autoStatus}
			f.columns[boardID] = append(f.columns[boardID], col)
			writeJSON(w, http.StatusCreated, col)

		default:
			t.Errorf("fake board store was asked for a route it does not serve: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// columnCreatedAs returns the payload the named column was created with.
func (f *fakeBoardStore) columnCreatedAs(name string) (map[string]any, bool) {
	for _, payload := range f.createdColumns {
		if payload["name"] == name {
			return payload, true
		}
	}
	return nil, false
}

// TestEnsureBoardBindsTheBoardNamedAndFinishesItsColumnSet covers the whole
// bootstrap in one pass, because the four things it asserts are one contract:
// after EnsureBoard returns, the client is bound to the right board and can
// place a card in any column the caller asked for.
//
// The failures each assertion stops:
//
//   - Bound to the wrong board. Board lookup is by name, and the wanted board is
//     deliberately not the first one the store lists here. A client that binds
//     to whichever board came back first writes every card onto a stranger's
//     board and reports success doing it.
//   - A half-built board. EnsureBoard creates the columns it found missing; if
//     it stops, the first CreateCard for a missing column fails with "column
//     missing on board" — at dispatch time, on a board that looks fine.
//   - Column ids cached from the ones it FOUND rather than the ones it ensured.
//     Same symptom, and it survives a second run, because the column now exists
//     and is found next time.
//
// auto_status has a test of its own below; it is a different kind of failure.
func TestEnsureBoardBindsTheBoardNamedAndFinishesItsColumnSet(t *testing.T) {
	store := &fakeBoardStore{
		// The wanted board is listed second on purpose.
		boards: []Board{
			{ID: "board-other", Name: "Some Other Board"},
			{ID: "board-wanted", Name: boardvocabulary.BoardAgentRuns},
		},
		columns: map[string][]Column{
			"board-other":  {{ID: "col-other", BoardID: "board-other", Name: boardvocabulary.ColumnQueued}},
			"board-wanted": {{ID: "col-queued", BoardID: "board-wanted", Name: boardvocabulary.ColumnQueued}},
		},
	}
	srv := httptest.NewServer(store.handler(t))
	defer srv.Close()

	c := New(srv.URL, "")
	want := boardvocabulary.AgentRunsColumns()
	if err := c.EnsureBoard(boardvocabulary.BoardAgentRuns, want); err != nil {
		t.Fatalf("EnsureBoard: %v", err)
	}

	if c.BoardID != "board-wanted" {
		t.Errorf("bound to board %q, want %q — a board is found by its name, never by its position in the list",
			c.BoardID, "board-wanted")
	}

	// Every column the caller asked for is creatable-or-present AND resolvable
	// on the client afterwards.
	for _, name := range want {
		id, ok := c.ColumnsByName[name]
		if !ok || id == "" {
			t.Errorf("column %q is missing from ColumnsByName after EnsureBoard — CreateCard cannot place a card there", name)
			continue
		}
		var found bool
		for _, col := range store.columns["board-wanted"] {
			if col.ID == id {
				if col.Name != name {
					t.Errorf("ColumnsByName[%q] points at column %q, which the store calls %q", name, id, col.Name)
				}
				found = true
			}
		}
		if !found {
			t.Errorf("ColumnsByName[%q] = %q, which is not a column on the bound board", name, id)
		}
	}

	// The pre-existing column was reused rather than duplicated.
	if _, created := store.columnCreatedAs(boardvocabulary.ColumnQueued); created {
		t.Errorf("EnsureBoard created %q, which already existed — it must be idempotent", boardvocabulary.ColumnQueued)
	}
	if got := len(store.createdColumns); got != len(want)-1 {
		t.Errorf("created %d columns, want %d — one per column that was missing", got, len(want)-1)
	}
}

// TestEnsureBoardGivesDoneAndFailedTheAutoStatusesThatDecideWhatClosingACardDoes
// pins the two literals that reach out of kanban-store and edit the user's own
// noteboard queue.
//
// auto_status on a column is what makes MOVING a card rewrite the status of the
// noteboard todo behind it. EnsureBoard is the only writer of it in this repo,
// and it is written once, when the column is first created — so a drift here is
// not fixed by a later run, and nothing downstream can detect it. "Failed"
// carrying the Done column's "archived" -> "done" swap marks an ABANDONED task
// as completed in the user's own list: the queue then reads as work that was
// finished by someone, which is the one wrong answer nobody re-checks.
//
// The values are spelled out rather than compared to a constant. They are wire
// values noteboard accepts ("done", "archived"); rows already written into
// kanban-store carry these strings, so renaming a constant must fail here.
func TestEnsureBoardGivesDoneAndFailedTheAutoStatusesThatDecideWhatClosingACardDoes(t *testing.T) {
	store := &fakeBoardStore{
		boards:  []Board{{ID: "board-1", Name: boardvocabulary.BoardAgentRuns}},
		columns: map[string][]Column{"board-1": nil},
	}
	srv := httptest.NewServer(store.handler(t))
	defer srv.Close()

	c := New(srv.URL, "")
	if err := c.EnsureBoard(boardvocabulary.BoardAgentRuns, boardvocabulary.AgentRunsColumns()); err != nil {
		t.Fatalf("EnsureBoard: %v", err)
	}

	wantAutoStatus := map[string]string{
		boardvocabulary.ColumnDone:   "done",
		boardvocabulary.ColumnFailed: "archived",
	}
	for _, name := range boardvocabulary.AgentRunsColumns() {
		payload, created := store.columnCreatedAs(name)
		if !created {
			t.Errorf("column %q was never created", name)
			continue
		}
		got, present := payload["auto_status"]
		want, shouldHave := wantAutoStatus[name]
		switch {
		case shouldHave && got != want:
			t.Errorf("column %q was created with auto_status %v, want %q — this is what closing a card does to the todo behind it",
				name, got, want)
		case !shouldHave && present:
			t.Errorf("column %q was created with auto_status %v; only %q and %q may rewrite a noteboard item's status",
				name, got, boardvocabulary.ColumnDone, boardvocabulary.ColumnFailed)
		}
	}
}

// TestEnsureBoardCreatesAMissingBoardUnderTheNameItWasAskedFor pins the name on
// the create-board payload.
//
// Every caller finds this board again by name on the next run. A board created
// under any other name is found by nobody, so the next tick creates another
// one, and the cards spread across a pile of boards that each look empty.
func TestEnsureBoardCreatesAMissingBoardUnderTheNameItWasAskedFor(t *testing.T) {
	store := &fakeBoardStore{columns: map[string][]Column{}}
	srv := httptest.NewServer(store.handler(t))
	defer srv.Close()

	c := New(srv.URL, "")
	if err := c.EnsureBoard(boardvocabulary.BoardAutoworkerQueue, []string{boardvocabulary.ColumnQueued}); err != nil {
		t.Fatalf("EnsureBoard: %v", err)
	}
	if len(store.createdBoards) != 1 {
		t.Fatalf("store was asked to create %d boards, want 1", len(store.createdBoards))
	}
	if got := store.createdBoards[0]["name"]; got != boardvocabulary.BoardAutoworkerQueue {
		t.Errorf("created a board named %q, want %q — the next run looks it up by this name",
			got, boardvocabulary.BoardAutoworkerQueue)
	}
}

// ============================ card creation ============================

// TestCreateCardRefusesAResponseThatNamesNoCard stops a card id of "" being
// handed back to the caller.
//
// The caller's next move is to link the new card to a session, so an empty id
// posts those links to /api/cards//links. Whatever that route does, the links do
// not reach the card — and the caller was told the card was created.
func TestCreateCardRefusesAResponseThatNamesNoCard(t *testing.T) {
	store := newRecordingStore(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"item": map[string]any{}})
	})

	id, err := store.client().CreateCard(CreateCardArgs{
		ColumnName: boardvocabulary.ColumnInProgress,
		Title:      "a card the store did not name",
		SessionID:  "sess-1",
	})
	if err == nil {
		t.Fatalf("CreateCard returned id %q and no error for a response carrying no id", id)
	}
}

// TestCreateCardFailsBeforeTheWireWhenTheBoardOrColumnIsUnresolved pins the two
// guards that run before any request is made.
//
// Both are about a URL or a payload built from a blank id. Without the board
// guard the card is posted to /api/boards//cards; without the column guard it is
// posted with column_id "", which the store accepts. The test asserts that NO
// request was made, not merely that an error came back — a mutation that drops
// either guard still returns an error, from the store, after writing something.
func TestCreateCardFailsBeforeTheWireWhenTheBoardOrColumnIsUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setUp func(*Client)
	}{
		{
			name: "EnsureBoard was never called",
			setUp: func(c *Client) {
				c.BoardID = ""
				c.ColumnsByName = map[string]string{boardvocabulary.ColumnInProgress: "col-1"}
			},
		},
		{
			name:  "the named column is not on the board",
			setUp: func(c *Client) { c.BoardID = "board-1"; c.ColumnsByName = map[string]string{} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newRecordingStore(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusCreated, map[string]any{"item": map[string]any{"id": "card-1"}})
			})
			c := New(store.server.URL, "")
			tc.setUp(c)

			if _, err := c.CreateCard(CreateCardArgs{
				ColumnName: boardvocabulary.ColumnInProgress,
				Title:      "a card with nowhere to go",
			}); err == nil {
				t.Error("CreateCard reported success")
			}
			if n := store.count(); n != 0 {
				t.Errorf("CreateCard made %d requests, want 0 — an unresolved id must fail before the wire, not on it", n)
			}
		})
	}
}

// ============================ transport ============================

// TestNewSetsARequestTimeoutSoAWedgedStoreCannotHangATick pins the timeout.
//
// Go's zero value here means "wait forever", and every caller of this client is
// a cron tick. A wedged kanban-store then does not fail the tick, it holds it
// open — and the scheduler drops the next tick of a job whose previous run is
// still in flight, so one hung request quietly stops the job rather than erroring
// once and recovering on the next tick.
func TestNewSetsARequestTimeoutSoAWedgedStoreCannotHangATick(t *testing.T) {
	if timeout := New("http://127.0.0.1:1", "").HTTP.Timeout; timeout <= 0 {
		t.Fatalf("New built a client with timeout %v; a zero timeout waits forever", timeout)
	}
}

// TestABaseURLWithATrailingSlashDoesNotDoubleTheSlashOnTheWire pins the trim.
//
// The base URL comes from configuration, so a trailing slash is a matter of how
// someone typed it. Untrimmed, every request goes to //api/... — a path most
// routers answer with a 404 or a redirect, which is a whole binary failing on a
// character nobody can see.
//
// The fake store is a plain handler, not a ServeMux: ServeMux would clean the
// doubled slash and redirect to the right path, and the client follows
// redirects, so this test would pass while the requests were malformed.
func TestABaseURLWithATrailingSlashDoesNotDoubleTheSlashOnTheWire(t *testing.T) {
	store := newRecordingStore(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []Board{})
	})

	c := New(store.server.URL+"/", "")
	if _, err := c.FindBoard("Agent Runs"); err != nil {
		t.Fatalf("FindBoard: %v", err)
	}
	if req := store.only(t); req.Path != "/api/boards" {
		t.Errorf("requested %q, want %q — the base URL's trailing slash must be trimmed once, not sent", req.Path, "/api/boards")
	}
}
