package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/executor"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/modelstoreroles"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/sessionmodelrole"
	"github.com/kayushkin/llm-bridge/msg"
)

// The scheduler inside this server reads instances, sessions, model roles
// and harness providers by direct calls, with the answers the routes gave.
func TestTheSchedulerReadsThisServerInProcess(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	bridge := gated.server.SchedulerBridge()

	instances, err := bridge.Instances()
	if err != nil || len(instances) != 2 || instances[0].Machine == nil {
		t.Fatalf("Instances = %+v, %v; want both test instances with their machines", instances, err)
	}
	if _, err := bridge.Session("br_nobody"); !errors.Is(err, executor.ErrSessionNotFound) {
		t.Errorf("an unknown session = %v, want ErrSessionNotFound", err)
	}
	created := gated.createSessionAs(t, gated.loginAs(t, firstTestPrincipalID), firstTestPrincipalID)
	if session, err := bridge.Session(created.SessionID); err != nil || session.SessionID != created.SessionID {
		t.Errorf("Session(%s) = %+v, %v", created.SessionID, session, err)
	}

	roles := modelstoreroles.Checker{Roles: gated.server.SchedulerModelStoreRoles()}
	if err := roles.CheckRole("default"); err != nil {
		t.Errorf("the default role: %v", err)
	}
	if err := roles.CheckRole("cheapest"); err == nil || !strings.Contains(err.Error(), `no role "cheapest"`) {
		t.Errorf("an unknown role: %v", err)
	}

	checker := sessionmodelrole.SourcesChecker{Sources: gated.server.SchedulerModelRoleSources()}
	if model, err := checker.Check("default", msg.HarnessMock); err != nil || model.ID != "mock-model" {
		t.Errorf("the default role on the mock harness = %+v, %v", model, err)
	}
	if _, err := checker.Check("default", msg.HarnessClaudeCode); err == nil || !strings.Contains(err.Error(), "runs only") {
		t.Errorf("a mock model on claude_code = %v, want the provider refusal", err)
	}
}
