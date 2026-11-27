// Package permclient asks permission-store for its verdict on one tool call.
// bridge-server's PreToolUse permission-prehook handler consults it on every
// tool call that reaches rule evaluation.
//
// permission-store runs in this process (internal/access/permissions), so
// Evaluate is a call into its engine, not a request over HTTP. The engine is
// the one the mounted /permission-store/ routes use, so a rule written there
// reaches the next verdict.
package permclient

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/access/permissions"
)

// Client is permission-store's engine in this process. Safe for concurrent use.
type Client struct {
	engine *permissions.Engine
}

// New asks engine for verdicts.
func New(engine *permissions.Engine) *Client {
	return &Client{engine: engine}
}

// Result is the verdict. Outcome is one of "allow", "deny" or "ask".
type Result struct {
	Outcome       string
	MatchedRuleID string
	Message       string
}

// Request is one tool call. BridgeSessionID and InstanceID give the scope —
// permission-store matches them against its "bridge:<id>" and "instance:<id>"
// rule scopes, and its audit log records both. Tool is the harness-native tool
// name; Input is the raw tool input.
type Request struct {
	BridgeSessionID string
	InstanceID      string
	Tool            string
	Input           json.RawMessage
}

// Evaluate asks the engine. A failure is returned as an error, never a verdict:
// no rule has been consulted, so there is nothing to report but the failure.
// The prehook turns that into an "ask" for a session with a human attached and
// a deny for one without (writeHookNoVerdict). Neither path silently allows.
func (c *Client) Evaluate(ctx context.Context, request Request) (*Result, error) {
	verdict, err := c.engine.Evaluate(ctx, permissions.EvaluateRequest{
		BridgeID: request.BridgeSessionID, InstanceID: request.InstanceID, Tool: request.Tool, Input: request.Input,
	})
	if err != nil {
		return nil, fmt.Errorf("permission-store: %w", err)
	}
	switch verdict.Outcome {
	case permissions.OutcomeAllow, permissions.OutcomeDeny, permissions.OutcomeAsk:
		return &Result{Outcome: verdict.Outcome, MatchedRuleID: verdict.MatchedRuleID, Message: verdict.Message}, nil
	}
	return nil, fmt.Errorf("permission-store returned unknown outcome %q", verdict.Outcome)
}
