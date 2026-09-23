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
	// CompletionTarget is the instance to call and the model to ask for when
	// an input names none, read from the stored settings at the time of use.
	CompletionTarget() (instanceID, defaultModel string)
	// RunOneShot calls the instance's harness. An error means no usable
	// answer came back.
	RunOneShot(ctx context.Context, instanceID string, request msg.OneShotRequest) (msg.OneShotResponse, error)
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
	instanceID, defaultModel := completion.Caller.CompletionTarget()
	if instanceID == "" {
		return failed("completion_instance_not_configured", "operations.completion_instance is empty, so there is no harness to call")
	}
	model := input.Model
	if model == "" {
		model = defaultModel
	}
	remainingUSD, limited, err := receipt.SpendingAllowance()
	if err != nil {
		return writeFailure(err)
	}
	if limited {
		switch {
		case remainingUSD <= 0:
			return failed("budget_exhausted", fmt.Sprintf("no budget left for a model call (%.4f dollars remaining)", remainingUSD))
		case model == "":
			return failed("budget_unenforceable", "the operation is under a budget and names no model, and operations.completion_model is empty, so the call's price cannot be known")
		case !receipt.ModelHasListPrice(model):
			return failed("model_price_unknown", fmt.Sprintf("the operation is under a budget and model-store has no price for %q", model))
		}
	}
	response, err := completion.Caller.RunOneShot(ctx, instanceID, msg.OneShotRequest{
		Prompt: input.Prompt, SystemPrompt: input.SystemPrompt, Model: model, Schema: input.Schema, MaxTokens: input.MaxTokens,
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
		answeredBy = model
	}
	if err := receipt.RecordModelCall(answeredBy, response.Usage); err != nil {
		return writeFailure(err)
	}
	detail, _ := json.Marshal(map[string]any{"instance_id": instanceID, "model": answeredBy, "duration_ms": response.DurationMs})
	if err := receipt.AddEvidence(msg.OperationEvidence{Kind: "model_call", Summary: "answered by " + answeredBy, Detail: detail}); err != nil {
		return writeFailure(err)
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
