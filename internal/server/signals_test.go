package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/ids"
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

// --- POST /signals/{id}/resolve (P4) ---

// newSignalRow writes one signal straight to the store. Going through the
// store rather than a producer keeps each resolve case to the two fields it
// is actually about — kind and source — instead of staging a park or a
// classifier verdict to reach them.
func newSignalRow(t *testing.T, st *store.Store, sig *msg.Signal) *msg.Signal {
	t.Helper()
	if sig.ID == "" {
		sig.ID = ids.NewSignalID()
	}
	if sig.Title == "" {
		sig.Title = "a signal"
	}
	if err := st.CreateSignal(sig); err != nil {
		t.Fatalf("create signal: %v", err)
	}
	return sig
}

func resolveSignal(t *testing.T, srv *Server, signalID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(
		http.MethodPost, "/signals/"+signalID+"/resolve", strings.NewReader(body)))
	return rec
}

func TestResolveSignalAcknowledgesANotification(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)
	sig := newSignalRow(t, st, &msg.Signal{
		SessionID: "br_1",
		Kind:      msg.SignalKindNotification,
		Source:    msg.SignalSourceDerived,
		Surface:   msg.SignalSurfaceChat,
		Title:     "The migration finished",
		State:     msg.SignalStateOpen,
	})

	rec := resolveSignal(t, srv, sig.ID, `{"state":"acknowledged"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got msg.Signal
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rec.Body.String())
	}
	if got.State != msg.SignalStateAcknowledged {
		t.Errorf("state = %q, want acknowledged", got.State)
	}
	// The response is the row, not an echo of the request: a surface that
	// renders what it gets back must not show a resolved_at that is missing.
	if got.ResolvedAt == nil {
		t.Error("resolved_at is nil on an acknowledged signal")
	}
	if got.Answer != nil {
		t.Errorf("an acknowledgement carries an answer %+v; it answers nothing", got.Answer)
	}

	stored, err := st.GetSignal(sig.ID)
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if stored.State != msg.SignalStateAcknowledged {
		t.Errorf("stored state = %q, want acknowledged", stored.State)
	}
}

// A duplicate click is ordinary — the same row renders in the chat, the
// inbox, the RefChip panel and (P4) the kanban drawer — so a second resolve
// reports what happened rather than overwriting it or erroring.
func TestResolveSignalIsIdempotent(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)
	sig := newSignalRow(t, st, &msg.Signal{
		SessionID: "br_1",
		Kind:      msg.SignalKindNotification,
		Source:    msg.SignalSourceDerived,
		Surface:   msg.SignalSurfaceChat,
		State:     msg.SignalStateOpen,
	})

	if rec := resolveSignal(t, srv, sig.ID, `{"state":"acknowledged"}`); rec.Code != http.StatusOK {
		t.Fatalf("first resolve status = %d, body = %s", rec.Code, rec.Body.String())
	}
	first, err := st.GetSignal(sig.ID)
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}

	// Dismiss the same row: a different state, so an overwrite would be
	// visible rather than hidden behind an identical value.
	rec := resolveSignal(t, srv, sig.ID, `{"state":"dismissed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second resolve status = %d, body = %s", rec.Code, rec.Body.String())
	}
	second, err := st.GetSignal(sig.ID)
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if second.State != msg.SignalStateAcknowledged {
		t.Errorf("state = %q after a second resolve, want the first one (acknowledged)", second.State)
	}
	if !second.ResolvedAt.Equal(*first.ResolvedAt) {
		t.Errorf("resolved_at moved from %v to %v; the first resolution is the one that happened",
			first.ResolvedAt, second.ResolvedAt)
	}
}

// Acknowledging is the notification verb. A question nobody answered has not
// been handled, and grading it the same as a read notification is the enum
// collapse feedback_status_enum_granularity warns about.
func TestResolveSignalRefusesToAcknowledgeAQuestion(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)
	sig := newSignalRow(t, st, &msg.Signal{
		SessionID: "br_1",
		Kind:      msg.SignalKindQuestion,
		Source:    msg.SignalSourceDerived,
		Surface:   msg.SignalSurfaceChat,
		State:     msg.SignalStateOpen,
	})

	rec := resolveSignal(t, srv, sig.ID, `{"state":"acknowledged"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	stored, err := st.GetSignal(sig.ID)
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if stored.State != msg.SignalStateOpen {
		t.Errorf("state = %q after a refused resolve, want it left open", stored.State)
	}

	// The same question dismisses fine — refusing the ack must not make the
	// row unclosable.
	if rec := resolveSignal(t, srv, sig.ID, `{"state":"dismissed"}`); rec.Code != http.StatusOK {
		t.Errorf("dismiss status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// An answer has to reach the session. The two paths that carry one close
// their own rows; recording `answered` here would claim an answer the
// session never received.
func TestResolveSignalRefusesAnswered(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)
	sig := newSignalRow(t, st, &msg.Signal{
		SessionID: "br_1",
		Kind:      msg.SignalKindQuestion,
		Source:    msg.SignalSourceDerived,
		Surface:   msg.SignalSurfaceChat,
		State:     msg.SignalStateOpen,
	})

	rec := resolveSignal(t, srv, sig.ID, `{"state":"answered"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	// The refusal has to say where an answer does go, or the caller has no
	// next move.
	if body := rec.Body.String(); !strings.Contains(body, "/send") {
		t.Errorf("refusal does not name the path that answers a derived question: %s", body)
	}
	stored, err := st.GetSignal(sig.ID)
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if stored.State != msg.SignalStateOpen {
		t.Errorf("state = %q after a refused resolve, want it left open", stored.State)
	}
}

func TestResolveSignalRejectsUnusableState(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)
	sig := newSignalRow(t, st, &msg.Signal{
		SessionID: "br_1",
		Kind:      msg.SignalKindNotification,
		Source:    msg.SignalSourceDerived,
		Surface:   msg.SignalSurfaceChat,
		State:     msg.SignalStateOpen,
	})

	for _, body := range []string{
		`{"state":"open"}`,     // not a resolution at all
		`{"state":"resolved"}`, // not in the enum
		`{"state":""}`,         // omitted
		`{}`,                   // omitted, harder
		`not json`,             // unparseable
	} {
		rec := resolveSignal(t, srv, sig.ID, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s → status %d, want 400; body = %s", body, rec.Code, rec.Body.String())
		}
	}
	stored, err := st.GetSignal(sig.ID)
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if stored.State != msg.SignalStateOpen {
		t.Errorf("state = %q, want it left open by every rejected request", stored.State)
	}
}

func TestResolveSignalUnknownSignalIs404(t *testing.T) {
	srv, _ := testServer(t)
	rec := resolveSignal(t, srv, "sig_missing", `{"state":"dismissed"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

// A tool signal is a surface on a parked hook. Closing it while the harness
// still sits on the channel hides the ask and leaves the session blocked
// with nothing on screen to unblock it.
func TestResolveSignalRefusesWhileTheRequestIsStillParked(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)
	sig := newSignalRow(t, st, &msg.Signal{
		SessionID: "br_1",
		Kind:      msg.SignalKindQuestion,
		Source:    msg.SignalSourceTool,
		RequestID: "hreq_1",
		Surface:   msg.SignalSurfaceChat,
		State:     msg.SignalStateOpen,
	})
	srv.parkedAsks.park("br_1", "hreq_1")

	rec := resolveSignal(t, srv, sig.ID, `{"state":"dismissed"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "hreq_1") {
		t.Errorf("refusal does not name the parked request to resolve instead: %s", body)
	}
	stored, err := st.GetSignal(sig.ID)
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if stored.State != msg.SignalStateOpen {
		t.Errorf("state = %q while its request is parked, want open", stored.State)
	}

	// Once the park is gone — harness restart, cancelled request — the row is
	// a leftover with nothing behind it, and dismissing it is the only way to
	// clear it.
	srv.parkedAsks.cancel("br_1", "hreq_1")
	if rec := resolveSignal(t, srv, sig.ID, `{"state":"dismissed"}`); rec.Code != http.StatusOK {
		t.Fatalf("status after the park went away = %d, body = %s", rec.Code, rec.Body.String())
	}
	stored, err = st.GetSignal(sig.ID)
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if stored.State != msg.SignalStateDismissed {
		t.Errorf("state = %q, want dismissed", stored.State)
	}
}

func TestResolutionUnparksSession(t *testing.T) {
	cases := []struct {
		name   string
		source msg.SignalSource
		kind   msg.SignalKind
		state  msg.SignalState
		want   bool
	}{
		// The one row type that parks its own session.
		{"dismissed derived question", msg.SignalSourceDerived, msg.SignalKindQuestion, msg.SignalStateDismissed, true},
		// A derived notification drove the session to idle at mint time.
		{"dismissed derived notification", msg.SignalSourceDerived, msg.SignalKindNotification, msg.SignalStateDismissed, false},
		{"acknowledged derived notification", msg.SignalSourceDerived, msg.SignalKindNotification, msg.SignalStateAcknowledged, false},
		// A tool signal's session state belongs to its parked hook.
		{"dismissed tool question", msg.SignalSourceTool, msg.SignalKindQuestion, msg.SignalStateDismissed, false},
		{"dismissed tool notification", msg.SignalSourceTool, msg.SignalKindNotification, msg.SignalStateDismissed, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig := &store.Signal{Source: tc.source, Kind: tc.kind, State: msg.SignalStateOpen}
			if got := resolutionUnparksSession(sig, tc.state); got != tc.want {
				t.Errorf("resolutionUnparksSession = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsParkedDoesNotConsumeTheEntry(t *testing.T) {
	p := newParkedAsks()
	if p.isParked("br_1", "hreq_1") {
		t.Error("isParked is true for a request that was never parked")
	}
	ch := p.park("br_1", "hreq_1")
	if !p.isParked("br_1", "hreq_1") {
		t.Fatal("isParked is false for a live park")
	}
	// The read must leave the park deliverable — an isParked that removed
	// the entry would turn every 409 check into a wedged session.
	if !p.isParked("br_1", "hreq_1") {
		t.Fatal("isParked went false on a second read; it consumed the entry")
	}
	if !p.deliver("br_1", "hreq_1", permissionDecision{Behavior: "deny"}) {
		t.Fatal("deliver found no parked entry after isParked read it")
	}
	if p.isParked("br_1", "hreq_1") {
		t.Error("isParked is still true after the decision was delivered")
	}
	<-ch
}

// A duplicate click must not write session state a second time. By the time
// it lands the session may be back at awaiting_user on a NEW question, and
// the allowedFrom bound cannot tell that apart from the one being dismissed.
func TestAResolvedSignalUnparksNothing(t *testing.T) {
	for _, state := range []msg.SignalState{
		msg.SignalStateDismissed, msg.SignalStateAnswered, msg.SignalStateAcknowledged,
	} {
		sig := &store.Signal{
			Source: msg.SignalSourceDerived,
			Kind:   msg.SignalKindQuestion,
			State:  state,
		}
		if resolutionUnparksSession(sig, msg.SignalStateDismissed) {
			t.Errorf("an already-%s signal still unparks its session", state)
		}
	}
}
