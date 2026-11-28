package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	schedulerconfig "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/config"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/sessionmodelrole"
	"github.com/kayushkin/llm-bridge/msg"
)

// A lane whose role cannot start a session on its harness dispatches nothing,
// takes no card off the queue, and the tick says why and exits 1.
func TestALaneWhoseModelRoleCannotRunDispatchesNothing(t *testing.T) {
	usage := newStubUsage(t, firingLimits())
	kanban := newStubKanban(t, stubQueueCard{cardID: "card-1", title: "do the thing"})
	cfg := tickConfig(t, usage.URL(), kanban.URL(), newStubSpend(t).URL())

	refuse := func(p providerSpec) (string, error) {
		return "", errors.New(`role "balanced" starts a session on gpt-test, and harness claude_code runs only anthropic models`)
	}
	pub := &recordingStarter{}
	var code int
	out := captureLog(t, func() { code = run(cfg, refuse, pub.start) })

	if pub.calls != 0 {
		t.Errorf("started %d workers on a model the harness cannot run", pub.calls)
	}
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if line := shortfallLine(t, out); !strings.Contains(line, "runs only anthropic") {
		t.Errorf("the report does not name the cause: %s", line)
	}
	if len(kanban.moved) != 0 || len(kanban.created) != 0 {
		t.Errorf("the board changed for a worker that never started: moved %v, created %v", kanban.moved, kanban.created)
	}
}

// readLaneModelRole reads the lane's own setting from the scheduler and checks
// the role against the lane's harness.
func TestReadLaneModelRoleReadsTheLanesSettingAndChecksItsHarness(t *testing.T) {
	services := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/settings":
			fmt.Fprintf(w, `{"service":"scheduler","settings":[{"key":%q,"value_type":"model_role","value":"default"},{"key":%q,"value_type":"model_role","value":"default"}]}`,
				schedulerconfig.SettingAutoworkerAnthropicModelRole, schedulerconfig.SettingAutoworkerCodexModelRole)
		case "/api/roles/default":
			fmt.Fprint(w, `{"id":"claude-test","provider":"anthropic"}`)
		case "/harnesses":
			fmt.Fprint(w, `[{"name":"claude_code","supported_providers":["anthropic"]},{"name":"codex","supported_providers":["openai"]}]`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer services.Close()
	read := readLaneModelRole(services.URL, sessionmodelrole.New(services.URL, services.URL))

	anthropic := providerSpec{Name: "anthropic", Harness: msg.HarnessClaudeCode, ModelRoleSetting: schedulerconfig.SettingAutoworkerAnthropicModelRole}
	if role, err := read(anthropic); err != nil || role != "default" {
		t.Errorf("anthropic lane: %q %v, want default", role, err)
	}
	codex := providerSpec{Name: "codex", Harness: msg.HarnessCodex, ModelRoleSetting: schedulerconfig.SettingAutoworkerCodexModelRole}
	if _, err := read(codex); err == nil || !strings.Contains(err.Error(), schedulerconfig.SettingAutoworkerCodexModelRole) || !strings.Contains(err.Error(), "runs only openai") {
		t.Errorf("codex lane on an anthropic model: err = %v, want a refusal naming the setting", err)
	}
}
