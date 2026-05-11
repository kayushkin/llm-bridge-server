//go:build pty_integration

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kayushkin/llm-bridge/msg"
)

// PTY-mode end-to-end integration test. Live, slow, and assumes:
//
//   - `llm-bridge-claudecode` is on PATH (the harness wrapper).
//   - `claude` is on PATH (the upstream CLI it execs into).
//   - Whatever credential storage `claude` reads from is populated, OR the
//     CLI prints something on a missing-creds path. Either way the test
//     only asserts that bytes flow through the pty, not what those bytes
//     are — claude's auth/setup screens count as valid output.
//
// Gated by `//go:build pty_integration` so `go test ./...` skips it by
// default. Run locally with:
//
//	go test -tags pty_integration -run TestPTYIntegration ./internal/server/...
//
// See README.md → "PTY mode" → "Running the live integration test".

// claudecodeBin / claudeBin are the binary names probed before the test
// runs. If either is missing the test skips with a clear message rather
// than failing — pty_integration is opt-in, so a CI runner without claude
// installed should silently no-op when the tag is supplied.
const (
	claudecodeBin = "llm-bridge-claudecode"
	claudeBin     = "claude"
)

func TestPTYIntegration_ClaudeCode_RoundTrip(t *testing.T) {
	if _, err := exec.LookPath(claudecodeBin); err != nil {
		t.Skipf("%s not in PATH: %v", claudecodeBin, err)
	}
	if _, err := exec.LookPath(claudeBin); err != nil {
		t.Skipf("%s not in PATH: %v", claudeBin, err)
	}

	srv, st, instID := testServerWithInstance(t, msg.HarnessClaudeCode)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	bridgeID, attachToken := createPTYSession(t, ts.URL, instID)

	conn := dialAttach(t, ts.URL, bridgeID, attachToken)
	t.Cleanup(func() { _ = conn.Close() })

	// First frame after attach MUST be the role announcement (server-emitted
	// before the ring-buffer replay). On a fresh single-client attach this
	// is always "writer"; the server-side promotion logic is exercised by
	// attachhub_test.go.
	expectRoleFrame(t, conn, "writer")

	// Send a keystroke immediately. Claude's TUI takes several seconds
	// to come up and may not paint a banner until something nudges it,
	// so we don't wait for "first output" before typing — we just want
	// _some_ bytes to land in the pty stream within the deadline. The
	// trailing newline gets us past canonical-mode line buffering during
	// the brief window before claude flips into raw mode.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("?\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	// Wait until we've seen output that proves the *upstream claude TUI*
	// is actually running, not just pty echo of "?\n". The previous bar
	// — first non-empty binary frame — was met by the kernel's canonical-
	// mode echo of the keystroke, so the test fake-passed in 60ms even
	// when claude never finished booting. ANSI CSI sequences (`ESC [`)
	// are the cheapest robust signal that a TUI is repainting; the test
	// keystroke contains no escape bytes, so any ESC `[` we observe came
	// from claude itself (or from claude's terminal queries). 60s
	// headroom for first-run init (asar load, JS warmup).
	awaitClaudeTUI(t, conn, 60*time.Second)

	// Pin a minimum elapsed time before /stop so a regression that lets
	// claude die mid-startup (e.g. missing-creds path that prints an
	// error and exits in ~1s) gets caught here. Session state must
	// remain "running" — if claude exited and watchPTYExit fired, the
	// state will be "completed" and the assertion below trips.
	time.Sleep(5 * time.Second)
	finalDuringRun, err := st.GetSession(bridgeID)
	if err != nil {
		t.Fatalf("get session mid-run: %v", err)
	}
	if msg.SessionState(finalDuringRun.State) != msg.SessionRunning {
		t.Fatalf("session state mid-run = %q, want running (claude exited prematurely?)", finalDuringRun.State)
	}

	// Stop the session — exercises the writer-side teardown path the way
	// a real client does it (POST /stop, not just closing the WS). After
	// /stop returns, Manager.Kill has already SIGKILLed the child and
	// waited on cmd.Wait, so the pty fd is closed and watchPTYExit has
	// run (or is racing with the handler's own UpdateSessionState).
	stopResp, err := http.Post(ts.URL+"/sessions/"+bridgeID+"/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	stopResp.Body.Close()
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, want 200", stopResp.StatusCode)
	}

	// Drain WS until the server sends the {"type":"exit"} control frame
	// or the connection closes — whichever happens first proves the
	// attach hub tore down cleanly when the pty exited.
	if !awaitExitOrClose(t, conn, 5*time.Second) {
		t.Errorf("attach WS did not signal exit or close after stop")
	}

	// Session row should reflect a terminal state. handleStopSession sets
	// it to "aborted"; watchPTYExit (running in parallel for pty mode)
	// sets it to "completed" when the child exits. Either is a clean
	// outcome — what we care about is that it left "running".
	final, err := st.GetSession(bridgeID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	switch msg.SessionState(final.State) {
	case msg.SessionAborted, msg.SessionCompleted:
		// expected
	default:
		t.Errorf("final state = %q, want aborted or completed", final.State)
	}
}

// createPTYSession POSTs a pty-mode session with auto_start and returns
// its bridge id along with the per-session attach token. Fails the test
// on any non-201 response so callers can treat the result as live.
func createPTYSession(t *testing.T, baseURL, instID string) (string, string) {
	t.Helper()
	body := msg.CreateSessionRequest{
		Harness:    msg.HarnessClaudeCode,
		InstanceID: instID,
		Mode:       msg.SessionModePTY,
		AutoStart:  true,
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal create: %v", err)
	}
	resp, err := http.Post(baseURL+"/sessions", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST /sessions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /sessions status = %d: %s", resp.StatusCode, b)
	}
	// Decode into a flexible struct so we can read both the canonical
	// ManagedSession fields and the pty-only attach_token sibling without
	// adding a transient field to ManagedSession itself.
	var sess struct {
		msg.ManagedSession
		AttachToken string `json:"attach_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if sess.Mode != msg.SessionModePTY {
		t.Fatalf("session mode = %q, want pty", sess.Mode)
	}
	if sess.State != string(msg.SessionRunning) {
		t.Fatalf("session state = %q, want running (auto_start)", sess.State)
	}
	if sess.AttachToken == "" {
		t.Fatalf("attach_token missing in pty create response")
	}
	return sess.SessionID, sess.AttachToken
}

// dialAttach upgrades a fresh WebSocket against /sessions/{id}/attach,
// rewriting the http(s) test-server URL into ws(s). The attach token
// goes on the query string — that's the only browser-compatible auth
// channel for WebSockets and the only path the server accepts.
func dialAttach(t *testing.T, baseURL, bridgeID, attachToken string) *websocket.Conn {
	t.Helper()
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		t.Fatalf("unexpected scheme %q", u.Scheme)
	}
	u.Path = "/sessions/" + bridgeID + "/attach"
	q := u.Query()
	q.Set("token", attachToken)
	u.RawQuery = q.Encode()

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		t.Fatalf("ws dial %s: %v", u.String(), err)
	}
	return conn
}

// expectRoleFrame reads the next WS frame and fails the test unless it's
// the {"type":"role","role":wantRole} announcement. Run immediately after
// attach so we don't accidentally consume the ring-buffer replay frame.
func expectRoleFrame(t *testing.T, conn *websocket.Conn, wantRole string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	mt, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read role frame: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("first frame type = %d, want TextMessage (role announcement)", mt)
	}
	var ctrl struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(payload, &ctrl); err != nil {
		t.Fatalf("parse role frame %q: %v", payload, err)
	}
	if ctrl.Type != "role" || ctrl.Role != wantRole {
		t.Fatalf("first frame = %s, want type=role role=%s", payload, wantRole)
	}
	// Clear the deadline so subsequent reads use the helpers' own deadlines.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear read deadline: %v", err)
	}
}

// awaitClaudeTUI reads message frames until cumulative pty output
// contains at least one ANSI CSI sequence (ESC `[`) — the cheapest
// robust signal that a TUI is actually rendering — or the deadline
// passes. The accumulated buffer is also size-checked so a single
// stray ESC byte from a kernel response doesn't satisfy the contract.
//
// A text "exit" control frame from the server is surfaced as a
// t.Fatalf so a pty that dies during startup (missing creds, bad
// config) yields a clear diagnostic rather than the generic i/o
// timeout. The previous helper accepted the first non-empty binary
// frame, which kernel pty echo of the test's "?\n" satisfied in 60ms
// regardless of whether claude ever booted — see
// `5a22797d-6b03-4094-a26a-50824fa6974a` for the original write-up.
func awaitClaudeTUI(t *testing.T, conn *websocket.Conn, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	if err := conn.SetReadDeadline(deadline); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	const minBytes = 64 // raise the bar above kernel-echo of "?\n" (~3 bytes)
	var buf []byte
	for {
		mt, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read frame: %v (got %d bytes so far: %q)", err, len(buf), truncate(buf, 200))
		}
		if mt == websocket.BinaryMessage {
			buf = append(buf, payload...)
			if len(buf) >= minBytes && bytes.Contains(buf, []byte{0x1b, '['}) {
				return
			}
		}
		if mt == websocket.TextMessage {
			var ctrl struct {
				Type string `json:"type"`
				Code int    `json:"code"`
			}
			if err := json.Unmarshal(payload, &ctrl); err == nil && ctrl.Type == "exit" {
				t.Fatalf("pty exited before claude TUI produced output: ctrl=%s, got %d bytes: %q",
					string(payload), len(buf), truncate(buf, 200))
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no claude TUI output within %s; got %d bytes: %q",
				timeout, len(buf), truncate(buf, 200))
		}
	}
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}

// awaitExitOrClose drains the WS until either:
//   - a text frame parsed as {"type":"exit"} arrives, or
//   - ReadMessage returns an error (close / EOF / deadline).
//
// Returns true if the exit-or-close signal was observed inside timeout.
func awaitExitOrClose(t *testing.T, conn *websocket.Conn, timeout time.Duration) bool {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for {
		mt, payload, err := conn.ReadMessage()
		if err != nil {
			// Connection closed by server (or read deadline): both count
			// as a clean shutdown for our purposes — we just need to know
			// the pty isn't streaming forever.
			return true
		}
		if mt == websocket.TextMessage {
			var ctrl struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(payload, &ctrl); err == nil && ctrl.Type == "exit" {
				return true
			}
			if strings.Contains(string(payload), "exit") {
				return true
			}
		}
	}
}
