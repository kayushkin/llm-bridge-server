// Package operationstore persists operations: each one's intent, its current
// receipt, the events that changed it, and the lease a worker holds on it.
//
// It keeps its own SQLite file, apart from the session database, so the
// coordinator's writes never queue behind session writes and the store can
// move out of this server without a data migration.
//
// Every change to a receipt goes through one transaction that raises the
// receipt's revision by one and writes one event whose sequence is that
// revision. A reader can therefore resume a stream from any revision, and
// nothing changes a receipt without an event saying so.
package operationstore

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite"
)

var (
	// ErrNotFound: no operation has that id.
	ErrNotFound = errors.New("operation not found")
	// ErrIdempotencyKeyReused: the key was already used for a different intent.
	ErrIdempotencyKeyReused = errors.New("idempotency key reused with a different intent")
	// ErrLeaseLost: the caller's lease token is not the operation's current
	// one — another worker holds it now, or the operation was requeued.
	ErrLeaseLost = errors.New("lease lost")
	// ErrTerminal: the operation has finished and its receipt no longer changes.
	ErrTerminal = errors.New("operation already finished")
)

// Store is one operations database.
type Store struct {
	db       *sql.DB
	notifier func(msg.OperationEvent)
}

// Operation is a stored operation: its intent, its receipt, and the lease
// fields only the coordinator reads.
type Operation struct {
	Intent  msg.OperationIntent
	Receipt msg.OperationReceipt
	// LeaseOwner is the worker process holding the operation while running.
	LeaseOwner string
	// LeaseToken rises every time a worker claims the operation. A write that
	// carries an older token is refused, which is how a worker that lost its
	// lease is kept from overwriting the one that took it over.
	LeaseToken     int64
	LeaseExpiresAt time.Time
}

// Open opens or creates the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create operations database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open operations database %s: %w", path, err)
	}
	// One connection: every write is a short transaction, and SQLite allows
	// one writer at a time anyway. A single connection makes that queue
	// visible in Go rather than as busy errors.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL on %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create operations schema in %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SetNotifier registers a function called with every event after its
// transaction commits. It must not block.
func (s *Store) SetNotifier(notify func(msg.OperationEvent)) { s.notifier = notify }

const schema = `
CREATE TABLE IF NOT EXISTS operations (
	id                   TEXT PRIMARY KEY,
	type                 TEXT NOT NULL,
	organization_id      TEXT NOT NULL,
	principal_id         TEXT NOT NULL,
	idempotency_key      TEXT NOT NULL,
	intent_fingerprint   TEXT NOT NULL,
	parent_operation_id  TEXT NOT NULL,
	state                TEXT NOT NULL,
	revision             INTEGER NOT NULL,
	intent_json          TEXT NOT NULL,
	receipt_json         TEXT NOT NULL,
	lease_owner          TEXT NOT NULL DEFAULT '',
	lease_token          INTEGER NOT NULL DEFAULT 0,
	lease_expires_at     INTEGER NOT NULL DEFAULT 0,
	created_at           INTEGER NOT NULL,
	updated_at           INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS operations_idempotency
	ON operations(principal_id, organization_id, type, idempotency_key);
CREATE INDEX IF NOT EXISTS operations_state_created ON operations(state, created_at);
CREATE INDEX IF NOT EXISTS operations_parent ON operations(parent_operation_id) WHERE parent_operation_id != '';
CREATE INDEX IF NOT EXISTS operations_organization_created ON operations(organization_id, created_at);
CREATE TABLE IF NOT EXISTS operation_events (
	operation_id TEXT NOT NULL REFERENCES operations(id),
	sequence     INTEGER NOT NULL,
	kind         TEXT NOT NULL,
	event_json   TEXT NOT NULL,
	at           INTEGER NOT NULL,
	PRIMARY KEY (operation_id, sequence)
);
`

// NewOperationID returns a fresh operation_<ULID>.
func NewOperationID(now time.Time) string {
	return "operation_" + ulid.MustNew(ulid.Timestamp(now), rand.Reader).String()
}

// Create stores a new queued operation for intent, or returns the one already
// stored under the same idempotency key. created is false for the second.
// A stored operation whose fingerprint differs is ErrIdempotencyKeyReused.
func (s *Store) Create(intent msg.OperationIntent, fingerprint string, now time.Time) (receipt msg.OperationReceipt, created bool, err error) {
	var events []msg.OperationEvent
	err = s.inTransaction(func(tx *sql.Tx) error {
		existing, found, err := findByIdempotencyKey(tx, intent)
		if err != nil {
			return err
		}
		if found {
			if existing.fingerprint != fingerprint {
				return fmt.Errorf("%w: key %q already names operation %s", ErrIdempotencyKeyReused, intent.IdempotencyKey, existing.operation.Receipt.ID)
			}
			receipt = existing.operation.Receipt
			return nil
		}
		operationID := NewOperationID(now)
		if intent.CorrelationID == "" {
			intent.CorrelationID = operationID
		}
		receipt = msg.OperationReceipt{
			ID:                operationID,
			Type:              intent.Type,
			OrganizationID:    intent.OrganizationID,
			PrincipalID:       intent.PrincipalID,
			IdempotencyKey:    intent.IdempotencyKey,
			CorrelationID:     intent.CorrelationID,
			ParentOperationID: intent.ParentOperationID,
			State:             msg.OperationStateQueued,
			Revision:          1,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		intentJSON, err := json.Marshal(intent)
		if err != nil {
			return fmt.Errorf("encode intent: %w", err)
		}
		receiptJSON, err := json.Marshal(receipt)
		if err != nil {
			return fmt.Errorf("encode receipt: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO operations (id, type, organization_id, principal_id, idempotency_key,
			intent_fingerprint, parent_operation_id, state, revision, intent_json, receipt_json, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			receipt.ID, receipt.Type, receipt.OrganizationID, receipt.PrincipalID, receipt.IdempotencyKey,
			fingerprint, receipt.ParentOperationID, receipt.State, receipt.Revision, intentJSON, receiptJSON,
			now.UnixNano(), now.UnixNano()); err != nil {
			return fmt.Errorf("insert operation: %w", err)
		}
		event, err := insertEvent(tx, receipt, msg.OperationEventAccepted, "", now)
		if err != nil {
			return err
		}
		events = append(events, event)
		created = true
		if receipt.ParentOperationID != "" {
			parentEvent, err := refreshParentAfterChildChange(tx, receipt.ParentOperationID, receipt.ID, now)
			if err != nil {
				return err
			}
			events = append(events, parentEvent)
		}
		return nil
	})
	if err != nil {
		return msg.OperationReceipt{}, false, err
	}
	s.notify(events)
	return receipt, created, nil
}

type storedIdempotencyMatch struct {
	operation   Operation
	fingerprint string
}

func findByIdempotencyKey(tx *sql.Tx, intent msg.OperationIntent) (storedIdempotencyMatch, bool, error) {
	row := tx.QueryRow(`SELECT `+operationColumns+`, intent_fingerprint FROM operations
		WHERE principal_id = ? AND organization_id = ? AND type = ? AND idempotency_key = ?`,
		intent.PrincipalID, intent.OrganizationID, intent.Type, intent.IdempotencyKey)
	var match storedIdempotencyMatch
	operation, err := scanOperation(row, &match.fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return match, false, nil
	}
	if err != nil {
		return match, false, err
	}
	match.operation = operation
	return match, true, nil
}

const operationColumns = `intent_json, receipt_json, lease_owner, lease_token, lease_expires_at`

func scanOperation(row interface{ Scan(...any) error }, extra ...any) (Operation, error) {
	var intentJSON, receiptJSON string
	var leaseExpiresAt int64
	var operation Operation
	destinations := append([]any{&intentJSON, &receiptJSON, &operation.LeaseOwner, &operation.LeaseToken, &leaseExpiresAt}, extra...)
	if err := row.Scan(destinations...); err != nil {
		return Operation{}, err
	}
	if err := json.Unmarshal([]byte(intentJSON), &operation.Intent); err != nil {
		return Operation{}, fmt.Errorf("decode stored intent: %w", err)
	}
	if err := json.Unmarshal([]byte(receiptJSON), &operation.Receipt); err != nil {
		return Operation{}, fmt.Errorf("decode stored receipt: %w", err)
	}
	if leaseExpiresAt != 0 {
		operation.LeaseExpiresAt = time.Unix(0, leaseExpiresAt)
	}
	return operation, nil
}

// Get returns one operation.
func (s *Store) Get(operationID string) (Operation, error) {
	operation, err := scanOperation(s.db.QueryRow(`SELECT `+operationColumns+` FROM operations WHERE id = ?`, operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, fmt.Errorf("%w: %s", ErrNotFound, operationID)
	}
	return operation, err
}

// Filter narrows List. A zero field does not narrow.
type Filter struct {
	OrganizationID    string
	PrincipalID       string
	ParentOperationID string
	Type              msg.OperationType
	State             msg.OperationState
	CreatedAfter      time.Time
	CreatedBefore     time.Time
	// Limit caps the rows returned; List requires it.
	Limit int
}

// List returns receipts matching filter, newest first.
func (s *Store) List(filter Filter) ([]msg.OperationReceipt, error) {
	if filter.Limit <= 0 {
		return nil, fmt.Errorf("list operations: limit must be positive, got %d", filter.Limit)
	}
	var conditions []string
	var arguments []any
	add := func(condition string, value any) {
		conditions = append(conditions, condition)
		arguments = append(arguments, value)
	}
	if filter.OrganizationID != "" {
		add("organization_id = ?", filter.OrganizationID)
	}
	if filter.PrincipalID != "" {
		add("principal_id = ?", filter.PrincipalID)
	}
	if filter.ParentOperationID != "" {
		add("parent_operation_id = ?", filter.ParentOperationID)
	}
	if filter.Type != "" {
		add("type = ?", filter.Type)
	}
	if filter.State != "" {
		add("state = ?", filter.State)
	}
	if !filter.CreatedAfter.IsZero() {
		add("created_at > ?", filter.CreatedAfter.UnixNano())
	}
	if !filter.CreatedBefore.IsZero() {
		add("created_at < ?", filter.CreatedBefore.UnixNano())
	}
	query := `SELECT receipt_json FROM operations`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	arguments = append(arguments, filter.Limit)
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list operations: %w", err)
	}
	defer rows.Close()
	receipts := []msg.OperationReceipt{}
	for rows.Next() {
		var receiptJSON string
		if err := rows.Scan(&receiptJSON); err != nil {
			return nil, err
		}
		var receipt msg.OperationReceipt
		if err := json.Unmarshal([]byte(receiptJSON), &receipt); err != nil {
			return nil, fmt.Errorf("decode stored receipt: %w", err)
		}
		receipts = append(receipts, receipt)
	}
	return receipts, rows.Err()
}

// EventsAfter returns an operation's events with sequence above afterSequence,
// in order.
func (s *Store) EventsAfter(operationID string, afterSequence int64) ([]msg.OperationEvent, error) {
	rows, err := s.db.Query(`SELECT event_json FROM operation_events WHERE operation_id = ? AND sequence > ? ORDER BY sequence`,
		operationID, afterSequence)
	if err != nil {
		return nil, fmt.Errorf("read events of %s: %w", operationID, err)
	}
	defer rows.Close()
	events := []msg.OperationEvent{}
	for rows.Next() {
		var eventJSON string
		if err := rows.Scan(&eventJSON); err != nil {
			return nil, err
		}
		var event msg.OperationEvent
		if err := json.Unmarshal([]byte(eventJSON), &event); err != nil {
			return nil, fmt.Errorf("decode stored event: %w", err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// Claim takes the oldest queued operation for a worker: running, attempt
// raised by one, a new lease token. found is false when nothing is queued.
func (s *Store) Claim(leaseOwner string, leaseDuration time.Duration, now time.Time) (operation Operation, found bool, err error) {
	var events []msg.OperationEvent
	err = s.inTransaction(func(tx *sql.Tx) error {
		row := tx.QueryRow(`SELECT ` + operationColumns + ` FROM operations WHERE state = 'queued' ORDER BY created_at, id LIMIT 1`)
		operation, err = scanOperation(row)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		operation.LeaseOwner = leaseOwner
		operation.LeaseToken++
		operation.LeaseExpiresAt = now.Add(leaseDuration)
		receipt := &operation.Receipt
		receipt.State = msg.OperationStateRunning
		receipt.Attempt++
		if receipt.StartedAt == nil {
			startedAt := now
			receipt.StartedAt = &startedAt
		}
		events, err = writeReceiptChange(tx, &operation, msg.OperationEventStarted, "", now)
		return err
	})
	if err != nil {
		return Operation{}, false, err
	}
	s.notify(events)
	return operation, found, nil
}

// RenewLease extends a held lease. It changes no receipt and writes no event.
func (s *Store) RenewLease(operationID string, leaseToken int64, expiresAt time.Time) error {
	result, err := s.db.Exec(`UPDATE operations SET lease_expires_at = ? WHERE id = ? AND lease_token = ? AND state = 'running'`,
		expiresAt.UnixNano(), operationID, leaseToken)
	if err != nil {
		return fmt.Errorf("renew lease on %s: %w", operationID, err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return fmt.Errorf("%w: %s", ErrLeaseLost, operationID)
	}
	return nil
}

// RunningNotHeldBy returns running operations whose lease is held by another
// owner or has expired: the ones a restart or a dead worker left behind.
func (s *Store) RunningNotHeldBy(leaseOwner string, now time.Time) ([]Operation, error) {
	rows, err := s.db.Query(`SELECT `+operationColumns+` FROM operations
		WHERE state = 'running' AND (lease_owner != ? OR lease_expires_at < ?) ORDER BY created_at`,
		leaseOwner, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("list abandoned operations: %w", err)
	}
	defer rows.Close()
	var operations []Operation
	for rows.Next() {
		operation, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	return operations, rows.Err()
}

// Change is one mutation of a receipt, applied under the lease token given to
// Update. It returns the event kind that describes the change.
type Change func(operation *Operation) (msg.OperationEventKind, error)

// AnyLeaseToken passed to Update skips the lease check. Only the coordinator
// uses it, for changes no worker makes: cancelling a queued operation, and
// settling one whose worker is gone.
const AnyLeaseToken int64 = -1

// Update applies change to an operation that has not finished. leaseToken
// must be the operation's current token unless it is AnyLeaseToken. A change
// that leaves the operation terminal releases the lease.
func (s *Store) Update(operationID string, leaseToken int64, now time.Time, change Change) (msg.OperationReceipt, error) {
	var events []msg.OperationEvent
	var receipt msg.OperationReceipt
	err := s.inTransaction(func(tx *sql.Tx) error {
		operation, err := scanOperation(tx.QueryRow(`SELECT `+operationColumns+` FROM operations WHERE id = ?`, operationID))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, operationID)
		}
		if err != nil {
			return err
		}
		if operation.Receipt.State.IsTerminal() {
			return fmt.Errorf("%w: %s is %s", ErrTerminal, operationID, operation.Receipt.State)
		}
		if leaseToken != AnyLeaseToken && operation.LeaseToken != leaseToken {
			return fmt.Errorf("%w: %s is on lease %d, not %d", ErrLeaseLost, operationID, operation.LeaseToken, leaseToken)
		}
		kind, err := change(&operation)
		if err != nil {
			return err
		}
		if !operation.Receipt.State.IsKnown() {
			return fmt.Errorf("change left %s in unknown state %q", operationID, operation.Receipt.State)
		}
		if operation.Receipt.State.IsTerminal() || operation.Receipt.State == msg.OperationStateQueued {
			// Releasing the lease also retires its token, so a worker still
			// running under it finds every later write refused.
			operation.LeaseOwner = ""
			operation.LeaseExpiresAt = time.Time{}
			operation.LeaseToken++
		}
		if operation.Receipt.State.IsTerminal() && operation.Receipt.CompletedAt == nil {
			completedAt := now
			operation.Receipt.CompletedAt = &completedAt
		}
		events, err = writeReceiptChange(tx, &operation, kind, "", now)
		receipt = operation.Receipt
		return err
	})
	if err != nil {
		return msg.OperationReceipt{}, err
	}
	s.notify(events)
	return receipt, nil
}

// writeReceiptChange raises the revision, writes the row and its event, and
// tells the parent when this is a child. It returns every event written.
func writeReceiptChange(tx *sql.Tx, operation *Operation, kind msg.OperationEventKind, childOperationID string, now time.Time) ([]msg.OperationEvent, error) {
	receipt := &operation.Receipt
	receipt.Revision++
	receipt.UpdatedAt = now
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		return nil, fmt.Errorf("encode receipt: %w", err)
	}
	var leaseExpiresAt int64
	if !operation.LeaseExpiresAt.IsZero() {
		leaseExpiresAt = operation.LeaseExpiresAt.UnixNano()
	}
	if _, err := tx.Exec(`UPDATE operations SET state = ?, revision = ?, receipt_json = ?, lease_owner = ?,
		lease_token = ?, lease_expires_at = ?, updated_at = ? WHERE id = ?`,
		receipt.State, receipt.Revision, receiptJSON, operation.LeaseOwner, operation.LeaseToken, leaseExpiresAt,
		now.UnixNano(), receipt.ID); err != nil {
		return nil, fmt.Errorf("update operation %s: %w", receipt.ID, err)
	}
	event, err := insertEvent(tx, *receipt, kind, childOperationID, now)
	if err != nil {
		return nil, err
	}
	events := []msg.OperationEvent{event}
	if receipt.ParentOperationID != "" {
		parentEvent, err := refreshParentAfterChildChange(tx, receipt.ParentOperationID, receipt.ID, now)
		if err != nil {
			return nil, err
		}
		events = append(events, parentEvent)
	}
	return events, nil
}

// refreshParentAfterChildChange recounts a parent's children and records the
// change on the parent as child_updated. A finished parent still gets the
// count: a child may outlive the executor that started it.
func refreshParentAfterChildChange(tx *sql.Tx, parentOperationID, childOperationID string, now time.Time) (msg.OperationEvent, error) {
	parent, err := scanOperation(tx.QueryRow(`SELECT `+operationColumns+` FROM operations WHERE id = ?`, parentOperationID))
	if err != nil {
		return msg.OperationEvent{}, fmt.Errorf("read parent %s of %s: %w", parentOperationID, childOperationID, err)
	}
	rows, err := tx.Query(`SELECT id, state FROM operations WHERE parent_operation_id = ? ORDER BY created_at, id`, parentOperationID)
	if err != nil {
		return msg.OperationEvent{}, fmt.Errorf("count children of %s: %w", parentOperationID, err)
	}
	summary := &msg.OperationChildrenSummary{ByState: map[msg.OperationState]int{}}
	var childIDs []string
	for rows.Next() {
		var id string
		var state msg.OperationState
		if err := rows.Scan(&id, &state); err != nil {
			rows.Close()
			return msg.OperationEvent{}, err
		}
		childIDs = append(childIDs, id)
		summary.Total++
		summary.ByState[state]++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return msg.OperationEvent{}, err
	}
	parent.Receipt.ChildOperationIDs = childIDs
	parent.Receipt.Children = summary
	parent.Receipt.Revision++
	parent.Receipt.UpdatedAt = now
	receiptJSON, err := json.Marshal(parent.Receipt)
	if err != nil {
		return msg.OperationEvent{}, fmt.Errorf("encode receipt: %w", err)
	}
	if _, err := tx.Exec(`UPDATE operations SET revision = ?, receipt_json = ?, updated_at = ? WHERE id = ?`,
		parent.Receipt.Revision, receiptJSON, now.UnixNano(), parentOperationID); err != nil {
		return msg.OperationEvent{}, fmt.Errorf("update parent %s: %w", parentOperationID, err)
	}
	return insertEvent(tx, parent.Receipt, msg.OperationEventChildUpdated, childOperationID, now)
}

func insertEvent(tx *sql.Tx, receipt msg.OperationReceipt, kind msg.OperationEventKind, childOperationID string, now time.Time) (msg.OperationEvent, error) {
	event := msg.OperationEvent{
		OperationID:      receipt.ID,
		Sequence:         receipt.Revision,
		Kind:             kind,
		ChildOperationID: childOperationID,
		Receipt:          receipt,
		At:               now,
	}
	eventJSON, err := json.Marshal(event)
	if err != nil {
		return msg.OperationEvent{}, fmt.Errorf("encode event: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operation_events (operation_id, sequence, kind, event_json, at) VALUES (?, ?, ?, ?, ?)`,
		event.OperationID, event.Sequence, event.Kind, eventJSON, now.UnixNano()); err != nil {
		return msg.OperationEvent{}, fmt.Errorf("insert event %d of %s: %w", event.Sequence, event.OperationID, err)
	}
	return event, nil
}

func (s *Store) inTransaction(work func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin operations transaction: %w", err)
	}
	if err := work(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit operations transaction: %w", err)
	}
	return nil
}

func (s *Store) notify(events []msg.OperationEvent) {
	if s.notifier == nil {
		return
	}
	for _, event := range events {
		s.notifier(event)
	}
}
