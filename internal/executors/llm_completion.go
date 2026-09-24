package executors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/operations"
	"github.com/kayushkin/llm-bridge/msg"
)

// OneShotCaller makes one stateless model call through a harness instance.
// The server implements it with its one-shot path, so no provider API and no
// credential passes through this package: the harness holds the login.
type OneShotCaller interface {
	// CompletionTarget resolves the model an input asked for — empty for the
	// configured default — to a concrete model and the instance that serves
	// its provider. A failure is a *TargetError.
	CompletionTarget(requestedModel string) (CompletionTarget, error)
	// RunOneShot calls the instance's harness. An error means no usable
	// answer came back.
	RunOneShot(ctx context.Context, instanceID string, request msg.OneShotRequest) (msg.OneShotResponse, error)
}

// CompletionTarget is where one model call goes.
type CompletionTarget struct {
	// RequestedModel is what the input or the default setting named: an id,
	// an alias or a role.
	RequestedModel string
	// ModelID is model-store's id for it, sent to the harness and priced.
	ModelID    string
	Provider   string
	InstanceID string
}

// TargetError is a model that cannot be called: none named, one model-store
// does not know, or a provider no instance serves. Retrying cannot fix it.
type TargetError struct {
	Code    string
	Message string
}

func (e *TargetError) Error() string { return e.Code + ": " + e.Message }

// resolveTarget asks the caller for the target and turns a failure into the
// result that ends the attempt.
func resolveTarget(caller OneShotCaller, requestedModel string) (CompletionTarget, *operations.Result) {
	target, err := caller.CompletionTarget(requestedModel)
	if err == nil {
		return target, nil
	}
	var targetError *TargetError
	if errors.As(err, &targetError) {
		refusal := failed(targetError.Code, targetError.Message)
		return CompletionTarget{}, &refusal
	}
	return CompletionTarget{}, &operations.Result{Error: &msg.OperationError{Code: "model_resolution_failed", Retryable: true, Message: err.Error()}}
}

// targetEvidence records which model the call was routed to, for diagnostics.
func targetEvidence(target CompletionTarget, answeredBy string, durationMilliseconds int64) msg.OperationEvidence {
	detail, _ := json.Marshal(map[string]any{"requested_model": target.RequestedModel, "model": target.ModelID, "answered_by": answeredBy,
		"provider": target.Provider, "instance_id": target.InstanceID, "duration_ms": durationMilliseconds})
	return msg.OperationEvidence{Kind: "model_call", Summary: fmt.Sprintf("%s (asked for %s) on %s", answeredBy, target.RequestedModel, target.InstanceID), Detail: detail}
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
	target, refusal := resolveTarget(completion.Caller, input.Model)
	if refusal != nil {
		return *refusal
	}
	if refusal := refuseCallOverBudget(receipt, target.ModelID); refusal != nil {
		return *refusal
	}
	response, err := completion.Caller.RunOneShot(ctx, target.InstanceID, msg.OneShotRequest{
		Prompt: input.Prompt, SystemPrompt: input.SystemPrompt, Model: target.ModelID, Schema: input.Schema, MaxTokens: input.MaxTokens,
	})
	if err != nil {
		// A model call changes nothing outside the bridge, so trying again
		// is safe; the coordinator decides whether attempts remain.
		code := "model_call_failed"
		if ctx.Err() != nil {
			code = "attempt_interrupted"
		}
		return operations.Result{Error: &msg.OperationError{Code: code, Message: err.Error(), Retryable: true}}
	}
	answeredBy := response.Model
	if answeredBy == "" {
		answeredBy = target.ModelID
	}
	if err := receipt.RecordModelCall(answeredBy, response.Usage); err != nil {
		return writeFailure(err)
	}
	if err := receipt.AddEvidence(targetEvidence(target, answeredBy, response.DurationMs)); err != nil {
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

// refuseCallOverBudget is the check before every model call: nil when the
// call may be made, the result to end the attempt with when it may not.
func refuseCallOverBudget(receipt operations.ReceiptWriter, model string) *operations.Result {
	remainingUSD, limited, err := receipt.SpendingAllowance()
	if err != nil {
		refusal := writeFailure(err)
		return &refusal
	}
	if !limited {
		return nil
	}
	var refusal operations.Result
	switch {
	case remainingUSD <= 0:
		refusal = failed("budget_exhausted", fmt.Sprintf("no budget left for a model call (%.4f dollars remaining)", remainingUSD))
	case !receipt.ModelHasListPrice(model):
		refusal = failed("model_price_unknown", fmt.Sprintf("the operation is under a budget and model-store has no price for %q", model))
	default:
		return nil
	}
	return &refusal
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
