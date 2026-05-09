package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// HarnessProcess is the runtime handle for a spawned harness session,
// abstracting over the underlying transport (local subprocess, SSH, or
// remote runner over WebSocket). The Manager treats every active session
// as a HarnessProcess so the read/dispatch path is transport-agnostic.
type HarnessProcess interface {
	PID() int
	SessionID() string
	Events() <-chan msg.Event
	// Send writes a user message to the harness. Exactly one of message or
	// blocks should be non-empty: message for text-only input, blocks for
	// multimodal input (text/image/document/audio/video). Setting both is
	// rejected at the HTTP boundary so this layer can stay pass-through.
	Send(message string, blocks []msg.ContentBlock) error
	SendCommand(cmd string) error
	// SendJSONRPC writes a generic JSON-RPC request to the harness's stdin.
	// Used for methods that take params (e.g. resolve_hook). PTY-mode
	// processes return an error; runner-backed processes route via WS.
	SendJSONRPC(method string, params json.RawMessage) error
	Interrupt() error
	Kill() error
}

// Request is sent to harness subprocess via stdin.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}


// StartParams for the "start" method. BridgeSessionID is the routing identifier
// on the wire. HarnessSessionID is the harness-internal id (e.g. Claude Code
// session UUID) the bridge should use when resuming — bridges with their own
// state.db (codex) ignore this and consult their local chain instead;
// state.db-less bridges (jig, aider, openclaw, nanoclaw) use it directly for
// their --resume / equivalent arg.
type StartParams struct {
	BridgeSessionID  string `json:"bridge_session_id"`
	HarnessSessionID string `json:"harness_session_id,omitempty"`
	DisplayName      string `json:"display_name,omitempty"`
	AgentID          string `json:"agent_id,omitempty"`
	CredentialID     string `json:"credential_id,omitempty"`
	Prompt           string `json:"prompt,omitempty"`
	Resume           bool   `json:"resume,omitempty"`
	Fork             string `json:"fork,omitempty"` // parent harness_session_id (the harness UUID to fork from)
	WorkDir          string `json:"work_dir,omitempty"`
}

// MessageParams for the "message" method. BridgeSessionID is added so a single
// bridge process can host multiple sessions; old bridges (which assume one
// session per process) ignore it harmlessly. Either Content or Blocks carries
// the user's turn — bridges that don't yet support multimodal input ignore
// Blocks and process Content as before.
type MessageParams struct {
	BridgeSessionID string             `json:"bridge_session_id,omitempty"`
	Content         string             `json:"content"`
	Blocks          []msg.ContentBlock `json:"blocks,omitempty"`
}

// InterruptParams for the "interrupt" method.
type InterruptParams struct {
	BridgeSessionID string `json:"bridge_session_id,omitempty"`
}

// CommandParams for generic commands like "compact" / "resume".
type CommandParams struct {
	BridgeSessionID string `json:"bridge_session_id,omitempty"`
}

// buildStartParams creates the start request params for a harness subprocess.
// BridgeSessionID is the stable routing key. HarnessSessionID is the
// harness-internal id (e.g. Claude Code session UUID) when known — bridges
// without state.db use it for their --resume arg.
// Returns merged JSON: base start params + any harness-specific config from the session.
func buildStartParams(sess *store.Session, credentialID string) json.RawMessage {
	params := StartParams{
		BridgeSessionID:  sess.SessionID,
		HarnessSessionID: sess.HarnessSessionID,
		DisplayName:      sess.DisplayName,
		AgentID:          sess.AgentID,
		CredentialID:     credentialID,
		Resume:           sess.HarnessSessionID != "",
	}
	if params.Resume && sess.DisplayName != "" && sess.DisplayName[0] == '/' {
		params.WorkDir = sess.DisplayName
	}
	if sess.ParentID != "" {
		params.Fork = sess.ParentID
	}

	data, _ := json.Marshal(params)

	// Merge harness-specific config into the start params so the harness
	// receives its own flags without the server needing to understand them.
	if len(sess.HarnessConfig) > 0 {
		var base map[string]json.RawMessage
		json.Unmarshal(data, &base)
		var extra map[string]json.RawMessage
		if json.Unmarshal(sess.HarnessConfig, &extra) == nil {
			for k, v := range extra {
				base[k] = v
			}
		}
		data, _ = json.Marshal(base)
	}

	return data
}

// Process represents a running harness subprocess.
type Process struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	sessionID string
	events    chan msg.Event
	done      chan struct{}
}

// StartProcess spawns a harness bridge subprocess.
// If credentialID is non-empty, it's passed to the subprocess via LLMBRIDGE_CREDENTIAL_ID env var.
func StartProcess(ctx context.Context, binPath string, sess *store.Session, credentialID string) (*Process, error) {
	cmd := exec.Command(binPath)
	cmd.Env = os.Environ()
	if credentialID != "" {
		cmd.Env = append(cmd.Env, "LLMBRIDGE_CREDENTIAL_ID="+credentialID)
	}
	cmd.Stderr = os.Stderr // Surface harness subprocess errors

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("start: %w", err)
	}

	p := &Process{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		sessionID: sess.SessionID,
		events:    make(chan msg.Event, 100),
		done:      make(chan struct{}),
	}

	params := buildStartParams(sess, credentialID)
	if err := p.sendRequest("start", params); err != nil {
		p.Kill()
		return nil, fmt.Errorf("send start: %w", err)
	}

	// Start reading stdout
	go p.readLoop()

	return p, nil
}

// PID returns the process ID.
func (p *Process) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// SessionID returns the session ID.
func (p *Process) SessionID() string {
	return p.sessionID
}

// Events returns the event channel.
func (p *Process) Events() <-chan msg.Event {
	return p.events
}

// Send writes a user message to the harness. Pass blocks=nil for text-only
// input; pass message="" with blocks for multimodal input.
func (p *Process) Send(message string, blocks []msg.ContentBlock) error {
	return p.sendRequest("message", MessageParams{
		BridgeSessionID: p.sessionID,
		Content:         message,
		Blocks:          blocks,
	})
}

// SendCommand sends a command (compact, resume, etc.).
func (p *Process) SendCommand(cmd string) error {
	return p.sendRequest(cmd, CommandParams{BridgeSessionID: p.sessionID})
}

// SendJSONRPC writes a JSON-RPC request with arbitrary params to the
// harness's stdin. params is a pre-marshalled JSON document; passing it as
// json.RawMessage to the marshaller preserves it byte-for-byte.
func (p *Process) SendJSONRPC(method string, params json.RawMessage) error {
	return p.sendRequest(method, params)
}

// Interrupt sends SIGINT to pause the harness.
func (p *Process) Interrupt() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd.Process == nil {
		return fmt.Errorf("process not running")
	}
	return p.cmd.Process.Signal(syscall.SIGINT)
}

// Kill terminates the process.
func (p *Process) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	close(p.done)

	if p.stdin != nil {
		p.stdin.Close()
	}
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	return p.cmd.Wait()
}

// sendRequest writes a JSON request to stdin.
func (p *Process) sendRequest(method string, params any) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	req := Request{Method: method}
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = data
	}

	data, err := json.Marshal(req)
	if err != nil {
		return err
	}

	data = append(data, '\n')
	_, err = p.stdin.Write(data)
	return err
}

// readLoop reads NDJSON events from stdout.
func (p *Process) readLoop() {
	defer close(p.events)

	scanner := bufio.NewScanner(p.stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB buffer

	for scanner.Scan() {
		select {
		case <-p.done:
			return
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event msg.Event
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}

		select {
		case p.events <- event:
		case <-p.done:
			return
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[harness] scanner error: %v", err)
	}
}

// PTYProcess is a HarnessProcess for sessions started in pty mode. It
// wraps a harness binary running inside a pseudoterminal so a remote
// attacher can read raw bytes and inject keystrokes via the WebSocket
// /sessions/{id}/attach endpoint. There are no msg.Events to translate
// — the upstream CLI's TUI is what's on the wire — so Events() returns
// a closed channel and the existing readEvents() loop is a no-op for
// these processes.
type PTYProcess struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	tty       *os.File // server side of the pty (read+write)
	sessionID string
	events    chan msg.Event
	done      chan struct{}
	exitErr   error
	exitOnce  sync.Once
}

// StartProcessPTY spawns a harness bridge subprocess inside a pty. The
// child sees LLMBRIDGE_PTY_MODE=1 in its environment; pty-capable
// harnesses are expected to detect that and exec into their upstream
// CLI so the pty fd is wired straight through to the user's TUI.
//
// On TransportLocal only — SSH/runner paths reject pty mode upstream.
func StartProcessPTY(ctx context.Context, binPath string, sess *store.Session, credentialID string) (*PTYProcess, error) {
	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(), "LLMBRIDGE_PTY_MODE=1")
	if credentialID != "" {
		cmd.Env = append(cmd.Env, "LLMBRIDGE_CREDENTIAL_ID="+credentialID)
	}
	if hid := sess.HarnessSessionID; hid != "" {
		// Resume hint passed via env so the pty-mode harness can pick
		// the right session without us shoehorning a JSON-RPC handshake
		// across the tty (where the user is going to be typing).
		cmd.Env = append(cmd.Env, "LLMBRIDGE_PTY_RESUME_ID="+hid)
	}

	// StartWithSize sets the pty dimensions before exec, so the child sees
	// a 24x80 terminal from byte zero. With the previous Start+Setsize
	// sequence the pty was 0x0 at exec time; TUIs like `claude` query
	// their tty size during startup and bail out immediately on a 0x0
	// terminal, which made the integration test flash green in 60ms with
	// "pty read: input/output error" and never actually run claude. The
	// follow-up resize control messages from attach (child 3) still work
	// the same way.
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		return nil, fmt.Errorf("pty start: %w", err)
	}

	closed := make(chan msg.Event)
	close(closed)

	p := &PTYProcess{
		cmd:       cmd,
		tty:       tty,
		sessionID: sess.SessionID,
		events:    closed,
		done:      make(chan struct{}),
	}

	go func() {
		err := cmd.Wait()
		p.exitOnce.Do(func() {
			p.exitErr = err
			tty.Close()
			close(p.done)
		})
	}()

	return p, nil
}

// PID returns the child process ID.
func (p *PTYProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// SessionID returns the bridge session ID.
func (p *PTYProcess) SessionID() string { return p.sessionID }

// Events satisfies HarnessProcess. PTY mode emits no canonical events;
// the channel is pre-closed so callers that range over it return
// immediately without blocking.
func (p *PTYProcess) Events() <-chan msg.Event { return p.events }

// Send is a no-op for pty sessions. Input flows through the attach
// WebSocket directly to the pty fd; there is no JSON-RPC channel.
func (p *PTYProcess) Send(message string, blocks []msg.ContentBlock) error {
	return fmt.Errorf("send not supported in pty mode; write to attach WebSocket instead")
}

// SendCommand is a no-op for pty sessions for the same reason as Send.
func (p *PTYProcess) SendCommand(cmd string) error {
	return fmt.Errorf("command %q not supported in pty mode", cmd)
}

// SendJSONRPC is unsupported in pty mode — the harness has no JSON-RPC
// stdin channel.
func (p *PTYProcess) SendJSONRPC(method string, params json.RawMessage) error {
	return fmt.Errorf("method %q not supported in pty mode", method)
}

// Interrupt sends SIGINT to the foreground process group of the pty,
// mirroring what Ctrl-C in a terminal would do.
func (p *PTYProcess) Interrupt() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd.Process == nil {
		return fmt.Errorf("process not running")
	}
	return p.cmd.Process.Signal(syscall.SIGINT)
}

// Kill terminates the pty session: closes the tty fd and SIGKILLs the
// child. Blocks until cmd.Wait completes (via the goroutine started in
// StartProcessPTY) so callers can rely on resources being released.
func (p *PTYProcess) Kill() error {
	p.mu.Lock()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	p.mu.Unlock()
	<-p.done
	return p.exitErr
}

// PTY returns the server-side pseudoterminal fd for read/write by the
// attach hub. Owned by the PTYProcess — do not close from the caller.
func (p *PTYProcess) PTY() *os.File { return p.tty }

// Done is closed when the underlying process exits.
func (p *PTYProcess) Done() <-chan struct{} { return p.done }

// StartSSHProcess spawns a harness bridge subprocess via SSH.
// args should be the full SSH arguments including target and remote command.
// credentialID is passed to the remote harness via the start params JSON.
func StartSSHProcess(ctx context.Context, args []string, sess *store.Session, credentialID string) (*Process, error) {
	cmd := exec.Command("ssh", args...)
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("start ssh: %w", err)
	}

	p := &Process{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		sessionID: sess.SessionID,
		events:    make(chan msg.Event, 100),
		done:      make(chan struct{}),
	}

	params := buildStartParams(sess, credentialID)
	if err := p.sendRequest("start", params); err != nil {
		p.Kill()
		return nil, fmt.Errorf("send start: %w", err)
	}

	go p.readLoop()

	return p, nil
}
