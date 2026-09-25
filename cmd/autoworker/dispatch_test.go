package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kayushkin/bus/messages"
	"github.com/kayushkin/llm-bridge/msg"
)

// Ported from e0f686a5 (test/first-tests-for-autoworker). Main's publishWorker
// also carries a working directory, an agent and a card; these tests pass all
// three empty.
//
// What publishWorker puts on the bus is a WIRE contract, and nothing downstream
// re-derives any of it. The adapter passes BusSessionID through unchanged as the
// bridge session_id, which is the same id the kanban session link points at — so
// a session id that differs by one character from the one returned leaves a card
// linked to a session that does not exist, and nothing errors.

// recordingBus captures what was published instead of sending it.
type recordingBus struct {
	subject string
	inbound messages.BridgeInbound
	calls   int
	err     error
}

func (r *recordingBus) Publish(subject string, data any) error {
	r.calls++
	if r.err != nil {
		return r.err
	}
	r.subject = subject
	in, ok := data.(messages.BridgeInbound)
	if !ok {
		panic("autoworker published something that is not a BridgeInbound")
	}
	r.inbound = in
	return nil
}

func anthropicSpec() providerSpec {
	return providerSpec{
		Name:         "anthropic",
		Harness:      msg.HarnessClaudeCode,
		InstanceID:   "inst-cc-local",
		Channel:      "autoworker",
		Orchestrator: "claude-code",
	}
}

// The id returned to the caller is the id that went on the bus. The caller writes
// it onto the kanban card as a session link, so the two disagreeing produces a
// card pointing at nothing — silently, because both halves succeed.
func TestTheReturnedSessionIDIsTheOnePublished(t *testing.T) {
	bus := &recordingBus{}
	got, err := publishWorker(bus, anthropicSpec(), "do the work", 0, "", "", "")
	if err != nil {
		t.Fatalf("publishWorker: %v", err)
	}
	if bus.inbound.BusSessionID != got {
		t.Errorf("published session id %q but returned %q; the returned id becomes the "+
			"card's session link, so they cannot differ",
			bus.inbound.BusSessionID, got)
	}
	if got == "" {
		t.Error("no session id was minted at all")
	}
}

// Two fires inside one second must not collide. The id is minted here rather than
// by the server, so uniqueness is this function's job.
func TestTwoFiresInTheSameSecondGetDifferentSessionIDs(t *testing.T) {
	first, err := publishWorker(&recordingBus{}, anthropicSpec(), "prompt", 0, "", "", "")
	if err != nil {
		t.Fatalf("publishWorker: %v", err)
	}
	second, err := publishWorker(&recordingBus{}, anthropicSpec(), "prompt", 0, "", "", "")
	if err != nil {
		t.Fatalf("publishWorker: %v", err)
	}
	if first == second {
		t.Errorf("two fires produced the same session id %q; the second session would "+
			"attach to the first one's card", first)
	}
}

func TestTheWorkerIsPublishedOnTheBridgeInboundSubject(t *testing.T) {
	bus := &recordingBus{}
	if _, err := publishWorker(bus, anthropicSpec(), "prompt", 0, "", "", ""); err != nil {
		t.Fatalf("publishWorker: %v", err)
	}
	if bus.subject != messages.BridgeInboundSubject {
		t.Errorf("published on %q, want %q — nothing is listening anywhere else",
			bus.subject, messages.BridgeInboundSubject)
	}
}

// The session is autonomous and starts itself. Filed as anything else it either
// waits for a human who is asleep, or is counted among the sessions the user
// opened by hand.
func TestTheDispatchedSessionIsAutonomousAndStartsItself(t *testing.T) {
	bus := &recordingBus{}
	if _, err := publishWorker(bus, anthropicSpec(), "prompt", 0, "", "", ""); err != nil {
		t.Fatalf("publishWorker: %v", err)
	}
	create := bus.inbound.Create
	if create == nil {
		t.Fatal("nothing was published to create a session with")
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
	bus := &recordingBus{}
	p := anthropicSpec()
	if _, err := publishWorker(bus, p, "prompt", 0, "", "", ""); err != nil {
		t.Fatalf("publishWorker: %v", err)
	}
	if bus.inbound.Create.Harness != p.Harness {
		t.Errorf("harness %q, want %q", bus.inbound.Create.Harness, p.Harness)
	}
	if bus.inbound.Create.InstanceID != p.InstanceID {
		t.Errorf("instance %q, want %q", bus.inbound.Create.InstanceID, p.InstanceID)
	}
	if bus.inbound.Orchestrator != p.Orchestrator {
		t.Errorf("orchestrator %q, want %q", bus.inbound.Orchestrator, p.Orchestrator)
	}
}

// The prompt is the whole task. A fire that publishes an empty one spends a
// session on nothing.
func TestThePromptReachesTheSession(t *testing.T) {
	bus := &recordingBus{}
	const prompt = "Rename the thing in four places."
	if _, err := publishWorker(bus, anthropicSpec(), prompt, 0, "", "", ""); err != nil {
		t.Fatalf("publishWorker: %v", err)
	}
	if bus.inbound.Send == nil {
		t.Fatal("nothing was published to send the prompt with")
	}
	if !strings.Contains(bus.inbound.Send.Message, prompt) {
		t.Errorf("the published prompt %q does not carry the task", bus.inbound.Send.Message)
	}
}

// The resolved ceiling has to survive the trip onto the bus. Resolving it
// correctly and then publishing a message without it is indistinguishable, from
// the outside, from having no ceiling at all.
func TestTheResolvedCeilingIsPublishedOnBothEnforcers(t *testing.T) {
	bus := &recordingBus{}
	if _, err := publishWorker(bus, anthropicSpec(), "prompt", 3.50, "", "", ""); err != nil {
		t.Fatalf("publishWorker: %v", err)
	}
	create := bus.inbound.Create
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

// An uncapped fire publishes NO ceiling rather than a zero one. Zero is read as
// unlimited by both enforcers, so the two are the same on the wire — but writing
// it means a later reader cannot tell "nobody set a cap" from "somebody set a cap
// of nothing", and the distinction is the whole of this mechanism.
func TestAnUncappedFirePublishesNoCeilingAtAll(t *testing.T) {
	bus := &recordingBus{}
	if _, err := publishWorker(bus, anthropicSpec(), "prompt", 0, "", "", ""); err != nil {
		t.Fatalf("publishWorker: %v", err)
	}
	if bus.inbound.Create.MaxBudget != nil {
		t.Errorf("published a server-side ceiling of %v for an uncapped fire",
			*bus.inbound.Create.MaxBudget)
	}
	if bus.inbound.Create.HarnessConfig != nil {
		t.Errorf("published a harness config %q for an uncapped fire",
			string(bus.inbound.Create.HarnessConfig))
	}
}

// A publish that fails must not be reported as a session that started: the caller
// stamps the cooldown and links a kanban card on the strength of this return.
func TestAFailedPublishReturnsNoSessionID(t *testing.T) {
	bus := &recordingBus{err: errTestPublish}
	got, err := publishWorker(bus, anthropicSpec(), "prompt", 0, "", "", "")
	if err == nil {
		t.Fatal("a failed publish was reported as a successful fire")
	}
	if got != "" {
		t.Errorf("a failed publish returned session id %q, which the caller would link "+
			"a card to", got)
	}
}

type testPublishError struct{}

func (testPublishError) Error() string { return "bus is down" }

var errTestPublish = testPublishError{}
