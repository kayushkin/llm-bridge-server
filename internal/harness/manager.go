package harness

import (
	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	logstore "github.com/kayushkin/log-store/client"
	"github.com/kayushkin/llm-bridge-server/internal/ids"
	"github.com/kayushkin/llm-bridge-server/internal/productiondefaults"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// sessionMsgState tracks per-session message-id assignment for the manager.
// The manager owns the single-threaded event channel for a session, so this
// is the right place to mint canonical bridge MessageIDs and reconcile them
// against harness-native ids on resume/replay.
type sessionMsgState struct {
	bridgeMsgID      string                            // currently-open assistant bridge id, "" between turns
	harnessMsgID     string                            // last-seen harness id for the open bridge message
	harnessToBridge  map[string]string                 // harness id → bridge id, for resume reconciliation
	toolUseToMessage map[string]store.ToolUseBinding   // tool_use_id → bubble ids, for task_progress correlation
	clientRequestID  string                            // caller's per-turn id from the latest user_message, "" between turns
	turnID           string                            // bridge-minted per-turn id, "" between turns
	turnPromptText   string                            // text of the prompt that opened the current turn, "" between turns; used to recognise Claude Code's OTel echo of that prompt
}

// StoredEvent pairs an event with its database row ID, assigned at insert time.
type StoredEvent struct {
	msg.Event
	RowID int64
}

// Manager handles harness subprocess lifecycle.
type Manager struct {
	mu              sync.RWMutex
	processes       map[string]HarnessProcess     // sessionID → process
	subscribers     map[string][]chan StoredEvent // sessionID → SSE subscriber channels
	msgState        map[string]*sessionMsgState   // sessionID → message-id assignment state
	attachHubs      map[string]*AttachHub         // sessionID → fan-out hub for pty sessions
	derivation      map[string]*derivationState   // sessionID → convenience-event derivation state
	budgetHalted    map[string]bool               // sessionID → already announced this session's spend-ceiling breach (see budget.go)
	otelSidecars    map[string]*otelSidecar       // sessionID → per-PTY OTel sidecar (nil for non-PTY sessions)
	pending         *pendingHooks                 // awaiting_resolution hooks indexed by sessionID, request_id
	store           *store.Store
	logStore        *logstore.Client
	logStoreWrites  *logStoreQueue  // ordered per-session writer; keeps the log-store POST off the SSE fan-out path
	runners         *RunnerRegistry // optional; nil disables TransportRunner spawns
	authClient      *authstoreclient.Client
	publicServerURL string          // public bridge URL runners use for /api/runner/binary fetches
	localBridgeURL  string          // localhost URL the per-session OTel sidecar POSTs translated events to; derived from ListenAddr at startup
	ptyRingBytes    int             // configured ring buffer size for pty late-attach replay
	turnEnd         TurnEndObserver // optional; notified after each turn-end event is derived and fanned out

	// folderResolver maps a session purpose to its sidebar folder, using the
	// same env-defaults-plus-DB-overrides mapping the HTTP layer uses. Owned by
	// the server (the manager has no config of its own) and installed at
	// startup; nil leaves promoted sessions unfiled rather than guessing a
	// folder name the user never configured.
	folderResolver func(purpose string) string
}

// SetFolderResolver installs the purpose→sidebar-folder mapping. The manager
// needs it to file sessions it promotes out of a harness process (subagents),
// which are created below the HTTP layer that normally owns folder placement.
func (m *Manager) SetFolderResolver(resolve func(purpose string) string) {
	m.folderResolver = resolve
}

// folderForPurpose resolves the sidebar folder for a session purpose, or "" if
// no resolver is installed or the purpose is unmapped.
func (m *Manager) folderForPurpose(purpose string) string {
	if m.folderResolver == nil {
		return ""
	}
	return m.folderResolver(purpose)
}

// TurnEndObserver is called once per turn-end, after the terminating event
// and everything derived from it have been broadcast. state is the session
// state derivation settled on for that turn.
//
// Observers run on their own goroutine and must not assume the session is
// still in state by the time they act — a new turn can open underneath them.
// They are advisory: the derivation state machine stays authoritative, and
// an observer that wants to change state goes through
// Manager.ApplyDerivedSessionState, which bounds the write.
type TurnEndObserver func(bridgeID string, ev *msg.Event, state msg.SessionState)

// SetTurnEndObserver registers the turn-end observer. Passing nil clears it.
// Called once at wiring time, before any session starts.
func (m *Manager) SetTurnEndObserver(fn TurnEndObserver) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turnEnd = fn
}

// ApplyDerivedSessionState writes a session state decided outside the raw
// event stream — today only the turn-end signal classifier, whose verdict
// arrives after a network round trip. It reports whether the state actually
// changed.
//
// allowedFrom names the states the caller's decision was formed about; the
// write is dropped when the session has since moved to anything else, so a
// late verdict can never overwrite a fresher truth. Passing no allowedFrom
// drops the write rather than treating it as unbounded.
func (m *Manager) ApplyDerivedSessionState(bridgeID string, next msg.SessionState, reason string, allowedFrom ...msg.SessionState) bool {
	m.mu.Lock()
	d := m.derivation[bridgeID]
	m.mu.Unlock()
	if d == nil {
		return false
	}
	ev := d.applyExternalState(next, reason, allowedFrom...)
	if ev == nil {
		return false
	}
	m.publishDerived(bridgeID, []msg.Event{*ev})
	return true
}

// NewManager creates a harness manager.
//
// publicServerURL is the externally-reachable URL runners use to fetch
// backend binaries (manifest BinaryURL) — same URL the runner is already
// connected to via WS. May be empty when no remote runners are in play.
//
// localBridgeURL is the localhost-reachable URL bridge-server listens on,
// used to point the per-PTY OTel sidecar at the /sidecar/event endpoint.
// Distinct from publicServerURL: sidecar IPC is always loopback, and
// publicServerURL may be a HTTPS hostname behind a reverse proxy that
// the sidecar can't reach. Empty disables the PTY OTel sidecar.
//
// ptyRingBytes is the per-session ring buffer size used for late-attach
// replay on pty sessions. <=0 falls back to the package default.
func NewManager(st *store.Store, logStoreURL, publicServerURL, localBridgeURL string, ptyRingBytes int, authClient *authstoreclient.Client) *Manager {
	// This is the only place in the repo a log-store client is built, so it
	// is the last point at which a test can be stopped before it writes to
	// the live event log. config.Load makes the same check, but a test that
	// builds a config.Config literal by hand never goes through Load.
	productiondefaults.PanicIfUsedUnderTest(map[string]string{"LogStoreURL": logStoreURL})

	ls := logstore.New(logStoreURL)
	m := &Manager{
		processes:       make(map[string]HarnessProcess),
		subscribers:     make(map[string][]chan StoredEvent),
		msgState:        make(map[string]*sessionMsgState),
		attachHubs:      make(map[string]*AttachHub),
		derivation:      make(map[string]*derivationState),
		budgetHalted:    make(map[string]bool),
		otelSidecars:    make(map[string]*otelSidecar),
		pending:         newPendingHooks(),
		store:           st,
		logStore:        ls,
		runners:         NewRunnerRegistry(),
		authClient:      authClient,
		publicServerURL: publicServerURL,
		localBridgeURL:  localBridgeURL,
		ptyRingBytes:    ptyRingBytes,
	}
	m.logStoreWrites = newLogStoreQueue(ls.PushEvent)
	return m
}

// PendingHooks returns the awaiting_resolution HookEvents currently
// outstanding for the session. Returns nil for sessions with none.
// Used by the /sessions/:id/hooks/pending HTTP endpoint so the UI can
// recover the banner state on a fresh connection.
func (m *Manager) PendingHooks(sessionID string) []msg.Event {
	return m.pending.list(sessionID)
}

// AttachHubFor returns the per-session attach hub for a pty session, or
// nil if no pty hub is registered (events-mode session, or pty session
// whose process has already exited). Hubs are created in lockstep with
// pty processes by StartOnInstance and torn down by watchPTYExit.
func (m *Manager) AttachHubFor(sessionID string) *AttachHub {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.attachHubs[sessionID]
}

// Runners returns the runner registry so the HTTP layer can mount the
// /api/runner/ws endpoint against it.
func (m *Manager) Runners() *RunnerRegistry {
	return m.runners
}

// loadMsgState rehydrates message-id state for a session from the DB. Called
// when a process starts so that resume-replays from the harness can be mapped
// back to their original bridge MessageIDs. Caller must hold m.mu.
func (m *Manager) loadMsgState(bridgeID string) *sessionMsgState {
	st := &sessionMsgState{
		harnessToBridge:  make(map[string]string),
		toolUseToMessage: make(map[string]store.ToolUseBinding),
	}
	if mp, err := m.store.HarnessToBridgeMap(bridgeID); err == nil {
		st.harnessToBridge = mp
	}
	if mp, err := m.store.ToolUseToMessageMap(bridgeID); err == nil {
		st.toolUseToMessage = mp
	}
	// Recover in-flight turn state so events emitted after a process restart
	// get stamped with the same TurnID/MessageID as events from before the
	// restart, instead of being left unstamped until the next user_message.
	// Reads log-store (Phase II.A cutover); bridge-server's local events
	// table is still dual-written but no longer the source for this query.
	if turn, err := m.RecoverInFlightTurn(bridgeID); err == nil && turn != nil {
		st.turnID = turn.TurnID
		st.clientRequestID = turn.ClientRequestID
		st.bridgeMsgID = turn.BridgeMessageID
		st.harnessMsgID = turn.HarnessMessageID
	} else if err != nil {
		log.Printf("[harness] RecoverInFlightTurn %s: %v", bridgeID, err)
	}
	m.msgState[bridgeID] = st
	return st
}

// AssignMessageID stamps an event with its canonical bridge MessageID,
// extracts and records the harness-native id, stamps a bridge-minted TurnID
// on every event in the turn, and tracks turn boundaries.
//
// Rules:
//   - user_message: mints a fresh MessageID for the user bubble and opens a new
//     turn (fresh TurnID, closing any open assistant bubble) — UNLESS it is
//     Claude Code's OTel echo of the prompt that opened the currently-open turn
//     (source=otel + matching text), in which case it attaches to that turn
//     instead of minting a second one (Bug 3). The echo is never dropped: in
//     PTY mode the OTel copy is the only record of the user's input.
//   - assistant-side events (stream/thinking/tool_call/tool_result/plan/approval/result):
//     reuse an existing bridge MessageID when the harness id has been seen
//     before (resume case); split when the harness id changes mid-turn;
//     otherwise mint a new MessageID on the first event of a bubble.
//   - result/error: stamp with the in-flight MessageID, then close the turn
//     (clear MessageID, TurnID, ClientRequestID, turnPromptText state).
//   - system events: no MessageID. TurnID is stamped when one is in-flight.
//   - everything else (session_info, harness_id_set): no MessageID.
//
// TurnID is stamped on every event while a turn is open, including system
// events that don't belong to a message bubble — that's the coarser
// correlator for init/task_progress/retry alongside the bubble(s) they
// accompany.
func (m *Manager) AssignMessageID(bridgeID string, ev *msg.Event) {
	hid := msg.HarnessMessageIDOf(ev)
	ev.HarnessMessageID = hid

	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.msgState[bridgeID]
	if st == nil {
		st = m.loadMsgState(bridgeID)
	}

	switch ev.Type {
	case msg.EventUserMessage:
		if ev.MessageID == "" {
			ev.MessageID = ids.NewMessageID()
		}
		// Bug-3 guard: Claude Code reports every prompt twice — once via the
		// harness/rollout stream and again, ~1s later, via its OTel exporter
		// tagged extensions.source=="otel". That echo is the SAME logical
		// prompt, not a new turn, so it must attach to the currently-open
		// turn instead of minting a second turn_id (which orphaned the real
		// prompt's turn and split one prompt into two rendered turns). We do
		// NOT drop the echo: in PTY mode keystrokes never pass through /send,
		// so the OTel copy is the only record of what the user typed. Match
		// on source=otel + text equal to the prompt that opened the open
		// turn. See docs/findings/2026-07-27-interrupt-dual-emit-turn-hijack.md §3.
		if st.turnID != "" && isOTelUserEcho(ev, st.turnPromptText) {
			// Attach to the open turn: leave turn/bubble state untouched so
			// the open assistant bubble isn't closed. ev.TurnID is stamped
			// from st.turnID at the tail of this method.
			break
		}
		st.bridgeMsgID = ""
		st.harnessMsgID = ""
		// Latch the caller's per-turn id so we can stamp it on downstream
		// events coming back from the harness.
		st.clientRequestID = ev.ClientRequestID
		// Open a new turn and remember its originating prompt so the OTel
		// echo of it can be recognised above.
		st.turnID = ids.NewTurnID()
		st.turnPromptText = userMessageText(ev)

	case msg.EventStream, msg.EventBlock, msg.EventThinking, msg.EventToolCall,
		msg.EventToolResult, msg.EventPlan, msg.EventApproval:
		ev.MessageID = m.assignAssistantID(st, hid, ev.MessageID)
		if ev.ClientRequestID == "" {
			ev.ClientRequestID = st.clientRequestID
		}
		// Record tool_use_id → bubble so later task_progress events (which
		// carry tool_use_id but no harness message id of their own) can be
		// resolved back to the right message bubble.
		if ev.ToolCall != nil && ev.ToolCall.ToolID != "" {
			st.toolUseToMessage[ev.ToolCall.ToolID] = store.ToolUseBinding{
				BridgeMessageID:  ev.MessageID,
				HarnessMessageID: ev.HarnessMessageID,
			}
		}
		if ev.ToolResult != nil && ev.ToolResult.ToolID != "" {
			if _, exists := st.toolUseToMessage[ev.ToolResult.ToolID]; !exists {
				st.toolUseToMessage[ev.ToolResult.ToolID] = store.ToolUseBinding{
					BridgeMessageID:  ev.MessageID,
					HarnessMessageID: ev.HarnessMessageID,
				}
			}
		}

	case msg.EventResult, msg.EventError:
		ev.MessageID = m.assignAssistantID(st, hid, ev.MessageID)
		if ev.ClientRequestID == "" {
			ev.ClientRequestID = st.clientRequestID
		}
		// Stamp TurnID before clearing state.
		if ev.TurnID == "" {
			ev.TurnID = st.turnID
		}
		st.bridgeMsgID = ""
		st.harnessMsgID = ""
		st.clientRequestID = ""
		st.turnID = ""
		st.turnPromptText = ""
		return

	case msg.EventSystem:
		// task_progress (and any future system event that carries a
		// tool_use_id correlator) gets resolved back to the bubble it
		// narrates. System events don't get their own MessageID minted,
		// but inherited ones make them show up alongside the bubble.
		if ev.System != nil && ev.System.ToolUseID != "" {
			if bind, ok := st.toolUseToMessage[ev.System.ToolUseID]; ok {
				if ev.MessageID == "" {
					ev.MessageID = bind.BridgeMessageID
				}
				if ev.HarnessMessageID == "" {
					ev.HarnessMessageID = bind.HarnessMessageID
				}
			}
		}

	default:
		// session_info / harness_id_set / unknown: no MessageID.
	}

	if ev.TurnID == "" {
		ev.TurnID = st.turnID
	}
}

// isOTelUserEcho reports whether ev is Claude Code's OTel echo of the prompt
// that opened the currently-open turn. Claude Code reports every prompt twice
// — once via the harness/rollout stream and again, ~1s later, via its OTel
// exporter tagged extensions.source=="otel". The echo carries the same prompt
// text, so it must attach to the open turn rather than mint a second turn_id.
// openPrompt is the text of the prompt that opened the current turn ("" when no
// turn is open); an empty openPrompt never matches, so a content-less state can
// never cause a false attach. Matching only against the OPEN turn's prompt is
// deliberate: an echo arriving after the turn has already closed opens its own
// (degenerate) turn, which the render edge dedupes by source — see the finding.
func isOTelUserEcho(ev *msg.Event, openPrompt string) bool {
	if openPrompt == "" || !isOTelSourced(ev) {
		return false
	}
	return userMessageText(ev) == openPrompt
}

// isOTelSourced reports whether ev carries extensions.source=="otel", the tag
// the claudecode harness/sidecar stamps on OTel-derived events so consumers can
// dedupe them against the same logical signal arriving via the stream-json path.
func isOTelSourced(ev *msg.Event) bool {
	return ev != nil && string(ev.Extensions["source"]) == `"otel"`
}

// userMessageText returns the prompt text carried by a user_message event, or
// "" when absent. Both the /send path and the OTel translator put the prompt in
// Result.Text.
func userMessageText(ev *msg.Event) string {
	if ev == nil || ev.Result == nil {
		return ""
	}
	return ev.Result.Text
}

// assignAssistantID picks the bridge MessageID for an assistant-side event.
// Caller must hold m.mu.
//
// preStamped is the value already on ev.MessageID before assignment. When
// non-empty, the adapter has already done the harness_id → bridge_id
// mapping itself (Phase III.B); we honor its choice and update internal
// state to match so split-detection and resume-reconciliation stay
// consistent for any downstream events the adapter doesn't pre-stamp.
// When empty, fall back to the legacy assign-here behavior.
func (m *Manager) assignAssistantID(st *sessionMsgState, hid, preStamped string) string {
	if preStamped != "" {
		st.bridgeMsgID = preStamped
		if hid != "" {
			st.harnessToBridge[hid] = preStamped
			st.harnessMsgID = hid
		}
		return preStamped
	}

	// Resume reconciliation: if we've seen this harness id before in this
	// session, reuse the bridge id we minted then. Re-emitted events thus
	// land back in their original bubble.
	if hid != "" {
		if existing, ok := st.harnessToBridge[hid]; ok {
			st.bridgeMsgID = existing
			st.harnessMsgID = hid
			return existing
		}
	}

	// Split detection: harness moved to a new message inside the same turn.
	if hid != "" && st.harnessMsgID != "" && st.harnessMsgID != hid {
		st.bridgeMsgID = ""
	}

	if st.bridgeMsgID == "" {
		st.bridgeMsgID = ids.NewMessageID()
	}
	if hid != "" {
		st.harnessToBridge[hid] = st.bridgeMsgID
		st.harnessMsgID = hid
	}
	return st.bridgeMsgID
}

// Available checks if a harness binary is in PATH.
func Available(h msg.Harness) (string, bool) {
	bin := msg.HarnessBinaryName(h)
	if bin == "" {
		return "", false
	}
	path, err := exec.LookPath(bin)
	return path, err == nil
}

// Start spawns a new harness session (local, no credential binding).
func (m *Manager) Start(ctx context.Context, sess *store.Session) (*Process, error) {
	h := msg.Harness(sess.Harness)
	binPath, ok := Available(h)
	if !ok {
		return nil, fmt.Errorf("harness binary not found: %s", msg.HarnessBinaryName(h))
	}

	proc, err := StartProcess(ctx, binPath, sess, "", "")
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.processes[sess.SessionID] = proc
	m.mu.Unlock()

	// Update session with PID. `starting` — the subprocess exists and has not
	// emitted its first event yet, which is exactly what the enum says that
	// moment is. The derivation moves it on from here.
	m.store.UpdateSessionPID(sess.SessionID, proc.PID())
	m.store.UpdateSessionState(sess.SessionID, string(msg.SessionStarting))

	// Start event reader goroutine
	go m.readEvents(proc)

	return proc, nil
}

// Get returns a running process by session ID.
func (m *Manager) Get(sessionID string) HarnessProcess {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.processes[sessionID]
}

// Stop sends interrupt signal to pause session.
func (m *Manager) Stop(sessionID string) error {
	proc := m.Get(sessionID)
	if proc == nil {
		return fmt.Errorf("session not running: %s", sessionID)
	}
	return proc.Interrupt()
}

// Kill terminates the session.
func (m *Manager) Kill(sessionID string) error {
	proc := m.Get(sessionID)
	if proc == nil {
		return fmt.Errorf("session not running: %s", sessionID)
	}

	m.mu.Lock()
	delete(m.processes, sessionID)
	m.mu.Unlock()

	return proc.Kill()
}

// Send writes a message to the harness stdin. Pass blocks=nil for text-only
// input; pass message="" with blocks for multimodal input.
func (m *Manager) Send(sessionID string, message string, blocks []msg.ContentBlock) error {
	proc := m.Get(sessionID)
	if proc == nil {
		return fmt.Errorf("session not running: %s", sessionID)
	}
	return proc.Send(message, blocks)
}

// SendCommand sends a command (compact, resume, etc.) to the harness.
func (m *Manager) SendCommand(sessionID string, cmd string) error {
	proc := m.Get(sessionID)
	if proc == nil {
		return fmt.Errorf("session not running: %s", sessionID)
	}
	return proc.SendCommand(cmd)
}

// SendJSONRPC forwards a generic JSON-RPC request to the harness's stdin.
// Used for methods that take parameters and aren't covered by Send /
// SendCommand (e.g. resolve_hook).
func (m *Manager) SendJSONRPC(sessionID, method string, params json.RawMessage) error {
	proc := m.Get(sessionID)
	if proc == nil {
		return fmt.Errorf("session not running: %s", sessionID)
	}
	return proc.SendJSONRPC(method, params)
}

// Subscribe creates a new event channel for SSE consumers.
// The returned channel receives all events for the session.
// Call Unsubscribe when done.
func (m *Manager) Subscribe(sessionID string) chan StoredEvent {
	ch := make(chan StoredEvent, 100)
	m.mu.Lock()
	m.subscribers[sessionID] = append(m.subscribers[sessionID], ch)
	m.mu.Unlock()
	return ch
}

// Unsubscribe removes an SSE subscriber channel.
func (m *Manager) Unsubscribe(sessionID string, ch chan StoredEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	subs := m.subscribers[sessionID]
	for i, s := range subs {
		if s == ch {
			m.subscribers[sessionID] = append(subs[:i], subs[i+1:]...)
			close(ch)
			return
		}
	}
}

// HasProcess returns true if a harness process is running for the session.
func (m *Manager) HasProcess(sessionID string) bool {
	return m.Get(sessionID) != nil
}

// ListActiveSessions returns the bridge_session_id of every harness process
// currently running. Used for fan-out broadcasts (e.g. set_bypass_permissions
// when the global toggle flips). Snapshot semantics — the returned slice is
// a copy and may be stale by the time the caller acts on it.
func (m *Manager) ListActiveSessions() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.processes))
	for id := range m.processes {
		out = append(out, id)
	}
	return out
}

// readEvents reads events from process, persists them, updates state,
// and fans out to all SSE subscribers.
func (m *Manager) readEvents(proc HarnessProcess) {
	bridgeID := proc.SessionID()
	harnessIDSet := false
	subagents := newSubagentRouter(m, bridgeID)

	// The session's log-store write queue lives exactly as long as this
	// pump. Closed in the exit path below, which drains it, so a session
	// whose process has gone is complete in log-store before its
	// subscribers are told the stream ended.
	m.logStoreWrites.Open(bridgeID)

	for event := range proc.Events() {
		// Drop harness-emitted EventSessionState entirely. SessionState
		// is derived centrally from the raw event stream + server-only
		// context (pause/abort calls, permission-store hook state,
		// subprocess lifecycle, provider rate-limit signals). Harnesses
		// don't have that context — letting them write SessionState
		// produces drift between what the harness thinks and what the
		// server knows. Logged once per occurrence so a misbehaving
		// harness is visible without spamming.
		if event.Type == msg.EventSessionState {
			log.Printf("[harness] dropping harness-emitted EventSessionState from %s (state=%q): SessionState is derived centrally", event.Harness, func() string {
				if event.State != nil {
					return string(event.State.State)
				}
				return ""
			}())
			continue
		}

		// A harness may run subagents inside the parent's process and emit their
		// frames on the same stream. routeID is the session the event actually
		// belongs to — the parent for its own frames, a promoted subagent
		// session for a subagent's. It equals bridgeID for every harness that
		// doesn't do this, which is the whole existing fleet.
		subagents.observe(&event)
		routeID := subagents.route(&event)

		// Per the event contract, HarnessSessionID is the harness-native id and
		// must never equal BridgeSessionID. If it does, the harness bridge is
		// emitting bridge_id in the harness slot — surface loudly and discard
		// the value rather than persisting a poisoned harness_session_id.
		harnessID := event.HarnessSessionID
		if harnessID != "" && harnessID == bridgeID {
			log.Printf("[harness] CONTRACT VIOLATION: %s emitted event.HarnessSessionID == BridgeSessionID (%s); discarding", event.Harness, bridgeID)
			harnessID = ""
		}

		// On first event with a harness_session_id, persist it on the session row.
		// bridge_session_id stays stable across the conversation. Only the
		// parent's own frames carry the process's harness id — a subagent frame
		// carries it too (CC stamps every frame with the parent's session_id),
		// but writing it onto the subagent's row would overwrite the row's own
		// dedupe key with the parent's UUID and collide the two sessions.
		if routeID == bridgeID && !harnessIDSet && harnessID != "" {
			if err := m.store.SetHarnessSessionID(bridgeID, harnessID); err != nil {
				log.Printf("[harness] failed to set harness_session_id on %s: %v", bridgeID, err)
			} else {
				harnessIDSet = true

				// Notify SSE subscribers so frontends learn the harness id.
				idEvent := msg.Event{
					Type:             msg.EventSystem,
					BridgeSessionID:  bridgeID,
					HarnessSessionID: harnessID,
					Timestamp:        time.Now(),
					System:           &msg.SystemEvent{Subtype: "harness_id_set", Message: harnessID},
				}
				if data, err := json.Marshal(idEvent); err == nil {
					rowID, _ := m.store.StoreEventReturningID(bridgeID, string(idEvent.Type), "", "", data)
					stored := StoredEvent{Event: idEvent, RowID: rowID}
					m.mu.RLock()
					for _, ch := range m.subscribers[bridgeID] {
						select {
						case ch <- stored:
						default:
						}
					}
					m.mu.RUnlock()
				}
			}
		}

		// Force bridge_session_id to the routed value. Bridges should already be
		// setting it correctly for their own frames; defend against drift.
		event.BridgeSessionID = routeID

		// Assign canonical bridge MessageID and capture harness id. Message
		// numbering is per-session, so a subagent's messages number within its
		// own session rather than punching holes in the parent's sequence.
		m.AssignMessageID(routeID, &event)

		// Persist event keyed by bridge_id (stable PK) and capture row ID.
		var rowID int64
		if data, err := json.Marshal(event); err == nil {
			rowID, err = m.store.StoreEventReturningID(routeID, string(event.Type), event.MessageID, event.HarnessMessageID, data)
			if err != nil {
				log.Printf("[harness] failed to store event: %v", err)
			}
		}

		// Push to log-store (durable source of truth). Queued rather than
		// written here: the POST used to sit in front of the fan-out
		// below, so every token delta the browser was waiting on paid a
		// round trip first. The queue keeps write order per session and
		// blocks rather than drops when it fills; the read-back paths
		// (RecoverInFlightTurn, InterruptedTurn, RecentTurnTexts,
		// ListToolCallInputs) and the synchronous /send push drain it
		// first, so nothing observes a half-written session.
		m.logStoreWrites.Enqueue(bridgeID, "event", event)

		// Update bookkeeping based on event type. SessionState row
		// updates are owned by the derivation path (deriveAndBroadcast
		// → UpdateSessionState) — readEvents no longer flips state on
		// EventResult/EventError directly.
		switch event.Type {
		case msg.EventSessionInfo:
			if event.Info != nil {
				if err := m.store.SetSessionInfo(routeID, event.Info); err != nil {
					log.Printf("[harness] failed to persist session info: %v", err)
				}
			}
		case msg.EventHook:
			// Track awaiting_resolution → completed transitions so a freshly
			// connected client can recover the pending set without a full
			// event-stream replay (see /sessions/:id/hooks/pending).
			m.pending.record(routeID, &event)
		}

		// Fan out to SSE subscribers. Sends are parallel so one slow client
		// can't starve the others, and each has a bounded timeout. On timeout
		// the subscriber is evicted — its SSE stream closes, the client
		// reconnects with Last-Event-ID, and the store replays missed events.
		stored := StoredEvent{Event: event, RowID: rowID}
		m.mu.RLock()
		subs := make([]chan StoredEvent, len(m.subscribers[routeID]))
		copy(subs, m.subscribers[routeID])
		m.mu.RUnlock()

		if len(subs) > 0 {
			const sendTimeout = 5 * time.Second
			var evictMu sync.Mutex
			var evicted []chan StoredEvent
			var wg sync.WaitGroup
			for _, ch := range subs {
				wg.Add(1)
				go func(c chan StoredEvent) {
					defer wg.Done()
					timer := time.NewTimer(sendTimeout)
					defer timer.Stop()
					select {
					case c <- stored:
					case <-timer.C:
						evictMu.Lock()
						evicted = append(evicted, c)
						evictMu.Unlock()
					}
				}(ch)
			}
			wg.Wait()

			if len(evicted) > 0 {
				m.mu.Lock()
				remaining := m.subscribers[routeID]
				for _, dead := range evicted {
					for i, s := range remaining {
						if s == dead {
							remaining = append(remaining[:i], remaining[i+1:]...)
							close(dead)
							break
						}
					}
				}
				m.subscribers[routeID] = remaining
				m.mu.Unlock()
				log.Printf("[harness] evicted %d slow SSE subscribers on session %s", len(evicted), routeID)
			}
		}

		// Convenience-event derivation runs AFTER the raw event has
		// been fanned out so subscribers see cause before effect on
		// Last-Event-ID replay. See msg/CONVENIENCE-EVENTS.md.
		m.deriveAndBroadcast(routeID, &event)
	}

	// Process exited — drain everything still queued for log-store before
	// tearing the session down, so the durable history is complete.
	m.logStoreWrites.Close(bridgeID)

	// Close all subscriber channels. Subagent sessions live and die with the
	// process that hosts them, so they are torn down here too: their
	// per-session maps would otherwise leak for the lifetime of the server,
	// and any subagent still marked running (the process died before its task
	// reported a terminal status) would sit at "running" forever.
	m.mu.Lock()
	for _, id := range append(subagents.sessionIDs(), bridgeID) {
		for _, ch := range m.subscribers[id] {
			close(ch)
		}
		delete(m.subscribers, id)
		delete(m.processes, id)
		delete(m.msgState, id)
		delete(m.derivation, id)
	}
	// The loop above already cleared the per-session maps for bridgeID and
	// every subagent it hosted. budgetHalted is not one of them: it is
	// per-process announcement bookkeeping, not the verdict — the verdict is
	// the persisted spend against the persisted ceiling, which
	// SessionOverBudget reads. Dropping it here means a session that comes
	// back and breaches again says so again. Only the parent can hold one; a
	// subagent has no ceiling of its own.
	delete(m.budgetHalted, bridgeID)
	m.mu.Unlock()
	m.pending.drop(bridgeID)
	subagents.settleUnfinished()

	m.store.UpdateSessionPID(bridgeID, 0)
}

// deriveAndBroadcast runs the convenience-event derivation against
// src, persists any derived events, and fans them out to the same
// subscriber set as the raw stream. Mirrors the persistence + fan-out
// tail of readEvents but bypasses AssignMessageID (derived events
// don't belong to a message bubble — they're bookkeeping like
// session_state).
// ForceSessionState records a state the SERVER decided on rather than one
// an event implied, then persists and broadcasts it exactly as a derived
// transition. Returns false when the session already held that state, so
// the caller can answer without claiming a change that did not happen.
//
// The interrupt handler is the caller this exists for. The user pressing
// Stop produces no harness event, so before this the handler wrote the
// session row behind the derivation's back: the derivation kept holding
// tool_running, the next event computed its transition from a state the
// session had left, and no SSE subscriber ever heard the interrupt — only
// derive() broadcasts. Every client learned about it by refetching, which
// is why bridge-ui carried a localStorage set of interrupted ids instead.
func (m *Manager) ForceSessionState(bridgeID string, state msg.SessionState, reason string) bool {
	m.mu.Lock()
	d := m.derivation[bridgeID]
	m.mu.Unlock()
	if d == nil {
		// No live derivation — the process is gone, so there is no state
		// machine to keep honest. Write the row and say so; the next
		// derivation seeds itself from exactly this value.
		if err := m.store.UpdateSessionState(bridgeID, string(state)); err != nil {
			log.Printf("[harness] ForceSessionState: update session %s: %v", bridgeID, err)
			return false
		}
		return true
	}

	prev, changed := d.forceState(state)
	if !changed {
		return false
	}

	if err := m.store.UpdateSessionState(bridgeID, string(state)); err != nil {
		log.Printf("[harness] ForceSessionState: update session %s: %v", bridgeID, err)
		return false
	}

	var harnessName msg.Harness
	if sess, err := m.store.GetSession(bridgeID); err == nil && sess != nil {
		harnessName = sess.Harness
	}

	m.broadcastDerived(bridgeID, &msg.Event{
		Type:            msg.EventSessionState,
		Harness:         harnessName,
		BridgeSessionID: bridgeID,
		Timestamp:       time.Now(),
		State: &msg.StateEvent{
			State:    state,
			Previous: prev,
			Reason:   reason,
		},
	})
	return true
}

func (m *Manager) deriveAndBroadcast(bridgeID string, src *msg.Event) {
	m.mu.Lock()
	d := m.derivation[bridgeID]
	if d == nil {
		// Seed the fresh derivation's prev from the canonical persisted
		// sessions.state row. Without this, a derivation recreated after
		// a bridge-server restart resets prev to idle and suppresses the
		// closing transition on the next EventResult, leaving a row stuck
		// at tool_running/running (the F1 settle bug).
		d = newDerivationStateSeeded(m.persistedSessionState(bridgeID))
		// Seed the spend accumulators from the same row, for the same
		// reason one step further: a derivation is thrown away when its
		// harness process exits, which happens on every stop, crash, idle
		// reap and bridge-server restart — not only when the session ends.
		// Unseeded, the next run reports api_spend_total from zero, the
		// persisted MAX() ignores every dollar under the previous
		// high-water mark, and a session's ceiling re-arms once per resume.
		totalUSD, detail := m.persistedSpend(bridgeID)
		d.seedAPISpend(totalUSD, detail.Calls, detail.Usage, detail.ByModel, detail.ByQuerySource)
		m.derivation[bridgeID] = d
	}
	m.mu.Unlock()

	derived := d.derive(src)

	// Track whether the derivation emitted a session_state transition for
	// src. When it did, the persisted row is written by publishDerived and no
	// reconcile is needed. When it did NOT (next==prev), a terminal event may
	// still leave the persisted row stale — reconciled below.
	emittedState := m.publishDerived(bridgeID, derived)

	// The turn-end observer runs after the derived events are out, so a
	// consumer that reacts to a turn ending never races the state event that
	// announced it. Async because observers do slow work (the signal
	// classifier calls a model): readEvents owns the session's only event
	// channel, and blocking it here would stall the whole stream.
	if src.Type == msg.EventResult {
		m.mu.RLock()
		observer := m.turnEnd
		m.mu.RUnlock()
		if observer != nil {
			go observer(bridgeID, src, d.currentState())
		}
	}

	// F1 server-side settle reconcile. A terminal event (EventResult/
	// EventError) whose derived transition was suppressed (next==prev)
	// can leave the persisted sessions.state row lying at a holding value
	// — e.g. a direct write on send/resume (manager.go Start/Resume) or a
	// pre-seed restart. Correct the row (and broadcast) ONLY when it
	// actually disagrees with the settled truth, so no-ops don't spam.
	//
	// NOTE: the OTHER settle failure — a hung harness that never emits any
	// terminator at all — is handled by the reaper and the
	// TURN_IDLE_TIMEOUT watchdog in llm-bridge-claudecode, not here.
	if (src.Type == msg.EventResult || src.Type == msg.EventError) && !emittedState {
		m.reconcileSettledSessionState(bridgeID, d)
	}
}

// publishDerived persists derived events, writes any session-state
// transition through to the session row, enforces the spend ceiling, and
// fans the events out to the same subscriber set as the raw stream.
//
// It reports whether any of them was a session_state transition, which is
// what tells deriveAndBroadcast the persisted row is already current and
// needs no settle reconcile.
func (m *Manager) publishDerived(bridgeID string, derived []msg.Event) (emittedSessionState bool) {
	for i := range derived {
		ev := &derived[i]

		// SessionState transitions also update the persistent session
		// row — derivation owns the authoritative state. readEvents no
		// longer flips state on EventResult/EventError directly; this
		// is the single point where session.state is written.
		if ev.Type == msg.EventSessionState && ev.State != nil {
			emittedSessionState = true
			if err := m.store.UpdateSessionState(bridgeID, string(ev.State.State)); err != nil {
				log.Printf("[harness] failed to update session state for %s: %v", bridgeID, err)
			}
		}
		m.broadcastDerived(bridgeID, ev)

		// The spend ceiling is checked here because this is the only
		// place the running per-session dollar total exists: the
		// derivation produces api_spend_total, nothing upstream carries
		// a cumulative figure, and no harness knows what its ceiling is.
		m.enforceBudget(bridgeID, ev)
	}
	return emittedSessionState
}

// broadcastDerived persists a derived (or reconcile) event to the local
// store and log-store, then fans it out to the session's SSE
// subscribers. It stamps BridgeSessionID but does NOT write
// sessions.state — callers own the state write so the single-source-of-
// truth update stays explicit at each call site.
func (m *Manager) broadcastDerived(bridgeID string, ev *msg.Event) {
	ev.BridgeSessionID = bridgeID

	var rowID int64
	if data, err := json.Marshal(ev); err == nil {
		var storeErr error
		rowID, storeErr = m.store.StoreEventReturningID(bridgeID, string(ev.Type), "", "", data)
		if storeErr != nil {
			log.Printf("[harness] failed to store derived event: %v", storeErr)
		}
	}
	m.logStoreWrites.Enqueue(bridgeID, "derived event", *ev)

	stored := StoredEvent{Event: *ev, RowID: rowID}
	m.mu.RLock()
	subs := make([]chan StoredEvent, len(m.subscribers[bridgeID]))
	copy(subs, m.subscribers[bridgeID])
	m.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- stored:
		default:
			// Drop on a full channel — replay path covers it.
		}
	}
}

// persistedSessionState reads the canonical sessions.state row for
// bridgeID and returns it as a SessionState. Missing row (fresh session)
// or empty state falls back to idle; a real read error is logged loudly
// (fail-fast) and also falls back to idle so the event loop stays live.
func (m *Manager) persistedSessionState(bridgeID string) msg.SessionState {
	sess, err := m.store.GetSession(bridgeID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("[harness] persistedSessionState: read session %s: %v", bridgeID, err)
		}
		return msg.SessionIdle
	}
	if sess == nil || sess.State == "" {
		return msg.SessionIdle
	}
	return msg.SessionState(sess.State)
}

// persistedSpend reads the cumulative spend already recorded for bridgeID.
// Missing row (fresh session) reads as zero, which is the correct seed for
// one. A real read error is logged loudly and also reads as zero: refusing to
// derive at all would take the session's whole event stream down, and the
// MAX() in RecordSessionSpend is what stops a zero seed from walking the
// recorded figure backwards.
func (m *Manager) persistedSpend(bridgeID string) (float64, store.SessionSpendDetail) {
	totalUSD, detail, err := m.store.SessionSpend(bridgeID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("[harness] persistedSpend: read spend for %s: %v", bridgeID, err)
		}
		return 0, store.SessionSpendDetail{}
	}
	return totalUSD, detail
}

// reconcileSettledSessionState corrects a persisted sessions.state row
// that still reads a holding value (running / tool_running /
// awaiting_permission / ...) after the turn has actually settled. Called
// only on a terminal event whose derived transition was suppressed
// (next==prev), so it fires exactly when the in-memory prev already
// matched the settled state but the persisted row diverged.
//
// It gates on the persisted row (a) being a holding state — so a late
// stray result cannot clobber a legitimate completed/aborted/paused row
// — and (b) differing from the settled truth, so a row already correct
// triggers no redundant write or broadcast. d.currentState() is the
// settled target: derive() leaves d.sessionState at the terminal value
// even when it suppressed the transition (prev already equalled next).
func (m *Manager) reconcileSettledSessionState(bridgeID string, d *derivationState) {
	settled := d.currentState()

	sess, err := m.store.GetSession(bridgeID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("[harness] reconcileSettledSessionState: read session %s: %v", bridgeID, err)
		}
		return
	}
	if sess == nil {
		return
	}
	stored := msg.SessionState(sess.State)
	if stored == settled || !isHoldingSessionState(stored) {
		return
	}

	if err := m.store.UpdateSessionState(bridgeID, string(settled)); err != nil {
		log.Printf("[harness] reconcileSettledSessionState: update session %s: %v", bridgeID, err)
		return
	}
	log.Printf("[harness] reconciled stale session state for %s: %s → %s (turn settled, transition was suppressed)", bridgeID, stored, settled)

	m.broadcastDerived(bridgeID, &msg.Event{
		Type:            msg.EventSessionState,
		Harness:         sess.Harness,
		BridgeSessionID: bridgeID,
		Timestamp:       time.Now(),
		State: &msg.StateEvent{
			State:    settled,
			Previous: stored,
			Reason:   "turn_settled_reconcile",
		},
	})
}

// BroadcastEvent assigns a MessageID on ev (mutating it), persists, and fans
// out an event that originates from the bridge server itself (not from
// harness stdout). Used by the /send handler to publish user_message events
// so other SSE subscribers see them without an extra round-trip.
func (m *Manager) BroadcastEvent(ev *msg.Event) (int64, error) {
	bridgeID := ev.BridgeSessionID
	if bridgeID == "" {
		return 0, fmt.Errorf("BroadcastEvent: bridge_session_id is required")
	}

	// Stamp Harness from the session row when the caller didn't set it.
	// Bridge-originated events (user_message from /send, derived state
	// transitions, system bookkeeping) all flow through here without a
	// harness adapter to fill this field, but the session row carries
	// the canonical harness — copy it so downstream consumers can route
	// per-harness UI without re-querying the session.
	if ev.Harness == "" {
		if sess, err := m.store.GetSession(bridgeID); err == nil && sess != nil {
			ev.Harness = sess.Harness
		}
	}

	m.AssignMessageID(bridgeID, ev)

	data, err := json.Marshal(ev)
	if err != nil {
		return 0, err
	}
	rowID, err := m.store.StoreEventReturningID(bridgeID, string(ev.Type), ev.MessageID, ev.HarnessMessageID, data)
	if err != nil {
		return 0, err
	}

	// Mirror readEvents: bridge-originated events are also durable in
	// log-store. Without this, hook events / session-state / display-name
	// events landed in bridge-server's local table but never reached
	// log-store, causing II.A dual-write parity drift (see audit
	// 2026-05-11). Errors are returned — callers that don't care wrap and
	// log; callers that do care (e.g. /send) propagate.
	// This one stays on the caller's goroutine because the error is
	// returned, not logged: PushSync drains the session's write queue
	// first so the event still lands after everything the pump queued
	// before it, then writes it here and hands back the real failure.
	if _, err := m.logStoreWrites.PushSync(bridgeID, *ev); err != nil {
		return rowID, fmt.Errorf("push to log-store: %w", err)
	}

	// Mirror readEvents: hook events drive the awaiting_resolution →
	// completed pending map so a freshly-connected client can hydrate the
	// banner via /sessions/:id/hooks/pending. Required for bridge-emitted
	// hook events (e.g. the PreToolUse permission-prehook handler) since
	// those don't pass through the harness stdout reader.
	if ev.Type == msg.EventHook {
		m.pending.record(bridgeID, ev)
	}

	stored := StoredEvent{Event: *ev, RowID: rowID}
	m.mu.RLock()
	subs := make([]chan StoredEvent, len(m.subscribers[bridgeID]))
	copy(subs, m.subscribers[bridgeID])
	m.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- stored:
		default:
			// Drop on a full subscriber channel — replay path covers reconnect.
		}
	}

	// Drive convenience-event derivation off bridge-originated events too.
	// /send's user_message is the canonical case: without this, the
	// state machine never sees turn_started, so the closing →idle
	// transition on EventResult is suppressed (prev==next==idle) and
	// agent_state events disappear for the whole turn.
	m.deriveAndBroadcast(bridgeID, ev)

	return rowID, nil
}

// FlushLogStoreWrites blocks until every event queued for bridgeID has
// reached log-store. The manager's own read-backs call this internally;
// it is exported for the HTTP layer, which proxies a session's message
// history straight to log-store and would otherwise render a
// conversation missing whatever the pump has queued but not yet written.
func (m *Manager) FlushLogStoreWrites(bridgeID string) {
	m.logStoreWrites.Flush(bridgeID)
}

// RecoverInFlightTurn reads log-store to reconstruct turn-level state for a
// session whose process is restarting. Replaces the legacy
// store.RecoverInFlightTurn local-DB query as part of Phase II.A cutover.
// Returns (nil, nil) when no turn is in flight; errors propagate.
func (m *Manager) RecoverInFlightTurn(bridgeID string) (*store.InFlightTurnState, error) {
	// Read-after-write: the pump writes to log-store through an ordered
	// queue, so drain it before reading back or this can miss the turn it
	// is trying to recover. Cheap on the path that matters — loadMsgState
	// reaches here holding the manager lock, but only on a session's first
	// event, when the queue the pump just opened is still empty.
	m.logStoreWrites.Flush(bridgeID)
	ts, err := m.logStore.GetTurnState(bridgeID)
	if err != nil {
		return nil, fmt.Errorf("turn-state: %w", err)
	}
	if !ts.InFlight || ts.LastUserMessageEventID == 0 {
		return nil, nil
	}

	userEvents, err := m.logStore.ListEvents(bridgeID, 0, []string{"user_message"})
	if err != nil {
		return nil, fmt.Errorf("user_message events: %w", err)
	}
	if len(userEvents) == 0 {
		return nil, nil
	}
	var userEv msg.Event
	if err := json.Unmarshal(userEvents[len(userEvents)-1], &userEv); err != nil {
		return nil, fmt.Errorf("decode user_message: %w", err)
	}
	if userEv.TurnID == "" {
		return nil, nil
	}
	st := &store.InFlightTurnState{
		TurnID:          userEv.TurnID,
		ClientRequestID: userEv.ClientRequestID,
	}

	assistTypes := []string{"block", "stream", "thinking", "tool_call", "tool_result", "plan", "approval", "result"}
	assistEvents, err := m.logStore.ListEvents(bridgeID, int64(ts.LastUserMessageEventID), assistTypes)
	if err != nil {
		return nil, fmt.Errorf("assistant events: %w", err)
	}
	for i := len(assistEvents) - 1; i >= 0; i-- {
		var e msg.Event
		if err := json.Unmarshal(assistEvents[i], &e); err != nil {
			continue
		}
		if e.MessageID != "" {
			st.BridgeMessageID = e.MessageID
			st.HarnessMessageID = e.HarnessMessageID
			break
		}
	}
	return st, nil
}

// InterruptedTurn returns the most recent turn when no terminator follows its
// user_message, along with how many tool calls that turn had already run.
// Returns nil when the last turn is balanced or when no user_message exists.
// Replaces store.InterruptedTurn's local-DB query.
func (m *Manager) InterruptedTurn(bridgeID string) (*store.InterruptedTurn, error) {
	m.logStoreWrites.Flush(bridgeID)
	ts, err := m.logStore.GetTurnState(bridgeID)
	if err != nil {
		return nil, fmt.Errorf("turn-state: %w", err)
	}
	if !ts.InFlight || ts.LastUserMessageEventID == 0 {
		return nil, nil
	}
	events, err := m.logStore.ListEvents(bridgeID, 0, []string{"user_message"})
	if err != nil {
		return nil, fmt.Errorf("user_message events: %w", err)
	}
	if len(events) == 0 {
		return nil, nil
	}
	var ev msg.Event
	if err := json.Unmarshal(events[len(events)-1], &ev); err != nil {
		return nil, fmt.Errorf("decode user_message: %w", err)
	}
	if ev.Result == nil {
		return nil, nil
	}
	// Counted from the user_message that opened the turn, so tool calls from
	// earlier, completed turns cannot make this turn look like it did work.
	toolCalls, err := m.logStore.ListEvents(bridgeID, int64(ts.LastUserMessageEventID), []string{"tool_call"})
	if err != nil {
		return nil, fmt.Errorf("tool_call events: %w", err)
	}
	return &store.InterruptedTurn{
		UserMessageText:     ev.Result.Text,
		ToolCallsAlreadyRun: len(toolCalls),
	}, nil
}

// RecentTurnTexts pairs user_message events with following result events to
// produce up to limit recent (user, assistant) text pairs, oldest first.
// Replaces store.RecentTurnTexts.
func (m *Manager) RecentTurnTexts(bridgeID string, limit int) ([]store.TurnText, error) {
	m.logStoreWrites.Flush(bridgeID)
	events, err := m.logStore.ListEvents(bridgeID, 0, []string{"user_message", "result"})
	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	var turns []store.TurnText
	var pending *store.TurnText
	for _, raw := range events {
		var ev msg.Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}
		text := ""
		if ev.Result != nil {
			text = ev.Result.Text
		}
		text = truncateRunes(text, 2000)
		if ev.Type == msg.EventUserMessage {
			if pending != nil {
				turns = append(turns, *pending)
			}
			pending = &store.TurnText{User: text}
			continue
		}
		if pending == nil {
			pending = &store.TurnText{Assistant: text}
		} else {
			pending.Assistant = text
		}
		turns = append(turns, *pending)
		pending = nil
	}
	if pending != nil {
		turns = append(turns, *pending)
	}
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, nil
}

// ListToolCallInputs returns the raw tool_call.input JSON for every tool_call
// in the session, oldest first. Empty/null inputs are skipped. Replaces
// store.ListToolCallInputs.
func (m *Manager) ListToolCallInputs(bridgeID string) ([]json.RawMessage, error) {
	m.logStoreWrites.Flush(bridgeID)
	events, err := m.logStore.ListEvents(bridgeID, 0, []string{"tool_call"})
	if err != nil {
		return nil, fmt.Errorf("tool_call events: %w", err)
	}
	out := make([]json.RawMessage, 0, len(events))
	for _, raw := range events {
		var holder struct {
			ToolCall struct {
				Input json.RawMessage `json:"input"`
			} `json:"tool_call"`
		}
		if err := json.Unmarshal(raw, &holder); err != nil {
			continue
		}
		if len(holder.ToolCall.Input) == 0 || string(holder.ToolCall.Input) == "null" {
			continue
		}
		out = append(out, holder.ToolCall.Input)
	}
	return out, nil
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// ActiveCount returns the number of running processes.
func (m *Manager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.processes)
}

// StartOnInstance spawns a harness session on a specific instance with credential binding.
// Dispatches by the bound machine's Transport: TransportLocal forks a
// subprocess on this host; TransportSSH wraps it in an ssh client;
// TransportRunner sends a Spawn message over a registered runner WS.
//
// inst.Machine must be populated by the caller (startOnInstance helper in
// the server package handles this).
func (m *Manager) StartOnInstance(ctx context.Context, sess *store.Session, inst *msg.Instance, credentialID string) (HarnessProcess, error) {
	if inst.Machine == nil {
		return nil, fmt.Errorf("instance %s missing Machine; caller must populate it before StartOnInstance", inst.ID)
	}
	h := msg.Harness(sess.Harness)

	// One reading of the session's working directory, shared by every branch
	// below. The local branches also have to prove it exists before spawning;
	// ssh and runner name a path on another host and must not be checked here.
	// workingDirOwner names the record that supplied the path, so a refusal
	// tells the operator which of session/instance/machine to go and edit.
	workingDir, workingDirOwner := workingDirForSession(sess, inst)

	var proc HarnessProcess
	var err error

	if sess.Mode == msg.SessionModePTY {
		// PTY mode: only TransportLocal in v1. SSH/runner pty forwarding
		// is in scope but adds an extra hop and protocol layer; defer
		// until child 5+ once local pty has bedded in.
		if inst.Machine.Transport != msg.TransportLocal {
			return nil, fmt.Errorf("pty mode requires local transport; instance %s is %s", inst.ID, inst.Machine.Transport)
		}
		binPath, ok := Available(h)
		if !ok {
			return nil, fmt.Errorf("harness binary not found: %s", msg.HarnessBinaryName(h))
		}
		if err := verifyLocalWorkingDir(workingDirOwner, workingDir); err != nil {
			return nil, err
		}

		// Spawn the per-session OTel sidecar first so the PTY child sees
		// OTEL_EXPORTER_OTLP_ENDPOINT etc. at spawn time. Sidecar failure
		// is non-fatal — telemetry is observability, the user's chat
		// session should still come up. Log loudly so the gap is visible.
		//
		// localBridgeURL is the loopback URL bridge-server listens on; the
		// sidecar POSTs translated msg.Events to {localBridgeURL}/sidecar/event/{bridge_id}.
		// When empty (misconfigured deployment), the sidecar is skipped
		// and telemetry is silently disabled for PTY sessions — log once
		// per session so the gap surfaces.
		var sidecarEnv []string
		if h == msg.HarnessClaudeCode {
			if m.localBridgeURL == "" {
				log.Printf("[sidecar] localBridgeURL not configured; PTY session %s starts without OTel", sess.SessionID)
			} else {
				// Tell the sidecar the same directory the child gets.
				// How the harness uses it to find the session's telemetry is
				// the harness's own business; the contract this layer owes is
				// only that the two agree.
				ptyCwd := ptyChildWorkingDir(workingDir)
				sc, env, err := startOTelSidecar(binPath, sess.SessionID, m.localBridgeURL, ptyCwd, sess.HarnessSessionID)
				if err != nil {
					log.Printf("[sidecar] start failed for %s (continuing without OTel): %v", sess.SessionID, err)
				} else {
					sidecarEnv = env
					m.mu.Lock()
					m.otelSidecars[sess.SessionID] = sc
					m.mu.Unlock()
					log.Printf("[sidecar] spawned for PTY session %s, endpoint=%s, cwd=%s, resume=%s", sess.SessionID, sc.endpointURL, ptyCwd, sess.HarnessSessionID)
				}
			}
		}

		proc, err = StartProcessPTY(ctx, binPath, sess, credentialID, sidecarEnv, workingDir)
		if err != nil {
			// PTY launch failed — sidecar is now orphaned. Stop it.
			m.mu.Lock()
			if sc := m.otelSidecars[sess.SessionID]; sc != nil {
				sc.stop()
				delete(m.otelSidecars, sess.SessionID)
			}
			m.mu.Unlock()
		}
	} else {
		switch inst.Machine.Transport {
		case msg.TransportSSH:
			proc, err = m.startSSH(ctx, sess, inst, credentialID)
		case msg.TransportRunner:
			proc, err = m.startRunner(ctx, sess, inst, credentialID)
		default:
			// Local transport
			binPath, ok := Available(h)
			if !ok {
				return nil, fmt.Errorf("harness binary not found: %s", msg.HarnessBinaryName(h))
			}
			if err := verifyLocalWorkingDir(workingDirOwner, workingDir); err != nil {
				return nil, err
			}
			proc, err = StartProcess(ctx, binPath, sess, credentialID, workingDir)
		}
	}

	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.processes[sess.SessionID] = proc
	m.mu.Unlock()

	m.store.UpdateSessionPID(sess.SessionID, proc.PID())
	m.store.UpdateSessionState(sess.SessionID, string(msg.SessionStarting))

	if sess.Mode == msg.SessionModePTY {
		// PTY processes have no event channel to drain; readEvents would
		// see the pre-closed Events() channel and immediately tear the
		// process out of the manager map, breaking the attach hub. Watch
		// for child exit instead so we still clean up state when the pty
		// dies.
		if pp, ok := proc.(*PTYProcess); ok {
			hub := NewAttachHub(pp, m.ptyRingBytes)
			m.mu.Lock()
			m.attachHubs[sess.SessionID] = hub
			m.mu.Unlock()
			go m.watchPTYExit(pp)
		}
	} else {
		go m.readEvents(proc)
	}

	return proc, nil
}

// watchPTYExit waits for a pty session's child to exit, then drops it
// from the manager map and resets the session row's state. Mirrors the
// cleanup tail of readEvents, minus the SSE subscriber fan-out (pty
// sessions don't broadcast msg.Events).
//
// The attach hub's own watchExit goroutine handles client teardown; we
// just unregister it here so attempts to attach after the pty died fall
// through to a clean 404/closed-hub error.
func (m *Manager) watchPTYExit(p *PTYProcess) {
	<-p.Done()
	bridgeID := p.SessionID()

	m.mu.Lock()
	delete(m.processes, bridgeID)
	delete(m.msgState, bridgeID)
	delete(m.attachHubs, bridgeID)
	sidecar := m.otelSidecars[bridgeID]
	delete(m.otelSidecars, bridgeID)
	m.mu.Unlock()

	// Stop the OTel sidecar after the PTY child exits. The sidecar's
	// internal flush window (~2s) is honored by stop() so trailing
	// telemetry batches still land before the receiver closes.
	if sidecar != nil {
		sidecar.stop()
	}

	m.store.UpdateSessionPID(bridgeID, 0)
	m.store.UpdateSessionState(bridgeID, string(msg.SessionCompleted))
}

// startSSH spawns a harness process on a remote machine via SSH. Reads
// the connection details from inst.Machine.
func (m *Manager) startSSH(ctx context.Context, sess *store.Session, inst *msg.Instance, credentialID string) (*Process, error) {
	binName := msg.HarnessBinaryName(msg.Harness(sess.Harness))
	if binName == "" {
		return nil, fmt.Errorf("unknown harness type: %s", sess.Harness)
	}
	mach := inst.Machine

	// Build SSH command
	args := []string{}

	if mach.SSHKeyPath != "" {
		args = append(args, "-i", mach.SSHKeyPath)
	}

	port := mach.SSHPort
	if port == 0 {
		port = 22
	}
	if port != 22 {
		args = append(args, "-p", strconv.Itoa(port))
	}

	// Disable host key checking for automated use (consider making this configurable)
	args = append(args, "-o", "StrictHostKeyChecking=accept-new")
	args = append(args, "-o", "BatchMode=yes")

	// Add user@host
	target := mach.Hostname
	if mach.SSHUser != "" {
		target = mach.SSHUser + "@" + mach.Hostname
	}
	args = append(args, target)

	// Remote command: cd to the resolved working dir and run the harness.
	// Not verified here — the path is on mach, not on this host.
	workDir, _ := workingDirForSession(sess, inst)
	remoteCmd := binName
	if workDir != "" {
		remoteCmd = fmt.Sprintf("cd %s && %s", workDir, binName)
	}
	args = append(args, remoteCmd)

	return StartSSHProcess(ctx, args, sess, credentialID)
}

// DiscoverSessions invokes a harness binary with -discover to find sessions
// stored on disk by the underlying CLI tool.
// If harness is empty, it discovers across all available harness types.
func (m *Manager) DiscoverSessions(ctx context.Context, h msg.Harness) ([]msg.StoredSession, error) {
	var harnesses []msg.Harness
	if h != "" {
		harnesses = []msg.Harness{h}
	} else {
		harnesses = discoverableHarnesses()
	}

	var all []msg.StoredSession
	for _, hType := range harnesses {
		binPath, ok := Available(hType)
		if !ok {
			continue
		}

		sessions, err := runDiscover(ctx, binPath)
		if err != nil {
			log.Printf("[harness] discover %s: %v", hType, err)
			continue
		}
		all = append(all, sessions...)
	}

	return all, nil
}

// discoverableHarnesses returns harness types that support -discover.
func discoverableHarnesses() []msg.Harness {
	return []msg.Harness{
		msg.HarnessClaudeCode,
		msg.HarnessCodex,
		msg.HarnessHermes,
	}
}

// runDiscover executes a harness binary with -discover and parses the JSON output.
func runDiscover(ctx context.Context, binPath string) ([]msg.StoredSession, error) {
	cmd := exec.CommandContext(ctx, binPath, "-discover")
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("exec %s -discover: %w", binPath, err)
	}

	var sessions []msg.StoredSession
	if err := json.Unmarshal(out, &sessions); err != nil {
		return nil, fmt.Errorf("parse discover output: %w", err)
	}

	return sessions, nil
}

// ImportHistory runs a harness with -import-history and pushes events to log-store.
// Used to import conversation history for discovered sessions. The harness has no
// concept of bridge sessions, so each emitted event leaves bridge_session_id empty;
// the manager owns that mapping and stamps it here before pushing to log-store.
func (m *Manager) ImportHistory(ctx context.Context, bridgeSessionID string, h msg.Harness, harnessSessionID string) (int, error) {
	if bridgeSessionID == "" {
		return 0, fmt.Errorf("ImportHistory: bridge_session_id is required")
	}
	binPath, ok := Available(h)
	if !ok {
		return 0, fmt.Errorf("harness binary not found: %s", msg.HarnessBinaryName(h))
	}

	cmd := exec.CommandContext(ctx, binPath, "-import-history", harnessSessionID)
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("exec %s -import-history: %w", binPath, err)
	}

	// Parse NDJSON output and push each event to log-store
	var imported int
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event msg.Event
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}

		event.BridgeSessionID = bridgeSessionID

		if _, err := m.logStore.PushEvent(event); err != nil {
			log.Printf("[import-history] failed to push event: %v", err)
			continue
		}
		imported++
	}

	return imported, scanner.Err()
}

// CheckSSHReachability tests if a machine is reachable. Local machines
// are trivially reachable; runner machines are reachable iff a runner is
// currently registered for them; SSH machines run a quick login probe.
func (m *Manager) CheckSSHReachability(mach *msg.Machine) bool {
	if mach == nil {
		return false
	}
	switch mach.Transport {
	case msg.TransportLocal:
		return true
	case msg.TransportRunner:
		return m.runners != nil && m.runners.Get(mach.Name) != nil
	case msg.TransportSSH:
		// fall through
	default:
		return false
	}

	args := []string{}
	if mach.SSHKeyPath != "" {
		args = append(args, "-i", mach.SSHKeyPath)
	}
	port := mach.SSHPort
	if port == 0 {
		port = 22
	}
	if port != 22 {
		args = append(args, "-p", strconv.Itoa(port))
	}
	args = append(args, "-o", "StrictHostKeyChecking=accept-new")
	args = append(args, "-o", "BatchMode=yes")
	args = append(args, "-o", "ConnectTimeout=5")

	target := mach.Hostname
	if mach.SSHUser != "" {
		target = mach.SSHUser + "@" + mach.Hostname
	}
	args = append(args, target, "echo", "ok")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ssh", args...)
	err := cmd.Run()
	return err == nil
}
