package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/oneshotrouting"
	"github.com/kayushkin/llm-bridge/msg"
	modelstore "github.com/kayushkin/model-store"
)

// routerAnsweringEveryCall is a router over the server's own model-store and
// harness-store, with every provider the test model-store knows mapped to
// instanceID, and answer standing in for the harness.
func routerAnsweringEveryCall(t *testing.T, srv *Server, instanceID string, answer func(msg.OneShotRequest) (msg.OneShotResponse, error)) *oneshotrouting.Router {
	t.Helper()
	if srv.modelStore == nil {
		t.Fatal("the test server has no model-store")
	}
	if err := srv.modelStore.SetRoleModels(modelstore.RoleEfficient, []string{"mock-model-alt", "mock-model"}); err != nil {
		t.Fatalf("seed efficient role: %v", err)
	}
	return &oneshotrouting.Router{
		Models:             srv.modelStore,
		Instances:          srv.harnessStore,
		InstanceByProvider: func() map[string]string { return map[string]string{"mock": instanceID} },
		Call: func(_ context.Context, _ *msg.Instance, request msg.OneShotRequest) (msg.OneShotResponse, error) {
			return answer(request)
		},
		Logf: t.Logf,
	}
}

func postRoutedOneShot(t *testing.T, srv *Server, body string) *http.Response {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("test body is not JSON: %v", err)
	}
	return doJSON(t, srv, "POST", "/oneshot", decoded)
}

func TestRoutedOneShotRouteRefusesAModelAMissingRoleAndAnUnknownRole(t *testing.T) {
	srv, _, instanceID := testServerWithInstance(t, msg.HarnessClaudeCode)
	srv.oneShotRouter = routerAnsweringEveryCall(t, srv, instanceID, func(msg.OneShotRequest) (msg.OneShotResponse, error) {
		t.Fatal("a refused request reached a model")
		return msg.OneShotResponse{}, nil
	})
	for body, code := range map[string]string{
		`{"prompt":"hi","model":"mock-model","model_role":"efficient"}`: "model_not_accepted",
		`{"prompt":"hi","model":"mock-model"}`:                          "model_not_accepted",
		`{"prompt":"hi"}`:                                               oneshotrouting.CodeNoModelRole,
		`{"prompt":"hi","model_role":"cheapest"}`:                       oneshotrouting.CodeUnknownModelRole,
		`{"model_role":"efficient"}`:                                    "prompt_required",
		`{"prompt":"hi","model_role":"efficient","modle":"x"}`:          "invalid_body",
	} {
		response := postRoutedOneShot(t, srv, body)
		var answer struct {
			Error struct{ Code string } `json:"error"`
		}
		json.NewDecoder(response.Body).Decode(&answer)
		if response.StatusCode != http.StatusBadRequest || answer.Error.Code != code {
			t.Errorf("%s: %d %q, want 400 %s", body, response.StatusCode, answer.Error.Code, code)
		}
	}
}

func TestRoutedOneShotRouteAnswersWithTheModelTheInstanceAndTheAttempts(t *testing.T) {
	srv, _, instanceID := testServerWithInstance(t, msg.HarnessClaudeCode)
	var asked []msg.OneShotRequest
	srv.oneShotRouter = routerAnsweringEveryCall(t, srv, instanceID, func(request msg.OneShotRequest) (msg.OneShotResponse, error) {
		asked = append(asked, request)
		if request.Model == "mock-model-alt" {
			return msg.OneShotResponse{}, errors.New("rate limited")
		}
		return msg.OneShotResponse{Text: "hello"}, nil
	})
	response := postRoutedOneShot(t, srv, `{"prompt":"hi","model_role":"efficient","caller":"scheduler-digest"}`)
	var answer msg.OneShotResponse
	json.NewDecoder(response.Body).Decode(&answer)
	if response.StatusCode != http.StatusOK || answer.Text != "hello" || answer.Model != "mock-model" || answer.InstanceID != instanceID {
		t.Fatalf("%d %+v", response.StatusCode, answer)
	}
	if len(answer.Attempts) != 1 || answer.Attempts[0].Model != "mock-model-alt" || !strings.Contains(answer.Attempts[0].Error, "rate limited") {
		t.Errorf("attempts = %+v", answer.Attempts)
	}
	if len(asked) != 2 || asked[1].ModelRole != "" || asked[1].Caller != "scheduler-digest" {
		t.Errorf("the harness was asked %+v; it must get a model, never a role", asked)
	}
}

func TestRoutedOneShotRouteNamesEveryAttemptWhenAllFail(t *testing.T) {
	srv, _, instanceID := testServerWithInstance(t, msg.HarnessClaudeCode)
	srv.oneShotRouter = routerAnsweringEveryCall(t, srv, instanceID, func(request msg.OneShotRequest) (msg.OneShotResponse, error) {
		return msg.OneShotResponse{}, errors.New(request.Model + " is down")
	})
	response := postRoutedOneShot(t, srv, `{"prompt":"hi","model_role":"efficient"}`)
	var answer struct {
		Error    struct{ Code, Message string } `json:"error"`
		Attempts []msg.OneShotAttempt           `json:"attempts"`
	}
	json.NewDecoder(response.Body).Decode(&answer)
	if response.StatusCode != http.StatusBadGateway || answer.Error.Code != "all_models_failed" || len(answer.Attempts) != 2 {
		t.Fatalf("%d %+v", response.StatusCode, answer)
	}
	for _, model := range []string{"mock-model-alt is down", "mock-model is down"} {
		if !strings.Contains(answer.Error.Message, model) {
			t.Errorf("the error does not name %q: %s", model, answer.Error.Message)
		}
	}
}

func TestAModelRoleSettingRefusesARoleModelStoreDoesNotHave(t *testing.T) {
	srv, _, _ := testServerWithInstance(t, msg.HarnessClaudeCode)
	put := func(key, value string) *http.Response {
		return doJSON(t, srv, "PUT", "/settings/"+key, map[string]string{"value": value})
	}
	for _, key := range config.ModelRoleSettings {
		if response := put(key, "cheapest"); response.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = cheapest: %d, want 400", key, response.StatusCode)
		}
		// The test model-store assigns efficient and default, not balanced.
		if response := put(key, "balanced"); response.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = balanced (unassigned): %d, want 400", key, response.StatusCode)
		}
		if response := put(key, "efficient"); response.StatusCode != http.StatusOK {
			t.Errorf("%s = efficient: %d, want 200", key, response.StatusCode)
		}
		if got := srv.settings.ModelRole(key); got != "efficient" {
			t.Errorf("%s in force = %q", key, got)
		}
	}
}

func TestTheSignalClassifierAsksItsRoleThroughTheRouter(t *testing.T) {
	srv, _, instanceID := testServerWithInstance(t, msg.HarnessClaudeCode)
	var asked msg.OneShotRequest
	srv.oneShotRouter = routerAnsweringEveryCall(t, srv, instanceID, func(request msg.OneShotRequest) (msg.OneShotResponse, error) {
		asked = request
		return msg.OneShotResponse{Parsed: json.RawMessage(`{"kind":"neither","title":"","body":"","options":[],"severity":""}`)}, nil
	})
	classifier := newSignalClassifier("efficient", 0, 100, nil, srv.routedOneShotJSON)
	verdict, err := classifier.classify(t.Context(), "Done.")
	if err != nil || verdict.Kind != turnSignalNeither {
		t.Fatalf("classify: %+v %v", verdict, err)
	}
	if asked.Model != "mock-model-alt" || asked.Caller != oneShotCallerSignalClassifier {
		t.Errorf("the harness was asked %+v", asked)
	}
}
