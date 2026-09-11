package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/slava/scheduler/internal/kanbanclient"
)

func bridge(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/instances/inst-cc-local":
			w.Write([]byte(`{"id":"inst-cc-local","harness_type":"claude_code","enabled":true}`))
		case "/instances/inst-codex-local":
			w.Write([]byte(`{"id":"inst-codex-local","harness_type":"codex","enabled":true}`))
		case "/agents":
			w.Write([]byte(`[{"id":12,"slug":"argraphments"},{"id":65,"slug":"northwind-builder"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
}

// The harness is the instance's, not a constant: before 2026-09-11 the
// dispatcher spawned every session as claude_code on inst-cc-local, so a board
// pointed at a codex instance would have sent a request the bridge refuses.
func TestTheSessionSpecIsResolvedFromTheBoardThroughTheBridge(t *testing.T) {
	server := bridge(t)
	defer server.Close()

	spec, err := resolveSessionSpec(server.URL, &kanbanclient.Board{
		ID: "b", DefaultInstanceID: "inst-codex-local", DefaultAgentID: "65", DefaultBundleID: "6",
	})
	if err != nil {
		t.Fatalf("a fully set board: %v", err)
	}
	if spec.Harness != msg.HarnessCodex || spec.InstanceID != "inst-codex-local" || spec.AgentSlug != "northwind-builder" || spec.BundleID != "6" {
		t.Errorf("spec = %+v", spec)
	}

	// No agent and no bundle on the board leaves both empty on the request —
	// the bridge then does what it did before boards had either.
	spec, err = resolveSessionSpec(server.URL, &kanbanclient.Board{ID: "b", DefaultInstanceID: "inst-cc-local"})
	if err != nil {
		t.Fatalf("a board with only an instance: %v", err)
	}
	if spec.Harness != msg.HarnessClaudeCode || spec.AgentSlug != "" || spec.BundleID != "" {
		t.Errorf("spec = %+v, want claude_code with no agent and no bundle", spec)
	}
}

func TestABoardWithoutAnInstanceOrWithAnUnknownAgentStopsTheTick(t *testing.T) {
	server := bridge(t)
	defer server.Close()

	_, err := resolveSessionSpec(server.URL, &kanbanclient.Board{ID: "board-1"})
	if err == nil || !strings.Contains(err.Error(), "default_instance_id") {
		t.Errorf("no instance must be refused naming the setting, got %v", err)
	}
	_, err = resolveSessionSpec(server.URL, &kanbanclient.Board{ID: "board-1", DefaultInstanceID: "inst-gone"})
	if err == nil || !strings.Contains(err.Error(), "inst-gone") {
		t.Errorf("an instance the bridge lacks must be refused naming it, got %v", err)
	}
	_, err = resolveSessionSpec(server.URL, &kanbanclient.Board{ID: "board-1", DefaultInstanceID: "inst-cc-local", DefaultAgentID: "404"})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("an agent id the bridge lacks must be refused naming it, got %v", err)
	}
}
