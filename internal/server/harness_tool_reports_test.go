package server

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/agents/tools"

	"github.com/kayushkin/llm-bridge/msg"
)

// A session's reported built-in tools reach tool-store with its harness; MCP
// tools do not, since tool-store already owns them.
func TestASessionsReportedToolsReachToolStoreWithoutItsMCPTools(t *testing.T) {
	srv, st, instanceID := serverWithHarnessDefaults(t, msg.HarnessDefaults{})
	toolStore := newFakeToolRegistry(t)
	toolStore.harnessTools = []map[string]any{}
	toolStore.created = []string{"NewTool"}
	srv.toolRegistry = toolStore
	sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID})

	srv.onSessionInfo(sess.SessionID, &msg.SessionInfo{Tools: []msg.ToolInfo{{Name: "Read"}, {Name: "mcp__multichat__send"}, {Name: "NewTool"}}})

	if len(toolStore.observed) != 1 {
		t.Fatalf("tool-store was told %d times, want once", len(toolStore.observed))
	}
	got := toolStore.observed[0]
	if got.Harness != string(msg.HarnessClaudeCode) {
		t.Errorf("harness = %v", got.Harness)
	}
	if names := strings.Join(got.ToolNames, ","); names != "Read,NewTool" {
		t.Errorf("tool_names = %s; want the built-ins only", names)
	}
}

// tool-store refusing the report is an error the caller can log, not a silence.
func TestAToolStoreThatRefusesTheReportIsAnError(t *testing.T) {
	srv, _ := testServer(t)
	toolStore := newFakeToolRegistry(t)
	toolStore.recordObservedError = fmt.Errorf("%w: tool name %q contains whitespace", tools.ErrInvalidObservedHarnessTools, "Bad Name")
	srv.toolRegistry = toolStore
	_, err := srv.reportObservedHarnessTools(t.Context(), msg.HarnessClaudeCode, []string{"Bad Name"})
	if err == nil || !errors.Is(err, tools.ErrInvalidObservedHarnessTools) {
		t.Errorf("err = %v; want the store's refusal", err)
	}
}
