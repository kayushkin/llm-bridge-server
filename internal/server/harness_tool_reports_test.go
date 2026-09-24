package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// A session's reported built-in tools reach tool-store with its harness; MCP
// tools do not, since tool-store already owns them.
func TestASessionsReportedToolsReachToolStoreWithoutItsMCPTools(t *testing.T) {
	srv, st, instanceID := serverWithHarnessDefaults(t, msg.HarnessDefaults{})
	var got map[string]any
	toolStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/harness-tools/observed" {
			_ = json.NewDecoder(r.Body).Decode(&got)
			_ = json.NewEncoder(w).Encode(map[string]any{"created": []string{"NewTool"}, "seen": 2})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/tools" {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(toolStore.Close)
	srv.cfg.ToolStoreURL = toolStore.URL
	sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID})

	srv.onSessionInfo(sess.SessionID, &msg.SessionInfo{Tools: []msg.ToolInfo{{Name: "Read"}, {Name: "mcp__multichat__send"}, {Name: "NewTool"}}})

	if got == nil {
		t.Fatal("tool-store was not told")
	}
	if got["harness"] != string(msg.HarnessClaudeCode) {
		t.Errorf("harness = %v", got["harness"])
	}
	names, _ := json.Marshal(got["tool_names"])
	if string(names) != `["Read","NewTool"]` {
		t.Errorf("tool_names = %s; want the built-ins only", names)
	}
}

// tool-store refusing the report is an error the caller can log, not a silence.
func TestAToolStoreThatRefusesTheReportIsAnError(t *testing.T) {
	srv, _ := testServer(t)
	toolStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad name"}`, http.StatusBadRequest)
	}))
	t.Cleanup(toolStore.Close)
	srv.cfg.ToolStoreURL = toolStore.URL
	_, err := srv.reportObservedHarnessTools(t.Context(), msg.HarnessClaudeCode, []string{"Read"})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v; want the 400", err)
	}
}
