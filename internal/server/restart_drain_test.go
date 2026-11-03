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

// drainTestServer is a server with a mock harness and a log-store that takes
// every event, since /send and every broadcast write to log-store.
func drainTestServer(t *testing.T) (*Server, *store.Store, *store.Session) {
	t.Helper()
	buildMockHarnessOnPath(t)
	logStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(logStore.Close)
	srv, st, instID := testServerWithInstanceAndLogStore(t, msg.HarnessMock, logStore.URL)
	session := postCreateSession(t, srv, autoStartRequest(instID))
	t.Cleanup(func() { srv.harness.Kill(session.SessionID) })
	return srv, st, session
}

func sendThroughRoute(t *testing.T, srv *Server, sessionID, message string) (int, msg.SendMessageResponse) {
	t.Helper()
	status, body := srv.serveInternally(http.MethodPost, "/sessions/"+sessionID+"/send", msg.SendMessageRequest{Message: message})
	var answer msg.SendMessageResponse
	if status < 300 {
		if err := json.Unmarshal(body, &answer); err != nil {
			t.Fatalf("decode send answer %q: %v", body, err)
		}
	}
	return status, answer
}

// systemSubtypesAndUserMessages lists, in order, the user messages and the
// held-message system events stored for a session.
func systemSubtypesAndUserMessages(t *testing.T, st *store.Store, sessionID string) []string {
	t.Helper()
	events, err := st.ListEventsSinceID(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, row := range events {
		var event msg.Event
		if err := json.Unmarshal(row.Data, &event); err != nil {
			t.Fatal(err)
		}
		switch {
		case event.Type == msg.EventUserMessage && event.HarnessSessionID == "" && event.Result != nil:
			seen = append(seen, "user_message:"+event.Result.Text)
		case event.Type == msg.EventSystem && event.System != nil && strings.Contains(event.System.Subtype, "held"):
			seen = append(seen, event.System.Subtype+":"+event.System.HeldMessageID)
		}
	}
	return seen
}

func waitForHeldMessagesToBeSent(t *testing.T, st *store.Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		held, err := st.UnclaimedHeldMessages()
		if err != nil {
			t.Fatal(err)
		}
		if len(held) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the held messages were never sent")
}

func indexOf(entries []string, want string) int {
	for index, entry := range entries {
		if entry == want {
			return index
		}
	}
	return -1
}

func waitForEvent(t *testing.T, st *store.Store, sessionID, want string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		seen := systemSubtypesAndUserMessages(t, st, sessionID)
		for _, entry := range seen {
			if strings.HasPrefix(entry, want) {
				return seen
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q event; saw %v", want, seen)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// While the server drains, a send is held: it answers 202 with the held id,
// starts no turn, leaves the session's state alone, and shows the message as
// held. When the drain ends, the message is sent as an ordinary user message
// and marked delivered.
func TestASendWhileDrainingIsHeldAndSentWhenTheDrainEnds(t *testing.T) {
	srv, st, session := drainTestServer(t)
	stateBefore := func() string { row, _ := st.GetSession(session.SessionID); return row.State }()

	srv.startRestartDrain(time.Hour, "test deploy")
	status, answer := sendThroughRoute(t, srv, session.SessionID, "please continue")
	if status != http.StatusAccepted || answer.Status != msg.SendStatusHeld || !strings.HasPrefix(answer.HeldMessageID, "held_") {
		t.Fatalf("send while draining = %d %+v; want 202 held with a held_ id", status, answer)
	}
	if row, _ := st.GetSession(session.SessionID); row.State != stateBefore {
		t.Fatalf("holding the message moved the session from %s to %s", stateBefore, row.State)
	}
	seen := systemSubtypesAndUserMessages(t, st, session.SessionID)
	if len(seen) != 1 || seen[0] != msg.SystemSubtypeMessageHeldForRestart+":"+answer.HeldMessageID {
		t.Fatalf("while held the conversation shows %v; want only the held event", seen)
	}
	if readiness := readRestartReadiness(t, srv); !readiness.Drain.Draining || readiness.HeldMessages != 1 {
		t.Fatalf("readiness while draining = %+v; want draining with one held message", readiness)
	}

	srv.endRestartDrain("test")
	waitForHeldMessagesToBeSent(t, st)
	seen = waitForEvent(t, st, session.SessionID, msg.SystemSubtypeHeldMessageDelivered)
	// The mock harness echoes each message it gets as a second user_message,
	// so read the order, not the count: held, then the message, then
	// delivered.
	held, sentAt, delivered := indexOf(seen, msg.SystemSubtypeMessageHeldForRestart+":"+answer.HeldMessageID),
		indexOf(seen, "user_message:please continue"), indexOf(seen, msg.SystemSubtypeHeldMessageDelivered+":"+answer.HeldMessageID)
	if !(held == 0 && sentAt > held && delivered > sentAt) {
		t.Fatalf("after the drain the conversation shows %v; want held, then the message, then delivered", seen)
	}
	if status, answer := sendThroughRoute(t, srv, session.SessionID, "and again"); status != http.StatusOK || answer.Status != msg.SendStatusSent {
		t.Fatalf("send after the drain = %d %+v; want 200 sent", status, answer)
	}
}

// A message the previous process held is sent by the restarted server at
// start, and only once even when two deliveries race for it.
func TestMessagesHeldByThePreviousProcessAreSentOnceAtStart(t *testing.T) {
	srv, st, session := drainTestServer(t)
	if err := st.HoldMessage(store.HeldMessage{HeldMessageID: "held_01TESTHELDBEFORERESTART0000", SessionID: session.SessionID,
		Request: msg.SendMessageRequest{Message: "held before the restart"}, Source: "send", HeldAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	srv.DeliverMessagesHeldBeforeRestart()
	srv.DeliverMessagesHeldBeforeRestart()
	waitForHeldMessagesToBeSent(t, st)
	waitForEvent(t, st, session.SessionID, msg.SystemSubtypeHeldMessageDelivered)
	time.Sleep(300 * time.Millisecond) // give a second, wrong delivery time to show up
	delivered := 0
	for _, entry := range systemSubtypesAndUserMessages(t, st, session.SessionID) {
		if entry == msg.SystemSubtypeHeldMessageDelivered+":held_01TESTHELDBEFORERESTART0000" {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("the held message was delivered %d times; want once", delivered)
	}
}

// A drain the deploy never calls off ends by itself, and sends what it held.
func TestADrainEndsByItselfAndSendsWhatItHeld(t *testing.T) {
	srv, st, session := drainTestServer(t)
	srv.startRestartDrain(200*time.Millisecond, "test deploy that died")
	if status, _ := sendThroughRoute(t, srv, session.SessionID, "sent after the drain ran out"); status != http.StatusAccepted {
		t.Fatalf("send while draining = %d; want 202", status)
	}
	waitForHeldMessagesToBeSent(t, st)
	waitForEvent(t, st, session.SessionID, "user_message:sent after the drain ran out")
	if srv.restartDrainView().Draining {
		t.Fatal("the drain is still on after its time ran out")
	}
}

func TestStartingADrainNeedsALimitAndAReason(t *testing.T) {
	srv, _, _ := drainTestServer(t)
	for body, wantCode := range map[string]string{
		`{"reason":"deploy"}`:                              "invalid_maximum_seconds",
		`{"maximum_seconds":999999,"reason":"deploy"}`:     "invalid_maximum_seconds",
		`{"maximum_seconds":60}`:                           "reason_required",
		`{"maximum_seconds":60,"reason":"x","extra":true}`: "invalid_body",
	} {
		recorder := httptest.NewRecorder()
		srv.handleStartRestartDrain(recorder, httptest.NewRequest(http.MethodPost, "/restart-drain", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), wantCode) {
			t.Errorf("POST /restart-drain %s = %d %s; want 400 %s", body, recorder.Code, recorder.Body.String(), wantCode)
		}
	}
	if srv.restartDrainView().Draining {
		t.Fatal("a refused request started a drain")
	}
}
