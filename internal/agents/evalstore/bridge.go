package evalstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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

// ModelCaller makes model calls. BridgeClient is the real one; tests fake it.
type ModelCaller interface {
	Complete(ctx context.Context, request CompletionRequest) (CompletionOutcome, error)
}

// ErrBridgeBudgetRefused: the bridge refused the call for money — the
// organization's month is spent, or a model has no price under a budget.
// A run stops on it rather than failing every remaining call the same way.
var ErrBridgeBudgetRefused = errors.New("llm-bridge-server refused the call for budget")

// BridgeClient speaks to llm-bridge-server's operations API as an internal
// service, as scheduler does: the service token alone, acting as no
// principal. Each call is billed to the organization the intent names.
type BridgeClient struct {
	BaseURL      string
	ServiceToken string
	PollInterval time.Duration
	HTTP         *http.Client
}

func (b *BridgeClient) configured() error {
	switch {
	case b.BaseURL == "":
		return errors.New("EVAL_STORE_LLM_BRIDGE_URL is not set, so there is no llm-bridge-server to call")
	case b.ServiceToken == "":
		return errors.New("LLMBRIDGE_SERVICE_TOKEN is not set, so llm-bridge-server would answer 401")
	}
	return nil
}

func (b *BridgeClient) base() string { return strings.TrimRight(b.BaseURL, "/") }

func (b *BridgeClient) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, b.base()+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-LLM-Bridge-Service-Token", b.ServiceToken)
	client := b.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("read %s %s: %w", method, path, err)
	}
	return response.StatusCode, data, nil
}

// Complete submits one llm.completion operation and waits for its receipt.
// Sending the same idempotency key again returns the first operation, so a
// resumed run does not pay twice for a call it already made.
func (b *BridgeClient) Complete(ctx context.Context, request CompletionRequest) (CompletionOutcome, error) {
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
	intent, err := json.Marshal(msg.OperationIntent{Type: msg.OperationTypeLLMCompletion, OrganizationID: request.OrganizationID,
		IdempotencyKey: request.IdempotencyKey, CorrelationID: request.CorrelationID, Input: input})
	if err != nil {
		return CompletionOutcome{}, err
	}
	waitContext, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()
	status, body, err := b.do(waitContext, http.MethodPost, "/operations", intent)
	if err != nil {
		return CompletionOutcome{}, err
	}
	switch status {
	case http.StatusAccepted, http.StatusOK:
	case http.StatusPaymentRequired:
		return CompletionOutcome{}, fmt.Errorf("%w: POST /operations: %s", ErrBridgeBudgetRefused, strings.TrimSpace(string(body)))
	default:
		return CompletionOutcome{}, fmt.Errorf("POST /operations answered %d: %s", status, strings.TrimSpace(string(body)))
	}
	var receipt msg.OperationReceipt
	if err := json.Unmarshal(body, &receipt); err != nil || receipt.ID == "" {
		return CompletionOutcome{}, fmt.Errorf("POST /operations answered with something that is not a receipt: %s", strings.TrimSpace(string(body)))
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
			b.do(stopContext, http.MethodPost, "/operations/"+url.PathEscape(receipt.ID)+"/cancel", nil)
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
func (b *BridgeClient) ReadOperation(ctx context.Context, id string) (msg.OperationReceipt, error) {
	status, body, err := b.do(ctx, http.MethodGet, "/operations/"+url.PathEscape(id), nil)
	if err != nil {
		return msg.OperationReceipt{}, err
	}
	if status != http.StatusOK {
		return msg.OperationReceipt{}, fmt.Errorf("GET /operations/%s answered %d: %s", id, status, strings.TrimSpace(string(body)))
	}
	var receipt msg.OperationReceipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		return msg.OperationReceipt{}, fmt.Errorf("GET /operations/%s: not a receipt: %w", id, err)
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
// first, paging back through created_before.
func (b *BridgeClient) ListOperations(ctx context.Context, filter OperationsFilter) ([]msg.OperationReceipt, error) {
	if err := b.configured(); err != nil {
		return nil, err
	}
	var matched []msg.OperationReceipt
	before := filter.CreatedBefore
	for {
		query := url.Values{"state": {string(msg.OperationStateSucceeded)}, "limit": {strconv.Itoa(operationListPageSize)}}
		for name, value := range map[string]string{"type": filter.Type, "organization_id": filter.OrganizationID, "principal_id": filter.PrincipalID,
			"created_after": filter.CreatedAfter, "created_before": before} {
			if value != "" {
				query.Set(name, value)
			}
		}
		status, body, err := b.do(ctx, http.MethodGet, "/operations?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("GET /operations answered %d: %s", status, strings.TrimSpace(string(body)))
		}
		var page []msg.OperationReceipt
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("GET /operations: not a list of receipts: %w", err)
		}
		for _, receipt := range page {
			if strings.HasPrefix(receipt.CorrelationID, filter.CorrelationIDPrefix) {
				matched = append(matched, receipt)
			}
		}
		if len(page) < operationListPageSize {
			return matched, nil
		}
		before = page[len(page)-1].CreatedAt.Format(time.RFC3339Nano)
	}
}

// CountOperations counts the succeeded operations the filter matches.
func (b *BridgeClient) CountOperations(ctx context.Context, filter OperationsFilter) (int, error) {
	receipts, err := b.ListOperations(ctx, filter)
	return len(receipts), err
}

// ErrIntentRouteMissing: llm-bridge-server serves receipts but not the
// intents behind them, and a case needs the intent's prompts.
var ErrIntentRouteMissing = errors.New("llm-bridge-server has no GET /operations/{id}/intent route; importing operations needs it to read the prompts")

// ReadIntent reads the intent an operation was submitted with: the prompts
// and schema a case is made from.
func (b *BridgeClient) ReadIntent(ctx context.Context, id string) (msg.OperationIntent, error) {
	if err := b.configured(); err != nil {
		return msg.OperationIntent{}, err
	}
	status, body, err := b.do(ctx, http.MethodGet, "/operations/"+url.PathEscape(id)+"/intent", nil)
	if err != nil {
		return msg.OperationIntent{}, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusForbidden:
		var refusal struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		json.Unmarshal(body, &refusal)
		if status != http.StatusNotFound || refusal.Error.Code == "" {
			return msg.OperationIntent{}, fmt.Errorf("%w (answered %d: %s)", ErrIntentRouteMissing, status, strings.TrimSpace(string(body)))
		}
		return msg.OperationIntent{}, fmt.Errorf("GET /operations/%s/intent answered %d: %s", id, status, strings.TrimSpace(string(body)))
	default:
		return msg.OperationIntent{}, fmt.Errorf("GET /operations/%s/intent answered %d: %s", id, status, strings.TrimSpace(string(body)))
	}
	var intent msg.OperationIntent
	if err := json.Unmarshal(body, &intent); err != nil {
		return msg.OperationIntent{}, fmt.Errorf("GET /operations/%s/intent: not an intent: %w", id, err)
	}
	return intent, nil
}
