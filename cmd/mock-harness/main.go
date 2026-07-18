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
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
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

func main() {
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

		case "compact", "compact:":
			emit(msg.Event{
				Type:      msg.EventSystem,
				Harness:   msg.Harness(harnessName),
				BridgeSessionID: sessionID,
				Timestamp: time.Now(),
				System:    &msg.SystemEvent{Subtype: "compact_complete", Message: "Context compacted"},
			})

		case "resume":
			// Resume emits no events on its own: SessionState is derived
			// centrally by the server from the raw event stream, and a bare
			// resume produces no new content. (A harness-emitted
			// EventSessionState would be dropped by the server anyway — see
			// manager.go readEvents.)

		default:
			// Handle config: or other prefixed commands
			if strings.HasPrefix(req.Method, "compact:") {
				emit(msg.Event{
					Type:      msg.EventSystem,
					Harness:   msg.Harness(harnessName),
					BridgeSessionID: sessionID,
					Timestamp: time.Now(),
					System:    &msg.SystemEvent{Subtype: "compact_complete", Message: "Context compacted with summary"},
				})
			} else if strings.HasPrefix(req.Method, "config:") {
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
}
