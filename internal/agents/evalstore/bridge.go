package evalstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// CompletionRequest is one schema-forced model call.
type CompletionRequest struct {
	OrganizationID string
	IdempotencyKey string
	CorrelationID  string
	// Model is a model-store id; the bridge calls that model alone.
	Model        string
	SystemPrompt string
	Prompt       string
	Schema       any
	MaxTokens    int
	Timeout      time.Duration
}

// CompletionOutcome is what came back. OperationID is set whenever the
// bridge accepted the intent, even when the call then failed.
type CompletionOutcome struct {
	OperationID      string
	Parsed           json.RawMessage
	InputTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	OutputTokens     int64
	UsageKnown       bool
	DurationMs       *int64
	AnsweredBy       string
}

// ModelCaller makes model calls. BridgeOperations is the real one; tests fake
// it.
type ModelCaller interface {
	Complete(ctx context.Context, request CompletionRequest) (CompletionOutcome, error)
}

// ErrBridgeBudgetRefused: the bridge refused the call for money — the
// organization's month is spent, or a model has no price under a budget.
// A run stops on it rather than failing every remaining call the same way.
var ErrBridgeBudgetRefused = errors.New("llm-bridge-server refused the call for budget")

// Operations is llm-bridge-server's operations coordinator as eval-store uses
// it: as the server's internal service, acting as no principal, as scheduler
// does. eval-store runs inside llm-bridge-server, which implements it with
// the same checks and coordinator calls its operation routes make.
type Operations interface {
	// SubmitOperation submits an intent and answers its receipt; the same
	// idempotency key answers the first operation's. A refusal for money
	// wraps ErrBridgeBudgetRefused.
	SubmitOperation(ctx context.Context, intent msg.OperationIntent) (msg.OperationReceipt, error)
	ReadOperation(ctx context.Context, id string) (msg.OperationReceipt, error)
	CancelOperation(ctx context.Context, id string) error
	// ListSucceededOperations answers the succeeded operations the query
	// matches, newest first, at most query.Limit of them.
	ListSucceededOperations(ctx context.Context, query OperationQuery) ([]msg.OperationReceipt, error)
	// ReadOperationIntent answers the intent an operation was submitted with,
	// or ErrOperationIntentsNotServed.
	ReadOperationIntent(ctx context.Context, id string) (msg.OperationIntent, error)
}

// OperationQuery selects operations on llm-bridge-server. A zero time or an
// empty field selects on nothing.
type OperationQuery struct {
	Type           msg.OperationType
	OrganizationID string
	PrincipalID    string
	CreatedAfter   time.Time
	CreatedBefore  time.Time
	Limit          int
}

// BridgeOperations makes model calls and reads operations through
// llm-bridge-server's operations coordinator. Each call is billed to the
// organization the intent names.
type BridgeOperations struct {
	Operations   Operations
	PollInterval time.Duration
}

func (b *BridgeOperations) configured() error {
	if b.Operations == nil {
		return errors.New("this eval-store was given no llm-bridge-server to call")
	}
	return nil
}

// Complete submits one llm.completion operation and waits for its receipt.
// Sending the same idempotency key again returns the first operation, so a
// resumed run does not pay twice for a call it already made.
func (b *BridgeOperations) Complete(ctx context.Context, request CompletionRequest) (CompletionOutcome, error) {
	if err := b.configured(); err != nil {
		return CompletionOutcome{}, err
	}
	schema, err := json.Marshal(request.Schema)
	if err != nil {
		return CompletionOutcome{}, fmt.Errorf("encode schema: %w", err)
	}
	input, err := json.Marshal(msg.LLMCompletionInput{Prompt: request.Prompt, SystemPrompt: request.SystemPrompt, Model: request.Model,
		Schema: schema, MaxTokens: request.MaxTokens})
	if err != nil {
		return CompletionOutcome{}, err
	}
	intent := msg.OperationIntent{Type: msg.OperationTypeLLMCompletion, OrganizationID: request.OrganizationID,
		IdempotencyKey: request.IdempotencyKey, CorrelationID: request.CorrelationID, Input: input}
	waitContext, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()
	receipt, err := b.Operations.SubmitOperation(waitContext, intent)
	if err != nil {
		return CompletionOutcome{}, fmt.Errorf("submitting the operation: %w", err)
	}
	if receipt.ID == "" {
		return CompletionOutcome{}, errors.New("submitting the operation answered a receipt with no id")
	}
	pollInterval := b.PollInterval
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	for !receipt.State.IsTerminal() {
		select {
		case <-waitContext.Done():
			if ctx.Err() != nil {
				// The caller is going away, not giving up on the call: leave the
				// operation running so the same key collects it later.
				return CompletionOutcome{OperationID: receipt.ID}, fmt.Errorf("operation %s left %s: %w", receipt.ID, receipt.State, ctx.Err())
			}
			stopContext, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer stopCancel()
			if cancelError := b.Operations.CancelOperation(stopContext, receipt.ID); cancelError != nil {
				log.Printf("eval-store: asking operation %s to stop after its wait ended: %v", receipt.ID, cancelError)
			}
			return CompletionOutcome{OperationID: receipt.ID}, fmt.Errorf("operation %s still %s when the wait ended; asked it to stop: %w",
				receipt.ID, receipt.State, waitContext.Err())
		case <-time.After(pollInterval):
		}
		latest, err := b.ReadOperation(waitContext, receipt.ID)
		if err != nil {
			if waitContext.Err() != nil {
				continue
			}
			return CompletionOutcome{OperationID: receipt.ID}, err
		}
		receipt = latest
	}
	return outcomeOfReceipt(receipt)
}

// ReadOperation reads one receipt.
func (b *BridgeOperations) ReadOperation(ctx context.Context, id string) (msg.OperationReceipt, error) {
	receipt, err := b.Operations.ReadOperation(ctx, id)
	if err != nil {
		return msg.OperationReceipt{}, fmt.Errorf("reading operation %s: %w", id, err)
	}
	return receipt, nil
}

// outcomeOfReceipt reads a terminal receipt into an outcome, or the error
// that ended it.
func outcomeOfReceipt(receipt msg.OperationReceipt) (CompletionOutcome, error) {
	outcome := CompletionOutcome{OperationID: receipt.ID}
	if receipt.Usage != nil {
		tokens := receipt.Usage.Tokens
		outcome.InputTokens, outcome.CacheReadTokens = int64(tokens.InputTokens), int64(tokens.CacheReadTokens)
		outcome.CacheWriteTokens, outcome.OutputTokens = int64(tokens.CacheWriteTokens), int64(tokens.OutputTokens)
		outcome.UsageKnown = true
	}
	for _, evidence := range receipt.Evidence {
		if evidence.Kind != "model_call" {
			continue
		}
		var detail struct {
			AnsweredBy string `json:"answered_by"`
			DurationMs *int64 `json:"duration_ms"`
		}
		if err := json.Unmarshal(evidence.Detail, &detail); err == nil {
			outcome.AnsweredBy, outcome.DurationMs = detail.AnsweredBy, detail.DurationMs
		}
	}
	if outcome.DurationMs == nil && receipt.StartedAt != nil && receipt.CompletedAt != nil {
		milliseconds := receipt.CompletedAt.Sub(*receipt.StartedAt).Milliseconds()
		outcome.DurationMs = &milliseconds
	}
	if receipt.State != msg.OperationStateSucceeded {
		code, message := "", ""
		if receipt.Error != nil {
			code, message = receipt.Error.Code, receipt.Error.Message
		}
		failure := fmt.Errorf("operation %s ended %s: %s: %s", receipt.ID, receipt.State, code, message)
		switch code {
		case "budget_exhausted", "model_price_unknown", "budget_unenforceable":
			return outcome, fmt.Errorf("%w: %v", ErrBridgeBudgetRefused, failure)
		}
		return outcome, failure
	}
	var result msg.LLMCompletionResult
	if err := json.Unmarshal(receipt.Result, &result); err != nil {
		return outcome, fmt.Errorf("operation %s: result is not an llm.completion result: %w", receipt.ID, err)
	}
	if len(result.Parsed) == 0 || string(result.Parsed) == "null" {
		return outcome, fmt.Errorf("operation %s succeeded with no parsed answer (stop reason %q)", receipt.ID, result.StopReason)
	}
	outcome.Parsed = result.Parsed
	return outcome, nil
}

// OperationsFilter selects operations on llm-bridge-server. The bridge
// filters on everything but CorrelationIDPrefix, which eval-store applies.
type OperationsFilter struct {
	Type                string `json:"type,omitempty"`
	OrganizationID      string `json:"organization_id,omitempty"`
	PrincipalID         string `json:"principal_id,omitempty"`
	CorrelationIDPrefix string `json:"correlation_id_prefix,omitempty"`
	CreatedAfter        string `json:"created_after,omitempty"`
	CreatedBefore       string `json:"created_before,omitempty"`
}

// operationListPageSize is the bridge's largest page.
const operationListPageSize = 500

// ListOperations reads every succeeded operation the filter matches, newest
// first, paging back by creation time.
func (b *BridgeOperations) ListOperations(ctx context.Context, filter OperationsFilter) ([]msg.OperationReceipt, error) {
	if err := b.configured(); err != nil {
		return nil, err
	}
	query := OperationQuery{Type: msg.OperationType(filter.Type), OrganizationID: filter.OrganizationID, PrincipalID: filter.PrincipalID, Limit: operationListPageSize}
	for name, field := range map[string]struct {
		value  string
		target *time.Time
	}{"created_after": {filter.CreatedAfter, &query.CreatedAfter}, "created_before": {filter.CreatedBefore, &query.CreatedBefore}} {
		if field.value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, field.value)
		if err != nil {
			return nil, fmt.Errorf("%s must be an RFC 3339 time: %w", name, err)
		}
		*field.target = parsed
	}
	var matched []msg.OperationReceipt
	for {
		page, err := b.Operations.ListSucceededOperations(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("listing operations: %w", err)
		}
		for _, receipt := range page {
			if strings.HasPrefix(receipt.CorrelationID, filter.CorrelationIDPrefix) {
				matched = append(matched, receipt)
			}
		}
		if len(page) < operationListPageSize {
			return matched, nil
		}
		query.CreatedBefore = page[len(page)-1].CreatedAt
	}
}

// CountOperations counts the succeeded operations the filter matches.
func (b *BridgeOperations) CountOperations(ctx context.Context, filter OperationsFilter) (int, error) {
	receipts, err := b.ListOperations(ctx, filter)
	return len(receipts), err
}

// ErrOperationIntentsNotServed: llm-bridge-server hands out receipts but not
// the intents behind them, and a case needs the intent's prompts.
var ErrOperationIntentsNotServed = errors.New("llm-bridge-server does not hand out the intents behind its operations; importing operations needs them to read the prompts")

// ReadIntent reads the intent an operation was submitted with: the prompts
// and schema a case is made from.
func (b *BridgeOperations) ReadIntent(ctx context.Context, id string) (msg.OperationIntent, error) {
	if err := b.configured(); err != nil {
		return msg.OperationIntent{}, err
	}
	intent, err := b.Operations.ReadOperationIntent(ctx, id)
	if err != nil {
		return msg.OperationIntent{}, fmt.Errorf("reading the intent of operation %s: %w", id, err)
	}
	return intent, nil
}
