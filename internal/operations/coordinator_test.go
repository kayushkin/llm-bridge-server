package operations_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/executors"
	"github.com/kayushkin/llm-bridge-server/internal/executors/executorstest"
	"github.com/kayushkin/llm-bridge-server/internal/operations"
	"github.com/kayushkin/llm-bridge-server/internal/operationstore"
	"github.com/kayushkin/llm-bridge/msg"
)

// scriptedExecutor runs whatever its test tells it to.
type scriptedExecutor struct {
	operationType   msg.OperationType
	maximumAttempts int
	execute         func(ctx context.Context, intent msg.OperationIntent, receipt operations.ReceiptWriter) operations.Result
	reconcile       func(msg.OperationReceipt) operations.ReconcileResult
}

func (e *scriptedExecutor) Describe() msg.OperationTypeDescription {
	attempts := e.maximumAttempts
	if attempts == 0 {
		attempts = 2
	}
	return msg.OperationTypeDescription{Type: e.operationType, Executor: "scripted", MaximumAttempts: attempts, TimeoutSeconds: 5}
}
func (e *scriptedExecutor) Validate(msg.OperationIntent) error { return nil }
func (e *scriptedExecutor) Execute(ctx context.Context, intent msg.OperationIntent, receipt operations.ReceiptWriter) operations.Result {
	return e.execute(ctx, intent, receipt)
}
func (e *scriptedExecutor) Reconcile(_ context.Context, receipt msg.OperationReceipt) operations.ReconcileResult {
	if e.reconcile == nil {
		return operations.ReconcileResult{Action: operations.ReconcileRunAgain}
	}
	return e.reconcile(receipt)
}
func (e *scriptedExecutor) Cancel(context.Context, msg.OperationReceipt) error { return nil }

const scriptedType msg.OperationType = "test.scripted"

func openStore(t *testing.T) (*operationstore.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "operations.db")
	store, err := operationstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path
}

func newCoordinator(t *testing.T, store *operationstore.Store, owner string, executorList ...operations.Executor) *operations.Coordinator {
	t.Helper()
	coordinator, err := operations.NewCoordinator(store, operations.Config{WorkerCount: 1, LeaseDuration: 3 * time.Second, LeaseOwner: owner}, executorList...)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

// supportTaxonomy is a small two-axis taxonomy the fake harness can answer:
// it gives an item the values whose names appear in its text.
var supportTaxonomy = msg.ClassificationTaxonomy{Name: "support", Domain: "support mail", Axes: []msg.ClassificationAxis{
	{Name: "category", Required: true, Values: []msg.ClassificationValue{{Name: "billing"}, {Name: "outage"}}},
	{Name: "urgency", Values: []msg.ClassificationValue{{Name: "urgent"}}},
}}

func modelClassifier() executors.ModelClassifier {
	return executors.ModelClassifier{
		Caller:     &executorstest.FakeOneShot{InstanceID: "inst-test", DefaultModel: "test-model"},
		Taxonomies: executorstest.FakeTaxonomies{},
	}
}

func classificationIntent(key string) msg.OperationIntent {
	input, _ := json.Marshal(msg.ClassificationRunInput{
		Taxonomy: &supportTaxonomy,
		Items:    []msg.ClassificationItem{{ID: "mail_1", Text: "Your BILLING statement is urgent"}, {ID: "mail_2", Text: "hello"}},
	})
	return msg.OperationIntent{Type: msg.OperationTypeClassificationRun, OrganizationID: "principal_000006",
		PrincipalID: "principal_000004", IdempotencyKey: key, Input: input}
}

func runUntilIdle(t *testing.T, coordinator *operations.Coordinator) {
	t.Helper()
	for {
		ran, err := coordinator.RunNext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			return
		}
	}
}

func get(t *testing.T, store *operationstore.Store, id string) msg.OperationReceipt {
	t.Helper()
	operation, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Receipt
}

func TestClassificationRunsToSuccessWithValuesPerAxis(t *testing.T) {
	store, _ := openStore(t)
	coordinator := newCoordinator(t, store, "owner-a", modelClassifier())
	receipt, created, err := coordinator.Submit(classificationIntent("k1"))
	if err != nil || !created || receipt.State != msg.OperationStateQueued || receipt.CorrelationID != receipt.ID {
		t.Fatalf("submit: created=%v err=%v receipt=%+v", created, err, receipt)
	}
	runUntilIdle(t, coordinator)
	finished := get(t, store, receipt.ID)
	if finished.State != msg.OperationStateSucceeded || finished.Executor != "model-classifier" || finished.CompletedAt == nil {
		t.Fatalf("finished: %+v", finished)
	}
	var result msg.ClassificationRunResult
	if err := json.Unmarshal(finished.Result, &result); err != nil {
		t.Fatal(err)
	}
	first, second := result.Items[0], result.Items[1]
	if len(result.Items) != 2 || result.Taxonomy.Name != "support" || result.TaxonomySource != nil ||
		strings.Join(first.Values["category"], ",") != "billing" || strings.Join(first.Values["urgency"], ",") != "urgent" || first.NeedsReview ||
		len(second.Values["category"]) != 0 || !second.NeedsReview {
		t.Fatalf("result: %+v", result)
	}
	if finished.Progress == nil || finished.Progress.Completed != 2 || finished.Progress.Total != 2 {
		t.Fatalf("progress: %+v", finished.Progress)
	}
	events, err := store.EventsAfter(receipt.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for index, event := range events {
		if event.Sequence != int64(index+1) || event.Receipt.Revision != event.Sequence {
			t.Fatalf("event %d has sequence %d and revision %d", index, event.Sequence, event.Receipt.Revision)
		}
	}
	if first, last := events[0].Kind, events[len(events)-1].Kind; first != msg.OperationEventAccepted || last != msg.OperationEventSucceeded {
		t.Fatalf("events run %s … %s", first, last)
	}
}

func TestSameKeySameIntentReturnsTheFirstReceiptAndDifferentIntentIsRefused(t *testing.T) {
	store, _ := openStore(t)
	coordinator := newCoordinator(t, store, "owner-a", modelClassifier())
	first, _, err := coordinator.Submit(classificationIntent("k1"))
	if err != nil {
		t.Fatal(err)
	}
	// Same intent, input re-encoded with different white space and key order.
	again := classificationIntent("k1")
	var reordered map[string]any
	json.Unmarshal(again.Input, &reordered)
	again.Input, _ = json.MarshalIndent(reordered, "", "   ")
	second, created, err := coordinator.Submit(again)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("repeat: created=%v err=%v id=%s want %s", created, err, second.ID, first.ID)
	}
	different := classificationIntent("k1")
	different.Input = json.RawMessage(`{"taxonomy":{"name":"x","axes":[{"name":"a","values":[{"name":"b"}]}]},"items":[{"id":"a","text":"b"}]}`)
	if _, _, err := coordinator.Submit(different); !errors.Is(err, operationstore.ErrIdempotencyKeyReused) {
		t.Fatalf("different intent under the same key: %v", err)
	}
	// Another principal may use the same key for its own operation.
	otherPrincipal := classificationIntent("k1")
	otherPrincipal.PrincipalID = "principal_000009"
	third, created, err := coordinator.Submit(otherPrincipal)
	if err != nil || !created || third.ID == first.ID {
		t.Fatalf("other principal: created=%v err=%v", created, err)
	}
}

func TestIntentRefusals(t *testing.T) {
	store, _ := openStore(t)
	coordinator := newCoordinator(t, store, "owner-a", modelClassifier())
	cases := map[string]func(*msg.OperationIntent){
		"unknown_operation_type":   func(i *msg.OperationIntent) { i.Type = "nope.run" },
		"organization_required":    func(i *msg.OperationIntent) { i.OrganizationID = "" },
		"idempotency_key_required": func(i *msg.OperationIntent) { i.IdempotencyKey = " " },
		"credential_in_input": func(i *msg.OperationIntent) {
			i.Input = json.RawMessage(`{"taxonomy_board_id":"b","items":[{"id":"1","text":"t"}],"options":{"mailApiKey":"x"}}`)
		},
		"invalid_input":       func(i *msg.OperationIntent) { i.Input = json.RawMessage(`{"taxonomy_board_id":"b","items":[]}`) },
		"parent_not_settable": func(i *msg.OperationIntent) { i.ParentOperationID = "operation_x" },
	}
	for code, spoil := range cases {
		intent := classificationIntent("k-" + code)
		spoil(&intent)
		_, _, err := coordinator.Submit(intent)
		var intentError *operations.IntentError
		if !errors.As(err, &intentError) || intentError.Code != code {
			t.Errorf("%s: got %v", code, err)
		}
	}
}

func TestInputKeysLikeMaxTokensAreNotCredentials(t *testing.T) {
	store, _ := openStore(t)
	coordinator := newCoordinator(t, store, "owner-a", &scriptedExecutor{operationType: scriptedType})
	intent := msg.OperationIntent{Type: scriptedType, OrganizationID: "o", IdempotencyKey: "k",
		Input: json.RawMessage(`{"max_tokens":10,"input_tokens":3,"keyword":"a","session_key":"b"}`)}
	if _, _, err := coordinator.Submit(intent); err != nil {
		t.Fatal(err)
	}
}

// A restart leaves an operation running under a lease nobody renews. The next
// process reconciles it, and a classification is safe to run again.
func TestRestartRequeuesAndFinishesAnAbandonedClassification(t *testing.T) {
	store, _ := openStore(t)
	first := newCoordinator(t, store, "process-before-restart", modelClassifier())
	receipt, _, err := first.Submit(classificationIntent("k1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Claim("process-before-restart", time.Minute, time.Now()); err != nil || !found {
		t.Fatalf("claim: %v %v", found, err)
	}
	second := newCoordinator(t, store, "process-after-restart", modelClassifier())
	second.ReconcileAbandoned(context.Background())
	if state := get(t, store, receipt.ID).State; state != msg.OperationStateQueued {
		t.Fatalf("after reconcile: %s", state)
	}
	runUntilIdle(t, second)
	finished := get(t, store, receipt.ID)
	if finished.State != msg.OperationStateSucceeded || finished.Attempt != 2 {
		t.Fatalf("finished: state %s attempt %d", finished.State, finished.Attempt)
	}
}

func TestAWorkerThatLostItsLeaseCannotWrite(t *testing.T) {
	store, _ := openStore(t)
	coordinator := newCoordinator(t, store, "owner-b", &scriptedExecutor{operationType: scriptedType})
	receipt, _, _ := coordinator.Submit(msg.OperationIntent{Type: scriptedType, OrganizationID: "o", IdempotencyKey: "k"})
	stale, _, _ := store.Claim("owner-a", time.Minute, time.Now())
	coordinator.ReconcileAbandoned(context.Background())
	_, err := store.Update(receipt.ID, stale.LeaseToken, time.Now(), func(operation *operationstore.Operation) (msg.OperationEventKind, error) {
		operation.Receipt.State = msg.OperationStateSucceeded
		return msg.OperationEventSucceeded, nil
	})
	if !errors.Is(err, operationstore.ErrLeaseLost) {
		t.Fatalf("stale write: %v", err)
	}
	if err := store.RenewLease(receipt.ID, stale.LeaseToken, time.Now().Add(time.Minute)); !errors.Is(err, operationstore.ErrLeaseLost) {
		t.Fatalf("stale renewal: %v", err)
	}
}

func TestRetryableFailureRetriesThenFails(t *testing.T) {
	store, _ := openStore(t)
	calls := 0
	coordinator := newCoordinator(t, store, "owner-a", &scriptedExecutor{operationType: scriptedType, maximumAttempts: 2,
		execute: func(context.Context, msg.OperationIntent, operations.ReceiptWriter) operations.Result {
			calls++
			return operations.Result{Error: &msg.OperationError{Code: "model_timeout", Message: "no answer in 60s", Retryable: true}}
		}})
	receipt, _, _ := coordinator.Submit(msg.OperationIntent{Type: scriptedType, OrganizationID: "o", IdempotencyKey: "k"})
	runUntilIdle(t, coordinator)
	finished := get(t, store, receipt.ID)
	if calls != 2 || finished.State != msg.OperationStateFailed || finished.Error.Code != "model_timeout" || !finished.Error.Retryable {
		t.Fatalf("calls %d, receipt %+v error %+v", calls, finished, finished.Error)
	}
}

// A timeout after an external request left must never read as success or as
// a plain failure: the effect may have happened.
func TestAnEffectInDoubtMakesTheOperationUnknown(t *testing.T) {
	for name, reported := range map[string]msg.OperationState{"failed": msg.OperationStateFailed, "succeeded": msg.OperationStateSucceeded, "retry": ""} {
		t.Run(name, func(t *testing.T) {
			store, _ := openStore(t)
			coordinator := newCoordinator(t, store, "owner-a", &scriptedExecutor{operationType: scriptedType,
				execute: func(_ context.Context, _ msg.OperationIntent, receipt operations.ReceiptWriter) operations.Result {
					number, err := receipt.RecordEffect(msg.OperationReference{EntityType: "card", EntityID: "card_1", Version: "4"}, "move")
					if err != nil || number != 1 {
						t.Fatalf("record effect: %d %v", number, err)
					}
					headers := receipt.EffectHeaders(number)
					if headers.Get(operations.IdempotencyKeyHeader) != receipt.OperationID()+":1" {
						t.Fatalf("headers %v", headers)
					}
					return operations.Result{State: reported, Error: &msg.OperationError{Code: "kanban_timeout", Message: "no answer", Retryable: true}}
				}})
			receipt, _, _ := coordinator.Submit(msg.OperationIntent{Type: scriptedType, OrganizationID: "o", IdempotencyKey: "k"})
			runUntilIdle(t, coordinator)
			finished := get(t, store, receipt.ID)
			if finished.State != msg.OperationStateUnknown || finished.Error.Retryable || finished.Attempt != 1 ||
				finished.Effects[0].Outcome != msg.OperationEffectOutcomeUnknown {
				t.Fatalf("receipt %+v error %+v", finished, finished.Error)
			}
		})
	}
}

func TestReconcileNeverRunsAgainPastAnEffectInDoubt(t *testing.T) {
	store, _ := openStore(t)
	executor := &scriptedExecutor{operationType: scriptedType}
	coordinator := newCoordinator(t, store, "owner-b", executor)
	receipt, _, _ := coordinator.Submit(msg.OperationIntent{Type: scriptedType, OrganizationID: "o", IdempotencyKey: "k"})
	claimed, _, _ := store.Claim("owner-a", time.Minute, time.Now())
	store.Update(receipt.ID, claimed.LeaseToken, time.Now(), func(operation *operationstore.Operation) (msg.OperationEventKind, error) {
		operation.Receipt.Effects = append(operation.Receipt.Effects, msg.OperationEffect{Action: "move", Outcome: msg.OperationEffectOutcomeUnknown})
		return msg.OperationEventProgress, nil
	})
	coordinator.ReconcileAbandoned(context.Background())
	if state := get(t, store, receipt.ID).State; state != msg.OperationStateUnknown {
		t.Fatalf("state %s", state)
	}
}

func TestCancel(t *testing.T) {
	store, _ := openStore(t)
	started := make(chan struct{})
	coordinator := newCoordinator(t, store, "owner-a", &scriptedExecutor{operationType: scriptedType,
		execute: func(ctx context.Context, _ msg.OperationIntent, _ operations.ReceiptWriter) operations.Result {
			close(started)
			<-ctx.Done()
			return operations.Result{Error: &msg.OperationError{Code: "interrupted", Message: ctx.Err().Error(), Retryable: true}}
		}})

	queued, _, _ := coordinator.Submit(msg.OperationIntent{Type: scriptedType, OrganizationID: "o", IdempotencyKey: "queued"})
	cancelled, err := coordinator.Cancel(context.Background(), queued.ID)
	if err != nil || cancelled.State != msg.OperationStateCancelled {
		t.Fatalf("cancel queued: %v %+v", err, cancelled)
	}
	if _, err := coordinator.Cancel(context.Background(), queued.ID); !errors.Is(err, operationstore.ErrTerminal) {
		t.Fatalf("cancel finished: %v", err)
	}

	running, _, _ := coordinator.Submit(msg.OperationIntent{Type: scriptedType, OrganizationID: "o", IdempotencyKey: "running"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		coordinator.RunNext(context.Background())
	}()
	<-started
	requested, err := coordinator.Cancel(context.Background(), running.ID)
	if err != nil || requested.State != msg.OperationStateRunning || requested.CancelRequestedAt == nil {
		t.Fatalf("cancel running: %v %+v", err, requested)
	}
	<-done
	if state := get(t, store, running.ID).State; state != msg.OperationStateCancelled {
		t.Fatalf("after cancel: %s", state)
	}
}

func TestChildrenAreCountedOnTheParent(t *testing.T) {
	store, _ := openStore(t)
	const childType msg.OperationType = "test.child"
	parentExecutor := &scriptedExecutor{operationType: scriptedType,
		execute: func(_ context.Context, _ msg.OperationIntent, receipt operations.ReceiptWriter) operations.Result {
			for _, key := range []string{"a", "b", "a"} {
				if _, err := receipt.StartChild(childType, key, nil, nil); err != nil {
					t.Fatalf("start child %s: %v", key, err)
				}
			}
			return operations.Result{State: msg.OperationStateSucceeded}
		}}
	childExecutor := &scriptedExecutor{operationType: childType,
		execute: func(context.Context, msg.OperationIntent, operations.ReceiptWriter) operations.Result {
			return operations.Result{State: msg.OperationStateSucceeded}
		}}
	coordinator := newCoordinator(t, store, "owner-a", parentExecutor, childExecutor)
	parent, _, _ := coordinator.Submit(msg.OperationIntent{Type: scriptedType, OrganizationID: "o", PrincipalID: "p", IdempotencyKey: "k", CorrelationID: "trace-1"})
	runUntilIdle(t, coordinator)
	finished := get(t, store, parent.ID)
	if finished.State != msg.OperationStateSucceeded || len(finished.ChildOperationIDs) != 2 ||
		finished.Children.Total != 2 || finished.Children.ByState[msg.OperationStateSucceeded] != 2 {
		t.Fatalf("parent: %+v children %+v", finished, finished.Children)
	}
	child := get(t, store, finished.ChildOperationIDs[0])
	if child.ParentOperationID != parent.ID || child.CorrelationID != "trace-1" || child.PrincipalID != "p" || child.OrganizationID != "o" {
		t.Fatalf("child: %+v", child)
	}
	children, err := store.List(operationstore.Filter{ParentOperationID: parent.ID, Limit: 10})
	if err != nil || len(children) != 2 {
		t.Fatalf("list children: %v %d", err, len(children))
	}
}

// A per-operation cap covers the whole tree: what a child spends comes out
// of its root's allowance.
func TestAChildsSpendingCountsAgainstItsRootsCap(t *testing.T) {
	store, _ := openStore(t)
	const childType msg.OperationType = "test.child"
	var allowanceSeenBySecondChild float64
	var limitedForSecondChild bool
	calls := 0
	childExecutor := &scriptedExecutor{operationType: childType,
		execute: func(_ context.Context, _ msg.OperationIntent, receipt operations.ReceiptWriter) operations.Result {
			calls++
			if calls == 2 {
				var err error
				allowanceSeenBySecondChild, limitedForSecondChild, err = receipt.SpendingAllowance()
				if err != nil {
					t.Fatal(err)
				}
			}
			// $1 per million input tokens: 1000 tokens is $0.001.
			if err := receipt.RecordModelCall("priced-model", msg.TokenUsage{InputTokens: 1000}); err != nil {
				t.Fatal(err)
			}
			return operations.Result{State: msg.OperationStateSucceeded}
		}}
	parentExecutor := &scriptedExecutor{operationType: scriptedType,
		execute: func(_ context.Context, _ msg.OperationIntent, receipt operations.ReceiptWriter) operations.Result {
			for _, key := range []string{"a", "b"} {
				if _, err := receipt.StartChild(childType, key, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			return operations.Result{State: msg.OperationStateSucceeded}
		}}
	coordinator, err := operations.NewCoordinator(store, operations.Config{WorkerCount: 1, LeaseDuration: 3 * time.Second, LeaseOwner: "o",
		ModelListPrice: func(model string) (float64, float64, bool) { return 1, 1, model == "priced-model" }}, parentExecutor, childExecutor)
	if err != nil {
		t.Fatal(err)
	}
	parent, _, err := coordinator.Submit(msg.OperationIntent{Type: scriptedType, OrganizationID: "o", IdempotencyKey: "k", MaximumCostUSD: 0.0025})
	if err != nil {
		t.Fatal(err)
	}
	runUntilIdle(t, coordinator)
	if !limitedForSecondChild || allowanceSeenBySecondChild < 0.0014 || allowanceSeenBySecondChild > 0.0016 {
		t.Fatalf("second child saw allowance %v limited %v, want $0.0015 left of the root's $0.0025", allowanceSeenBySecondChild, limitedForSecondChild)
	}
	spent, err := store.TreeSpentUSD(parent.ID)
	if err != nil || spent < 0.00199 || spent > 0.00201 {
		t.Fatalf("tree spent %v (%v)", spent, err)
	}
}
