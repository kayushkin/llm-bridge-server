package executor

import (
	"errors"
	"fmt"
	"strings"
	"time"

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/bus/messages"
	"github.com/kayushkin/llm-bridge/msg"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/runstatus"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/sessionmodelrole"
)

// AgentExecutor runs agent turns by publishing a BridgeInbound message on
// NATS — the same path the interactive bridge-chat UI uses, so scheduled
// sessions land in llm-bridge-server alongside manual ones — and then follows
// the session until the turn ends, so the run's status is the turn's outcome
// rather than the fact that a message was sent (agent_session_outcome.go).
//
// For the first turn (empty BusSessionID) the executor looks up the first
// enabled harness instance matching the job's orchestrator in
// llm-bridge-server's instances and builds a CreateSessionRequest
// identical in shape to what bridge-ui sends, naming the model-store role the
// session runs on: the job's model_role, or the agent_job_model_role setting.
type AgentExecutor struct {
	client   *natsbus.Client
	bridge   Bridge
	notesURL string
	// defaultModelRole reads the agent_job_model_role setting when a run
	// starts, so a change on the settings page applies to the next session.
	defaultModelRole func() string
	modelRoles       sessionmodelrole.RoleChecker
	// sessionPollInterval is how often a run reads its session's state while
	// it waits for the turn to end. Zero takes defaultSessionPollInterval.
	sessionPollInterval time.Duration
}

// Bridge is llm-bridge-server as an agent run reads it: the instances a
// session can start on, and the session a run follows. The scheduler runs
// inside llm-bridge-server, which implements it in process as its internal
// service.
type Bridge interface {
	// Instances answers every instance, enabled or not.
	Instances() ([]msg.Instance, error)
	// Session answers one session, or an error wrapping ErrSessionNotFound
	// when there is none.
	Session(sessionID string) (*msg.ManagedSession, error)
}

// ErrSessionNotFound is the bridge holding no such session: the inbound
// message has not become a session yet, or never will.
var ErrSessionNotFound = errors.New("session not found")

// NewAgentExecutor takes the address of notes from the caller, which reads it
// from the service's settings; this package reads no environment variable.
// notesURL is where a job's agent reads and rewrites its working memory.
// defaultModelRole returns the role a job that names none starts its session
// on, and modelRoles checks that the session's harness can run it.
func NewAgentExecutor(client *natsbus.Client, bridge Bridge, notesURL string, defaultModelRole func() string, modelRoles sessionmodelrole.RoleChecker) *AgentExecutor {
	return &AgentExecutor{
		client:           client,
		bridge:           bridge,
		notesURL:         strings.TrimRight(notesURL, "/"),
		defaultModelRole: defaultModelRole,
		modelRoles:       modelRoles,
	}
}

// orchestratorHarness maps scheduler's free-form orchestrator string to the
// canonical llm-bridge harness kind. Anything unrecognized falls through to
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

// firstEnabledInstance returns the ID of the first enabled instance whose
// harness_type matches h. Errors surface back to the caller so the run record
// captures the failure.
func (e *AgentExecutor) firstEnabledInstance(h msg.Harness) (string, error) {
	if e.bridge == nil {
		return "", fmt.Errorf("the agent executor has no llm-bridge-server to read instances from")
	}
	instances, err := e.bridge.Instances()
	if err != nil {
		return "", fmt.Errorf("reading llm-bridge-server's instances: %w", err)
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
// is a workspace in notes: a markdown document the agent rewrites each run,
// which survives compaction, reaping, and a fresh session entirely. So the
// workspace is the source of truth and the session is only an optimization — a
// job with a workspace but no session still remembers everything that matters.
type AgentRun struct {
	Agent        string
	Prompt       string
	Orchestrator string
	SessionID    string
	WorkspaceID  string
	// ModelRole is the model-store role a new session starts on. Empty takes
	// the executor's default. A run that continues SessionID starts no
	// session, and the session keeps the model it was created on.
	ModelRole string
	// SessionWaitLimit is the longest the run waits for the session's turn to
	// end before it gives up as a timeout. See AgentSessionWaitLimit.
	SessionWaitLimit time.Duration
}

// workingMemoryPreamble prepends the job's prompt with the location of its
// durable memory and the contract for using it. Injected here rather than
// written into every job's prompt by hand: a convention each job re-states is
// one every job can quietly get wrong, and the whole point of the workspace is
// that the agent cannot silently forget it has one.
func workingMemoryPreamble(notesURL, workspaceID string) string {
	base := notesURL
	return fmt.Sprintf(`## Your working memory

This job has a durable memory: workspace %[1]s, in notes. It is the only thing
about you that survives between runs — your conversation context does not.

READ IT FIRST, before you do anything else:
    curl -sfS %[2]s/items/%[1]s

REWRITE IT LAST, so the next run inherits what you learned:
    curl -sfS -X PATCH %[2]s/items/%[1]s \
      -H 'Content-Type: application/json' \
      -d '{"body": "<the full updated markdown>"}'

The body is timestamped markdown, newest section first. Record what you found,
what you already escalated, and what the human decided — so you neither repeat
work nor re-raise something that was already dismissed.

Rewriting is safe: every prior version is kept, and you can read them back with
GET %[2]s/items/%[1]s/revisions. So do not hoard: summarize aggressively and
keep the workspace small enough for a human to read in one sitting. If it has
grown past that, split the detail into child workspaces (create an item of type
workspace with parent_id=%[1]s) and leave a short roll-up here.

---

`, workspaceID, base)
}

func (e *AgentExecutor) Run(run AgentRun) Result {
	if e.client == nil {
		return Result{Status: runstatus.Error, Output: "NATS client not configured"}
	}

	agent := run.Agent
	prompt := run.Prompt
	orchestrator := run.Orchestrator
	sessionID := run.SessionID

	if orchestrator == "" {
		orchestrator = "claude-code"
	}
	if run.WorkspaceID != "" {
		prompt = workingMemoryPreamble(e.notesURL, run.WorkspaceID) + prompt
	}

	now := time.Now().UTC()
	busSessionID := sessionID
	var create *msg.CreateSessionRequest

	// First turn: no BusSessionID yet → look up an instance and emit Create.
	// Subsequent turns reuse the adapter's bus_session_id → bridge_id mapping.
	var startsOn string
	if busSessionID == "" {
		var err error
		create, startsOn, err = e.newSessionRequest(agent, orchestrator, run.ModelRole)
		if err != nil {
			return Result{Status: runstatus.Error, Output: err.Error()}
		}
		busSessionID = fmt.Sprintf("scheduler-%s-%d", orchestrator, now.UnixNano())
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

	// Taken before the publish, so no state the session reaches because of
	// this message can be stamped earlier.
	publishedAt := time.Now()
	if err := e.client.Publish(messages.BridgeInboundSubject, in); err != nil {
		return Result{Status: runstatus.Error, Output: fmt.Sprintf("publish failed: %v", err)}
	}
	published := fmt.Sprintf("published to %s (bus_session=%s%s)", messages.BridgeInboundSubject, busSessionID, startsOn)

	waitLimit := run.SessionWaitLimit
	if waitLimit <= 0 {
		waitLimit = DefaultAgentSessionWaitLimit
	}
	outcome := e.waitForSessionOutcome(busSessionID, publishedAt, waitLimit)
	outcome.Output = published + "; " + outcome.Output
	return outcome
}

// newSessionRequest builds the create request for a job's first turn: the
// first enabled instance of the orchestrator's harness, and the model role the
// session runs on, checked against what that harness can run. The second value
// says which role and model, for the run's output.
func (e *AgentExecutor) newSessionRequest(agent, orchestrator, jobModelRole string) (*msg.CreateSessionRequest, string, error) {
	if e.defaultModelRole == nil || e.modelRoles == nil {
		return nil, "", fmt.Errorf("the agent executor has no model role to start a session on")
	}
	role, from := jobModelRole, "the job's model_role"
	if role == "" {
		role, from = e.defaultModelRole(), "the agent_job_model_role setting"
	}
	if role == "" {
		return nil, "", fmt.Errorf("no model role: the job names none and the agent_job_model_role setting is empty")
	}
	h := orchestratorHarness(orchestrator)
	model, err := e.modelRoles.Check(role, h)
	if err != nil {
		return nil, "", fmt.Errorf("model role %q from %s: %w", role, from, err)
	}
	instanceID, err := e.firstEnabledInstance(h)
	if err != nil {
		return nil, "", err
	}
	create := &msg.CreateSessionRequest{
		Harness:    h,
		InstanceID: instanceID,
		AgentID:    agent,
		Purpose:    "scheduled-task",
		Origin:     "scheduler",
		Type:       msg.SessionTypeAutonomous,
	}
	if err := sessionmodelrole.SetModelRole(create, role); err != nil {
		return nil, "", err
	}
	return create, fmt.Sprintf(" role=%s from %s, model=%s", role, from, model.ID), nil
}
