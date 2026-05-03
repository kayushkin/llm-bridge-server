package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// PingIntervalSecs is the app-level ping cadence advertised to runners in
// the Welcome message. The runner sends a WebSocket ping at this rate; the
// server's pong handler resets the read deadline to keep the conn alive.
const RunnerPingIntervalSecs = 30

// runnerWriteTimeout bounds how long a single WS write may block before
// the connection is considered dead.
const runnerWriteTimeout = 10 * time.Second

// runnerOutgoingBuffer is the per-connection write queue depth. Sized to
// absorb short network stalls; once full, message producers (RunnerProcess
// Send/Kill, etc.) return an error rather than block indefinitely.
const runnerOutgoingBuffer = 256

// RunnerRegistry tracks every llm-bridge-runner currently dialed in to
// this server. Indexed by Machine.Name (the canonical name resolved
// from the bearer token at handshake) so the harness manager can route
// Spawn requests to the right connection. Methods are safe for
// concurrent use.
type RunnerRegistry struct {
	mu    sync.RWMutex
	conns map[string]*RunnerConnection // machineName → connection
}

// NewRunnerRegistry constructs an empty registry.
func NewRunnerRegistry() *RunnerRegistry {
	return &RunnerRegistry{conns: make(map[string]*RunnerConnection)}
}

// Get returns the active connection for a machine name, or nil if no
// runner is currently registered under that name.
func (r *RunnerRegistry) Get(machineName string) *RunnerConnection {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conns[machineName]
}

// List returns a snapshot of all currently connected runners.
func (r *RunnerRegistry) List() []*RunnerConnection {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*RunnerConnection, 0, len(r.conns))
	for _, c := range r.conns {
		out = append(out, c)
	}
	return out
}

// register attaches a new RunnerConnection. If a previous connection
// existed under the same machine name (stale, not yet timed out), it is
// closed — last writer wins. The previous connection is returned for
// caller cleanup.
func (r *RunnerRegistry) register(c *RunnerConnection) *RunnerConnection {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.conns[c.MachineName]
	r.conns[c.MachineName] = c
	return prev
}

// deregister removes a connection. No-op if a newer one has taken its slot.
func (r *RunnerRegistry) deregister(c *RunnerConnection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[c.MachineName] == c {
		delete(r.conns, c.MachineName)
	}
}

// RunnerConnection represents one live runner WebSocket session. It owns
// its WS conn, runs a single writer goroutine, and dispatches incoming
// per-session messages to RunnerProcess instances.
type RunnerConnection struct {
	MachineName string
	MachineID   string           // resolved at accept time from the bearer token
	Hello       *msg.RunnerHello // received at handshake
	ConnectedAt time.Time

	conn     *websocket.Conn
	registry *RunnerRegistry
	outgoing chan *msg.RunnerMessage

	mu       sync.Mutex
	sessions map[string]*RunnerProcess // sessionID → RunnerProcess
	closed   bool
}

// MachineName getter for external use.
// (Field is exported but accessor reads better at call sites.)
func (c *RunnerConnection) Name() string { return c.MachineName }

// Send queues a message for the writer goroutine. Returns an error if
// the connection is closed or the outgoing buffer is full (i.e. the
// peer is too slow / stalled).
func (c *RunnerConnection) Send(m *msg.RunnerMessage) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("runner connection closed")
	}
	c.mu.Unlock()
	select {
	case c.outgoing <- m:
		return nil
	default:
		return fmt.Errorf("runner outgoing buffer full")
	}
}

// registerSession associates a RunnerProcess with this connection so
// incoming per-session messages can be routed to it.
func (c *RunnerConnection) registerSession(p *RunnerProcess) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions[p.sessionID] = p
}

// deregisterSession removes a session and closes its events channel.
func (c *RunnerConnection) deregisterSession(sessionID string) {
	c.mu.Lock()
	p := c.sessions[sessionID]
	delete(c.sessions, sessionID)
	c.mu.Unlock()
	if p != nil {
		p.closeEvents()
	}
}

// close shuts the connection down. All open sessions are torn down.
// Idempotent.
func (c *RunnerConnection) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	sessions := make([]*RunnerProcess, 0, len(c.sessions))
	for _, p := range c.sessions {
		sessions = append(sessions, p)
	}
	c.sessions = nil
	c.mu.Unlock()

	c.conn.Close()
	for _, p := range sessions {
		p.closeEvents()
	}
	c.registry.deregister(c)
}

// AcceptRunner wraps a freshly-upgraded WebSocket. The caller has already
// authenticated the bearer token against a Machine row; this method reads
// the Hello, sanity-checks it against the resolved machine, sends Welcome,
// and runs the read+write loop until the connection closes.
func (r *RunnerRegistry) AcceptRunner(ctx context.Context, conn *websocket.Conn, machine *msg.Machine, serverVersion string) error {
	conn.SetReadLimit(8 * 1024 * 1024) // 8MB per frame max

	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var hello msg.RunnerMessage
	if err := conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	if hello.Type != msg.RunnerMsgHello || hello.Hello == nil {
		_ = sendError(conn, "bad_hello", "expected hello message")
		return fmt.Errorf("expected hello, got %s", hello.Type)
	}

	rc := &RunnerConnection{
		MachineName: machine.Name,
		MachineID:   machine.ID,
		Hello:       hello.Hello,
		ConnectedAt: time.Now(),
		conn:        conn,
		registry:    r,
		outgoing:    make(chan *msg.RunnerMessage, runnerOutgoingBuffer),
		sessions:    make(map[string]*RunnerProcess),
	}

	if prev := r.register(rc); prev != nil {
		log.Printf("[runner-server] replacing stale connection for %s", rc.MachineName)
		prev.close()
	}

	welcome := &msg.RunnerMessage{
		Type: msg.RunnerMsgWelcome,
		Welcome: &msg.RunnerWelcome{
			MachineID:        machine.ID,
			MachineName:      machine.Name,
			ServerVersion:    serverVersion,
			PingIntervalSecs: RunnerPingIntervalSecs,
			AcceptedAt:       time.Now(),
		},
	}
	if err := writeRunnerJSON(conn, welcome); err != nil {
		rc.close()
		return fmt.Errorf("send welcome: %w", err)
	}

	deadline := time.Duration(RunnerPingIntervalSecs*3) * time.Second
	conn.SetReadDeadline(time.Now().Add(deadline))
	// Runner sends WS pings every PingIntervalSecs; server (this side)
	// auto-replies pong via the default control-frame handler. The
	// gorilla default Ping handler does NOT extend the read deadline,
	// so we override it: every incoming ping is also our liveness
	// signal. Without this, the server read times out 90s after
	// connection regardless of how many pings the runner sends.
	conn.SetPingHandler(func(message string) error {
		conn.SetReadDeadline(time.Now().Add(deadline))
		return conn.WriteControl(
			websocket.PongMessage,
			[]byte(message),
			time.Now().Add(runnerWriteTimeout),
		)
	})
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(deadline))
		return nil
	})

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go rc.writer(connCtx)
	err := rc.reader()
	cancel()
	rc.close()
	return err
}

// reader reads RunnerMessages until the connection dies. Per-session
// messages are dispatched to the matching RunnerProcess.
func (c *RunnerConnection) reader() error {
	for {
		var m msg.RunnerMessage
		if err := c.conn.ReadJSON(&m); err != nil {
			return fmt.Errorf("read: %w", err)
		}
		switch m.Type {
		case msg.RunnerMsgStdout:
			c.handleStdout(&m)
		case msg.RunnerMsgStderr:
			if m.Stderr != nil {
				log.Printf("[runner:%s/%s stderr] %s", c.MachineName, m.SessionID, m.Stderr.Data)
			}
		case msg.RunnerMsgExit:
			c.handleExit(&m)
		case msg.RunnerMsgPong:
			// Read deadline is reset by SetPongHandler.
		case msg.RunnerMsgPing:
			_ = c.Send(&msg.RunnerMessage{Type: msg.RunnerMsgPong})
		default:
			log.Printf("[runner-server] unexpected message from %s: %s", c.MachineName, m.Type)
		}
	}
}

// handleStdout parses a runner stdout line as msg.Event and forwards it
// to the matching RunnerProcess events channel.
func (c *RunnerConnection) handleStdout(m *msg.RunnerMessage) {
	if m.Stdout == nil || m.SessionID == "" {
		return
	}
	c.mu.Lock()
	p := c.sessions[m.SessionID]
	c.mu.Unlock()
	if p == nil {
		return
	}
	var ev msg.Event
	if err := json.Unmarshal([]byte(m.Stdout.Data), &ev); err != nil {
		log.Printf("[runner-server] %s/%s: parse stdout: %v (line=%q)", c.MachineName, m.SessionID, err, m.Stdout.Data)
		return
	}
	select {
	case p.events <- ev:
	default:
		// Drop on a full event channel — the manager's readEvents loop
		// owns it and is expected to drain promptly. If it can't, the
		// session is wedged and dropping is the lesser evil.
		log.Printf("[runner-server] %s/%s: events buffer full, dropping event", c.MachineName, m.SessionID)
	}
}

// handleExit closes the matching session's events channel.
func (c *RunnerConnection) handleExit(m *msg.RunnerMessage) {
	if m.SessionID == "" {
		return
	}
	if m.Exit != nil && m.Exit.Error != "" {
		log.Printf("[runner-server] %s/%s exited: code=%d err=%s", c.MachineName, m.SessionID, m.Exit.ExitCode, m.Exit.Error)
	}
	c.deregisterSession(m.SessionID)
}

// writer drains outgoing onto the WS conn until the conn dies or ctx
// cancels.
func (c *RunnerConnection) writer(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-c.outgoing:
			if err := writeRunnerJSON(c.conn, m); err != nil {
				log.Printf("[runner-server] %s: write failed: %v", c.MachineName, err)
				return
			}
		}
	}
}

// writeRunnerJSON marshals m and writes it as a single text frame. Used
// by both the registry (handshake) and the per-connection writer.
func writeRunnerJSON(conn *websocket.Conn, m *msg.RunnerMessage) error {
	conn.SetWriteDeadline(time.Now().Add(runnerWriteTimeout))
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

// sendError sends a RunnerError message and is best-effort — used during
// handshake rejection just before closing.
func sendError(conn *websocket.Conn, code, message string) error {
	return writeRunnerJSON(conn, &msg.RunnerMessage{
		Type: msg.RunnerMsgError,
		Err:  &msg.RunnerError{Code: code, Message: message},
	})
}

// RunnerProcess satisfies HarnessProcess for sessions backed by a remote
// runner. Send/SendCommand/Interrupt/Kill all translate to messages on
// the underlying RunnerConnection's outgoing channel; Events are pushed
// to it by the connection's reader goroutine.
type RunnerProcess struct {
	sessionID string
	conn      *RunnerConnection
	events    chan msg.Event
	closeOnce sync.Once
}

// PID returns 0 — the local PID lives on the runner machine and isn't
// reported back. Callers use this only for cosmetic display; 0 is fine.
func (p *RunnerProcess) PID() int { return 0 }

// SessionID returns the bridge session identifier this process serves.
func (p *RunnerProcess) SessionID() string { return p.sessionID }

// Events returns the channel of events streamed back from the remote
// harness. Closed when the remote subprocess exits or the runner
// disconnects.
func (p *RunnerProcess) Events() <-chan msg.Event { return p.events }

// Send writes a user message to the remote harness via the runner. Pass
// blocks=nil for text-only input; pass message="" with blocks for multimodal
// input.
func (p *RunnerProcess) Send(message string, blocks []msg.ContentBlock) error {
	return p.sendRequest("message", MessageParams{Content: message, Blocks: blocks})
}

// SendCommand sends a control command (compact, resume, etc.).
func (p *RunnerProcess) SendCommand(cmd string) error {
	return p.sendRequest(cmd, nil)
}

// SendJSONRPC routes a generic JSON-RPC request through the runner WS to
// the remote harness's stdin. params is preserved byte-for-byte by
// json.RawMessage's marshaller.
func (p *RunnerProcess) SendJSONRPC(method string, params json.RawMessage) error {
	return p.sendRequest(method, params)
}

// Interrupt asks the runner to deliver SIGINT to the subprocess.
func (p *RunnerProcess) Interrupt() error {
	return p.conn.Send(&msg.RunnerMessage{
		Type:      msg.RunnerMsgSignal,
		SessionID: p.sessionID,
		Signal:    &msg.RunnerSignal{Signal: msg.RunnerSignalInterrupt},
	})
}

// Kill asks the runner to terminate the subprocess and tears down the
// local events channel. The Exit message coming back from the runner
// will trigger deregisterSession, closing the channel through the
// reader path; this Kill also closes early so callers don't block if
// the runner is unresponsive.
func (p *RunnerProcess) Kill() error {
	err := p.conn.Send(&msg.RunnerMessage{
		Type:      msg.RunnerMsgSignal,
		SessionID: p.sessionID,
		Signal:    &msg.RunnerSignal{Signal: msg.RunnerSignalKill},
	})
	p.conn.deregisterSession(p.sessionID)
	return err
}

// sendRequest builds a JSON-RPC frame matching the harness's stdin
// protocol and ships it as a Stdin runner message.
func (p *RunnerProcess) sendRequest(method string, params any) error {
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
	return p.conn.Send(&msg.RunnerMessage{
		Type:      msg.RunnerMsgStdin,
		SessionID: p.sessionID,
		Stdin:     &msg.RunnerStdin{Data: string(data)},
	})
}

// closeEvents closes the events channel exactly once, releasing the
// reader in the harness manager.
func (p *RunnerProcess) closeEvents() {
	p.closeOnce.Do(func() { close(p.events) })
}

// startRunner asks the runner registered for inst.Machine.Name to spawn
// the session's harness binary. Returns a HarnessProcess wired into the
// connection so subsequent Send/Kill calls flow through the WS.
//
// Spawn carries Provision — the deployment manifest for service-style
// harnesses (e.g. inber-server). The runner ensures every Provision
// service is installed and running before forking the wrapper, so an
// instance on a remote machine actually deploys the harness's stack
// there rather than expecting it to already exist.
func (m *Manager) startRunner(ctx context.Context, sess *store.Session, inst *msg.Instance, credentialID string) (HarnessProcess, error) {
	if m.runners == nil {
		return nil, fmt.Errorf("runner registry not configured")
	}
	if inst.Machine == nil {
		return nil, fmt.Errorf("instance %s missing Machine for runner spawn", inst.ID)
	}
	conn := m.runners.Get(inst.Machine.Name)
	if conn == nil {
		return nil, fmt.Errorf("no runner connected for machine %q", inst.Machine.Name)
	}

	params := buildStartParams(sess, credentialID)
	workDir := inst.WorkingDir
	if workDir == "" {
		workDir = inst.Machine.DefaultWorkingDir
	}

	publicURL := m.publicServerURL
	if publicURL == "" {
		// Empty PublicURL falls back to whatever the runner connected
		// to. For SSH-tunneled deployments the runner's server_url is
		// http://localhost:<port>, which is reachable from the runner
		// (it's the same tunnel) but not from anywhere else — fine
		// here since the runner is the only consumer of these URLs.
		publicURL = "http://localhost:8160"
	}
	reason := "session:" + sess.BridgeID
	mctx := ManifestContext{
		ServerURL:  publicURL,
		OS:         conn.Hello.OS,
		Arch:       conn.Hello.Arch,
		Credential: resolveAuthCredential(m.authClient, credentialID, reason),
		AuthClient: m.authClient,
		Reason:     reason,
	}
	provision, err := BuildProvision(msg.Harness(sess.Harness), mctx)
	if err != nil {
		// Fail fast — an instance whose harness needs deployment but
		// can't produce a valid manifest (missing credential, etc.)
		// would silently fall back to "wrapper crashes on connect"
		// otherwise.
		return nil, fmt.Errorf("build provision manifest: %w", err)
	}

	rp := &RunnerProcess{
		sessionID: sess.BridgeID,
		conn:      conn,
		events:    make(chan msg.Event, 100),
	}
	conn.registerSession(rp)

	spawn := &msg.RunnerMessage{
		Type:      msg.RunnerMsgSpawn,
		SessionID: sess.BridgeID,
		Spawn: &msg.RunnerSpawn{
			Harness:     msg.Harness(sess.Harness),
			WorkingDir:  workDir,
			StartParams: params,
			Provision:   provision,
		},
	}
	if err := conn.Send(spawn); err != nil {
		conn.deregisterSession(sess.BridgeID)
		return nil, fmt.Errorf("send spawn: %w", err)
	}

	return rp, nil
}
