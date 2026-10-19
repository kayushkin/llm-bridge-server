package server

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// A harness subagent never has a process of its own: it runs inside its
// parent's. The watchdog used to read that as a crashed session, log
// "resuming" and force it idle every tick (41 a day), and the resume never
// happened because the bridge does not control a subagent. While the parent's
// process runs it must leave the subagent alone; once that process is gone,
// nothing will report the task's end, so it records one.
func TestWatchdogLeavesARunningSubagentAloneAndEndsAnOrphanedOne(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, st, instID := testServerWithInstance(t, msg.HarnessMock)

	parent := postCreateSession(t, srv, autoStartRequest(instID))
	t.Cleanup(func() { srv.harness.Kill(parent.SessionID) })
	if !srv.harness.HasProcess(parent.SessionID) {
		t.Fatal("the parent has no process; the test needs one")
	}
	const subagentID = "br_subagent_under_watch"
	if err := st.CreateSession(&store.Session{SessionID: subagentID, Harness: msg.HarnessMock, InstanceID: instID,
		State: string(msg.SessionToolRunning), ManagerSessionID: parent.SessionID, ControlledBy: msg.ControlledByHarness,
		Purpose: "subagent", Type: msg.SessionTypeSystem}); err != nil {
		t.Fatalf("create subagent: %v", err)
	}

	srv.watchdogTick()
	if row, _ := st.GetSession(subagentID); row.State != string(msg.SessionToolRunning) {
		t.Fatalf("with the parent's process running, the watchdog moved the subagent to %s (status %+v)", row.State, row.Status)
	}

	// Kill reports the process's exit ("signal: killed"), which is not a
	// failure here; what matters is that no process is left.
	_ = srv.harness.Kill(parent.SessionID)
	if srv.harness.HasProcess(parent.SessionID) {
		t.Fatal("the parent's process is still registered after Kill")
	}
	// Settle the parent, or this same tick would resume it.
	if _, err := srv.harness.ForceSessionState(parent.SessionID, msg.SessionAborted, msg.ReasonKilled); err != nil {
		t.Fatal(err)
	}
	srv.watchdogTick()
	row, _ := st.GetSession(subagentID)
	if row.State != string(msg.SessionError) || row.Status == nil || row.Status.Reason != msg.ReasonParentProcessExited {
		t.Fatalf("with the parent's process gone: state %s status %+v; want error, parent_process_exited", row.State, row.Status)
	}
}
