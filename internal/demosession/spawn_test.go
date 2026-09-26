package demosession

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubBridge answers the three calls Spawn makes and keeps the create body.
func stubBridge(t *testing.T, created *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/instances/inst-test":
			w.Write([]byte(`{"id":"inst-test","harness_type":"claude_code"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			if err := json.NewDecoder(r.Body).Decode(created); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"state":"running"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/send"):
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// A session spawned for a card names it, so llm-bridge-server can read the
// card's tags and inject the context sections they select.
func TestASessionForACardNamesTheCard(t *testing.T) {
	var created map[string]any
	bridge := stubBridge(t, &created)
	defer bridge.Close()

	if _, err := Spawn(bridge.URL, Task{
		Prompt: "do it", AgentSlug: "builder", InstanceID: "inst-test", CardID: "card-42",
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if created["card_id"] != "card-42" {
		t.Errorf("card_id in the create body = %v, want card-42", created["card_id"])
	}
}

// A task with no card sends no card_id: an empty one is not a card the
// bridge could look up.
func TestASessionForNoCardSendsNoCardID(t *testing.T) {
	var created map[string]any
	bridge := stubBridge(t, &created)
	defer bridge.Close()

	if _, err := Spawn(bridge.URL, Task{
		Prompt: "do it", AgentSlug: "builder", InstanceID: "inst-test",
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, present := created["card_id"]; present {
		t.Errorf("card_id present in the create body: %v", created["card_id"])
	}
}

// The session id embeds the agent slug, which comes from outside this
// process. A slug holding "/" or "?" must still reach /send as one segment.
func TestTheSendCallKeepsTheSessionIDOnePathSegment(t *testing.T) {
	var sendPath, sendQuery string
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/instances/inst-test":
			w.Write([]byte(`{"id":"inst-test","harness_type":"claude_code"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"state":"running"}`))
		default:
			sendPath, sendQuery = r.URL.EscapedPath(), r.URL.RawQuery
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer bridge.Close()

	sessionID, err := Spawn(bridge.URL, Task{Prompt: "do it", AgentSlug: "bad/slug?x", InstanceID: "inst-test"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	wantPrefix := "/sessions/demo-bad%2Fslug%3Fx-"
	if !strings.HasPrefix(sendPath, wantPrefix) || !strings.HasSuffix(sendPath, "/send") {
		t.Errorf("send path = %q, want %s<nanos>/send (session %q)", sendPath, wantPrefix, sessionID)
	}
	if sendQuery != "" {
		t.Errorf("send carried query %q; the session id leaked past the path", sendQuery)
	}
}
