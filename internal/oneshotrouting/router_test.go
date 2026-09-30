package oneshotrouting

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	modelstore "github.com/kayushkin/model-store"
)

type fakeModels map[string][]*modelstore.Model

func (models fakeModels) RoleModels(role string) ([]*modelstore.Model, error) {
	list, assigned := models[role]
	if !assigned {
		return nil, fmt.Errorf("role %q is not assigned", role)
	}
	return list, nil
}

func (models fakeModels) ResolveModel(name string) (*modelstore.Model, error) {
	for _, list := range models {
		for _, model := range list {
			if model.ID == name {
				return model, nil
			}
		}
	}
	return nil, fmt.Errorf("model not found: %s", name)
}

type fakeInstances map[string]*msg.Instance

func (instances fakeInstances) GetInstance(id string) (*msg.Instance, error) {
	instance, known := instances[id]
	if !known {
		return nil, errors.New("no such instance")
	}
	return instance, nil
}

type fakeLimits map[string]string

func (limits fakeLimits) ExhaustedWindow(_ context.Context, usageProvider string) (string, error) {
	if limits == nil {
		return "", errors.New("usage-store is down")
	}
	return limits[usageProvider], nil
}

// balancedRouter routes balanced to terra (openai, on codex) then haiku
// (anthropic, on claude_code), as the live model-store did on 2026-09-30.
func balancedRouter(t *testing.T, answer func(instance *msg.Instance, request msg.OneShotRequest) (msg.OneShotResponse, error)) (*Router, *[]string) {
	var logged []string
	return &Router{
		Models: fakeModels{"balanced": {
			{ID: "gpt-5.6-terra", Provider: "openai"},
			{ID: "claude-haiku-4-5-20251001", Provider: "anthropic"},
		}},
		Instances: fakeInstances{
			"inst-codex-local": {ID: "inst-codex-local", HarnessType: msg.HarnessCodex, Enabled: true},
			"inst-cc-local":    {ID: "inst-cc-local", HarnessType: msg.HarnessClaudeCode, Enabled: true},
		},
		InstanceByProvider: func() map[string]string {
			return map[string]string{"openai": "inst-codex-local", "anthropic": "inst-cc-local"}
		},
		Call: func(_ context.Context, instance *msg.Instance, request msg.OneShotRequest) (msg.OneShotResponse, error) {
			return answer(instance, request)
		},
		Logf: func(format string, arguments ...any) {
			line := fmt.Sprintf(format, arguments...)
			logged = append(logged, line)
			t.Log(line)
		},
	}, &logged
}

func answerText(instance *msg.Instance, request msg.OneShotRequest) (msg.OneShotResponse, error) {
	return msg.OneShotResponse{Text: "from " + request.Model}, nil
}

func balancedRequest() msg.OneShotRequest {
	return msg.OneShotRequest{Prompt: "hi", ModelRole: "balanced", Caller: "test-caller"}
}

func TestTheFirstModelOfTheRoleAnswers(t *testing.T) {
	var asked []msg.OneShotRequest
	router, _ := balancedRouter(t, func(instance *msg.Instance, request msg.OneShotRequest) (msg.OneShotResponse, error) {
		asked = append(asked, request)
		return answerText(instance, request)
	})
	response, err := router.RunRole(context.Background(), balancedRequest())
	if err != nil {
		t.Fatal(err)
	}
	if response.Model != "gpt-5.6-terra" || response.InstanceID != "inst-codex-local" || len(response.Attempts) != 0 || response.Text != "from gpt-5.6-terra" {
		t.Errorf("response = %+v", response)
	}
	if len(asked) != 1 || asked[0].Model != "gpt-5.6-terra" || asked[0].ModelRole != "" || asked[0].Caller != "test-caller" {
		t.Errorf("asked = %+v", asked)
	}
}

func TestAFailedModelFallsBackToTheNextAndIsLogged(t *testing.T) {
	router, logged := balancedRouter(t, func(instance *msg.Instance, request msg.OneShotRequest) (msg.OneShotResponse, error) {
		if instance.ID == "inst-codex-local" {
			return msg.OneShotResponse{}, errors.New("exec codex -oneshot: exit status 1")
		}
		return answerText(instance, request)
	})
	response, err := router.RunRole(context.Background(), balancedRequest())
	if err != nil {
		t.Fatal(err)
	}
	if response.Model != "claude-haiku-4-5-20251001" || response.InstanceID != "inst-cc-local" {
		t.Errorf("response = %+v", response)
	}
	if len(response.Attempts) != 1 || response.Attempts[0].Model != "gpt-5.6-terra" || response.Attempts[0].InstanceID != "inst-codex-local" || !strings.Contains(response.Attempts[0].Error, "exit status 1") {
		t.Errorf("attempts = %+v", response.Attempts)
	}
	want := "[oneshot] role=balanced caller=test-caller model gpt-5.6-terra failed: exec codex -oneshot: exit status 1; trying claude-haiku-4-5-20251001"
	if len(*logged) != 1 || (*logged)[0] != want {
		t.Errorf("logged %q, want %q", *logged, want)
	}
}

func TestAModelWhoseSubscriptionIsFullIsSkipped(t *testing.T) {
	var called []string
	router, _ := balancedRouter(t, func(instance *msg.Instance, request msg.OneShotRequest) (msg.OneShotResponse, error) {
		called = append(called, request.Model)
		return answerText(instance, request)
	})
	router.Limits = fakeLimits{"codex": "weekly limit at 100%"}
	response, err := router.RunRole(context.Background(), balancedRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(called) != 1 || called[0] != "claude-haiku-4-5-20251001" {
		t.Errorf("called %v; the codex model should have been skipped", called)
	}
	if len(response.Attempts) != 1 || response.Attempts[0].Skipped != "codex weekly limit at 100%" || response.Attempts[0].Error != "" {
		t.Errorf("attempts = %+v", response.Attempts)
	}
}

func TestAHarnessWithNoUsageProviderAndAnUnreadableUsageStoreSkipNothing(t *testing.T) {
	router, logged := balancedRouter(t, answerText)
	router.Limits = fakeLimits(nil)
	response, err := router.RunRole(context.Background(), balancedRequest())
	if err != nil || response.Model != "gpt-5.6-terra" {
		t.Fatalf("an unreadable usage-store must not skip: %+v %v", response, err)
	}
	if len(*logged) != 1 || !strings.Contains((*logged)[0], "could not be read") {
		t.Errorf("logged %q", *logged)
	}

	router, _ = balancedRouter(t, answerText)
	router.Instances.(fakeInstances)["inst-codex-local"].HarnessType = msg.HarnessHermes
	router.Limits = fakeLimits{"codex": "weekly limit at 100%", "hermes": "everything at 100%"}
	if response, err := router.RunRole(context.Background(), balancedRequest()); err != nil || response.Model != "gpt-5.6-terra" {
		t.Errorf("a harness with no usage provider was skipped: %+v %v", response, err)
	}
}

func TestAProviderWithNoInstanceIsAnAttemptError(t *testing.T) {
	router, _ := balancedRouter(t, answerText)
	router.InstanceByProvider = func() map[string]string { return map[string]string{"anthropic": "inst-cc-local"} }
	response, err := router.RunRole(context.Background(), balancedRequest())
	if err != nil {
		t.Fatal(err)
	}
	if response.Model != "claude-haiku-4-5-20251001" || len(response.Attempts) != 1 || response.Attempts[0].Error != "no instance configured for provider openai (oneshot.instance_by_provider)" {
		t.Errorf("response = %+v", response)
	}
}

func TestWhenEveryModelFailsTheErrorNamesEachAttempt(t *testing.T) {
	router, logged := balancedRouter(t, func(instance *msg.Instance, request msg.OneShotRequest) (msg.OneShotResponse, error) {
		return msg.OneShotResponse{}, errors.New("down")
	})
	router.Limits = fakeLimits{"codex": "weekly limit at 100%"}
	_, err := router.RunRole(context.Background(), balancedRequest())
	var allFailed *AllModelsFailedError
	if !errors.As(err, &allFailed) || len(allFailed.Attempts) != 2 {
		t.Fatalf("err = %v", err)
	}
	want := "role=balanced caller=test-caller: every model failed: gpt-5.6-terra on inst-codex-local skipped: codex weekly limit at 100%; claude-haiku-4-5-20251001 on inst-cc-local failed: down"
	if err.Error() != want {
		t.Errorf("error\n got %s\nwant %s", err, want)
	}
	if last := (*logged)[len(*logged)-1]; !strings.HasSuffix(last, "; no model left") {
		t.Errorf("last log line %q", last)
	}
}

func TestARoleMustBeNamedKnownAndAssigned(t *testing.T) {
	router, _ := balancedRouter(t, answerText)
	for role, code := range map[string]string{"": CodeNoModelRole, "cheapest": CodeUnknownModelRole, "best": CodeModelRoleUnresolved} {
		_, err := router.RunRole(context.Background(), msg.OneShotRequest{Prompt: "hi", ModelRole: role})
		if resolution, ok := IsResolutionError(err); !ok || resolution.Code != code {
			t.Errorf("role %q: %v, want %s", role, err, code)
		}
	}
	if _, err := router.RunRole(context.Background(), msg.OneShotRequest{Prompt: "hi", ModelRole: "balanced", Model: "gpt-5.6-terra"}); err == nil {
		t.Error("a request naming both a model and a role was run")
	}
}

func TestAdmitSkipsAModel(t *testing.T) {
	router, _ := balancedRouter(t, answerText)
	route, err := router.Resolve("balanced")
	if err != nil {
		t.Fatal(err)
	}
	response, err := router.Run(context.Background(), "test-caller", route, msg.OneShotRequest{Prompt: "hi"}, func(candidate Candidate) string {
		if candidate.Provider == "openai" {
			return "no price"
		}
		return ""
	})
	if err != nil || response.Model != "claude-haiku-4-5-20251001" || response.Attempts[0].Skipped != "no price" {
		t.Errorf("%+v %v", response, err)
	}
	single, err := router.Resolve("gpt-5.6-terra")
	if err != nil || single.Role != "" || len(single.Candidates) != 1 {
		t.Errorf("a model id resolves to itself alone: %+v %v", single, err)
	}
}

func TestUsageStoreLimitsReadsAFullFreshWindowAndIgnoresAStaleOne(t *testing.T) {
	now := time.Unix(1790730100, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/usage/limits" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{
			"anthropic":{"windows":{"five_hour":{"used_percent":12},"seven_day":{"used_percent":100}},"stale_after":%d},
			"codex":{"windows":{"weekly":{"used_percent":100}},"stale_after":%d}}`, now.Unix()+60, now.Unix()-60)
	}))
	defer server.Close()
	limits := NewUsageStoreLimits(server.URL)
	limits.now = func() time.Time { return now }
	for provider, want := range map[string]string{"anthropic": "seven_day limit at 100%", "codex": "", "gemini": ""} {
		got, err := limits.ExhaustedWindow(context.Background(), provider)
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", provider, got, err, want)
		}
	}
	if _, err := NewUsageStoreLimits("http://127.0.0.1:1").ExhaustedWindow(context.Background(), "anthropic"); err == nil {
		t.Error("an unreachable usage-store read as no limit instead of an error")
	}
}
