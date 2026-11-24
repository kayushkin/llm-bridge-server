package evalstore

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every fixture here is synthetic. Cases in a real database hold real mail;
// none of it may reach the repository.

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

const (
	syntheticCardA = "aaaaaaaa-0000-0000-0000-000000000001"
	syntheticCardB = "bbbbbbbb-0000-0000-0000-000000000002"
)

// syntheticFilingPrompt is shaped like the classifier's prompt: cards, then
// one block per email.
func syntheticFilingPrompt(emailRefs ...string) string {
	var b strings.Builder
	b.WriteString("[TODAY IS 2026-01-01]\nCARDS:\n")
	fmt.Fprintf(&b, "- card_id=%s Renew the library card\n- card_id=%s Plan the picnic\n\nEMAILS:\n", syntheticCardA, syntheticCardB)
	for _, ref := range emailRefs {
		fmt.Fprintf(&b, "--- email_ref=%s\nFrom: someone@example.test\nSubject: synthetic\n\n", ref)
	}
	return b.String()
}

func syntheticFiling(emailRef, cardID string) string {
	return fmt.Sprintf(`{"email_ref":%q,"card_ids":[%q],"link_label":"synthetic"}`, emailRef, cardID)
}

func emailSuite(t *testing.T, store *Store) *Suite {
	t.Helper()
	suite, err := store.CreateSuite(&Suite{Name: "synthetic filing", TaskKind: "email-filing", JudgeInstructions: "Judge the filing.",
		JudgeFailureKinds: []string{"wrong_card", "no_answer", "other", "none"}, OutputSchema: json.RawMessage(`{"type":"object"}`),
		MaxTokens: 1000, JudgeMaxTokens: 2000, CheckerName: "email-filing", BaselineModelID: "model-original",
		WeeklyCallsSource: WeeklyCallsSource{Kind: WeeklyCallsSourceFixed, CallsPerWeek: 10, Note: "a synthetic figure"}})
	if err != nil {
		t.Fatal(err)
	}
	return suite
}

func addEmailCase(t *testing.T, store *Store, suite *Suite, ref string) *Case {
	t.Helper()
	duration := int64(4000)
	c, err := store.CreateCase(suite.ID, &Case{SourceKind: SourceKindLLMBridgeOperation, SourceRef: "operation_" + ref, SystemPrompt: "File mail.",
		UserPrompt: syntheticFilingPrompt("e-" + ref), OriginalModelID: "model-original",
		OriginalAnswer: json.RawMessage(`{"filings":[` + syntheticFiling("e-"+ref, syntheticCardA) + `]}`), OriginalUsageKnown: true,
		OriginalInputTokens: 1000, OriginalOutputTokens: 100, OriginalDurationMs: &duration})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fakeCatalog is model-store.
type fakeCatalog map[string]ModelPrice

func (f fakeCatalog) Models(context.Context) (map[string]ModelPrice, error) { return f, nil }

func syntheticCatalog() fakeCatalog {
	return fakeCatalog{
		"model-original": {Name: "Original", Provider: "anthropic", InputCostPerMillion: 3, OutputCostPerMillion: 15, Known: true},
		"model-small":    {Name: "Small", Provider: "openai", InputCostPerMillion: 0.5, OutputCostPerMillion: 2, Known: true},
		"model-large":    {Name: "Large", Provider: "anthropic", InputCostPerMillion: 5, OutputCostPerMillion: 25, Known: true},
		"judge-a":        {Name: "Judge A", Provider: "anthropic", InputCostPerMillion: 5, OutputCostPerMillion: 25, Known: true},
		"judge-b":        {Name: "Judge B", Provider: "openai", InputCostPerMillion: 2, OutputCostPerMillion: 10, Known: true},
	}
}

// fakeUsage is usage-store; percent can change between readings.
type fakeUsage struct {
	mutex    sync.Mutex
	readings []float64 // anthropic seven_day percent per reading; the last repeats
	reads    int
	stale    bool
}

func (f *fakeUsage) Limits(context.Context) (UsageLimits, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	percent := 1.0
	if len(f.readings) > 0 {
		percent = f.readings[min(f.reads, len(f.readings)-1)]
	}
	f.reads++
	staleAfter := time.Now().Add(time.Hour).Unix()
	if f.stale {
		staleAfter = time.Now().Add(-time.Hour).Unix()
	}
	return UsageLimits{
		"anthropic": {Windows: map[string]UsageWindow{"seven_day": {UsedPercent: percent}}, StaleAfter: staleAfter},
		"codex":     {Windows: map[string]UsageWindow{"weekly": {UsedPercent: 1}}, StaleAfter: staleAfter},
	}, nil
}

var syntheticLimits = []BudgetLimit{{Subscription: "codex", Window: "weekly", MaxUsedPercent: 60}, {Subscription: "anthropic", Window: "seven_day", MaxUsedPercent: 40}}

// fakeCaller is the bridge. Candidates file every email to card A; judges
// rank the labels in order and accept everything. It remembers every key it
// was sent and answers a repeated key with the first outcome, as the bridge
// does.
type fakeCaller struct {
	mutex    sync.Mutex
	calls    []CompletionRequest
	byKey    map[string]CompletionOutcome
	failures map[string]error // by model id: fail this model's answers
	refuse   bool             // refuse every call for budget before accepting it
	sequence int
}

func newFakeCaller() *fakeCaller {
	return &fakeCaller{byKey: map[string]CompletionOutcome{}, failures: map[string]error{}}
}

func (f *fakeCaller) Complete(_ context.Context, request CompletionRequest) (CompletionOutcome, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, request)
	if f.refuse {
		return CompletionOutcome{}, fmt.Errorf("%w: organization_budget_exhausted", ErrBridgeBudgetRefused)
	}
	if outcome, seen := f.byKey[request.IdempotencyKey]; seen {
		if outcome.Parsed == nil {
			return outcome, fmt.Errorf("operation %s ended failed", outcome.OperationID)
		}
		return outcome, nil
	}
	f.sequence++
	duration := int64(1000 * f.sequence)
	outcome := CompletionOutcome{OperationID: fmt.Sprintf("operation_%04d", f.sequence), InputTokens: 2000, CacheReadTokens: 500,
		OutputTokens: 50, UsageKnown: true, DurationMs: &duration, AnsweredBy: request.Model}
	if err := f.failures[request.Model]; err != nil {
		f.byKey[request.IdempotencyKey] = outcome
		return outcome, err
	}
	if strings.Contains(request.IdempotencyKey, ":judge:") {
		labels := judgeLabelsIn(request.Prompt)
		var verdicts []string
		for _, label := range labels {
			verdicts = append(verdicts, fmt.Sprintf(`{"label":%q,"acceptable":true,"failure_kind":"none","reason":"fine"}`, label))
		}
		outcome.Parsed = json.RawMessage(fmt.Sprintf(`{"ranking":%s,"verdicts":[%s]}`, mustJSON(labels), strings.Join(verdicts, ",")))
	} else {
		ref := emailFilingEmailPattern.FindStringSubmatch(request.Prompt)[1]
		outcome.Parsed = json.RawMessage(`{"filings":[` + syntheticFiling(ref, syntheticCardA) + `]}`)
	}
	f.byKey[request.IdempotencyKey] = outcome
	return outcome, nil
}

func judgeLabelsIn(prompt string) []string {
	var labels []string
	for _, label := range labelsFor(26) {
		if strings.Contains(prompt, "=== Candidate "+label+" ===") {
			labels = append(labels, label)
		}
	}
	return labels
}

func (f *fakeCaller) keys() []string {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	keys := make([]string, len(f.calls))
	for i, call := range f.calls {
		keys[i] = call.IdempotencyKey
	}
	return keys
}
