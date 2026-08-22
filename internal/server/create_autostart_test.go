package server

import (
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// The tests below pin what `POST /sessions {auto_start:true}` does on the
// SUCCESS path. Before them, the only compiled test naming AutoStart was
// TestCreateSession_AutoStart_HarnessUnavailable, which never reached the
// auto_start branch at all — see the note on the test now called
// TestCreateSessionWithoutHarnessStoreConfiguredIsUnavailable.
//
// That gap has a price already paid. Commit 72cc3c8 (2026-08-04) narrowed the
// reported success state from "running" to "starting"; nothing a default
// `go test ./...` builds noticed, and two tag-gated tests sat red on the old
// value for eighteen days with no way to say so.
//
// The seam is harness.Available -> exec.LookPath(msg.HarnessBinaryName(h)), so
// PATH decides whether the handler meets a real subprocess. This file uses the
// seam the repo already has — buildMockHarnessOnPath, from
// interrupt_resume_live_test.go, which builds cmd/mock-harness as
// llm-bridge-mock and prepends it to PATH. A second, competing stand-in was
// written first and thrown away: the repo has one harness double and these
// tests should not add another.

// pathWithNoHarnessBinary points PATH at an empty directory, so no harness
// binary resolves. This is the absence of the seam above, not a rival to it.
func pathWithNoHarnessBinary(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// autoStartRequest is the create body these tests post.
func autoStartRequest(instanceID string) msg.CreateSessionRequest {
	return msg.CreateSessionRequest{
		Type:       msg.SessionTypeInteractive,
		Purpose:    msg.PurposeChat,
		Origin:     "test",
		Harness:    msg.HarnessMock,
		InstanceID: instanceID,
		AutoStart:  true,
	}
}

// postCreateSession posts the body and returns the session the handler
// reported, failing the test on any status but 201.
func postCreateSession(t *testing.T, srv *Server, body msg.CreateSessionRequest) *store.Session {
	t.Helper()
	resp := doJSON(t, srv, "POST", "/sessions", body)
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, raw)
	}
	var got store.Session
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return &got
}

// TestCreateSessionWithAutoStartReportsStarting pins the state a successful
// auto_start reports and stores: "starting", the value 72cc3c8 introduced.
//
// It stays "starting" rather than advancing to "running", and that is by
// design, not a race. mock-harness emits a session_state=running the moment it
// handles "start", but Manager.readEvents drops harness-emitted
// EventSessionState outright — session state is derived centrally, because a
// harness cannot see the context the server has. So "starting" is where an
// auto-started session sits until the server itself moves it, and asserting
// it is deterministic.
func TestCreateSessionWithAutoStartReportsStarting(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, st, instID := testServerWithInstance(t, msg.HarnessMock)

	got := postCreateSession(t, srv, autoStartRequest(instID))
	t.Cleanup(func() { srv.harness.Kill(got.SessionID) })

	if got.State != string(msg.SessionStarting) {
		t.Errorf("reported state = %q, want %q", got.State, msg.SessionStarting)
	}

	// The manager writes the pid from StartOnInstance before the handler
	// returns, so the row already carries it. A zero pid here means nothing
	// was spawned and the state above came from somewhere other than a live
	// process.
	row, err := st.GetSession(got.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if row.PID == 0 {
		t.Errorf("persisted PID = 0, want the spawned harness pid")
	}
	if row.State != string(msg.SessionStarting) {
		t.Errorf("persisted state = %q, want %q", row.State, msg.SessionStarting)
	}

	// Re-read after the harness has certainly emitted its own running state,
	// to pin that the server does not adopt it.
	time.Sleep(250 * time.Millisecond)
	row, err = st.GetSession(got.SessionID)
	if err != nil {
		t.Fatalf("re-read session: %v", err)
	}
	if row.State != string(msg.SessionStarting) {
		t.Errorf("state after the harness emitted its own = %q, want it still %q "+
			"(readEvents drops harness-emitted EventSessionState)", row.State, msg.SessionStarting)
	}
}

// TestCreateSessionWithAutoStartReportsErrorWhenBinaryMissing is the control
// for the test above: identical request and instance, only the harness binary
// absent from PATH. It pins the other half of the branch — a spawn that fails
// leaves the session created and marked "error" rather than failing the
// request — and it is what shows PATH is the thing moving the verdict.
func TestCreateSessionWithAutoStartReportsErrorWhenBinaryMissing(t *testing.T) {
	pathWithNoHarnessBinary(t)
	srv, st, instID := testServerWithInstance(t, msg.HarnessMock)

	got := postCreateSession(t, srv, autoStartRequest(instID))

	if got.State != string(msg.SessionError) {
		t.Errorf("reported state = %q, want %q", got.State, msg.SessionError)
	}
	row, err := st.GetSession(got.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if row.State != string(msg.SessionError) {
		t.Errorf("persisted state = %q, want %q", row.State, msg.SessionError)
	}
	if row.PID != 0 {
		t.Errorf("persisted PID = %d, want 0 (nothing should have spawned)", row.PID)
	}
}

// TestCreateSessionWithoutAutoStartSpawnsNoProcess is the third leg, and it is
// what makes the first test an assertion about auto_start rather than about
// session creation: same request with auto_start omitted, the harness binary
// still on PATH, and nothing may spawn.
//
// TestCreateSession_NoAutoStart already covers the reported state for this
// case. What it does not check is that no process was started — it asserts the
// response body only, and the response body would read "idle" whether or not a
// child was spawned. The pid on the row is the part that says so.
func TestCreateSessionWithoutAutoStartSpawnsNoProcess(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, st, instID := testServerWithInstance(t, msg.HarnessMock)

	req := autoStartRequest(instID)
	req.AutoStart = false

	got := postCreateSession(t, srv, req)

	if got.State != string(msg.SessionIdle) {
		t.Errorf("reported state = %q, want %q", got.State, msg.SessionIdle)
	}

	// Give a stray spawn time to reach the row, so this cannot pass merely by
	// reading it before anything happened.
	time.Sleep(250 * time.Millisecond)
	row, err := st.GetSession(got.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if row.State != string(msg.SessionIdle) {
		t.Errorf("persisted state = %q, want %q", row.State, msg.SessionIdle)
	}
	if row.PID != 0 {
		t.Errorf("persisted PID = %d, want 0 (nothing should have spawned)", row.PID)
	}
}
