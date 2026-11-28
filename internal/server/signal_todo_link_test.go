package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/sessions/signals"

	"github.com/kayushkin/llm-bridge-server/internal/sessions/parkedasks"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issuereads/issuereadstest"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issuereads"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	issuesapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/api"
	issuesmodel "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/model"
	"github.com/kayushkin/llm-bridge/msg"
)

// testServerWithKanban is testServer wired to a stub kanban-store. kanban
// answers the session-cards reverse lookup in this process; pass nil for a
// store that refuses every read 502, as it does when noteboard is down.
func testServerWithKanban(t *testing.T, kanban *issuereadstest.Store) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st := newTestBridgeStore(t)

	if kanban == nil {
		kanban = &issuereadstest.Store{EntityCardsAnswer: func(string, string) ([]issuesmodel.EntityCardView, error) {
			return nil, &issuesapi.Refusal{Status: http.StatusBadGateway, Message: "noteboard did not answer"}
		}}
	}

	cfg := &config.Config{
		ImagesDir:       filepath.Join(dir, "images"),
		BridgePrefsPath: filepath.Join(dir, "prefs.json"),
	}
	testAuthorizationConfig(cfg)
	srv := New(st, nil, nil, nil, nil, nil, nil, cfg)
	enableTestSessionEvents(t, srv, st)
	srv.issueReads = issuereads.New(kanban)
	return srv, st
}

// newSessionCardsStub stands in for kanban-store's entity reverse lookup as
// kanban-store answers it: a session ref with links is its cards, and an
// entity with no links is an EMPTY list — not a refusal. That last rule is
// why the stub has to tell refs apart rather than answer every lookup with
// the same card. An unknown ref is indistinguishable from a known one with no
// links, so a lookup aimed at the wrong session id fails *silently*:
// LinkedTodoForSession reads the empty list as "this session has no todo",
// and the signal is minted unlinked with nothing logged. A stub that serves
// the canned card for every ref cannot tell a right id from a wrong one, so
// it certifies whichever id the caller happens to pass.
func newSessionCardsStub(todoByRef map[string]string) *issuereadstest.Store {
	return &issuereadstest.Store{EntityCardsAnswer: func(entityType, entityRef string) ([]issuesmodel.EntityCardView, error) {
		todo, linked := todoByRef[entityRef]
		if entityType != "session" || !linked {
			return []issuesmodel.EntityCardView{}, nil
		}
		return []issuesmodel.EntityCardView{{CardID: todo, Item: map[string]any{"id": todo}}}, nil
	}}
}

// cardsForSession links one bridge session id to one todo.
func cardsForSession(bridgeID, todoID string) *issuereadstest.Store {
	return newSessionCardsStub(map[string]string{bridgeID: todoID})
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

	srv.signals.RecordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

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
	srv, st := testServerWithKanban(t, newSessionCardsStub(nil))
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)

	srv.signals.RecordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

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

	srv.signals.RecordDerivedQuestions(sess, []signals.ClassifiedQuestion{{Question: "Which branch should I ship from?"}}, nil, signals.CardContext{})

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
	cases := map[string]*issuereadstest.Store{
		"store errors": {EntityCardsAnswer: func(string, string) ([]issuesmodel.EntityCardView, error) {
			return nil, &issuesapi.Refusal{Status: http.StatusInternalServerError, Message: "boom"}
		}},
		"store gives an unreadable card": {EntityCardsAnswer: func(string, string) ([]issuesmodel.EntityCardView, error) {
			return []issuesmodel.EntityCardView{{CardID: "todo-42", Item: map[string]any{"id": "todo-42", "title": 7}}}, nil
		}},
		"store unreachable": nil,
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			srv, st := testServerWithKanban(t, handler)
			sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)

			srv.signals.RecordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

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

// With no kanban-store enabled the lookup is off, not broken: no request
// is attempted and every signal is minted unlinked.
func TestNoKanbanStoreConfiguredMeansNoLookup(t *testing.T) {
	srv, st := testServer(t)
	if srv.issueReads != nil {
		t.Fatal("expected no kanban client on a server that never enabled kanban-store")
	}
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)
	srv.signals.RecordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

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
// This pins the ref asked about, not just that a todo came back, because a
// wrong id still produces a well-formed lookup, and only the ref is wrong.
func TestTheLinkLookupAsksAboutTheBridgeSessionID(t *testing.T) {
	stub := newSessionCardsStub(map[string]string{"br_1": "todo-42"})
	srv, st := testServerWithKanban(t, stub)
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)
	withHarnessSessionID(t, st, sess)

	srv.signals.RecordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

	asked := stub.Calls()
	if len(asked) != 1 {
		t.Fatalf("kanban-store asked %d times, want exactly 1 for one request: %v", len(asked), asked)
	}
	if asked[0].Method != "EntityCards" || asked[0].About != "session/br_1" {
		t.Errorf("asked = %+v, want the cards of session br_1", asked[0])
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
	srv, st := testServerWithKanban(t, stub)
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)

	srv.signals.RecordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

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
	}))
	first := newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)
	second := newSessionForSignals(t, st, "br_2", msg.SessionTypeAutonomous)
	srv.signals.RecordAskUserQuestionSignals("br_1", first, "hreq_1", json.RawMessage(askUserQuestionToolInput))
	srv.signals.RecordAskUserQuestionSignals("br_2", second, "hreq_2", json.RawMessage(askUserQuestionToolInput))
	srv.signals.ResolveSignalsForRequest("br_2", "hreq_2", parkedasks.Decision{Behavior: "deny"})

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
			asInternalService(srv).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.query, nil))
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
	srv.signals.RecordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

	for _, query := range []string{"/signals?linked_todo_id=", "/signals?linked_todo_id=%20"} {
		t.Run(query, func(t *testing.T) {
			rec := httptest.NewRecorder()
			asInternalService(srv).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, query, nil))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}

	// Omitting the parameter entirely still means "don't narrow".
	rec := httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/signals", nil))
	var signals []msg.Signal
	if err := json.Unmarshal(rec.Body.Bytes(), &signals); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(signals) != 2 {
		t.Errorf("got %d signals with no filter, want 2", len(signals))
	}
}
