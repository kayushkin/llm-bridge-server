package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"

	"github.com/kayushkin/llm-bridge-server/internal/automation/clientrequestids"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/runstatus"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/sessionmodelrole"
	"github.com/kayushkin/llm-bridge-server/internal/config"
)

// AgentExecutor runs agent turns by calling llm-bridge-server's session core,
// the functions POST /sessions and POST /sessions/{id}/send call, in this
// process — so scheduled sessions land alongside manual ones and are checked
// exactly as they are — and then follows the session until the turn ends, so
// the run's status is the turn's outcome rather than the fact that a message
// was sent (agent_session_outcome.go).
//
// For the first turn (empty SessionID) the executor looks up the first enabled
// harness instance matching the job's orchestrator in llm-bridge-server's
// instances and builds a CreateSessionRequest naming the model-store role the
// session runs on: the job's model_role, or the agent_job_model_role setting.
// The session runs as the principal scheduler.agent_job_principal_id names, so
// its agent holds that principal's token for the operator routes its prompt
// calls (the workspace in notes, issues).
//
// Until 2026-11-27 the turns went out on NATS (bridge.inbound), and
// llm-bridge-adapter made the same two calls over HTTP as the internal service
// acting as nobody, so the sessions had no principal and their agents no
// token.
type AgentExecutor struct {
	bridge   Bridge
	notesURL string
	// sessionPrincipalID is the principal every session a job starts runs as,
	// or empty for none.
	sessionPrincipalID string
	// defaultModelRole reads the agent_job_model_role setting when a run
	// starts, so a change on the settings page applies to the next session.
	defaultModelRole func() string
	modelRoles       sessionmodelrole.RoleChecker
	// sessionPollInterval is how often a run reads its session's state while
	// it waits for the turn to end. Zero takes defaultSessionPollInterval.
	sessionPollInterval time.Duration
}

// Bridge is llm-bridge-server as an agent run uses it: the instances a
// session can start on, the session core that creates a session and sends it
// its turn, and the session a run follows. The scheduler runs inside
// llm-bridge-server, which implements it in process.
type Bridge interface {
	// Instances answers every instance, enabled or not.
	Instances() ([]msg.Instance, error)
	// CreateSession creates a session as POST /sessions does for the internal
	// service acting as nobody, so the session runs as the principal the
	// request names, or none. A refusal is an error carrying the route's answer.
	CreateSession(ctx context.Context, request msg.CreateSessionRequest) (*msg.ManagedSession, error)
	// SendMessage sends a message into a session as POST /sessions/{id}/send
	// does. A refusal is an error carrying the route's answer.
	SendMessage(ctx context.Context, sessionID string, request msg.SendMessageRequest) error
	// Session answers one session, or an error wrapping ErrSessionNotFound
	// when there is none.
	Session(sessionID string) (*msg.ManagedSession, error)
}

// ErrSessionNotFound is the bridge holding no such session.
var ErrSessionNotFound = errors.New("session not found")

// NewAgentExecutor takes the address of notes and the principal sessions run
// as from the caller, which reads them from the service's settings; this
// package reads no environment variable. notesURL is where a job's agent reads
// and rewrites its working memory. sessionPrincipalID is the principal every
// session a job starts runs as, or empty for none, and a job with a workspace
// is refused without one, since notes answers only a principal's token.
// defaultModelRole returns the role a job that names none starts its session
// on, and modelRoles checks that the session's harness can run it.
func NewAgentExecutor(bridge Bridge, notesURL, sessionPrincipalID string, defaultModelRole func() string, modelRoles sessionmodelrole.RoleChecker) *AgentExecutor {
	return &AgentExecutor{
		bridge:             bridge,
		notesURL:           strings.TrimRight(notesURL, "/"),
		sessionPrincipalID: sessionPrincipalID,
		defaultModelRole:   defaultModelRole,
		modelRoles:         modelRoles,
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
Notes answers only with your session's token, which is in
$LLM_BRIDGE_PRINCIPAL_TOKEN; send it on every call, as below.

READ IT FIRST, before you do anything else:
    curl -sfS -H "Authorization: Bearer $LLM_BRIDGE_PRINCIPAL_TOKEN" %[2]s/items/%[1]s

REWRITE IT LAST, so the next run inherits what you learned:
    curl -sfS -X PATCH %[2]s/items/%[1]s \
      -H "Authorization: Bearer $LLM_BRIDGE_PRINCIPAL_TOKEN" \
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

// errNoSessionPrincipalForWorkspace refuses a job with a workspace when
// sessions run as no principal: notes is an operator route, so an agent with
// no principal's token could neither read its memory nor rewrite it.
var errNoSessionPrincipalForWorkspace = fmt.Errorf("%s is empty, so this job's session would run as no principal and its agent would hold no token; notes answers its workspace only to a principal's token", config.SettingSchedulerAgentJobPrincipalID)

func (e *AgentExecutor) Run(run AgentRun) Result {
	if e.bridge == nil {
		return Result{Status: runstatus.Error, Output: "the agent executor has no llm-bridge-server to start sessions on"}
	}

	agent := run.Agent
	prompt := run.Prompt
	orchestrator := run.Orchestrator
	sessionID := run.SessionID

	if orchestrator == "" {
		orchestrator = "claude-code"
	}
	if run.WorkspaceID != "" {
		if e.sessionPrincipalID == "" {
			return Result{Status: runstatus.Error, Output: fmt.Sprintf("workspace %s: %v", run.WorkspaceID, errNoSessionPrincipalForWorkspace)}
		}
		prompt = workingMemoryPreamble(e.notesURL, run.WorkspaceID) + prompt
	}
	clientRequestID, err := clientrequestids.New()
	if err != nil {
		return Result{Status: runstatus.Error, Output: err.Error()}
	}
	send := msg.SendMessageRequest{Message: prompt, ClientRequestID: clientRequestID}

	// Taken before the session is created, so no state the session reaches
	// because of this turn can be stamped earlier.
	sentAt := time.Now()
	ctx := context.Background()
	var started string
	if sessionID == "" {
		// First turn: look up an instance and create the session.
		create, startsOn, err := e.newSessionRequest(agent, orchestrator, run.ModelRole)
		if err != nil {
			return Result{Status: runstatus.Error, Output: err.Error()}
		}
		sessionID = fmt.Sprintf("scheduler-%s-%d", orchestrator, sentAt.UTC().UnixNano())
		create.SessionID = sessionID
		if _, err := e.bridge.CreateSession(ctx, *create); err != nil {
			return Result{Status: runstatus.Error, Output: fmt.Sprintf("creating session %s: %v", sessionID, err)}
		}
		started = fmt.Sprintf("created session %s as %s%s", sessionID, describePrincipal(create.PrincipalID), startsOn)
	} else {
		started = "continued session " + sessionID
	}
	if err := e.bridge.SendMessage(ctx, sessionID, send); err != nil {
		return Result{Status: runstatus.Error, Output: fmt.Sprintf("%s; sending its turn: %v", started, err)}
	}

	waitLimit := run.SessionWaitLimit
	if waitLimit <= 0 {
		waitLimit = DefaultAgentSessionWaitLimit
	}
	outcome := e.waitForSessionOutcome(sessionID, sentAt, waitLimit)
	outcome.Output = started + "; " + outcome.Output
	return outcome
}

// describePrincipal names the principal a session runs as, for a run's output.
func describePrincipal(principalID string) string {
	if principalID == "" {
		return "no principal"
	}
	return principalID
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
		Harness:     h,
		InstanceID:  instanceID,
		AgentID:     agent,
		Purpose:     "scheduled-task",
		Origin:      "scheduler",
		Type:        msg.SessionTypeAutonomous,
		PrincipalID: e.sessionPrincipalID,
	}
	if err := sessionmodelrole.SetModelRole(create, role); err != nil {
		return nil, "", err
	}
	return create, fmt.Sprintf(" role=%s from %s, model=%s", role, from, model.ID), nil
}
