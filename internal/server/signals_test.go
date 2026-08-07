package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// askUserQuestionToolInput is the tool_input Claude Code sends for a
// two-question AskUserQuestion call.
const askUserQuestionToolInput = `{"questions":[
	{"question":"Ship it?","header":"Release","multiSelect":false,
	 "options":[{"label":"Yes","description":"deploy now"},{"label":"No"}]},
	{"question":"Which branch?","header":"Branch","multiSelect":false,
	 "options":[{"label":"main"},{"label":"release"}]}
]}`

func newSessionForSignals(t *testing.T, st *store.Store, bridgeID string, sessionType msg.SessionType) *store.Session {
	t.Helper()
	sess := &store.Session{
		SessionID: bridgeID,
		Harness:   "claude_code",
		State:     "idle",
		Type:      sessionType,
	}
	if err := st.CreateSession(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess
}

func TestSignalSurfaceForSession(t *testing.T) {
	cases := []struct {
		name string
		sess *store.Session
		want msg.SignalSurface
	}{
		{"interactive", &store.Session{Type: msg.SessionTypeInteractive}, msg.SignalSurfaceChat},
		// Herald is grouped with autonomous by isUnattendedSession but NOT
		// here: the relay reaches the user's chat, and no herald session has
		// a kanban card.
		{"herald", &store.Session{Type: msg.SessionTypeHerald}, msg.SignalSurfaceChat},
		{"system", &store.Session{Type: msg.SessionTypeSystem}, msg.SignalSurfaceChat},
		{"autonomous", &store.Session{Type: msg.SessionTypeAutonomous}, msg.SignalSurfaceKanban},
		{"unknown session", nil, msg.SignalSurfaceChat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := signalSurfaceForSession(tc.sess); got != tc.want {
				t.Errorf("surface = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRecordAskUserQuestionSignalsWritesOneRowPerQuestion(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)

	srv.recordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

	signals, err := st.ListSignalsByRequestID("br_1", "hreq_1")
	if err != nil {
		t.Fatalf("list signals: %v", err)
	}
	if len(signals) != 2 {
		t.Fatalf("got %d signals for a two-question ask, want 2", len(signals))
	}

	first := signals[0]
	if first.Title != "Ship it?" {
		t.Errorf("title = %q, want the question text", first.Title)
	}
	if first.Body != "Release" {
		t.Errorf("body = %q, want the question header", first.Body)
	}
	if first.Kind != msg.SignalKindQuestion || first.Source != msg.SignalSourceTool {
		t.Errorf("kind=%q source=%q, want question/tool", first.Kind, first.Source)
	}
	if first.Surface != msg.SignalSurfaceChat {
		t.Errorf("surface = %q, want chat for an interactive session", first.Surface)
	}
	if first.SessionType != msg.SessionTypeInteractive {
		t.Errorf("session_type = %q, want interactive", first.SessionType)
	}
	if first.State != msg.SignalStateOpen {
		t.Errorf("state = %q, want open", first.State)
	}
	if len(first.Options) != 2 {
		t.Fatalf("got %d options, want 2", len(first.Options))
	}
	if first.Options[0].Label != "Yes" || first.Options[0].Value != "Yes" {
		t.Errorf("option label/value = %q/%q, want Yes/Yes", first.Options[0].Label, first.Options[0].Value)
	}
	if first.Options[0].Description != "deploy now" {
		t.Errorf("option description dropped: %q", first.Options[0].Description)
	}
	if signals[1].Title != "Which branch?" {
		t.Errorf("second signal title = %q, want the second question", signals[1].Title)
	}
}

// A malformed or empty tool input must not mint a row and must not panic —
// recording is observational and never blocks the park.
func TestRecordAskUserQuestionSignalsIgnoresUnusableInput(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)

	srv.recordAskUserQuestionSignals("br_1", sess, "hreq_bad", json.RawMessage(`not json`))
	srv.recordAskUserQuestionSignals("br_1", sess, "hreq_empty", json.RawMessage(`{"questions":[]}`))

	signals, err := st.ListSignals(store.SignalFilter{SessionID: "br_1"})
	if err != nil {
		t.Fatalf("list signals: %v", err)
	}
	if len(signals) != 0 {
		t.Errorf("got %d signals from unusable input, want 0", len(signals))
	}
}

func TestResolveSignalsForRequestStampsAnswers(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)
	srv.recordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

	updatedInput := json.RawMessage(`{"questions":[],"answers":{"Ship it?":"Yes","Which branch?":"main"}}`)
	srv.resolveSignalsForRequest("br_1", "hreq_1", permissionDecision{
		Behavior:     "allow",
		UpdatedInput: updatedInput,
		ResolvedBy:   "user",
	})

	signals, err := st.ListSignalsByRequestID("br_1", "hreq_1")
	if err != nil {
		t.Fatalf("list signals: %v", err)
	}
	answers := map[string]string{}
	for _, sig := range signals {
		if sig.State != msg.SignalStateAnswered {
			t.Errorf("%s state = %q, want answered", sig.Title, sig.State)
		}
		if sig.ResolvedAt == nil {
			t.Errorf("%s has no resolved_at", sig.Title)
		}
		if sig.Answer == nil {
			t.Fatalf("%s carries no answer", sig.Title)
		}
		answers[sig.Title] = sig.Answer.Text
	}
	if answers["Ship it?"] != "Yes" || answers["Which branch?"] != "main" {
		t.Errorf("answers paired wrongly: %v", answers)
	}
}

func TestResolveSignalsForRequestDismissesOnDeny(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)
	srv.recordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

	srv.resolveSignalsForRequest("br_1", "hreq_1", permissionDecision{
		Behavior:   "deny",
		Message:    "user declined",
		ResolvedBy: "user",
	})

	signals, err := st.ListSignalsByRequestID("br_1", "hreq_1")
	if err != nil {
		t.Fatalf("list signals: %v", err)
	}
	for _, sig := range signals {
		if sig.State != msg.SignalStateDismissed {
			t.Errorf("%s state = %q, want dismissed", sig.Title, sig.State)
		}
		if sig.Answer != nil {
			t.Errorf("%s should carry no answer after a deny: %+v", sig.Title, sig.Answer)
		}
	}
}

// A permission-prompt park mints no signals, so resolving one must be a
// silent no-op rather than an error or a stray row.
func TestResolveSignalsForRequestWithNoSignalsIsNoOp(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)

	srv.resolveSignalsForRequest("br_1", "hreq_never_recorded", permissionDecision{Behavior: "allow"})
	srv.resolveSignalsForRequest("br_1", "", permissionDecision{Behavior: "allow"})

	signals, err := st.ListSignals(store.SignalFilter{SessionID: "br_1"})
	if err != nil {
		t.Fatalf("list signals: %v", err)
	}
	if len(signals) != 0 {
		t.Errorf("got %d signals, want 0", len(signals))
	}
}

func TestHandleListSessionSignals(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)
	newSessionForSignals(t, st, "br_2", msg.SessionTypeInteractive)
	srv.recordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/br_1/signals", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var signals []msg.Signal
	if err := json.Unmarshal(rec.Body.Bytes(), &signals); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rec.Body.String())
	}
	if len(signals) != 2 {
		t.Errorf("got %d signals, want 2", len(signals))
	}

	// A session with no signals returns an empty array, never null — the
	// frontend maps over this directly.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/br_2/signals", nil))
	if got := rec.Body.String(); got != "[]\n" && got != "[]" {
		t.Errorf("empty result body = %q, want an empty array", got)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/br_missing/signals", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown session status = %d, want 404", rec.Code)
	}
}

func TestHandleListSignalsAcrossSessions(t *testing.T) {
	srv, st := testServer(t)
	first := newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)
	second := newSessionForSignals(t, st, "br_2", msg.SessionTypeAutonomous)
	srv.recordAskUserQuestionSignals("br_1", first, "hreq_1", json.RawMessage(askUserQuestionToolInput))
	srv.recordAskUserQuestionSignals("br_2", second, "hreq_2", json.RawMessage(askUserQuestionToolInput))
	srv.resolveSignalsForRequest("br_2", "hreq_2", permissionDecision{Behavior: "deny"})

	cases := []struct {
		query string
		want  int
	}{
		{"/signals", 4},
		{"/signals?state=open", 2},
		{"/signals?state=dismissed", 2},
		{"/signals?surface=kanban", 2},
		{"/signals?kind=notification", 0},
		{"/signals?session_id=br_1", 2},
		{"/signals?state=open&limit=1", 1},
	}
	for _, tc := range cases {
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

// A typo in a filter must be a 400, not an empty list that reads as
// "there are no signals".
func TestHandleListSignalsRejectsUnknownFilterValues(t *testing.T) {
	srv, _ := testServer(t)

	for _, query := range []string{
		"/signals?state=resolved",
		"/signals?kind=alert",
		"/signals?surface=email",
		"/signals?limit=-1",
		"/signals?limit=soon",
	} {
		t.Run(query, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, query, nil))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// The full tool path, end to end: a real AskUserQuestion prehook parks, the
// signal row appears while the request is still parked, and resolving the
// hook through the public verb answers it. Unit tests on the two halves
// can both pass while the wiring between them is missing.
func TestAskUserQuestionPrehookRecordsAndResolvesSignal(t *testing.T) {
	// Parking broadcasts an awaiting_resolution event, and the broadcast
	// path pushes through log-store. Without a reachable one the broadcast
	// errors, the park is cancelled, and nothing is recorded — so stub it.
	logStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(logStore.Close)

	srv, st, _ := testServerWithInstanceAndLogStore(t, msg.HarnessClaudeCode, logStore.URL)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)

	prehookBody := `{"session_id":"cc-1","hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_input":` + askUserQuestionToolInput + `}`
	parked := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(
			http.MethodPost, "/permission/cc-prehook/br_1", strings.NewReader(prehookBody)))
		parked <- rec
	}()

	// Wait for the park to land its signal rows rather than sleeping a fixed
	// interval — a sleep either flakes or wastes the whole duration.
	var signals []store.Signal
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		signals, err = st.ListSignals(store.SignalFilter{SessionID: "br_1"})
		if err != nil {
			t.Fatalf("list signals: %v", err)
		}
		if len(signals) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(signals) != 2 {
		t.Fatalf("the parked AskUserQuestion recorded %d signals, want 2", len(signals))
	}
	requestID := signals[0].RequestID
	if requestID == "" {
		t.Fatal("signal carries no request_id, so no resolve can ever find it")
	}
	for _, sig := range signals {
		if sig.State != msg.SignalStateOpen {
			t.Errorf("%s is %q while still parked, want open", sig.Title, sig.State)
		}
	}

	resolveBody := `{"behavior":"allow","resolved_by":"user","updated_input":{"answers":{"Ship it?":"Yes","Which branch?":"main"}}}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(
		http.MethodPost, "/sessions/br_1/hooks/"+requestID+"/resolve", strings.NewReader(resolveBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve status = %d, body = %s", rec.Code, rec.Body.String())
	}

	select {
	case prehookResponse := <-parked:
		if prehookResponse.Code != http.StatusOK {
			t.Errorf("prehook status = %d, body = %s", prehookResponse.Code, prehookResponse.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked prehook never returned after its resolve")
	}

	resolved, err := st.ListSignalsByRequestID("br_1", requestID)
	if err != nil {
		t.Fatalf("list signals: %v", err)
	}
	for _, sig := range resolved {
		if sig.State != msg.SignalStateAnswered {
			t.Errorf("%s state = %q after resolve, want answered", sig.Title, sig.State)
		}
		if sig.Answer == nil {
			t.Errorf("%s carries no answer after resolve", sig.Title)
		}
	}
}
