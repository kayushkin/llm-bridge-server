package executor

import (
	"encoding/json"
	"fmt"
	"time"

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/bus/messages"
)

// AgentExecutor runs agent turns via NATS request/reply.
type AgentExecutor struct {
	client *natsbus.Client
}

func NewAgentExecutor(client *natsbus.Client) *AgentExecutor {
	return &AgentExecutor{client: client}
}

func (e *AgentExecutor) Run(agent, prompt, model, sessionTarget, jobID string) Result {
	if e.client == nil {
		return Result{Status: "error", Output: "NATS client not configured"}
	}

	req := messages.AgentRunRequest{
		Agent:         agent,
		Prompt:        prompt,
		Model:         model,
		SessionTarget: sessionTarget,
		JobID:         jobID,
	}

	respBytes, err := e.client.Request("agent.run", req, 5*time.Minute)
	if err != nil {
		return Result{Status: "error", Output: fmt.Sprintf("agent request failed: %v", err)}
	}

	var res messages.AgentRunResult
	if err := json.Unmarshal(respBytes, &res); err != nil {
		return Result{Status: "error", Output: fmt.Sprintf("failed to parse agent response: %v", err)}
	}

	if !res.Success {
		errMsg := res.Error
		if errMsg == "" {
			errMsg = "agent returned failure"
		}
		return Result{Status: "error", Output: errMsg}
	}

	return Result{Status: "success", Output: res.Output}
}
