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
	url  string
	http *http.Client
}

// New constructs a Client. baseURL is the permission-store root (e.g.
// "http://localhost:8304"); the /evaluate suffix is appended internally.
func New(baseURL string) *Client {
	return &Client{
		url: baseURL,
		// Loopback HTTP — typical evaluate is sub-millisecond. The timeout
		// is a generous ceiling so a wedged store reaches the caller's
		// error-handling path rather than holding the parent request
		// forever.
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

// Request is the body of POST /evaluate. BridgeSessionID and InstanceID
// provide scope — permission-store matches them against its "bridge:<id>"
// and "instance:<id>" rule scopes, and its audit log records both. The wire
// keys are permission-store's (EvaluateRequest in its engine.go): until
// 2026-09-11 this struct sent "session_id", which the store's decoder dropped
// as unknown, so no scoped rule ever matched from a live session and every
// audit row it wrote had an empty bridge_id. Tool is the harness-native tool
// name; Input is the raw tool input.
type Request struct {
	BridgeSessionID string          `json:"bridge_id,omitempty"`
	InstanceID      string          `json:"instance_id,omitempty"`
	Tool            string          `json:"tool"`
	Input           json.RawMessage `json:"input,omitempty"`
}

// Evaluate posts the request and parses the response. Any transport or
// parse failure is returned as an error, never a verdict: no rule has been
// consulted, so there is nothing to report but the failure. The prehook
// turns that into an "ask" for a session with a human attached and a deny
// for one without (writeHookNoVerdict). Neither path silently allows.
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
