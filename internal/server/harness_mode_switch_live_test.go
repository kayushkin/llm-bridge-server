//go:build harness_live

package server

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	harnessstore "github.com/kayushkin/harness-store"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/productiondefaults"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
	modelstore "github.com/kayushkin/model-store"
	_ "modernc.org/sqlite"
)

// Live tests for the two ways a person leaves the dash's chat view and talks
// to Claude Code or Codex in its own terminal screen, and what the dash shows
// while they do:
//
//  1. Mode switch inside the bridge (TestHarnessModeSwitchLive): the session
//     starts in events mode, switches to pty (the CLI's own screen, streamed
//     to the dash), and back. The conversation must carry across both
//     switches, and a turn typed into the pty must reach the dash's event
//     stream and move the session's state.
//
//  2. The CLI run outside the bridge (TestHarnessExternalCLILive): the bridge
//     already knows the session, the person runs `claude --resume` or
//     `codex exec resume` in their own terminal, and the dash must show the
//     new turn. A second case runs a brand-new conversation outside the
//     bridge and asks that it appear in the session list.
//
// What "the dash shows" means here is what the dash reads: the session row
// (GET /sessions/{id}: mode, state, harness_session_id), the per-session event
// stream (GET /sessions/{id}/events, the live chat), and log-store, which the
// chat loads history from. log-store is a fake that records what it is sent.
//
// These spend real model calls on the operator's own logins, take minutes,
// and drive the real CLIs, so they sit behind a build tag:
//
//	go test -tags harness_live -run 'TestHarness(ModeSwitch|ExternalCLI)Live' -v -timeout 20m ./internal/server/
//
// Each test runs once per harness. A harness whose wrapper or CLI is not on
// PATH is skipped, not failed.
//
// No server-side discovery runs here. In production it runs once, when the
// server starts (cmd/llm-bridge-server calls AutoDiscover), so a conversation
// that happens in a terminal while the server is up is exactly as invisible to
// a running server as it is to this one.

// liveHarness names one harness and the binaries it needs.
type liveHarness struct {
	harness    msg.Harness
	wrapperBin string
	cliBin     string
	// screensThatTakeKeystrokes are texts of startup screens a typed line
	// would answer instead of reaching the model. The test stops rather than
	// type into one: on 2026-10-20 a run typed into Codex's "Update
	// available!" screen, which took the keystrokes as "update now" and ran
	// npm install -g @openai/codex, upgrading the host's Codex.
	screensThatTakeKeystrokes []string
	// modelProvider is the model-store provider whose models this CLI runs.
	// The session's model is the first model of production's efficient role
	// from this provider (liveModelForHarness).
	modelProvider string
}

var liveHarnesses = []liveHarness{
	{harness: msg.HarnessClaudeCode, wrapperBin: "llm-bridge-claudecode", cliBin: "claude", modelProvider: "anthropic"},
	{harness: msg.HarnessCodex, wrapperBin: "llm-bridge-codex", cliBin: "codex", modelProvider: "openai",
		screensThatTakeKeystrokes: []string{"Update available"}},
}

// liveWorkingDirectory is where every session in these tests runs. Both CLIs
// stop at a "do you trust this folder" screen in a folder they have not been
// told to trust, which a test cannot answer through a pty without knowing the
// screen's layout. /tmp is trusted in this host's ~/.codex/config.toml, and
// Claude Code's trust of "/" covers it.
const liveWorkingDirectory = "/tmp"

func requireLiveHarness(t *testing.T, h liveHarness) {
	t.Helper()
	for _, bin := range []string{h.wrapperBin, h.cliBin} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH: %v", bin, err)
		}
	}
	clearParentAgentEnvironment(t)
}

// parentAgentEnvironmentPrefixes name the variables an agent session puts in
// the environment of every command it runs. An agent is who runs these tests,
// and the server passes its own environment to each harness child, so without
// this a child believes it is nested inside the agent: Claude Code reads
// CLAUDE_CODE_CHILD_SESSION and stops writing its session file ("Transcript
// saving is off — inherited CLAUDE_CODE_CHILD_SESSION marker"), which leaves
// the pty sidecar nothing to read. The production server runs under systemd
// and has none of them.
var parentAgentEnvironmentPrefixes = []string{"CLAUDE", "CODEX_", "LLM_BRIDGE_"}

// clearParentAgentEnvironment unsets those variables for this test and puts
// them back when it ends.
func clearParentAgentEnvironment(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		for _, prefix := range parentAgentEnvironmentPrefixes {
			if strings.HasPrefix(name, prefix) {
				t.Setenv(name, "") // registers the restore
				os.Unsetenv(name)
				break
			}
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// 1. events → pty → events inside the bridge
// ──────────────────────────────────────────────────────────────────────────────

func TestHarnessModeSwitchLive(t *testing.T) {
	for _, h := range liveHarnesses {
		t.Run(string(h.harness), func(t *testing.T) {
			requireLiveHarness(t, h)
			env := newLiveBridge(t, h)

			codeWord := liveCodeWord()
			bridgeID := env.createEventsSession(t)
			env.sendAndWaitForTurnEnd(t, bridgeID, fmt.Sprintf(
				"Remember this code word for later in our conversation: %s. Reply with only the word OK.", codeWord))
			harnessSessionID := env.waitForHarnessSessionID(t, bridgeID)
			t.Logf("events turn done; harness_session_id=%s", harnessSessionID)

			// ── switch to pty ──
			stream := env.subscribeSessionEvents(t, bridgeID)
			switched := env.switchMode(t, bridgeID, msg.SessionModePTY)
			if switched.Session.Mode != msg.SessionModePTY {
				t.Fatalf("mode after switch to pty = %q, want pty", switched.Session.Mode)
			}
			if switched.AttachToken == "" {
				t.Fatalf("switch to pty returned no attach_token")
			}
			env.expectSessionRow(t, bridgeID, msg.SessionModePTY, harnessSessionID)

			conn := dialAttach(t, env.baseURL, bridgeID, switched.AttachToken)
			t.Cleanup(func() { _ = conn.Close() })
			expectRoleFrame(t, conn, "writer")
			screen := newPTYScreen(conn)
			screen.waitForTUI(t, 60*time.Second)
			screen.refuseScreensThatTakeKeystrokes(t, h.screensThatTakeKeystrokes)

			ptyQuestion := fmt.Sprintf("What code word did I ask you to remember? Reply with only the word. (check %s)", liveCodeWord())
			screen.typeLine(t, ptyQuestion)

			// The dash's chat must show the line typed into the pty, and the
			// answer. They are checked apart because they fail apart: the
			// prompt and the answer come from different entries in the CLI's
			// session file. The first turn's answer was "OK", so an answer
			// naming the code word can only come from the pty turn, and it is
			// also what proves the pty process resumed the events-mode
			// conversation rather than starting a new one.
			_, sawPrompt := stream.waitFor(90*time.Second, func(ev msg.Event) bool {
				return ev.Type == msg.EventUserMessage && strings.Contains(eventText(ev), ptyQuestion)
			})
			_, answeredInDash := stream.waitFor(30*time.Second, func(ev msg.Event) bool {
				return isAssistantText(ev) && strings.Contains(eventText(ev), codeWord)
			})
			if !sawPrompt {
				t.Errorf("the dash's event stream never showed the message typed into the pty (%d events seen: %s)",
					stream.count(), stream.summary())
			}
			if !answeredInDash {
				t.Errorf("the dash's event stream never showed an answer naming %s (%s)", codeWord, stream.summary())
			}
			if !sawPrompt || !answeredInDash {
				// The pty screen itself says whether the CLI resumed the
				// conversation, which separates "no resume" from "no events".
				if screen.waitForText(60*time.Second, codeWord) {
					t.Logf("the pty screen shows %s, so the CLI resumed the conversation", codeWord)
				} else {
					t.Errorf("the pty screen never showed %s: the CLI did not resume the events-mode conversation", codeWord)
				}
				t.Logf("pty screen, last 3000 characters:\n%s", screen.text(3000))
				t.Logf("events with text on the dash's stream:\n%s", stream.dump())
			}

			// The session row's state is what the dash's sidebar and Send button
			// read. After the pty turn ends it must say so; a row stuck in an
			// active state also refuses the switch back (session_busy).
			if !env.waitForSettledState(t, bridgeID, 60*time.Second) {
				t.Errorf("session after the pty turn is not settled (idle or awaiting_user): %s", env.describeState(t, bridgeID))
			}
			env.expectSessionRow(t, bridgeID, msg.SessionModePTY, harnessSessionID)

			// ── switch back to events ──
			back := env.switchMode(t, bridgeID, msg.SessionModeEvents)
			if back.Session.Mode != msg.SessionModeEvents {
				t.Fatalf("mode after switch to events = %q, want events", back.Session.Mode)
			}
			if !awaitExitOrClose(t, conn, 10*time.Second) {
				t.Errorf("attach socket did not close after switching back to events")
			}
			env.expectSessionRow(t, bridgeID, msg.SessionModeEvents, harnessSessionID)

			answer := env.sendAndWaitForTurnEnd(t, bridgeID,
				"Repeat the code word from the start of our conversation. Reply with only the word.")
			if !strings.Contains(answer, codeWord) {
				t.Errorf("events-mode answer after switching back = %q, want it to contain %s (conversation lost across the switch)", answer, codeWord)
			}
			env.expectSessionRow(t, bridgeID, msg.SessionModeEvents, harnessSessionID)
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// 2. the CLI run in the person's own terminal
// ──────────────────────────────────────────────────────────────────────────────

func TestHarnessExternalCLILive(t *testing.T) {
	for _, h := range liveHarnesses {
		t.Run(string(h.harness)+"/resume_known_session", func(t *testing.T) {
			requireLiveHarness(t, h)
			env := newLiveBridge(t, h)

			codeWord := liveCodeWord()
			bridgeID := env.createEventsSession(t)
			env.sendAndWaitForTurnEnd(t, bridgeID, fmt.Sprintf(
				"Remember this code word for later in our conversation: %s. Reply with only the word OK.", codeWord))
			harnessSessionID := env.waitForHarnessSessionID(t, bridgeID)

			// The person leaves the dash: the bridge's process stops, the row
			// stays, and its harness_session_id is what they resume.
			env.stopSession(t, bridgeID)
			stream := env.subscribeSessionEvents(t, bridgeID)
			logStoreEventsBefore := env.logStore.count()
			before := env.getSession(t, bridgeID)

			externalQuestion := fmt.Sprintf("What code word did I ask you to remember? Reply with only the word. (check %s)", liveCodeWord())
			output := runExternalCLI(t, h, externalResumeArgs(h, harnessSessionID, externalQuestion))
			if !strings.Contains(output, codeWord) {
				t.Fatalf("the external CLI's own answer does not contain %s, so it did not resume the session; output: %s", codeWord, truncateString(output, 600))
			}
			t.Logf("external CLI resumed %s and answered with %s", harnessSessionID, codeWord)

			// The dash must now show that turn, live or in history.
			deadline := 90 * time.Second
			_, inStream := stream.waitFor(deadline, func(ev msg.Event) bool {
				return ev.Type == msg.EventUserMessage && strings.Contains(eventText(ev), externalQuestion)
			})
			inLogStore := env.logStore.containsSince(logStoreEventsBefore, externalQuestion)
			if !inStream && !inLogStore {
				t.Errorf("after %s the dash shows nothing of the turn run outside the bridge: not on the session's event stream (%d events) and not sent to log-store (%d new posts)",
					deadline, stream.count(), env.logStore.count()-logStoreEventsBefore)
			}
			after := env.getSession(t, bridgeID)
			if !after.UpdatedAt.After(before.UpdatedAt) {
				t.Errorf("session row updated_at did not move (%s), so the session list will not show recent activity", after.UpdatedAt.Format(time.RFC3339))
			}
		})

		t.Run(string(h.harness)+"/new_session_appears", func(t *testing.T) {
			requireLiveHarness(t, h)
			env := newLiveBridge(t, h)

			harnessSessionID := runExternalNewSession(t, h, "Reply with only the word OK.")
			t.Logf("external CLI started conversation %s", harnessSessionID)

			deadline := 90 * time.Second
			end := time.Now().Add(deadline)
			for time.Now().Before(end) {
				if sess, err := env.store.GetSessionByHarnessSessionID(harnessSessionID); err == nil && sess != nil {
					t.Logf("conversation %s appeared as %s", harnessSessionID, sess.SessionID)
					return
				}
				time.Sleep(time.Second)
			}
			t.Errorf("a conversation started outside the bridge (%s) did not appear in the session list within %s", harnessSessionID, deadline)
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// the bridge under test
// ──────────────────────────────────────────────────────────────────────────────

type liveBridge struct {
	server     *Server
	store      *store.Store
	instanceID string
	baseURL    string
	logStore   *recordingLogStore
	model      string
}

// newLiveBridge builds a server like testServerWithInstance does, with two
// differences a live harness needs. ListenAddr names the test server's own
// address, because a harness calls back to it: Claude Code's pty sidecar posts
// its events to {ListenAddr}/sidecar/event/{id}, and the permission hook asks
// the bridge before each tool. Left empty, a pty session gets no sidecar at
// all. log-store is a recording fake, so the chat's history is visible. And
// model-store is a copy of production's, because the unit tests' registry
// holds only mock models that a real CLI refuses.
func newLiveBridge(t *testing.T, h liveHarness) *liveBridge {
	t.Helper()
	harness := h.harness
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	hs, err := harnessstore.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("open harness-store: %v", err)
	}
	t.Cleanup(func() { hs.Close() })
	mach := &msg.Machine{ID: "m_live", Name: "live-machine", Hostname: "localhost", Transport: msg.TransportLocal}
	if err := hs.CreateMachine(mach); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	inst := &msg.Instance{ID: "inst_live", HarnessType: harness, Name: "live-instance", MachineID: mach.ID, Enabled: true}
	if err := hs.CreateInstance(inst); err != nil {
		t.Fatalf("seed instance: %v", err)
	}

	logStore := newRecordingLogStore(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg := &config.Config{
		ImagesDir:       filepath.Join(dir, "images"),
		BridgePrefsPath: filepath.Join(dir, "prefs.json"),
		LogStoreURL:     logStore.url,
		ListenAddr:      listener.Addr().String(),
	}
	testAuthorizationConfig(cfg)
	models := copyOfProductionModelStore(t, dir)
	model := liveModelForHarness(t, models, h)
	t.Logf("%s sessions run on %s", harness, model)
	srv := New(st, nil, nil, hs, nil, models, nil, cfg)

	ts := httptest.NewUnstartedServer(asInternalService(srv))
	ts.Listener.Close()
	ts.Listener = listener
	ts.Start()
	t.Cleanup(ts.Close)

	env := &liveBridge{server: srv, store: st, instanceID: inst.ID, baseURL: ts.URL, logStore: logStore, model: model}
	// Kill whatever process the test left running, before the server closes.
	t.Cleanup(func() {
		for _, sess := range env.listSessionIDs() {
			_ = srv.harness.Kill(sess)
		}
	})
	return env
}

// copyOfProductionModelStore copies the model registry the production server
// reads into dir and opens the copy. VACUUM INTO through a read-only handle
// takes a consistent copy, WAL included, and cannot write to the source.
func copyOfProductionModelStore(t *testing.T, dir string) *modelstore.Store {
	t.Helper()
	source := productiondefaults.ModelStoreDatabasePath()
	db, err := sql.Open("sqlite", "file:"+source+"?mode=ro")
	if err != nil {
		t.Fatalf("open %s read-only: %v", source, err)
	}
	defer db.Close()
	copyPath := filepath.Join(dir, "models.db")
	if _, err := db.Exec("VACUUM INTO ?", copyPath); err != nil {
		t.Fatalf("copy %s: %v", source, err)
	}
	models, err := modelstore.Open(copyPath)
	if err != nil {
		t.Fatalf("open model-store copy: %v", err)
	}
	t.Cleanup(func() { models.Close() })
	return models
}

// liveModelForHarness picks the cheapest kind of model the registry offers
// that this CLI can run: the first model of the efficient role whose provider
// is the harness's.
func liveModelForHarness(t *testing.T, models *modelstore.Store, h liveHarness) string {
	t.Helper()
	efficient, err := models.RoleModels(modelstore.RoleEfficient)
	if err != nil {
		t.Fatalf("read efficient role: %v", err)
	}
	for _, m := range efficient {
		if m.Provider == h.modelProvider {
			return m.ID
		}
	}
	t.Fatalf("production's efficient role has no %s model for %s", h.modelProvider, h.harness)
	return ""
}

func (e *liveBridge) listSessionIDs() []string {
	sessions, err := e.store.ListSessions()
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.SessionID)
	}
	return ids
}

func (e *liveBridge) postJSON(t *testing.T, path string, body any) (*http.Response, []byte) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	resp, err := http.Post(e.baseURL+path, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp, respBody
}

func (e *liveBridge) createEventsSession(t *testing.T) string {
	t.Helper()
	resp, body := e.postJSON(t, "/sessions", msg.CreateSessionRequest{
		Type:          msg.SessionTypeInteractive,
		Purpose:       msg.PurposeChat,
		Origin:        "test",
		Harness:       e.harness(t),
		InstanceID:    e.instanceID,
		Mode:          msg.SessionModeEvents,
		WorkingDir:    liveWorkingDirectory,
		AutoStart:     true,
		HarnessConfig: json.RawMessage(fmt.Sprintf(`{"model":%q}`, e.model)),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /sessions = %d: %s", resp.StatusCode, body)
	}
	var sess msg.ManagedSession
	if err := json.Unmarshal(body, &sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	return sess.SessionID
}

func (e *liveBridge) harness(t *testing.T) msg.Harness {
	t.Helper()
	inst, err := e.server.harnessStore.GetInstance(e.instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	return inst.HarnessType
}

// sendAndWaitForTurnEnd sends one message in events mode and returns the
// text of the turn's result, read from the session's event stream.
func (e *liveBridge) sendAndWaitForTurnEnd(t *testing.T, bridgeID, text string) string {
	t.Helper()
	stream := e.subscribeSessionEvents(t, bridgeID)
	defer stream.close()
	resp, body := e.postJSON(t, "/sessions/"+bridgeID+"/send", msg.SendMessageRequest{Message: text})
	if resp.StatusCode/100 != 2 {
		t.Fatalf("POST /sessions/%s/send = %d: %s", bridgeID, resp.StatusCode, body)
	}
	// A new subscription replays the current turn, which can hold an earlier
	// result, so the answer is the first result after this message.
	_, sentAt, sawSent := stream.waitFrom(-1, 60*time.Second, func(ev msg.Event) bool {
		return ev.Type == msg.EventUserMessage && strings.Contains(eventText(ev), text)
	})
	if !sawSent {
		t.Fatalf("the sent message %q never appeared on the session's event stream (%s)", text, stream.summary())
	}
	result, _, ok := stream.waitFrom(sentAt, 180*time.Second, func(ev msg.Event) bool { return ev.Type == msg.EventResult })
	if !ok {
		t.Fatalf("no result event within 180s for %q (%s); session state %q", text, stream.summary(), e.getSession(t, bridgeID).State)
	}
	if !e.waitForSettledState(t, bridgeID, 30*time.Second) {
		t.Fatalf("session did not settle after the result; %s", e.describeState(t, bridgeID))
	}
	return eventText(result)
}

func (e *liveBridge) waitForHarnessSessionID(t *testing.T, bridgeID string) string {
	t.Helper()
	end := time.Now().Add(30 * time.Second)
	for time.Now().Before(end) {
		if sess := e.getSession(t, bridgeID); sess.HarnessSessionID != "" {
			return sess.HarnessSessionID
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("session %s never recorded a harness_session_id", bridgeID)
	return ""
}

type switchModeResponse struct {
	Session     msg.ManagedSession
	AttachToken string
}

func (e *liveBridge) switchMode(t *testing.T, bridgeID string, mode msg.SessionMode) switchModeResponse {
	t.Helper()
	resp, body := e.postJSON(t, "/sessions/"+bridgeID+"/mode", switchModeRequest{Mode: mode})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /sessions/%s/mode %s = %d: %s (state %q)", bridgeID, mode, resp.StatusCode, bytes.TrimSpace(body), e.getSession(t, bridgeID).State)
	}
	// A pty switch answers the session with attach_token beside its fields
	// (createSessionResponse embeds the session); an events switch answers
	// the bare session.
	var flat struct {
		msg.ManagedSession
		AttachToken string `json:"attach_token"`
	}
	if err := json.Unmarshal(body, &flat); err != nil {
		t.Fatalf("decode mode switch response: %v: %s", err, body)
	}
	return switchModeResponse{Session: flat.ManagedSession, AttachToken: flat.AttachToken}
}

func (e *liveBridge) stopSession(t *testing.T, bridgeID string) {
	t.Helper()
	resp, body := e.postJSON(t, "/sessions/"+bridgeID+"/stop", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /sessions/%s/stop = %d: %s", bridgeID, resp.StatusCode, body)
	}
}

// getSession reads the row over HTTP, the way the dash does.
func (e *liveBridge) getSession(t *testing.T, bridgeID string) msg.ManagedSession {
	t.Helper()
	resp, err := http.Get(e.baseURL + "/sessions/" + bridgeID)
	if err != nil {
		t.Fatalf("GET /sessions/%s: %v", bridgeID, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sessions/%s = %d: %s", bridgeID, resp.StatusCode, body)
	}
	var sess msg.ManagedSession
	if err := json.Unmarshal(body, &sess); err != nil {
		t.Fatalf("decode session: %v: %s", err, body)
	}
	return sess
}

func (e *liveBridge) expectSessionRow(t *testing.T, bridgeID string, wantMode msg.SessionMode, wantHarnessSessionID string) {
	t.Helper()
	sess := e.getSession(t, bridgeID)
	if sess.Mode != wantMode {
		t.Errorf("GET /sessions/%s mode = %q, want %q", bridgeID, sess.Mode, wantMode)
	}
	if sess.HarnessSessionID != wantHarnessSessionID {
		t.Errorf("GET /sessions/%s harness_session_id = %q, want %q (the switch lost the conversation's id)", bridgeID, sess.HarnessSessionID, wantHarnessSessionID)
	}
}

// describeState names the session's state and the reason recorded with it.
func (e *liveBridge) describeState(t *testing.T, bridgeID string) string {
	t.Helper()
	sess := e.getSession(t, bridgeID)
	if sess.Status == nil {
		return fmt.Sprintf("state %q, no status recorded", sess.State)
	}
	return fmt.Sprintf("state %q, reason %q %s", sess.State, sess.Status.Reason, sess.Status.ReasonDetail)
}

func (e *liveBridge) waitForSettledState(t *testing.T, bridgeID string, timeout time.Duration) bool {
	t.Helper()
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		switch msg.SessionState(e.getSession(t, bridgeID).State) {
		case msg.SessionIdle, msg.SessionAwaitingUser:
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────────
// the dash's view: per-session event stream and log-store
// ──────────────────────────────────────────────────────────────────────────────

// sessionEventStream reads GET /sessions/{id}/events, the stream the dash's
// chat draws from, and keeps every event in arrival order.
type sessionEventStream struct {
	mu         sync.Mutex
	events     []msg.Event
	reconnects int
	arrived    chan struct{}
	cancel     context.CancelFunc
}

func (e *liveBridge) subscribeSessionEvents(t *testing.T, bridgeID string) *sessionEventStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &sessionEventStream{arrived: make(chan struct{}, 1), cancel: cancel}
	t.Cleanup(s.close)
	first, err := s.connect(ctx, e.baseURL, bridgeID, "")
	if err != nil {
		cancel()
		t.Fatalf("GET /sessions/%s/events: %v", bridgeID, err)
	}
	go s.follow(ctx, e.baseURL, bridgeID, first)
	return s
}

// follow reads the stream and reconnects when the server ends it, sending the
// last row id it saw as Last-Event-ID, a second later, the way the chat's
// SyncEngine does. The server ends every stream of a session when that
// session's process exits, which a mode switch always does, so a reader that
// does not reconnect stops seeing the session at the switch.
func (s *sessionEventStream) follow(ctx context.Context, baseURL, bridgeID string, body io.ReadCloser) {
	lastEventID := ""
	for {
		lastEventID = s.read(body, lastEventID)
		body.Close()
		s.mu.Lock()
		s.reconnects++
		s.mu.Unlock()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			next, err := s.connect(ctx, baseURL, bridgeID, lastEventID)
			if err == nil {
				body = next
				break
			}
		}
	}
}

func (s *sessionEventStream) connect(ctx context.Context, baseURL, bridgeID, lastEventID string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/sessions/"+bridgeID+"/events", nil)
	if err != nil {
		return nil, err
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// read consumes one connection and returns the last "id:" it carried.
func (s *sessionEventStream) read(body io.Reader, lastEventID string) string {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if id, ok := strings.CutPrefix(line, "id: "); ok {
			lastEventID = id
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev msg.Event
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		s.mu.Lock()
		s.events = append(s.events, ev)
		s.mu.Unlock()
		select {
		case s.arrived <- struct{}{}:
		default:
		}
	}
	return lastEventID
}

func (s *sessionEventStream) close() { s.cancel() }

func (s *sessionEventStream) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// summary lists the event types seen, for failure messages.
func (s *sessionEventStream) summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[msg.EventType]int{}
	var order []msg.EventType
	for _, ev := range s.events {
		if counts[ev.Type] == 0 {
			order = append(order, ev.Type)
		}
		counts[ev.Type]++
	}
	parts := make([]string, 0, len(order))
	for _, typ := range order {
		parts = append(parts, fmt.Sprintf("%s×%d", typ, counts[typ]))
	}
	return fmt.Sprintf("[%s] after %d reconnects", strings.Join(parts, " "), s.reconnects)
}

// dump lists every event that carries text, for failure messages.
func (s *sessionEventStream) dump() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for i, ev := range s.events {
		text := eventText(ev)
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "  #%d %s %s: %q\n", i, ev.Timestamp.Format("15:04:05.000"), ev.Type, truncateString(text, 120))
	}
	return b.String()
}

// streamPosition is the index of an event in the stream; -1 means "before
// anything".
type streamPosition int

func (s *sessionEventStream) waitFor(timeout time.Duration, match func(msg.Event) bool) (msg.Event, bool) {
	ev, _, ok := s.waitFrom(-1, timeout, match)
	return ev, ok
}

func (s *sessionEventStream) waitFrom(after streamPosition, timeout time.Duration, match func(msg.Event) bool) (msg.Event, streamPosition, bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		for i := int(after) + 1; i < len(s.events); i++ {
			if match(s.events[i]) {
				ev := s.events[i]
				s.mu.Unlock()
				return ev, streamPosition(i), true
			}
		}
		s.mu.Unlock()
		select {
		case <-s.arrived:
		case <-time.After(250 * time.Millisecond):
		case <-deadline.C:
			return msg.Event{}, -1, false
		}
	}
}

// eventText is the human-readable text an event carries, whichever field
// its type puts it in.
func eventText(ev msg.Event) string {
	var parts []string
	if ev.Result != nil && ev.Result.Text != "" {
		parts = append(parts, ev.Result.Text)
	}
	if ev.Block != nil && ev.Block.Block != nil && ev.Block.Block.Text != nil {
		parts = append(parts, ev.Block.Block.Text.Text)
	}
	return strings.Join(parts, "\n")
}

func isAssistantText(ev msg.Event) bool {
	return ev.Type == msg.EventResult || ev.Type == msg.EventBlock
}

// recordingLogStore stands in for log-store: it accepts every write and keeps
// the raw request bodies so a test can ask whether a piece of text reached it.
type recordingLogStore struct {
	url   string
	mu    sync.Mutex
	posts [][]byte
}

func newRecordingLogStore(t *testing.T) *recordingLogStore {
	t.Helper()
	r := &recordingLogStore{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			body, _ := io.ReadAll(req.Body)
			r.mu.Lock()
			r.posts = append(r.posts, body)
			r.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(ts.Close)
	r.url = ts.URL
	return r
}

func (r *recordingLogStore) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.posts)
}

func (r *recordingLogStore) containsSince(index int, text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	needle, _ := json.Marshal(text) // match the text as JSON encodes it
	needle = bytes.Trim(needle, `"`)
	for _, body := range r.posts[index:] {
		if bytes.Contains(body, needle) {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────────
// the pty screen
// ──────────────────────────────────────────────────────────────────────────────

// ptyScreen reads the attach socket in the background and keeps every byte the
// CLI painted, so a test can type into it and look for text on it.
type ptyScreen struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	output []byte
	exited bool
}

func newPTYScreen(conn *websocket.Conn) *ptyScreen {
	p := &ptyScreen{conn: conn}
	go func() {
		for {
			mt, payload, err := conn.ReadMessage()
			if err != nil {
				p.mu.Lock()
				p.exited = true
				p.mu.Unlock()
				return
			}
			p.mu.Lock()
			switch mt {
			case websocket.BinaryMessage:
				p.output = append(p.output, payload...)
			case websocket.TextMessage:
				if bytes.Contains(payload, []byte(`"exit"`)) {
					p.exited = true
				}
			}
			p.mu.Unlock()
		}
	}()
	return p
}

// waitForTUI waits for the CLI to paint a screen, then for it to stop
// repainting for two seconds, which is when it takes keystrokes.
func (p *ptyScreen) waitForTUI(t *testing.T, timeout time.Duration) {
	t.Helper()
	end := time.Now().Add(timeout)
	lastLength, quietSince := -1, time.Now()
	for time.Now().Before(end) {
		p.mu.Lock()
		length, exited := len(p.output), p.exited
		painted := length >= 64 && bytes.Contains(p.output, []byte{0x1b, '['})
		p.mu.Unlock()
		if exited {
			t.Fatalf("pty exited before the CLI was ready; screen: %q", p.text(400))
		}
		if length != lastLength {
			lastLength, quietSince = length, time.Now()
		}
		if painted && time.Since(quietSince) >= 2*time.Second {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("CLI screen not ready within %s; screen: %q", timeout, p.text(400))
}

func (p *ptyScreen) refuseScreensThatTakeKeystrokes(t *testing.T, screens []string) {
	t.Helper()
	p.mu.Lock()
	plain := string(stripANSI(p.output))
	p.mu.Unlock()
	for _, text := range screens {
		if strings.Contains(plain, text) {
			t.Fatalf("the CLI opened on a screen showing %q, which would take the typed line as its answer; deal with that screen by hand and run again. Screen: %q", text, p.text(600))
		}
	}
}

// typeLine types text, then presses Enter as a separate keystroke. Both CLIs
// read a fast burst of bytes as a paste, and an Enter inside a paste is a
// newline in the prompt rather than a submit.
func (p *ptyScreen) typeLine(t *testing.T, text string) {
	t.Helper()
	if err := p.conn.WriteMessage(websocket.BinaryMessage, []byte(text)); err != nil {
		t.Fatalf("type into pty: %v", err)
	}
	time.Sleep(700 * time.Millisecond)
	if err := p.conn.WriteMessage(websocket.BinaryMessage, []byte("\r")); err != nil {
		t.Fatalf("press enter in pty: %v", err)
	}
}

func (p *ptyScreen) waitForText(timeout time.Duration, text string) bool {
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		p.mu.Lock()
		found := bytes.Contains(stripANSI(p.output), []byte(text))
		p.mu.Unlock()
		if found {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func (p *ptyScreen) text(n int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	plain := stripANSI(p.output)
	if len(plain) > n {
		plain = plain[len(plain)-n:]
	}
	return string(plain)
}

// stripANSI drops escape sequences so painted text can be searched. TUIs
// often move the cursor between words, so this removes CSI and OSC sequences
// and keeps the printable bytes between them.
func stripANSI(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] != 0x1b || i+1 >= len(b) {
			out = append(out, b[i])
			continue
		}
		switch b[i+1] {
		case '[':
			i += 2
			for i < len(b) && (b[i] < 0x40 || b[i] > 0x7e) {
				i++
			}
		case ']':
			i += 2
			for i < len(b) && b[i] != 0x07 && !(b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\') {
				i++
			}
			if i < len(b) && b[i] == 0x1b {
				i++
			}
		default:
			i++
		}
	}
	return out
}

// ──────────────────────────────────────────────────────────────────────────────
// the CLI outside the bridge
// ──────────────────────────────────────────────────────────────────────────────

// externalResumeArgs is the headless form of what a person types to pick a
// conversation back up in their terminal. The headless form writes the same
// session files the interactive one does, which is all the bridge could ever
// read, and a test can drive it without a screen.
func externalResumeArgs(h liveHarness, harnessSessionID, prompt string) []string {
	switch h.harness {
	case msg.HarnessClaudeCode:
		return []string{"-p", "--resume", harnessSessionID, prompt}
	case msg.HarnessCodex:
		return []string{"exec", "--skip-git-repo-check", "resume", harnessSessionID, prompt}
	}
	panic("no resume command for harness " + string(h.harness))
}

func runExternalCLI(t *testing.T, h liveHarness, args []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.cliBin, args...)
	cmd.Dir = liveWorkingDirectory
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", h.cliBin, strings.Join(args, " "), err, truncateString(string(out), 1200))
	}
	return string(out)
}

// runExternalNewSession starts a fresh conversation outside the bridge and
// returns the id the CLI gave it.
func runExternalNewSession(t *testing.T, h liveHarness, prompt string) string {
	t.Helper()
	switch h.harness {
	case msg.HarnessClaudeCode:
		out := runExternalCLI(t, h, []string{"-p", "--output-format", "json", prompt})
		var result struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(lastJSONLine(out)), &result); err != nil || result.SessionID == "" {
			t.Fatalf("no session_id in claude output: %v: %s", err, truncateString(out, 600))
		}
		return result.SessionID
	case msg.HarnessCodex:
		out := runExternalCLI(t, h, []string{"exec", "--skip-git-repo-check", "--json", prompt})
		for _, line := range strings.Split(out, "\n") {
			var ev struct {
				Type     string `json:"type"`
				ThreadID string `json:"thread_id"`
			}
			if json.Unmarshal([]byte(line), &ev) == nil && ev.ThreadID != "" {
				return ev.ThreadID
			}
		}
		t.Fatalf("no thread_id in codex output: %s", truncateString(out, 600))
	}
	panic("no new-session command for harness " + string(h.harness))
}

func lastJSONLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "{") {
			return lines[i]
		}
	}
	return ""
}

func liveCodeWord() string {
	return fmt.Sprintf("ZEBRA%d", time.Now().UnixNano()%1000000)
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
