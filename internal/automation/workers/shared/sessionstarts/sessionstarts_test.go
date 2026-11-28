package sessionstarts

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// bridgeRecording is llm-bridge-server's two session routes, recording each
// request body in the order it came.
type bridgeRecording struct {
	paths        []string
	creates      []msg.CreateSessionRequest
	sends        []msg.SendMessageRequest
	createAnswer int
	createdUnder string
	sendAnswer   int
}

func (bridge *bridgeRecording) serve(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bridge.paths = append(bridge.paths, r.Method+" "+r.URL.EscapedPath())
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			var create msg.CreateSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&create); err != nil {
				t.Errorf("create body: %v", err)
			}
			bridge.creates = append(bridge.creates, create)
			w.WriteHeader(bridge.createAnswer)
			id := create.SessionID
			if bridge.createdUnder != "" {
				id = bridge.createdUnder
			}
			json.NewEncoder(w).Encode(msg.ManagedSession{SessionID: id})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/send"):
			var send msg.SendMessageRequest
			if err := json.NewDecoder(r.Body).Decode(&send); err != nil {
				t.Errorf("send body: %v", err)
			}
			bridge.sends = append(bridge.sends, send)
			w.WriteHeader(bridge.sendAnswer)
			w.Write([]byte(`{"status":"sent"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// A new session is created under the turn's id, with every field the caller
// set, and then sent the message with a client request id, in that order: the
// two calls llm-bridge-adapter made for a bus message.
func TestStartCreatesUnderTheTurnsIDThenSends(t *testing.T) {
	bridge := &bridgeRecording{createAnswer: http.StatusCreated, sendAnswer: http.StatusOK}
	server := bridge.serve(t)
	err := Start(server.URL, Turn{
		SessionID: "tcl-a-1",
		Create:    &msg.CreateSessionRequest{Harness: msg.HarnessClaudeCode, InstanceID: "inst-1", PrincipalID: "principal_000001", Purpose: "dispatcher", Origin: "scheduler", Type: msg.SessionTypeAutonomous, AutoStart: true},
		Send:      msg.SendMessageRequest{Message: "do it"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(bridge.paths, ", ") != "POST /sessions, POST /sessions/tcl-a-1/send" {
		t.Fatalf("calls = %v", bridge.paths)
	}
	create := bridge.creates[0]
	if create.SessionID != "tcl-a-1" || create.PrincipalID != "principal_000001" || create.InstanceID != "inst-1" || !create.AutoStart || create.Purpose != "dispatcher" {
		t.Errorf("create = %+v", create)
	}
	send := bridge.sends[0]
	if send.Message != "do it" || !strings.HasPrefix(send.ClientRequestID, "req_") {
		t.Errorf("send = %+v, want the message and a minted client request id", send)
	}
}

// A turn with no create only sends, and keeps a client request id the caller
// brought.
func TestAContinuedTurnOnlySendsAndKeepsItsRequestID(t *testing.T) {
	bridge := &bridgeRecording{sendAnswer: http.StatusAccepted}
	server := bridge.serve(t)
	if err := Start(server.URL, Turn{SessionID: "herald-7", Send: msg.SendMessageRequest{Message: "again", ClientRequestID: "req_mine"}}); err != nil {
		t.Fatal(err)
	}
	if len(bridge.creates) != 0 || len(bridge.sends) != 1 || bridge.sends[0].ClientRequestID != "req_mine" {
		t.Errorf("creates %v, sends %v", bridge.creates, bridge.sends)
	}
}

// A refused create is an error carrying the bridge's answer, and nothing is
// sent; so is a session made under another id than the one asked for.
func TestARefusedCreateSendsNothing(t *testing.T) {
	bridge := &bridgeRecording{createAnswer: http.StatusForbidden}
	server := bridge.serve(t)
	err := Start(server.URL, Turn{SessionID: "s-1", Create: &msg.CreateSessionRequest{}, Send: msg.SendMessageRequest{Message: "m"}})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want the 403", err)
	}
	if len(bridge.sends) != 0 {
		t.Error("a message was sent to a session that was never created")
	}

	elsewhere := &bridgeRecording{createAnswer: http.StatusCreated, createdUnder: "br_1"}
	server = elsewhere.serve(t)
	if err := Start(server.URL, Turn{SessionID: "s-1", Create: &msg.CreateSessionRequest{}, Send: msg.SendMessageRequest{Message: "m"}}); err == nil || !strings.Contains(err.Error(), "br_1") {
		t.Fatalf("err = %v, want the id the bridge made named", err)
	}
	if len(elsewhere.sends) != 0 {
		t.Error("a message was sent after the bridge made the session under another id")
	}
}

// A refused send is an error carrying the bridge's answer.
func TestARefusedSendIsAnError(t *testing.T) {
	bridge := &bridgeRecording{sendAnswer: http.StatusNotFound}
	server := bridge.serve(t)
	if err := Start(server.URL, Turn{SessionID: "gone", Send: msg.SendMessageRequest{Message: "m"}}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want the 404", err)
	}
}
