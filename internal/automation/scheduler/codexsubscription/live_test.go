package codexsubscription

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestLiveClassificationAgainstSubscriptionEndpoint calls the real endpoint with
// the real credential on this host.
//
// Skipped unless CODEX_SUBSCRIPTION_LIVE=1, because it spends quota, needs
// network, and depends on a credential that only exists on a host where the
// Codex CLI is logged in. Run it when checking that the endpoint still behaves
// the way this package assumes — that is the whole point of an unsupported
// dependency: the assumption needs re-checking, not trusting.
func TestLiveClassificationAgainstSubscriptionEndpoint(t *testing.T) {
	if os.Getenv("CODEX_SUBSCRIPTION_LIVE") != "1" {
		t.Skip("set CODEX_SUBSCRIPTION_LIVE=1 to call the real endpoint")
	}

	authFilePath, err := DefaultAuthFilePath()
	if err != nil {
		t.Fatalf("locate auth file: %v", err)
	}
	token, err := ResolveSubscriptionAccessToken(authFilePath, time.Now())
	if err != nil {
		t.Fatalf("resolve token: %v", err)
	}
	t.Logf("token expires %s (%.1f hours out)",
		token.ExpiresAt.UTC().Format(time.RFC3339), time.Until(token.ExpiresAt).Hours())

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"category":   map[string]any{"type": "string", "enum": []string{"billing", "personal", "spam"}},
			"confidence": map[string]any{"type": "number"},
		},
		"required":             []string{"category", "confidence"},
		"additionalProperties": false,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	result, err := NewClient(token).CreateStructuredResponse(ctx, StructuredResponseRequest{
		Model:        "gpt-5.6-luna",
		Instructions: "Classify the email. Reply only with JSON matching the schema.",
		UserText:     "Subject: Your invoice #4432 is due Friday. Amount $88.10.",
		SchemaName:   "email_classification",
		Schema:       schema,
	})
	if err != nil {
		t.Fatalf("live call: %v", err)
	}

	var decoded struct {
		Category   string  `json:"category"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(result.OutputText), &decoded); err != nil {
		t.Fatalf("decode %q: %v", result.OutputText, err)
	}
	if decoded.Category != "billing" {
		t.Errorf("category = %q, want billing (an invoice is the unambiguous case)", decoded.Category)
	}

	t.Logf("output=%s", result.OutputText)
	t.Logf("usage: input=%d cached=%d output=%d total=%d",
		result.Usage.InputTokens, result.Usage.CachedInputTokens,
		result.Usage.OutputTokens, result.Usage.TotalTokens)

	// The cost case for this package rests on a small prompt staying small. If
	// the endpoint starts injecting a preamble, that shows up here first.
	if result.Usage.InputTokens > 1000 {
		t.Errorf("input tokens = %d, far above the ~73 measured; the endpoint may now inject a preamble",
			result.Usage.InputTokens)
	}
}
