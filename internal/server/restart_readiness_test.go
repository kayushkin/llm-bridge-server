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

// deploy.sh waits on this before it stops the service, so it must name a
// session that is doing work and has a running process, and only that: a row
// left busy by a process that is gone would hold every deploy until the time
// limit, and an idle session loses nothing to a restart.
func TestSessionsARestartWouldCutOffAreBusyAndHaveARunningProcess(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, msg.HarnessMock)
	rows := map[string]msg.SessionState{
		"br_running_a_tool":          msg.SessionToolRunning,
		"br_background_agents":       msg.SessionBackgroundTasksRunning,
		"br_idle_with_a_process":     msg.SessionIdle,
		"br_busy_row_but_no_process": msg.SessionModelGenerating,
	}
	for sessionID, state := range rows {
		if err := st.CreateSession(&store.Session{SessionID: sessionID, Harness: msg.HarnessMock, InstanceID: instID,
			State: string(state), Type: msg.SessionTypeAutonomous}); err != nil {
			t.Fatal(err)
		}
	}
	subagent := msg.BackgroundTask{TaskID: "a1", TaskType: msg.TaskTypeLocalAgent, Description: "Research the tuner"}
	data, err := json.Marshal(msg.Event{Type: msg.EventSystem, System: &msg.SystemEvent{
		Subtype: msg.SystemSubtypeBackgroundTasksChanged, BackgroundTasks: []msg.BackgroundTask{subagent}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StoreEventReturningID("br_background_agents", string(msg.EventSystem), "", "", data); err != nil {
		t.Fatal(err)
	}
	withProcess := map[string]bool{"br_running_a_tool": true, "br_background_agents": true, "br_idle_with_a_process": true}

	busy, err := srv.sessionsARestartWouldCutOff(func(sessionID string) bool { return withProcess[sessionID] })
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]sessionARestartWouldCutOff{}
	for _, entry := range busy {
		got[entry.SessionID] = entry
	}
	if len(got) != 2 {
		t.Fatalf("busy = %+v; want br_running_a_tool and br_background_agents only", busy)
	}
	if got["br_running_a_tool"].State != string(msg.SessionToolRunning) {
		t.Errorf("br_running_a_tool = %+v", got["br_running_a_tool"])
	}
	if tasks := got["br_background_agents"].BackgroundTasks; len(tasks) != 1 || tasks[0].TaskID != "a1" {
		t.Errorf("br_background_agents tasks = %+v; want task a1", tasks)
	}

	// The route itself: nothing here has a live process in the harness
	// manager, so it is ready, and it reports the drain and held messages.
	answer := readRestartReadiness(t, srv)
	if !answer.Ready || len(answer.BusySessions) != 0 || answer.Drain.Draining || answer.HeldMessages != 0 {
		t.Fatalf("readiness with no live processes = %+v; want ready, nothing busy, not draining", answer)
	}
	if err := st.HoldMessage(store.HeldMessage{HeldMessageID: "held_01TESTREADINESS00000000000", SessionID: "br_idle_with_a_process",
		Request: msg.SendMessageRequest{Message: "later"}, Source: "send", HeldAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if answer := readRestartReadiness(t, srv); answer.HeldMessages != 1 {
		t.Fatalf("held_messages = %d; want 1", answer.HeldMessages)
	}
}
