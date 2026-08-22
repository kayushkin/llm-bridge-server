//go:build convenience_events_integration

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Convenience-events end-to-end integration test. Live, slow, and
// assumes:
//
//   - `llm-bridge-claudecode` is on PATH (the harness wrapper).
//   - `claude` is on PATH (the upstream CLI it spawns).
//   - Whatever credential storage `claude` reads from is populated so a
//     real LLM round-trip can complete. The test asserts that derived
//     convenience events (agent_state, usage_total, turn_complete) flow
//     in-band on the SSE feed alongside the raw event stream — it does
//     not assert what claude says back.
//
// Gated by `//go:build convenience_events_integration` so `go test ./...`
// skips it by default. Run locally with:
//
//	go test -tags convenience_events_integration -run TestConvenienceEventsIntegration ./internal/server/...
//
// See README.md → "Live convenience-events integration test".

const (
	convClaudecodeBin = "llm-bridge-claudecode"
	convClaudeBin     = "claude"

	// First-paint budget: how long we wait after /send before the SSE
	// stream produces its first derived event. Real claude turns can
	// take 20–40s on first run (asar load, model warmup) before any
	// stream/result lands; 60s gives generous headroom.
	convDeriveTimeout = 60 * time.Second
)

func TestConvenienceEventsIntegration_ClaudeCode_TurnSequence(t *testing.T) {
	if _, err := exec.LookPath(convClaudecodeBin); err != nil {
		t.Skipf("%s not in PATH: %v", convClaudecodeBin, err)
	}
	if _, err := exec.LookPath(convClaudeBin); err != nil {
		t.Skipf("%s not in PATH: %v", convClaudeBin, err)
	}

	// /send pushes the user_message to log-store before broadcasting on
	// SSE; without a reachable log-store the call 500s and the SSE feed
	// never sees the user_message echo (or any derived events for the
	// turn). Stub it with an httptest 200-on-everything server so the
	// production push path is exercised end-to-end without hitting a
	// real log-store backend.
	logStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(logStore.Close)

	srv, _, instID := testServerWithInstanceAndLogStore(t, msg.HarnessClaudeCode, logStore.URL)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	bridgeID := createEventsSession(t, ts.URL, instID)

	// Subscribe to SSE *before* /send so we don't miss the user_message
	// echo or its derived agent_state(idle→tool_running). The handler
	// replays current-turn events on first connect, so a late subscriber
	// would still catch up — but live capture is closer to how real
	// consumers (bridge-ui, llmux) attach.
	sseCtx, cancelSSE := context.WithTimeout(context.Background(), convDeriveTimeout+15*time.Second)
	t.Cleanup(cancelSSE)

	events := make(chan sseEvent, 256)
	sseDone := make(chan error, 1)
	go func() {
		sseDone <- streamSSE(sseCtx, ts.URL+"/sessions/"+bridgeID+"/events", events)
	}()

	// The prompt MUST provoke a tool call. The assertions below require a
	// transition into tool_running, and the comment beneath this line
	// reasons entirely about a tool-using turn — but the prompt shipped in
	// 1719699 was "Reply with just the word 'ok' and nothing else.",
	// written to suppress exactly the tool call the test then demanded. The
	// file is behind a build tag, so no default run ever built it and the
	// contradiction stood from 2026-04-28 until it was first executed.
	// Keep the tool call, or drop the tool_running assertion with it.
	sendUserMessage(t, ts.URL, bridgeID, "Run the bash command `echo ok` and then reply with just the word ok.")

	// Read SSE until we've collected the convenience-event triple for
	// this turn — session_state, usage_total, turn_complete — or the
	// timeout expires.
	//
	// A tool-using turn walks idle → model_generating → tool_running →
	// model_generating → idle. It opens on model_generating because the
	// user message arrives before any tool call does (derivation.go,
	// "turn_started"), and it leaves tool_running for model_generating
	// once the last tool result drains ("tools_drained"). So the closing
	// transition is model_generating→idle, and idle→tool_running does not
	// occur at all.
	//
	// We don't pin the order between that closing session_state and
	// usage_total/turn_complete (spec leans usage_total → turn_complete
	// after it, but consumers shouldn't rely on a specific interleaving).
	// What we DO assert: at least one transition into tool_running, at
	// least one back to idle from some non-idle state, exactly one
	// usage_total for the turn, and one turn_complete carrying the same
	// turn_id.
	var (
		sessionStates  []*msg.StateEvent
		usageTotals    []*msg.UsageTotalEvent
		turnCompletes  []*msg.TurnCompleteEvent
		seenTurnIDs    = map[string]bool{}
		sawUserMessage bool
	)
	deadline := time.Now().Add(convDeriveTimeout)

	for {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for full convenience-event triple; got agent_state=%d usage_total=%d turn_complete=%d sawUserMessage=%v",
				convDeriveTimeout, len(sessionStates), len(usageTotals), len(turnCompletes), sawUserMessage)
		}

		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("SSE stream closed before convenience-event triple landed; got agent_state=%d usage_total=%d turn_complete=%d",
					len(sessionStates), len(usageTotals), len(turnCompletes))
			}
			parsed, err := parseEventBody(ev)
			if err != nil {
				t.Fatalf("parse SSE event %q: %v\nraw: %s", ev.event, err, ev.data)
			}
			switch parsed.Type {
			case msg.EventUserMessage:
				sawUserMessage = true
				if parsed.TurnID != "" {
					seenTurnIDs[parsed.TurnID] = true
				}
			case msg.EventSessionState:
				if parsed.State == nil {
					t.Fatalf("agent_state event with nil body: %+v", parsed)
				}
				sessionStates = append(sessionStates, parsed.State)
				if parsed.TurnID != "" {
					seenTurnIDs[parsed.TurnID] = true
				}
			case msg.EventUsageTotal:
				if parsed.UsageTotal == nil {
					t.Fatalf("usage_total event with nil body: %+v", parsed)
				}
				usageTotals = append(usageTotals, parsed.UsageTotal)
				if parsed.TurnID != "" {
					seenTurnIDs[parsed.TurnID] = true
				}
			case msg.EventTurnComplete:
				if parsed.TurnComplete == nil {
					t.Fatalf("turn_complete event with nil body: %+v", parsed)
				}
				turnCompletes = append(turnCompletes, parsed.TurnComplete)
				if parsed.TurnID != "" {
					seenTurnIDs[parsed.TurnID] = true
				}
			}

			if haveTurnTriple(sessionStates, usageTotals, turnCompletes) {
				goto done
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("timed out after %s waiting for events; got agent_state=%d usage_total=%d turn_complete=%d",
				convDeriveTimeout, len(sessionStates), len(usageTotals), len(turnCompletes))
		}
	}

done:
	cancelSSE()
	<-sseDone

	// Assertions on the triple. Each derived event MUST carry the same
	// turn_id as the user_message that opened the turn, otherwise the
	// "consumers can correlate cause and effect" contract from the spec
	// is broken.
	if !sawUserMessage {
		t.Fatalf("SSE feed never delivered the user_message — server didn't broadcast /send")
	}
	if len(seenTurnIDs) != 1 {
		t.Fatalf("expected exactly one turn_id in SSE feed, got %d: %v", len(seenTurnIDs), seenTurnIDs)
	}

	var startTransition, endTransition bool
	for _, st := range sessionStates {
		// Into tool_running from wherever. Pinning the predecessor to
		// idle made this unreachable: the turn is already in
		// model_generating by the time the first tool call arrives.
		if st.State == msg.SessionToolRunning {
			startTransition = true
		}
		if st.State == msg.SessionIdle && st.Previous != "" && st.Previous != msg.SessionIdle {
			endTransition = true
		}
	}
	if !startTransition {
		t.Errorf("missing session_state transition into tool_running; got %+v", sessionStates)
	}
	if !endTransition {
		t.Errorf("missing session_state →idle transition (turn never closed); got %+v", sessionStates)
	}

	if len(usageTotals) != 1 {
		t.Errorf("expected exactly 1 usage_total for the turn, got %d", len(usageTotals))
	} else {
		ut := usageTotals[0]
		if ut.Turns != 1 {
			t.Errorf("usage_total.Turns = %d; want 1 (this is the first turn)", ut.Turns)
		}
		// The total token counts must be > 0 to prove the derivation
		// folded a real ResultEvent.Usage payload, not a zero-valued
		// placeholder. Real claude turns always report at least input
		// tokens (the prompt itself).
		if ut.Usage.InputTokens == 0 && ut.Usage.OutputTokens == 0 && ut.Usage.TotalTokens == 0 {
			t.Errorf("usage_total payload has all-zero token counts; derivation didn't fold ResultEvent.Usage: %+v", ut.Usage)
		}
	}

	if len(turnCompletes) != 1 {
		t.Errorf("expected exactly 1 turn_complete for the turn, got %d", len(turnCompletes))
	} else {
		tc := turnCompletes[0]
		if tc.TurnID == "" {
			t.Errorf("turn_complete has empty turn_id")
		}
		if !seenTurnIDs[tc.TurnID] {
			t.Errorf("turn_complete.TurnID = %q not in seen set %v (cause-effect correlation broken)", tc.TurnID, seenTurnIDs)
		}
	}

	// Clean teardown — exercises /stop on a session that's just gone
	// idle. Manager.Kill should be a no-op-fast on an already-idle
	// session; the row should reach a terminal state regardless.
	stopResp, err := http.Post(ts.URL+"/sessions/"+bridgeID+"/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	stopResp.Body.Close()
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, want 200", stopResp.StatusCode)
	}
}

// haveTurnTriple returns true once we've collected the closing-side
// convenience events for a turn: at least one agent_state into idle, one
// usage_total, and one turn_complete. Used to short-circuit the read
// loop so the test doesn't burn its full timeout once the relevant
// events have landed.
func haveTurnTriple(states []*msg.StateEvent, totals []*msg.UsageTotalEvent, completes []*msg.TurnCompleteEvent) bool {
	if len(totals) == 0 || len(completes) == 0 {
		return false
	}
	for _, st := range states {
		if st.State == msg.SessionIdle && st.Previous != "" && st.Previous != msg.SessionIdle {
			return true
		}
	}
	return false
}

// createEventsSession POSTs a default-mode (events) session with
// auto_start and returns its bridge id. Fails the test on any
// non-201 response.
func createEventsSession(t *testing.T, baseURL, instID string) string {
	t.Helper()
	body := msg.CreateSessionRequest{
		Type:       msg.SessionTypeInteractive,
		Purpose:    msg.PurposeChat,
		Origin:     "test",
		Harness:    msg.HarnessClaudeCode,
		InstanceID: instID,
		AutoStart:  true,
		// Mode left empty → defaults to events at the server.
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal create: %v", err)
	}
	resp, err := http.Post(baseURL+"/sessions", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST /sessions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /sessions status = %d: %s", resp.StatusCode, b)
	}
	var sess msg.ManagedSession
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	// auto_start reports the state the session is actually in when the
	// create response is written: the harness process has been spawned but
	// has not yet announced itself, which is "starting", not "running".
	// Server-side this is sessions.go's auto_start branch, narrowed from
	// running to starting by 72cc3c8 ("Say what a session is actually
	// doing"). Nothing in the default suite pins that branch, so this
	// assertion is the only executable statement of it.
	if sess.State != string(msg.SessionStarting) {
		t.Fatalf("session state = %q, want starting (auto_start)", sess.State)
	}
	return sess.SessionID
}

// sendUserMessage POSTs a /send with the given prompt. Fails the test
// on any non-200 response so callers can treat the result as live.
func sendUserMessage(t *testing.T, baseURL, bridgeID, prompt string) {
	t.Helper()
	body := msg.SendMessageRequest{Message: prompt}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal send: %v", err)
	}
	resp, err := http.Post(baseURL+"/sessions/"+bridgeID+"/send", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST /send: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /send status = %d: %s", resp.StatusCode, b)
	}
}

// sseEvent is a minimal record of one SSE frame: the event type
// (`event:` line) and the JSON payload (`data:` line). id is dropped on
// the floor — tests assert on convenience-event semantics, not on
// rowid stability.
type sseEvent struct {
	event string
	data  []byte
}

// streamSSE reads SSE frames from url and forwards them to out until
// the context is canceled or the response body closes. Returns the
// terminal error (context.Canceled is not surfaced — that's the happy
// path).
func streamSSE(ctx context.Context, url string, out chan<- sseEvent) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("SSE status = %d: %s", resp.StatusCode, b)
	}
	defer close(out)

	r := bufio.NewReader(resp.Body)
	var (
		eventName string
		dataBuf   bytes.Buffer
	)
	flush := func() {
		if eventName == "" && dataBuf.Len() == 0 {
			return
		}
		select {
		case out <- sseEvent{event: eventName, data: append([]byte(nil), dataBuf.Bytes()...)}:
		case <-ctx.Done():
		}
		eventName = ""
		dataBuf.Reset()
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if err == io.EOF {
				flush()
				return nil
			}
			return fmt.Errorf("read SSE: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case strings.HasPrefix(line, "id:"):
			// drop on the floor — tests don't assert on row ids
		}
	}
}

// parseEventBody decodes the SSE data payload into a msg.Event.
func parseEventBody(ev sseEvent) (*msg.Event, error) {
	if len(ev.data) == 0 {
		return nil, fmt.Errorf("empty SSE data")
	}
	var out msg.Event
	if err := json.Unmarshal(ev.data, &out); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return &out, nil
}
