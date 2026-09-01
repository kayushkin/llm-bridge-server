// Package demosession runs a one-shot task as a Claude Code session instead of
// as a direct Anthropic API call.
//
// The reason is billing, and it is not a detail. A direct call to
// /v1/messages spends API credit; a Claude Code session runs on the Max
// subscription, where the marginal cost of a turn is zero. Everything on this
// host that dispatches agents already works that way — the builder and the
// reviewer do — while the tools that called the API directly did not, and
// stopped the day the API credit ran out.
//
// It also removes a credential problem rather than maintaining one. auth-store
// holds a Max OAuth credential whose refresh_mode is client_lease, which means
// auth-store never refreshes it and nothing else did either: the stored token
// is invalid, while Claude Code holds a live one it refreshes itself. Going
// through the harness means never handling that token at all.
//
// What is given up: a forced tool call. An API call can demand a schema and get
// a validated object back. A session is an agent doing work, so the task has to
// be expressible as WORK — write these messages into that mailbox — rather than
// as a value to be returned and then acted on. For these tasks that is the
// better shape anyway, since the caller only wanted the side effect.
package demosession

import (
	"fmt"
	"time"

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/bus/messages"
	"github.com/kayushkin/llm-bridge/msg"
)

// Task is one piece of work to hand an agent.
type Task struct {
	// Prompt is the whole instruction. It must describe work with a visible
	// result, because nothing reads a value back out of the session.
	Prompt string
	// AgentSlug is the identity the session acts as, written to
	// CreateSessionRequest.AgentID.
	AgentSlug string
	// DisplayName is what a human sees in the session list.
	DisplayName string
	// WorkingDir is where the harness runs. Empty leaves it to the instance.
	WorkingDir string
	// MaxBudgetUSD caps the session. Zero means uncapped, which is what a
	// harness reads a missing flag as — so a caller that wants a cap must say a
	// positive number.
	MaxBudgetUSD float64
	// InstanceID is the harness instance to host it.
	InstanceID string
	// Purpose and Origin are governed vocabularies on llm-bridge-server and are
	// enforced at creation, so they are not free text.
	Purpose string
	Origin  string
}

// Spawn publishes the session request and returns the session id.
//
// The id is minted here because the adapter passes BusSessionID through as the
// bridge session_id, so this value is what a card's session link resolves to.
func Spawn(natsURL string, t Task) (string, error) {
	if t.Prompt == "" {
		return "", fmt.Errorf("demosession: a task with no prompt would spawn an agent with nothing to do")
	}
	if t.AgentSlug == "" {
		return "", fmt.Errorf("demosession: a session with no identity cannot be attributed to anyone")
	}
	if t.InstanceID == "" {
		t.InstanceID = "inst-cc-local"
	}
	if t.Purpose == "" {
		t.Purpose = "autoworker"
	}
	if t.Origin == "" {
		t.Origin = "scheduler"
	}

	nc, err := natsbus.Connect(natsbus.Options{URL: natsURL, Name: "demosession"})
	if err != nil {
		return "", fmt.Errorf("demosession: nats: %w", err)
	}
	defer nc.Close()

	now := time.Now().UTC()
	sessionID := fmt.Sprintf("demo-%s-%d", t.AgentSlug, now.UnixNano())

	create := &msg.CreateSessionRequest{
		Harness:     msg.HarnessClaudeCode,
		InstanceID:  t.InstanceID,
		DisplayName: t.DisplayName,
		Purpose:     t.Purpose,
		Origin:      t.Origin,
		Type:        msg.SessionTypeAutonomous,
		AutoStart:   true,
		AgentID:     t.AgentSlug,
		WorkingDir:  t.WorkingDir,
	}
	if t.MaxBudgetUSD > 0 {
		budget := t.MaxBudgetUSD
		create.MaxBudget = &budget
		// The harness-side cap as well as the server-side one. A poller that
		// notices a breach afterwards is not a control: the fastest session
		// observed on this host burned $29.22 a minute.
		create.HarnessConfig = []byte(fmt.Sprintf(`{"max_budget_usd":%.2f}`, budget))
	}

	in := messages.BridgeInbound{
		BusSessionID: sessionID,
		Agent:        t.AgentSlug,
		Orchestrator: "demo",
		Channel:      "demo",
		Timestamp:    now,
		Create:       create,
		Send:         &msg.SendMessageRequest{Message: t.Prompt},
	}
	if err := nc.Publish(messages.BridgeInboundSubject, in); err != nil {
		return "", fmt.Errorf("demosession: publish: %w", err)
	}
	return sessionID, nil
}
