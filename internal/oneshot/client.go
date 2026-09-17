// Package oneshot runs one stateless, single-turn, schema-forced LLM call
// through llm-bridge-server's instance oneshot endpoint.
//
// It replaces the direct api.anthropic.com calls that email-classifier,
// kanban-classifier and kanban-scoper each carried: those billed an API key
// resolved from auth-store, while this routes through a claude_code harness
// instance and bills the Claude Code subscription login. No credential
// passes through this package at all.
//
// The truncation guard the direct callers relied on survives in two layers:
// the harness rejects a reply that is not one valid JSON object (a reply cut
// off at the output-token cap fails that check loudly), and stop_reason is
// passed through and checked here so "max_tokens" is reported as truncation
// rather than as a decode error.
package oneshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/slava/scheduler/internal/bridgeauth"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// ErrTruncated is returned when the model stopped because it ran out of
// output tokens. The reply is incomplete, and an incomplete classification
// reads exactly like a model that had less to say — callers must not act on
// one.
var ErrTruncated = errors.New("oneshot: reply hit max_tokens")

// Config is one call's worth of settings.
type Config struct {
	// BridgeURL is the llm-bridge-server base URL, e.g. http://localhost:8160.
	BridgeURL string
	// InstanceID names the harness instance to run against. It must be a
	// claude_code instance with no bound credential — that is what makes the
	// call land on the subscription login. Required; there is deliberately no
	// default, the caller's job definition is the single source of truth.
	InstanceID string
	// Model is the model id passed through to the harness. Required.
	Model string
	// MaxTokens caps the reply. Must be sized to the expected answer: too
	// small does not fail silently (see ErrTruncated) but it does fail.
	MaxTokens int
	// Timeout bounds the whole HTTP call. Model calls that produce long
	// structured replies routinely take over a minute.
	Timeout time.Duration
}

func (c Config) validate() error {
	if c.BridgeURL == "" {
		return errors.New("oneshot: BridgeURL is required")
	}
	if c.InstanceID == "" {
		return errors.New("oneshot: InstanceID is required (a claude_code instance id, e.g. via -oneshot-instance)")
	}
	if c.Model == "" {
		return errors.New("oneshot: Model is required")
	}
	if c.MaxTokens <= 0 {
		return errors.New("oneshot: MaxTokens must be positive")
	}
	return nil
}

// Call sends one prompt with a JSON Schema the reply must conform to and
// decodes the reply into out. schema is the bare JSON Schema (what the
// direct API put under a tool's "input_schema"), not a tool definition.
func Call(ctx context.Context, cfg Config, system, prompt string, schema map[string]any, out any) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	schemaRaw, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("oneshot: marshal schema: %w", err)
	}
	body, err := json.Marshal(msg.OneShotRequest{
		Prompt:       prompt,
		SystemPrompt: system,
		Model:        cfg.Model,
		Schema:       schemaRaw,
		MaxTokens:    cfg.MaxTokens,
	})
	if err != nil {
		return fmt.Errorf("oneshot: marshal request: %w", err)
	}

	url := strings.TrimRight(cfg.BridgeURL, "/") + "/instances/" + cfg.InstanceID + "/oneshot"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("oneshot: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 3 * time.Minute
	}
	httpClient := bridgeauth.AuthenticateClient(&http.Client{Timeout: timeout})
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("oneshot: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("oneshot: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oneshot: %s %s: %s", url, resp.Status, strings.TrimSpace(string(respBody)))
	}

	var oneShotResponse msg.OneShotResponse
	if err := json.Unmarshal(respBody, &oneShotResponse); err != nil {
		return fmt.Errorf("oneshot: decode response: %w", err)
	}
	if oneShotResponse.StopReason == "max_tokens" {
		return fmt.Errorf("%w (max_tokens=%d, output_tokens=%d): the reply is incomplete and its missing items are indistinguishable from 'no opinion'",
			ErrTruncated, cfg.MaxTokens, oneShotResponse.Usage.OutputTokens)
	}
	if len(oneShotResponse.Parsed) == 0 {
		return fmt.Errorf("oneshot: response carried no parsed JSON (stop_reason=%q, text=%q)",
			oneShotResponse.StopReason, truncateForError(oneShotResponse.Text))
	}
	if err := json.Unmarshal(oneShotResponse.Parsed, out); err != nil {
		return fmt.Errorf("oneshot: decode parsed reply: %w (raw=%s)", err, truncateForError(string(oneShotResponse.Parsed)))
	}
	return nil
}

func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
