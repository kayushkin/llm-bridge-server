package harness

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// toolResultLine renders a tool_result event whose output is exactly size
// bytes, so the resulting NDJSON line is at least that large. A base64
// screenshot from the Playwright MCP or a large file read produces exactly
// this shape on the wire.
func toolResultLine(t *testing.T, toolID string, size int) []byte {
	t.Helper()
	ev := msg.Event{
		Type:            msg.EventToolResult,
		Harness:         "claude_code",
		BridgeSessionID: "bridge-1",
		ToolResult: &msg.ToolResultEvent{
			ToolID: toolID,
			Name:   "Read",
			Output: strings.Repeat("x", size),
		},
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(data, '\n')
}

// newTestProcess wires a Process directly to an in-memory stdout so readLoop
// can be exercised without spawning a harness binary.
func newTestProcess(stdout io.Reader) *Process {
	return &Process{
		stdout:    io.NopCloser(stdout),
		sessionID: "bridge-1",
		events:    make(chan msg.Event, 16),
		done:      make(chan struct{}),
	}
}

// collectToolIDs drains p.events until the channel closes, returning the tool
// ids seen in order. A closed channel is how readLoop signals "process exited".
func collectToolIDs(t *testing.T, p *Process) []string {
	t.Helper()
	var got []string
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-p.events:
			if !ok {
				return got
			}
			if ev.ToolResult != nil {
				got = append(got, ev.ToolResult.ToolID)
			}
		case <-timeout:
			t.Fatal("timed out draining events")
		}
	}
}

// TestReadLoopSurvivesOversizedLine pins the central contract of the harness
// stdout reader: an event line larger than the reader's working buffer must
// still be delivered whole, and must never be mistaken for process exit.
//
// Harness bridges read their upstream CLI at a 10MB line cap and pass events
// through unchanged, so a >1MB line on the wire is ordinary. When readLoop
// used a bufio.Scanner capped at 1MB, such a line made Scan() return false,
// which closed p.events. manager.readEvents treats that close as "Process
// exited": it closes every SSE subscriber, deletes the process from the map,
// and zeroes the session PID — while the harness subprocess is still alive and
// still streaming. Everything after the big line is lost, and the orphaned
// subprocess can no longer be interrupted or killed through the server.
func TestReadLoopSurvivesOversizedLine(t *testing.T) {
	var wire []byte
	wire = append(wire, toolResultLine(t, "before", 32)...)
	wire = append(wire, toolResultLine(t, "oversized", 2*1024*1024)...) // 2MB output
	wire = append(wire, toolResultLine(t, "after", 32)...)

	p := newTestProcess(strings.NewReader(string(wire)))
	go p.readLoop()

	got := collectToolIDs(t, p)

	want := []string{"before", "oversized", "after"}
	if len(got) != len(want) {
		t.Fatalf("delivered %d events %v, want %d %v — the oversized line killed the read loop", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestReadLoopDeliversOversizedPayloadIntact guards the transparency half: the
// oversized event must arrive byte-for-byte, never truncated to the buffer size.
func TestReadLoopDeliversOversizedPayloadIntact(t *testing.T) {
	const payload = 3 * 1024 * 1024

	p := newTestProcess(strings.NewReader(string(toolResultLine(t, "big", payload))))
	go p.readLoop()

	select {
	case ev, ok := <-p.events:
		if !ok {
			t.Fatal("read loop closed the channel without delivering the oversized event")
		}
		if ev.ToolResult == nil {
			t.Fatal("event carried no tool_result payload")
		}
		if len(ev.ToolResult.Output) != payload {
			t.Fatalf("payload truncated: got %d bytes, want %d", len(ev.ToolResult.Output), payload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the oversized event")
	}
}

// TestReadLoopClosesOnEOF pins the one condition that legitimately means
// "process exited": stdout reaching EOF. The oversized-line fix must not turn
// the read loop into one that hangs there instead.
func TestReadLoopClosesOnEOF(t *testing.T) {
	p := newTestProcess(strings.NewReader(string(toolResultLine(t, "only", 16))))
	go p.readLoop()

	if got := collectToolIDs(t, p); len(got) != 1 || got[0] != "only" {
		t.Fatalf("got %v, want [only]", got)
	}
	// collectToolIDs returned, so the channel closed — EOF still terminates.
}

// TestReadLoopSkipsUnparseableLineWithoutExiting pins that a malformed line is
// a per-line failure, not a session-ending one.
func TestReadLoopSkipsUnparseableLineWithoutExiting(t *testing.T) {
	var wire []byte
	wire = append(wire, toolResultLine(t, "before", 16)...)
	wire = append(wire, []byte("{not json\n")...)
	wire = append(wire, toolResultLine(t, "after", 16)...)

	p := newTestProcess(strings.NewReader(string(wire)))
	go p.readLoop()

	got := collectToolIDs(t, p)
	if len(got) != 2 || got[0] != "before" || got[1] != "after" {
		t.Fatalf("got %v, want [before after]", got)
	}
}
