package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"syscall"

	"github.com/kayushkin/llm-bridge/msg"
)

// Subprocess wraps a single harness binary spawned for a session. The
// runner owns one Subprocess per active session_id; multiple Subprocesses
// share the same upstream WebSocket via the outgoing channel.
type Subprocess struct {
	sessionID string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	writeMu   sync.Mutex // serializes writes to stdin
}

// Pid returns the subprocess PID, or 0 if it isn't running yet.
func (s *Subprocess) Pid() int {
	if s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// WriteLine writes a single line to the subprocess stdin. The runner
// preserves the canonical NDJSON contract: data is the line content
// without trailing newline, the runner appends "\n".
func (s *Subprocess) WriteLine(data string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.stdin.Write([]byte(data + "\n")); err != nil {
		return fmt.Errorf("subprocess %s stdin: %w", s.sessionID, err)
	}
	return nil
}

// Signal delivers an OS signal mapped from the wire signal type. Returns
// an error if the process is no longer running.
func (s *Subprocess) Signal(sig msg.RunnerSignalType) error {
	if s.cmd.Process == nil {
		return fmt.Errorf("subprocess %s not running", s.sessionID)
	}
	switch sig {
	case msg.RunnerSignalInterrupt:
		return s.cmd.Process.Signal(syscall.SIGINT)
	case msg.RunnerSignalKill:
		return s.cmd.Process.Kill()
	default:
		return fmt.Errorf("unknown signal: %s", sig)
	}
}

// SubprocessRegistry tracks active subprocesses by session ID. Methods are
// safe for concurrent use.
type SubprocessRegistry struct {
	mu    sync.Mutex
	procs map[string]*Subprocess
	// startingStdin holds, per session whose Spawn is still running, the
	// stdin lines that arrived before its process did. The bridge sends a
	// session's first input right behind its Spawn, and Spawn runs off the
	// read loop, so the input can win: the runner used to drop it ("stdin for
	// unknown session") and the harness then waited forever for a prompt
	// (br_1792016658336160620, 2026-10-14).
	startingStdin     map[string][]string
	outgoing          chan<- *msg.RunnerMessage // single shared sink to the WS writer
	installCurrentBin func(harness msg.Harness) (string, error)
	serverURL         string // propagated to subprocesses as LLMBRIDGE_SERVER
	services          *ServiceRegistry
}

// NewSubprocessRegistry constructs a registry that ships subprocess output
// onto outgoing. installCurrentBin is invoked when a Spawn request names a
// harness whose wrapper is not on PATH, or whose wrapper on PATH is the copy
// the runner itself installed under ~/.local/bin, so that copy follows the
// bridge's; the returned absolute path is used for the exec. A wrapper
// installed anywhere else is the machine owner's and is left alone. nil
// disables both (Spawn fails fast on a missing wrapper).
//
// services owns long-lived backend daemons declared in spawn manifests
// (e.g. inber-server). Provided at construction so Spawn can ensure
// those backends are running before forking the wrapper.
//
// serverURL is exported to every spawned subprocess as LLMBRIDGE_SERVER so
// wrappers can refer back to the bridge for any side-channel needs.
func NewSubprocessRegistry(outgoing chan<- *msg.RunnerMessage, installCurrentBin func(harness msg.Harness) (string, error), serverURL string, services *ServiceRegistry) *SubprocessRegistry {
	return &SubprocessRegistry{
		procs:             make(map[string]*Subprocess),
		startingStdin:     make(map[string][]string),
		outgoing:          outgoing,
		installCurrentBin: installCurrentBin,
		serverURL:         serverURL,
		services:          services,
	}
}

// Get returns the Subprocess for a session or nil if none.
// BeginSpawn marks a session as starting, so DeliverStdin holds its input
// until Spawn has a process to give it to. Call it on the read loop, before
// handing the Spawn to a goroutine.
func (r *SubprocessRegistry) BeginSpawn(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, running := r.procs[sessionID]; !running {
		r.startingStdin[sessionID] = []string{}
	}
}

// AbandonSpawn forgets a session whose Spawn failed, with any input held for it.
func (r *SubprocessRegistry) AbandonSpawn(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.startingStdin, sessionID)
}

// DeliverStdin writes a line to the session's process, or holds it while
// the session is starting. It reports false for a session that is neither.
func (r *SubprocessRegistry) DeliverStdin(sessionID, line string) (bool, error) {
	r.mu.Lock()
	if held, starting := r.startingStdin[sessionID]; starting {
		r.startingStdin[sessionID] = append(held, line)
		r.mu.Unlock()
		return true, nil
	}
	sp := r.procs[sessionID]
	r.mu.Unlock()
	if sp == nil {
		return false, nil
	}
	return true, sp.WriteLine(line)
}

func (r *SubprocessRegistry) Get(sessionID string) *Subprocess {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.procs[sessionID]
}

// Spawn forks a new harness subprocess for the given Spawn request and
// starts the I/O pump goroutines. Returns an error if the process cannot
// be started; on success the caller can immediately Send Stdin/Signal
// against the registry.
//
// On any subprocess exit (clean or error) the registry sends an Exit
// message upstream and removes the entry. Stdout/Stderr lines are
// streamed as RunnerMsgStdout / RunnerMsgStderr events.
func (r *SubprocessRegistry) Spawn(sessionID string, spawn *msg.RunnerSpawn) error {
	r.mu.Lock()
	if _, exists := r.procs[sessionID]; exists {
		r.mu.Unlock()
		return fmt.Errorf("subprocess already exists for session %s", sessionID)
	}
	r.mu.Unlock()

	// Ensure every long-lived backend the harness needs is running on
	// this host before we fork the wrapper. The bridge populates
	// Provision based on the harness's deployment manifest.
	if r.services != nil {
		for _, svc := range spawn.Provision {
			if err := r.services.Ensure(svc); err != nil {
				return fmt.Errorf("provision %s: %w", svc.Name, err)
			}
		}
	}

	// A session given computer use gets the tool as it is on the bridge now.
	if spawn.ComputerUse {
		if _, err := installCurrentComputerUseTool(r.serverURL); err != nil {
			log.Printf("[runner] WARNING: could not update the computer-use tool for session %s; it runs the copy on disk: %v", sessionID, err)
		}
	}

	binPath := spawn.BinaryPath
	if binPath == "" {
		bin := msg.HarnessBinaryName(spawn.Harness)
		if bin == "" {
			return fmt.Errorf("unknown harness type: %s", spawn.Harness)
		}
		resolved, lookErr := exec.LookPath(bin)
		managed, managedErr := harnessBinaryPath(spawn.Harness)
		runnerInstalled := lookErr == nil && managedErr == nil && resolved == managed
		if lookErr != nil || runnerInstalled {
			if r.installCurrentBin == nil {
				if lookErr != nil {
					return fmt.Errorf("binary not found on PATH: %s (no fetch hook configured)", bin)
				}
			} else {
				installed, err := r.installCurrentBin(spawn.Harness)
				switch {
				case err == nil:
					resolved = installed
				case lookErr != nil:
					return fmt.Errorf("binary not found on PATH: %s; auto-fetch failed: %w", bin, err)
				default:
					// The copy on disk still runs; say loudly that it may be stale.
					log.Printf("[runner] WARNING: could not check %s against the bridge, running the copy on disk, which may be out of date: %v", resolved, err)
				}
			}
		}
		binPath = resolved
	}

	cmd := exec.Command(binPath)
	if spawn.WorkingDir != "" {
		cmd.Dir = spawn.WorkingDir
	}
	// Always propagate LLMBRIDGE_SERVER so wrappers can fall back to the
	// bridge's harness-proxy for service-style harnesses (inber, hermes).
	// Spawn-supplied Env wins for everything else.
	cmd.Env = append(cmd.Environ(), "LLMBRIDGE_SERVER="+r.serverURL)
	if len(spawn.Env) > 0 {
		cmd.Env = append(cmd.Env, spawn.Env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		return fmt.Errorf("start %s: %w", binPath, err)
	}

	sp := &Subprocess{
		sessionID: sessionID,
		cmd:       cmd,
		stdin:     stdin,
	}

	// Send the harness's start params as the first stdin line. This mirrors
	// what llm-bridge-server's local subprocess transport does — the harness
	// expects a JSON-RPC "start" request to initialize the session.
	startReq := fmt.Sprintf(`{"method":"start","params":%s}`, string(spawn.StartParams))
	if err := sp.WriteLine(startReq); err != nil {
		cmd.Process.Kill()
		return fmt.Errorf("send start: %w", err)
	}

	// Input that arrived while the process was starting goes in first, after
	// the start line and before anything later; registering under the same
	// lock keeps that order.
	r.mu.Lock()
	for _, line := range r.startingStdin[sessionID] {
		if err := sp.WriteLine(line); err != nil {
			delete(r.startingStdin, sessionID)
			r.mu.Unlock()
			cmd.Process.Kill()
			return fmt.Errorf("deliver input that arrived during the spawn: %w", err)
		}
	}
	delete(r.startingStdin, sessionID)
	r.procs[sessionID] = sp
	r.mu.Unlock()

	log.Printf("[runner] spawned %s (session=%s pid=%d)", binPath, sessionID, sp.Pid())

	go r.pumpStdout(sessionID, stdout)
	go r.pumpStderr(sessionID, stderr)
	go r.waitExit(sessionID, sp)

	return nil
}

// pumpStdout reads NDJSON lines from a subprocess stdout and ships each
// upstream as a Stdout RunnerMessage. Exits when the pipe closes.
func (r *SubprocessRegistry) pumpStdout(sessionID string, stdout io.ReadCloser) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024) // up to 4MB lines
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		r.outgoing <- &msg.RunnerMessage{
			Type:      msg.RunnerMsgStdout,
			SessionID: sessionID,
			Stdout:    &msg.RunnerStdout{Data: line},
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[runner] stdout scanner error (session=%s): %v", sessionID, err)
	}
}

// pumpStderr ships subprocess stderr lines upstream, line by line, for
// server-side logging. Not parsed.
func (r *SubprocessRegistry) pumpStderr(sessionID string, stderr io.ReadCloser) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		r.outgoing <- &msg.RunnerMessage{
			Type:      msg.RunnerMsgStderr,
			SessionID: sessionID,
			Stderr:    &msg.RunnerStderr{Data: line},
		}
	}
}

// waitExit blocks until the subprocess exits, then sends an Exit message
// and deregisters the subprocess. Always runs exactly once per Spawn.
func (r *SubprocessRegistry) waitExit(sessionID string, sp *Subprocess) {
	err := sp.cmd.Wait()
	exitCode := 0
	errStr := ""
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			errStr = err.Error()
		}
	}
	r.mu.Lock()
	delete(r.procs, sessionID)
	r.mu.Unlock()
	log.Printf("[runner] exit session=%s code=%d err=%q", sessionID, exitCode, errStr)
	r.outgoing <- &msg.RunnerMessage{
		Type:      msg.RunnerMsgExit,
		SessionID: sessionID,
		Exit:      &msg.RunnerExit{ExitCode: exitCode, Error: errStr},
	}
}

// KillAll terminates every active subprocess. Used on shutdown / WS
// disconnect when we cannot deliver further messages to or from the
// server. Does not send Exit messages — by the time KillAll is called,
// the WS is gone.
func (r *SubprocessRegistry) KillAll() {
	r.mu.Lock()
	procs := make([]*Subprocess, 0, len(r.procs))
	for _, sp := range r.procs {
		procs = append(procs, sp)
	}
	r.mu.Unlock()
	for _, sp := range procs {
		if sp.cmd.Process != nil {
			sp.cmd.Process.Kill()
		}
	}
}
