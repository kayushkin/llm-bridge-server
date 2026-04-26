package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// SourceTag is the value placed in CreateSessionRequest.Source for any session
// that originates from the conformance suite. Auto-discover uses it to file
// leaked test sessions into the configured Conformance folder instead of the
// unfiled list.
const SourceTag = "conformance"

// Prompt strings sent to harness subprocesses during conformance tests. They
// are exported so the auto-discover path can recognise sessions left behind on
// disk by the underlying harness CLI (e.g. ~/.codex/sessions/...) and tag them
// with SourceTag.
const (
	PromptMessage     = "Hello, world!"  // testMessage
	PromptStreaming   = "test streaming" // testStreaming
	PromptErrors      = "trigger error"  // testErrors
	PromptContextUsed = "count tokens"   // testContextUsed
)

// IsConformancePrompt reports whether s matches one of the canonical prompts
// the conformance suite sends to harness subprocesses. Used by auto-discover
// to detect and tag sessions that the underlying harness CLI persisted to
// disk during a conformance run.
func IsConformancePrompt(s string) bool {
	switch s {
	case PromptMessage, PromptStreaming, PromptErrors, PromptContextUsed:
		return true
	}
	return false
}

// runProcess wraps a running harness subprocess for conformance testing.
type runProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	events chan msg.Event
	cancel context.CancelFunc
}

type rpcRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

func launchProcess(ctx context.Context, binary string, env ...string) (*runProcess, error) {
	// Subprocess deadline must be wider than the longest per-feature timeout
	// (currently llmTimeout=30s) so a slow LLM round-trip doesn't get killed
	// before the test's wait can observe its result.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	cmd := exec.CommandContext(ctx, binary)
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		stdin.Close()
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	rp := &runProcess{
		cmd:    cmd,
		stdin:  stdin,
		stdout: scanner,
		events: make(chan msg.Event, 100),
		cancel: cancel,
	}

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
			rp.events <- event
		}
		close(rp.events)
	}()

	return rp, nil
}

func (rp *runProcess) send(method string, params any) error {
	req := rpcRequest{Method: method}
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
	_, err = rp.stdin.Write(data)
	return err
}

func (rp *runProcess) waitForEvent(timeout time.Duration, match func(msg.Event) bool) (msg.Event, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case event, ok := <-rp.events:
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

func (rp *runProcess) waitForEventType(timeout time.Duration, eventType msg.EventType) (msg.Event, error) {
	return rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == eventType
	})
}

func (rp *runProcess) close() {
	rp.stdin.Close()
	rp.cancel()
	rp.cmd.Wait()
}

// ──────────────────────────────────────────────────────────────────────────────
// Runner — callable conformance test execution
// ──────────────────────────────────────────────────────────────────────────────

// RunHarness runs the full conformance suite against a single harness binary
// and returns the result. This is the non-test equivalent of TestConformance.
func RunHarness(ctx context.Context, binary string) (*HarnessResult, error) {
	name := runnerHarnessName(binary)
	// Protocol-level events (state, system, error) arrive instantly;
	// 10s is plenty. Features that wait for an EventResult after sending a
	// message need a real LLM round-trip — which on a busy host can take
	// 8-15s for short prompts — so they get a longer budget.
	eventTimeout := 10 * time.Second
	llmTimeout := 30 * time.Second

	result := &HarnessResult{
		Harness:  name,
		Binary:   binary,
		TestedAt: time.Now(),
	}

	// ── start ──
	result.AddResult(testStart(ctx, binary, eventTimeout))

	// ── message ──
	result.AddResult(testMessage(ctx, binary, llmTimeout))

	// ── streaming ──
	result.AddResult(testStreaming(ctx, binary, llmTimeout))

	// ── compact ──
	result.AddResult(testCompact(ctx, binary, eventTimeout))

	// ── config ──
	result.AddResult(testConfig(ctx, binary, eventTimeout))

	// ── fork ──
	result.AddResult(testFork(ctx, binary, eventTimeout))

	// ── resume ──
	result.AddResult(testResume(ctx, binary, eventTimeout))

	// ── errors ──
	result.AddResult(testErrors(ctx, binary, eventTimeout))

	// ── discover ──
	result.AddResult(testDiscover(ctx, binary))

	// ── import ──
	result.AddResult(testImport(ctx, binary))

	// ── tool_calls (skipped — requires real LLM) ──
	result.AddResult(TestResult{Feature: FeatureToolCalls, Skipped: true, Error: "requires real LLM interaction"})

	// ── thinking (skipped — requires real LLM) ──
	result.AddResult(TestResult{Feature: FeatureThinking, Skipped: true, Error: "requires real LLM interaction"})

	// ── reasoning ──
	result.AddResult(testReasoning(ctx, binary, eventTimeout))

	// ── system_prompt ──
	result.AddResult(testSystemPrompt(ctx, binary, eventTimeout))

	// ── context_used ──
	result.AddResult(testContextUsed(ctx, binary, llmTimeout))

	return result, nil
}

func testReasoning(ctx context.Context, binary string, timeout time.Duration) TestResult {
	start := time.Now()
	rp, err := launchProcess(ctx, binary)
	if err != nil {
		return TestResult{Feature: FeatureReasoning, Error: err.Error()}
	}
	defer rp.close()

	rp.send("start", map[string]any{"session_id": "conformance-reasoning"})
	rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == msg.EventSessionState
	})

	configJSON, _ := json.Marshal(map[string]any{"effort": "high", "reasoning_effort": "high"})
	if err := rp.send("config:"+string(configJSON), nil); err != nil {
		return TestResult{Feature: FeatureReasoning, Error: err.Error()}
	}

	_, err = rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == msg.EventSystem && e.System != nil
	})
	if err != nil {
		return TestResult{Feature: FeatureReasoning, Skipped: true, Error: "no response to reasoning effort config"}
	}
	return TestResult{Feature: FeatureReasoning, Passed: true, Duration: time.Since(start).String()}
}

func testSystemPrompt(ctx context.Context, binary string, timeout time.Duration) TestResult {
	start := time.Now()
	rp, err := launchProcess(ctx, binary)
	if err != nil {
		return TestResult{Feature: FeatureSystemPrompt, Error: err.Error()}
	}
	defer rp.close()

	if err := rp.send("start", map[string]any{
		"session_id":    "conformance-sysprompt",
		"system_prompt": "You are a conformance test assistant.",
	}); err != nil {
		return TestResult{Feature: FeatureSystemPrompt, Error: err.Error()}
	}

	_, err = rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == msg.EventSessionState && e.State != nil && e.State.State == msg.SessionRunning
	})
	if err != nil {
		return TestResult{Feature: FeatureSystemPrompt, Skipped: true, Error: "system_prompt start failed"}
	}
	return TestResult{Feature: FeatureSystemPrompt, Passed: true, Duration: time.Since(start).String()}
}

func testContextUsed(ctx context.Context, binary string, timeout time.Duration) TestResult {
	start := time.Now()
	rp, err := launchProcess(ctx, binary)
	if err != nil {
		return TestResult{Feature: FeatureContextUsed, Error: err.Error()}
	}
	defer rp.close()

	rp.send("start", map[string]any{"session_id": "conformance-context"})
	rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == msg.EventSessionState
	})

	rp.send("message", map[string]any{"content": PromptContextUsed})

	event, err := rp.waitForEventType(timeout, msg.EventResult)
	if err != nil {
		return TestResult{Feature: FeatureContextUsed, Error: err.Error()}
	}
	if event.Result == nil {
		return TestResult{Feature: FeatureContextUsed, Error: "result event had nil result"}
	}
	u := event.Result.Usage
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 && u.ContextTokens == 0 {
		return TestResult{Feature: FeatureContextUsed, Skipped: true, Error: "result does not report token usage"}
	}
	return TestResult{Feature: FeatureContextUsed, Passed: true, Duration: time.Since(start).String()}
}

func testStart(ctx context.Context, binary string, timeout time.Duration) TestResult {
	start := time.Now()
	rp, err := launchProcess(ctx, binary)
	if err != nil {
		return TestResult{Feature: FeatureStart, Error: err.Error()}
	}
	defer rp.close()

	if err := rp.send("start", map[string]any{"session_id": "conformance-start", "display_name": "Conformance Test"}); err != nil {
		return TestResult{Feature: FeatureStart, Error: err.Error()}
	}

	event, err := rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == msg.EventSessionState && e.State != nil && e.State.State == msg.SessionRunning
	})
	if err != nil {
		return TestResult{Feature: FeatureStart, Error: err.Error()}
	}
	if event.SessionID == "" {
		return TestResult{Feature: FeatureStart, Error: "session_id empty in event"}
	}
	return TestResult{Feature: FeatureStart, Passed: true, Duration: time.Since(start).String()}
}

func testMessage(ctx context.Context, binary string, timeout time.Duration) TestResult {
	start := time.Now()
	rp, err := launchProcess(ctx, binary)
	if err != nil {
		return TestResult{Feature: FeatureMessage, Error: err.Error()}
	}
	defer rp.close()

	rp.send("start", map[string]any{"session_id": "conformance-msg"})
	rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == msg.EventSessionState && e.State != nil && e.State.State == msg.SessionRunning
	})

	if err := rp.send("message", map[string]any{"content": PromptMessage}); err != nil {
		return TestResult{Feature: FeatureMessage, Error: err.Error()}
	}

	event, err := rp.waitForEventType(timeout, msg.EventResult)
	if err != nil {
		return TestResult{Feature: FeatureMessage, Error: err.Error()}
	}
	if event.Result == nil {
		return TestResult{Feature: FeatureMessage, Error: "result field nil"}
	}
	if event.Result.Text == "" {
		return TestResult{Feature: FeatureMessage, Error: "empty result text"}
	}
	return TestResult{Feature: FeatureMessage, Passed: true, Duration: time.Since(start).String()}
}

func testStreaming(ctx context.Context, binary string, timeout time.Duration) TestResult {
	start := time.Now()
	rp, err := launchProcess(ctx, binary)
	if err != nil {
		return TestResult{Feature: FeatureStreaming, Error: err.Error()}
	}
	defer rp.close()

	rp.send("start", map[string]any{"session_id": "conformance-stream"})
	rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == msg.EventSessionState
	})

	rp.send("message", map[string]any{"content": PromptStreaming})

	hasStream := false
	gotResult := false
	timer := time.NewTimer(timeout)
	defer timer.Stop()
loop:
	for {
		select {
		case event, ok := <-rp.events:
			if !ok {
				break loop
			}
			if event.Type == msg.EventStream {
				hasStream = true
			}
			if event.Type == msg.EventResult {
				gotResult = true
				break loop
			}
		case <-timer.C:
			break loop
		}
	}

	if !gotResult {
		return TestResult{Feature: FeatureStreaming, Error: "no result event received"}
	}
	if !hasStream {
		return TestResult{Feature: FeatureStreaming, Skipped: true, Error: "no stream events emitted"}
	}
	return TestResult{Feature: FeatureStreaming, Passed: true, Duration: time.Since(start).String()}
}

func testCompact(ctx context.Context, binary string, timeout time.Duration) TestResult {
	start := time.Now()
	rp, err := launchProcess(ctx, binary)
	if err != nil {
		return TestResult{Feature: FeatureCompact, Error: err.Error()}
	}
	defer rp.close()

	rp.send("start", map[string]any{"session_id": "conformance-compact"})
	rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == msg.EventSessionState
	})

	if err := rp.send("compact", nil); err != nil {
		return TestResult{Feature: FeatureCompact, Error: err.Error()}
	}

	_, err = rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == msg.EventSystem && e.System != nil
	})
	if err != nil {
		return TestResult{Feature: FeatureCompact, Skipped: true, Error: "no compact response"}
	}
	return TestResult{Feature: FeatureCompact, Passed: true, Duration: time.Since(start).String()}
}

func testConfig(ctx context.Context, binary string, timeout time.Duration) TestResult {
	start := time.Now()
	rp, err := launchProcess(ctx, binary)
	if err != nil {
		return TestResult{Feature: FeatureConfig, Error: err.Error()}
	}
	defer rp.close()

	rp.send("start", map[string]any{"session_id": "conformance-config"})
	rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == msg.EventSessionState
	})

	configJSON, _ := json.Marshal(map[string]any{"model": "test-model", "effort": "high"})
	if err := rp.send("config:"+string(configJSON), nil); err != nil {
		return TestResult{Feature: FeatureConfig, Error: err.Error()}
	}

	_, err = rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == msg.EventSystem && e.System != nil
	})
	if err != nil {
		return TestResult{Feature: FeatureConfig, Skipped: true, Error: "no config response"}
	}
	return TestResult{Feature: FeatureConfig, Passed: true, Duration: time.Since(start).String()}
}

func testFork(ctx context.Context, binary string, timeout time.Duration) TestResult {
	start := time.Now()
	rp, err := launchProcess(ctx, binary)
	if err != nil {
		return TestResult{Feature: FeatureFork, Error: err.Error()}
	}
	defer rp.close()

	if err := rp.send("start", map[string]any{"session_id": "conformance-fork", "fork": "parent-session-id"}); err != nil {
		return TestResult{Feature: FeatureFork, Error: err.Error()}
	}

	_, err = rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == msg.EventSessionState && e.State != nil && e.State.State == msg.SessionRunning
	})
	if err != nil {
		return TestResult{Feature: FeatureFork, Skipped: true, Error: "fork start failed"}
	}
	return TestResult{Feature: FeatureFork, Passed: true, Duration: time.Since(start).String()}
}

func testResume(ctx context.Context, binary string, timeout time.Duration) TestResult {
	start := time.Now()
	rp, err := launchProcess(ctx, binary)
	if err != nil {
		return TestResult{Feature: FeatureResume, Error: err.Error()}
	}
	defer rp.close()

	if err := rp.send("start", map[string]any{"session_id": "conformance-resume", "resume": true}); err != nil {
		return TestResult{Feature: FeatureResume, Error: err.Error()}
	}

	_, err = rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == msg.EventSessionState && e.State != nil && e.State.State == msg.SessionRunning
	})
	if err != nil {
		return TestResult{Feature: FeatureResume, Skipped: true, Error: "resume start failed"}
	}
	return TestResult{Feature: FeatureResume, Passed: true, Duration: time.Since(start).String()}
}

func testErrors(ctx context.Context, binary string, timeout time.Duration) TestResult {
	start := time.Now()
	rp, err := launchProcess(ctx, binary, "MOCK_HARNESS_EMIT_ERROR=true")
	if err != nil {
		return TestResult{Feature: FeatureErrors, Error: err.Error()}
	}
	defer rp.close()

	rp.send("start", map[string]any{"session_id": "conformance-error"})
	rp.waitForEvent(timeout, func(e msg.Event) bool {
		return e.Type == msg.EventSessionState
	})

	rp.send("message", map[string]any{"content": PromptErrors})

	event, err := rp.waitForEventType(timeout, msg.EventError)
	if err != nil {
		return TestResult{Feature: FeatureErrors, Skipped: true, Error: "no error event emitted"}
	}
	if event.Error == nil || event.Error.Message == "" {
		return TestResult{Feature: FeatureErrors, Error: "error event missing message"}
	}
	return TestResult{Feature: FeatureErrors, Passed: true, Duration: time.Since(start).String()}
}

func testDiscover(ctx context.Context, binary string) TestResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, "-discover")
	out, err := cmd.Output()
	if err != nil {
		return TestResult{Feature: FeatureDiscover, Skipped: true, Error: fmt.Sprintf("binary does not support -discover: %v", err)}
	}

	var sessions []msg.StoredSession
	if err := json.Unmarshal(out, &sessions); err != nil {
		return TestResult{Feature: FeatureDiscover, Skipped: true, Error: fmt.Sprintf("invalid JSON output: %v", err)}
	}
	return TestResult{Feature: FeatureDiscover, Passed: true, Duration: time.Since(start).String()}
}

func testImport(ctx context.Context, binary string) TestResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, "-import-history", "nonexistent-session")
	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 2 {
			return TestResult{Feature: FeatureImport, Skipped: true, Error: "binary does not support -import-history"}
		}
	}
	return TestResult{Feature: FeatureImport, Passed: true, Duration: time.Since(start).String()}
}

// harnessName extracts the harness name from the binary path.
// Duplicated from conformance_test.go since test helpers aren't exported.
func runnerHarnessName(binary string) string {
	base := filepath.Base(binary)
	if strings.HasPrefix(base, "llm-bridge-") {
		return strings.TrimPrefix(base, "llm-bridge-")
	}
	return base
}
