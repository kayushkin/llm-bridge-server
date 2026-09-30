package executors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/oneshotrouting"
	"github.com/kayushkin/llm-bridge-server/internal/operations"
	"github.com/kayushkin/llm-bridge/msg"
)

// OneShotCaller makes stateless model calls through harness instances. The
// server implements it with its one-shot router, so no provider API and no
// credential passes through this package: the harness holds the login.
type OneShotCaller interface {
	// OneShotRoute resolves what an input asked for — a model-store role, a
	// model id or an alias; empty for the configured default role — to the
	// models to try, in order. A failure is a *TargetError.
	OneShotRoute(requestedModel string) (oneshotrouting.Route, error)
	// RunOneShotRoute tries the route's models in order, each on the
	// instance configured for its provider, until one answers. admit is asked
	// about each model first; a reason skips it. The response names the model
	// and instance that answered and the attempts before it; an error means
	// no model answered (an *oneshotrouting.AllModelsFailedError).
	RunOneShotRoute(ctx context.Context, route oneshotrouting.Route, request msg.OneShotRequest, admit func(oneshotrouting.Candidate) string) (msg.OneShotResponse, error)
}

// Callers named on the executors' one-shot requests, for logs.
const (
	callerLLMCompletion    = "operations-llm-completion"
	callerModelClassifier  = "operations-classification-run"
	priceUnknownSkipPrefix = "model-store has no price for "
)

// TargetError is a route that cannot be built: no model named, one
// model-store does not know, a role with no models. Retrying cannot fix it.
type TargetError struct {
	Code    string
	Message string
}

func (e *TargetError) Error() string { return e.Code + ": " + e.Message }

// resolveRoute asks the caller for the route and turns a failure into the
// result that ends the attempt.
func resolveRoute(caller OneShotCaller, requestedModel string) (oneshotrouting.Route, *operations.Result) {
	route, err := caller.OneShotRoute(requestedModel)
	if err == nil {
		return route, nil
	}
	var targetError *TargetError
	if errors.As(err, &targetError) {
		refusal := failed(targetError.Code, targetError.Message)
		return oneshotrouting.Route{}, &refusal
	}
	return oneshotrouting.Route{}, &operations.Result{Error: &msg.OperationError{Code: "model_resolution_failed", Retryable: true, Message: err.Error()}}
}

// routeEvidence records which model answered and where, for diagnostics.
func routeEvidence(route oneshotrouting.Route, response msg.OneShotResponse) msg.OperationEvidence {
	detail, _ := json.Marshal(map[string]any{"requested_model": route.Requested, "role": route.Role, "answered_by": response.Model,
		"instance_id": response.InstanceID, "attempts": response.Attempts, "duration_ms": response.DurationMs})
	return msg.OperationEvidence{Kind: "model_call", Summary: fmt.Sprintf("%s (asked for %s) on %s", response.Model, route.Requested, response.InstanceID), Detail: detail}
}

// budgetGate is the budget check around one routed call. Before the call it
// refuses when nothing is left; during it, admit skips a model with no list
// price when the operation is under a budget.
type budgetGate struct {
	receipt    operations.ReceiptWriter
	limited    bool
	priceSkips int
}

// openBudgetGate is nil and a refusal when the budget is spent.
func openBudgetGate(receipt operations.ReceiptWriter) (*budgetGate, *operations.Result) {
	remainingUSD, limited, err := receipt.SpendingAllowance()
	if err != nil {
		refusal := writeFailure(err)
		return nil, &refusal
	}
	if limited && remainingUSD <= 0 {
		refusal := failed("budget_exhausted", fmt.Sprintf("no budget left for a model call (%.4f dollars remaining)", remainingUSD))
		return nil, &refusal
	}
	return &budgetGate{receipt: receipt, limited: limited}, nil
}

func (gate *budgetGate) admit(candidate oneshotrouting.Candidate) string {
	if gate.limited && !gate.receipt.ModelHasListPrice(candidate.ModelID) {
		gate.priceSkips++
		return priceUnknownSkipPrefix + candidate.ModelID + " and the operation is under a budget"
	}
	return ""
}

// callFailure is the result for a routed call that got no answer.
func (gate *budgetGate) callFailure(ctx context.Context, err error, context string) operations.Result {
	var allFailed *oneshotrouting.AllModelsFailedError
	if errors.As(err, &allFailed) && gate.priceSkips > 0 && gate.priceSkips == len(allFailed.Attempts) {
		return failed("model_price_unknown", context+err.Error())
	}
	// A model call changes nothing outside the bridge, so trying again is
	// safe; the coordinator decides whether attempts remain.
	code := "model_call_failed"
	if ctx.Err() != nil {
		code = "attempt_interrupted"
	}
	return operations.Result{Error: &msg.OperationError{Code: code, Message: context + err.Error(), Retryable: true}}
}

// LLMCompletion runs llm.completion: one model call through a harness
// instance, priced at list price and held to the operation's budgets.
type LLMCompletion struct {
	Caller OneShotCaller
}

// Describe implements operations.Executor.
func (LLMCompletion) Describe() msg.OperationTypeDescription {
	return msg.OperationTypeDescription{
		Type:     msg.OperationTypeLLMCompletion,
		Executor: "harness-oneshot",
		Description: "One stateless model call through a bridge harness instance, " +
			"optionally forced to a JSON Schema. The instance holds the login; nothing here calls a provider directly.",
		MaximumAttempts: 2,
		TimeoutSeconds:  300,
	}
}

// Validate implements operations.Executor.
func (LLMCompletion) Validate(intent msg.OperationIntent) error {
	_, err := decodeCompletionInput(intent.Input)
	return err
}

func decodeCompletionInput(raw json.RawMessage) (msg.LLMCompletionInput, error) {
	var input msg.LLMCompletionInput
	if len(raw) == 0 {
		return input, errors.New("llm.completion needs input with a prompt")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("llm.completion input: %w", err)
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return input, errors.New("llm.completion input needs a prompt")
	}
	if input.MaxTokens < 0 {
		return input, errors.New("max_tokens must not be negative")
	}
	if len(input.Schema) > 0 {
		var schema map[string]any
		if err := json.Unmarshal(input.Schema, &schema); err != nil {
			return input, fmt.Errorf("schema must be a JSON object: %w", err)
		}
	}
	return input, nil
}

// Execute implements operations.Executor.
func (completion LLMCompletion) Execute(ctx context.Context, intent msg.OperationIntent, receipt operations.ReceiptWriter) operations.Result {
	input, err := decodeCompletionInput(intent.Input)
	if err != nil {
		return failed("invalid_input", err.Error())
	}
	route, refusal := resolveRoute(completion.Caller, input.Model)
	if refusal != nil {
		return *refusal
	}
	gate, refusal := openBudgetGate(receipt)
	if refusal != nil {
		return *refusal
	}
	response, err := completion.Caller.RunOneShotRoute(ctx, route, msg.OneShotRequest{
		Prompt: input.Prompt, SystemPrompt: input.SystemPrompt, Schema: input.Schema, MaxTokens: input.MaxTokens, Caller: callerLLMCompletion,
	}, gate.admit)
	if err != nil {
		return gate.callFailure(ctx, err, "")
	}
	if err := receipt.RecordModelCall(response.Model, response.Usage); err != nil {
		return writeFailure(err)
	}
	if err := receipt.AddEvidence(routeEvidence(route, response)); err != nil {
		return writeFailure(err)
	}
	if response.StopReason == "max_tokens" {
		// A cut-off reply cannot be trusted, and running it again with the
		// same limit is cut off the same way. The caller must raise
		// max_tokens or send less.
		return failed("reply_truncated", fmt.Sprintf("the reply stopped at the output limit (max_tokens %d, output tokens %d)", input.MaxTokens, response.Usage.OutputTokens))
	}
	if len(input.Schema) > 0 && len(response.Parsed) == 0 {
		return operations.Result{Error: &msg.OperationError{Code: "schema_not_followed", Retryable: true,
			Message: fmt.Sprintf("asked for JSON matching a schema and got none (stop reason %q)", response.StopReason)}}
	}
	encoded, err := json.Marshal(msg.LLMCompletionResult{Text: response.Text, Parsed: response.Parsed, StopReason: response.StopReason})
	if err != nil {
		return failed("result_not_encodable", err.Error())
	}
	return operations.Result{State: msg.OperationStateSucceeded, Result: encoded}
}

// writeFailure ends an attempt whose receipt write failed, usually because
// its lease was lost. Retryable: whoever holds the operation now decides.
func writeFailure(err error) operations.Result {
	return operations.Result{Error: &msg.OperationError{Code: "receipt_write_failed", Retryable: true, Message: err.Error()}}
}

func failed(code, message string) operations.Result {
	return operations.Result{State: msg.OperationStateFailed, Error: &msg.OperationError{Code: code, Message: message}}
}

// Reconcile implements operations.Executor. A model call has no effect
// outside the bridge beyond its cost, so running it again is safe.
func (LLMCompletion) Reconcile(context.Context, msg.OperationReceipt) operations.ReconcileResult {
	return operations.ReconcileResult{Action: operations.ReconcileRunAgain}
}

// Cancel implements operations.Executor. Ending the attempt's context kills
// the harness process; there is nothing else to stop.
func (LLMCompletion) Cancel(context.Context, msg.OperationReceipt) error { return nil }
