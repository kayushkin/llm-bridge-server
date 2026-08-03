package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// seedTranscript pushes one event carrying a harness session id, the way an
// earlier import would have left it in log-store.
func seedTranscript(t *testing.T, m *Manager, bridgeSessionID, harnessSessionID string) {
	t.Helper()
	ev := msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeSessionID, HarnessSessionID: harnessSessionID}
	if _, err := m.logStore.PushEvent(ev); err != nil {
		t.Fatalf("seed log-store: %v", err)
	}
}

// hideHarnessBinaries empties PATH for the test, so Available() finds nothing
// and an import that gets past the guard fails with "harness binary not
// found". Without it these tests assert on whichever harnesses happen to be
// installed on the host: a real llm-bridge-claudecode answers an unknown id
// with "session not found (no-op)" and exit 0, which is indistinguishable
// from the guard having skipped the import.
func hideHarnessBinaries(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// The whole point: a transcript log-store already holds is not imported again.
//
// With PATH emptied, an import that got past the guard fails on the missing
// binary — so (0, nil) is proof the guard fired, not proof the import was
// cheap.
func TestImportHistorySkipsATranscriptLogStoreAlreadyHolds(t *testing.T) {
	m := newTestManager(t)
	seedTranscript(t, m, "br_first_import", "cc-uuid-held")
	hideHarnessBinaries(t)

	n, err := m.ImportHistory(context.Background(), "br_fresh_database", msg.HarnessClaudeCode, "cc-uuid-held")
	if err != nil {
		t.Fatalf("ImportHistory: %v", err)
	}
	if n != 0 {
		t.Errorf("imported %d events, want 0 — log-store already holds this transcript", n)
	}
}

// The control. A harness session log-store has never seen must still be
// imported, and here that means reaching the exec and failing on the missing
// binary. Without this the test above passes on a function that skips
// everything.
func TestImportHistoryStillImportsATranscriptLogStoreDoesNotHold(t *testing.T) {
	m := newTestManager(t)
	seedTranscript(t, m, "br_first_import", "cc-uuid-held")
	hideHarnessBinaries(t)

	_, err := m.ImportHistory(context.Background(), "br_new", msg.HarnessClaudeCode, "cc-uuid-never-seen")
	if err == nil {
		t.Fatal("ImportHistory returned no error; wanted it to reach the harness exec")
	}
	if !strings.Contains(err.Error(), "harness binary not found") {
		t.Errorf("error = %v; wanted one from the import path, not from the guard", err)
	}
}

// A discovered session that names no harness id cannot be asked about. It has
// to import, or every id-less session on disk silently loses its history.
func TestImportHistoryWithNoHarnessIDImports(t *testing.T) {
	m := newTestManager(t)
	hideHarnessBinaries(t)

	_, err := m.ImportHistory(context.Background(), "br_new", msg.HarnessClaudeCode, "")
	if err == nil {
		t.Fatal("ImportHistory returned no error; an id-less session must still reach the import path")
	}
	if !strings.Contains(err.Error(), "harness binary not found") {
		t.Errorf("error = %v; wanted one from the import path, not from the guard", err)
	}
}

// bridge_session_id is still required, and the guard must not have moved that
// check behind a network call — a caller with no bridge id gets an error
// whether or not log-store is reachable.
func TestImportHistoryStillRequiresABridgeSessionID(t *testing.T) {
	m := newTestManager(t)
	seedTranscript(t, m, "br_first_import", "cc-uuid-held")

	if _, err := m.ImportHistory(context.Background(), "", msg.HarnessClaudeCode, "cc-uuid-held"); err == nil {
		t.Fatal("ImportHistory accepted an empty bridge_session_id")
	}
}

// Fail open, not closed. log-store being down must cost duplicates, never a
// transcript: nothing ever comes back for a history that was skipped.
func TestLogStoreUnreachableFailsOpen(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// A server that refuses every request the way a broken log-store would.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "log-store is having a bad day", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)

	m := NewManager(st, broken.URL, "http://127.0.0.1:0", "", 0, nil)
	if _, held := m.logStoreAlreadyHoldsTranscript("cc-uuid-anything"); held {
		t.Error("an erroring log-store reported the transcript as held; the guard must fail open")
	}
}

// A projection row with no events behind it is not a transcript. Treating it
// as one would suppress a real import on the strength of an empty row.
func TestSessionRowWithNoEventsIsNotHeld(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"harness_session_id": r.URL.Query().Get("harness_session_id"),
			"sessions": []map[string]any{
				{"session_id": "br_ghost", "event_count": 0, "last_active": ""},
			},
		})
	}))
	t.Cleanup(empty.Close)

	m := NewManager(st, empty.URL, "http://127.0.0.1:0", "", 0, nil)
	if _, held := m.logStoreAlreadyHoldsTranscript("cc-uuid-ghost"); held {
		t.Error("a session row with zero events reported the transcript as held")
	}
}

// The id the guard sends must be the harness id, not the bridge id. Sending
// the bridge id would make the check answer "no" every time — discovery mints
// a fresh bridge id on every import, which is the mechanism being fixed here.
func TestGuardAsksAboutTheHarnessID(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	asked := make(chan string, 4)
	spy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- r.URL.Query().Get("harness_session_id")
		json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]any{}})
	}))
	t.Cleanup(spy.Close)

	m := NewManager(st, spy.URL, "http://127.0.0.1:0", "", 0, nil)
	m.logStoreAlreadyHoldsTranscript("cc-uuid-asked-about")

	select {
	case got := <-asked:
		if got != "cc-uuid-asked-about" {
			t.Errorf("guard asked about %q, want cc-uuid-asked-about", got)
		}
	default:
		t.Fatal("guard never asked log-store anything")
	}
}

// A session with no harness id must not reach log-store at all. The client
// refuses an empty id too, so behaviour is identical either way — but going
// over the wire to be told no turns every id-less discovered session into a
// failed request and a "could not ask log-store" line in the gateway log,
// which reads as an outage. Discovery finds 1,675 such sessions on this host.
func TestGuardDoesNotAskAboutAnEmptyHarnessID(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	var requests atomic.Int64
	spy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]any{}})
	}))
	t.Cleanup(spy.Close)

	m := NewManager(st, spy.URL, "http://127.0.0.1:0", "", 0, nil)
	if _, held := m.logStoreAlreadyHoldsTranscript(""); held {
		t.Error("an empty harness id reported a held transcript")
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("guard made %d log-store requests for an empty harness id, want 0", n)
	}
}

// realLogStoreURL is the address of a REAL log-store the canary script booted
// for this run. Empty means the cross-service check is not being asked for.
//
// It exists because every other test here talks to logStoreStub, a
// hand-written mirror of log-store's HTTP surface living in this repo. A
// mirror agrees with itself forever: if log-store renames the field, moves the
// route or changes what an unknown id returns, the stub keeps answering the
// old way and the guard keeps passing while the live gateway re-imports
// everything on disk. This is the only check in the package that would notice.
const realLogStoreURLEnv = "LLMBRIDGE_CANARY_LOG_STORE_URL"

// Drives the guard against a real log-store. Run by
// scripts/import-dedupe-canary.sh, which boots one on a throwaway port and
// database; skipped otherwise.
func TestImportHistoryAgainstRealLogStore(t *testing.T) {
	url := os.Getenv(realLogStoreURLEnv)
	if url == "" {
		t.Skipf("%s not set; run scripts/import-dedupe-canary.sh for the cross-service check", realLogStoreURLEnv)
	}
	// The production log-store is the one store whose writes cannot be thrown
	// away with a temp directory, and this test writes events. Refusing the
	// default URL is not paranoia: an isolated canary that left
	// LLMBRIDGE_LOG_STORE_URL unset is exactly how 2,863 duplicate sessions
	// reached it on 2026-08-01.
	if strings.Contains(url, ":8175") {
		t.Fatalf("%s=%s points at the production log-store; boot a throwaway one", realLogStoreURLEnv, url)
	}

	st, err := store.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	m := NewManager(st, url, "http://127.0.0.1:0", "", 0, nil)

	harnessSessionID := fmt.Sprintf("canary-harness-%d", os.Getpid())
	seedTranscript(t, m, "br_canary_first_import", harnessSessionID)
	hideHarnessBinaries(t)

	n, err := m.ImportHistory(context.Background(), "br_canary_fresh_database", msg.HarnessClaudeCode, harnessSessionID)
	if err != nil {
		t.Fatalf("ImportHistory against a real log-store: %v", err)
	}
	if n != 0 {
		t.Errorf("imported %d events, want 0 — a real log-store already holds this transcript", n)
	}

	if _, err := m.ImportHistory(context.Background(), "br_canary_new", msg.HarnessClaudeCode, harnessSessionID+"-never-seen"); err == nil {
		t.Error("an unheld transcript did not reach the import path against a real log-store")
	}
}
