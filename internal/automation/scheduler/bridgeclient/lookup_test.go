package bridgeclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// The shapes here are the ones llm-bridge-server answered with on 2026-09-11:
// GET /agents renders id as a JSON number, and kanban-store stores a board's
// default_agent_id as the string "12". The match has to bridge that.
func bridge(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/instances/inst-cc-local":
			w.Write([]byte(`{"id":"inst-cc-local","harness_type":"claude_code","name":"Clawd","enabled":true}`))
		case strings.HasPrefix(r.URL.Path, "/instances/"):
			http.Error(w, `{"error":"instance not found"}`, http.StatusNotFound)
		case r.URL.Path == "/agents":
			w.Write([]byte(`[{"id":17,"slug":"agent-dashboard"},{"id":12,"slug":"argraphments"},{"id":99,"slug":""}]`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestInstanceHarnessTypeIsReadFromTheBridge(t *testing.T) {
	server := bridge(t)
	defer server.Close()

	harness, err := InstanceHarnessType(server.URL, "inst-cc-local")
	if err != nil {
		t.Fatalf("known instance: %v", err)
	}
	if harness != msg.HarnessClaudeCode {
		t.Errorf("harness = %q, want %q", harness, msg.HarnessClaudeCode)
	}

	if _, err := InstanceHarnessType(server.URL, "inst-gone"); err == nil || !strings.Contains(err.Error(), "inst-gone") {
		t.Errorf("an unknown instance must fail naming it, got %v", err)
	}
	if _, err := InstanceHarnessType(server.URL, ""); err == nil {
		t.Error("an empty instance id must not be looked up")
	}
}

func TestAgentSlugIsMatchedOnTheNumericIdAsAString(t *testing.T) {
	server := bridge(t)
	defer server.Close()

	slug, err := AgentSlugByID(server.URL, "12")
	if err != nil {
		t.Fatalf("known agent: %v", err)
	}
	if slug != "argraphments" {
		t.Errorf("slug = %q, want argraphments", slug)
	}

	if _, err := AgentSlugByID(server.URL, "404"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("an unknown agent id must fail naming it, got %v", err)
	}
	if _, err := AgentSlugByID(server.URL, "99"); err == nil {
		t.Error("an agent with no slug cannot start a session and must be refused")
	}
	// The slug is never accepted in the id's place: a board that stored a slug
	// would be a kanban-store bug, and matching on it here would hide it.
	if _, err := AgentSlugByID(server.URL, "argraphments"); err == nil {
		t.Error("a slug passed as an id matched; the join is on the numeric id only")
	}
}
