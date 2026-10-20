package harness

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/childprocessenv"
	"github.com/kayushkin/llm-bridge/msg"
)

// ptySidecar is a long-running co-process spawned alongside a PTY session to
// turn what the CLI does in its own screen into events. PTY-mode sessions
// can't do that in-process because the harness wrapper exec's straight into
// the CLI — there's no Go process left. Each sidecar posts translated
// msg.Events to {localBridgeURL}/sidecar/event/{bridge_id}, and runs until
// its stdin is closed (which the manager does when the PTY session ends).
//
// Two kinds, by harness (ptySidecarStarters):
//   - Claude Code, `llm-bridge-claudecode -otel-sidecar` (startOTelSidecar):
//     an OTLP receiver for claude's telemetry plus a tailer of its session
//     file. It prints its receiver URL on stdout first, and the PTY child
//     gets OTel env vars pointing at it.
//   - Codex, `llm-bridge-codex -pty-sidecar` (startRolloutSidecar): a tailer
//     of codex's rollout file, with nothing for the PTY child to know.
type ptySidecar struct {
	cmd         *exec.Cmd
	endpointURL string
	stdin       io.WriteCloser
}

// startOTelSidecar spawns the sidecar process, reads its receiver URL
// from stdout, and returns the OTel env-var bundle to inject on the PTY
// child plus a handle for lifecycle management.
//
// Returns (nil, nil) when the sidecar binary doesn't support the mode
// (e.g. an older build) — fail-soft so the PTY session still works.
// Real errors (binary missing, sidecar crashed before printing URL)
// surface to the caller, but the caller (manager) treats sidecar
// failure as non-fatal and just logs.
func startOTelSidecar(binPath, bridgeSessionID, bridgeServerURL, ptyCwd, ptyResumeID string) (*ptySidecar, []string, error) {
	if binPath == "" {
		return nil, nil, fmt.Errorf("sidecar bin path empty")
	}
	if bridgeSessionID == "" {
		return nil, nil, fmt.Errorf("bridge_session_id empty")
	}
	if bridgeServerURL == "" {
		return nil, nil, fmt.Errorf("bridge server URL empty")
	}

	cmd := exec.Command(binPath, "-otel-sidecar")
	cmd.Env = append(childprocessenv.EnvironmentWithoutServerSecrets(),
		"LLMBRIDGE_BRIDGE_SESSION_ID="+bridgeSessionID,
		"LLMBRIDGE_BRIDGE_SERVER_URL="+bridgeServerURL,
	)
	// Rollout-tailer inputs. Both optional from the sidecar's
	// perspective (it falls back gracefully when unset); we forward
	// whatever the manager knows.
	if ptyCwd != "" {
		cmd.Env = append(cmd.Env, "LLMBRIDGE_PTY_CWD="+ptyCwd)
	}
	if ptyResumeID != "" {
		cmd.Env = append(cmd.Env, "LLMBRIDGE_PTY_RESUME_ID="+ptyResumeID)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("sidecar stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("sidecar stdin pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("sidecar start: %w", err)
	}

	// Read the endpoint URL line from sidecar stdout. We block on this
	// because the PTY child needs the OTel env vars set at spawn time.
	// Timeout aggressively — a 5s startup means we're already breaking
	// the user-facing chat latency budget.
	endpointCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		br := bufio.NewReader(stdout)
		line, err := br.ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		endpointCh <- line
	}()

	var endpoint string
	select {
	case endpoint = <-endpointCh:
	case err := <-errCh:
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, nil, fmt.Errorf("sidecar endpoint read: %w", err)
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, nil, fmt.Errorf("sidecar endpoint read: timeout after 3s")
	}

	endpoint = trimNewline(endpoint)
	if endpoint == "" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, nil, fmt.Errorf("sidecar endpoint was empty")
	}

	// Drain the rest of stdout so the sidecar doesn't block on a full
	// pipe later. We don't actually look at anything after the URL line.
	go func() {
		_, _ = io.Copy(io.Discard, stdout)
	}()

	env := []string{
		"CLAUDE_CODE_ENABLE_TELEMETRY=1",
		"OTEL_LOGS_EXPORTER=otlp",
		"OTEL_METRICS_EXPORTER=otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL=http/json",
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + endpoint,
		"OTEL_METRIC_EXPORT_INTERVAL=1000",
		"OTEL_LOGS_EXPORT_INTERVAL=1000",
		"OTEL_LOG_USER_PROMPTS=1",
		"OTEL_SERVICE_NAME=llm-bridge-claudecode-pty",
	}

	return &ptySidecar{cmd: cmd, endpointURL: endpoint, stdin: stdin}, env, nil
}

// stop terminates the sidecar. Closes stdin first (the sidecar's normal
// shutdown signal, which drains pending OTel batches with a flush window
// before exiting), then SIGTERMs as a backstop, then SIGKILLs if the
// process still lingers.
func (s *ptySidecar) stop() {
	if s == nil || s.cmd == nil {
		return
	}
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	exited := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
		return
	case <-time.After(5 * time.Second):
	}
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-exited:
		return
	case <-time.After(2 * time.Second):
	}
	if s.cmd.Process != nil {
		log.Printf("[sidecar] forcing kill on stuck pty sidecar pid=%d", s.cmd.Process.Pid)
		_ = s.cmd.Process.Kill()
	}
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// ptySidecarStarter starts a harness's sidecar and returns the env vars the
// PTY child needs to reach it (none for a rollout tailer).
type ptySidecarStarter func(binPath, bridgeSessionID, bridgeServerURL, ptyCwd, ptyResumeID string) (*ptySidecar, []string, error)

// ptySidecarStarters names the harnesses whose PTY sessions report events,
// and how. A harness missing here runs its PTY with no events reaching the
// dash, which the manager logs at spawn.
var ptySidecarStarters = map[msg.Harness]ptySidecarStarter{
	msg.HarnessClaudeCode: startOTelSidecar,
	msg.HarnessCodex:      startRolloutSidecar,
}

// startRolloutSidecar spawns `<wrapper> -pty-sidecar`, which follows the PTY
// session's rollout file and posts its events to the bridge. It needs no
// handshake: the PTY child is not told anything about it.
func startRolloutSidecar(binPath, bridgeSessionID, bridgeServerURL, ptyCwd, ptyResumeID string) (*ptySidecar, []string, error) {
	if binPath == "" || bridgeSessionID == "" || bridgeServerURL == "" {
		return nil, nil, fmt.Errorf("rollout sidecar needs a binary, a bridge session id and a bridge URL (got %q, %q, %q)", binPath, bridgeSessionID, bridgeServerURL)
	}
	cmd := exec.Command(binPath, "-pty-sidecar")
	cmd.Env = append(childprocessenv.EnvironmentWithoutServerSecrets(),
		"LLMBRIDGE_BRIDGE_SESSION_ID="+bridgeSessionID,
		"LLMBRIDGE_BRIDGE_SERVER_URL="+bridgeServerURL,
		"LLMBRIDGE_PTY_CWD="+ptyCwd,
	)
	if ptyResumeID != "" {
		cmd.Env = append(cmd.Env, "LLMBRIDGE_PTY_RESUME_ID="+ptyResumeID)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("rollout sidecar stdin pipe: %w", err)
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("rollout sidecar start: %w", err)
	}
	return &ptySidecar{cmd: cmd, stdin: stdin}, nil, nil
}
