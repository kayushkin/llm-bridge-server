package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
	snapshotstore "github.com/kayushkin/snapshot-store"
)

// Two session ids reach this handler and only one of them is the key the rows
// were written under. Every assertion here is built so that swapping one for the
// other fails: the seeded bridge id and harness id are different strings, and
// the capture happens under the harness id alone.
//
// The bug this pins shipped and survived because the wrong answer is a 200 with
// an empty list — indistinguishable from "this tool touched no file" unless the
// test knows a file was in fact captured.
const (
	snapTestBridgeID  = "br_1785171126409277953"
	snapTestHarnessID = "0d69dd7d-7918-4380-8cce-074498d42b33"
	snapTestToolUseID = "toolu_0112jYRzLLLNvCzi9tV3mnha"
)

// snapshotTestServer returns a server wired to a real snapshot store on disk.
// A real one rather than a stub: Capture writes a git blob and a row, and the
// point of the test is the key those land under, which a stub would let the
// test author choose.
func snapshotTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	srv, st, _ := testServerWithInstance(t, msg.HarnessClaudeCode)

	dir := t.TempDir()
	ss, err := snapshotstore.Open(snapshotstore.Config{
		DBPath: filepath.Join(dir, "snapshots.db"),
		GitDir: filepath.Join(dir, "blobs.git"),
	})
	if err != nil {
		t.Fatalf("open snapshot store: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	srv.snapshotStore = ss
	return srv, st
}

func seedSnapshotSession(t *testing.T, st *store.Store, bridgeID, harnessID string) {
	t.Helper()
	if err := st.CreateSession(&store.Session{
		SessionID: bridgeID,
		Harness:   "claude_code",
		State:     string(msg.SessionIdle),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if harnessID == "" {
		return
	}
	if err := st.SetHarnessSessionID(bridgeID, harnessID); err != nil {
		t.Fatalf("set harness session id: %v", err)
	}
}

// captureUnderHarnessID writes a before/after pair the way production does:
// keyed by the harness's own session id, which is what arrives on the hook-exec
// payload.
func captureUnderHarnessID(t *testing.T, srv *Server, harnessID string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "subject.go")
	if err := os.WriteFile(path, []byte("package before\n"), 0o644); err != nil {
		t.Fatalf("write subject: %v", err)
	}
	if _, err := srv.snapshotStore.Capture(harnessID, snapTestToolUseID, snapshotstore.PhaseBefore, path); err != nil {
		t.Fatalf("capture before: %v", err)
	}
	if err := os.WriteFile(path, []byte("package after\n"), 0o644); err != nil {
		t.Fatalf("rewrite subject: %v", err)
	}
	if _, err := srv.snapshotStore.Capture(harnessID, snapTestToolUseID, snapshotstore.PhaseAfter, path); err != nil {
		t.Fatalf("capture after: %v", err)
	}
	return path
}

func getSnapshots(t *testing.T, srv *Server, bridgeID string) (int, []fileSnapshots) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/sessions/"+bridgeID+"/tools/"+snapTestToolUseID+"/snapshots", nil)
	r.SetPathValue("id", bridgeID)
	r.SetPathValue("tool_use_id", snapTestToolUseID)
	w := httptest.NewRecorder()

	srv.handleGetSnapshots(w, r)

	if w.Code != http.StatusOK {
		return w.Code, nil
	}
	var body struct {
		Files []fileSnapshots `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (body %q)", err, w.Body.String())
	}
	return w.Code, body.Files
}

// The regression itself: rows written under the harness id must be reachable
// through the bridge id the route is addressed by.
func TestGetSnapshotsResolvesTheHarnessSessionID(t *testing.T) {
	srv, st := snapshotTestServer(t)
	seedSnapshotSession(t, st, snapTestBridgeID, snapTestHarnessID)
	path := captureUnderHarnessID(t, srv, snapTestHarnessID)

	code, files := getSnapshots(t, srv, snapTestBridgeID)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1 — a lookup keyed on the bridge id returns 0 here, which is the bug", len(files))
	}
	if files[0].FilePath != path {
		t.Errorf("file_path = %q, want %q", files[0].FilePath, path)
	}
	// Both phases must survive the grouping: a diff needs two sides, and one
	// side present is the same blank card to a reader as none.
	if files[0].Before == nil || files[0].After == nil {
		t.Errorf("before/after = %v/%v, want both present", files[0].Before, files[0].After)
	}
}

// The inverse, which is what makes the test above mean something. Querying by
// the harness id — the key the rows are actually under — must NOT work, because
// this route's {id} is a bridge id and nothing else. Without this case the
// handler could resolve nothing at all and still pass, by reading the path value
// straight through as it used to.
func TestGetSnapshotsRejectsAHarnessIDInTheRoute(t *testing.T) {
	srv, st := snapshotTestServer(t)
	seedSnapshotSession(t, st, snapTestBridgeID, snapTestHarnessID)
	captureUnderHarnessID(t, srv, snapTestHarnessID)

	code, _ := getSnapshots(t, srv, snapTestHarnessID)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — no session is addressed by a harness id", code)
	}
}

// A session that never reached its harness cannot have captured anything, and
// "nothing was captured" is an answer, not a failure.
func TestGetSnapshotsEmptyWhenNoHarnessSessionYet(t *testing.T) {
	srv, st := snapshotTestServer(t)
	seedSnapshotSession(t, st, snapTestBridgeID, "")

	code, files := getSnapshots(t, srv, snapTestBridgeID)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(files) != 0 {
		t.Fatalf("files = %d, want 0", len(files))
	}
}

func TestGetSnapshotsUnknownSessionIs404(t *testing.T) {
	srv, _ := snapshotTestServer(t)

	code, _ := getSnapshots(t, srv, "br_does_not_exist")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

// A tool call that captured nothing must answer with an empty list rather than
// the previous call's files — the session resolves, the tool id does not.
func TestGetSnapshotsUnknownToolUseIDIsEmpty(t *testing.T) {
	srv, st := snapshotTestServer(t)
	seedSnapshotSession(t, st, snapTestBridgeID, snapTestHarnessID)
	captureUnderHarnessID(t, srv, snapTestHarnessID)

	r := httptest.NewRequest(http.MethodGet, "/sessions/"+snapTestBridgeID+"/tools/toolu_absent/snapshots", nil)
	r.SetPathValue("id", snapTestBridgeID)
	r.SetPathValue("tool_use_id", "toolu_absent")
	w := httptest.NewRecorder()

	srv.handleGetSnapshots(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Files []fileSnapshots `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Files) != 0 {
		t.Fatalf("files = %d, want 0", len(body.Files))
	}
}
