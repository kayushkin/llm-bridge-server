package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type answer struct {
	Items []string `json:"items"`
}

func testConfig() Config {
	return Config{APIKey: "test-key", Model: "claude-test", MaxTokens: 1024, Timeout: 5 * time.Second}
}

// serve points MessagesURL at a stub for the duration of one test.
func serve(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	previous := MessagesURL
	MessagesURL = srv.URL
	t.Cleanup(func() {
		MessagesURL = previous
		srv.Close()
	})
}

func TestInvokeToolDecodesTheToolInput(t *testing.T) {
	serve(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != APIVersion {
			t.Errorf("anthropic-version = %q", got)
		}
		var sent map[string]any
		json.NewDecoder(r.Body).Decode(&sent)
		choice, _ := sent["tool_choice"].(map[string]any)
		if choice["type"] != "tool" || choice["name"] != "report" {
			t.Errorf("tool_choice = %v, want the tool forced", choice)
		}
		w.Write([]byte(`{"stop_reason":"tool_use","content":[
			{"type":"tool_use","name":"report","input":{"items":["a","b"]}}]}`))
	})

	var got answer
	if err := InvokeTool(context.Background(), testConfig(), "sys", map[string]any{"name": "report"}, "report", "go", &got); err != nil {
		t.Fatalf("InvokeTool: %v", err)
	}
	if len(got.Items) != 2 || got.Items[0] != "a" {
		t.Fatalf("decoded %+v", got)
	}
}

// The failure this package exists to catch. A truncated reply still carries a
// tool_use block whose partial input parses cleanly, so a caller that trusts
// the decode acts on half an answer.
func TestATruncatedReplyIsAnErrorEvenThoughItDecodes(t *testing.T) {
	serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"stop_reason":"max_tokens","usage":{"output_tokens":1024},
			"content":[{"type":"tool_use","name":"report","input":{"items":["a"]}}]}`))
	})

	var got answer
	err := InvokeTool(context.Background(), testConfig(), "sys", map[string]any{}, "report", "go", &got)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
	if len(got.Items) != 0 {
		t.Fatalf("a truncated reply was decoded into %+v", got)
	}
}

func TestAReplyWithNoMatchingToolIsAnError(t *testing.T) {
	serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"stop_reason":"end_turn","content":[{"type":"text","text":"I would rather chat"}]}`))
	})
	var got answer
	if err := InvokeTool(context.Background(), testConfig(), "s", map[string]any{}, "report", "go", &got); err == nil {
		t.Fatal("a reply with no tool call was accepted")
	}
}

func TestAnAPIErrorCarriesItsStatusAndBody(t *testing.T) {
	serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	})
	var got answer
	err := InvokeTool(context.Background(), testConfig(), "s", map[string]any{}, "report", "go", &got)
	if err == nil {
		t.Fatal("a 429 was treated as success")
	}
	if !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("error does not say what happened: %v", err)
	}
}

func TestAnIncompleteConfigIsRefusedBeforeAnyRequest(t *testing.T) {
	serve(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a request was sent despite an invalid config")
	})
	for name, cfg := range map[string]Config{
		"no key":    {Model: "m", MaxTokens: 10},
		"no model":  {APIKey: "k", MaxTokens: 10},
		"no tokens": {APIKey: "k", Model: "m"},
	} {
		t.Run(name, func(t *testing.T) {
			var got answer
			if err := InvokeTool(context.Background(), cfg, "s", map[string]any{}, "report", "go", &got); err == nil {
				t.Fatal("an incomplete config was accepted")
			}
		})
	}
}

func TestResolveKeyPrefersAnAPIKeyAndFallsBackToAToken(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"api key":      {`{"api_key":"sk-1"}`, "sk-1"},
		"access token": {`{"access_token":"oauth-1"}`, "oauth-1"},
		"both":         {`{"api_key":"sk-1","access_token":"oauth-1"}`, "sk-1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Auth-App") != "synthetic-mail" {
					t.Errorf("X-Auth-App = %q; auth-store's audit log needs the caller", r.Header.Get("X-Auth-App"))
				}
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			got, err := ResolveKey(context.Background(), srv.URL, "tok", "synthetic-mail", "why")
			if err != nil {
				t.Fatalf("ResolveKey: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveKeyRefusesACredentialWithNeither(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if _, err := ResolveKey(context.Background(), srv.URL, "tok", "app", "why"); err == nil {
		t.Fatal("an empty credential was accepted")
	}
}
