package server

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"

	"github.com/kayushkin/llm-bridge-server/internal/access/login"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/executor"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/sessionmodelrole"
)

// modelRolesForTheMockHarness checks every role as one model the mock
// harness runs.
type modelRolesForTheMockHarness struct{}

func (modelRolesForTheMockHarness) Check(role string, harness msg.Harness) (sessionmodelrole.Model, error) {
	return sessionmodelrole.Model{ID: "mock-model", Provider: "mock"}, nil
}

// An agent job starts its session through this server's session core, as the
// principal the scheduler is configured with: the session row carries that
// principal, and its harness child gets the gateway and a token that reaches
// what the principal may, here an operator route. Until 2026-11-27 the job
// published its turn on NATS, the adapter created the session as nobody, and
// the child got neither.
func TestASchedulerAgentJobSessionRunsAsTheConfiguredPrincipalWithItsToken(t *testing.T) {
	recordTo := writeEnvironmentRecordingMockHarness(t)
	gated := newGatedTestServer(t, nil)
	agents := executor.NewAgentExecutor(gated.server.SchedulerBridge(), "http://127.0.0.1:8160/notes", administratorTestPrincipalID,
		func() string { return "default" }, modelRolesForTheMockHarness{})

	result := agents.Run(executor.AgentRun{Prompt: "check the drift", Orchestrator: string(msg.HarnessMock), WorkspaceID: "ws-1",
		SessionWaitLimit: 50 * time.Millisecond})
	// The mock child reads its input and never ends the turn, so the run
	// times out; what matters is the session it started.
	if !strings.Contains(result.Output, "created session scheduler-mock-") || !strings.Contains(result.Output, "as "+administratorTestPrincipalID) {
		t.Fatalf("run = %q %q, want a created session naming its principal", result.Status, result.Output)
	}
	sessionID := strings.Fields(strings.TrimPrefix(result.Output, "created session "))[0]
	t.Cleanup(func() { gated.server.harness.Kill(sessionID) })

	session, err := gated.server.store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("read session %s: %v", sessionID, err)
	}
	if session.PrincipalID != administratorTestPrincipalID {
		t.Errorf("session principal = %q, want %s", session.PrincipalID, administratorTestPrincipalID)
	}
	if session.Origin != "scheduler" || session.Purpose != "scheduled-task" || session.Type != msg.SessionTypeAutonomous {
		t.Errorf("origin %q, purpose %q, type %q", session.Origin, session.Purpose, session.Type)
	}

	var environment []string
	deadline := time.Now().Add(10 * time.Second)
	for environment == nil && time.Now().Before(deadline) {
		if data, err := os.ReadFile(recordTo); err == nil {
			environment = strings.Split(strings.TrimSpace(string(data)), "\n")
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if environment == nil {
		t.Fatal("the harness child never recorded its environment: the send did not start it")
	}
	var gatewayURL, token string
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		switch name {
		case login.GatewayURLEnvironmentVariable:
			gatewayURL = value
		case login.PrincipalTokenEnvironmentVariable:
			token = value
		}
	}
	if gatewayURL != gatedTestServerPublicURL {
		t.Errorf("%s = %q, want %q", login.GatewayURLEnvironmentVariable, gatewayURL, gatedTestServerPublicURL)
	}
	if token == "" {
		t.Fatalf("the harness child got no %s", login.PrincipalTokenEnvironmentVariable)
	}
	if response := gated.requestWithBearer(t, token, "GET", "/machines"); response.Code != http.StatusOK {
		t.Errorf("the child's token on an operator route = %d %s, want 200", response.Code, response.Body.String())
	}
}
