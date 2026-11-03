package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func writeStateForRestartReadinessTest(t *testing.T, st *store.Store, sessionID string, state msg.SessionState) {
	t.Helper()
	if _, err := st.WriteSessionStatus(sessionID, &msg.Event{Type: msg.EventSessionStatus, Timestamp: time.Now(),
		Status: &msg.SessionStatus{State: state}}); err != nil {
		t.Fatalf("write %s to %s: %v", state, sessionID, err)
	}
}

func readRestartReadiness(t *testing.T, srv *Server) restartReadiness {
	t.Helper()
	recorder := httptest.NewRecorder()
	srv.handleRestartReadiness(recorder, httptest.NewRequest("GET", "/restart-readiness", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /restart-readiness = %d: %s", recorder.Code, recorder.Body.String())
	}
	var answer restartReadiness
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return answer
}

// deploy.sh waits on this route before it stops the service, so it must name
// a session whose process is running work, and only that: a row left busy by a
// process that is gone would hold every deploy until the time limit, and an
// idle session loses nothing to a restart.
func TestRestartReadinessNamesOnlyBusySessionsWithARunningProcess(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, st, instID := testServerWithInstance(t, msg.HarnessMock)

	live := postCreateSession(t, srv, autoStartRequest(instID))
	t.Cleanup(func() { srv.harness.Kill(live.SessionID) })
	if !srv.harness.HasProcess(live.SessionID) {
		t.Fatal("the session has no process; the test needs one")
	}
	const staleSessionID = "br_busy_row_without_a_process"
	if err := st.CreateSession(&store.Session{SessionID: staleSessionID, Harness: msg.HarnessMock, InstanceID: instID,
		State: string(msg.SessionModelGenerating), Type: msg.SessionTypeAutonomous}); err != nil {
		t.Fatalf("create stale session: %v", err)
	}

	writeStateForRestartReadinessTest(t, st, live.SessionID, msg.SessionIdle)
	if answer := readRestartReadiness(t, srv); !answer.Ready || len(answer.BusySessions) != 0 {
		t.Fatalf("with only an idle live session and a stale busy row: %+v; want ready, none busy", answer)
	}

	writeStateForRestartReadinessTest(t, st, live.SessionID, msg.SessionToolRunning)
	answer := readRestartReadiness(t, srv)
	if answer.Ready || len(answer.BusySessions) != 1 || answer.BusySessions[0].SessionID != live.SessionID ||
		answer.BusySessions[0].State != string(msg.SessionToolRunning) {
		t.Fatalf("with the live session running a tool: %+v; want not ready, that session alone", answer)
	}

	subagent := msg.BackgroundTask{TaskID: "a1", TaskType: msg.TaskTypeLocalAgent, Description: "Research the tuner"}
	data, err := json.Marshal(msg.Event{Type: msg.EventSystem, System: &msg.SystemEvent{
		Subtype: msg.SystemSubtypeBackgroundTasksChanged, BackgroundTasks: []msg.BackgroundTask{subagent}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StoreEventReturningID(live.SessionID, string(msg.EventSystem), "", "", data); err != nil {
		t.Fatalf("store background tasks: %v", err)
	}
	writeStateForRestartReadinessTest(t, st, live.SessionID, msg.SessionBackgroundTasksRunning)
	answer = readRestartReadiness(t, srv)
	if answer.Ready || len(answer.BusySessions) != 1 || len(answer.BusySessions[0].BackgroundTasks) != 1 ||
		answer.BusySessions[0].BackgroundTasks[0].TaskID != "a1" {
		t.Fatalf("with a background agent running: %+v; want not ready, naming task a1", answer)
	}
}
