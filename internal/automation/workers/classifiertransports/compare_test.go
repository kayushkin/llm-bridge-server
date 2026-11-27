// Package classifiertransports compares the ways this host can run one
// classification call, so the choice rests on measurements rather than on which
// transport was implemented first.
//
// There is no production code here. The comparison is a test because its only
// output is numbers about this host, and they go stale — the point is to re-run
// it, not to trust a table written down once.
package classifiertransports

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/classifiertransports/claudecodeheadless"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/classifiertransports/codexsubscription"
)

// The email is deliberately the unambiguous case. The question here is what a
// call costs, not whether a model can tell a bill from a friend — a hard input
// would make disagreement expected and hide the cost comparison behind it.
const (
	emailToClassify = "Subject: Your invoice #4432 is due Friday. Amount $88.10."
	instructions    = "Classify the email into one of the categories. Reply only with JSON matching the schema."
)

func classificationSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"category":   map[string]any{"type": "string", "enum": []string{"billing", "personal", "spam"}},
			"confidence": map[string]any{"type": "number"},
		},
		"required":             []string{"category", "confidence"},
		"additionalProperties": false,
	}
}

// transportResult is one measured call.
type transportResult struct {
	Name         string
	Model        string
	InputTokens  int
	OutputTokens int
	// CostUSD is money for a metered API key, and a notional price for a
	// subscription transport where no balance is drawn down. Which one applies
	// is the Billing column, not this number.
	CostUSD float64
	Billing string
	Answer  string
	Err     error
}

func classificationCategory(t *testing.T, outputText string) string {
	t.Helper()
	var decoded struct {
		Category string `json:"category"`
	}
	if err := json.Unmarshal([]byte(outputText), &decoded); err != nil {
		t.Errorf("output %q is not valid JSON: %v", outputText, err)
		return ""
	}
	return decoded.Category
}

// resolveAnthropicKey reads the key from auth-store, the way the live
// classifier does, rather than from an environment variable.
func resolveAnthropicKey(t *testing.T) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:8303/api/resolve/anthropic", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer changeme")
	request.Header.Set("X-Auth-App", "classifier-transport-comparison")
	request.Header.Set("X-Auth-Reason", "measure per-call token cost of the current classifier path")

	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		t.Skipf("auth-store unreachable: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Skipf("auth-store returned %s: %s", response.Status, body)
	}
	var resolved struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(body, &resolved); err != nil || resolved.APIKey == "" {
		t.Skipf("no anthropic key in auth-store response")
	}
	return resolved.APIKey
}

// measureAnthropicForcedTool reproduces what cmd/email-classifier does today:
// one forced single-tool call against the Messages API on a metered key.
func measureAnthropicForcedTool(t *testing.T, model, apiKey string) transportResult {
	result := transportResult{Name: "anthropic forced tool (current)", Model: model, Billing: "metered API key"}

	body := map[string]any{
		"model":      model,
		"max_tokens": 1024,
		"system":     instructions,
		"tools": []any{map[string]any{
			"name":         "file_email",
			"description":  "File the email under a category.",
			"input_schema": classificationSchema(),
		}},
		"tool_choice": map[string]any{"type": "tool", "name": "file_email"},
		"messages":    []any{map[string]any{"role": "user", "content": emailToClassify}},
	}
	encoded, _ := json.Marshal(body)
	request, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(encoded))
	if err != nil {
		result.Err = err
		return result
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", apiKey)
	request.Header.Set("anthropic-version", "2023-06-01")

	response, err := (&http.Client{Timeout: 120 * time.Second}).Do(request)
	if err != nil {
		result.Err = err
		return result
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		result.Err = fmt.Errorf("anthropic %s: %s", response.Status, raw)
		return result
	}

	var decoded struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		result.Err = err
		return result
	}
	result.InputTokens = decoded.Usage.InputTokens
	result.OutputTokens = decoded.Usage.OutputTokens
	for _, block := range decoded.Content {
		if block.Type == "tool_use" {
			result.Answer = string(block.Input)
		}
	}
	result.CostUSD = anthropicCostUSD(model, result.InputTokens, result.OutputTokens)
	return result
}

// anthropicCostUSD prices a call from the published per-million rates. Kept
// beside the measurement so the table reports money rather than leaving the
// reader to multiply.
func anthropicCostUSD(model string, inputTokens, outputTokens int) float64 {
	rates := map[string][2]float64{
		"claude-sonnet-4-6": {3.00, 15.00},
		"claude-haiku-4-5":  {0.80, 4.00},
	}
	rate, known := rates[model]
	if !known {
		return 0
	}
	return float64(inputTokens)/1e6*rate[0] + float64(outputTokens)/1e6*rate[1]
}

// TestCompareClassifierTransports runs one identical classification through
// every available transport and prints what each cost.
//
// Skipped unless CLASSIFIER_TRANSPORT_COMPARE=1: it spends real money on the
// metered arm, real quota on the others, and needs both CLIs logged in.
func TestCompareClassifierTransports(t *testing.T) {
	if os.Getenv("CLASSIFIER_TRANSPORT_COMPARE") != "1" {
		t.Skip("set CLASSIFIER_TRANSPORT_COMPARE=1 to spend real tokens comparing transports")
	}
	ctx := context.Background()
	apiKey := resolveAnthropicKey(t)
	var results []transportResult

	// The model the live classifier actually runs, and the cheap sibling it
	// deliberately does not — both on the metered key.
	results = append(results, measureAnthropicForcedTool(t, "claude-sonnet-4-6", apiKey))
	results = append(results, measureAnthropicForcedTool(t, "claude-haiku-4-5", apiKey))

	// Codex subscription, called directly. Cheapest by a wide margin and the
	// least supported — see the package header.
	codexResult := transportResult{Name: "codex subscription (direct)", Model: "gpt-5.6-luna", Billing: "ChatGPT plan quota"}
	if authPath, err := codexsubscription.DefaultAuthFilePath(); err != nil {
		codexResult.Err = err
	} else if token, err := codexsubscription.ResolveSubscriptionAccessToken(authPath, time.Now()); err != nil {
		codexResult.Err = err
	} else {
		got, err := codexsubscription.NewClient(token).CreateStructuredResponse(ctx, codexsubscription.StructuredResponseRequest{
			Model:        "gpt-5.6-luna",
			Instructions: instructions,
			UserText:     emailToClassify,
			SchemaName:   "email_classification",
			Schema:       classificationSchema(),
		})
		if err != nil {
			codexResult.Err = err
		} else {
			codexResult.InputTokens = got.Usage.InputTokens
			codexResult.OutputTokens = got.Usage.OutputTokens
			codexResult.Answer = got.OutputText
		}
	}
	results = append(results, codexResult)

	// Claude Code in print mode. Supported, and the most expensive per call
	// because the harness ships its whole prompt and tool set every time.
	headlessResult := transportResult{Name: "claude code headless", Model: "claude-haiku-4-5", Billing: "Claude plan quota"}
	got, err := claudecodeheadless.NewClient().CreateStructuredResponse(ctx, claudecodeheadless.StructuredResponseRequest{
		Model:  "claude-haiku-4-5",
		Prompt: instructions + "\n\n" + emailToClassify,
		Schema: classificationSchema(),
	})
	if err != nil {
		headlessResult.Err = err
	} else {
		headlessResult.InputTokens = got.Usage.TotalInputTokens()
		headlessResult.OutputTokens = got.Usage.OutputTokens
		headlessResult.CostUSD = got.Usage.CostUSD
		headlessResult.Answer = got.OutputText
	}
	results = append(results, headlessResult)

	t.Logf("%-32s %-18s %9s %8s %12s  %s", "transport", "model", "input", "output", "cost usd", "billing")
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("%-32s %-18s  FAILED: %v", r.Name, r.Model, r.Err)
			continue
		}
		t.Logf("%-32s %-18s %9d %8d %12.6f  %s", r.Name, r.Model, r.InputTokens, r.OutputTokens, r.CostUSD, r.Billing)
	}

	// Every transport must reach the same verdict on an unambiguous email. If
	// they disagree here, the cost table is beside the point.
	for _, r := range results {
		if r.Err != nil || r.Answer == "" {
			continue
		}
		if category := classificationCategory(t, r.Answer); category != "billing" {
			t.Errorf("%s answered %q, want billing", r.Name, category)
		}
	}
}
