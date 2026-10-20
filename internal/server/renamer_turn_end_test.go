package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge/msg"
)

// transcriptLogStore keeps every event pushed to it and answers the events
// read the renamer makes, so the title call sees the turns the test wrote.
func transcriptLogStore(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var events []json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events") {
			json.NewEncoder(w).Encode(events)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/api/v1/events" && json.Valid(body) {
			events = append(events, json.RawMessage(body))
		}
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// storeTurn writes one turn the way a live session leaves it: the user
// message twice under one turn id (once when the server takes it, once when
// the harness echoes it) and the assistant's result.
func storeTurn(t *testing.T, srv *Server, sessionID, turnID, user, assistant string) {
	t.Helper()
	for _, ev := range []msg.Event{
		{Type: msg.EventUserMessage, BridgeSessionID: sessionID, TurnID: turnID, Result: &msg.ResultEvent{Text: user}},
		{Type: msg.EventUserMessage, BridgeSessionID: sessionID, TurnID: turnID, HarnessSessionID: "echo", Result: &msg.ResultEvent{Text: user}},
		{Type: msg.EventResult, BridgeSessionID: sessionID, TurnID: turnID, Result: &msg.ResultEvent{Text: assistant}},
	} {
		if _, err := srv.harness.BroadcastEvent(&ev); err != nil {
			t.Fatalf("broadcast %s: %v", ev.Type, err)
		}
	}
}

// A session that got one message and was never sent another is the case the
// send-time trigger missed: the renamer must title it once its first turn ends,
// and only for purposes the registry says get a title.
func TestAutoRenameRunsAfterTheFirstTurnForTitledPurposesOnly(t *testing.T) {
	cases := []struct {
		purpose     string
		sessionType msg.SessionType
		wantTitled  bool
	}{
		{msg.PurposeChat, msg.SessionTypeInteractive, true},
		{msg.PurposeAutoworker, msg.SessionTypeAutonomous, false},
		{msg.PurposeDelegate, msg.SessionTypeAutonomous, false},
	}
	for _, c := range cases {
		t.Run(c.purpose, func(t *testing.T) {
			srv, st, instID := testServerWithInstanceAndLogStore(t, msg.HarnessClaudeCode, transcriptLogStore(t).URL)
			resp := doJSON(t, srv, "POST", "/sessions", msg.CreateSessionRequest{
				Type:       c.sessionType,
				Purpose:    c.purpose,
				Origin:     "test",
				Harness:    "claude_code",
				InstanceID: instID,
			})
			if resp.StatusCode != 201 {
				t.Fatalf("create session: status %d", resp.StatusCode)
			}
			created := decodeJSON[msg.ManagedSession](t, resp)
			// The test config names no renamer role, and an empty role turns
			// the renamer off.
			if response := doJSON(t, srv, "PUT", "/settings/"+config.SettingSessionRenamerModelRole, map[string]string{"value": "efficient"}); response.StatusCode != http.StatusOK {
				t.Fatalf("set the renamer role: status %d", response.StatusCode)
			}
			var calls atomic.Int32
			srv.oneShotRouter = routerAnsweringEveryCall(t, srv, instID, func(msg.OneShotRequest) (msg.OneShotResponse, error) {
				calls.Add(1)
				title, _ := json.Marshal(map[string]string{"title": "model bump fix"})
				return msg.OneShotResponse{Parsed: title}, nil
			})

			// Writing a turn's result is what ends a turn: the manager's
			// turn-end observer fires on it, as it does for a live session, so
			// the test calls neither the observer nor the renamer itself.
			waitForCalls := func(want int32) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for calls.Load() < want && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				// Give a call that should not happen the time to.
				time.Sleep(150 * time.Millisecond)
				if got := calls.Load(); got != want {
					t.Fatalf("the renamer ran %d times, want %d", got, want)
				}
				// And let the call that did happen store its title.
				for time.Now().Before(deadline) {
					if _, _, slot, err := st.RenamerState(created.SessionID); err == nil && slot == "" {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatalf("the title call never released the renamer slot")
			}

			storeTurn(t, srv, created.SessionID, "turn_1", "There are breaking changes with CC for the new model", "I read the changelog.")
			if !c.wantTitled {
				waitForCalls(0)
				return
			}
			waitForCalls(1)
			sess, err := st.GetSession(created.SessionID)
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if sess.DisplayName != "model bump fix" {
				t.Errorf("display name = %q, want the title the renamer wrote", sess.DisplayName)
			}
			_, namedAtTurn, _, err := st.RenamerState(created.SessionID)
			if err != nil {
				t.Fatalf("renamer state: %v", err)
			}
			if namedAtTurn != 1 {
				t.Errorf("named at turn %d, want 1: the echoed user_message row must not count as a second turn", namedAtTurn)
			}

			// The next title waits renameEvery turns, counted as turns.
			for turn := 2; turn <= renameEvery; turn++ {
				storeTurn(t, srv, created.SessionID, fmt.Sprintf("turn_%d", turn), "more", "ok")
				waitForCalls(1)
			}
			storeTurn(t, srv, created.SessionID, fmt.Sprintf("turn_%d", renameEvery+1), "more", "ok")
			waitForCalls(2)
		})
	}
}
