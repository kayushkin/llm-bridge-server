package oneshot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func testConfig(bridgeURL string) Config {
	return Config{BridgeURL: bridgeURL, ModelRole: "efficient", Caller: "kanban-scoper", MaxTokens: 512, Timeout: 5 * time.Second}
}

func TestCallPostsTheRoleAndCallerToTheBridgesOneshotRouteAndNamesNoModel(t *testing.T) {
	var received map[string]any
	var path, method string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		json.NewEncoder(w).Encode(msg.OneShotResponse{
			Parsed: json.RawMessage(`{"answer":"yes"}`), StopReason: "tool_use",
			Model: "claude-haiku-4-5-20251001", InstanceID: "inst-cc-local",
			Attempts: []msg.OneShotAttempt{{Model: "gpt-5.6-luna", InstanceID: "inst-codex", Error: "502 from provider"}},
		})
	}))
	defer server.Close()

	var out struct{ Answer string }
	answer, err := Call(context.Background(), testConfig(server.URL+"/"), "system text", "prompt text",
		map[string]any{"type": "object"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/oneshot" {
		t.Errorf("request = %s %s, want POST /oneshot", method, path)
	}
	if received["model_role"] != "efficient" || received["caller"] != "kanban-scoper" {
		t.Errorf("body = %v, want model_role efficient and caller kanban-scoper", received)
	}
	if _, named := received["model"]; named {
		t.Errorf("body names a model (%v); the bridge refuses a request with both", received["model"])
	}
	if received["prompt"] != "prompt text" || received["system_prompt"] != "system text" || received["max_tokens"] != float64(512) {
		t.Errorf("body = %v, want the prompt, system prompt and max_tokens passed through", received)
	}
	if out.Answer != "yes" {
		t.Errorf("decoded answer = %q, want yes", out.Answer)
	}
	if answer.Model != "claude-haiku-4-5-20251001" || answer.InstanceID != "inst-cc-local" || len(answer.Attempts) != 1 {
		t.Errorf("answer = %+v, want the model, instance and attempts the bridge reported", answer)
	}
	if text := answer.String(); !strings.Contains(text, "after gpt-5.6-luna: 502 from provider") {
		t.Errorf("answer.String() = %q, want the failed attempt named", text)
	}
}

func TestCallPassesTheBridgesRefusalThroughWhole(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"every model of role efficient failed: gpt-5.6-luna: 502; claude-haiku-4-5-20251001: 529"}`, http.StatusBadGateway)
	}))
	defer server.Close()
	_, err := Call(context.Background(), testConfig(server.URL), "", "p", map[string]any{}, &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "claude-haiku-4-5-20251001: 529") || !strings.Contains(err.Error(), "502 Bad Gateway") {
		t.Fatalf("err = %v, want the status and every attempt the bridge named", err)
	}
}

func TestCallReportsTruncationAsItsOwnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(msg.OneShotResponse{Parsed: json.RawMessage(`{}`), StopReason: "max_tokens", Model: "gpt-5.6-luna"})
	}))
	defer server.Close()
	answer, err := Call(context.Background(), testConfig(server.URL), "", "p", map[string]any{}, &struct{}{})
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
	if answer.Model != "gpt-5.6-luna" {
		t.Errorf("answer.Model = %q, want the model that was cut off", answer.Model)
	}
}

func TestCallRefusesAReplyWithNoParsedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(msg.OneShotResponse{Text: "I would rather not", StopReason: "end_turn"})
	}))
	defer server.Close()
	_, err := Call(context.Background(), testConfig(server.URL), "", "p", map[string]any{}, &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "no parsed JSON") {
		t.Fatalf("err = %v, want a refusal of a reply with no parsed JSON", err)
	}
}

func TestCallRefusesAnIncompleteConfigBeforeSendingAnything(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"no bridge":     func(c *Config) { c.BridgeURL = "" },
		"no role":       func(c *Config) { c.ModelRole = "" },
		"no caller":     func(c *Config) { c.Caller = "" },
		"no max tokens": func(c *Config) { c.MaxTokens = 0 },
		"no timeout":    func(c *Config) { c.Timeout = 0 },
	} {
		cfg := testConfig("http://127.0.0.1:1")
		mutate(&cfg)
		if _, err := Call(context.Background(), cfg, "", "p", map[string]any{}, &struct{}{}); err == nil {
			t.Errorf("%s: Call accepted the config", name)
		}
	}
}
