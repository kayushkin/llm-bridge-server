// Package anthropic calls the Anthropic Messages API and decodes a forced tool
// call.
//
// It exists because three commands in this repo — email-classifier,
// kanban-classifier and kanban-scoper — each carry their own copy of the same
// request builder and their own copy of the auth-store key lookup, and the
// copies have already drifted apart. kanban-scoper lost the truncation guard
// the other two have, and hardcodes the endpoint inline so a test cannot point
// it anywhere. This is the one implementation for anything written from here on.
//
// It deliberately does NOT try to be a general SDK. It does exactly what those
// commands need: one call, one forced tool, one decoded struct.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MessagesURL is a var, not a const, so a test can point it at an httptest
// server. kanban-scoper hardcodes this inline and is untestable as a result.
var MessagesURL = "https://api.anthropic.com/v1/messages"

// APIVersion is the dated Anthropic API contract this package speaks.
const APIVersion = "2023-06-01"

// ErrTruncated is returned when the model stopped because it ran out of output
// tokens.
//
// This is its own error for a reason that is easy to get wrong: a truncated
// reply still contains a tool_use block, and that block's partial input often
// parses as perfectly valid JSON. The decode succeeds and the caller acts on
// half an answer. stop_reason is the only evidence, so it is checked before the
// content is read at all.
var ErrTruncated = errors.New("anthropic: reply hit max_tokens")

// Config is one call's worth of settings.
type Config struct {
	APIKey string
	Model  string
	// MaxTokens must be sized to the expected reply. Too small does not fail
	// loudly on its own — see ErrTruncated.
	MaxTokens int
	// Timeout bounds the request. Model calls that produce long structured
	// replies routinely take over a minute.
	Timeout time.Duration
}

func (c Config) validate() error {
	if c.APIKey == "" {
		return errors.New("anthropic: no API key")
	}
	if c.Model == "" {
		return errors.New("anthropic: no model id")
	}
	if c.MaxTokens <= 0 {
		return errors.New("anthropic: max_tokens must be positive")
	}
	return nil
}

// ResolveKey reads a credential from auth-store, which is the only place on
// this host that holds one.
//
// app and reason are recorded in auth-store's audit log, so they should name
// the caller and what it wants the key for rather than being left generic.
func ResolveKey(ctx context.Context, authStoreURL, authToken, app, reason string) (string, error) {
	url := strings.TrimRight(authStoreURL, "/") + "/api/resolve/anthropic"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("anthropic: build auth-store request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("X-Auth-App", app)
	req.Header.Set("X-Auth-Reason", reason)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic: auth-store unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("anthropic: read auth-store reply: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic: auth-store returned %d: %s", resp.StatusCode, truncate(body))
	}

	var credential struct {
		APIKey      string `json:"api_key"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &credential); err != nil {
		return "", fmt.Errorf("anthropic: decode auth-store reply: %w", err)
	}
	// An OAuth access token goes in the same header as an API key. That is not
	// obvious and is what the existing copies do; spelling it out here so the
	// next reader does not "fix" it into an Authorization header.
	if credential.APIKey != "" {
		return credential.APIKey, nil
	}
	if credential.AccessToken != "" {
		return credential.AccessToken, nil
	}
	return "", errors.New("anthropic: auth-store credential has no api_key or access_token")
}

// InvokeTool asks the model for one structured answer and decodes it into out.
//
// The reply is forced through a tool schema rather than asked for as JSON,
// because a forced tool call is the only shape that makes the model's answer
// machine-decodable by construction instead of by hope.
func InvokeTool(ctx context.Context, cfg Config, system string, schema map[string]any, toolName, userText string, out any) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	payload := map[string]any{
		"model":       cfg.Model,
		"max_tokens":  cfg.MaxTokens,
		"system":      system,
		"tools":       []any{schema},
		"tool_choice": map[string]any{"type": "tool", "name": toolName},
		"messages":    []any{map[string]any{"role": "user", "content": userText}},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("anthropic: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, MessagesURL, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("anthropic: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("anthropic-version", APIVersion)

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 180 * time.Second
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("anthropic: request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("anthropic: read reply: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("anthropic: API returned %d: %s", resp.StatusCode, truncate(body))
	}

	var reply struct {
		StopReason string `json:"stop_reason"`
		Usage      struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return fmt.Errorf("anthropic: decode reply envelope: %w", err)
	}
	// Before the content, never after: a truncated tool_use block frequently
	// decodes cleanly into a partial answer.
	if reply.StopReason == "max_tokens" {
		return fmt.Errorf("%w: %d output tokens against a limit of %d",
			ErrTruncated, reply.Usage.OutputTokens, cfg.MaxTokens)
	}

	for _, block := range reply.Content {
		if block.Type != "tool_use" || block.Name != toolName {
			continue
		}
		if err := json.Unmarshal(block.Input, out); err != nil {
			return fmt.Errorf("anthropic: decode tool input: %w (raw=%s)", err, truncate(block.Input))
		}
		return nil
	}
	return fmt.Errorf("anthropic: no tool_use block named %q in the reply: %s", toolName, truncate(body))
}

func truncate(b []byte) string {
	const limit = 600
	if len(b) <= limit {
		return string(b)
	}
	return string(b[:limit]) + "…"
}
