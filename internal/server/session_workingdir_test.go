package server

import (
	"io"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// A working directory that only lives in the response is a working directory
// nothing spawns in: the harness manager reads it back out of the store at
// spawn time, so the create path has to persist it, not echo it.
func TestCreateSession_StoresTheWorkingDirectory(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, "claude_code")

	resp := doJSON(t, srv, "POST", "/sessions", msg.CreateSessionRequest{
		Harness:    "claude_code",
		InstanceID: instID,
		WorkingDir: "/srv/project",
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
	}
	sess := decodeJSON[msg.ManagedSession](t, resp)
	if sess.WorkingDir != "/srv/project" {
		t.Errorf("response working_dir = %q; want /srv/project", sess.WorkingDir)
	}

	stored, err := st.GetSession(sess.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.WorkingDir != "/srv/project" {
		t.Errorf("stored working_dir = %q; want /srv/project", stored.WorkingDir)
	}
}

// Omitting the field must leave the session on the instance's directory, which
// is where every session ran before this field existed. Inventing one here —
// bridge-server's own directory, say — would silently move every existing
// caller's sessions.
func TestCreateSession_NoWorkingDirectoryInheritsTheInstance(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, "claude_code")

	resp := doJSON(t, srv, "POST", "/sessions", msg.CreateSessionRequest{
		Harness:    "claude_code",
		InstanceID: instID,
	})
	sess := decodeJSON[msg.ManagedSession](t, resp)
	stored, err := st.GetSession(sess.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.WorkingDir != "" {
		t.Errorf("stored working_dir = %q; want empty (inherit the instance)", stored.WorkingDir)
	}
}
