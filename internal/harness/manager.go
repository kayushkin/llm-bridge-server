package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	logstore "github.com/kayushkin/log-store/client"
	"github.com/kayushkin/llm-bridge-server/internal/ids"
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
}

// StoredEvent pairs an event with its database row ID, assigned at insert time.
type StoredEvent struct {
	msg.Event
	RowID int64
}

// Manager handles harness subprocess lifecycle.
type Manager struct {
	mu          sync.RWMutex
	processes   map[string]HarnessProcess     // sessionID → process
	subscribers map[string][]chan StoredEvent // sessionID → SSE subscriber channels
	msgState    map[string]*sessionMsgState   // sessionID → message-id assignment state
	store       *store.Store
	logStore    *logstore.Client
	runners     *RunnerRegistry // optional; nil disables TransportRunner spawns
}

// NewManager creates a harness manager.
func NewManager(st *store.Store, logStoreURL string) *Manager {
	return &Manager{
		processes:   make(map[string]HarnessProcess),
		subscribers: make(map[string][]chan StoredEvent),
		msgState:    make(map[string]*sessionMsgState),
		store:       st,
		logStore:    logstore.New(logStoreURL),
		runners:     NewRunnerRegistry(),
	}
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
	m.msgState[bridgeID] = st
	return st
}

// AssignMessageID stamps an event with its canonical bridge MessageID,
// extracts and records the harness-native id, stamps a bridge-minted TurnID
// on every event in the turn, and tracks turn boundaries.
//
// Rules:
//   - user_message: mints a fresh MessageID for the user bubble, mints a fresh
//     TurnID for the turn, closes any open assistant bubble.
//   - assistant-side events (stream/thinking/tool_call/tool_result/plan/approval/result):
//     reuse an existing bridge MessageID when the harness id has been seen
//     before (resume case); split when the harness id changes mid-turn;
//     otherwise mint a new MessageID on the first event of a bubble.
//   - result/error: stamp with the in-flight MessageID, then close the turn
//     (clear MessageID, TurnID, ClientRequestID state).
//   - session_state leaving running: drop any in-flight turn state.
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
		st.bridgeMsgID = ""
		st.harnessMsgID = ""
		// Latch the caller's per-turn id so we can stamp it on downstream
		// events coming back from the harness.
		st.clientRequestID = ev.ClientRequestID
		// Open a new turn.
		st.turnID = ids.NewTurnID()

	case msg.EventStream, msg.EventThinking, msg.EventToolCall,
		msg.EventToolResult, msg.EventPlan, msg.EventApproval:
		ev.MessageID = m.assignAssistantID(st, hid)
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
		ev.MessageID = m.assignAssistantID(st, hid)
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
		return

	case msg.EventSessionState:
		if ev.State != nil && ev.State.State != msg.SessionRunning {
			st.bridgeMsgID = ""
			st.harnessMsgID = ""
			st.clientRequestID = ""
			st.turnID = ""
		}

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

// assignAssistantID picks the bridge MessageID for an assistant-side event.
// Caller must hold m.mu.
func (m *Manager) assignAssistantID(st *sessionMsgState, hid string) string {
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

	proc, err := StartProcess(ctx, binPath, sess, "")
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.processes[sess.BridgeID] = proc
	m.mu.Unlock()

	// Update session with PID
	m.store.UpdateSessionPID(sess.BridgeID, proc.PID())
	m.store.UpdateSessionState(sess.BridgeID, string(msg.SessionRunning))

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

// Send writes a message to the harness stdin.
func (m *Manager) Send(sessionID string, message string) error {
	proc := m.Get(sessionID)
	if proc == nil {
		return fmt.Errorf("session not running: %s", sessionID)
	}
	return proc.Send(message)
}

// SendCommand sends a command (compact, resume, etc.) to the harness.
func (m *Manager) SendCommand(sessionID string, cmd string) error {
	proc := m.Get(sessionID)
	if proc == nil {
		return fmt.Errorf("session not running: %s", sessionID)
	}
	return proc.SendCommand(cmd)
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

// readEvents reads events from process, persists them, updates state,
// and fans out to all SSE subscribers.
func (m *Manager) readEvents(proc HarnessProcess) {
	bridgeID := proc.SessionID()
	harnessIDSet := false

	for event := range proc.Events() {
		// On first event with a harness session ID, store it on the session row.
		// The bridge_id stays stable — we just fill in harness_id.
		if !harnessIDSet && event.SessionID != "" && event.SessionID != bridgeID {
			if err := m.store.SetHarnessID(bridgeID, event.SessionID); err != nil {
				log.Printf("[harness] failed to set harness_id on %s: %v", bridgeID, err)
			} else {
				harnessIDSet = true

				// Notify SSE subscribers so frontends learn the harness ID.
				idEvent := msg.Event{
					Type:      msg.EventSystem,
					SessionID: bridgeID,
					ClientID:  event.SessionID, // carry harness_id in client_id for now
					Timestamp: time.Now(),
					System:    &msg.SystemEvent{Subtype: "harness_id_set", Message: event.SessionID},
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

		// Stamp bridge_id so downstream stores can index by it.
		event.BridgeID = bridgeID

		// Assign canonical bridge MessageID and capture harness id.
		m.AssignMessageID(bridgeID, &event)

		// Persist event keyed by bridge_id (stable PK) and capture row ID.
		var rowID int64
		if data, err := json.Marshal(event); err == nil {
			rowID, err = m.store.StoreEventReturningID(bridgeID, string(event.Type), event.MessageID, event.HarnessMessageID, data)
			if err != nil {
				log.Printf("[harness] failed to store event: %v", err)
			}
		}

		// Push to log-store (durable source of truth)
		if _, err := m.logStore.PushEvent(event); err != nil {
			log.Printf("[harness] failed to push event to log-store: %v", err)
		}

		// Update session state based on event type
		switch event.Type {
		case msg.EventSessionState:
			if event.State != nil {
				m.store.UpdateSessionState(bridgeID, string(event.State.State))
			}
		case msg.EventResult:
			m.store.UpdateSessionState(bridgeID, string(msg.SessionCompleted))
		case msg.EventError:
			m.store.UpdateSessionState(bridgeID, string(msg.SessionError))
		case msg.EventSessionInfo:
			if event.Info != nil {
				if err := m.store.SetSessionInfo(bridgeID, event.Info); err != nil {
					log.Printf("[harness] failed to persist session info: %v", err)
				}
			}
		}

		// Fan out to SSE subscribers. Sends are parallel so one slow client
		// can't starve the others, and each has a bounded timeout. On timeout
		// the subscriber is evicted — its SSE stream closes, the client
		// reconnects with Last-Event-ID, and the store replays missed events.
		stored := StoredEvent{Event: event, RowID: rowID}
		m.mu.RLock()
		subs := make([]chan StoredEvent, len(m.subscribers[bridgeID]))
		copy(subs, m.subscribers[bridgeID])
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
				remaining := m.subscribers[bridgeID]
				for _, dead := range evicted {
					for i, s := range remaining {
						if s == dead {
							remaining = append(remaining[:i], remaining[i+1:]...)
							close(dead)
							break
						}
					}
				}
				m.subscribers[bridgeID] = remaining
				m.mu.Unlock()
				log.Printf("[harness] evicted %d slow SSE subscribers on session %s", len(evicted), bridgeID)
			}
		}
	}

	// Process exited — close all subscriber channels
	m.mu.Lock()
	for _, ch := range m.subscribers[bridgeID] {
		close(ch)
	}
	delete(m.subscribers, bridgeID)
	delete(m.processes, bridgeID)
	delete(m.msgState, bridgeID)
	m.mu.Unlock()

	m.store.UpdateSessionPID(bridgeID, 0)
}

// BroadcastEvent assigns a MessageID on ev (mutating it), persists, and fans
// out an event that originates from the bridge server itself (not from
// harness stdout). Used by the /send handler to publish user_message events
// so other SSE subscribers see them without an extra round-trip.
func (m *Manager) BroadcastEvent(ev *msg.Event) (int64, error) {
	bridgeID := ev.BridgeID
	if bridgeID == "" {
		bridgeID = ev.SessionID
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
	return rowID, nil
}

// PushEvent sends an event directly to log-store.
func (m *Manager) PushEvent(ev msg.Event) error {
	_, err := m.logStore.PushEvent(ev)
	return err
}

// ActiveCount returns the number of running processes.
func (m *Manager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.processes)
}

// StartOnInstance spawns a harness session on a specific instance with credential binding.
// Dispatches by Transport: TransportLocal forks a subprocess on this host;
// TransportSSH wraps it in an ssh client; TransportRunner sends a Spawn
// message over a registered runner WebSocket.
func (m *Manager) StartOnInstance(ctx context.Context, sess *store.Session, inst *msg.Instance, credentialID string) (HarnessProcess, error) {
	h := msg.Harness(sess.Harness)

	var proc HarnessProcess
	var err error

	switch inst.Transport {
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
		proc, err = StartProcess(ctx, binPath, sess, credentialID)
	}

	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.processes[sess.BridgeID] = proc
	m.mu.Unlock()

	m.store.UpdateSessionPID(sess.BridgeID, proc.PID())
	m.store.UpdateSessionState(sess.BridgeID, string(msg.SessionRunning))

	go m.readEvents(proc)

	return proc, nil
}

// startSSH spawns a harness process on a remote machine via SSH.
func (m *Manager) startSSH(ctx context.Context, sess *store.Session, inst *msg.Instance, credentialID string) (*Process, error) {
	binName := msg.HarnessBinaryName(msg.Harness(sess.Harness))
	if binName == "" {
		return nil, fmt.Errorf("unknown harness type: %s", sess.Harness)
	}

	// Build SSH command
	args := []string{}

	// Add key if specified
	if inst.SSHKeyPath != "" {
		args = append(args, "-i", inst.SSHKeyPath)
	}

	// Add port if non-standard
	port := inst.SSHPort
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
	target := inst.Host
	if inst.SSHUser != "" {
		target = inst.SSHUser + "@" + inst.Host
	}
	args = append(args, target)

	// Remote command: cd to working dir and run harness
	remoteCmd := binName
	if inst.WorkingDir != "" {
		remoteCmd = fmt.Sprintf("cd %s && %s", inst.WorkingDir, binName)
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
// Used to import conversation history for discovered sessions.
func (m *Manager) ImportHistory(ctx context.Context, h msg.Harness, sessionID string) (int, error) {
	binPath, ok := Available(h)
	if !ok {
		return 0, fmt.Errorf("harness binary not found: %s", msg.HarnessBinaryName(h))
	}

	cmd := exec.CommandContext(ctx, binPath, "-import-history", sessionID)
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

		if _, err := m.logStore.PushEvent(event); err != nil {
			log.Printf("[import-history] failed to push event: %v", err)
			continue
		}
		imported++
	}

	return imported, scanner.Err()
}

// CheckSSHReachability tests if an SSH instance is reachable.
func (m *Manager) CheckSSHReachability(inst *msg.Instance) bool {
	if inst.Transport != msg.TransportSSH {
		return true // Local is always reachable
	}

	args := []string{}
	if inst.SSHKeyPath != "" {
		args = append(args, "-i", inst.SSHKeyPath)
	}
	port := inst.SSHPort
	if port == 0 {
		port = 22
	}
	if port != 22 {
		args = append(args, "-p", strconv.Itoa(port))
	}
	args = append(args, "-o", "StrictHostKeyChecking=accept-new")
	args = append(args, "-o", "BatchMode=yes")
	args = append(args, "-o", "ConnectTimeout=5")

	target := inst.Host
	if inst.SSHUser != "" {
		target = inst.SSHUser + "@" + inst.Host
	}
	args = append(args, target, "echo", "ok")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ssh", args...)
	err := cmd.Run()
	return err == nil
}
