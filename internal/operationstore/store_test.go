package operationstore_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/operationstore"
	"github.com/kayushkin/llm-bridge-server/internal/operationstore/operationstoretest"
	"github.com/kayushkin/llm-bridge/msg"
)

func testIntent(key string) msg.OperationIntent {
	return msg.OperationIntent{Type: "llm.completion", OrganizationID: "principal_000006", PrincipalID: "principal_000004",
		IdempotencyKey: key, Input: []byte(`{"prompt":"hello"}`)}
}

var testNow = time.Date(2026, 11, 1, 12, 0, 0, 123456789, time.UTC)

// checkEventsMatchRevisions holds the store's one rule: every receipt change
// wrote one event, numbered 1, 2, 3… up to the receipt's revision, each
// carrying the receipt as it was at that revision.
func checkEventsMatchRevisions(t *testing.T, store *operationstore.Store, operationID string) {
	t.Helper()
	operation, err := store.Get(operationID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.EventsAfter(operationID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(events)) != operation.Receipt.Revision {
		t.Fatalf("%s is at revision %d with %d events", operationID, operation.Receipt.Revision, len(events))
	}
	for index, event := range events {
		if event.Sequence != int64(index+1) || event.Receipt.Revision != event.Sequence {
			t.Fatalf("event %d of %s has sequence %d and receipt revision %d", index, operationID, event.Sequence, event.Receipt.Revision)
		}
	}
}

func TestReceiptChangesAndEventsStayInStepAndKeepNanoseconds(t *testing.T) {
	store := operationstoretest.NewStore(t)
	receipt, created, err := store.Create(testIntent("k1"), "fingerprint", testNow)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	operation, found, err := store.Claim("owner", time.Minute, testNow.Add(time.Nanosecond))
	if err != nil || !found || operation.Receipt.ID != receipt.ID || operation.LeaseToken != 1 {
		t.Fatalf("claim: %+v %v %v", operation, found, err)
	}
	if _, err := store.Update(receipt.ID, operation.LeaseToken, testNow.Add(2*time.Nanosecond), func(stored *operationstore.Operation) (msg.OperationEventKind, error) {
		stored.Receipt.State = msg.OperationStateSucceeded
		return msg.OperationEventSucceeded, nil
	}); err != nil {
		t.Fatal(err)
	}
	checkEventsMatchRevisions(t, store, receipt.ID)
	later, err := store.EventsAfter(receipt.ID, 2)
	if err != nil || len(later) != 1 || later[0].Kind != msg.OperationEventSucceeded {
		t.Fatalf("events after 2: %+v %v", later, err)
	}
	stored, err := store.Get(receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Receipt.CreatedAt.Equal(testNow) || stored.LeaseOwner != "" || stored.LeaseToken != 2 {
		t.Fatalf("stored: created %s, lease %q %d", stored.Receipt.CreatedAt, stored.LeaseOwner, stored.LeaseToken)
	}
	listed, err := store.List(operationstore.Filter{PrincipalID: "principal_000004", CreatedAfter: testNow.Add(-time.Nanosecond), Limit: 10})
	if err != nil || len(listed) != 1 {
		t.Fatalf("list: %d %v", len(listed), err)
	}
	if none, err := store.List(operationstore.Filter{CreatedAfter: testNow, Limit: 10}); err != nil || len(none) != 0 {
		t.Fatalf("list after its own nanosecond: %d %v", len(none), err)
	}
	if _, err := store.Update(receipt.ID, stored.LeaseToken, testNow, func(*operationstore.Operation) (msg.OperationEventKind, error) {
		return msg.OperationEventProgress, nil
	}); !errors.Is(err, operationstore.ErrTerminal) {
		t.Fatalf("update of a finished operation: %v", err)
	}
	if _, err := store.Get("operation_unknown"); !errors.Is(err, operationstore.ErrNotFound) {
		t.Fatalf("get unknown: %v", err)
	}
}

func TestAWorkerThatLostItsLeaseIsRefused(t *testing.T) {
	store := operationstoretest.NewStore(t)
	receipt, _, err := store.Create(testIntent("k1"), "fingerprint", testNow)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := store.Claim("owner-a", time.Minute, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// The coordinator requeues it (as reconciliation would), which retires the
	// first worker's token, and a second worker claims it.
	if _, err := store.Update(receipt.ID, operationstore.AnyLeaseToken, testNow, func(stored *operationstore.Operation) (msg.OperationEventKind, error) {
		stored.Receipt.State = msg.OperationStateQueued
		return msg.OperationEventQueued, nil
	}); err != nil {
		t.Fatal(err)
	}
	second, found, err := store.Claim("owner-b", time.Minute, testNow)
	if err != nil || !found || second.LeaseToken == first.LeaseToken {
		t.Fatalf("second claim: %+v %v %v", second, found, err)
	}
	if err := store.RenewLease(receipt.ID, first.LeaseToken, testNow.Add(time.Hour)); !errors.Is(err, operationstore.ErrLeaseLost) {
		t.Fatalf("renew with the old token: %v", err)
	}
	if _, err := store.Update(receipt.ID, first.LeaseToken, testNow, func(*operationstore.Operation) (msg.OperationEventKind, error) {
		return msg.OperationEventProgress, nil
	}); !errors.Is(err, operationstore.ErrLeaseLost) {
		t.Fatalf("update with the old token: %v", err)
	}
	if err := store.RenewLease(receipt.ID, second.LeaseToken, testNow.Add(time.Hour)); err != nil {
		t.Fatalf("renew with the current token: %v", err)
	}
	abandoned, err := store.RunningNotHeldBy("owner-a", testNow)
	if err != nil || len(abandoned) != 1 || abandoned[0].LeaseOwner != "owner-b" {
		t.Fatalf("running not held by owner-a: %+v %v", abandoned, err)
	}
	if held, err := store.RunningNotHeldBy("owner-b", testNow); err != nil || len(held) != 0 {
		t.Fatalf("running not held by owner-b: %+v %v", held, err)
	}
	checkEventsMatchRevisions(t, store, receipt.ID)
}

func TestWorkersClaimingTogetherNeverTakeOneOperationTwice(t *testing.T) {
	store := operationstoretest.NewStore(t)
	const operations = 40
	for index := 0; index < operations; index++ {
		if _, _, err := store.Create(testIntent(fmt.Sprintf("k%d", index)), "fingerprint", testNow.Add(time.Duration(index))); err != nil {
			t.Fatal(err)
		}
	}
	var mutex sync.Mutex
	claimedBy := map[string]int{}
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for {
				operation, found, err := store.Claim(fmt.Sprintf("worker-%d", worker), time.Minute, testNow)
				if err != nil {
					t.Error(err)
					return
				}
				if !found {
					return
				}
				mutex.Lock()
				claimedBy[operation.Receipt.ID]++
				mutex.Unlock()
			}
		}(worker)
	}
	workers.Wait()
	if len(claimedBy) != operations {
		t.Fatalf("%d of %d operations claimed", len(claimedBy), operations)
	}
	for id, claims := range claimedBy {
		if claims != 1 {
			t.Fatalf("%s claimed %d times", id, claims)
		}
		checkEventsMatchRevisions(t, store, id)
	}
}

func TestChangesMadeTogetherTakeTurnsOnTheRevision(t *testing.T) {
	store := operationstoretest.NewStore(t)
	receipt, _, err := store.Create(testIntent("k1"), "fingerprint", testNow)
	if err != nil {
		t.Fatal(err)
	}
	operation, _, err := store.Claim("owner", time.Minute, testNow)
	if err != nil {
		t.Fatal(err)
	}
	const changes = 30
	var workers sync.WaitGroup
	for index := 0; index < changes; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := store.RecordModelCall(receipt.ID, operation.LeaseToken, testNow, operationstore.ModelCall{
				Model: "test-model", Tokens: msg.TokenUsage{InputTokens: 10, OutputTokens: 1}, CostUSD: 0.5, CostKnown: true,
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	stored, err := store.Get(receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Receipt.Revision != 2+changes || stored.Receipt.Usage.Calls != changes || stored.Receipt.Usage.Tokens.InputTokens != 10*changes {
		t.Fatalf("revision %d, usage %+v", stored.Receipt.Revision, stored.Receipt.Usage)
	}
	checkEventsMatchRevisions(t, store, receipt.ID)
	spent, err := store.TreeSpentUSD(receipt.ID)
	if err != nil || spent != 0.5*changes {
		t.Fatalf("tree spend %v %v", spent, err)
	}
	organizationSpent, err := store.OrganizationSpentUSD("principal_000006", operationstore.MonthStart(testNow))
	if err != nil || organizationSpent != 0.5*changes {
		t.Fatalf("organization spend %v %v", organizationSpent, err)
	}
}

func TestChildrenChangingTogetherCountOnTheirParent(t *testing.T) {
	store := operationstoretest.NewStore(t)
	parent, _, err := store.Create(testIntent("parent"), "fingerprint", testNow)
	if err != nil {
		t.Fatal(err)
	}
	const children = 20
	var workers sync.WaitGroup
	for index := 0; index < children; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			intent := testIntent(fmt.Sprintf("child-%d", index))
			intent.ParentOperationID = parent.ID
			child, _, err := store.Create(intent, "fingerprint", testNow.Add(time.Duration(index+1)))
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := store.Update(child.ID, operationstore.AnyLeaseToken, testNow, func(stored *operationstore.Operation) (msg.OperationEventKind, error) {
				stored.Receipt.State = msg.OperationStateCancelled
				return msg.OperationEventCancelled, nil
			}); err != nil {
				t.Error(err)
			}
		}(index)
	}
	workers.Wait()
	stored, err := store.Get(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	summary := stored.Receipt.Children
	if summary == nil || summary.Total != children || summary.ByState[msg.OperationStateCancelled] != children || len(stored.Receipt.ChildOperationIDs) != children {
		t.Fatalf("parent's children: %+v", summary)
	}
	if stored.Receipt.Revision != 1+2*children {
		t.Fatalf("parent at revision %d, want %d", stored.Receipt.Revision, 1+2*children)
	}
	checkEventsMatchRevisions(t, store, parent.ID)
	root, err := store.RootOf(stored.Receipt.ChildOperationIDs[0])
	if err != nil || root.Receipt.ID != parent.ID {
		t.Fatalf("root of a child: %v %v", root.Receipt.ID, err)
	}
}

func TestCreatesWithOneKeyTogetherMakeOneOperation(t *testing.T) {
	store := operationstoretest.NewStore(t)
	var workers sync.WaitGroup
	ids := make(chan string, 10)
	createdCount := make(chan bool, 10)
	for index := 0; index < 10; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			receipt, created, err := store.Create(testIntent("same"), "fingerprint", testNow)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- receipt.ID
			createdCount <- created
		}()
	}
	workers.Wait()
	close(ids)
	close(createdCount)
	distinct := map[string]bool{}
	for id := range ids {
		distinct[id] = true
	}
	creations := 0
	for created := range createdCount {
		if created {
			creations++
		}
	}
	if len(distinct) != 1 || creations != 1 {
		t.Fatalf("%d distinct operations, %d creations", len(distinct), creations)
	}
	if _, _, err := store.Create(testIntent("same"), "another fingerprint", testNow); !errors.Is(err, operationstore.ErrIdempotencyKeyReused) {
		t.Fatalf("reused key: %v", err)
	}
}

func TestOrganizationBudgets(t *testing.T) {
	store := operationstoretest.NewStore(t)
	if err := store.SetOrganizationBudget("principal_000006", 100, "principal_000001", testNow); err != nil {
		t.Fatal(err)
	}
	if err := store.SetOrganizationBudget("principal_000006", 50, "principal_000002", testNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	budget, found, err := store.OrganizationBudget("principal_000006", testNow)
	if err != nil || !found || budget.MonthlyLimitUSD != 50 || budget.UpdatedBy != "principal_000002" || budget.SpentUSD != 0 {
		t.Fatalf("budget: %+v %v %v", budget, found, err)
	}
	budgets, err := store.ListOrganizationBudgets(testNow)
	if err != nil || len(budgets) != 1 {
		t.Fatalf("budgets: %+v %v", budgets, err)
	}
	if err := store.DeleteOrganizationBudget("principal_000006"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteOrganizationBudget("principal_000006"); !errors.Is(err, operationstore.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}
