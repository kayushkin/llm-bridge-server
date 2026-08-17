// Package claudecodeheadless runs one classification through the Claude Code
// CLI in print mode, so the work draws on the Claude subscription this host is
// already logged into rather than on a metered API key.
//
// It is the Claude-side counterpart to internal/codexsubscription, and unlike
// that package it uses a supported, documented entry point: `claude -p` with
// `--json-schema` is the product's own non-interactive mode.
//
// The cost is the reason to measure before adopting. Claude Code sends its full
// system prompt and tool definitions on every call, so a twenty-token
// classification measured about 25,500 input tokens here — against 79 for the
// same task posted straight to a model API. Most of that is cached after the
// first call, which cuts the price roughly fivefold but not the token count.
// See BenchmarkClassifierTransports for the numbers on this host.
package claudecodeheadless

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ErrCLIReportedError means Claude Code itself reported the turn failed. Its
// own message is surfaced rather than a generic one.
var ErrCLIReportedError = errors.New("claude code reported an error")

// ErrNoResult means the CLI exited cleanly without producing a result payload.
var ErrNoResult = errors.New("claude code produced no result")

// TokenUsage is what one call cost.
//
// Claude Code reports cache creation and cache reads separately, and the split
// matters: a cache read is billed at a tenth of a write, so the same call costs
// about five times less once the prompt is warm. Collapsing them into one
// number would hide the only thing that makes this transport affordable.
type TokenUsage struct {
	InputTokens              int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	OutputTokens             int
	// CostUSD is the CLI's own costing of the turn. On a subscription plan it
	// describes what the tokens would have cost on the API, not money charged.
	CostUSD float64
}

// TotalInputTokens is every input token the turn consumed, cached or not — the
// figure to compare against another transport's input count.
func (u TokenUsage) TotalInputTokens() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// StructuredResponseRequest asks for one JSON document matching Schema.
type StructuredResponseRequest struct {
	// Model is a Claude Code model alias or full name, e.g. "claude-haiku-4-5".
	Model string
	// Prompt is the whole instruction, including the text to classify. Claude
	// Code's print mode takes a single prompt rather than a system/user split.
	Prompt string
	// Schema is the JSON Schema the reply must match.
	Schema map[string]any
}

// StructuredResponseResult is one completed reply.
type StructuredResponseResult struct {
	// OutputText is the raw JSON document. Callers decode it themselves so this
	// package stays free of their types.
	OutputText string
	Usage      TokenUsage
	SessionID  string
	NumTurns   int
}

// Client runs the Claude Code CLI.
type Client struct {
	// BinaryPath is the claude executable. Empty means look up "claude" on PATH.
	BinaryPath string
	Timeout    time.Duration
}

// NewClient builds a client with a timeout suited to one classification.
func NewClient() *Client {
	return &Client{Timeout: 180 * time.Second}
}

// cliEnvelope is the --output-format json envelope.
type cliEnvelope struct {
	IsError   bool    `json:"is_error"`
	Result    string  `json:"result"`
	SessionID string  `json:"session_id"`
	NumTurns  int     `json:"num_turns"`
	CostUSD   float64 `json:"total_cost_usd"`
	Usage     struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
	} `json:"usage"`
}

// CreateStructuredResponse runs one classification and returns the JSON document.
func (c *Client) CreateStructuredResponse(ctx context.Context, request StructuredResponseRequest) (*StructuredResponseResult, error) {
	if request.Prompt == "" {
		return nil, errors.New("prompt is required")
	}
	if len(request.Schema) == 0 {
		return nil, errors.New("schema is required")
	}
	encodedSchema, err := json.Marshal(request.Schema)
	if err != nil {
		return nil, fmt.Errorf("encode schema: %w", err)
	}

	binary := c.BinaryPath
	if binary == "" {
		binary = "claude"
	}
	args := []string{
		"-p", request.Prompt,
		"--json-schema", string(encodedSchema),
		"--output-format", "json",
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}

	timeout := c.Timeout
	if timeout == 0 {
		timeout = 180 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(runCtx, binary, args...)
	// Leave Stdin nil so the child reads /dev/null. Claude Code waits three
	// seconds for piped input otherwise, and warns on stderr — a delay on
	// every call for input this package never sends.
	output, err := command.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("claude code exited %d: %s", exitErr.ExitCode(), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("run claude code: %w", err)
	}

	var envelope cliEnvelope
	if err := json.Unmarshal(output, &envelope); err != nil {
		return nil, fmt.Errorf("decode claude code envelope: %w", err)
	}
	if envelope.IsError {
		return nil, fmt.Errorf("%w: %s", ErrCLIReportedError, strings.TrimSpace(envelope.Result))
	}
	if strings.TrimSpace(envelope.Result) == "" {
		return nil, ErrNoResult
	}

	return &StructuredResponseResult{
		OutputText: envelope.Result,
		SessionID:  envelope.SessionID,
		NumTurns:   envelope.NumTurns,
		Usage: TokenUsage{
			InputTokens:              envelope.Usage.InputTokens,
			CacheCreationInputTokens: envelope.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     envelope.Usage.CacheReadInputTokens,
			OutputTokens:             envelope.Usage.OutputTokens,
			CostUSD:                  envelope.CostUSD,
		},
	}, nil
}
