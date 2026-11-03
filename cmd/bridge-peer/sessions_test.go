package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// fakeProxiedServer answers the connection proxy's routes for one session.
// Its event stream replays the previous turn's result first, and its first
// stream drops before the new turn's result, as a socket that drops would.
type fakeProxiedServer struct {
	mutex            sync.Mutex
	created          []msg.CreateSessionRequest
	clientRequestID  string
	streamsOpened    int
	lastEventIDsSent []string
}

const fakeProxy = "/peer-connections/peer_connection_000001/proxy"

func newFakeProxiedServer(t *testing.T) (*fakeProxiedServer, *httptest.Server) {
	fake := &fakeProxiedServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mutex.Lock()
		defer fake.mutex.Unlock()
		if strings.HasPrefix(r.URL.Path, fakeProxy) && r.Header.Get("Authorization") != "Bearer principal-token" {
			http.Error(w, "the proxy takes the session agent token", http.StatusUnauthorized)
			return
		}
		switch route := strings.TrimPrefix(r.URL.Path, fakeProxy); {
		case r.URL.Path == "/peer-connections":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"connection": map[string]any{"id": "peer_connection_000001", "transport": "dial_out"}, "other_server_name": "laptop", "socket_connected": true}})
		case r.URL.Path == "/server":
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "main"})
		case r.Method == "GET" && route == "/instances":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "inst_000001", "harness_type": "claude_code", "name": "CC"}})
		case r.Method == "POST" && route == "/sessions":
			var request msg.CreateSessionRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			fake.created = append(fake.created, request)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"session_id": "br_000002"})
		case r.Method == "POST" && route == "/sessions/br_000002/send":
			var request msg.SendMessageRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			fake.clientRequestID = request.ClientRequestID
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "sent", "message_id": "msg_000001"})
		case r.Method == "GET" && route == "/sessions/br_000002/events":
			fake.streamsOpened++
			fake.lastEventIDsSent = append(fake.lastEventIDsSent, r.Header.Get("Last-Event-ID"))
			w.Header().Set("Content-Type", "text/event-stream")
			frame := func(id int, event msg.Event) {
				data, _ := json.Marshal(event)
				fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, event.Type, data)
			}
			if fake.streamsOpened == 1 {
				frame(7, msg.Event{Type: msg.EventResult, ClientRequestID: "an-earlier-turn", Result: &msg.ResultEvent{Text: "the old answer"}})
				frame(8, msg.Event{Type: msg.EventSessionState, ClientRequestID: fake.clientRequestID})
				return // the socket drops here
			}
			frame(9, msg.Event{Type: msg.EventResult, ClientRequestID: fake.clientRequestID, Result: &msg.ResultEvent{Text: "the new answer"}})
		default:
			http.Error(w, "not faked: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return fake, server
}

func TestRunStartsASessionThereAndPrintsOnlyItsOwnTurnsResultAcrossADroppedStream(t *testing.T) {
	fake, server := newFakeProxiedServer(t)
	t.Setenv(postingTokenEnvironmentVariable, "posting-token")
	t.Setenv(principalTokenEnvironmentVariable, "principal-token")
	t.Setenv(serviceTokenEnvironmentVariable, "")
	c, err := newClient(server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := c.resolveServer("laptop")
	if err != nil {
		t.Fatal(err)
	}
	var out, progress bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := runOnServer(ctx, c, &out, &progress, connection, "inst_000001", "", "check the disk"); err != nil {
		t.Fatalf("run: %v (progress: %s)", err, progress.String())
	}
	if got := strings.TrimSpace(out.String()); got != "the new answer" {
		t.Fatalf("printed %q, want only this turn's result", got)
	}
	if len(fake.created) != 1 || fake.created[0].Harness != "claude_code" || fake.created[0].InstanceID != "inst_000001" || fake.created[0].ManagerSessionID != "" {
		t.Fatalf("created %+v, want one claude_code session on inst_000001 naming no manager", fake.created)
	}
	if !strings.HasPrefix(fake.created[0].DisplayName, "from main: ") {
		t.Fatalf("display name %q does not say which server asked", fake.created[0].DisplayName)
	}
	if fake.streamsOpened != 2 || fake.lastEventIDsSent[1] != "8" {
		t.Fatalf("streams opened %d with Last-Event-ID %q, want a second one resuming after event 8", fake.streamsOpened, fake.lastEventIDsSent)
	}
}

func TestRunRefusesAnInstanceTheOtherServerDoesNotList(t *testing.T) {
	_, server := newFakeProxiedServer(t)
	t.Setenv(postingTokenEnvironmentVariable, "posting-token")
	t.Setenv(principalTokenEnvironmentVariable, "principal-token")
	t.Setenv(serviceTokenEnvironmentVariable, "")
	c, _ := newClient(server.URL, "")
	connection, _ := c.resolveServer("laptop")
	var out, progress bytes.Buffer
	err := runOnServer(context.Background(), c, &out, &progress, connection, "inst_000009", "", "x")
	if err == nil || !strings.Contains(err.Error(), "lists no instance inst_000009") {
		t.Fatalf("err = %v, want the unknown instance named", err)
	}
}

func TestRunWithoutASessionAgentTokenSaysWhy(t *testing.T) {
	_, server := newFakeProxiedServer(t)
	t.Setenv(postingTokenEnvironmentVariable, "posting-token")
	t.Setenv(principalTokenEnvironmentVariable, "")
	t.Setenv(serviceTokenEnvironmentVariable, "")
	c, _ := newClient(server.URL, "")
	connection := connectionView{}
	connection.Connection.ID = "peer_connection_000001"
	err := listInstances(context.Background(), c, &bytes.Buffer{}, connection)
	if err == nil || !strings.Contains(err.Error(), principalTokenEnvironmentVariable) {
		t.Fatalf("err = %v, want it to name %s", err, principalTokenEnvironmentVariable)
	}
}
