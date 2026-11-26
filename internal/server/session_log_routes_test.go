package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// testServerWithSessionLog is testServer: log-store in this process, on a
// Postgres database of the test's own, as main wires it.
func testServerWithSessionLog(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	return testServer(t)
}

// An event the bridge writes reaches log-store in this process, and every
// route the bridge answers from log-store reads it back: the reading page, the
// entry, the validators, the search, the aggregates and log-store's own
// routes under /log-store/.
func TestTheBridgeWritesAndReadsTheSessionLogInProcess(t *testing.T) {
	srv, st := testServerWithSessionLog(t)
	const sessionID = "br_session_log_test"
	if err := st.CreateSession(&store.Session{SessionID: sessionID, Harness: "claude_code", State: string(msg.SessionIdle), Type: msg.SessionTypeInteractive}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	userMessage := &msg.Event{Type: msg.EventUserMessage, BridgeSessionID: sessionID, Harness: msg.HarnessClaudeCode,
		HarnessSessionID: "cc-uuid-session-log", TurnID: "turn_1", Timestamp: time.Now(),
		Result: &msg.ResultEvent{Text: "find the zebra-quartz note"}}
	userRowID, err := srv.harness.BroadcastEvent(userMessage)
	if err != nil {
		t.Fatalf("BroadcastEvent: %v", err)
	}
	if userRowID <= 0 {
		t.Fatalf("bridge row id = %d", userRowID)
	}
	result := &msg.Event{Type: msg.EventResult, BridgeSessionID: sessionID, Harness: msg.HarnessClaudeCode,
		HarnessSessionID: "cc-uuid-session-log", TurnID: "turn_1", Timestamp: time.Now(),
		Result: &msg.ResultEvent{Text: "done", Usage: msg.TokenUsage{InputTokens: 7, OutputTokens: 3}}}
	if _, err := srv.harness.BroadcastEvent(result); err != nil {
		t.Fatalf("BroadcastEvent: %v", err)
	}

	read := func(path string) string {
		t.Helper()
		resp := doJSON(t, srv, http.MethodGet, path, nil)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, body)
		}
		return string(body)
	}

	var page struct {
		Model struct {
			Turns   []json.RawMessage `json:"turns"`
			Entries map[string]struct {
				EventID int64 `json:"eventId"`
			} `json:"entries"`
		} `json:"model"`
	}
	if err := json.Unmarshal([]byte(read("/sessions/"+sessionID+"/messages?limit=5")), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Model.Turns) != 1 || len(page.Model.Entries) == 0 {
		t.Fatalf("reading page = %+v, want one turn with entries", page)
	}
	var firstEntry int64
	for _, entry := range page.Model.Entries {
		if firstEntry == 0 || entry.EventID < firstEntry {
			firstEntry = entry.EventID
		}
	}
	if !strings.Contains(read("/sessions/"+sessionID+"/entries/"+int64Text(firstEntry)), "zebra-quartz") {
		t.Errorf("the entry does not hold the user's message")
	}
	if !strings.Contains(read("/sessions/search?q=zebra-quartz"), sessionID) {
		t.Errorf("search does not find the session")
	}
	if !strings.Contains(read("/sessions/aggregates"), `"input_tokens":7`) {
		t.Errorf("aggregates do not carry the result's tokens")
	}
	if !strings.Contains(read("/log-store/api/v1/sessions/by-harness-id?harness_session_id=cc-uuid-session-log"), sessionID) {
		t.Errorf("log-store's own route under /log-store/ does not find the session")
	}
	var events []json.RawMessage
	if err := json.Unmarshal([]byte(read("/log-store/api/v1/sessions/"+sessionID+"/events?after=0")), &events); err != nil {
		t.Fatal(err)
	}
	// The two events and the ones the bridge derives from them, such as the
	// session's state.
	if len(events) < 2 {
		t.Fatalf("log-store holds %d events, want at least the 2 written", len(events))
	}
	var validators map[string]struct {
		EventCount int `json:"eventCount"`
	}
	if err := json.Unmarshal([]byte(read("/sessions/validators?ids="+sessionID)), &validators); err != nil {
		t.Fatal(err)
	}
	if validators[sessionID].EventCount != len(events) {
		t.Errorf("validators count %d events, log-store holds %d", validators[sessionID].EventCount, len(events))
	}
}

// Before EnableSessionLog the bridge's log-store routes answer an error rather
// than reach anything.
func TestLogStoreRoutesAnswer503BeforeTheSessionLogIsEnabled(t *testing.T) {
	srv, _ := testServerWithoutSessionLog(t)
	resp := doJSON(t, srv, http.MethodGet, "/log-store/health", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /log-store/health = %d, want 503", resp.StatusCode)
	}
}

func int64Text(number int64) string {
	data, _ := json.Marshal(number)
	return string(data)
}
