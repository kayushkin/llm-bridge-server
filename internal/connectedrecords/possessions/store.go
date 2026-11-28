// Package possessions keeps an inventory of the things a person owns:
// one core record per possession, a table of its own for each kind of thing
// (garments, computers, computer parts, audio devices, musical instruments,
// vehicles and the rest), products and purchases. It holds the schema and
// migrations of every record the store keeps, their ids and their owners.
// The photo review, in which two models settle what a photo shows, is
// package review beneath it, and outfits made of wearables are package
// outfits beneath that.
//
// Every record is cited elsewhere by its id (possession_000001,
// garment_000001…), which reference-store's type list claims, so a chat or a
// note can draw one as a chip.
//
// The records live in Postgres, in the schema of Module, and every row has an
// owner: principal-store's id of the person who keeps it. A Store reaches
// records only once Narrowed has chosen whose (store.go), and Postgres itself
// holds it there: each connection names one owner, or every owner for a read,
// and row-level security shows and lets it write nothing else
// (migrations/0001_…sql).
package possessions

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its records in, with
// the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: ModuleSchema, Role: "possessions_module", Migrations: migrations}, nil
}

// ModuleSchema is the module's schema, which also names its session settings.
const ModuleSchema = "possessions"

// The session settings the row-level security policies read: the one owner a
// connection reads and writes, or every_owner = on for a connection that reads
// every owner's records and writes none.
const (
	ownerSessionSetting      = ModuleSchema + ".owner_principal_id"
	everyOwnerSessionSetting = ModuleSchema + ".every_owner"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
)

// ReferenceChecker says whether an id another store owns exists among the
// records of the owner a write is for. An error that wraps ErrInvalid means
// the record does not exist there; any other error means the owning store
// could not be asked.
type ReferenceChecker interface {
	Check(ctx context.Context, ownerPrincipalID string, kind ReferenceKind, id string) error
}

// PoolOpener opens a pool on the module whose every connection starts with
// settings (OpenWithSessionSettings).
type PoolOpener func(ctx context.Context, settings map[string]string) (*sql.DB, error)

// Store owns the records of one scope: one owner's, or every owner's for a
// read. The Store New answers chooses no scope and reaches nothing; Narrowed
// answers one that does.
type Store struct {
	DB    *Database
	scope ownership.Scope
	pools *scopedPools

	references ReferenceChecker
	originals  OriginalFiles
	// WriteMutex serializes every write the store makes, across every scope,
	// as it did when the store was one SQLite file.
	WriteMutex *sync.Mutex
	now        func() time.Time
	// Budget links order lines to possessions (UseBudgetStore); nil leaves
	// order lines unlinkable.
	Budget BudgetOrders
}

// scopedPools is one pool per owner a store has acted for, and one that reads
// every owner's records, opened when first needed and kept.
type scopedPools struct {
	open       PoolOpener
	mutex      sync.Mutex
	byOwner    map[string]*Database
	everyOwner *Database
	closed     bool
}

// New is the store over the module open opens pools on. It reaches no record
// until Narrowed.
func New(open PoolOpener, references ReferenceChecker, originals OriginalFiles) (*Store, error) {
	if open == nil {
		return nil, errors.New("possession-store: no way to open the module's pools")
	}
	if references == nil {
		return nil, errors.New("possession-store: no reference checker given")
	}
	if originals == nil {
		return nil, errors.New("possession-store: nowhere to keep originals given")
	}
	return &Store{pools: &scopedPools{open: open, byOwner: map[string]*Database{}},
		references: references, originals: originals, WriteMutex: &sync.Mutex{}, now: time.Now}, nil
}

// UseBudgetStore wires budget-store, where an answer that a possession came
// from an order line is written as a link.
func (s *Store) UseBudgetStore(budget BudgetOrders) { s.Budget = budget }

// Narrowed is the store reaching scope's records: one owner's, read and
// written, or every owner's, read only.
func (s *Store) Narrowed(ctx context.Context, scope ownership.Scope) (*Store, error) {
	if err := scope.Check(); err != nil {
		return nil, err
	}
	pool, err := s.pools.of(ctx, scope)
	if err != nil {
		return nil, err
	}
	narrowed := *s
	narrowed.DB = pool
	narrowed.scope = scope
	return &narrowed, nil
}

// NarrowedToOwner is Narrowed to one owner's records.
func (s *Store) NarrowedToOwner(ctx context.Context, ownerPrincipalID string) (*Store, error) {
	return s.Narrowed(ctx, ownership.OwnedBy(ownerPrincipalID))
}

// Scope is whose records the store reaches; the zero scope on a store New
// answered.
func (s *Store) Scope() ownership.Scope { return s.scope }

// writeOwner is the owner every row this store writes belongs to. A store
// reading every owner's records writes none.
func (s *Store) writeOwner() (string, error) {
	if s.scope.IsEveryOwner() || s.scope.OwnerPrincipalID() == "" {
		return "", fmt.Errorf("%w: a write is for one owner, and this request reads every owner's records; name the owner in %s", ErrInvalid, ownership.OwnerParameter)
	}
	return s.scope.OwnerPrincipalID(), nil
}

// CheckOutside asks the store that owns id whether the write's owner has it.
func (s *Store) CheckOutside(ctx context.Context, kind ReferenceKind, id string) error {
	owner, err := s.writeOwner()
	if err != nil {
		return err
	}
	return s.references.Check(ctx, owner, kind, id)
}

func (pools *scopedPools) of(ctx context.Context, scope ownership.Scope) (*Database, error) {
	pools.mutex.Lock()
	defer pools.mutex.Unlock()
	if pools.closed {
		return nil, errors.New("possession-store: the store is closed")
	}
	if scope.IsEveryOwner() {
		if pools.everyOwner == nil {
			pool, err := pools.open(ctx, map[string]string{ownerSessionSetting: "", everyOwnerSessionSetting: "on"})
			if err != nil {
				return nil, fmt.Errorf("possession-store: opening the pool that reads every owner's records: %w", err)
			}
			pools.everyOwner = configuredPool(pool)
		}
		return pools.everyOwner, nil
	}
	owner := scope.OwnerPrincipalID()
	if pool, open := pools.byOwner[owner]; open {
		return pool, nil
	}
	pool, err := pools.open(ctx, map[string]string{ownerSessionSetting: owner, everyOwnerSessionSetting: "off"})
	if err != nil {
		return nil, fmt.Errorf("possession-store: opening the pool of %s: %w", owner, err)
	}
	pools.byOwner[owner] = configuredPool(pool)
	return pools.byOwner[owner], nil
}

// configuredPool keeps few idle connections per pool, since there is one pool
// per owner, and lets an idle one go after a while.
func configuredPool(pool *sql.DB) *Database {
	pool.SetMaxIdleConns(2)
	pool.SetConnMaxIdleTime(5 * time.Minute)
	return &Database{pool: pool}
}

// Close closes every pool the store opened, for every scope.
func (s *Store) Close() error {
	s.pools.mutex.Lock()
	defer s.pools.mutex.Unlock()
	s.pools.closed = true
	var errs []error
	if s.pools.everyOwner != nil {
		errs = append(errs, s.pools.everyOwner.pool.Close())
	}
	for _, pool := range s.pools.byOwner {
		errs = append(errs, pool.pool.Close())
	}
	return errors.Join(errs...)
}

func (s *Store) UnixNow() int64 { return s.now().Unix() }

// NextID mints the next id of a table: <prefix>_<seq, six digits or more>,
// from the table's identity, which Postgres hands out once and never again.
func NextID(transaction *DatabaseTransaction, table, prefix string) (string, int64, error) {
	var sequence int64
	if err := transaction.QueryRow(`SELECT nextval(pg_get_serial_sequence(?, 'seq'))`, table).Scan(&sequence); err != nil {
		return "", 0, fmt.Errorf("next id of %s: %w", table, err)
	}
	return postgresstorage.FormatRecordID(prefix, sequence), sequence, nil
}

// SaveRevision copies a whole row, as it stands, into revisions.
func SaveRevision(transaction *DatabaseTransaction, table, idColumn, id, changedBy string, changedAt int64) error {
	rows, err := transaction.Query(`SELECT * FROM `+table+` WHERE `+idColumn+` = ?`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s %s", ErrNotFound, table, id)
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for index := range values {
		pointers[index] = &values[index]
	}
	if err := rows.Scan(pointers...); err != nil {
		return err
	}
	record := map[string]any{"table": table}
	for index, name := range columns {
		if bytes, isBytes := values[index].([]byte); isBytes {
			record[name] = string(bytes)
		} else {
			record[name] = values[index]
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = transaction.Exec(`INSERT INTO revisions (record_id, record, changed_by, changed_at) VALUES (?, ?, ?, ?)`, id, string(encoded), changedBy, changedAt)
	return err
}

// Revision is one saved copy of a record before a change.
type Revision struct {
	ID        int64           `json:"id"`
	RecordID  string          `json:"record_id"`
	Record    json.RawMessage `json:"record"`
	ChangedBy string          `json:"changed_by"`
	ChangedAt int64           `json:"changed_at"`
}

// Revisions lists a record's saved copies, newest first.
func (s *Store) Revisions(recordID string) ([]Revision, error) {
	rows, err := s.DB.Query(`SELECT id, record_id, record, changed_by, changed_at FROM revisions WHERE record_id = ? ORDER BY id DESC`, recordID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []Revision{}
	for rows.Next() {
		var revision Revision
		var record string
		if err := rows.Scan(&revision.ID, &revision.RecordID, &record, &revision.ChangedBy, &revision.ChangedAt); err != nil {
			return nil, err
		}
		revision.Record = json.RawMessage(record)
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

// checkReference checks one id a write is about to store: this store's own
// records in the transaction, another store's through the checker.
func (s *Store) checkReference(ctx context.Context, transaction *DatabaseTransaction, kind ReferenceKind, id string) error {
	switch kind {
	case ReferenceComputer:
		return requireRow(transaction, "computers", id)
	case ReferencePurchaseLine:
		return requireRow(transaction, "purchase_lines", id)
	case ReferencePossession:
		return RequireLivePossession(transaction, id)
	case ReferenceProduct:
		return requireLiveProduct(transaction, id)
	case ReferenceMusicInstrument:
		return nil
	}
	return s.CheckOutside(ctx, kind, id)
}

func requireRow(transaction *DatabaseTransaction, table, id string) error {
	var found int
	err := transaction.QueryRow(`SELECT 1 FROM `+table+` WHERE id = ?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no %s %s", ErrInvalid, table, id)
	}
	return err
}

// ServiceReferences checks ids against the stores that own them, each asked
// about the records of the owner the write is for: photo-store,
// location-store and budget-store in this process through their
// RecordFinders, which read the owner from the context; article-store over
// HTTP; harness-store through Machines.
type ServiceReferences struct {
	Photos          RecordFinder
	Places          RecordFinder
	Transactions    RecordFinder
	ArticleStoreURL string
	HTTPClient      *http.Client
	Machines        HarnessMachines
}

// RecordFinder says whether a store holds a record among those of the owner
// the context's ownership.Caller names. A record outside them is not found,
// as a missing one.
type RecordFinder interface {
	RecordExists(ctx context.Context, id string) (bool, error)
}

// HarnessMachines answers whether harness-store has a machine.
type HarnessMachines interface {
	MachineExists(ctx context.Context, machineID string) (bool, error)
}

// Check asks the owning store whether id exists among ownerPrincipalID's
// records.
func (references ServiceReferences) Check(ctx context.Context, ownerPrincipalID string, kind ReferenceKind, id string) error {
	ctx = ownership.ContextWithCaller(ctx, ownership.Caller{PrincipalID: ownerPrincipalID})
	switch kind {
	case ReferencePhoto:
		return checkRecord(ctx, references.Photos, "photo-store", id)
	case ReferencePlace:
		return checkRecord(ctx, references.Places, "location-store", id)
	case ReferenceBudgetTransaction:
		return checkRecord(ctx, references.Transactions, "budget-store", id)
	case ReferenceArticle:
		return references.checkURL(ctx, strings.TrimRight(references.ArticleStoreURL, "/")+"/articles/"+url.PathEscape(id), "article-store", id)
	case ReferenceHarnessMachine:
		exists, err := references.Machines.MachineExists(ctx, id)
		if err != nil {
			return fmt.Errorf("read harness-store machines: %w", err)
		}
		if !exists {
			return fmt.Errorf("%w: harness-store has no machine %s", ErrInvalid, id)
		}
		return nil
	}
	return fmt.Errorf("no way to check a %s reference", kind)
}

// checkRecord asks finder whether it holds id; a record it does not hold is
// ErrInvalid, as the HTTP check's 404 was.
func checkRecord(ctx context.Context, finder RecordFinder, service, id string) error {
	if finder == nil {
		return fmt.Errorf("%s is not wired into this possession-store, so %s cannot be checked", service, id)
	}
	exists, err := finder.RecordExists(ctx, id)
	if err != nil {
		return fmt.Errorf("ask %s about %s: %w", service, id, err)
	}
	if !exists {
		return fmt.Errorf("%w: %s has no %s", ErrInvalid, service, id)
	}
	return nil
}

func (references ServiceReferences) checkURL(ctx context.Context, address, service, id string) error {
	if strings.HasPrefix(address, "/") {
		return fmt.Errorf("%s's address is not set, so %s cannot be checked", service, id)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	response, err := references.HTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("ask %s about %s: %w", service, id, err)
	}
	defer response.Body.Close()
	// Read to the end, so a store answering in this process finishes its
	// answer rather than writing into a closed pipe.
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		return fmt.Errorf("ask %s about %s: reading the answer: %w", service, id, err)
	}
	switch response.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s has no %s", ErrInvalid, service, id)
	}
	return fmt.Errorf("ask %s about %s: status %d", service, id, response.StatusCode)
}
