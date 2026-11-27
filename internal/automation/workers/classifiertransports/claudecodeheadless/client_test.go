package claudecodeheadless

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClaude writes a stub CLI that records its arguments and prints the given
// stdout, so the envelope handling can be tested without spending a real turn.
func fakeClaude(t *testing.T, stdout string, exitCode int, stderr string) (binaryPath, argsPath string) {
	t.Helper()
	dir := t.TempDir()
	argsPath = filepath.Join(dir, "args.txt")
	binaryPath = filepath.Join(dir, "claude")

	script := fmt.Sprintf(`#!/usr/bin/env bash
printf '%%s\n' "$@" > %q
printf '%%s' %q >&2
printf '%%s' %q
exit %d
`, argsPath, stderr, stdout, exitCode)

	if err := os.WriteFile(binaryPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binaryPath, argsPath
}

func testSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"category": map[string]any{"type": "string"}},
	}
}

const goodEnvelope = `{"is_error":false,"result":"{\"category\":\"billing\"}","session_id":"sess-1","num_turns":2,"total_cost_usd":0.0104759,"usage":{"input_tokens":10,"cache_creation_input_tokens":3300,"cache_read_input_tokens":22169,"output_tokens":209}}`

func TestCreateStructuredResponseParsesResultAndUsage(t *testing.T) {
	binary, argsPath := fakeClaude(t, goodEnvelope, 0, "")
	client := &Client{BinaryPath: binary}

	result, err := client.CreateStructuredResponse(context.Background(), StructuredResponseRequest{
		Model:  "claude-haiku-4-5",
		Prompt: "Classify this email.",
		Schema: testSchema(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if result.OutputText != `{"category":"billing"}` {
		t.Errorf("output = %q", result.OutputText)
	}
	if result.SessionID != "sess-1" || result.NumTurns != 2 {
		t.Errorf("session=%q turns=%d", result.SessionID, result.NumTurns)
	}

	// The cache split is the whole cost story for this transport, so each part
	// has to survive decoding rather than being folded into one total.
	u := result.Usage
	if u.InputTokens != 10 || u.CacheCreationInputTokens != 3300 || u.CacheReadInputTokens != 22169 || u.OutputTokens != 209 {
		t.Errorf("usage = %+v", u)
	}
	if got, want := u.TotalInputTokens(), 25479; got != want {
		t.Errorf("total input = %d, want %d", got, want)
	}
	if u.CostUSD != 0.0104759 {
		t.Errorf("cost = %v", u.CostUSD)
	}

	raw, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-p", "--json-schema", "--output-format", "json", "--model", "claude-haiku-4-5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	// The schema must reach the CLI as JSON, since that is what enforces the
	// reply shape; passing it as a Go map string would silently not.
	var schemaArg map[string]any
	for i, a := range args {
		if a == "--json-schema" && i+1 < len(args) {
			if err := json.Unmarshal([]byte(args[i+1]), &schemaArg); err != nil {
				t.Errorf("schema arg is not JSON: %v", err)
			}
		}
	}
	if schemaArg["type"] != "object" {
		t.Errorf("schema arg = %+v", schemaArg)
	}
}

// The model is optional; omitting it must not pass an empty --model, which the
// CLI would reject.
func TestCreateStructuredResponseOmitsModelWhenUnset(t *testing.T) {
	binary, argsPath := fakeClaude(t, goodEnvelope, 0, "")
	client := &Client{BinaryPath: binary}

	if _, err := client.CreateStructuredResponse(context.Background(), StructuredResponseRequest{
		Prompt: "Classify.", Schema: testSchema(),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	raw, _ := os.ReadFile(argsPath)
	if strings.Contains(string(raw), "--model") {
		t.Errorf("args should not carry --model when none was set: %s", raw)
	}
}

func TestCreateStructuredResponseSurfacesCLIError(t *testing.T) {
	binary, _ := fakeClaude(t, `{"is_error":true,"result":"rate limit reached"}`, 0, "")
	client := &Client{BinaryPath: binary}

	_, err := client.CreateStructuredResponse(context.Background(), StructuredResponseRequest{
		Prompt: "Classify.", Schema: testSchema(),
	})
	if !errors.Is(err, ErrCLIReportedError) {
		t.Fatalf("error = %v, want ErrCLIReportedError", err)
	}
	if !strings.Contains(err.Error(), "rate limit reached") {
		t.Errorf("error should carry the CLI's own message, got %v", err)
	}
}

func TestCreateStructuredResponseRejectsEmptyResult(t *testing.T) {
	binary, _ := fakeClaude(t, `{"is_error":false,"result":"  "}`, 0, "")
	client := &Client{BinaryPath: binary}

	_, err := client.CreateStructuredResponse(context.Background(), StructuredResponseRequest{
		Prompt: "Classify.", Schema: testSchema(),
	})
	if !errors.Is(err, ErrNoResult) {
		t.Fatalf("error = %v, want ErrNoResult", err)
	}
}

// A crashed CLI must report its own stderr. Without it the caller sees only an
// exit code and cannot tell a bad flag from a missing login.
func TestCreateStructuredResponseSurfacesNonZeroExit(t *testing.T) {
	binary, _ := fakeClaude(t, "", 3, "not logged in")
	client := &Client{BinaryPath: binary}

	_, err := client.CreateStructuredResponse(context.Background(), StructuredResponseRequest{
		Prompt: "Classify.", Schema: testSchema(),
	})
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("error = %v, want the CLI stderr", err)
	}
}

func TestCreateStructuredResponseRequiresPromptAndSchema(t *testing.T) {
	client := &Client{BinaryPath: "/nonexistent"}
	if _, err := client.CreateStructuredResponse(context.Background(), StructuredResponseRequest{Schema: testSchema()}); err == nil {
		t.Error("expected an error for a missing prompt")
	}
	if _, err := client.CreateStructuredResponse(context.Background(), StructuredResponseRequest{Prompt: "x"}); err == nil {
		t.Error("expected an error for a missing schema")
	}
}
