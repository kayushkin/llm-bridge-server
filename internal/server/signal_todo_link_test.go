package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// testServerWithKanban is testServer wired to a stub kanban-store. handler
// serves the session-cards reverse lookup; pass nil for a store that is
// configured but unreachable.
func testServerWithKanban(t *testing.T, handler http.HandlerFunc) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	kanbanURL := "http://127.0.0.1:0" // configured, never answers
	if handler != nil {
		stub := httptest.NewServer(handler)
		t.Cleanup(stub.Close)
		kanbanURL = stub.URL
	}

	cfg := &config.Config{
		ImagesDir:       filepath.Join(dir, "images"),
		BridgePrefsPath: filepath.Join(dir, "prefs.json"),
		LogStoreURL:     "http://localhost:0",
		KanbanStoreURL:  kanbanURL,
	}
	return New(st, nil, nil, nil, nil, nil, nil, cfg), st
}

// cardsForSession serves the reverse lookup, answering with one card id for
// the session it is asked about.
func cardsForSession(todoID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"card_id":"` + todoID + `","item":{"id":"` + todoID + `"}}]`))
	}
}

func TestToolSignalsCarryTheLinkedTodo(t *testing.T) {
	srv, st := testServerWithKanban(t, cardsForSession("todo-42"))
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)

	srv.recordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

	signals, err := st.ListSignals(store.SignalFilter{SessionID: "br_1"})
	if err != nil {
		t.Fatalf("list signals: %v", err)
	}
	if len(signals) != 2 {
		t.Fatalf("got %d signals, want 2", len(signals))
	}
	// Every question in one request belongs to the same session, so they all
	// propagate to the same todo.
	for _, sig := range signals {
		if sig.LinkedTodoID != "todo-42" {
			t.Errorf("signal %q linked_todo_id = %q, want todo-42", sig.Title, sig.LinkedTodoID)
		}
	}
}

func TestSignalsAreUnlinkedWhenTheSessionHasNoTodo(t *testing.T) {
	srv, st := testServerWithKanban(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)

	srv.recordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

	signals, _ := st.ListSignals(store.SignalFilter{SessionID: "br_1"})
	if len(signals) == 0 {
		t.Fatal("no signals recorded")
	}
	for _, sig := range signals {
		if sig.LinkedTodoID != "" {
			t.Errorf("linked_todo_id = %q, want empty for a session with no link", sig.LinkedTodoID)
		}
	}
}

// Both producers stamp the link, not just the tool one. A derived signal on
// an autoworker session is the case the feature exists for: nobody is reading
// that chat, and the todo is where the blocker has to appear.
func TestDerivedSignalsCarryTheLinkedTodo(t *testing.T) {
	srv, st := testServerWithKanban(t, cardsForSession("todo-42"))
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)

	srv.recordDerivedSignal(sess, &turnClassification{
		Kind:  turnSignalQuestion,
		Title: "Which branch should I ship from?",
	}, msg.SignalKindQuestion)

	signals, err := st.ListSignals(store.SignalFilter{SessionID: "br_1"})
	if err != nil {
		t.Fatalf("list signals: %v", err)
	}
	if len(signals) != 1 {
		t.Fatalf("got %d signals, want 1", len(signals))
	}
	if signals[0].LinkedTodoID != "todo-42" {
		t.Errorf("linked_todo_id = %q, want todo-42", signals[0].LinkedTodoID)
	}
}

// The lookup is a courtesy on top of a signal that must be recorded either
// way. A kanban-store that is down, wedged or absent may cost the todo
// pointer; it may never cost the signal.
func TestSignalIsStillRecordedWhenTheLookupFails(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"store errors":      func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"store gives junk":  func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<html>`)) },
		"store unreachable": nil,
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			srv, st := testServerWithKanban(t, handler)
			sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)

			srv.recordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

			signals, _ := st.ListSignals(store.SignalFilter{SessionID: "br_1"})
			if len(signals) != 2 {
				t.Fatalf("got %d signals, want 2 — a failed link lookup must not cost the signal", len(signals))
			}
			for _, sig := range signals {
				if sig.LinkedTodoID != "" {
					t.Errorf("linked_todo_id = %q, want empty after a failed lookup", sig.LinkedTodoID)
				}
			}
		})
	}
}

// With no kanban-store configured the lookup is off, not broken: no request
// is attempted and every signal is minted unlinked.
func TestNoKanbanStoreConfiguredMeansNoLookup(t *testing.T) {
	srv, st := testServer(t)
	if srv.kanbanClient != nil {
		t.Fatal("expected no kanban client when KanbanStoreURL is empty")
	}
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)
	srv.recordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

	signals, _ := st.ListSignals(store.SignalFilter{SessionID: "br_1"})
	if len(signals) != 2 {
		t.Fatalf("got %d signals, want 2", len(signals))
	}
}

// The query a todo surface makes: "are any signals open against this todo?".
func TestHandleListSignalsFiltersByLinkedTodo(t *testing.T) {
	srv, st := testServerWithKanban(t, func(w http.ResponseWriter, r *http.Request) {
		// Two sessions, two different todos, so the filter has something to
		// exclude.
		todo := "todo-a"
		if r.URL.Path == "/api/entities/session/br_2/cards" {
			todo = "todo-b"
		}
		_, _ = w.Write([]byte(`[{"card_id":"` + todo + `","item":{"id":"` + todo + `"}}]`))
	})
	first := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)
	second := newSessionForSignals(t, st, "br_2", msg.SessionTypeAutonomous)
	srv.recordAskUserQuestionSignals("br_1", first, "hreq_1", json.RawMessage(askUserQuestionToolInput))
	srv.recordAskUserQuestionSignals("br_2", second, "hreq_2", json.RawMessage(askUserQuestionToolInput))
	srv.resolveSignalsForRequest("br_2", "hreq_2", permissionDecision{Behavior: "deny"})

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"/signals?linked_todo_id=todo-a", 2},
		{"/signals?linked_todo_id=todo-b", 2},
		{"/signals?linked_todo_id=todo-b&state=open", 0},
		{"/signals?linked_todo_id=todo-a&state=open", 2},
		{"/signals?linked_todo_id=todo-nobody", 0},
	} {
		t.Run(tc.query, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.query, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var signals []msg.Signal
			if err := json.Unmarshal(rec.Body.Bytes(), &signals); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if len(signals) != tc.want {
				t.Errorf("got %d signals, want %d", len(signals), tc.want)
			}
		})
	}
}

// An empty linked_todo_id is a caller that meant to name a todo and named
// none. Answering it with every signal in the store would badge one todo
// with another todo's question.
func TestHandleListSignalsRejectsAnEmptyLinkedTodo(t *testing.T) {
	srv, st := testServerWithKanban(t, cardsForSession("todo-42"))
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)
	srv.recordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

	for _, query := range []string{"/signals?linked_todo_id=", "/signals?linked_todo_id=%20"} {
		t.Run(query, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, query, nil))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}

	// Omitting the parameter entirely still means "don't narrow".
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/signals", nil))
	var signals []msg.Signal
	if err := json.Unmarshal(rec.Body.Bytes(), &signals); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(signals) != 2 {
		t.Errorf("got %d signals with no filter, want 2", len(signals))
	}
}
