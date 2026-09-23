package executors_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/executors"
	"github.com/kayushkin/llm-bridge-server/internal/operations"
	"github.com/kayushkin/llm-bridge-server/internal/operationstore"
	"github.com/kayushkin/llm-bridge/msg"
)

// fakeOneShot answers every call the same way and counts them.
type fakeOneShot struct {
	instanceID   string
	defaultModel string
	response     msg.OneShotResponse
	err          error
	calls        []msg.OneShotRequest
}

func (f *fakeOneShot) CompletionTarget() (string, string) { return f.instanceID, f.defaultModel }
func (f *fakeOneShot) RunOneShot(_ context.Context, instanceID string, request msg.OneShotRequest) (msg.OneShotResponse, error) {
	if instanceID != f.instanceID {
		return msg.OneShotResponse{}, errors.New("wrong instance " + instanceID)
	}
	f.calls = append(f.calls, request)
	return f.response, f.err
}

// Prices in dollars per million tokens: haiku is known, mystery is not.
func listPrice(model string) (float64, float64, bool) {
	if model == "claude-haiku-4-5" {
		return 1, 5, true
	}
	return 0, 0, false
}

type harness struct {
	store       *operationstore.Store
	coordinator *operations.Coordinator
	caller      *fakeOneShot
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := operationstore.Open(filepath.Join(t.TempDir(), "operations.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	caller := &fakeOneShot{instanceID: "inst-cc-local", defaultModel: "claude-haiku-4-5",
		response: msg.OneShotResponse{Text: "hello", Model: "claude-haiku-4-5", StopReason: "end_turn",
			Usage: msg.TokenUsage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 1000}}}
	coordinator, err := operations.NewCoordinator(store, operations.Config{WorkerCount: 1, LeaseDuration: 3 * time.Second,
		LeaseOwner: "test", ModelListPrice: listPrice}, executors.LLMCompletion{Caller: caller})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{store: store, coordinator: coordinator, caller: caller}
}

func (h *harness) submit(t *testing.T, key string, input msg.LLMCompletionInput, maximumCostUSD float64) msg.OperationReceipt {
	t.Helper()
	encoded, _ := json.Marshal(input)
	receipt, _, err := h.coordinator.Submit(msg.OperationIntent{Type: msg.OperationTypeLLMCompletion, OrganizationID: "principal_000006",
		PrincipalID: "principal_000004", IdempotencyKey: key, Input: encoded, MaximumCostUSD: maximumCostUSD})
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func (h *harness) run(t *testing.T) {
	t.Helper()
	for {
		ran, err := h.coordinator.RunNext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			return
		}
	}
}

func (h *harness) receipt(t *testing.T, id string) msg.OperationReceipt {
	t.Helper()
	operation, err := h.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Receipt
}

// 2000 input-side tokens at $1/M plus 200 output at $5/M.
const oneCallUSD = 0.002 + 0.001

func TestACompletionIsPricedAtListPriceAndCountedAgainstTheMonth(t *testing.T) {
	h := newHarness(t)
	receipt := h.submit(t, "k1", msg.LLMCompletionInput{Prompt: "say hello"}, 0)
	h.run(t)
	finished := h.receipt(t, receipt.ID)
	if finished.State != msg.OperationStateSucceeded || finished.Executor != "harness-oneshot" {
		t.Fatalf("finished %+v error %+v", finished, finished.Error)
	}
	var result msg.LLMCompletionResult
	json.Unmarshal(finished.Result, &result)
	if result.Text != "hello" {
		t.Fatalf("result %+v", result)
	}
	usage := finished.Usage
	if usage == nil || usage.Calls != 1 || usage.CostBasis != msg.OperationCostBasisListPrice || math.Abs(usage.Cost.TotalUSD-oneCallUSD) > 1e-12 ||
		usage.Tokens.InputTokens != 1000 || usage.Tokens.CacheReadTokens != 1000 {
		t.Fatalf("usage %+v", usage)
	}
	if h.caller.calls[0].Model != "claude-haiku-4-5" {
		t.Fatalf("asked for model %q; the configured default should fill an empty one", h.caller.calls[0].Model)
	}
	spent, err := h.store.OrganizationSpentUSD("principal_000006", operationstore.MonthStart(time.Now()))
	if err != nil || math.Abs(spent-oneCallUSD) > 1e-12 {
		t.Fatalf("organization spent %v (%v)", spent, err)
	}
}

func TestAnExhaustedOrganizationBudgetRefusesNewIntentsButStillAnswersRepeats(t *testing.T) {
	h := newHarness(t)
	if err := h.store.SetOrganizationBudget("principal_000006", 0.004, "principal_000003", time.Now()); err != nil {
		t.Fatal(err)
	}
	first := h.submit(t, "k1", msg.LLMCompletionInput{Prompt: "one"}, 0)
	second := h.submit(t, "k2", msg.LLMCompletionInput{Prompt: "two"}, 0)
	h.run(t)
	// $0.003 spent of $0.004: the second call was allowed, and overshot.
	if h.receipt(t, first.ID).State != msg.OperationStateSucceeded || h.receipt(t, second.ID).State != msg.OperationStateSucceeded {
		t.Fatal("both calls should run: the allowance was positive before each")
	}
	encoded, _ := json.Marshal(msg.LLMCompletionInput{Prompt: "three"})
	_, _, err := h.coordinator.Submit(msg.OperationIntent{Type: msg.OperationTypeLLMCompletion, OrganizationID: "principal_000006",
		PrincipalID: "principal_000004", IdempotencyKey: "k3", Input: encoded})
	var budgetError *operations.BudgetError
	if !errors.As(err, &budgetError) || budgetError.Code != "organization_budget_exhausted" {
		t.Fatalf("third intent: %v", err)
	}
	repeat := h.submit(t, "k1", msg.LLMCompletionInput{Prompt: "one"}, 0)
	if repeat.ID != first.ID {
		t.Fatalf("a repeat after the budget ran out got %s, want %s", repeat.ID, first.ID)
	}
}

func TestABudgetRunningOutWhileQueuedStopsTheCallBeforeItIsMade(t *testing.T) {
	h := newHarness(t)
	receipt := h.submit(t, "k1", msg.LLMCompletionInput{Prompt: "one"}, 0)
	if err := h.store.SetOrganizationBudget("principal_000006", 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	finished := h.receipt(t, receipt.ID)
	if finished.State != msg.OperationStateFailed || finished.Error.Code != "budget_exhausted" || len(h.caller.calls) != 0 {
		t.Fatalf("state %s error %+v calls %d", finished.State, finished.Error, len(h.caller.calls))
	}
}

func TestAPerOperationCapIsEnforcedAndAnUnpricedModelIsRefusedUnderIt(t *testing.T) {
	h := newHarness(t)
	tooSmall := h.submit(t, "cap", msg.LLMCompletionInput{Prompt: "one"}, 0.001)
	unpriced := h.submit(t, "mystery", msg.LLMCompletionInput{Prompt: "one", Model: "mystery-model"}, 1)
	unlimitedUnpriced := h.submit(t, "free", msg.LLMCompletionInput{Prompt: "one", Model: "mystery-model"}, 0)
	h.run(t)
	// The capped one ran once (the allowance was positive) and spent past its cap.
	if state := h.receipt(t, tooSmall.ID).State; state != msg.OperationStateSucceeded {
		t.Fatalf("capped: %s", state)
	}
	if finished := h.receipt(t, unpriced.ID); finished.State != msg.OperationStateFailed || finished.Error.Code != "model_price_unknown" {
		t.Fatalf("unpriced under a cap: %s %+v", finished.State, finished.Error)
	}
	if finished := h.receipt(t, unlimitedUnpriced.ID); finished.State != msg.OperationStateSucceeded {
		t.Fatalf("unpriced with no budget: %s %+v", finished.State, finished.Error)
	}
	if len(h.caller.calls) != 2 {
		t.Fatalf("%d calls, want 2: the unpriced call under a cap must not be made", len(h.caller.calls))
	}
}

func TestAFailedCallIsRetriedThenFails(t *testing.T) {
	h := newHarness(t)
	h.caller.err = errors.New("instance inst-cc-local answered the one-shot call with 502: overloaded")
	receipt := h.submit(t, "k1", msg.LLMCompletionInput{Prompt: "one"}, 0)
	h.run(t)
	finished := h.receipt(t, receipt.ID)
	if finished.State != msg.OperationStateFailed || finished.Error.Code != "model_call_failed" || !finished.Error.Retryable || len(h.caller.calls) != 2 {
		t.Fatalf("state %s error %+v calls %d", finished.State, finished.Error, len(h.caller.calls))
	}
}

func TestCompletionInputIsChecked(t *testing.T) {
	h := newHarness(t)
	for name, raw := range map[string]string{
		"no prompt":        `{"prompt":"  "}`,
		"unknown field":    `{"prompt":"x","temperature":1}`,
		"schema not JSON":  `{"prompt":"x","schema":"yes"}`,
		"negative maximum": `{"prompt":"x","max_tokens":-1}`,
	} {
		_, _, err := h.coordinator.Submit(msg.OperationIntent{Type: msg.OperationTypeLLMCompletion, OrganizationID: "o",
			IdempotencyKey: name, Input: json.RawMessage(raw)})
		var intentError *operations.IntentError
		if !errors.As(err, &intentError) || intentError.Code != "invalid_input" {
			t.Errorf("%s: %v", name, err)
		}
	}
}
