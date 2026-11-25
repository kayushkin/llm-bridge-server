package evalstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// fakeBridge is the operations coordinator: one operation per idempotency
// key, running for one read, then finished as the handler says.
type fakeBridge struct {
	mutex      sync.Mutex
	intents    map[string]msg.OperationIntent
	reads      map[string]int
	finish     func(intent msg.OperationIntent, receipt *msg.OperationReceipt)
	refuseNext bool
	canceled   []string
}

func newFakeBridge(finish func(msg.OperationIntent, *msg.OperationReceipt)) *fakeBridge {
	return &fakeBridge{intents: map[string]msg.OperationIntent{}, reads: map[string]int{}, finish: finish}
}

func (bridge *fakeBridge) SubmitOperation(_ context.Context, intent msg.OperationIntent) (msg.OperationReceipt, error) {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	if bridge.refuseNext {
		return msg.OperationReceipt{}, fmt.Errorf("%w: organization_budget_exhausted", ErrBridgeBudgetRefused)
	}
	id := "operation_" + strings.ReplaceAll(intent.IdempotencyKey, ":", "_")
	bridge.intents[id] = intent
	return msg.OperationReceipt{ID: id, State: msg.OperationStateQueued}, nil
}

func (bridge *fakeBridge) ReadOperation(_ context.Context, id string) (msg.OperationReceipt, error) {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	bridge.reads[id]++
	receipt := msg.OperationReceipt{ID: id, State: msg.OperationStateRunning}
	if bridge.reads[id] > 1 {
		bridge.finish(bridge.intents[id], &receipt)
	}
	return receipt, nil
}

func (bridge *fakeBridge) CancelOperation(_ context.Context, id string) error {
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	bridge.canceled = append(bridge.canceled, id)
	return nil
}

func (bridge *fakeBridge) ListSucceededOperations(context.Context, OperationQuery) ([]msg.OperationReceipt, error) {
	return nil, nil
}

func (bridge *fakeBridge) ReadOperationIntent(context.Context, string) (msg.OperationIntent, error) {
	return msg.OperationIntent{}, ErrOperationIntentsNotServed
}

func succeedWithAnswer(intent msg.OperationIntent, receipt *msg.OperationReceipt) {
	var input msg.LLMCompletionInput
	json.Unmarshal(intent.Input, &input)
	receipt.State = msg.OperationStateSucceeded
	receipt.Result, _ = json.Marshal(msg.LLMCompletionResult{Parsed: json.RawMessage(`{"ok":true}`)})
	receipt.Usage = &msg.OperationUsage{Tokens: msg.TokenUsage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 100, CacheWriteTokens: 7}}
	detail, _ := json.Marshal(map[string]any{"answered_by": input.Model, "duration_ms": 1234})
	receipt.Evidence = []msg.OperationEvidence{{Kind: "model_call", Detail: detail}}
}

func testBridgeOperations(bridge *fakeBridge) *BridgeOperations {
	return &BridgeOperations{Operations: bridge, PollInterval: time.Millisecond}
}

func TestTheBridgeOperationsCallsAsAnInternalServiceAndReadsTheReceipt(t *testing.T) {
	bridge := newFakeBridge(succeedWithAnswer)
	outcome, err := testBridgeOperations(bridge).Complete(context.Background(), CompletionRequest{OrganizationID: "principal_000900",
		IdempotencyKey: "eval-store:run:case:answer:model-small:1", CorrelationID: "eval_run_000001", Model: "model-small", Prompt: "p",
		Schema: map[string]any{"type": "object"}, MaxTokens: 100, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if string(outcome.Parsed) != `{"ok":true}` || outcome.InputTokens != 10 || outcome.CacheReadTokens != 100 || outcome.CacheWriteTokens != 7 ||
		outcome.OutputTokens != 5 || !outcome.UsageKnown || outcome.AnsweredBy != "model-small" || *outcome.DurationMs != 1234 {
		t.Errorf("outcome %+v", outcome)
	}
	for _, intent := range bridge.intents {
		var input msg.LLMCompletionInput
		json.Unmarshal(intent.Input, &input)
		if intent.Type != msg.OperationTypeLLMCompletion || intent.OrganizationID != "principal_000900" || intent.CorrelationID != "eval_run_000001" || input.Model != "model-small" {
			t.Errorf("intent %+v input %+v", intent, input)
		}
	}
}

func TestTheBridgeOperationsNamesABudgetRefusal(t *testing.T) {
	bridge := newFakeBridge(succeedWithAnswer)
	bridge.refuseNext = true
	_, err := testBridgeOperations(bridge).Complete(context.Background(), CompletionRequest{IdempotencyKey: "k", Model: "m", Timeout: time.Second})
	if !errors.Is(err, ErrBridgeBudgetRefused) {
		t.Errorf("a refusal for money = %v, want ErrBridgeBudgetRefused", err)
	}
	failing := newFakeBridge(func(_ msg.OperationIntent, receipt *msg.OperationReceipt) {
		receipt.State = msg.OperationStateFailed
		receipt.Error = &msg.OperationError{Code: "budget_exhausted", Message: "no budget left"}
	})
	outcome, err := testBridgeOperations(failing).Complete(context.Background(), CompletionRequest{IdempotencyKey: "k", Model: "m", Timeout: time.Second})
	if !errors.Is(err, ErrBridgeBudgetRefused) || outcome.OperationID == "" {
		t.Errorf("budget_exhausted = %v (operation %q)", err, outcome.OperationID)
	}
}

func TestTheBridgeOperationsCancelsATimedOutCallButNotOneItsCallerLeft(t *testing.T) {
	bridge := newFakeBridge(func(_ msg.OperationIntent, receipt *msg.OperationReceipt) { receipt.State = msg.OperationStateRunning })
	client := testBridgeOperations(bridge)
	if _, err := client.Complete(context.Background(), CompletionRequest{IdempotencyKey: "timed-out", Model: "m", Timeout: 50 * time.Millisecond}); err == nil {
		t.Fatal("a call that never finished succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := client.Complete(ctx, CompletionRequest{IdempotencyKey: "left", Model: "m", Timeout: time.Minute}); err == nil {
		t.Fatal("a call whose caller left succeeded")
	}
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	if len(bridge.canceled) != 1 || !strings.Contains(bridge.canceled[0], "timed-out") {
		t.Errorf("canceled %v, want only the timed-out call", bridge.canceled)
	}
}

func TestTheBridgeOperationsRefuseToCallWithNoServerToCall(t *testing.T) {
	client := &BridgeOperations{}
	if _, err := client.Complete(context.Background(), CompletionRequest{Timeout: time.Second}); err == nil || !strings.Contains(err.Error(), "no llm-bridge-server") {
		t.Errorf("Complete = %v", err)
	}
}

func TestReadIntentSaysTheBridgeServesNoIntents(t *testing.T) {
	if _, err := testBridgeOperations(newFakeBridge(succeedWithAnswer)).ReadIntent(context.Background(), "operation_1"); !errors.Is(err, ErrOperationIntentsNotServed) {
		t.Errorf("ReadIntent from a bridge that serves no intents = %v", err)
	}
}

func TestBudgetVerdictStopsAboveALimitOnAStaleReadingAndOnAMissingOne(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	fresh := now.Add(time.Hour).Unix()
	limits := UsageLimits{
		"anthropic": {Windows: map[string]UsageWindow{"seven_day": {UsedPercent: 41}}, StaleAfter: fresh},
		"codex":     {Windows: map[string]UsageWindow{"weekly": {UsedPercent: 60}}, StaleAfter: fresh},
	}
	if verdict := budgetVerdict(limits, syntheticLimits, now); !strings.Contains(verdict, "anthropic seven_day is at 41%") || strings.Contains(verdict, "codex") {
		t.Errorf("verdict %q: want anthropic over its 40%%, codex at exactly 60%% allowed", verdict)
	}
	limits["anthropic"] = UsageSubscription{Windows: map[string]UsageWindow{"seven_day": {UsedPercent: 1}}, StaleAfter: now.Add(-time.Second).Unix()}
	if verdict := budgetVerdict(limits, syntheticLimits, now); !strings.Contains(verdict, "stale") {
		t.Errorf("stale reading: %q", verdict)
	}
	delete(limits, "anthropic")
	if verdict := budgetVerdict(limits, syntheticLimits, now); !strings.Contains(verdict, "no reading for anthropic") {
		t.Errorf("missing reading: %q", verdict)
	}
}

func TestTheDefaultBudgetLimitsSettingParses(t *testing.T) {
	limits, err := ParseBudgetLimits(map[string]string{"codex.weekly": "60", "anthropic.seven_day": "40"})
	if err != nil || len(limits) != 2 || limits[0] != (BudgetLimit{Subscription: "anthropic", Window: "seven_day", MaxUsedPercent: 40}) {
		t.Errorf("limits %+v, %v", limits, err)
	}
	for _, bad := range []map[string]string{{"codex": "60"}, {"codex.weekly": "0"}, {"codex.weekly": "many"}} {
		if _, err := ParseBudgetLimits(bad); err == nil {
			t.Errorf("%v parsed", bad)
		}
	}
}
