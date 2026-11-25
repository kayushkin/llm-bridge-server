package codexsubscription

import (
	"bufio"
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

// responsesEndpoint is the Responses API the Codex CLI itself posts to when the
// host is authenticated with a ChatGPT subscription. It is a package var so
// tests can point the client at an httptest server; nothing else reassigns it.
var responsesEndpoint = "https://chatgpt.com/backend-api/codex/responses"

// ErrResponseIncomplete means the model stopped before finishing — it ran out of
// output budget or was cut off.
//
// This matters for the same reason the Anthropic path guards stop_reason: a
// truncated answer still decodes as valid JSON, because the server drops the
// unfinished part rather than emitting broken JSON. The response status is the
// only evidence the answer is short, and a short answer is otherwise
// indistinguishable from a model that simply had less to say.
var ErrResponseIncomplete = errors.New("model response incomplete")

// ErrNoOutputText means the stream finished without producing any text. Treated
// as a failure rather than an empty result, because every caller here asks for a
// JSON document and no caller can do anything useful with nothing.
var ErrNoOutputText = errors.New("model produced no output text")

// TokenUsage is what one call cost, as reported by the server.
type TokenUsage struct {
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
	ReasoningTokens   int
	TotalTokens       int
}

// StructuredResponseRequest asks for exactly one JSON document matching Schema.
type StructuredResponseRequest struct {
	// Model is a Codex model id, for example "gpt-5.6-luna".
	Model string
	// Instructions replaces the system prompt outright. Sending a short one is
	// the whole cost advantage of calling this endpoint directly: driving the
	// Codex CLI instead carries its tool schemas and preamble on every call,
	// which measured over 12,000 input tokens against 73 here.
	Instructions string
	// UserText is the single user message.
	UserText string
	// SchemaName labels the schema in the request. The server requires one.
	SchemaName string
	// Schema is a JSON Schema the reply must match. It is enforced server-side
	// with strict decoding, so an unsatisfiable schema fails the call rather
	// than yielding near misses.
	Schema map[string]any
}

// StructuredResponseResult is one completed reply.
type StructuredResponseResult struct {
	// OutputText is the raw JSON document. Callers decode it themselves so this
	// package stays free of their types.
	OutputText string
	Usage      TokenUsage
	// ResponseID is the server's id for the call, worth logging when something
	// looks wrong.
	ResponseID string
}

// Client calls the subscription-backed Responses API.
type Client struct {
	Token      SubscriptionAccessToken
	HTTPClient *http.Client
}

// NewClient builds a client with a timeout suited to one classification call.
func NewClient(token SubscriptionAccessToken) *Client {
	return &Client{
		Token:      token,
		HTTPClient: &http.Client{Timeout: 180 * time.Second},
	}
}

// CreateStructuredResponse runs one request and returns the JSON document.
//
// The server rejects non-streaming requests outright ("Stream must be set to
// true"), so this always streams and reassembles the text from the event
// stream. That is a constraint of the endpoint, not a preference.
func (c *Client) CreateStructuredResponse(ctx context.Context, request StructuredResponseRequest) (*StructuredResponseResult, error) {
	if request.Model == "" {
		return nil, errors.New("model is required")
	}
	if request.SchemaName == "" {
		return nil, errors.New("schema name is required")
	}
	if len(request.Schema) == 0 {
		return nil, errors.New("schema is required")
	}

	body := map[string]any{
		"model":        request.Model,
		"instructions": request.Instructions,
		"input": []any{map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": request.UserText,
			}},
		}},
		"stream": true,
		"store":  false,
		"text": map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"name":   request.SchemaName,
				"strict": true,
				"schema": request.Schema,
			},
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesEndpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+c.Token.Value)

	response, err := c.HTTPClient.Do(httpRequest)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		failure, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("codex subscription endpoint %s: %s", response.Status, strings.TrimSpace(string(failure)))
	}
	return readStructuredResponseStream(response.Body)
}

// streamEvent is the subset of each server-sent event this package reads.
type streamEvent struct {
	Type     string `json:"type"`
	Delta    string `json:"delta"`
	Response struct {
		ID                string `json:"id"`
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Usage *struct {
			InputTokens        int `json:"input_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokens        int `json:"output_tokens"`
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	} `json:"response"`
}

// readStructuredResponseStream reassembles the text deltas and reads the final
// status. The completed event carries an empty output array on this endpoint —
// the text arrives only as deltas — so accumulating them is the only way to
// recover the answer.
func readStructuredResponseStream(stream io.Reader) (*StructuredResponseResult, error) {
	var outputText strings.Builder
	var result StructuredResponseResult
	sawTerminalEvent := false

	scanner := bufio.NewScanner(stream)
	// Model output easily exceeds bufio's default 64KB line cap, and a split
	// line would corrupt the JSON rather than fail loudly.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		payload, found := strings.CutPrefix(line, "data:")
		if !found {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var event streamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return nil, fmt.Errorf("decode stream event: %w", err)
		}

		switch event.Type {
		case "response.output_text.delta":
			outputText.WriteString(event.Delta)
		case "response.failed", "error":
			message := "stream reported failure"
			if event.Response.Error != nil && event.Response.Error.Message != "" {
				message = event.Response.Error.Message
			}
			return nil, fmt.Errorf("codex subscription stream failed: %s", message)
		case "response.incomplete":
			reason := "unspecified"
			if event.Response.IncompleteDetails != nil && event.Response.IncompleteDetails.Reason != "" {
				reason = event.Response.IncompleteDetails.Reason
			}
			return nil, fmt.Errorf("%w: %s", ErrResponseIncomplete, reason)
		case "response.completed":
			sawTerminalEvent = true
			result.ResponseID = event.Response.ID
			if event.Response.Status != "" && event.Response.Status != "completed" {
				reason := event.Response.Status
				if event.Response.IncompleteDetails != nil && event.Response.IncompleteDetails.Reason != "" {
					reason = event.Response.IncompleteDetails.Reason
				}
				return nil, fmt.Errorf("%w: %s", ErrResponseIncomplete, reason)
			}
			if usage := event.Response.Usage; usage != nil {
				result.Usage = TokenUsage{
					InputTokens:       usage.InputTokens,
					CachedInputTokens: usage.InputTokensDetails.CachedTokens,
					OutputTokens:      usage.OutputTokens,
					ReasoningTokens:   usage.OutputTokensDetails.ReasoningTokens,
					TotalTokens:       usage.TotalTokens,
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	// A stream that ends without a terminal event was cut off in transit. Saying
	// so beats handing back whatever text happened to arrive first.
	if !sawTerminalEvent {
		return nil, fmt.Errorf("%w: stream ended without a completion event", ErrResponseIncomplete)
	}
	result.OutputText = outputText.String()
	if strings.TrimSpace(result.OutputText) == "" {
		return nil, ErrNoOutputText
	}
	return &result, nil
}
