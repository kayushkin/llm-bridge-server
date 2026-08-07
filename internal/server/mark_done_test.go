package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// markDoneTestServer returns a server whose log-store push succeeds.
// handleMarkSessionDone goes through Manager.BroadcastEvent, which pushes
// the injected session_state event to log-store synchronously and returns
// the real error — so with the default unreachable "http://localhost:0"
// the handler 500s before it ever reaches the derivation this file is
// about, and every assertion below would be measuring the stub instead.
func markDoneTestServer(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	logStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(logStore.Close)
	return testServerWithInstanceAndLogStore(t, msg.HarnessClaudeCode, logStore.URL)
}

func seedMarkDoneSession(t *testing.T, st *store.Store, instanceID, id string) {
	t.Helper()
	if err := st.CreateSession(&store.Session{
		SessionID:  id,
		Harness:    "claude_code",
		InstanceID: instanceID,
		State:      string(msg.SessionIdle),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func readSession(t *testing.T, st *store.Store, id string) *store.Session {
	t.Helper()
	sess, err := st.GetSession(id)
	if err != nil {
		t.Fatalf("get session %s: %v", id, err)
	}
	if sess == nil {
		t.Fatalf("session %s not found", id)
	}
	return sess
}

func postMarkDone(t *testing.T, srv *Server, id string, done bool) {
	t.Helper()
	resp := doJSON(t, srv, "POST", "/sessions/"+id+"/mark-done", MarkSessionDoneRequest{Done: done})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("mark-done(done=%v) status = %d; want 204", done, resp.StatusCode)
	}
}

// TestMarkSessionDoneMovesBothHalvesInBothDirections is the property
// handleMarkSessionDone's own doc comment claims: it moves the session
// state AND the folder, and "reversing both undoes a manual mark".
//
// Marking done worked. Reversing it moved only the folder: the state
// half was dropped because derivation.go's EventSessionState pass-through
// listed every lifecycle state the handler can send EXCEPT the one it
// sends when done=false. A reopened session stayed "completed" forever,
// so anything reading session.state saw a finished session the user had
// explicitly put back on the board.
func TestMarkSessionDoneMovesBothHalvesInBothDirections(t *testing.T) {
	srv, st, instID := markDoneTestServer(t)
	const id = "br_mark_done_round_trip"
	seedMarkDoneSession(t, st, instID, id)

	postMarkDone(t, srv, id, true)
	if got := readSession(t, st, id); got.State != string(msg.SessionCompleted) || got.FolderName != store.ArchiveFolder {
		t.Fatalf("after mark done: state=%q folder=%q; want %q / %q",
			got.State, got.FolderName, msg.SessionCompleted, store.ArchiveFolder)
	}

	postMarkDone(t, srv, id, false)
	got := readSession(t, st, id)
	if got.FolderName != "" {
		t.Errorf("after reopen: folder=%q; want empty", got.FolderName)
	}
	if got.State != string(msg.SessionIdle) {
		t.Errorf("after reopen: state=%q; want %q — the state half of the mark did not reverse",
			got.State, msg.SessionIdle)
	}
}

// TestMarkSessionDoneReopenIsNotOnlyAFolderMove states the failure the
// test above catches as its own case, so a future change that fixes the
// folder while regressing the state cannot pass by half.
func TestMarkSessionDoneReopenIsNotOnlyAFolderMove(t *testing.T) {
	srv, st, instID := markDoneTestServer(t)
	const id = "br_mark_done_reopen"
	seedMarkDoneSession(t, st, instID, id)

	postMarkDone(t, srv, id, true)
	postMarkDone(t, srv, id, false)

	if got := readSession(t, st, id).State; got == string(msg.SessionCompleted) {
		t.Fatalf("reopened session still reads %q — the folder moved and the state did not", got)
	}
}
