package codexsubscription

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mintAccessToken builds a JWT-shaped token whose only meaningful claim is exp,
// so expiry handling can be tested without a real credential.
func mintAccessToken(expiry time.Time) string {
	claims, err := json.Marshal(map[string]any{"exp": expiry.Unix()})
	if err != nil {
		panic(err)
	}
	return strings.Join([]string{
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)),
		base64.RawURLEncoding.EncodeToString(claims),
		base64.RawURLEncoding.EncodeToString([]byte("signature")),
	}, ".")
}

func writeAuthFile(t *testing.T, contents map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	encoded, err := json.Marshal(contents)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveSubscriptionAccessTokenReadsTokenAndExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	expiry := now.Add(48 * time.Hour)
	path := writeAuthFile(t, map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": mintAccessToken(expiry),
			"account_id":   "account-123",
		},
		"last_refresh": "2026-08-14T12:29:51Z",
	})

	token, err := ResolveSubscriptionAccessToken(path, now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if token.AccountID != "account-123" {
		t.Errorf("account id = %q, want account-123", token.AccountID)
	}
	if !token.ExpiresAt.Equal(expiry) {
		t.Errorf("expiry = %s, want %s", token.ExpiresAt, expiry)
	}
	if token.LastRefresh.IsZero() {
		t.Error("last refresh not parsed")
	}
}

// An API-key host has no subscription credential to borrow, and saying so beats
// sending a request that fails at the server for an unrelated-looking reason.
func TestResolveSubscriptionAccessTokenRejectsApiKeyMode(t *testing.T) {
	path := writeAuthFile(t, map[string]any{
		"auth_mode": "apikey",
		"tokens":    map[string]any{"access_token": mintAccessToken(time.Now().Add(time.Hour))},
	})
	if _, err := ResolveSubscriptionAccessToken(path, time.Now()); !errors.Is(err, ErrNotSubscriptionAuth) {
		t.Fatalf("error = %v, want ErrNotSubscriptionAuth", err)
	}
}

func TestResolveSubscriptionAccessTokenRejectsExpiredToken(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	path := writeAuthFile(t, map[string]any{
		"auth_mode": "chatgpt",
		"tokens":    map[string]any{"access_token": mintAccessToken(now.Add(-time.Second))},
	})
	if _, err := ResolveSubscriptionAccessToken(path, now); !errors.Is(err, ErrAccessTokenExpired) {
		t.Fatalf("error = %v, want ErrAccessTokenExpired", err)
	}
}

// serveStream runs the client against a canned event stream and reports what the
// server saw, so request shape and response handling are checked together.
func serveStream(t *testing.T, events []string) (*StructuredResponseResult, error, map[string]any, http.Header) {
	t.Helper()
	var seenBody map[string]any
	var seenHeader http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeader = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&seenBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	defer server.Close()

	original := responsesEndpoint
	responsesEndpoint = server.URL
	defer func() { responsesEndpoint = original }()

	client := NewClient(SubscriptionAccessToken{Value: "test-token"})
	result, err := client.CreateStructuredResponse(context.Background(), StructuredResponseRequest{
		Model:        "gpt-5.6-luna",
		Instructions: "Classify.",
		UserText:     "invoice due Friday",
		SchemaName:   "classification",
		Schema:       map[string]any{"type": "object"},
	})
	return result, err, seenBody, seenHeader
}

func TestCreateStructuredResponseReassemblesDeltasAndUsage(t *testing.T) {
	result, err, body, header := serveStream(t, []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.output_text.delta","delta":"{\"category\":"}`,
		`{"type":"response.output_text.delta","delta":"\"billing\"}"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":73,"input_tokens_details":{"cached_tokens":9},"output_tokens":18,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":91}}}`,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if want := `{"category":"billing"}`; result.OutputText != want {
		t.Errorf("output = %q, want %q", result.OutputText, want)
	}
	if result.Usage.InputTokens != 73 || result.Usage.OutputTokens != 18 || result.Usage.TotalTokens != 91 {
		t.Errorf("usage = %+v", result.Usage)
	}
	if result.Usage.CachedInputTokens != 9 || result.Usage.ReasoningTokens != 2 {
		t.Errorf("usage detail = %+v", result.Usage)
	}
	if result.ResponseID != "resp_1" {
		t.Errorf("response id = %q", result.ResponseID)
	}

	// The endpoint rejects a non-streaming request, so streaming must always be
	// requested; strict schema decoding is the reason a caller can trust output.
	if body["stream"] != true {
		t.Errorf("stream = %v, want true", body["stream"])
	}
	format := body["text"].(map[string]any)["format"].(map[string]any)
	if format["strict"] != true || format["type"] != "json_schema" {
		t.Errorf("format = %+v", format)
	}
	if got := header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("authorization = %q", got)
	}
}

// A truncated answer still parses as JSON, so the status is the only signal the
// reply is short. Losing it would let a partial classification look complete.
func TestCreateStructuredResponseRejectsIncompleteStatus(t *testing.T) {
	_, err, _, _ := serveStream(t, []string{
		`{"type":"response.output_text.delta","delta":"{\"category\":"}`,
		`{"type":"response.completed","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
	})
	if !errors.Is(err, ErrResponseIncomplete) {
		t.Fatalf("error = %v, want ErrResponseIncomplete", err)
	}
	if !strings.Contains(err.Error(), "max_output_tokens") {
		t.Errorf("error should name the reason, got %v", err)
	}
}

func TestCreateStructuredResponseRejectsIncompleteEvent(t *testing.T) {
	_, err, _, _ := serveStream(t, []string{
		`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"content_filter"}}}`,
	})
	if !errors.Is(err, ErrResponseIncomplete) {
		t.Fatalf("error = %v, want ErrResponseIncomplete", err)
	}
}

// A stream cut off mid-flight has no completion event. Returning the text that
// did arrive would be indistinguishable from a complete short answer.
func TestCreateStructuredResponseRejectsStreamWithoutCompletion(t *testing.T) {
	_, err, _, _ := serveStream(t, []string{
		`{"type":"response.output_text.delta","delta":"{\"category\":\"billing\"}"}`,
	})
	if !errors.Is(err, ErrResponseIncomplete) {
		t.Fatalf("error = %v, want ErrResponseIncomplete", err)
	}
}

func TestCreateStructuredResponseSurfacesFailureEvent(t *testing.T) {
	_, err, _, _ := serveStream(t, []string{
		`{"type":"response.failed","response":{"error":{"message":"model unavailable"}}}`,
	})
	if err == nil || !strings.Contains(err.Error(), "model unavailable") {
		t.Fatalf("error = %v, want the server message", err)
	}
}

func TestCreateStructuredResponseRejectsEmptyOutput(t *testing.T) {
	_, err, _, _ := serveStream(t, []string{
		`{"type":"response.completed","response":{"id":"r","status":"completed"}}`,
	})
	if !errors.Is(err, ErrNoOutputText) {
		t.Fatalf("error = %v, want ErrNoOutputText", err)
	}
}

func TestCreateStructuredResponseSurfacesHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"detail":"token expired"}`)
	}))
	defer server.Close()

	original := responsesEndpoint
	responsesEndpoint = server.URL
	defer func() { responsesEndpoint = original }()

	client := NewClient(SubscriptionAccessToken{Value: "test-token"})
	_, err := client.CreateStructuredResponse(context.Background(), StructuredResponseRequest{
		Model: "gpt-5.6-luna", SchemaName: "s", Schema: map[string]any{"type": "object"},
	})
	if err == nil || !strings.Contains(err.Error(), "token expired") {
		t.Fatalf("error = %v, want the server body", err)
	}
}
