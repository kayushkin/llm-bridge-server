package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

// sessionCardsStub stands in for kanban-store's entity reverse lookup and
// ROUTES the way kanban-store actually routes, instead of answering every
// request with the same body.
//
// It mirrors kanban-store's entityScoped (internal/api/api.go), route for
// route:
//
//   - fewer than three segments after /api/entities/ is 400
//   - a sub-resource other than "cards" is 404
//   - "cards" under any method but GET is 405
//   - an entity ref with no links is 200 and an EMPTY array — not 404
//
// That last rule is why the routing has to be here rather than in a comment.
// An unknown ref is indistinguishable from a known one at the status line, so
// a lookup aimed at the wrong session id fails *silently*: kanban-store
// answers 200 `[]`, LinkedTodoForSession reads that as "this session has no
// todo", and the signal is minted unlinked with nothing logged. A stub that
// serves the canned card for every path cannot tell a right id from a wrong
// one, so it certifies whichever id the caller happens to pass.
//
// It splits r.URL.Path — the DECODED path — because that is what kanban-store
// splits. Assertions about what the client sent read RequestURI instead, since
// Go's server decodes %2F back to a slash in URL.Path and a path assertion
// against it would pass either way.
type sessionCardsStub struct {
	todoByRef map[string]string

	mu    sync.Mutex
	asked []string
}

func newSessionCardsStub(todoByRef map[string]string) *sessionCardsStub {
	return &sessionCardsStub{todoByRef: todoByRef}
}

func (s *sessionCardsStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.asked = append(s.asked, r.Method+" "+r.RequestURI)
	s.mu.Unlock()

	rest := strings.TrimPrefix(r.URL.Path, "/api/entities/")
	parts := strings.Split(rest, "/")
	if len(parts) < 3 {
		http.Error(w, "expected /api/entities/:type/:ref/...", http.StatusBadRequest)
		return
	}
	etype, eref, sub := parts[0], parts[1], parts[2]
	if sub != "cards" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	todo, linked := s.todoByRef[eref]
	if etype != "session" || !linked {
		// The silent case: a ref kanban-store knows nothing about is an empty
		// list under a 200, exactly like a session that genuinely has no todo.
		_, _ = w.Write([]byte(`[]`))
		return
	}
	_, _ = w.Write([]byte(`[{"card_id":"` + todo + `","item":{"id":"` + todo + `"}}]`))
}

// requestLines returns the raw request lines the client produced.
func (s *sessionCardsStub) requestLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// cardsForSession links one bridge session id to one todo, routing.
func cardsForSession(bridgeID, todoID string) http.HandlerFunc {
	return newSessionCardsStub(map[string]string{bridgeID: todoID}).ServeHTTP
}

// withHarnessSessionID stamps a harness-native session id on a session, so a
// lookup that reaches for the wrong one of the two ids a session carries has a
// non-empty wrong answer to reach for. Without it, passing the harness id
// resolves to "" and linkedTodoForSession short-circuits before any request —
// a mistake that would be caught for the wrong reason.
//
// It sets the field on the in-memory session as well as the row. Store.
// SetHarnessSessionID writes only the row, and the signal producers read the
// struct they were handed, so stamping the row alone leaves the struct's id
// empty and quietly restores the short-circuit this exists to remove.
func withHarnessSessionID(t *testing.T, st *store.Store, sess *store.Session) {
	t.Helper()
	harnessID := "cc-" + sess.SessionID
	if err := st.SetHarnessSessionID(sess.SessionID, harnessID); err != nil {
		t.Fatalf("set harness session id: %v", err)
	}
	sess.HarnessSessionID = harnessID
}

func TestToolSignalsCarryTheLinkedTodo(t *testing.T) {
	srv, st := testServerWithKanban(t, cardsForSession("br_1", "todo-42"))
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)
	withHarnessSessionID(t, st, sess)

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
	srv, st := testServerWithKanban(t, cardsForSession("br_1", "todo-42"))
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)
	withHarnessSessionID(t, st, sess)

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

// A session carries two ids — the bridge id (br_1) and the harness-native id
// (cc-br_1) — and kanban-store links cards against the bridge one. Which of
// the two the server layer hands to the client is not a detail: reaching for
// the harness id returns 200 `[]` from a real kanban-store, so every signal
// would be minted unlinked and nothing anywhere would say why.
//
// This pins the whole request line, not just that a todo came back, because
// the two failures are different sizes: a wrong id still produces a
// well-formed request, and only the ref inside it is wrong.
func TestTheLinkLookupAsksAboutTheBridgeSessionID(t *testing.T) {
	stub := newSessionCardsStub(map[string]string{"br_1": "todo-42"})
	srv, st := testServerWithKanban(t, stub.ServeHTTP)
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)
	withHarnessSessionID(t, st, sess)

	srv.recordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

	asked := stub.requestLines()
	if len(asked) != 1 {
		t.Fatalf("kanban-store asked %d times, want exactly 1 for one request: %v", len(asked), asked)
	}
	if want := "GET /api/entities/session/br_1/cards"; asked[0] != want {
		t.Errorf("request line = %q, want %q", asked[0], want)
	}

	signals, _ := st.ListSignals(store.SignalFilter{SessionID: "br_1"})
	if len(signals) == 0 {
		t.Fatal("no signals recorded")
	}
	for _, sig := range signals {
		if sig.LinkedTodoID != "todo-42" {
			t.Errorf("linked_todo_id = %q, want todo-42", sig.LinkedTodoID)
		}
	}
}

// An unknown ref is 200 and an empty array, not an error — so this is the
// shape a lookup aimed at the wrong id takes, and the reason a path-blind stub
// cannot see one. The signal must still be recorded, and recorded unlinked.
func TestAnUnknownSessionRefIsAnEmptyListNotAFailure(t *testing.T) {
	stub := newSessionCardsStub(map[string]string{"br_other": "todo-42"})
	srv, st := testServerWithKanban(t, stub.ServeHTTP)
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)

	srv.recordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

	signals, _ := st.ListSignals(store.SignalFilter{SessionID: "br_1"})
	if len(signals) != 2 {
		t.Fatalf("got %d signals, want 2 — an unlinked session must still raise its signals", len(signals))
	}
	for _, sig := range signals {
		if sig.LinkedTodoID != "" {
			t.Errorf("linked_todo_id = %q, want empty for a ref kanban-store does not know", sig.LinkedTodoID)
		}
	}
}

// The query a todo surface makes: "are any signals open against this todo?".
func TestHandleListSignalsFiltersByLinkedTodo(t *testing.T) {
	// Two sessions, two different todos, so the filter has something to
	// exclude.
	srv, st := testServerWithKanban(t, newSessionCardsStub(map[string]string{
		"br_1": "todo-a",
		"br_2": "todo-b",
	}).ServeHTTP)
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
	srv, st := testServerWithKanban(t, cardsForSession("br_1", "todo-42"))
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
