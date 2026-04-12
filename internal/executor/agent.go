package executor

import (
	"fmt"
	"time"

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/bus/messages"
)

// AgentExecutor runs agent turns by publishing a ChatInbound message.
type AgentExecutor struct {
	client *natsbus.Client
}

func NewAgentExecutor(client *natsbus.Client) *AgentExecutor {
	return &AgentExecutor{client: client}
}

func (e *AgentExecutor) Run(agent, prompt, orchestrator, sessionID string) Result {
	if e.client == nil {
		return Result{Status: "error", Output: "NATS client not configured"}
	}

	if orchestrator == "" {
		orchestrator = "claude-code"
	}

	msg := messages.ChatInbound{
		Text:         prompt,
		Author:       "scheduler",
		Agent:        agent,
		Orchestrator: orchestrator,
		Channel:      "scheduler",
		SessionID:    sessionID,
		Timestamp:    time.Now().UTC(),
	}

	subject := fmt.Sprintf("chat.inbound.%s", orchestrator)
	if err := e.client.Publish(subject, msg); err != nil {
		return Result{Status: "error", Output: fmt.Sprintf("publish failed: %v", err)}
	}

	return Result{Status: "success", Output: fmt.Sprintf("published to %s (session=%s)", subject, sessionID)}
}
