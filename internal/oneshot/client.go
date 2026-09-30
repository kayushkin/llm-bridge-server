// Package oneshot runs one stateless, single-turn, schema-forced LLM call
// through llm-bridge-server's POST /oneshot, naming a model-store role
// ("balanced") rather than a model and an instance.
//
// The bridge owns the routing: it reads the role's ordered model list from
// model-store and tries each model in turn on the harness instance configured
// for that model's provider, until one answers. This package never names a
// model or an instance, so which model a role means is changed in model-store,
// and which role a command asks for is changed on the scheduler's settings
// page; neither needs a rebuild. No credential passes through here.
//
// The truncation guard survives in two layers: the harness rejects a reply
// that is not one valid JSON object (a reply cut off at the output-token cap
// fails that check loudly), and stop_reason is passed through and checked here
// so "max_tokens" is reported as truncation rather than as a decode error.
package oneshot

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
	// ModelRole is the model-store role the call asks for ("balanced"). The
	// bridge tries the role's models in order. Required; the caller reads it
	// from the scheduler's stored settings.
	ModelRole string
	// Caller names the command making the call ("kanban-scoper"), so the
	// bridge's logs and usage say what the call was for. Required.
	Caller string
	// MaxTokens caps the reply. Must be sized to the expected answer: too
	// small does not fail silently (see ErrTruncated) but it does fail.
	MaxTokens int
	// Timeout bounds the whole HTTP call, every fallback model included.
	// Model calls that produce long structured replies routinely take over a
	// minute. Required.
	Timeout time.Duration
}

func (c Config) validate() error {
	switch {
	case c.BridgeURL == "":
		return errors.New("oneshot: BridgeURL is required")
	case c.ModelRole == "":
		return errors.New("oneshot: ModelRole is required (a model-store role such as balanced)")
	case c.Caller == "":
		return errors.New("oneshot: Caller is required (the command making the call)")
	case c.MaxTokens <= 0:
		return errors.New("oneshot: MaxTokens must be positive")
	case c.Timeout <= 0:
		return errors.New("oneshot: Timeout must be positive")
	}
	return nil
}

// Answer says who answered a call: the model, the instance it ran on, and the
// models the bridge tried or skipped first. For the caller's log line.
type Answer struct {
	Model      string
	InstanceID string
	Attempts   []msg.OneShotAttempt
}

// String is the answer as one log fragment: "gpt-5.6-luna on inst-codex"
// plus the earlier attempts when there were any.
func (a Answer) String() string {
	text := a.Model + " on " + a.InstanceID
	for _, attempt := range a.Attempts {
		reason := attempt.Error
		if attempt.Skipped != "" {
			reason = "skipped: " + attempt.Skipped
		}
		text += fmt.Sprintf(" (after %s: %s)", attempt.Model, reason)
	}
	return text
}

// Call sends one prompt with a JSON Schema the reply must conform to and
// decodes the reply into out. schema is the bare JSON Schema, not a tool
// definition.
func Call(ctx context.Context, cfg Config, system, prompt string, schema map[string]any, out any) (Answer, error) {
	if err := cfg.validate(); err != nil {
		return Answer{}, err
	}
	schemaRaw, err := json.Marshal(schema)
	if err != nil {
		return Answer{}, fmt.Errorf("oneshot: marshal schema: %w", err)
	}
	body, err := json.Marshal(msg.OneShotRequest{
		Prompt:       prompt,
		SystemPrompt: system,
		ModelRole:    cfg.ModelRole,
		Caller:       cfg.Caller,
		Schema:       schemaRaw,
		MaxTokens:    cfg.MaxTokens,
	})
	if err != nil {
		return Answer{}, fmt.Errorf("oneshot: marshal request: %w", err)
	}

	url := strings.TrimRight(cfg.BridgeURL, "/") + "/oneshot"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Answer{}, fmt.Errorf("oneshot: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// No Transport of its own: the request goes out through http.DefaultTransport,
	// which main stamped with the bridge's service token (bridgeauth).
	httpClient := &http.Client{Timeout: cfg.Timeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return Answer{}, fmt.Errorf("oneshot: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Answer{}, fmt.Errorf("oneshot: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The bridge names every model it tried and why each failed; pass it
		// through whole.
		return Answer{}, fmt.Errorf("oneshot: POST %s (role %s): %s: %s", url, cfg.ModelRole, resp.Status, strings.TrimSpace(string(respBody)))
	}

	var oneShotResponse msg.OneShotResponse
	if err := json.Unmarshal(respBody, &oneShotResponse); err != nil {
		return Answer{}, fmt.Errorf("oneshot: decode response: %w", err)
	}
	answer := Answer{Model: oneShotResponse.Model, InstanceID: oneShotResponse.InstanceID, Attempts: oneShotResponse.Attempts}
	if oneShotResponse.StopReason == "max_tokens" {
		return answer, fmt.Errorf("%w (model %s, max_tokens=%d, output_tokens=%d): the reply is incomplete and its missing items are indistinguishable from 'no opinion'",
			ErrTruncated, answer.Model, cfg.MaxTokens, oneShotResponse.Usage.OutputTokens)
	}
	if len(oneShotResponse.Parsed) == 0 {
		return answer, fmt.Errorf("oneshot: response from %s carried no parsed JSON (stop_reason=%q, text=%q)",
			answer.Model, oneShotResponse.StopReason, truncateForError(oneShotResponse.Text))
	}
	if err := json.Unmarshal(oneShotResponse.Parsed, out); err != nil {
		return answer, fmt.Errorf("oneshot: decode parsed reply from %s: %w (raw=%s)", answer.Model, err, truncateForError(string(oneShotResponse.Parsed)))
	}
	return answer, nil
}

func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
