package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	schedulerconfig "github.com/slava/scheduler/internal/config"
)

// Ported from 0530193e and 59414aad (test/first-tests-for-kanban-classifier),
// which pinned the request classify sent to api.anthropic.com. The call now goes
// through llm-bridge-server's oneshot route, so the tool_choice, tool-block and
// auth-store key tests of those commits have nothing left to pin (and neither
// does 6ea1096f, which pinned resolveAnthropicKey). What classify puts on the
// wire is still load-bearing and still invisible from the parsed Report: the
// digest is the only thing the model is told about the session, the schema is
// what makes the reply parseable, and max_tokens is the number the truncation
// warning quotes back to an operator.

// captureRequest points classify at a stub oneshot route and returns the decoded
// request body it received.
func captureRequest(t *testing.T, digest string) map[string]any {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		writeJSON(w, http.StatusOK, map[string]any{"stop_reason": "end_turn", "parsed": map[string]any{"trackable": true}})
	}))
	t.Cleanup(srv.Close)

	cfg := config{Model: schedulerconfig.DefaultKanbanClassifierModel, BridgeURL: srv.URL, OneshotInstance: "inst-test"}
	if _, err := classify(context.Background(), cfg, digest); err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got == nil {
		t.Fatal("no request body was captured")
	}
	return got
}

// The digest is the entire input. A request that carries something else is a
// classification of nothing, and it still writes a signature saying the
// response was read.
func TestTheDigestIsWhatTheModelIsAskedAbout(t *testing.T) {
	body := captureRequest(t, "THE-DIGEST-MARKER")

	if body["prompt"] != "THE-DIGEST-MARKER" {
		t.Fatalf("the model was sent %q, not the digest", body["prompt"])
	}
}

func TestTheSystemPromptIsSent(t *testing.T) {
	body := captureRequest(t, "digest")

	if body["system_prompt"] != systemPrompt {
		t.Fatalf("the request carries a system prompt other than the classifier's rules: %q", body["system_prompt"])
	}
}

// The schema is what makes the reply a Report at all. Without it a prose answer
// is legal, and every tick fails as "carried no parsed JSON".
func TestTheReportSchemaIsSent(t *testing.T) {
	body := captureRequest(t, "digest")

	schema, ok := body["schema"].(map[string]any)
	if !ok {
		t.Fatalf("no schema in the request: %v", body)
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, field := range []string{"trackable", "existing_cards", "new_tasks"} {
		if _, ok := properties[field]; !ok {
			t.Errorf("the schema sent does not declare %q: %v", field, properties)
		}
	}
}

// The truncation warning tells an operator to raise max_tokens and quotes the
// number. If the request sends a different one, that instruction is wrong.
func TestTheRequestSendsTheSameTokenCeilingTheTruncationWarningQuotes(t *testing.T) {
	body := captureRequest(t, "digest")

	got, ok := body["max_tokens"].(float64)
	if !ok {
		t.Fatalf("no max_tokens in the request: %v", body)
	}
	if int(got) != maxOutputTokens {
		t.Fatalf("max_tokens on the wire = %d, but the truncation warning quotes %d", int(got), maxOutputTokens)
	}
}

// A non-200 is the bridge relaying a refusal, not a classification. Asserting
// only that SOME error came back is not enough: the status is what tells a rate
// limit from a model that answered badly.
func TestAnHTTPErrorFromTheBridgeIsReportedAsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error"}}`))
	}))
	t.Cleanup(srv.Close)

	cfg := config{Model: schedulerconfig.DefaultKanbanClassifierModel, BridgeURL: srv.URL, OneshotInstance: "inst-test"}
	_, err := classify(context.Background(), cfg, "digest")
	if err == nil {
		t.Fatal("a 429 was accepted as a classification")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("the error does not name the HTTP status, so a rate limit reads as a bad reply: %v", err)
	}
}
