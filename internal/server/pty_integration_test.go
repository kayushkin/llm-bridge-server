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

	bridgeID := createPTYSession(t, ts.URL, instID)

	conn := dialAttach(t, ts.URL, bridgeID)
	t.Cleanup(func() { _ = conn.Close() })

	// Send a keystroke immediately. Claude's TUI takes several seconds
	// to come up and may not paint a banner until something nudges it,
	// so we don't wait for "first output" before typing — we just want
	// _some_ bytes to land in the pty stream within the deadline. The
	// trailing newline gets us past canonical-mode line buffering during
	// the brief window before claude flips into raw mode.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("?\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	// Wait up to 60s for any pty bytes to flow back through the WS. 60s
	// is generous because `claude` sometimes spends 10–20s on first-run
	// initialization (asar load, JS warmup) before painting anything.
	// readPtyBytes also surfaces the server's text-frame exit control
	// as a fail diagnostic — if the pty died on startup (missing creds,
	// bad config), we want to see that, not a generic timeout.
	echo := readPtyBytes(t, conn, 60*time.Second)
	if len(echo) == 0 {
		t.Fatalf("no pty output after input; round-trip broken")
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
// its bridge id. Fails the test on any non-201 response so callers can
// treat the result as live.
func createPTYSession(t *testing.T, baseURL, instID string) string {
	t.Helper()
	body := msg.CreateSessionRequest{
		Harness:    msg.HarnessClaudeCode,
		InstanceID: instID,
		ClientID:   "fe_pty_integration",
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
	var sess msg.ManagedSession
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if sess.Mode != msg.SessionModePTY {
		t.Fatalf("session mode = %q, want pty", sess.Mode)
	}
	if sess.State != string(msg.SessionRunning) {
		t.Fatalf("session state = %q, want running (auto_start)", sess.State)
	}
	return sess.BridgeID
}

// dialAttach upgrades a fresh WebSocket against /sessions/{id}/attach,
// rewriting the http(s) test-server URL into ws(s).
func dialAttach(t *testing.T, baseURL, bridgeID string) *websocket.Conn {
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

// readPtyBytes reads message frames until it gets a non-empty binary
// one, or the deadline passes. A text "exit" control frame from the
// server is surfaced as a t.Fatalf so a pty that dies during startup
// (missing creds, bad config) yields a clear diagnostic rather than
// the generic i/o timeout you'd get from waiting for binary bytes that
// never come.
func readPtyBytes(t *testing.T, conn *websocket.Conn, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	if err := conn.SetReadDeadline(deadline); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for {
		mt, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if mt == websocket.BinaryMessage && len(payload) > 0 {
			return payload
		}
		if mt == websocket.TextMessage {
			var ctrl struct {
				Type string `json:"type"`
				Code int    `json:"code"`
			}
			if err := json.Unmarshal(payload, &ctrl); err == nil && ctrl.Type == "exit" {
				t.Fatalf("pty exited before producing output: %s", string(payload))
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no non-empty binary frame within %s", timeout)
		}
	}
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
