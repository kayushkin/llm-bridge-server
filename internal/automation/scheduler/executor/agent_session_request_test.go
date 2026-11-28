package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/runstatus"
	"github.com/kayushkin/llm-bridge-server/internal/config"
)

// recordingBridge is llm-bridge-server holding one enabled claude_code
// instance. It records each create and send, and answers every read of a
// session with one that has finished the turn it was sent.
type recordingBridge struct {
	creates []msg.CreateSessionRequest
	sends   []recordedSend
	// refuseCreate, when set, is the create's refusal.
	refuseCreate error
}

type recordedSend struct {
	sessionID string
	request   msg.SendMessageRequest
}

func (bridge *recordingBridge) Instances() ([]msg.Instance, error) {
	return []msg.Instance{{ID: "inst-cc", HarnessType: msg.HarnessClaudeCode, Enabled: true}}, nil
}

func (bridge *recordingBridge) CreateSession(ctx context.Context, request msg.CreateSessionRequest) (*msg.ManagedSession, error) {
	bridge.creates = append(bridge.creates, request)
	if bridge.refuseCreate != nil {
		return nil, bridge.refuseCreate
	}
	return &msg.ManagedSession{SessionID: request.SessionID}, nil
}

func (bridge *recordingBridge) SendMessage(ctx context.Context, sessionID string, request msg.SendMessageRequest) error {
	bridge.sends = append(bridge.sends, recordedSend{sessionID: sessionID, request: request})
	return nil
}

func (bridge *recordingBridge) Session(sessionID string) (*msg.ManagedSession, error) {
	return sessionIn(msg.SessionIdle, msg.ReasonTurnComplete, time.Now().Add(time.Hour)), nil
}

func recordingExecutor(bridge *recordingBridge, principalID string) *AgentExecutor {
	executor := NewAgentExecutor(bridge, "http://notes.test/notes", principalID, func() string { return "default" }, &stubModelRoles{})
	executor.sessionPollInterval = time.Millisecond
	return executor
}

// A job's first turn creates one session and sends it one message: the create
// carries everything the bus message's create carried — harness, instance,
// agent, purpose, origin, type, the model role — plus the session id the
// adapter used to stamp on it and the principal the setting names, and the
// send carries the prompt and a client request id, as the adapter minted one.
func TestAFirstTurnCreatesTheSessionAsTheConfiguredPrincipalAndSendsThePrompt(t *testing.T) {
	bridge := &recordingBridge{}
	result := recordingExecutor(bridge, "principal_000001").Run(AgentRun{Agent: "deploy-judge", Prompt: "JUDGE THE DRIFT", Orchestrator: "claude-code", WorkspaceID: "ws-1"})
	if result.Status != runstatus.Success {
		t.Fatalf("run = %q %q", result.Status, result.Output)
	}
	if len(bridge.creates) != 1 || len(bridge.sends) != 1 {
		t.Fatalf("creates %d, sends %d; want one of each", len(bridge.creates), len(bridge.sends))
	}
	create := bridge.creates[0]
	if !strings.HasPrefix(create.SessionID, "scheduler-claude-code-") {
		t.Errorf("session_id = %q, want the scheduler-<orchestrator>-<nanoseconds> form jobs have always had", create.SessionID)
	}
	if create.PrincipalID != "principal_000001" {
		t.Errorf("principal_id = %q, want the configured principal", create.PrincipalID)
	}
	if create.Harness != msg.HarnessClaudeCode || create.InstanceID != "inst-cc" || create.AgentID != "deploy-judge" {
		t.Errorf("harness %q, instance %q, agent %q", create.Harness, create.InstanceID, create.AgentID)
	}
	if create.Purpose != "scheduled-task" || create.Origin != "scheduler" || create.Type != msg.SessionTypeAutonomous {
		t.Errorf("purpose %q, origin %q, type %q", create.Purpose, create.Origin, create.Type)
	}
	if create.AutoStart {
		t.Error("auto_start is set; the bus message never set it, and the send starts the harness")
	}
	if got := harnessConfigModel(t, &create); got != "default" {
		t.Errorf("harness_config.model = %q, want the setting's role", got)
	}
	send := bridge.sends[0]
	if send.sessionID != create.SessionID {
		t.Errorf("sent to %q, created %q", send.sessionID, create.SessionID)
	}
	if !strings.HasSuffix(send.request.Message, "JUDGE THE DRIFT") || !strings.Contains(send.request.Message, "ws-1") {
		t.Errorf("message does not carry the workspace preamble and then the prompt: %q", send.request.Message)
	}
	if !strings.HasPrefix(send.request.ClientRequestID, "req_") {
		t.Errorf("client_request_id = %q, want one minted in the adapter's form", send.request.ClientRequestID)
	}
	if !strings.Contains(result.Output, "as principal_000001") {
		t.Errorf("run output %q does not say whom the session runs as", result.Output)
	}
}

// The preamble's curls carry the session's token, since notes answers only a
// principal's.
func TestThePreambleSendsTheSessionToken(t *testing.T) {
	preamble := workingMemoryPreamble("http://notes.test/notes", "ws-1")
	if strings.Count(preamble, `-H "Authorization: Bearer $LLM_BRIDGE_PRINCIPAL_TOKEN"`) != 2 {
		t.Errorf("the read and the rewrite must both send the token:\n%s", preamble)
	}
}

// A job that continues its session sends one message into it and creates
// nothing.
func TestAContinuedTurnOnlySends(t *testing.T) {
	bridge := &recordingBridge{}
	result := recordingExecutor(bridge, "principal_000001").Run(AgentRun{Prompt: "again", SessionID: "scheduler-claude-code-7"})
	if result.Status != runstatus.Success {
		t.Fatalf("run = %q %q", result.Status, result.Output)
	}
	if len(bridge.creates) != 0 || len(bridge.sends) != 1 || bridge.sends[0].sessionID != "scheduler-claude-code-7" {
		t.Fatalf("creates %v, sends %v", bridge.creates, bridge.sends)
	}
}

// A job with a workspace and no principal to run as fails its run before any
// session exists, naming the setting: its agent could not read its memory.
func TestAWorkspaceJobWithNoPrincipalFailsNamingTheSetting(t *testing.T) {
	bridge := &recordingBridge{}
	result := recordingExecutor(bridge, "").Run(AgentRun{Prompt: "p", WorkspaceID: "ws-1"})
	if result.Status != runstatus.Error || !strings.Contains(result.Output, config.SettingSchedulerAgentJobPrincipalID) {
		t.Fatalf("run = %q %q, want an error naming %s", result.Status, result.Output, config.SettingSchedulerAgentJobPrincipalID)
	}
	if len(bridge.creates)+len(bridge.sends) != 0 {
		t.Fatal("a session was started for a job whose agent could not reach its workspace")
	}

	// A job with no workspace still runs, as no principal.
	result = recordingExecutor(bridge, "").Run(AgentRun{Prompt: "p"})
	if result.Status != runstatus.Success || bridge.creates[0].PrincipalID != "" {
		t.Fatalf("run = %q %q, principal %q", result.Status, result.Output, bridge.creates[0].PrincipalID)
	}
}

// A refused create fails the run with the bridge's answer, and nothing is sent.
func TestARefusedCreateFailsTheRun(t *testing.T) {
	bridge := &recordingBridge{refuseCreate: fmt.Errorf("POST /sessions answered 400: unknown_principal")}
	result := recordingExecutor(bridge, "principal_000099").Run(AgentRun{Prompt: "p"})
	if result.Status != runstatus.Error || !strings.Contains(result.Output, "unknown_principal") {
		t.Fatalf("run = %q %q", result.Status, result.Output)
	}
	if len(bridge.sends) != 0 {
		t.Fatal("a message was sent to a session that was never created")
	}
}
