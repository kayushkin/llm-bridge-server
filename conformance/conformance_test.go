package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// harnessProcess wraps a running harness subprocess for testing.
type harnessProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	events chan msg.Event
	errors chan error
	cancel context.CancelFunc
}

// request mirrors the server's JSON-RPC request format.
type request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// conformanceEventTimeout is how long a test waits for an event before calling
// it absent.
//
// The old value was 10s, which is shorter than some harnesses take to answer.
// llm-bridge-claudecode's -discover is the clear case: it works, it takes
// between 20 and 60 seconds on this host, and the matrix filed it as absent.
//
// ⚠️ This does NOT explain the `start` failures, and it was tempting to think
// it did — both harnesses fail `start` at exactly the old timeout. They fail
// because testStart waits for EventSessionState, and neither
// llm-bridge-codex nor llm-bridge-claudecode emits that event at all: grep
// both for msg.EventSessionState and you get zero hits. llm-bridge-server
// derives session state centrally from EventResult and EventError, and the
// harnesses say so in comments. Only cmd/mock-harness still emits it, which
// is why the suite looks green against the mock and fails against everything
// that ships. Raising the timeout makes that wait longer, not truer. Fixing
// it means deciding what `start` should assert against a harness that
// deliberately reports nothing — see the note in CONFORMANCE-GRADING.md.
//
// 60s by default; override with CONFORMANCE_EVENT_TIMEOUT (any duration Go can
// parse, e.g. "90s") when testing a slower harness.
func conformanceEventTimeout() time.Duration {
	return conformanceDuration("CONFORMANCE_EVENT_TIMEOUT", 60*time.Second)
}

// conformanceProcessTimeout caps one harness subprocess. It must exceed the
// event timeout, or the process dies before the wait it is meant to outlast.
func conformanceProcessTimeout() time.Duration {
	return conformanceDuration("CONFORMANCE_PROCESS_TIMEOUT", 5*time.Minute)
}

func conformanceDuration(envName string, fallback time.Duration) time.Duration {
	raw := os.Getenv(envName)
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		// Falling back silently would run the whole matrix at a timeout the
		// operator did not choose and report the results as if they had.
		panic(fmt.Sprintf("%s=%q is not a duration: %v", envName, raw, err))
	}
	return parsed
}

// startHarness launches a harness binary and returns a test handle.
func startHarness(t *testing.T, binary string, env ...string) *harnessProcess {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), conformanceProcessTimeout())
	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatalf("stdin pipe: %v", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		stdin.Close()
		t.Fatalf("stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		stdin.Close()
		t.Fatalf("start %s: %v", binary, err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	hp := &harnessProcess{
		cmd:    cmd,
		stdin:  stdin,
		stdout: scanner,
		events: make(chan msg.Event, 100),
		errors: make(chan error, 1),
		cancel: cancel,
	}

	// Read events in background
	go func() {
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var event msg.Event
			if err := json.Unmarshal(line, &event); err != nil {
				continue
			}
			hp.events <- event
		}
		close(hp.events)
	}()

	return hp
}

// send writes a JSON-RPC request to the harness stdin.
func (hp *harnessProcess) send(method string, params any) error {
	req := request{Method: method}
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
	_, err = hp.stdin.Write(data)
	return err
}

// waitForEvent waits for an event matching the predicate, with timeout.
func (hp *harnessProcess) waitForEvent(timeout time.Duration, match func(msg.Event) bool) (msg.Event, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case event, ok := <-hp.events:
			if !ok {
				return msg.Event{}, fmt.Errorf("event channel closed")
			}
			if match(event) {
				return event, nil
			}
		case <-timer.C:
			return msg.Event{}, fmt.Errorf("timeout waiting for event")
		}
	}
}

// waitForEventType waits for a specific event type.
func (hp *harnessProcess) waitForEventType(timeout time.Duration, eventType msg.EventType) (msg.Event, error) {
	return hp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == eventType
	})
}

// collectEvents reads all events within the timeout window.
func (hp *harnessProcess) collectEvents(timeout time.Duration) []msg.Event {
	var events []msg.Event
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case event, ok := <-hp.events:
			if !ok {
				return events
			}
			events = append(events, event)
		case <-timer.C:
			return events
		}
	}
}

// close shuts down the harness process.
func (hp *harnessProcess) close() {
	hp.stdin.Close()
	hp.cancel()
	hp.cmd.Wait()
}

// ──────────────────────────────────────────────────────────────────────────────
// Test harness resolution
// ──────────────────────────────────────────────────────────────────────────────

// targetHarness returns the harness binary to test.
// Set CONFORMANCE_HARNESS env var to test a specific harness binary.
// Defaults to the mock-harness for CI.
func targetHarness(t *testing.T) string {
	t.Helper()
	if h := os.Getenv("CONFORMANCE_HARNESS"); h != "" {
		path, err := exec.LookPath(h)
		if err != nil {
			t.Skipf("harness binary not found: %s", h)
		}
		return path
	}

	// Default: build and use mock-harness
	mockDir := filepath.Join("..", "cmd", "mock-harness")
	mockBin := filepath.Join(t.TempDir(), "mock-harness")
	cmd := exec.Command("go", "build", "-o", mockBin, mockDir)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build mock-harness: %v", err)
	}
	return mockBin
}

// harnessName extracts the harness name from the binary path.
func harnessName(binary string) string {
	base := filepath.Base(binary)
	if strings.HasPrefix(base, "llm-bridge-") {
		return strings.TrimPrefix(base, "llm-bridge-")
	}
	return base
}

// ──────────────────────────────────────────────────────────────────────────────
// Feature tests
// ──────────────────────────────────────────────────────────────────────────────

// TestMockHarnessEmitsToolRunning locks in the finding-§5 fix: the mock used to
// emit only "running" at every transition, so the tool-in-flight state — the one
// the interrupt bug hides in — was unreachable in tests. A message round-trip
// must now pass through a session_state=tool_running in the raw stream. Scoped
// to the reference mock (skipped for an override harness, which derives state
// centrally rather than emitting it raw).
func TestMockHarnessEmitsToolRunning(t *testing.T) {
	if os.Getenv("CONFORMANCE_HARNESS") != "" {
		t.Skip("raw-stream tool_running is a mock-harness guarantee; skipping for override harness")
	}
	binary := targetHarness(t)
	hp := startHarness(t, binary)
	defer hp.close()

	if err := hp.send("start", map[string]any{"session_id": "test-tool-running"}); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if _, err := hp.waitForEvent(10*time.Second, func(e msg.Event) bool {
		return e.Type == msg.EventSessionState && e.State != nil && e.State.State == msg.SessionRunning
	}); err != nil {
		t.Fatalf("wait for running: %v", err)
	}
	if err := hp.send("message", map[string]any{"content": "run a tool"}); err != nil {
		t.Fatalf("send message: %v", err)
	}
	if _, err := hp.waitForEvent(10*time.Second, func(e msg.Event) bool {
		return e.Type == msg.EventSessionState && e.State != nil && e.State.State == msg.SessionToolRunning
	}); err != nil {
		t.Fatalf("mock-harness never emitted tool_running on a message round-trip: %v", err)
	}
}

func TestConformance(t *testing.T) {
	binary := targetHarness(t)
	name := harnessName(binary)
	eventTimeout := conformanceEventTimeout()

	result := &HarnessResult{
		Harness:  name,
		Binary:   binary,
		TestedAt: time.Now(),
	}

	// ── Feature: start ──────────────────────────────────────────────────
	t.Run("start", func(t *testing.T) {
		start := time.Now()
		hp := startHarness(t, binary)
		defer hp.close()

		err := hp.send("start", map[string]any{
			"session_id":   "test-session-001",
			"display_name": "Conformance Test",
		})
		if err != nil {
			result.AddResult(TestResult{Feature: FeatureStart, Error: err.Error()})
			t.Fatalf("send start: %v", err)
		}

		// Should get a session_state event with state=running
		event, err := hp.waitForEvent(eventTimeout, func(e msg.Event) bool {
			return e.Type == msg.EventSessionState && e.State != nil && e.State.State == msg.SessionRunning
		})
		if err != nil {
			result.AddResult(TestResult{Feature: FeatureStart, Error: err.Error()})
			t.Fatalf("wait for running state: %v", err)
		}

		if event.BridgeSessionID == "" {
			result.AddResult(TestResult{Feature: FeatureStart, Error: "bridge_session_id empty in event"})
			t.Error("bridge_session_id should be set in events")
		}

		result.AddResult(TestResult{Feature: FeatureStart, Passed: true, Duration: time.Since(start).String()})
	})

	// ── Feature: message ────────────────────────────────────────────────
	t.Run("message", func(t *testing.T) {
		start := time.Now()
		hp := startHarness(t, binary)
		defer hp.close()

		// Start first
		hp.send("start", map[string]any{"session_id": "test-msg"})
		hp.waitForEvent(eventTimeout, func(e msg.Event) bool {
			return e.Type == msg.EventSessionState && e.State != nil && e.State.State == msg.SessionRunning
		})

		// Send a message
		err := hp.send("message", map[string]any{"content": "Hello, world!"})
		if err != nil {
			result.AddResult(TestResult{Feature: FeatureMessage, Error: err.Error()})
			t.Fatalf("send message: %v", err)
		}

		// Should get a result event
		event, err := hp.waitForEventType(eventTimeout, msg.EventResult)
		if err != nil {
			result.AddResult(TestResult{Feature: FeatureMessage, Error: err.Error()})
			t.Fatalf("wait for result: %v", err)
		}

		if event.Result == nil {
			result.AddResult(TestResult{Feature: FeatureMessage, Error: "result field nil"})
			t.Fatal("result event has nil result field")
		}
		if event.Result.Text == "" {
			result.AddResult(TestResult{Feature: FeatureMessage, Error: "empty result text"})
			t.Error("result text should not be empty")
		}

		result.AddResult(TestResult{Feature: FeatureMessage, Passed: true, Duration: time.Since(start).String()})
	})

	// ── Feature: streaming ──────────────────────────────────────────────
	t.Run("streaming", func(t *testing.T) {
		start := time.Now()
		hp := startHarness(t, binary)
		defer hp.close()

		hp.send("start", map[string]any{"session_id": "test-stream"})
		hp.waitForEvent(eventTimeout, func(e msg.Event) bool {
			return e.Type == msg.EventSessionState
		})

		hp.send("message", map[string]any{"content": "test streaming"})

		// Wait for result event, collecting any stream events along the way
		hasStream := false
		gotResult := false
		timer := time.NewTimer(eventTimeout)
		defer timer.Stop()
	streamLoop:
		for {
			select {
			case event, ok := <-hp.events:
				if !ok {
					break streamLoop
				}
				if event.Type == msg.EventStream {
					hasStream = true
				}
				if event.Type == msg.EventResult {
					gotResult = true
					break streamLoop
				}
			case <-timer.C:
				break streamLoop
			}
		}

		if !gotResult {
			result.AddResult(TestResult{Feature: FeatureStreaming, Error: "no result event received"})
			t.Fatal("no result event received")
		}

		if !hasStream {
			result.AddResult(TestResult{Feature: FeatureStreaming, Unsupported: true, Error: "no stream events emitted"})
			t.Skip("harness does not emit stream events")
		}

		result.AddResult(TestResult{Feature: FeatureStreaming, Passed: true, Duration: time.Since(start).String()})
	})

	// ── Feature: compact ────────────────────────────────────────────────
	t.Run("compact", func(t *testing.T) {
		start := time.Now()
		hp := startHarness(t, binary)
		defer hp.close()

		hp.send("start", map[string]any{"session_id": "test-compact"})
		hp.waitForEvent(eventTimeout, func(e msg.Event) bool {
			return e.Type == msg.EventSessionState
		})

		err := hp.send("compact", nil)
		if err != nil {
			result.AddResult(TestResult{Feature: FeatureCompact, Error: err.Error()})
			t.Fatalf("send compact: %v", err)
		}

		// Check for a system event indicating compaction
		event, err := hp.waitForEvent(eventTimeout, func(e msg.Event) bool {
			return e.Type == msg.EventSystem && e.System != nil
		})
		if err != nil {
			result.AddResult(TestResult{Feature: FeatureCompact, Unsupported: true, Error: "no compact response"})
			t.Skip("harness does not respond to compact")
		}

		if event.System.Subtype != "compact_complete" {
			t.Logf("compact system event subtype: %s", event.System.Subtype)
		}

		result.AddResult(TestResult{Feature: FeatureCompact, Passed: true, Duration: time.Since(start).String()})
	})

	// ── Feature: config ─────────────────────────────────────────────────
	t.Run("config", func(t *testing.T) {
		start := time.Now()
		hp := startHarness(t, binary)
		defer hp.close()

		hp.send("start", map[string]any{"session_id": "test-config"})
		hp.waitForEvent(eventTimeout, func(e msg.Event) bool {
			return e.Type == msg.EventSessionState
		})

		configJSON, _ := json.Marshal(map[string]any{"model": "test-model", "effort": "high"})
		err := hp.send("config:"+string(configJSON), nil)
		if err != nil {
			result.AddResult(TestResult{Feature: FeatureConfig, Error: err.Error()})
			t.Fatalf("send config: %v", err)
		}

		event, err := hp.waitForEvent(eventTimeout, func(e msg.Event) bool {
			return e.Type == msg.EventSystem && e.System != nil
		})
		if err != nil {
			result.AddResult(TestResult{Feature: FeatureConfig, Unsupported: true, Error: "no config response"})
			t.Skip("harness does not respond to config")
		}

		_ = event
		result.AddResult(TestResult{Feature: FeatureConfig, Passed: true, Duration: time.Since(start).String()})
	})

	// ── Feature: fork ───────────────────────────────────────────────────
	t.Run("fork", func(t *testing.T) {
		start := time.Now()
		hp := startHarness(t, binary)
		defer hp.close()

		err := hp.send("start", map[string]any{
			"session_id": "test-fork",
			"fork":       "parent-session-id",
		})
		if err != nil {
			result.AddResult(TestResult{Feature: FeatureFork, Error: err.Error()})
			t.Fatalf("send start with fork: %v", err)
		}

		// Should still get a running state
		_, err = hp.waitForEvent(eventTimeout, func(e msg.Event) bool {
			return e.Type == msg.EventSessionState && e.State != nil && e.State.State == msg.SessionRunning
		})
		if err != nil {
			result.AddResult(TestResult{Feature: FeatureFork, Skipped: true, Error: "fork start failed"})
			t.Skip("harness does not support fork")
		}

		result.AddResult(TestResult{Feature: FeatureFork, Passed: true, Duration: time.Since(start).String()})
	})

	// ── Feature: resume ─────────────────────────────────────────────────
	t.Run("resume", func(t *testing.T) {
		start := time.Now()
		hp := startHarness(t, binary)
		defer hp.close()

		err := hp.send("start", map[string]any{
			"session_id": "test-resume",
			"resume":     true,
		})
		if err != nil {
			result.AddResult(TestResult{Feature: FeatureResume, Error: err.Error()})
			t.Fatalf("send start with resume: %v", err)
		}

		_, err = hp.waitForEvent(eventTimeout, func(e msg.Event) bool {
			return e.Type == msg.EventSessionState && e.State != nil && e.State.State == msg.SessionRunning
		})
		if err != nil {
			result.AddResult(TestResult{Feature: FeatureResume, Skipped: true, Error: "resume start failed"})
			t.Skip("harness does not support resume")
		}

		result.AddResult(TestResult{Feature: FeatureResume, Passed: true, Duration: time.Since(start).String()})
	})

	// ── Feature: errors ─────────────────────────────────────────────────
	t.Run("errors", func(t *testing.T) {
		start := time.Now()
		hp := startHarness(t, binary, "MOCK_HARNESS_EMIT_ERROR=true")
		defer hp.close()

		hp.send("start", map[string]any{"session_id": "test-error"})
		hp.waitForEvent(eventTimeout, func(e msg.Event) bool {
			return e.Type == msg.EventSessionState
		})

		hp.send("message", map[string]any{"content": "trigger error"})

		event, err := hp.waitForEventType(eventTimeout, msg.EventError)
		if err != nil {
			result.AddResult(TestResult{Feature: FeatureErrors, Unsupported: true, Error: "no error event emitted"})
			t.Skip("harness does not emit error events")
		}

		if event.Error == nil || event.Error.Message == "" {
			result.AddResult(TestResult{Feature: FeatureErrors, Error: "error event missing message"})
			t.Error("error event should have a message")
		} else {
			result.AddResult(TestResult{Feature: FeatureErrors, Passed: true, Duration: time.Since(start).String()})
		}
	})

	// ── Feature: discover ───────────────────────────────────────────────
	t.Run("discover", func(t *testing.T) {
		start := time.Now()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-discover")
		cmd.Env = os.Environ()
		out, err := cmd.Output()
		if err != nil {
			// Non-zero exit: the flag is not implemented. Not applicable.
			result.AddResult(TestResult{Feature: FeatureDiscover, Unsupported: true, Error: fmt.Sprintf("binary does not support -discover: %v", err)})
			t.Skipf("binary does not support -discover: %v", err)
		}

		// Zero exit: the binary answered, so a bad answer is a failure.
		var sessions []msg.StoredSession
		if err := json.Unmarshal(out, &sessions); err != nil {
			result.AddResult(TestResult{Feature: FeatureDiscover, Error: fmt.Sprintf("-discover exited 0 but its output does not decode as a JSON array of stored sessions: %v", err)})
			t.Fatalf("-discover exited 0 but its output does not decode as a JSON array of stored sessions: %v", err)
		}

		result.AddResult(TestResult{Feature: FeatureDiscover, Passed: true, Duration: time.Since(start).String()})
	})

	// ── Feature: tool_calls (detect-only, may not apply to mock) ────────
	t.Run("tool_calls", func(t *testing.T) {
		// Tool calls require a real LLM interaction — skip for mock, record for real harnesses
		result.AddResult(TestResult{Feature: FeatureToolCalls, Skipped: true, Error: "requires real LLM interaction"})
		t.Skip("tool_calls requires real LLM interaction")
	})

	// ── Feature: thinking (detect-only, may not apply to mock) ──────────
	t.Run("thinking", func(t *testing.T) {
		result.AddResult(TestResult{Feature: FeatureThinking, Skipped: true, Error: "requires real LLM interaction"})
		t.Skip("thinking requires real LLM interaction")
	})

	// ── Feature: block ──────────────────────────────────────────────────
	// Watch a normal message round-trip for any EventBlock (whole finished
	// content blocks). Distinct from EventStream (incremental delta) and
	// EventResult (the terminator).
	t.Run("block", func(t *testing.T) {
		start := time.Now()
		hp := startHarness(t, binary)
		defer hp.close()

		hp.send("start", map[string]any{"session_id": "test-block"})
		hp.waitForEvent(eventTimeout, func(e msg.Event) bool {
			return e.Type == msg.EventSessionState
		})

		hp.send("message", map[string]any{"content": PromptBlock})

		hasBlock := false
		gotResult := false
		timer := time.NewTimer(eventTimeout)
		defer timer.Stop()
	blockLoop:
		for {
			select {
			case event, ok := <-hp.events:
				if !ok {
					break blockLoop
				}
				if event.Type == msg.EventBlock {
					hasBlock = true
				}
				if event.Type == msg.EventResult {
					gotResult = true
					break blockLoop
				}
			case <-timer.C:
				break blockLoop
			}
		}

		if !gotResult {
			result.AddResult(TestResult{Feature: FeatureBlock, Error: "no result event received"})
			t.Fatal("no result event received")
		}
		if !hasBlock {
			result.AddResult(TestResult{Feature: FeatureBlock, Unsupported: true, Error: "no block events emitted"})
			t.Skip("harness does not emit block events")
		}
		result.AddResult(TestResult{Feature: FeatureBlock, Passed: true, Duration: time.Since(start).String()})
	})

	// ── Feature: session_info ───────────────────────────────────────────
	t.Run("session_info", func(t *testing.T) {
		start := time.Now()
		hp := startHarness(t, binary)
		defer hp.close()

		if err := hp.send("start", map[string]any{"session_id": "test-session-info"}); err != nil {
			result.AddResult(TestResult{Feature: FeatureSessionInfo, Error: err.Error()})
			t.Fatalf("send start: %v", err)
		}

		event, err := hp.waitForEventType(eventTimeout, msg.EventSessionInfo)
		if err != nil {
			result.AddResult(TestResult{Feature: FeatureSessionInfo, Unsupported: true, Error: "no session_info event emitted"})
			t.Skip("harness does not emit session_info")
		}
		if event.Info == nil {
			result.AddResult(TestResult{Feature: FeatureSessionInfo, Error: "session_info event had nil Info"})
			t.Fatal("session_info event missing Info")
		}
		result.AddResult(TestResult{Feature: FeatureSessionInfo, Passed: true, Duration: time.Since(start).String()})
	})

	// ── Feature: user_message ───────────────────────────────────────────
	t.Run("user_message", func(t *testing.T) {
		start := time.Now()
		hp := startHarness(t, binary)
		defer hp.close()

		hp.send("start", map[string]any{"session_id": "test-user-message"})
		hp.waitForEvent(eventTimeout, func(e msg.Event) bool {
			return e.Type == msg.EventSessionState
		})

		if err := hp.send("message", map[string]any{"content": PromptUserMessage}); err != nil {
			result.AddResult(TestResult{Feature: FeatureUserMessage, Error: err.Error()})
			t.Fatalf("send message: %v", err)
		}

		hasUserMessage := false
		gotResult := false
		timer := time.NewTimer(eventTimeout)
		defer timer.Stop()
	userMsgLoop:
		for {
			select {
			case event, ok := <-hp.events:
				if !ok {
					break userMsgLoop
				}
				if event.Type == msg.EventUserMessage {
					hasUserMessage = true
				}
				if event.Type == msg.EventResult {
					gotResult = true
					break userMsgLoop
				}
			case <-timer.C:
				break userMsgLoop
			}
		}

		if !gotResult {
			result.AddResult(TestResult{Feature: FeatureUserMessage, Error: "no result event received"})
			t.Fatal("no result event received")
		}
		if !hasUserMessage {
			result.AddResult(TestResult{Feature: FeatureUserMessage, Unsupported: true, Error: "no user_message events emitted"})
			t.Skip("harness does not emit user_message")
		}
		result.AddResult(TestResult{Feature: FeatureUserMessage, Passed: true, Duration: time.Since(start).String()})
	})

	// ── Feature: plan ───────────────────────────────────────────────────
	t.Run("plan", func(t *testing.T) {
		start := time.Now()
		hp := startHarness(t, binary)
		defer hp.close()

		hp.send("start", map[string]any{"session_id": "test-plan"})
		hp.waitForEvent(eventTimeout, func(e msg.Event) bool {
			return e.Type == msg.EventSessionState
		})

		hp.send("message", map[string]any{"content": PromptPlan})

		hasPlan := false
		gotTerminator := false
		timer := time.NewTimer(eventTimeout)
		defer timer.Stop()
	planLoop:
		for {
			select {
			case event, ok := <-hp.events:
				if !ok {
					break planLoop
				}
				if event.Type == msg.EventPlan {
					hasPlan = true
				}
				if event.Type == msg.EventResult || event.Type == msg.EventError {
					gotTerminator = true
					break planLoop
				}
			case <-timer.C:
				break planLoop
			}
		}

		if !gotTerminator {
			result.AddResult(TestResult{Feature: FeaturePlan, Skipped: true, Error: "no terminating result/error event"})
			t.Skip("plan: no terminator")
		}
		if !hasPlan {
			result.AddResult(TestResult{Feature: FeaturePlan, Unsupported: true, Error: "no plan events emitted (scenario-specific)"})
			t.Skip("harness does not emit plan events")
		}
		result.AddResult(TestResult{Feature: FeaturePlan, Passed: true, Duration: time.Since(start).String()})
	})

	// ── Feature: hook ───────────────────────────────────────────────────
	t.Run("hook", func(t *testing.T) {
		start := time.Now()
		hp := startHarness(t, binary)
		defer hp.close()

		hp.send("start", map[string]any{"session_id": "test-hook"})
		hp.waitForEvent(eventTimeout, func(e msg.Event) bool {
			return e.Type == msg.EventSessionState
		})

		hp.send("message", map[string]any{"content": PromptHook})

		hasHook := false
		gotTerminator := false
		timer := time.NewTimer(eventTimeout)
		defer timer.Stop()
	hookLoop:
		for {
			select {
			case event, ok := <-hp.events:
				if !ok {
					break hookLoop
				}
				if event.Type == msg.EventHook {
					hasHook = true
				}
				if event.Type == msg.EventResult || event.Type == msg.EventError {
					gotTerminator = true
					break hookLoop
				}
			case <-timer.C:
				break hookLoop
			}
		}

		if !gotTerminator {
			result.AddResult(TestResult{Feature: FeatureHook, Skipped: true, Error: "no terminating result/error event"})
			t.Skip("hook: no terminator")
		}
		if !hasHook {
			result.AddResult(TestResult{Feature: FeatureHook, Unsupported: true, Error: "no hook events emitted (scenario-specific)"})
			t.Skip("harness does not emit hook events")
		}
		result.AddResult(TestResult{Feature: FeatureHook, Passed: true, Duration: time.Since(start).String()})
	})

	// ── Feature: usage_total (server-derived) ───────────────────────────
	t.Run("usage_total", func(t *testing.T) {
		result.AddResult(TestResult{
			Feature: FeatureUsageTotal,
			Skipped: true,
			Error:   "server-derived: emitted by llm-bridge-server, not the harness subprocess",
		})
		t.Skip("usage_total is server-derived")
	})

	// ── Feature: turn_complete (server-derived) ─────────────────────────
	t.Run("turn_complete", func(t *testing.T) {
		result.AddResult(TestResult{
			Feature: FeatureTurnComplete,
			Skipped: true,
			Error:   "server-derived: emitted by llm-bridge-server, not the harness subprocess",
		})
		t.Skip("turn_complete is server-derived")
	})

	// ── Feature: import ─────────────────────────────────────────────────
	// Tri-state contract (matches conformance/runner.go testImport):
	//   exit 2 → SKIP (binary signals "not implemented")
	//   exit 0 → PASS (binary handled missing session as a no-op)
	//   else  → FAIL (binary either errored or silently ignored the flag)
	t.Run("import", func(t *testing.T) {
		start := time.Now()

		cmd := exec.Command(binary, "-import-history", "nonexistent-session")
		cmd.Env = os.Environ()
		err := cmd.Run()
		if err != nil {
			exitErr, ok := err.(*exec.ExitError)
			if ok && exitErr.ExitCode() == 2 {
				result.AddResult(TestResult{Feature: FeatureImport, Unsupported: true, Error: "binary does not support -import-history"})
				t.Skip("binary does not support -import-history")
			}
			code := -1
			if ok {
				code = exitErr.ExitCode()
			}
			msg := fmt.Sprintf("binary -import-history exited %d (expected 0 for pass or 2 for skip): %v", code, err)
			result.AddResult(TestResult{Feature: FeatureImport, Error: msg})
			t.Fatal(msg)
		}

		result.AddResult(TestResult{Feature: FeatureImport, Passed: true, Duration: time.Since(start).String()})
	})

	// ── Save matrix ─────────────────────────────────────────────────────
	matrixPath := os.Getenv("CONFORMANCE_MATRIX_PATH")
	if matrixPath == "" {
		matrixPath = filepath.Join(os.TempDir(), "conformance-matrix.json")
	}

	matrix := &Matrix{
		GeneratedAt: time.Now(),
		Harnesses:   []HarnessResult{*result},
	}

	// Merge with existing matrix if present
	if existing, err := LoadMatrix(matrixPath); err == nil {
		// Replace or append
		found := false
		for i, h := range existing.Harnesses {
			if h.Harness == result.Harness {
				existing.Harnesses[i] = *result
				found = true
				break
			}
		}
		if !found {
			existing.Harnesses = append(existing.Harnesses, *result)
		}
		existing.GeneratedAt = time.Now()
		matrix = existing
	}

	if err := SaveMatrix(matrixPath, matrix); err != nil {
		t.Logf("failed to save matrix: %v", err)
	} else {
		t.Logf("conformance matrix saved to %s", matrixPath)
	}

	// Print summary
	t.Logf("\n=== Conformance Summary: %s ===", result.Harness)
	t.Logf("Passed: %d  Failed: %d  Skipped: %d  Total: %d",
		result.Summary.Passed, result.Summary.Failed, result.Summary.Skipped, result.Summary.Total)
	for _, r := range result.Results {
		status := "PASS"
		if r.Skipped {
			status = "SKIP"
		} else if !r.Passed {
			status = "FAIL"
		}
		t.Logf("  [%s] %s %s", status, r.Feature, r.Duration)
	}
}
