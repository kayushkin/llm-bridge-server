// Package completionoperation runs one schema-forced model call as an
// llm.completion operation on llm-bridge-server and waits for its receipt.
//
// It replaces a direct call to the bridge's instance oneshot endpoint. The
// model call is the same one — the bridge makes it through its own harness
// instance on the subscription login — but as an operation it is recorded,
// priced, counted against its organization's budget, and repeatable by
// idempotency key. docs/OPERATIONS.md in llm-bridge is the contract.
//
// Requests go out through http.DefaultTransport, which the binary's main has
// stamped with the bridge's service token (internal/bridgeauth).
package completionoperation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

var (
	// ErrTruncated: the reply stopped at the output limit. An incomplete
	// classification reads exactly like a model that had less to say, so a
	// caller must not act on one.
	ErrTruncated = errors.New("completion operation: reply hit max_tokens")
	// ErrBudgetExhausted: the organization has spent its month, or the call
	// would have gone past a budget. Nothing more will run until it is raised.
	ErrBudgetExhausted = errors.New("completion operation: budget exhausted")
	// ErrNoUsableAnswer: the call finished but left no answer that decodes
	// into the schema — the bridge said the model did not follow it
	// (schema_not_followed), or the parsed answer was empty or would not
	// decode into the caller's type.
	ErrNoUsableAnswer = errors.New("completion operation: no answer that fits the schema")
	// ErrPromptTooLong: the model refused the request because the input did
	// not fit, not because of the account. Sending less is the only fix.
	ErrPromptTooLong = errors.New("completion operation: prompt too long for the model")
)

// SmallerRequestMayHelp reports whether sending the same work in smaller
// pieces could change the outcome of a failed Call. The same prompt
// reproduces the same answer, so only failures about SIZE qualify: a reply
// cut off at the output limit, an answer that did not fit the schema, and a
// prompt the model would not take. A budget refusal, a missing instance, a
// canceled operation or an unreachable bridge fails the same way at any size.
func SmallerRequestMayHelp(err error) bool {
	return errors.Is(err, ErrTruncated) || errors.Is(err, ErrNoUsableAnswer) || errors.Is(err, ErrPromptTooLong)
}

// promptTooLongMarkers are the phrases a provider puts in a model call's
// error when the input, rather than the account, is the problem. Anthropic
// answers both "prompt is too long" and "credit balance is too low" with a
// 400 invalid_request_error, so the message is the only thing that separates
// them, and reading it is what stops a billing refusal being treated as a
// size failure.
var promptTooLongMarkers = []string{
	"prompt is too long",
	"input length and `max_tokens` exceed context limit",
	"exceeds the maximum",
	"too many tokens",
}

// modelCallFailureCodes are the operation error codes whose message carries
// the provider's own words, and so may carry a prompt-too-long marker.
var modelCallFailureCodes = map[string]bool{
	"model_call_failed":  true,
	"attempts_exhausted": true,
}

func promptWasTooLong(code, message string) bool {
	if !modelCallFailureCodes[code] {
		return false
	}
	lowered := strings.ToLower(message)
	for _, marker := range promptTooLongMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// Config is one call's worth of settings.
type Config struct {
	// BridgeURL is llm-bridge-server's base URL, e.g. http://localhost:8160.
	BridgeURL string
	// OrganizationID is the principal-store group the call is made for. Its
	// budget and grants apply.
	OrganizationID string
	// IdempotencyKey names this call. Sending the same key again returns the
	// first call's receipt instead of calling the model twice; a different
	// prompt under the same key is refused by the bridge.
	IdempotencyKey string
	// CorrelationID ties the call to the caller's own run, e.g. one tick.
	CorrelationID string
	// ModelRole is the model-store role the call asks for ("balanced"). It is
	// sent in the operation input's model field, which the bridge resolves as
	// an id, an alias or a role; the bridge calls the role's first model.
	ModelRole string
	// MaxTokens caps the reply, thinking included.
	MaxTokens int
	// Timeout bounds the wait for the receipt. On timeout the operation is
	// asked to stop.
	Timeout time.Duration
	// PollInterval is how often the receipt is read while waiting. Zero means
	// two seconds.
	PollInterval time.Duration
}

func (c Config) validate() error {
	switch {
	case c.BridgeURL == "":
		return errors.New("completion operation: BridgeURL is required")
	case c.OrganizationID == "":
		return errors.New("completion operation: OrganizationID is required (a principal-store group)")
	case c.IdempotencyKey == "":
		return errors.New("completion operation: IdempotencyKey is required")
	case c.ModelRole == "":
		return errors.New("completion operation: ModelRole is required (a model-store role such as balanced)")
	case c.MaxTokens <= 0:
		return errors.New("completion operation: MaxTokens must be positive")
	case c.Timeout <= 0:
		return errors.New("completion operation: Timeout must be positive")
	}
	return nil
}

// Call runs one completion forced to schema and decodes the answer into out.
// It returns the final receipt alongside any error, so a caller can log the
// operation id either way; the receipt is zero when nothing was accepted.
func Call(ctx context.Context, config Config, system, prompt string, schema map[string]any, out any) (msg.OperationReceipt, error) {
	if err := config.validate(); err != nil {
		return msg.OperationReceipt{}, err
	}
	schemaJSON, err := json.Marshal(schema)
	if err != nil {
		return msg.OperationReceipt{}, fmt.Errorf("completion operation: encode schema: %w", err)
	}
	input, err := json.Marshal(msg.LLMCompletionInput{Prompt: prompt, SystemPrompt: system, Model: config.ModelRole, Schema: schemaJSON, MaxTokens: config.MaxTokens})
	if err != nil {
		return msg.OperationReceipt{}, fmt.Errorf("completion operation: encode input: %w", err)
	}
	intent, err := json.Marshal(msg.OperationIntent{Type: msg.OperationTypeLLMCompletion, OrganizationID: config.OrganizationID,
		IdempotencyKey: config.IdempotencyKey, CorrelationID: config.CorrelationID, Input: input})
	if err != nil {
		return msg.OperationReceipt{}, fmt.Errorf("completion operation: encode intent: %w", err)
	}
	waitContext, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()
	base := strings.TrimRight(config.BridgeURL, "/")
	receipt, err := submit(waitContext, base, intent)
	if err != nil {
		return msg.OperationReceipt{}, err
	}
	pollInterval := config.PollInterval
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	stopWaiting := func() (msg.OperationReceipt, error) {
		stopContext, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = post(stopContext, base+"/operations/"+url.PathEscape(receipt.ID)+"/cancel", nil)
		return receipt, fmt.Errorf("completion operation %s: still %s when the wait ended; asked it to stop: %w", receipt.ID, receipt.State, waitContext.Err())
	}
	for !receipt.State.IsTerminal() {
		select {
		case <-waitContext.Done():
			return stopWaiting()
		case <-time.After(pollInterval):
		}
		latest, err := read(waitContext, base+"/operations/"+url.PathEscape(receipt.ID))
		if err != nil {
			if waitContext.Err() != nil {
				return stopWaiting()
			}
			return receipt, err
		}
		receipt = latest
	}
	return receipt, decodeAnswer(receipt, out)
}

func decodeAnswer(receipt msg.OperationReceipt, out any) error {
	if receipt.State != msg.OperationStateSucceeded {
		code, message := "", ""
		if receipt.Error != nil {
			code, message = receipt.Error.Code, receipt.Error.Message
		}
		failure := fmt.Errorf("completion operation %s ended %s: %s: %s", receipt.ID, receipt.State, code, message)
		switch code {
		case "reply_truncated":
			return fmt.Errorf("%w: %v", ErrTruncated, failure)
		case "budget_exhausted", "model_price_unknown", "budget_unenforceable":
			return fmt.Errorf("%w: %v", ErrBudgetExhausted, failure)
		case "schema_not_followed":
			return fmt.Errorf("%w: %v", ErrNoUsableAnswer, failure)
		}
		if promptWasTooLong(code, message) {
			return fmt.Errorf("%w: %v", ErrPromptTooLong, failure)
		}
		return failure
	}
	var result msg.LLMCompletionResult
	if err := json.Unmarshal(receipt.Result, &result); err != nil {
		return fmt.Errorf("completion operation %s: result is not an llm.completion result: %w", receipt.ID, err)
	}
	if len(result.Parsed) == 0 {
		return fmt.Errorf("%w: completion operation %s succeeded with no parsed answer (stop reason %q)", ErrNoUsableAnswer, receipt.ID, result.StopReason)
	}
	if err := json.Unmarshal(result.Parsed, out); err != nil {
		return fmt.Errorf("%w: completion operation %s: decode answer: %w", ErrNoUsableAnswer, receipt.ID, err)
	}
	return nil
}

func submit(ctx context.Context, base string, intent []byte) (msg.OperationReceipt, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/operations", bytes.NewReader(intent))
	if err != nil {
		return msg.OperationReceipt{}, fmt.Errorf("completion operation: build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, body, err := do(request)
	if err != nil {
		return msg.OperationReceipt{}, err
	}
	switch response.StatusCode {
	case http.StatusAccepted, http.StatusOK:
	case http.StatusPaymentRequired:
		return msg.OperationReceipt{}, fmt.Errorf("%w: POST %s/operations: %s", ErrBudgetExhausted, base, strings.TrimSpace(string(body)))
	case http.StatusRequestEntityTooLarge:
		return msg.OperationReceipt{}, fmt.Errorf("%w: POST %s/operations answered %s: %s", ErrPromptTooLong, base, response.Status, strings.TrimSpace(string(body)))
	default:
		return msg.OperationReceipt{}, fmt.Errorf("completion operation: POST %s/operations answered %s: %s", base, response.Status, strings.TrimSpace(string(body)))
	}
	var receipt msg.OperationReceipt
	if err := json.Unmarshal(body, &receipt); err != nil || receipt.ID == "" {
		return msg.OperationReceipt{}, fmt.Errorf("completion operation: POST %s/operations answered with something that is not a receipt: %s", base, strings.TrimSpace(string(body)))
	}
	return receipt, nil
}

func read(ctx context.Context, endpoint string) (msg.OperationReceipt, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return msg.OperationReceipt{}, fmt.Errorf("completion operation: build request: %w", err)
	}
	response, body, err := do(request)
	if err != nil {
		return msg.OperationReceipt{}, err
	}
	if response.StatusCode != http.StatusOK {
		return msg.OperationReceipt{}, fmt.Errorf("completion operation: GET %s answered %s: %s", endpoint, response.Status, strings.TrimSpace(string(body)))
	}
	var receipt msg.OperationReceipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		return msg.OperationReceipt{}, fmt.Errorf("completion operation: GET %s answered with something that is not a receipt: %w", endpoint, err)
	}
	return receipt, nil
}

func post(ctx context.Context, endpoint string, body []byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	_, _, err = do(request)
	return err
}

func do(request *http.Request) (*http.Response, []byte, error) {
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("completion operation: %s %s: %w", request.Method, request.URL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("completion operation: read %s %s: %w", request.Method, request.URL, err)
	}
	return response, body, nil
}
