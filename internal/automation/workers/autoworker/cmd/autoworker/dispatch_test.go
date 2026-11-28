package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/sessionstarts"
	"github.com/kayushkin/llm-bridge/msg"
)

// Ported from e0f686a5 (test/first-tests-for-autoworker). Main's startWorker
// also carries a working directory, an agent and a card; these tests pass all
// three empty.
//
// What startWorker hands the bridge is a WIRE contract, and nothing downstream
// re-derives any of it. The session is created under the id startWorker mints,
// which is the same id the kanban session link points at — so a session id that
// differs by one character from the one returned leaves a card linked to a
// session that does not exist, and nothing errors.

// testSessionPrincipalID is the principal the tests start sessions as.
const testSessionPrincipalID = "principal_000001"

// recordingBridge captures the turn it was handed instead of starting it.
type recordingBridge struct {
	turn  sessionstarts.Turn
	calls int
	err   error
}

func (r *recordingBridge) start(turn sessionstarts.Turn) error {
	r.calls++
	if r.err != nil {
		return r.err
	}
	r.turn = turn
	return nil
}

// startRecorded runs startWorker against bridge as testSessionPrincipalID.
func startRecorded(bridge *recordingBridge, p providerSpec, modelRole, prompt string, maxBudgetUSD float64) (string, error) {
	return startWorker(bridge.start, p, testSessionPrincipalID, modelRole, prompt, maxBudgetUSD, "", "", "")
}

func anthropicSpec() providerSpec {
	return providerSpec{
		Name:       "anthropic",
		Harness:    msg.HarnessClaudeCode,
		InstanceID: "inst-cc-local",
	}
}

// The id returned to the caller is the id that went on the bridge. The caller writes
// it onto the kanban card as a session link, so the two disagreeing produces a
// card pointing at nothing — silently, because both halves succeed.
func TestTheReturnedSessionIDIsTheOneStarted(t *testing.T) {
	bridge := &recordingBridge{}
	got, err := startRecorded(bridge, anthropicSpec(), "default", "do the work", 0)
	if err != nil {
		t.Fatalf("startWorker: %v", err)
	}
	if bridge.turn.SessionID != got {
		t.Errorf("started session id %q but returned %q; the returned id becomes the "+
			"card's session link, so they cannot differ",
			bridge.turn.SessionID, got)
	}
	if got == "" {
		t.Error("no session id was minted at all")
	}
}

// Two fires inside one second must not collide. The id is minted here rather than
// by the server, so uniqueness is this function's job.
func TestTwoFiresInTheSameSecondGetDifferentSessionIDs(t *testing.T) {
	first, err := startRecorded(&recordingBridge{}, anthropicSpec(), "default", "prompt", 0)
	if err != nil {
		t.Fatalf("startWorker: %v", err)
	}
	second, err := startRecorded(&recordingBridge{}, anthropicSpec(), "default", "prompt", 0)
	if err != nil {
		t.Fatalf("startWorker: %v", err)
	}
	if first == second {
		t.Errorf("two fires produced the same session id %q; the second session would "+
			"attach to the first one's card", first)
	}
}

// The worker runs as the principal sessions are started as, so its agent
// holds that principal's token for the issues its prompt reads and closes.
func TestTheWorkerRunsAsThePrincipalSessionsAreStartedAs(t *testing.T) {
	bridge := &recordingBridge{}
	if _, err := startRecorded(bridge, anthropicSpec(), "default", "prompt", 0); err != nil {
		t.Fatalf("startWorker: %v", err)
	}
	if bridge.turn.Create == nil || bridge.turn.Create.PrincipalID != testSessionPrincipalID {
		t.Errorf("create %+v, want principal_id %s — without it the worker's agent holds no token for /issues/items", bridge.turn.Create, testSessionPrincipalID)
	}
	if _, err := startWorker((&recordingBridge{}).start, anthropicSpec(), "", "default", "prompt", 0, "", "", ""); err == nil {
		t.Error("a worker was started as no principal")
	}
}

// The session is autonomous and starts itself. Filed as anything else it either
// waits for a human who is asleep, or is counted among the sessions the user
// opened by hand.
func TestTheDispatchedSessionIsAutonomousAndStartsItself(t *testing.T) {
	bridge := &recordingBridge{}
	if _, err := startRecorded(bridge, anthropicSpec(), "default", "prompt", 0); err != nil {
		t.Fatalf("startWorker: %v", err)
	}
	create := bridge.turn.Create
	if create == nil {
		t.Fatal("nothing was sent to create a session with")
	}
	if create.Type != msg.SessionTypeAutonomous {
		t.Errorf("session type %q, want %q — the user is asleep and nobody is "+
			"watching this one", create.Type, msg.SessionTypeAutonomous)
	}
	if !create.AutoStart {
		t.Error("AutoStart is false, so the session waits for a human who is asleep")
	}
}

// The harness and the instance decide which machine runs the session and which
// chat the user finds it in.
func TestTheDispatchedSessionCarriesTheProvidersHarnessAndInstance(t *testing.T) {
	bridge := &recordingBridge{}
	p := anthropicSpec()
	if _, err := startRecorded(bridge, p, "default", "prompt", 0); err != nil {
		t.Fatalf("startWorker: %v", err)
	}
	if bridge.turn.Create.Harness != p.Harness {
		t.Errorf("harness %q, want %q", bridge.turn.Create.Harness, p.Harness)
	}
	if bridge.turn.Create.InstanceID != p.InstanceID {
		t.Errorf("instance %q, want %q", bridge.turn.Create.InstanceID, p.InstanceID)
	}
}

// The prompt is the whole task. A fire that sends an empty one spends a
// session on nothing.
func TestThePromptReachesTheSession(t *testing.T) {
	bridge := &recordingBridge{}
	const prompt = "Rename the thing in four places."
	if _, err := startRecorded(bridge, anthropicSpec(), "default", prompt, 0); err != nil {
		t.Fatalf("startWorker: %v", err)
	}
	if !strings.Contains(bridge.turn.Send.Message, prompt) {
		t.Errorf("the sent prompt %q does not carry the task", bridge.turn.Send.Message)
	}
}

// The resolved ceiling has to survive the trip onto the bridge. Resolving it
// correctly and then sending a create without it is indistinguishable, from
// the outside, from having no ceiling at all.
func TestTheResolvedCeilingIsSentOnBothEnforcers(t *testing.T) {
	bridge := &recordingBridge{}
	if _, err := startRecorded(bridge, anthropicSpec(), "default", "prompt", 3.50); err != nil {
		t.Fatalf("startWorker: %v", err)
	}
	create := bridge.turn.Create
	if create.MaxBudget == nil || *create.MaxBudget != 3.50 {
		t.Errorf("server-side ceiling %v, want 3.50", create.MaxBudget)
	}
	var harnessConfig struct {
		MaxBudgetUSD float64 `json:"max_budget_usd"`
	}
	if err := json.Unmarshal(create.HarnessConfig, &harnessConfig); err != nil {
		t.Fatalf("harness config is not an object: %v", err)
	}
	if harnessConfig.MaxBudgetUSD != 3.50 {
		t.Errorf("harness-native ceiling %v, want 3.50", harnessConfig.MaxBudgetUSD)
	}
}

// An uncapped fire sends NO ceiling rather than a zero one. Zero is read as
// unlimited by both enforcers, so the two are the same on the wire — but writing
// it means a later reader cannot tell "nobody set a cap" from "somebody set a cap
// of nothing", and the distinction is the whole of this mechanism.
func TestAnUncappedFireSendsNoCeilingAtAll(t *testing.T) {
	bridge := &recordingBridge{}
	if _, err := startRecorded(bridge, anthropicSpec(), "default", "prompt", 0); err != nil {
		t.Fatalf("startWorker: %v", err)
	}
	if bridge.turn.Create.MaxBudget != nil {
		t.Errorf("sent a server-side ceiling of %v for an uncapped fire",
			*bridge.turn.Create.MaxBudget)
	}
	var harnessConfig map[string]any
	if err := json.Unmarshal(bridge.turn.Create.HarnessConfig, &harnessConfig); err != nil {
		t.Fatalf("harness config is not an object: %v", err)
	}
	if _, has := harnessConfig["max_budget_usd"]; has {
		t.Errorf("sent a harness config %q with a ceiling for an uncapped fire",
			string(bridge.turn.Create.HarnessConfig))
	}
}

// The lane's model role goes on the wire as the session's own model, beside the
// ceiling, so the bridge's per-harness default never decides what a worker runs
// on.
func TestTheModelRoleIsSentBesideTheCeiling(t *testing.T) {
	bridge := &recordingBridge{}
	if _, err := startRecorded(bridge, anthropicSpec(), "best", "prompt", 3.50); err != nil {
		t.Fatalf("startWorker: %v", err)
	}
	var harnessConfig struct {
		Model        string  `json:"model"`
		MaxBudgetUSD float64 `json:"max_budget_usd"`
	}
	if err := json.Unmarshal(bridge.turn.Create.HarnessConfig, &harnessConfig); err != nil {
		t.Fatalf("harness config is not an object: %v", err)
	}
	if harnessConfig.Model != "best" || harnessConfig.MaxBudgetUSD != 3.50 {
		t.Errorf("harness config %s, want model best and the 3.50 ceiling", bridge.turn.Create.HarnessConfig)
	}
	if _, err := startRecorded(&recordingBridge{}, anthropicSpec(), "", "prompt", 0); err == nil {
		t.Error("a worker was started with no model role")
	}
}

// Each lane reads its own setting. One setting for both would put an OpenAI
// model on the claude_code lane or the other way round.
func TestEachLaneReadsItsOwnModelRoleSetting(t *testing.T) {
	seen := map[string]bool{}
	for _, lane := range providers {
		if lane.ModelRoleSetting == "" {
			t.Errorf("lane %s names no model role setting", lane.Name)
		}
		if seen[lane.ModelRoleSetting] {
			t.Errorf("lane %s shares setting %s with another lane", lane.Name, lane.ModelRoleSetting)
		}
		seen[lane.ModelRoleSetting] = true
	}
}

// A start that fails must not be reported as a session that started: the caller
// stamps the cooldown and links a kanban card on the strength of this return.
func TestAFailedStartReturnsNoSessionID(t *testing.T) {
	bridge := &recordingBridge{err: errTestStart}
	got, err := startRecorded(bridge, anthropicSpec(), "default", "prompt", 0)
	if err == nil {
		t.Fatal("a failed start was reported as a successful fire")
	}
	if got != "" {
		t.Errorf("a failed start returned session id %q, which the caller would link "+
			"a card to", got)
	}
}

type testStartError struct{}

func (testStartError) Error() string { return "bridge is down" }

var errTestStart = testStartError{}
