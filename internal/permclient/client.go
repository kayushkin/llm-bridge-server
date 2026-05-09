// Package permclient is a minimal HTTP client for permission-store's
// /evaluate endpoint. Used by bridge-server's PreToolUse permission-prehook
// handler to consult the canonical rule store on every tool call.
//
// Mirrors the shape of the harness-side client that previously lived in
// llm-bridge-claudecode's permission_client.go. The harness copy is being
// retired as part of the MCP→PreToolUse-hook migration; bridge-server now
// owns the call so all gating decisions go through one process.
package permclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is a permission-store /evaluate caller. Safe for concurrent use.
type Client struct {
	url    string
	http   *http.Client
}

// New constructs a Client. baseURL is the permission-store root (e.g.
// "http://localhost:8304"); the /evaluate suffix is appended internally.
func New(baseURL string) *Client {
	return &Client{
		url: baseURL,
		// Loopback HTTP — typical evaluate is sub-millisecond. The timeout
		// is a generous ceiling so a wedged store collapses to "ask" via
		// the caller's error-handling path rather than holding the parent
		// request forever.
		http: &http.Client{Timeout: 3 * time.Second},
	}
}

// Result is the parsed /evaluate response. Outcome is one of "allow",
// "deny", or "ask"; UpdatedInput passes through for the rare rule that
// rewrites the tool input.
type Result struct {
	Outcome       string          `json:"outcome"`
	MatchedRuleID string          `json:"matched_rule_id"`
	Message       string          `json:"message"`
	UpdatedInput  json.RawMessage `json:"updated_input,omitempty"`
}

// Request is the body of POST /evaluate. SessionID and InstanceID provide
// scope; Tool is the harness-native tool name; Input is the raw tool input.
type Request struct {
	SessionID  string          `json:"session_id,omitempty"`
	InstanceID string          `json:"instance_id,omitempty"`
	Tool       string          `json:"tool"`
	Input      json.RawMessage `json:"input,omitempty"`
}

// Evaluate posts the request and parses the response. Any transport or
// parse failure is returned as an error — callers fall back to "ask" with a
// human-readable message rather than silently allowing.
func (c *Client) Evaluate(ctx context.Context, req Request) (*Result, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal evaluate request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/evaluate", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build evaluate request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("permission-store unreachable: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read evaluate response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("permission-store HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed Result
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode evaluate response: %w", err)
	}
	switch parsed.Outcome {
	case "allow", "deny", "ask":
		return &parsed, nil
	}
	return nil, fmt.Errorf("permission-store returned unknown outcome %q", parsed.Outcome)
}
