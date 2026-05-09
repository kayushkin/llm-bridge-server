package executor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/bus/messages"
	"github.com/kayushkin/llm-bridge/msg"
)

// AgentExecutor runs agent turns by publishing a BridgeInbound message on
// NATS — the same path the interactive bridge-chat UI uses, so scheduled
// sessions land in llm-bridge-server alongside manual ones.
//
// For the first turn (empty BusSessionID) the executor looks up the first
// enabled harness instance matching the job's orchestrator via
// llm-bridge-server's GET /instances and builds a CreateSessionRequest
// identical in shape to what bridge-ui sends.
type AgentExecutor struct {
	client     *natsbus.Client
	bridgeURL  string
	httpClient *http.Client
}

func NewAgentExecutor(client *natsbus.Client) *AgentExecutor {
	url := os.Getenv("LLMBRIDGE_URL")
	if url == "" {
		url = "http://localhost:8160"
	}
	return &AgentExecutor{
		client:     client,
		bridgeURL:  url,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// orchestratorHarness maps scheduler's free-form orchestrator string to the
// canonical llm-bridge harness kind. Anything unrecognised falls through to
// claude-code (the interactive default).
func orchestratorHarness(orchestrator string) msg.Harness {
	switch orchestrator {
	case "", "claude-code", "claude_code":
		return msg.HarnessClaudeCode
	case "codex":
		return msg.HarnessCodex
	case "openclaw":
		return msg.HarnessOpenClaw
	case "inber":
		return msg.HarnessInber
	case "hermes":
		return msg.HarnessHermes
	default:
		return msg.Harness(orchestrator)
	}
}

// firstEnabledInstance calls GET /instances on llm-bridge-server and returns
// the ID of the first enabled instance whose harness_type matches h. Errors
// surface back to the caller so the run record captures the failure.
func (e *AgentExecutor) firstEnabledInstance(h msg.Harness) (string, error) {
	resp, err := e.httpClient.Get(e.bridgeURL + "/instances")
	if err != nil {
		return "", fmt.Errorf("GET %s/instances: %w", e.bridgeURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("instances returned %d: %s", resp.StatusCode, body)
	}
	var instances []msg.Instance
	if err := json.NewDecoder(resp.Body).Decode(&instances); err != nil {
		return "", fmt.Errorf("decode instances: %w", err)
	}
	for _, inst := range instances {
		if inst.Enabled && inst.HarnessType == h {
			return inst.ID, nil
		}
	}
	return "", fmt.Errorf("no enabled instance found for harness %s", h)
}

func (e *AgentExecutor) Run(agent, prompt, orchestrator, sessionID string) Result {
	if e.client == nil {
		return Result{Status: "error", Output: "NATS client not configured"}
	}

	if orchestrator == "" {
		orchestrator = "claude-code"
	}

	now := time.Now().UTC()
	busSessionID := sessionID
	var create *msg.CreateSessionRequest

	// First turn: no BusSessionID yet → look up an instance and emit Create.
	// Subsequent turns reuse the adapter's bus_session_id → bridge_id mapping.
	if busSessionID == "" {
		h := orchestratorHarness(orchestrator)
		instanceID, err := e.firstEnabledInstance(h)
		if err != nil {
			return Result{Status: "error", Output: err.Error()}
		}
		busSessionID = fmt.Sprintf("scheduler-%s-%d", orchestrator, now.UnixNano())
		create = &msg.CreateSessionRequest{
			Harness:     h,
			InstanceID:  instanceID,
			AgentID:     agent,
			Purpose:     "scheduled-task",
			Origin:      "scheduler",
			Type:        msg.SessionTypeAutonomous,
		}
	}

	in := messages.BridgeInbound{
		BusSessionID: busSessionID,
		Agent:        agent,
		Orchestrator: orchestrator,
		Channel:      "scheduler",
		Timestamp:    now,
		Create:       create,
		Send: &msg.SendMessageRequest{
			Message: prompt,
		},
	}

	if err := e.client.Publish(messages.BridgeInboundSubject, in); err != nil {
		return Result{Status: "error", Output: fmt.Sprintf("publish failed: %v", err)}
	}

	return Result{Status: "success", Output: fmt.Sprintf("published to %s (bus_session=%s)", messages.BridgeInboundSubject, busSessionID)}
}
