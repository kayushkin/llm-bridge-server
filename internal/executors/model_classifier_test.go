package executors_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

var boardTaxonomy = msg.ClassificationTaxonomy{Name: "logistics-support", Domain: "support mail to a logistics company", Axes: []msg.ClassificationAxis{
	{Name: "category", Required: true, Values: []msg.ClassificationValue{{Name: "delivery"}, {Name: "invoice"}}},
	{Name: "urgency", Values: []msg.ClassificationValue{{Name: "today"}}},
	{Name: "topics", AllowMultiple: true, Values: []msg.ClassificationValue{{Name: "pallet"}, {Name: "customs"}}},
}}

type classifierHarness struct {
	store       *operationstore.Store
	coordinator *operations.Coordinator
}

func newClassifierHarness(t *testing.T, caller executors.OneShotCaller, taxonomies executors.TaxonomyReader) *classifierHarness {
	t.Helper()
	store, err := operationstore.Open(filepath.Join(t.TempDir(), "operations.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	coordinator, err := operations.NewCoordinator(store, operations.Config{WorkerCount: 1, LeaseDuration: 3 * time.Second, LeaseOwner: "test",
		ModelListPrice: listPrice}, executors.ModelClassifier{Caller: caller, Taxonomies: taxonomies})
	if err != nil {
		t.Fatal(err)
	}
	return &classifierHarness{store: store, coordinator: coordinator}
}

func (h *classifierHarness) classify(t *testing.T, principalID string, input msg.ClassificationRunInput) msg.OperationReceipt {
	t.Helper()
	encoded, _ := json.Marshal(input)
	receipt, _, err := h.coordinator.Submit(msg.OperationIntent{Type: msg.OperationTypeClassificationRun, OrganizationID: "principal_000006",
		PrincipalID: principalID, IdempotencyKey: fmt.Sprint(time.Now().UnixNano()), Input: encoded})
	if err != nil {
		t.Fatal(err)
	}
	for {
		ran, err := h.coordinator.RunNext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			break
		}
	}
	operation, err := h.store.Get(receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Receipt
}

func decodeClassification(t *testing.T, receipt msg.OperationReceipt) msg.ClassificationRunResult {
	t.Helper()
	var result msg.ClassificationRunResult
	if err := json.Unmarshal(receipt.Result, &result); err != nil {
		t.Fatalf("result of %s (%s, %+v): %v", receipt.ID, receipt.State, receipt.Error, err)
	}
	return result
}

func TestABoardsTaxonomyIsReadAsTheOperationsPrincipalAndRecorded(t *testing.T) {
	caller := &executorstest.FakeOneShot{InstanceID: "inst", DefaultModel: "claude-haiku-4-5", Usage: msg.TokenUsage{InputTokens: 100, OutputTokens: 10}}
	taxonomies := executorstest.FakeTaxonomies{
		Boards:         map[string]*msg.ClassificationTaxonomy{"board-support": &boardTaxonomy, "board-empty": nil},
		ViewersByBoard: map[string][]string{"board-support": {"principal_000004"}},
	}
	h := newClassifierHarness(t, caller, taxonomies)
	items := []msg.ClassificationItem{{ID: "m1", Text: "Pallet stuck in customs, need delivery today"}}

	receipt := h.classify(t, "principal_000004", msg.ClassificationRunInput{TaxonomyBoardID: "board-support", Items: items})
	result := decodeClassification(t, receipt)
	values := result.Items[0].Values
	if receipt.State != msg.OperationStateSucceeded || result.Taxonomy.Name != "logistics-support" ||
		result.TaxonomySource == nil || result.TaxonomySource.EntityID != "board-support" || result.TaxonomySource.Version == "" ||
		strings.Join(values["category"], ",") != "delivery" || strings.Join(values["urgency"], ",") != "today" {
		t.Fatalf("receipt %s %+v result %+v", receipt.State, receipt.Error, result)
	}
	if !strings.Contains(caller.Calls()[0].SystemPrompt, "support mail to a logistics company") {
		t.Fatalf("the taxonomy's domain did not reach the model:\n%s", caller.Calls()[0].SystemPrompt)
	}
	var sawSource bool
	for _, evidence := range receipt.Evidence {
		sawSource = sawSource || evidence.Kind == "taxonomy_source"
	}
	if !sawSource || receipt.Usage == nil || receipt.Usage.Calls != 1 {
		t.Fatalf("evidence %+v usage %+v", receipt.Evidence, receipt.Usage)
	}

	for _, check := range []struct{ principal, board, code string }{
		{"principal_000099", "board-support", "taxonomy_board_not_found"},
		{"principal_000004", "board-missing", "taxonomy_board_not_found"},
		{"principal_000004", "board-empty", "board_has_no_taxonomy"},
	} {
		refused := h.classify(t, check.principal, msg.ClassificationRunInput{TaxonomyBoardID: check.board, Items: items})
		if refused.State != msg.OperationStateFailed || refused.Error.Code != check.code {
			t.Errorf("%s on %s: %s %+v, want failed %s", check.principal, check.board, refused.State, refused.Error, check.code)
		}
	}
}

func TestItemsGoToTheModelInBatches(t *testing.T) {
	caller := &executorstest.FakeOneShot{InstanceID: "inst", DefaultModel: "m"}
	h := newClassifierHarness(t, caller, executorstest.FakeTaxonomies{})
	items := make([]msg.ClassificationItem, 45)
	for index := range items {
		items[index] = msg.ClassificationItem{ID: fmt.Sprintf("m%02d", index), Text: "an invoice"}
	}
	receipt := h.classify(t, "", msg.ClassificationRunInput{Taxonomy: &boardTaxonomy, Items: items})
	result := decodeClassification(t, receipt)
	if len(caller.Calls()) != 3 || len(result.Items) != 45 || receipt.Progress.Completed != 45 || result.Items[44].ID != "m44" {
		t.Fatalf("calls %d items %d progress %+v", len(caller.Calls()), len(result.Items), receipt.Progress)
	}
}

// scriptedAnswer answers every call with the same parsed JSON.
type scriptedAnswer struct{ parsed string }

func (s scriptedAnswer) CompletionTarget(string) (executors.CompletionTarget, error) {
	return executors.CompletionTarget{RequestedModel: "m", ModelID: "m", Provider: "test", InstanceID: "inst"}, nil
}
func (s scriptedAnswer) RunOneShot(context.Context, string, msg.OneShotRequest) (msg.OneShotResponse, error) {
	return msg.OneShotResponse{Parsed: json.RawMessage(s.parsed), Model: "m"}, nil
}

func TestAnswersOutsideTheTaxonomyAreDroppedAndMarkedForReview(t *testing.T) {
	h := newClassifierHarness(t, scriptedAnswer{`{"items":[
		{"id":"m1","values":{"category":["refund"],"urgency":["today"],"topics":["pallet","pallet","customs"]},"confidence":1.4,"rationale":"x"},
		{"id":"m2","values":{"category":["delivery","invoice"],"urgency":[],"topics":[]},"confidence":0.5,"rationale":"y"}]}`}, executorstest.FakeTaxonomies{})
	receipt := h.classify(t, "", msg.ClassificationRunInput{Taxonomy: &boardTaxonomy, Items: []msg.ClassificationItem{
		{ID: "m1", Text: "a"}, {ID: "m2", Text: "b"}, {ID: "m3", Text: "c"}}})
	result := decodeClassification(t, receipt)
	first, second, third := result.Items[0], result.Items[1], result.Items[2]
	kinds := func(item msg.ClassificationItemResult) string {
		var names []string
		for _, evidence := range item.Evidence {
			names = append(names, evidence.Kind)
		}
		return strings.Join(names, ",")
	}
	if !first.NeedsReview || len(first.Values["category"]) != 0 || strings.Join(first.Values["topics"], ",") != "customs,pallet" ||
		first.Confidence != 1 || kinds(first) != "rejected_value,required_axis_empty" {
		t.Fatalf("first %+v evidence %s", first, kinds(first))
	}
	if !second.NeedsReview || kinds(second) != "too_many_values" {
		t.Fatalf("second %+v evidence %s", second, kinds(second))
	}
	if !third.NeedsReview || kinds(third) != "no_answer" || third.Values["category"] == nil {
		t.Fatalf("third %+v evidence %s", third, kinds(third))
	}
}

func TestAnAnswerNamingAnItemNotAskedAboutIsNotTrusted(t *testing.T) {
	h := newClassifierHarness(t, scriptedAnswer{`{"items":[{"id":"stranger","values":{},"confidence":1,"rationale":""}]}`}, executorstest.FakeTaxonomies{})
	receipt := h.classify(t, "", msg.ClassificationRunInput{Taxonomy: &boardTaxonomy, Items: []msg.ClassificationItem{{ID: "m1", Text: "a"}}})
	if receipt.State != msg.OperationStateFailed || receipt.Error.Code != "schema_not_followed" || !receipt.Error.Retryable || receipt.Attempt != 2 {
		t.Fatalf("state %s error %+v attempt %d", receipt.State, receipt.Error, receipt.Attempt)
	}
}

func TestClassificationInputIsChecked(t *testing.T) {
	h := newClassifierHarness(t, scriptedAnswer{}, executorstest.FakeTaxonomies{})
	taxonomy := `{"name":"t","axes":[{"name":"a","values":[{"name":"b"}]}]}`
	for name, raw := range map[string]string{
		"both":             `{"taxonomy":` + taxonomy + `,"taxonomy_board_id":"b","items":[{"id":"1","text":"x"}]}`,
		"neither":          `{"items":[{"id":"1","text":"x"}]}`,
		"invalid taxonomy": `{"taxonomy":{"name":"t","axes":[]},"items":[{"id":"1","text":"x"}]}`,
		"no items":         `{"taxonomy":` + taxonomy + `,"items":[]}`,
		"repeated id":      `{"taxonomy":` + taxonomy + `,"items":[{"id":"1","text":"x"},{"id":"1","text":"y"}]}`,
		"empty text":       `{"taxonomy":` + taxonomy + `,"items":[{"id":"1","text":" "}]}`,
	} {
		_, _, err := h.coordinator.Submit(msg.OperationIntent{Type: msg.OperationTypeClassificationRun, OrganizationID: "o", IdempotencyKey: name, Input: json.RawMessage(raw)})
		var intentError *operations.IntentError
		if !errors.As(err, &intentError) || intentError.Code != "invalid_input" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestClassificationStopsBeforeAModelCallWhenTheBudgetIsSpent(t *testing.T) {
	caller := &executorstest.FakeOneShot{InstanceID: "inst", DefaultModel: "claude-haiku-4-5"}
	h := newClassifierHarness(t, caller, executorstest.FakeTaxonomies{})
	if err := h.store.SetOrganizationBudget("principal_000006", 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Submitting is refused outright once the month is spent.
	encoded, _ := json.Marshal(msg.ClassificationRunInput{Taxonomy: &boardTaxonomy, Items: []msg.ClassificationItem{{ID: "m1", Text: "a"}}})
	_, _, err := h.coordinator.Submit(msg.OperationIntent{Type: msg.OperationTypeClassificationRun, OrganizationID: "principal_000006", IdempotencyKey: "k", Input: encoded})
	var budgetError *operations.BudgetError
	if !errors.As(err, &budgetError) || len(caller.Calls()) != 0 {
		t.Fatalf("submit under a spent budget: %v, %d calls", err, len(caller.Calls()))
	}
}
