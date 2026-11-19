package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kayushkin/llm-bridge/machinestats"
	"github.com/kayushkin/llm-bridge/msg"
)

// seedTickInterval is the runner-side backstop polling cadence. The
// WS-driven path (SeedSnapshot from the server) fires sooner when changes
// land; this guarantees forward progress even if a snapshot was missed
// while disconnected.
const seedTickInterval = 5 * time.Minute

// Version is set at build time via -ldflags "-X main.Version=…".
var Version = "dev"

// Defaults for connection lifecycle.
const (
	dialTimeout         = 10 * time.Second
	welcomeTimeout      = 10 * time.Second
	defaultPingInterval = 30 * time.Second
	pongTimeout         = 90 * time.Second
	writeTimeout        = 10 * time.Second
	outgoingBuffer      = 256

	// Reconnect backoff envelope: starts at minBackoff, doubles to maxBackoff.
	minBackoff = 1 * time.Second
	maxBackoff = 60 * time.Second
)

// Client is a long-lived runner that maintains a single WebSocket connection
// to llm-bridge-server. On disconnect it kills all in-flight subprocesses
// and reconnects with exponential backoff.
type Client struct {
	cfg           *Config
	outgoing      chan *msg.RunnerMessage
	registry      *SubprocessRegistry
	services      *ServiceRegistry
	oauthRecovery *ClaudeOAuthRecovery

	// machineStatsCollector keeps CPU counters between readings, so it lives
	// across connections.
	machineStatsCollector *machinestats.Collector

	// Seeded context state. machineID is set once per Welcome and reused
	// across reconciliations; reconcilers run in the background for each
	// configured seed source (agent-store, skill-store).
	machineMu   sync.Mutex
	machineID   string
	reconcilers map[msg.SeedSource]*SeedReconciler
}

// NewClient constructs a runner client. The returned Client must be Run.
func NewClient(cfg *Config) *Client {
	outgoing := make(chan *msg.RunnerMessage, outgoingBuffer)
	installCurrent := func(h msg.Harness) (string, error) {
		return installCurrentHarnessBinary(cfg.ServerURL, h)
	}
	services := NewServiceRegistry()
	c := &Client{
		cfg:           cfg,
		outgoing:      outgoing,
		services:      services,
		registry:      NewSubprocessRegistry(outgoing, installCurrent, cfg.ServerURL, services),
		oauthRecovery: NewClaudeOAuthRecovery(outgoing),
		reconcilers:   make(map[msg.SeedSource]*SeedReconciler),

		machineStatsCollector: machinestats.NewCollector(),
	}
	c.initReconcilers()
	return c
}

// initReconcilers spins up one reconciler per seed source. They run for the
// life of the process; the Client just wakes them via Trigger when triggers
// arrive (Welcome, SeedSnapshot WS message). The reconciler internally backs
// off when machineID is unset, so starting before Welcome is safe.
func (c *Client) initReconcilers() {
	for _, source := range []msg.SeedSource{
		msg.SeedSourceAgentStore,
		msg.SeedSourceSkillStore,
	} {
		rec, err := NewSeedReconciler(source, c.cfg, c.currentMachineID)
		if err != nil {
			log.Printf("[seed] init %s reconciler: %v", source, err)
			continue
		}
		c.reconcilers[source] = rec
		go rec.Run(make(chan struct{}), seedTickInterval)
	}
}

// currentMachineID returns the machine_id assigned by the most recent
// Welcome, or "" if we haven't connected yet.
func (c *Client) currentMachineID() string {
	c.machineMu.Lock()
	defer c.machineMu.Unlock()
	return c.machineID
}

func (c *Client) setMachineID(id string) {
	c.machineMu.Lock()
	c.machineID = id
	c.machineMu.Unlock()
}

// triggerSeed pokes one reconciler (specific source) or all of them
// (zero-value source).
func (c *Client) triggerSeed(source msg.SeedSource) {
	if source == "" {
		for _, r := range c.reconcilers {
			r.Trigger()
		}
		return
	}
	if r := c.reconcilers[source]; r != nil {
		r.Trigger()
	}
}

// Run drives the connect-and-serve loop until the context is canceled.
// Each iteration attempts one connection; on disconnect, in-flight
// subprocesses are killed and the loop redials after a backoff.
//
// Teardown is deferred rather than written at each return, because Run has
// two of them and they used to disagree: canceling during the reconnect
// backoff — where the runner spends its whole life whenever the server is
// unreachable — returned without killing the managed services, orphaning
// every one of them. A defer makes the teardown a property of leaving Run
// at all, so a future third return path cannot reintroduce the leak.
func (c *Client) Run(ctx context.Context) {
	defer func() {
		log.Printf("[runner] shutting down: %v", ctx.Err())
		c.registry.KillAll()
		c.services.KillAll()
	}()

	backoff := minBackoff
	for {
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errRunnerReplaced) {
			log.Printf("[runner] %v; stopping so the service manager starts it", err)
			return
		}
		log.Printf("[runner] disconnected: %v", err)
		c.registry.KillAll()

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runOnce establishes one WebSocket connection, performs the Hello/Welcome
// handshake, then runs the read+write loops until the connection dies or
// the context is canceled. Returns the disconnect cause (nil if canceled).
func (c *Client) runOnce(ctx context.Context) error {
	wsURL, err := buildWSURL(c.cfg.ServerURL)
	if err != nil {
		return fmt.Errorf("build ws url: %w", err)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: dialTimeout,
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.cfg.RunnerToken)

	// Self-update first, while no session is running.
	if replaced, err := installCurrentRunner(c.cfg.ServerURL); err != nil {
		log.Printf("[runner] WARNING: could not check this runner against the bridge's build; running this one: %v", err)
	} else if replaced {
		return errRunnerReplaced
	}

	computerUse, err := installCurrentComputerUseTool(c.cfg.ServerURL)
	if err != nil {
		log.Printf("[runner] ERROR: computer use will not be offered from this machine: %v", err)
	} else if computerUse != nil {
		log.Printf("[runner] computer-use tool %s (%s)", computerUse.Command, computerUse.Version)
	}

	log.Printf("[runner] dialing %s", wsURL)
	conn, _, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	hello := &msg.RunnerMessage{
		Type: msg.RunnerMsgHello,
		Hello: &msg.RunnerHello{
			Hostname:           c.cfg.Hostname,
			OS:                 c.cfg.OS,
			Arch:               c.cfg.Arch,
			User:               c.cfg.User,
			WorkingDir:         c.cfg.WorkingDir,
			AvailableHarnesses: DetectHarnesses(),
			RunnerVersion:      Version,
			ComputerUse:        computerUse,
		},
	}
	if err := writeJSON(conn, hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(welcomeTimeout))
	var welcome msg.RunnerMessage
	if err := conn.ReadJSON(&welcome); err != nil {
		return fmt.Errorf("read welcome: %w", err)
	}
	switch welcome.Type {
	case msg.RunnerMsgWelcome:
		if welcome.Welcome == nil {
			return fmt.Errorf("welcome payload missing")
		}
		log.Printf("[runner] welcomed: machine_id=%s server=%s",
			welcome.Welcome.MachineID, welcome.Welcome.ServerVersion)
		// Sync local saved config if the server's canonical machine name
		// has drifted from ours (e.g. user renamed the machine in the
		// bridge UI after enrollment). Best-effort: a write failure
		// just means we'll re-detect on the next reconnect.
		if name := welcome.Welcome.MachineName; name != "" && name != c.cfg.MachineName {
			log.Printf("[runner] machine_name updated by server: %q → %q", c.cfg.MachineName, name)
			c.cfg.MachineName = name
			if saved, err := LoadSavedConfig(); err == nil {
				saved.MachineName = name
				_ = SaveConfig(saved)
			}
		}
		// Connect-time reconcile: pull every source so a fresh runner lands
		// on the bridge's canonical context before any harness spawns.
		c.setMachineID(welcome.Welcome.MachineID)
		c.triggerSeed("")
	case msg.RunnerMsgError:
		if welcome.Err != nil {
			return fmt.Errorf("server rejected hello: %s — %s",
				welcome.Err.Code, welcome.Err.Message)
		}
		return fmt.Errorf("server rejected hello (no detail)")
	default:
		return fmt.Errorf("expected welcome, got %s", welcome.Type)
	}

	pingInterval := defaultPingInterval
	if welcome.Welcome.PingIntervalSecs > 0 {
		pingInterval = time.Duration(welcome.Welcome.PingIntervalSecs) * time.Second
	}

	conn.SetReadDeadline(time.Now().Add(pongTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongTimeout))
		return nil
	})

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// One goroutine owns ALL writes — outgoing messages and the periodic
	// ping. gorilla/websocket connections aren't safe for concurrent
	// writes; running ping on a separate goroutine corrupts frames and
	// the server reads garbage until its read deadline expires.
	writeErr := make(chan error, 1)
	go c.writer(connCtx, conn, pingInterval, writeErr)
	go c.sendMachineStatsWhileConnected(connCtx)

	readErr := c.reader(conn)

	cancel()
	select {
	case <-writeErr:
	case <-time.After(writeTimeout):
	}

	return readErr
}

// reader reads RunnerMessages from the conn and dispatches them. Returns
// when the conn errors or closes. Server → runner messages: Spawn, Stdin,
// Signal, Ping, Pong (echoed). Anything unexpected is logged and ignored.
func (c *Client) reader(conn *websocket.Conn) error {
	for {
		var m msg.RunnerMessage
		if err := conn.ReadJSON(&m); err != nil {
			return fmt.Errorf("read: %w", err)
		}
		switch m.Type {
		case msg.RunnerMsgSpawn:
			if m.Spawn == nil || m.SessionID == "" {
				log.Printf("[runner] invalid spawn: missing fields")
				continue
			}
			// Spawn off the read loop. Provisioning service-style backends
			// can take tens of seconds (downloading the binary, waiting
			// for health), and blocking the reader starves WS pongs —
			// the connection then dies on the server-side read deadline
			// before we can finish setup.
			// Marked as starting here, on the read loop, so stdin that
			// arrives before the process exists is held, not dropped.
			c.registry.BeginSpawn(m.SessionID)
			go func(sessionID string, spawn *msg.RunnerSpawn) {
				if err := c.registry.Spawn(sessionID, spawn); err != nil {
					c.registry.AbandonSpawn(sessionID)
					log.Printf("[runner] spawn failed (session=%s): %v", sessionID, err)
					c.outgoing <- &msg.RunnerMessage{
						Type:      msg.RunnerMsgExit,
						SessionID: sessionID,
						Exit:      &msg.RunnerExit{ExitCode: -1, Error: err.Error()},
					}
				}
			}(m.SessionID, m.Spawn)
		case msg.RunnerMsgStdin:
			if m.Stdin == nil || m.SessionID == "" {
				continue
			}
			known, err := c.registry.DeliverStdin(m.SessionID, m.Stdin.Data)
			if !known {
				log.Printf("[runner] stdin for unknown session: %s", m.SessionID)
				continue
			}
			if err != nil {
				log.Printf("[runner] stdin write failed (session=%s): %v", m.SessionID, err)
			}
		case msg.RunnerMsgSignal:
			if m.Signal == nil || m.SessionID == "" {
				continue
			}
			sp := c.registry.Get(m.SessionID)
			if sp == nil {
				continue
			}
			if err := sp.Signal(m.Signal.Signal); err != nil {
				log.Printf("[runner] signal failed (session=%s): %v", m.SessionID, err)
			}
		case msg.RunnerMsgPing:
			c.outgoing <- &msg.RunnerMessage{Type: msg.RunnerMsgPong}
		case msg.RunnerMsgPong:
			// Pongs reset the read deadline via SetPongHandler.
		case msg.RunnerMsgSeedSnapshot:
			if m.SeedSnapshot == nil {
				continue
			}
			log.Printf("[runner] seed snapshot: source=%s reason=%s",
				m.SeedSnapshot.Source, m.SeedSnapshot.Reason)
			c.triggerSeed(m.SeedSnapshot.Source)
		case msg.RunnerMsgSeedDelta:
			// Per-file deltas are not used in v1 — the snapshot trigger
			// drives a full reconcile and the manifest endpoint is the
			// source of truth for what to apply. Ignore but log for debug.
			log.Printf("[runner] seed_delta received (unused in v1)")
		case msg.RunnerMsgClaudeOAuthRecovery:
			if m.ClaudeOAuthRecovery == nil {
				log.Printf("[runner] invalid Claude OAuth recovery message")
				continue
			}
			switch m.ClaudeOAuthRecovery.Action {
			case "start":
				go c.oauthRecovery.Start(m.ClaudeOAuthRecovery.RecoveryID)
			case "code":
				if err := c.oauthRecovery.SubmitCode(m.ClaudeOAuthRecovery.RecoveryID, m.ClaudeOAuthRecovery.Code); err != nil {
					c.oauthRecovery.Failed(m.ClaudeOAuthRecovery.RecoveryID, err)
				}
			default:
				log.Printf("[runner] invalid Claude OAuth recovery action: %s", m.ClaudeOAuthRecovery.Action)
			}
		default:
			log.Printf("[runner] unexpected message type: %s", m.Type)
		}
	}
}

// writer is the single owner of all WS writes for this connection.
// Drains outgoing JSON messages and emits a WebSocket-level ping at
// the negotiated cadence; multiplexing both into one goroutine
// guarantees serialized writes (gorilla/websocket isn't safe for
// concurrent writes). Reports its terminal error on errCh.
func (c *Client) writer(ctx context.Context, conn *websocket.Conn, pingInterval time.Duration, errCh chan<- error) {
	defer close(errCh)
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-c.outgoing:
			if err := writeJSON(conn, m); err != nil {
				errCh <- err
				return
			}
		case <-pingTicker.C:
			conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				errCh <- err
				return
			}
		}
	}
}

// writeJSON marshals m and writes it as a single text frame with the
// configured deadline.
func writeJSON(conn *websocket.Conn, m *msg.RunnerMessage) error {
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

// buildWSURL converts an http(s)://host:port base into the runner WS URL
// at /api/runner/ws. Accepts ws://, wss://, http://, https://.
func buildWSURL(serverURL string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/runner/ws"
	return u.String(), nil
}
