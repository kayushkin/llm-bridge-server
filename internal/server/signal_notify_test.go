package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func raiseSignal(t *testing.T, srv *Server, bridgeID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(
		http.MethodPost, "/sessions/"+bridgeID+"/signals", strings.NewReader(body)))
	return rec
}

func decodeSignal(t *testing.T, rec *httptest.ResponseRecorder) msg.Signal {
	t.Helper()
	var got msg.Signal
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rec.Body.String())
	}
	return got
}

func TestRaiseNotificationStampsTheServerOwnedFields(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)

	rec := raiseSignal(t, srv, "br_1", `{"title":"The migration finished","body":"12 tables, no data loss.","severity":"warn"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	got := decodeSignal(t, rec)

	if got.Kind != msg.SignalKindNotification {
		t.Errorf("kind = %q, want notification", got.Kind)
	}
	if got.Source != msg.SignalSourceTool {
		t.Errorf("source = %q, want tool — this route is the structured producer, not the classifier", got.Source)
	}
	if got.State != msg.SignalStateOpen {
		t.Errorf("state = %q, want open", got.State)
	}
	if got.Severity != msg.SignalSeverityWarn {
		t.Errorf("severity = %q, want warn", got.Severity)
	}
	if got.SessionType != msg.SessionTypeInteractive {
		t.Errorf("session_type = %q, want interactive", got.SessionType)
	}
	if got.Title != "The migration finished" || got.Body != "12 tables, no data loss." {
		t.Errorf("title/body round-tripped wrong: %q / %q", got.Title, got.Body)
	}
	// No park behind a notification. An empty request_id is what lets the
	// resolve verb acknowledge it without tripping the parked-request guard.
	if got.RequestID != "" {
		t.Errorf("request_id = %q; a notification parks nothing", got.RequestID)
	}
	if got.ID == "" {
		t.Error("id is empty; the caller cannot resolve a row it cannot name")
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}
	if got.ResolvedAt != nil {
		t.Errorf("resolved_at = %v on a fresh open row", got.ResolvedAt)
	}

	// The response is the stored row, not an echo.
	stored, err := st.GetSignal(got.ID)
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if stored.Title != got.Title || stored.Source != got.Source || stored.Severity != got.Severity {
		t.Errorf("stored row %+v disagrees with the response %+v", stored, got)
	}
}

// Surface comes from the session, never from the caller: an autonomous
// worker's notification belongs on its kanban card whether or not the worker
// knows it has one.
func TestRaiseNotificationTakesSurfaceFromTheSession(t *testing.T) {
	cases := []struct {
		sessionType msg.SessionType
		want        msg.SignalSurface
	}{
		{msg.SessionTypeInteractive, msg.SignalSurfaceChat},
		{msg.SessionTypeHerald, msg.SignalSurfaceChat},
		{msg.SessionTypeSystem, msg.SignalSurfaceChat},
		{msg.SessionTypeAutonomous, msg.SignalSurfaceKanban},
	}
	for _, tc := range cases {
		t.Run(string(tc.sessionType), func(t *testing.T) {
			srv, st := testServer(t)
			newSessionForSignals(t, st, "br_1", tc.sessionType)

			// The caller tries to pick its own surface; the field is not
			// even in the request shape, so this must be ignored.
			rec := raiseSignal(t, srv, "br_1", `{"title":"heads up","surface":"chat","source":"derived","state":"answered"}`)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			got := decodeSignal(t, rec)
			if got.Surface != tc.want {
				t.Errorf("surface = %q, want %q", got.Surface, tc.want)
			}
			if got.Source != msg.SignalSourceTool {
				t.Errorf("source = %q; a caller must not be able to claim another producer", got.Source)
			}
			if got.State != msg.SignalStateOpen {
				t.Errorf("state = %q; a caller must not be able to raise a pre-resolved row", got.State)
			}
		})
	}
}

func TestRaiseNotificationDefaultsSeverityToInfo(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)

	rec := raiseSignal(t, srv, "br_1", `{"title":"deploy done"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := decodeSignal(t, rec); got.Severity != msg.SignalSeverityInfo {
		t.Errorf("severity = %q, want info", got.Severity)
	}
}

// A question raised here would be answerable by nothing: no request_id for the
// hook resolve, source=tool so answerDerivedQuestions skips it, and the
// signal-level resolve verb refuses to acknowledge a question. Refuse it at
// the door rather than mint a row with no way out.
func TestRaiseNotificationRefusesAQuestion(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)

	rec := raiseSignal(t, srv, "br_1", `{"kind":"question","title":"Ship it?"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "AskUserQuestion") {
		t.Errorf("the refusal must name the verb that DOES ask; got %q", rec.Body.String())
	}
	assertNoSignals(t, st, "br_1")
}

func TestRaiseNotificationRejectsBadEnums(t *testing.T) {
	cases := []struct{ name, body, wantIn string }{
		{"bad kind", `{"kind":"alert","title":"x"}`, "kind=alert"},
		{"bad severity", `{"title":"x","severity":"critical"}`, "severity=critical"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, st := testServer(t)
			newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)

			rec := raiseSignal(t, srv, "br_1", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantIn) {
				t.Errorf("error %q does not name the offending value %q", rec.Body.String(), tc.wantIn)
			}
			assertNoSignals(t, st, "br_1")
		})
	}
}

func TestRaiseNotificationRequiresATitle(t *testing.T) {
	for _, body := range []string{`{}`, `{"title":"   "}`, `{"body":"detail with no headline"}`} {
		srv, st := testServer(t)
		newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)

		rec := raiseSignal(t, srv, "br_1", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body = %s", body, rec.Code, rec.Body.String())
		}
		assertNoSignals(t, st, "br_1")
	}
}

// Over-long text is refused, not trimmed. A model reads the 400 and retries;
// a silent truncation puts half a message on the board and calls it delivered.
func TestRaiseNotificationBoundsTextAndRefusesRatherThanTruncates(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)

	longTitle, err := json.Marshal(map[string]string{"title": strings.Repeat("t", notifyMaxTitleRunes+1)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := raiseSignal(t, srv, "br_1", string(longTitle))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over-long title: status = %d, want 400", rec.Code)
	}
	assertNoSignals(t, st, "br_1")

	longBody, err := json.Marshal(map[string]string{"title": "ok", "body": strings.Repeat("b", notifyMaxBodyRunes+1)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec = raiseSignal(t, srv, "br_1", string(longBody))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over-long body: status = %d, want 400", rec.Code)
	}
	assertNoSignals(t, st, "br_1")

	// The bound counts characters, not bytes: a multi-byte title inside the
	// limit is accepted. Counting bytes would refuse a legitimate headline
	// at a third of the stated length.
	multibyte, err := json.Marshal(map[string]string{"title": strings.Repeat("é", notifyMaxTitleRunes)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if rec = raiseSignal(t, srv, "br_1", string(multibyte)); rec.Code != http.StatusCreated {
		t.Fatalf("multi-byte title at the limit: status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
}

func TestRaiseNotificationOnUnknownSessionIs404(t *testing.T) {
	srv, st := testServer(t)

	rec := raiseSignal(t, srv, "br_nope", `{"title":"heads up"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	assertNoSignals(t, st, "br_nope")
}

func TestRaiseNotificationRejectsUnparseableBody(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)

	rec := raiseSignal(t, srv, "br_1", `{"title":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertNoSignals(t, st, "br_1")
}

// The point of the route, end to end: a row raised by this producer is one the
// existing resolve verb can close. The producer would be useless if its rows
// tripped the parked-request guard, and it would be a trap if they could be
// acknowledged as questions.
func TestRaisedNotificationIsAcknowledgeable(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)

	rec := raiseSignal(t, srv, "br_1", `{"title":"schema changed under you","severity":"warn"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("raise: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	raised := decodeSignal(t, rec)

	ack := resolveSignal(t, srv, raised.ID, `{"state":"acknowledged"}`)
	if ack.Code != http.StatusOK {
		t.Fatalf("acknowledge: status = %d, body = %s", ack.Code, ack.Body.String())
	}
	stored, err := st.GetSignal(raised.ID)
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if stored.State != msg.SignalStateAcknowledged {
		t.Errorf("state = %q, want acknowledged", stored.State)
	}
}

// A tool notification must not switch the derived classifier off. It is open
// until somebody clicks Acknowledge, and on an unattended worker's kanban card
// that may be never — so counting it as an open ask would end classification
// for the rest of the session's life, and the genuine blocker raised three
// turns later would never be surfaced at all.
func TestOpenToolNotificationDoesNotSuppressTheClassifier(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)

	if rec := raiseSignal(t, srv, "br_1", `{"title":"halfway through the migration"}`); rec.Code != http.StatusCreated {
		t.Fatalf("raise: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if srv.hasOpenToolQuestion("br_1") {
		t.Fatal("an open tool NOTIFICATION counts as an open ask; it demands nothing and must not gate the classifier")
	}

	// The suppression it does exist for still works: a parked question.
	newSignalRow(t, st, &msg.Signal{
		SessionID: "br_1",
		Kind:      msg.SignalKindQuestion,
		Source:    msg.SignalSourceTool,
		RequestID: "hreq_1",
		Surface:   msg.SignalSurfaceKanban,
		State:     msg.SignalStateOpen,
	})
	if !srv.hasOpenToolQuestion("br_1") {
		t.Error("an open parked tool question must still suppress the classifier")
	}
}

// A turn-end supersedes open DERIVED rows. A tool notification is not derived,
// and must survive — the worker raised it on purpose and nobody has read it.
func TestTurnEndDoesNotSupersedeAToolNotification(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeAutonomous)

	rec := raiseSignal(t, srv, "br_1", `{"title":"the deploy is half-applied"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("raise: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	raised := decodeSignal(t, rec)

	srv.supersedeDerivedSignals("br_1")

	stored, err := st.GetSignal(raised.ID)
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if stored.State != msg.SignalStateOpen {
		t.Errorf("state = %q after a turn-end; a deliberate notification is not superseded by the next turn", stored.State)
	}
}

func assertNoSignals(t *testing.T, st *store.Store, bridgeID string) {
	t.Helper()
	signals, err := st.ListSignals(store.SignalFilter{SessionID: bridgeID})
	if err != nil {
		t.Fatalf("list signals: %v", err)
	}
	if len(signals) != 0 {
		t.Errorf("a refused call wrote %d row(s): %+v", len(signals), signals)
	}
}
