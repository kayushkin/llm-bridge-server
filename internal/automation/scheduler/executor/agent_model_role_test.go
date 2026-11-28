package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/sessionmodelrole"
	"github.com/kayushkin/llm-bridge/msg"
)

// stubModelRoles stands in for model-store and the bridge's harness table: the
// default role starts on an anthropic model and balanced on an openai one.
type stubModelRoles struct{ asked []string }

func (s *stubModelRoles) Check(role string, harness msg.Harness) (sessionmodelrole.Model, error) {
	s.asked = append(s.asked, role+"@"+string(harness))
	models := map[string]sessionmodelrole.Model{
		"default":  {ID: "claude-test", Provider: "anthropic"},
		"balanced": {ID: "gpt-test", Provider: "openai"},
	}
	model, ok := models[role]
	if !ok {
		return sessionmodelrole.Model{}, fmt.Errorf("model-store has no role %q", role)
	}
	if harness == msg.HarnessClaudeCode && model.Provider != "anthropic" {
		return sessionmodelrole.Model{}, fmt.Errorf("harness %s runs only anthropic models", harness)
	}
	return model, nil
}

// stubInstances is llm-bridge-server holding one enabled claude_code
// instance.
type stubInstances struct{}

func (stubInstances) Instances() ([]msg.Instance, error) {
	return []msg.Instance{{ID: "inst-cc", HarnessType: msg.HarnessClaudeCode, Enabled: true}}, nil
}

func (stubInstances) Session(sessionID string) (*msg.ManagedSession, error) {
	return nil, fmt.Errorf("%s: %w", sessionID, ErrSessionNotFound)
}

func (stubInstances) CreateSession(ctx context.Context, request msg.CreateSessionRequest) (*msg.ManagedSession, error) {
	return nil, fmt.Errorf("stubInstances creates no session")
}

func (stubInstances) SendMessage(ctx context.Context, sessionID string, request msg.SendMessageRequest) error {
	return fmt.Errorf("stubInstances sends nothing")
}

func harnessConfigModel(t *testing.T, create *msg.CreateSessionRequest) string {
	t.Helper()
	var config map[string]string
	if err := json.Unmarshal(create.HarnessConfig, &config); err != nil {
		t.Fatalf("harness_config %s: %v", create.HarnessConfig, err)
	}
	return config["model"]
}

// A job that names no role takes the setting, read when the run starts; a job
// that names one sends its own. Either way the role goes to the bridge as the
// session's own model choice, so the bridge's per-harness default never
// decides.
func TestNewSessionRequestNamesTheRole(t *testing.T) {
	setting := "default"
	roles := &stubModelRoles{}
	executor := NewAgentExecutor(stubInstances{}, "http://notes.test", "", func() string { return setting }, roles)

	create, startsOn, err := executor.newSessionRequest("claude-code", "claude-code", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := harnessConfigModel(t, create); got != "default" {
		t.Errorf("harness_config.model = %q, want the setting's role", got)
	}
	if create.InstanceID != "inst-cc" || create.Harness != msg.HarnessClaudeCode {
		t.Errorf("create = %+v", create)
	}
	if !strings.Contains(startsOn, "agent_job_model_role") || !strings.Contains(startsOn, "claude-test") {
		t.Errorf("run output %q does not say where the role came from and which model it starts", startsOn)
	}

	setting = "best-of-all"
	if _, _, err := executor.newSessionRequest("claude-code", "claude-code", ""); err == nil {
		t.Error("a changed setting was not read at the next run")
	}

	create, _, err = executor.newSessionRequest("claude-code", "claude-code", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got := harnessConfigModel(t, create); got != "default" {
		t.Errorf("harness_config.model = %q, want the job's role", got)
	}
}

// A role whose first model the harness cannot run starts no session: the run
// fails with the reason, before any instance is asked for.
func TestNewSessionRequestRefusesAModelTheHarnessCannotRun(t *testing.T) {
	executor := NewAgentExecutor(nil, "http://notes.test", "", func() string { return "default" }, &stubModelRoles{})
	_, _, err := executor.newSessionRequest("claude-code", "claude-code", "balanced")
	if err == nil || !strings.Contains(err.Error(), "runs only anthropic") || !strings.Contains(err.Error(), "the job's model_role") {
		t.Fatalf("err = %v, want the harness refusal naming the job's model_role", err)
	}

	empty := NewAgentExecutor(nil, "http://notes.test", "", func() string { return "" }, &stubModelRoles{})
	if _, _, err := empty.newSessionRequest("claude-code", "claude-code", ""); err == nil {
		t.Error("a session was built with no role at all")
	}
	unwired := NewAgentExecutor(nil, "http://notes.test", "", nil, nil)
	if _, _, err := unwired.newSessionRequest("claude-code", "claude-code", "default"); err == nil {
		t.Error("an executor with no role checker built a session")
	}
}
