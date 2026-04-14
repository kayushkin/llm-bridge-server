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
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// Manager handles harness subprocess lifecycle.
type Manager struct {
	mu          sync.RWMutex
	processes   map[string]*Process        // sessionID → process
	subscribers map[string][]chan msg.Event // sessionID → SSE subscriber channels
	store       *store.Store
	logStore    *logstore.Client
}

// NewManager creates a harness manager.
func NewManager(st *store.Store, logStoreURL string) *Manager {
	return &Manager{
		processes:   make(map[string]*Process),
		subscribers: make(map[string][]chan msg.Event),
		store:       st,
		logStore:    logstore.New(logStoreURL),
	}
}

// BinaryName returns the expected binary name for a harness.
func BinaryName(h msg.Harness) string {
	switch h {
	case msg.HarnessClaudeCode:
		return "llm-bridge-claudecode"
	case msg.HarnessCodex:
		return "llm-bridge-codex"
	case msg.HarnessOpenClaw:
		return "llm-bridge-openclaw"
	case msg.HarnessInber:
		return "llm-bridge-inber"
	case msg.HarnessHermes:
		return "llm-bridge-hermes"
	case msg.HarnessAider:
		return "llm-bridge-aider"
	case msg.HarnessGoose:
		return "llm-bridge-goose"
	case msg.HarnessAutohand:
		return "llm-bridge-autohand"
	case msg.HarnessJig:
		return "llm-bridge-jig"
	case msg.HarnessDexto:
		return "llm-bridge-dexto"
	case msg.HarnessCommander:
		return "llm-bridge-commander"
	case msg.HarnessNanoClaw:
		return "llm-bridge-nanoclaw"
	case msg.HarnessCline:
		return "llm-bridge-cline"
	case msg.HarnessRooCode:
		return "llm-bridge-roocode"
	case msg.HarnessKiloCode:
		return "llm-bridge-kilocode"
	default:
		return ""
	}
}

// Available checks if a harness binary is in PATH.
func Available(h msg.Harness) (string, bool) {
	bin := BinaryName(h)
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
		return nil, fmt.Errorf("harness binary not found: %s", BinaryName(h))
	}

	proc, err := StartProcess(ctx, binPath, sess, "")
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.processes[sess.ID] = proc
	m.mu.Unlock()

	// Update session with PID
	m.store.UpdateSessionPID(sess.ID, proc.PID())
	m.store.UpdateSessionState(sess.ID, string(msg.SessionRunning))

	// Start event reader goroutine
	go m.readEvents(proc)

	return proc, nil
}

// Get returns a running process by session ID.
func (m *Manager) Get(sessionID string) *Process {
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
func (m *Manager) Subscribe(sessionID string) chan msg.Event {
	ch := make(chan msg.Event, 100)
	m.mu.Lock()
	m.subscribers[sessionID] = append(m.subscribers[sessionID], ch)
	m.mu.Unlock()
	return ch
}

// Unsubscribe removes an SSE subscriber channel.
func (m *Manager) Unsubscribe(sessionID string, ch chan msg.Event) {
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
func (m *Manager) readEvents(proc *Process) {
	sid := proc.SessionID()

	for event := range proc.Events() {
		// Normalize session ID to the bridge session ID so all
		// downstream stores key events consistently.
		event.SessionID = sid

		// Persist event locally
		if data, err := json.Marshal(event); err == nil {
			if err := m.store.StoreEvent(sid, string(event.Type), data); err != nil {
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
				m.store.UpdateSessionState(sid, string(event.State.State))
			}
		case msg.EventResult:
			m.store.UpdateSessionState(sid, string(msg.SessionCompleted))
		case msg.EventError:
			m.store.UpdateSessionState(sid, string(msg.SessionError))
		}

		// Fan out to SSE subscribers
		m.mu.RLock()
		for _, ch := range m.subscribers[sid] {
			select {
			case ch <- event:
			default:
				// Subscriber too slow, drop event
			}
		}
		m.mu.RUnlock()
	}

	// Process exited — close all subscriber channels
	m.mu.Lock()
	for _, ch := range m.subscribers[sid] {
		close(ch)
	}
	delete(m.subscribers, sid)
	delete(m.processes, sid)
	m.mu.Unlock()

	m.store.UpdateSessionPID(sid, 0)
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
func (m *Manager) StartOnInstance(ctx context.Context, sess *store.Session, inst *msg.Instance, credentialID string) (*Process, error) {
	h := msg.Harness(sess.Harness)

	var proc *Process
	var err error

	if inst.Transport == msg.TransportSSH {
		proc, err = m.startSSH(ctx, sess, inst, credentialID)
	} else {
		// Local transport
		binPath, ok := Available(h)
		if !ok {
			return nil, fmt.Errorf("harness binary not found: %s", BinaryName(h))
		}
		proc, err = StartProcess(ctx, binPath, sess, credentialID)
	}

	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.processes[sess.ID] = proc
	m.mu.Unlock()

	m.store.UpdateSessionPID(sess.ID, proc.PID())
	m.store.UpdateSessionState(sess.ID, string(msg.SessionRunning))

	go m.readEvents(proc)

	return proc, nil
}

// startSSH spawns a harness process on a remote machine via SSH.
func (m *Manager) startSSH(ctx context.Context, sess *store.Session, inst *msg.Instance, credentialID string) (*Process, error) {
	binName := BinaryName(msg.Harness(sess.Harness))
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
		return 0, fmt.Errorf("harness binary not found: %s", BinaryName(h))
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
