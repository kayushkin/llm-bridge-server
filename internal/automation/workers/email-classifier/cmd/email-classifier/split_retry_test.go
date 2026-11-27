package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/degradations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
	"github.com/kayushkin/llm-bridge/msg"
)

// The message Anthropic really returned on 2026-08-29, when this job went red on
// every tick. Kept verbatim: it is a 400 exactly like a prompt-too-long 400, and
// only the message separates them.
const creditBalanceMessage = `anthropic 400 Bad Request: {"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}}`

const promptTooLongMessage = `anthropic 400 Bad Request: {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000 maximum"}}`

// countingBridge stands in for llm-bridge-server and counts how many
// operations it is asked to run. Every submit is answered with a receipt that
// already failed with operationError, so Call never polls. The count IS the
// assertion: this defect costs model calls, not correctness, so a test that
// only checked the emails ended up unfiled would pass either way.
func countingBridge(t *testing.T, operationError msg.OperationError) (string, func() int) {
	t.Helper()
	var mutex sync.Mutex
	submits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /operations", func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		submits++
		id := fmt.Sprintf("operation_%d", submits)
		mutex.Unlock()
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(msg.OperationReceipt{ID: id, State: msg.OperationStateFailed, Error: &operationError})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL, func() int {
		mutex.Lock()
		defer mutex.Unlock()
		return submits
	}
}

func bridgeConfig(bridgeURL string) config {
	return config{
		BridgeURL:       bridgeURL,
		FilingModelRole: "default",
		RunID:           "email-classifier:test:1",
		Classifier:      issuesclient.ClassifierConfig{OrganizationID: "principal_000023"},
	}
}

func batchOf(n int) []pendingEmail {
	batch := make([]pendingEmail, 0, n)
	for i := 0; i < n; i++ {
		batch = append(batch, pendingEmail{
			Locator:   fmt.Sprintf("gmail-personal:%04d", i),
			MessageID: fmt.Sprintf("<%04d@example.test>", i),
			Meta:      messageMeta{Subject: fmt.Sprintf("subject %d", i)},
		})
	}
	return batch
}

func emptyBoardState() *boardState {
	return &boardState{
		Cards:            map[string]*cardState{},
		FiledLocators:    map[string]bool{},
		CardsByMessageID: map[string][]string{},
		CardsBySender:    map[string][]string{},
	}
}

func classifyThroughBridge(t *testing.T, operationError msg.OperationError, batchSize int) (calls int, reasons []string) {
	t.Helper()
	bridgeURL, submits := countingBridge(t, operationError)
	degraded := degradations.New("email")
	classifyAndApply(context.Background(), bridgeConfig(bridgeURL), &applier{}, emptyBoardState(), &routingNotes{}, batchOf(batchSize), degraded)
	return submits(), degraded.Reasons()
}

// A batch of 10 against a refusal that cannot change with batch size used to
// make 19 calls — 10 leaves plus 9 inner nodes of the binary recursion — each
// refused for the same reason as the first.
func TestAFailureASmallerBatchCannotFixIsNotSplit(t *testing.T) {
	cases := []msg.OperationError{
		{Code: "budget_exhausted", Message: "the organization has spent its month"},
		{Code: "model_price_unknown", Message: "no price for the model"},
		{Code: "budget_unenforceable", Message: "cannot enforce the budget"},
		{Code: "no_instance_for_provider", Message: "no instance"},
		{Code: "unknown_model", Message: "not in model-store"},
		{Code: "cancelled", Message: "cancelled"},
		{Code: "model_call_failed", Message: creditBalanceMessage},
		{Code: "attempts_exhausted", Message: "its worker stopped on attempt 3 of 3"},
	}
	for _, operationError := range cases {
		t.Run(operationError.Code, func(t *testing.T) {
			calls, reasons := classifyThroughBridge(t, operationError, 10)
			if calls != 1 {
				t.Errorf("made %d model calls for a failure that cannot change with batch size; want 1", calls)
			}
			if len(reasons) != 10 {
				t.Fatalf("recorded %d shortfalls; want one per email left unfiled (10): %q", len(reasons), reasons)
			}
			for i, reason := range reasons {
				if !strings.Contains(reason, fmt.Sprintf("gmail-personal:%04d", i)) {
					t.Errorf("shortfall %d does not name its email: %q", i, reason)
				}
			}
		})
	}
}

// The behavior the split exists for. Without these the fix above reads as
// "stop splitting", which would be a different and wrong change.
func TestAFailureASmallerBatchCanFixStillSplits(t *testing.T) {
	cases := []msg.OperationError{
		{Code: "reply_truncated", Message: "the reply stopped at the output limit (max_tokens 8192, output tokens 8192)"},
		{Code: "schema_not_followed", Message: `asked for JSON matching a schema and got none (stop reason "end_turn")`},
		{Code: "model_call_failed", Message: promptTooLongMessage},
	}
	for _, operationError := range cases {
		t.Run(operationError.Code, func(t *testing.T) {
			calls, reasons := classifyThroughBridge(t, operationError, 10)
			// 10 leaves + 9 inner nodes of a binary recursion over 10 items.
			if calls != 19 {
				t.Errorf("split retry made %d model calls over a batch of 10; want the full recursion (19)", calls)
			}
			if len(reasons) != 10 {
				t.Errorf("recorded %d shortfalls; want 10", len(reasons))
			}
		})
	}
}

// A bridge that refuses the submit outright is not a size failure either.
func TestARefusedSubmitIsNotSplit(t *testing.T) {
	var mutex sync.Mutex
	submits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		submits++
		mutex.Unlock()
		http.Error(w, "service token refused", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	degraded := degradations.New("email")
	classifyAndApply(context.Background(), bridgeConfig(server.URL), &applier{}, emptyBoardState(), &routingNotes{}, batchOf(4), degraded)
	mutex.Lock()
	defer mutex.Unlock()
	if submits != 1 {
		t.Errorf("made %d submits against a bridge that refuses them; want 1", submits)
	}
	if got := len(degraded.Reasons()); got != 4 {
		t.Errorf("recorded %d shortfalls; want 4", got)
	}
}
