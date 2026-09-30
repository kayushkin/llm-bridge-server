package completionoperation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// fakeBridge accepts one intent and answers reads with the receipts in
// states, one per read, repeating the last.
type fakeBridge struct {
	mutex      sync.Mutex
	submitCode int
	intent     msg.OperationIntent
	states     []msg.OperationReceipt
	reads      int
	cancelled  bool
}

func (f *fakeBridge) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /operations", func(w http.ResponseWriter, r *http.Request) {
		f.mutex.Lock()
		defer f.mutex.Unlock()
		json.NewDecoder(r.Body).Decode(&f.intent)
		if f.submitCode != 0 {
			w.WriteHeader(f.submitCode)
			w.Write([]byte(`{"error":{"code":"organization_budget_exhausted"}}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(msg.OperationReceipt{ID: "operation_1", State: msg.OperationStateQueued})
	})
	mux.HandleFunc("GET /operations/operation_1", func(w http.ResponseWriter, r *http.Request) {
		f.mutex.Lock()
		defer f.mutex.Unlock()
		receipt := f.states[min(f.reads, len(f.states)-1)]
		f.reads++
		json.NewEncoder(w).Encode(receipt)
	})
	mux.HandleFunc("POST /operations/operation_1/cancel", func(w http.ResponseWriter, r *http.Request) {
		f.mutex.Lock()
		f.cancelled = true
		f.mutex.Unlock()
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func config(url string) Config {
	return Config{BridgeURL: url, OrganizationID: "principal_000023", IdempotencyKey: "email-classifier:run:1", CorrelationID: "run",
		ModelRole: "balanced", MaxTokens: 1000, Timeout: 5 * time.Second, PollInterval: time.Millisecond}
}

func succeeded(parsed string, stopReason string) msg.OperationReceipt {
	result, _ := json.Marshal(msg.LLMCompletionResult{Parsed: json.RawMessage(parsed), StopReason: stopReason})
	return msg.OperationReceipt{ID: "operation_1", State: msg.OperationStateSucceeded, Result: result}
}

func TestACallWaitsForTheReceiptAndDecodesTheAnswer(t *testing.T) {
	bridge := &fakeBridge{states: []msg.OperationReceipt{
		{ID: "operation_1", State: msg.OperationStateRunning}, succeeded(`{"answer":"yes"}`, "end_turn")}}
	var out struct{ Answer string }
	receipt, err := Call(context.Background(), config(bridge.server(t).URL), "system", "prompt", map[string]any{"type": "object"}, &out)
	if err != nil || out.Answer != "yes" || receipt.ID != "operation_1" {
		t.Fatalf("err %v out %+v receipt %+v", err, out, receipt)
	}
	var input msg.LLMCompletionInput
	json.Unmarshal(bridge.intent.Input, &input)
	if bridge.intent.Type != msg.OperationTypeLLMCompletion || bridge.intent.OrganizationID != "principal_000023" ||
		bridge.intent.IdempotencyKey != "email-classifier:run:1" || input.Model != "balanced" || input.MaxTokens != 1000 ||
		input.SystemPrompt != "system" || string(input.Schema) != `{"type":"object"}` {
		t.Fatalf("intent %+v input %+v", bridge.intent, input)
	}
}

func TestFailuresAreNamedSoTheCallerCanTellThemApart(t *testing.T) {
	failedWith := func(code string) msg.OperationReceipt {
		return msg.OperationReceipt{ID: "operation_1", State: msg.OperationStateFailed, Error: &msg.OperationError{Code: code, Message: "m"}}
	}
	for code, want := range map[string]error{"reply_truncated": ErrTruncated, "budget_exhausted": ErrBudgetExhausted} {
		bridge := &fakeBridge{states: []msg.OperationReceipt{failedWith(code)}}
		var out any
		if _, err := Call(context.Background(), config(bridge.server(t).URL), "s", "p", map[string]any{}, &out); !errors.Is(err, want) {
			t.Errorf("%s: %v", code, err)
		}
	}
	bridge := &fakeBridge{submitCode: http.StatusPaymentRequired}
	var out any
	if _, err := Call(context.Background(), config(bridge.server(t).URL), "s", "p", map[string]any{}, &out); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("402 on submit: %v", err)
	}
}

func TestAWaitThatRunsOutAsksTheOperationToStop(t *testing.T) {
	bridge := &fakeBridge{states: []msg.OperationReceipt{{ID: "operation_1", State: msg.OperationStateRunning}}}
	cfg := config(bridge.server(t).URL)
	cfg.Timeout = 50 * time.Millisecond
	var out any
	_, err := Call(context.Background(), cfg, "s", "p", map[string]any{}, &out)
	bridge.mutex.Lock()
	defer bridge.mutex.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) || !bridge.cancelled {
		t.Fatalf("err %v cancelled %v", err, bridge.cancelled)
	}
}

// SmallerRequestMayHelp is what a caller's split retry reads, so each failure
// must land on the right side of it. A size failure marked "cannot help"
// leaves the same work failing on every run; a refusal marked "may help" is
// retried at every smaller size for the same answer.
func TestOnlySizeFailuresSayASmallerRequestMayHelp(t *testing.T) {
	failedWith := func(code, message string) msg.OperationReceipt {
		return msg.OperationReceipt{ID: "operation_1", State: msg.OperationStateFailed, Error: &msg.OperationError{Code: code, Message: message}}
	}
	cases := []struct {
		name    string
		receipt msg.OperationReceipt
		want    bool
	}{
		{"reply truncated", failedWith("reply_truncated", "m"), true},
		{"schema not followed", failedWith("schema_not_followed", "m"), true},
		{"prompt too long", failedWith("model_call_failed", "anthropic 400: Prompt is too long: 250000 tokens > 200000 maximum"), true},
		{"context limit phrasing", failedWith("model_call_failed", "input length and `max_tokens` exceed context limit: 200000 + 8192 > 200000"), true},
		{"credit balance too low", failedWith("model_call_failed", "anthropic 400: Your credit balance is too low to access the Anthropic API."), false},
		{"marker under a code that carries no provider text", failedWith("invalid_input", "prompt is too long"), false},
		{"budget exhausted", failedWith("budget_exhausted", "m"), false},
		{"no instance", failedWith("no_instance_for_provider", "m"), false},
		{"cancelled", failedWith("cancelled", "m"), false},
		{"succeeded with nothing parsed", succeeded(``, "end_turn"), true},
		{"answer that does not decode", succeeded(`"a string, not an object"`, "end_turn"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bridge := &fakeBridge{states: []msg.OperationReceipt{tc.receipt}}
			var out struct{ Answer string }
			_, err := Call(context.Background(), config(bridge.server(t).URL), "s", "p", map[string]any{}, &out)
			if err == nil {
				t.Fatal("Call succeeded; want a failure")
			}
			if got := SmallerRequestMayHelp(err); got != tc.want {
				t.Errorf("SmallerRequestMayHelp(%v) = %v; want %v", err, got, tc.want)
			}
		})
	}
	for status, want := range map[int]bool{http.StatusRequestEntityTooLarge: true, http.StatusPaymentRequired: false, http.StatusUnauthorized: false} {
		bridge := &fakeBridge{submitCode: status}
		var out any
		_, err := Call(context.Background(), config(bridge.server(t).URL), "s", "p", map[string]any{}, &out)
		if got := SmallerRequestMayHelp(err); got != want {
			t.Errorf("submit answered %d: SmallerRequestMayHelp(%v) = %v; want %v", status, err, got, want)
		}
	}
}
