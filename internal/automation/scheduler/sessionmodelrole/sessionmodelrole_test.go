package sessionmodelrole

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// stubServices serves model-store's GET /api/roles/{role} and
// llm-bridge-server's GET /harnesses from one server, with the shapes the live
// services answer.
func stubServices(t *testing.T) *httptest.Server {
	t.Helper()
	firstModels := map[string]Model{
		"default":  {ID: "claude-opus-test", Provider: "anthropic"},
		"balanced": {ID: "gpt-test", Provider: "openai"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/harnesses":
			w.Write([]byte(`[{"name":"claude_code","supported_providers":["anthropic"]},{"name":"codex","supported_providers":["openai"]},{"name":"openclaw","supported_providers":null}]`))
		case strings.HasPrefix(r.URL.Path, "/api/roles/"):
			model, ok := firstModels[strings.TrimPrefix(r.URL.Path, "/api/roles/")]
			if !ok {
				http.Error(w, `{"error":"unknown role"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(model)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestCheckReturnsTheRolesFirstModelWhenTheHarnessRunsIt(t *testing.T) {
	server := stubServices(t)
	model, err := New(server.URL, server.URL).Check("default", msg.HarnessClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if model.ID != "claude-opus-test" {
		t.Errorf("model = %q, want claude-opus-test", model.ID)
	}
	if _, err := New(server.URL, server.URL).Check("balanced", msg.HarnessCodex); err != nil {
		t.Errorf("an openai model on codex was refused: %v", err)
	}
	if _, err := New(server.URL, server.URL).Check("balanced", msg.HarnessOpenClaw); err != nil {
		t.Errorf("a harness that runs any provider was refused: %v", err)
	}
}

func TestCheckRefusesAModelTheHarnessCannotRun(t *testing.T) {
	server := stubServices(t)
	_, err := New(server.URL, server.URL).Check("balanced", msg.HarnessClaudeCode)
	if err == nil || !strings.Contains(err.Error(), "gpt-test") || !strings.Contains(err.Error(), "runs only anthropic") {
		t.Fatalf("err = %v, want a refusal naming the model and what the harness runs", err)
	}
}

func TestCheckRefusesWhatItCannotResolve(t *testing.T) {
	server := stubServices(t)
	for name, testCase := range map[string]struct {
		role    string
		harness msg.Harness
		want    string
	}{
		"unknown role":    {"cheapest", msg.HarnessClaudeCode, "404"},
		"empty role":      {"", msg.HarnessClaudeCode, "no model role"},
		"unknown harness": {"default", msg.Harness("nonesuch"), "does not know harness"},
	} {
		if _, err := New(server.URL, server.URL).Check(testCase.role, testCase.harness); err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, testCase.want)
		}
	}
	if _, err := New("http://127.0.0.1:1", server.URL).Check("default", msg.HarnessClaudeCode); err == nil {
		t.Error("an unreachable model-store was not an error")
	}
}

func TestSetModelRoleKeepsTheRestOfTheHarnessConfig(t *testing.T) {
	create := &msg.CreateSessionRequest{HarnessConfig: json.RawMessage(`{"max_budget_usd":3.5}`)}
	if err := SetModelRole(create, "default"); err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(create.HarnessConfig, &config); err != nil {
		t.Fatal(err)
	}
	if config["model"] != "default" || config["max_budget_usd"] != 3.5 {
		t.Errorf("harness_config = %s, want the role beside the budget", create.HarnessConfig)
	}

	empty := &msg.CreateSessionRequest{}
	if err := SetModelRole(empty, "balanced"); err != nil {
		t.Fatal(err)
	}
	if string(empty.HarnessConfig) != `{"model":"balanced"}` {
		t.Errorf("harness_config = %s", empty.HarnessConfig)
	}

	if err := SetModelRole(&msg.CreateSessionRequest{HarnessConfig: json.RawMessage(`{"model":"x"}`)}, "default"); err == nil {
		t.Error("a second model was written over the first")
	}
	if err := SetModelRole(&msg.CreateSessionRequest{}, ""); err == nil {
		t.Error("an empty role was written")
	}
}
