// mock-harness implements the llm-bridge subprocess protocol for testing.
//
// It reads JSON-RPC requests from stdin and emits canonical msg.Event on stdout
// as NDJSON. Behavior is controlled via environment variables:
//
//   MOCK_HARNESS_NAME        - harness name to report (default: "mock")
//   MOCK_HARNESS_SESSION_ID  - session ID to report in events (default: "mock-session-001")
//   MOCK_HARNESS_CAPS        - comma-separated capabilities (default: "compact,fork,model,effort")
//   MOCK_HARNESS_DELAY_MS    - delay before emitting events in ms (default: "0")
//   MOCK_HARNESS_FAIL_START  - if "true", exit with error on start
//   MOCK_HARNESS_EMIT_ERROR  - if "true", emit an error event instead of result
//
// Invoked with -discover it does not read stdin at all: it writes its on-disk
// sessions to stdout as a JSON array and exits. See runDiscover.
//
// Tool-execution phase: every message round-trip now passes through a
// tool_running state (a tool_call, a session_state=tool_running, and a
// tool_result) before the terminating result. Per the interrupt-bug
// finding (docs/findings/2026-07-27-…§5) Claude Code reports tool_running
// while a tool is in flight — the moment a user most often hits Stop — and
// the mock previously never left "running", so that failing state was
// unreachable in tests. A message whose content contains "hang" parks the
// mock in tool_running until it receives SIGINT (the interrupt path), so a
// test can deterministically catch the session mid-tool.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

type request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type startParams struct {
	SessionID   string `json:"session_id"`
	DisplayName string `json:"display_name"`
	AgentID     string `json:"agent_id"`
	Prompt      string `json:"prompt"`
	Resume      bool   `json:"resume"`
	Fork        string `json:"fork"`
}

type messageParams struct {
	Content string `json:"content"`
}

// compactParams mirrors what every real harness declares for the "compact"
// method. The reference harness reads the summary from the params for the same
// reason they do — see the compact case below.
type compactParams struct {
	Summary string `json:"summary"`
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string) bool {
	return os.Getenv(key) == "true"
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return fallback
}

// runDiscover answers the `-discover` flag: it writes the harness's on-disk
// sessions to stdout as a JSON array of msg.StoredSession and exits 0.
//
// The mock keeps no sessions on disk, so the honest answer is an empty array —
// the same answer a real harness gives on a machine it has never run on, and
// the no-op that `-import-history` on a missing session is documented against
// (conformance/runner.go, testImport).
//
// It has to be answered explicitly. Falling through to the JSON-RPC loop on an
// unrecognised flag reads, from outside, as a harness that accepted -discover
// and then wrote nothing: exit 0 with empty stdout, which decodes as neither an
// array nor anything else.
func runDiscover(w io.Writer) error {
	sessions := []msg.StoredSession{}
	return json.NewEncoder(w).Encode(sessions)
}

func main() {
	for _, arg := range os.Args[1:] {
		if arg == "-discover" || arg == "--discover" {
			if err := runDiscover(os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "mock-harness: write discover output: %v\n", err)
				os.Exit(1)
			}
			return
		}
	}

	harnessName := env("MOCK_HARNESS_NAME", "mock")
	sessionID := env("MOCK_HARNESS_SESSION_ID", "mock-session-001")
	delayMS := envInt("MOCK_HARNESS_DELAY_MS", 0)

	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	emit := func(event msg.Event) {
		if delayMS > 0 {
			time.Sleep(time.Duration(delayMS) * time.Millisecond)
		}
		enc.Encode(event)
	}

	emitState := func(state msg.SessionState) {
		emit(msg.Event{
			Type:      msg.EventSessionState,
			Harness:   msg.Harness(harnessName),
			BridgeSessionID: sessionID,
			Timestamp: time.Now(),
			State:     &msg.StateEvent{State: state},
		})
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			fmt.Fprintf(os.Stderr, "mock-harness: invalid request: %v\n", err)
			continue
		}

		switch req.Method {
		case "start":
			if envBool("MOCK_HARNESS_FAIL_START") {
				fmt.Fprintf(os.Stderr, "mock-harness: simulated start failure\n")
				os.Exit(1)
			}

			var sp startParams
			json.Unmarshal(req.Params, &sp)

			// Emit running state
			emitState(msg.SessionRunning)

			// Emit a session_info event with mock metadata so consumers
			// have a chance to render system_prompt, working_dir, tools,
			// etc. without having to wait for a message round-trip.
			emit(msg.Event{
				Type:            msg.EventSessionInfo,
				Harness:         msg.Harness(harnessName),
				BridgeSessionID: sessionID,
				Timestamp:       time.Now(),
				Info: &msg.SessionInfo{
					SystemPrompt: "mock-harness reference system prompt",
					WorkingDir:   "/tmp",
					Model:        "mock-model",
					Tools: []msg.ToolInfo{
						{Name: "echo", Description: "echoes its input"},
					},
				},
			})

			// If there was an initial prompt, process it
			if sp.Prompt != "" {
				emitResult(emit, harnessName, sessionID, sp.Prompt)
			}

		case "message":
			var mp messageParams
			json.Unmarshal(req.Params, &mp)

			emitState(msg.SessionRunning)

			// Echo the user's message as an EventUserMessage so consumers
			// that want a single source of truth for "what did the user
			// send?" can listen on the event stream rather than tracking
			// stdin separately.
			emit(msg.Event{
				Type:            msg.EventUserMessage,
				Harness:         msg.Harness(harnessName),
				BridgeSessionID: sessionID,
				Timestamp:       time.Now(),
				Result:          &msg.ResultEvent{Text: mp.Content},
			})

			emitResult(emit, harnessName, sessionID, mp.Content)

		// The summary arrives in the params, exactly as every real harness
		// reads it. This used to also accept a "compact:<summary>" method,
		// which nothing but this file ever honoured — bridge-server sent that
		// shape and the whole fleet answered "unknown method", so the one
		// implementation that accepted it was the reason the contract looked
		// alive. A reference harness that is more permissive than every real
		// one hides the defect it exists to expose.
		case "compact":
			var cp compactParams
			if len(req.Params) > 0 {
				json.Unmarshal(req.Params, &cp)
			}
			message := "Context compacted"
			if cp.Summary != "" {
				message = "Context compacted with summary: " + cp.Summary
			}
			emit(msg.Event{
				Type:      msg.EventSystem,
				Harness:   msg.Harness(harnessName),
				BridgeSessionID: sessionID,
				Timestamp: time.Now(),
				System:    &msg.SystemEvent{Subtype: "compact_complete", Message: message},
			})

		case "resume":
			emitState(msg.SessionRunning)

		default:
			// "config:<json>" is the one method whose payload really does ride
			// in the name; the harnesses implement it that way and the
			// conformance runner sends it that way.
			if strings.HasPrefix(req.Method, "config:") {
				emit(msg.Event{
					Type:      msg.EventSystem,
					Harness:   msg.Harness(harnessName),
					BridgeSessionID: sessionID,
					Timestamp: time.Now(),
					System:    &msg.SystemEvent{Subtype: "config_updated", Message: "Configuration updated"},
				})
			}
		}
	}
}

func emitResult(emit func(msg.Event), harnessName, sessionID, userMessage string) {
	if envBool("MOCK_HARNESS_EMIT_ERROR") {
		emit(msg.Event{
			Type:      msg.EventError,
			Harness:   msg.Harness(harnessName),
			BridgeSessionID: sessionID,
			Timestamp: time.Now(),
			Error:     &msg.ErrorEvent{Message: "simulated error"},
		})
		return
	}

	// Tool-execution phase. Emit a tool_call, transition to tool_running,
	// then a tool_result — so the tool-in-flight state is exercised
	// end-to-end. When routed through bridge-server the harness-emitted
	// session_state is dropped and tool_running is DERIVED from the
	// tool_call; the explicit session_state here is for consumers that read
	// the mock's raw stream directly (the conformance suite).
	emit(msg.Event{
		Type:            msg.EventToolCall,
		Harness:         msg.Harness(harnessName),
		BridgeSessionID: sessionID,
		Timestamp:       time.Now(),
		ToolCall:        &msg.ToolCallEvent{ToolID: "mock-tool-1", Name: "mock_tool", Input: json.RawMessage(`{}`)},
	})
	emit(msg.Event{
		Type:            msg.EventSessionState,
		Harness:         msg.Harness(harnessName),
		BridgeSessionID: sessionID,
		Timestamp:       time.Now(),
		State:           &msg.StateEvent{State: msg.SessionToolRunning},
	})

	// Hang mode: park in tool_running until interrupted (SIGINT — the path
	// Process.Interrupt drives). Block on a signal channel rather than a bare
	// select{} so Go's deadlock detector doesn't reap the process; on SIGINT
	// exit cleanly, which the manager observes as the harness exiting. No
	// tool_result / result / idle is emitted — the interrupt is the terminator.
	if strings.Contains(strings.ToLower(userMessage), "hang") {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		<-sigCh
		os.Exit(0)
	}

	emit(msg.Event{
		Type:            msg.EventToolResult,
		Harness:         msg.Harness(harnessName),
		BridgeSessionID: sessionID,
		Timestamp:       time.Now(),
		ToolResult:      &msg.ToolResultEvent{ToolID: "mock-tool-1", Name: "mock_tool", Output: "mock tool output"},
	})

	responseText := "Mock response to: " + userMessage

	// Emit a stream event
	emit(msg.Event{
		Type:      msg.EventStream,
		Harness:   msg.Harness(harnessName),
		BridgeSessionID: sessionID,
		Timestamp: time.Now(),
		Stream: &msg.HarnessStream{Delta: &msg.BlockDelta{
			Type: msg.DeltaText,
			Text: responseText,
		}},
	})

	// Emit a finished content block alongside the stream — distinct from
	// EventStream (incremental delta) because EventBlock carries one
	// finished block. Consumers that prefer post-finalized content over
	// streaming deltas listen on EventBlock.
	emit(msg.Event{
		Type:            msg.EventBlock,
		Harness:         msg.Harness(harnessName),
		BridgeSessionID: sessionID,
		Timestamp:       time.Now(),
		Block: &msg.BlockEvent{
			Index: 0,
			Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: responseText}},
		},
	})

	// Scenario-specific events the conformance suite probes for via
	// dedicated trigger prompts. Real harnesses emit these only when the
	// underlying agent's behavior surfaces them — mock-harness emits them
	// here so the reference implementation passes every conformance test.
	if strings.Contains(strings.ToLower(userMessage), "plan") {
		emit(msg.Event{
			Type:            msg.EventPlan,
			Harness:         msg.Harness(harnessName),
			BridgeSessionID: sessionID,
			Timestamp:       time.Now(),
			Plan:            &msg.PlanEvent{Text: "1. step one\n2. step two\n3. step three"},
		})
	}
	// A single event line far larger than any reader's working buffer. Real
	// harnesses produce these routinely — a base64 screenshot from the
	// Playwright MCP, a large file read — and the gateway must deliver the
	// event whole and keep the session running. When the stdout reader was a
	// bufio.Scanner capped at 1MB, this line ended the read loop, which the
	// manager could not distinguish from the harness exiting.
	if strings.Contains(strings.ToLower(userMessage), "oversized") {
		emit(msg.Event{
			Type:            msg.EventToolResult,
			Harness:         msg.Harness(harnessName),
			BridgeSessionID: sessionID,
			Timestamp:       time.Now(),
			ToolResult: &msg.ToolResultEvent{
				ToolID: "oversized-1",
				Name:   "Read",
				Output: strings.Repeat("x", 2*1024*1024),
			},
		})
	}
	if strings.Contains(strings.ToLower(userMessage), "hook") {
		emit(msg.Event{
			Type:            msg.EventHook,
			Harness:         msg.Harness(harnessName),
			BridgeSessionID: sessionID,
			Timestamp:       time.Now(),
			Hook: &msg.HookEvent{
				Event:    "PreToolUse",
				ToolName: "echo",
				Phase:    "completed",
				Decision: "allow",
			},
		})
	}

	// Emit result
	emit(msg.Event{
		Type:      msg.EventResult,
		Harness:   msg.Harness(harnessName),
		BridgeSessionID: sessionID,
		Timestamp: time.Now(),
		Result:    &msg.ResultEvent{Text: responseText},
	})

	// Emit idle state
	emit(msg.Event{
		Type:      msg.EventSessionState,
		Harness:   msg.Harness(harnessName),
		BridgeSessionID: sessionID,
		Timestamp: time.Now(),
		State:     &msg.StateEvent{State: msg.SessionIdle},
	})
}
