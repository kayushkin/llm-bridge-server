package harness

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// nopWriteCloser lets a Process write its requests into a buffer instead of a
// subprocess's stdin, so a test can read the exact bytes that go on the wire.
type nopWriteCloser struct{ *bytes.Buffer }

func (nopWriteCloser) Close() error { return nil }

func newTestSendProcess(sessionID string) (*Process, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &Process{
		stdin:     nopWriteCloser{buf},
		sessionID: sessionID,
	}, buf
}

// decodeRequest reads the single JSON request a test wrote to the fake stdin.
func decodeRequest(t *testing.T, buf *bytes.Buffer) Request {
	t.Helper()
	var req Request
	if err := json.NewDecoder(buf).Decode(&req); err != nil && err != io.EOF {
		t.Fatalf("decode request: %v", err)
	}
	return req
}

// TestASummaryNeverRidesInTheMethodName is the regression this whole change
// exists for. bridge-server used to send "compact:<summary>" as the method,
// which every harness answers with "unknown method: compact:…" because they all
// switch on the exact string "compact".
func TestASummaryNeverRidesInTheMethodName(t *testing.T) {
	proc, buf := newTestSendProcess("bridge-1")

	params, err := BuildCompactParams("bridge-1", "keep the API design decisions")
	if err != nil {
		t.Fatalf("build params: %v", err)
	}
	if err := proc.SendJSONRPC("compact", params); err != nil {
		t.Fatalf("send: %v", err)
	}

	req := decodeRequest(t, buf)
	if req.Method != "compact" {
		t.Fatalf("method = %q, want exactly %q — a harness switch on \"compact\" must match it", req.Method, "compact")
	}
	if strings.Contains(req.Method, "keep the API design decisions") {
		t.Fatalf("the summary is in the method name: %q", req.Method)
	}

	var got CompactParams
	if err := json.Unmarshal(req.Params, &got); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if got.Summary != "keep the API design decisions" {
		t.Fatalf("summary = %q, want it carried in the params", got.Summary)
	}
	if got.BridgeSessionID != "bridge-1" {
		t.Fatalf("bridge_session_id = %q, want it preserved alongside the summary", got.BridgeSessionID)
	}
}

// TestACompactWithNoSummaryIsUnchangedOnTheWire is the complement, and it is
// the assertion that matters most: the no-summary compact is the one path that
// works today against every deployed harness binary, so the fix must not move
// a single byte of it.
func TestACompactWithNoSummaryIsUnchangedOnTheWire(t *testing.T) {
	before, beforeBuf := newTestSendProcess("bridge-1")
	if err := before.SendCommand("compact"); err != nil {
		t.Fatalf("send command: %v", err)
	}

	after, afterBuf := newTestSendProcess("bridge-1")
	params, err := BuildCompactParams("bridge-1", "")
	if err != nil {
		t.Fatalf("build params: %v", err)
	}
	if err := after.SendJSONRPC("compact", params); err != nil {
		t.Fatalf("send jsonrpc: %v", err)
	}

	if beforeBuf.String() != afterBuf.String() {
		t.Fatalf("wire shape changed for a compact with no summary:\n old: %s new: %s", beforeBuf.String(), afterBuf.String())
	}
}

// TestAnEmptySummaryIsOmittedRatherThanSentEmpty pins the tag that makes the
// test above possible. A harness that checks `if params.Summary != ""` behaves
// the same either way, but one that checks for the key's presence does not.
func TestAnEmptySummaryIsOmittedRatherThanSentEmpty(t *testing.T) {
	params, err := BuildCompactParams("bridge-1", "")
	if err != nil {
		t.Fatalf("build params: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(params, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["summary"]; ok {
		t.Fatalf("params carry an empty summary key: %s", params)
	}
}
