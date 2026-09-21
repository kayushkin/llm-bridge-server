package executor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	client       *natsbus.Client
	bridgeURL    string
	noteboardURL string
	httpClient   *http.Client
}

// NewAgentExecutor takes both addresses from the caller, which reads them from
// the service's settings; this package reads no environment variable.
// noteboardURL is where a job's agent reads and rewrites its working memory.
func NewAgentExecutor(client *natsbus.Client, bridgeURL, noteboardURL string) *AgentExecutor {
	return &AgentExecutor{
		client:       client,
		bridgeURL:    bridgeURL,
		noteboardURL: strings.TrimRight(noteboardURL, "/"),
		httpClient:   &http.Client{Timeout: 5 * time.Second},
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

// AgentRun is one scheduled turn of an agent job.
//
// SessionID and WorkspaceID are the job's two kinds of memory, and they are not
// redundant. SessionID is the conversation: rich, free, and LOSSY — compaction
// erodes it and the bridge's reaper eventually discards the process. WorkspaceID
// is a noteboard workspace: a markdown document the agent rewrites each run,
// which survives compaction, reaping, and a fresh session entirely. So the
// workspace is the source of truth and the session is only an optimisation — a
// job with a workspace but no session still remembers everything that matters.
type AgentRun struct {
	Agent        string
	Prompt       string
	Orchestrator string
	SessionID    string
	WorkspaceID  string
}

// workingMemoryPreamble prepends the job's prompt with the location of its
// durable memory and the contract for using it. Injected here rather than
// written into every job's prompt by hand: a convention each job re-states is
// one every job can quietly get wrong, and the whole point of the workspace is
// that the agent cannot silently forget it has one.
func workingMemoryPreamble(noteboardURL, workspaceID string) string {
	base := noteboardURL
	return fmt.Sprintf(`## Your working memory

This job has a durable memory: noteboard workspace %[1]s. It is the only thing
about you that survives between runs — your conversation context does not.

READ IT FIRST, before you do anything else:
    curl -sfS %[2]s/api/items/%[1]s

REWRITE IT LAST, so the next run inherits what you learned:
    curl -sfS -X PATCH %[2]s/api/items/%[1]s \
      -H 'Content-Type: application/json' \
      -d '{"body": "<the full updated markdown>"}'

The body is timestamped markdown, newest section first. Record what you found,
what you already escalated, and what the human decided — so you neither repeat
work nor re-raise something that was already dismissed.

Rewriting is safe: every prior version is kept, and you can read them back with
GET %[2]s/api/items/%[1]s/revisions. So do not hoard: summarise aggressively and
keep the workspace small enough for a human to read in one sitting. If it has
grown past that, split the detail into child workspaces (create an item of type
workspace with parent_id=%[1]s) and leave a short roll-up here.

---

`, workspaceID, base)
}

func (e *AgentExecutor) Run(run AgentRun) Result {
	if e.client == nil {
		return Result{Status: "error", Output: "NATS client not configured"}
	}

	agent := run.Agent
	prompt := run.Prompt
	orchestrator := run.Orchestrator
	sessionID := run.SessionID

	if orchestrator == "" {
		orchestrator = "claude-code"
	}
	if run.WorkspaceID != "" {
		prompt = workingMemoryPreamble(e.noteboardURL, run.WorkspaceID) + prompt
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
			Harness:    h,
			InstanceID: instanceID,
			AgentID:    agent,
			Purpose:    "scheduled-task",
			Origin:     "scheduler",
			Type:       msg.SessionTypeAutonomous,
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
