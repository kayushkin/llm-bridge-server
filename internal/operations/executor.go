// Package operations runs operations: it accepts intents, hands queued
// operations to executors under a lease, and carries each one to a terminal
// state. docs/OPERATIONS.md in llm-bridge is the contract it keeps.
//
// The coordinator owns the lifecycle — leases, bounded concurrency, retries,
// cancellation, child counts, fencing out a worker that lost its lease, and
// settling what a restart left running. An executor owns one operation type's
// work and knows nothing of the rest.
package operations

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
)

// Executor runs one operation type.
type Executor interface {
	// Describe says which type this executor runs and how the coordinator
	// should bound it.
	Describe() msg.OperationTypeDescription
	// Validate checks an intent's input before it is accepted. An error here
	// is the caller's fault and refuses the intent; nothing is stored.
	Validate(intent msg.OperationIntent) error
	// Execute does the work of one attempt. ctx ends when the attempt times
	// out, the lease is lost, or the caller cancels. Progress, evidence and
	// effects go through receipt as they happen, so a crash loses none of
	// them.
	Execute(ctx context.Context, intent msg.OperationIntent, receipt ReceiptWriter) Result
	// Reconcile decides what to do with an operation whose worker is gone,
	// from its receipt: run it again, or settle it where it is.
	Reconcile(ctx context.Context, operation msg.OperationReceipt) ReconcileResult
	// Cancel asks whatever external system the operation is waiting on to
	// stop. The coordinator has already ended the attempt's ctx; an executor
	// with nothing outside this process to stop returns nil.
	Cancel(ctx context.Context, operation msg.OperationReceipt) error
}

// Result is how one attempt ended.
type Result struct {
	// State is succeeded, failed, conflicted or unknown. Leave it empty with
	// a Retryable error to have the coordinator try again.
	State            msg.OperationState
	Result           json.RawMessage
	ResultReferences []msg.OperationReference
	Usage            *msg.OperationUsage
	Error            *msg.OperationError
}

// ReconcileAction is what to do with an operation whose worker is gone.
type ReconcileAction string

const (
	// ReconcileRunAgain queues the operation for another attempt. Choose it
	// only when a rerun cannot repeat an effect.
	ReconcileRunAgain ReconcileAction = "run_again"
	// ReconcileSettle ends the operation in ReconcileResult.State.
	ReconcileSettle ReconcileAction = "settle"
)

// ReconcileResult is an executor's decision about an abandoned operation.
type ReconcileResult struct {
	Action ReconcileAction
	State  msg.OperationState
	Error  *msg.OperationError
}

// ReceiptWriter is what an executor may change on its operation's receipt.
// Every call is fenced by the attempt's lease: after the lease is lost each
// one returns an error wrapping operationstore.ErrLeaseLost, and the executor
// should return.
type ReceiptWriter interface {
	OperationID() string
	SetProgress(progress msg.OperationProgress) error
	AddEvidence(evidence ...msg.OperationEvidence) error
	// RecordEffect writes an effect with outcome unknown, before the request
	// that makes it leaves, and returns its number.
	RecordEffect(target msg.OperationReference, action string) (effectNumber int, err error)
	// SettleEffect records what the target system said about an effect.
	SettleEffect(effectNumber int, outcome msg.OperationEffectOutcome, externalOperationID string) error
	// EffectHeaders are the correlation, operation and idempotency headers a
	// call making effectNumber must carry.
	EffectHeaders(effectNumber int) http.Header
	// StartChild queues a child operation with the same organization,
	// principal and correlation id. childKey must be unique among this
	// operation's children; starting the same child twice returns the first.
	StartChild(childType msg.OperationType, childKey string, input json.RawMessage, scope []msg.OperationReference) (childOperationID string, err error)
}

// Correlation headers a call carries to another store. See docs/OPERATIONS.md.
const (
	CorrelationIDHeader  = "X-Correlation-Id"
	OperationIDHeader    = "X-Operation-Id"
	IdempotencyKeyHeader = "Idempotency-Key"
)
