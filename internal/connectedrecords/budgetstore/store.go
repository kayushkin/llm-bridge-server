// Package budgetstore is a personal finance ledger: accounts, cards, loans,
// retirement accounts, every transaction, categories and budgets, kept by
// double entry.
//
// Every transaction holds two or more postings that sum to zero. A posting
// lands on an account (money held or owed) or on a category (where money came
// from or went), so paying a card from checking is two account postings and
// no spending, and each account's balance is the sum of its postings.
//
// The books live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New. Every row carries the principal whose books it is in
// (owner_principal_id), and each person has a ledger of their own: Store.
// OpenLedger opens one owner's books on one connection, and Postgres row level
// security narrows every statement on that connection to that owner's rows,
// so no query can reach another owner's money by leaving a condition off.
package budgetstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var (
	// ErrNotFound is a 404: no such record, or one in another owner's books.
	ErrNotFound = errors.New("not found")
	// ErrInvalid is a 400: the caller sent something this store refuses.
	ErrInvalid = errors.New("invalid")
	// ErrConflict is a 409: the write contradicts a row that exists.
	ErrConflict = errors.New("conflict")
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its books in, with
// the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "budget", Role: "budget_module", Migrations: migrations}, nil
}

// The settings each connection carries. ownerSetting is whose books the
// connection reads and writes; every row level security policy compares a
// row's owner with it. readsEveryOwnerSetting lets a read-only transaction
// find which owner keeps a record (ownerOfRecord), and nothing else.
const (
	ownerSetting           = "budget_store.owner_principal_id"
	readsEveryOwnerSetting = "budget_store.reads_every_owner"
)

// Store is budget-store on its module's pool.
type Store struct {
	db    *sql.DB
	now   func() time.Time
	plaid PlaidVault // nil: Plaid is off
}

// New is the store on db, the module's pool from postgresstorage.Open.
func New(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now}
}

// SetPlaidVault turns Plaid on. Without one every Plaid route is ErrPlaidOff.
func (s *Store) SetPlaidVault(vault PlaidVault) { s.plaid = vault }

// PlaidIsOn reports whether SetPlaidVault turned Plaid on.
func (s *Store) PlaidIsOn() bool { return s.plaid != nil }

// Ledger is one owner's books, on one connection that reads and writes only
// that owner's rows. It is for one request or one pass at a time; Close hands
// the connection back.
type Ledger struct {
	db    *ownerConnection
	owner string
	now   func() time.Time
	plaid PlaidVault
}

// OwnerPrincipalID is whose books the ledger is.
func (s *Ledger) OwnerPrincipalID() string { return s.owner }

// OpenLedger opens owner's books. A new owner's books start with the two
// categories the store posts to itself (Uncategorized and Opening balances),
// as the standalone store's did, so the first call for an owner makes them.
func (s *Store) OpenLedger(ctx context.Context, owner string) (*Ledger, error) {
	if err := ownership.CheckPrincipalID("owner_principal_id", owner); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("budget-store: taking a connection: %w", err)
	}
	connection := &ownerConnection{ctx: ctx, conn: conn, owner: owner}
	if _, err := conn.ExecContext(ctx, `SELECT set_config($1, $2, false)`, ownerSetting, owner); err != nil {
		return nil, errors.Join(fmt.Errorf("budget-store: opening the books of %s: %w", owner, err), connection.discard())
	}
	ledger := &Ledger{db: connection, owner: owner, now: s.now, plaid: s.plaid}
	if err := ledger.ensureRoleCategories(); err != nil {
		return nil, errors.Join(fmt.Errorf("budget-store: the role categories of %s: %w", owner, err), ledger.Close())
	}
	return ledger, nil
}

// Close clears the connection's owner and hands it back to the pool. A
// connection whose owner cannot be cleared is closed rather than handed back,
// so no later caller could act as this owner on it.
func (s *Ledger) Close() error {
	return s.db.release()
}

// ownerConnection is one pooled connection acting for one owner: what a
// Ledger runs every statement on. Statements outside a transaction commit on
// their own; Begin starts a transaction holding the owner's lock.
type ownerConnection struct {
	ctx   context.Context
	conn  *sql.Conn
	owner string
}

func (c *ownerConnection) Exec(query string, args ...any) (sql.Result, error) {
	return c.conn.ExecContext(c.ctx, query, args...)
}

func (c *ownerConnection) Query(query string, args ...any) (*sql.Rows, error) {
	return c.conn.QueryContext(c.ctx, query, args...)
}

func (c *ownerConnection) QueryRow(query string, args ...any) *sql.Row {
	return c.conn.QueryRowContext(c.ctx, query, args...)
}

// Begin starts a transaction holding the owner's lock. The standalone store
// took SQLite's write lock at the start of every transaction, so two writers
// never read the same state and both acted on it; this serializes the writes
// of one owner, which is all a transaction may touch.
func (c *ownerConnection) Begin() (*sql.Tx, error) {
	tx, err := c.conn.BeginTx(c.ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext($1))`, "budgetstore.owner."+c.owner); err != nil {
		return nil, errors.Join(fmt.Errorf("locking the books of %s: %w", c.owner, err), tx.Rollback())
	}
	return tx, nil
}

func (c *ownerConnection) release() error {
	// The request's context may be over; clearing the owner must still run.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.conn.ExecContext(ctx, `SELECT set_config($1, '', false)`, ownerSetting); err != nil {
		return errors.Join(fmt.Errorf("budget-store: clearing the owner of a connection: %w", err), c.discard())
	}
	return c.conn.Close()
}

// discard closes the connection for good instead of handing it back.
func (c *ownerConnection) discard() error {
	err := c.conn.Raw(func(any) error { return driver.ErrBadConn })
	if errors.Is(err, driver.ErrBadConn) {
		return nil
	}
	return err
}

// ownerOfRecord is the owner who keeps the row of table whose column is id,
// for an administrator who asks for a record by id without naming its owner.
// It reads in a read-only transaction, the one place a policy lets a read see
// every owner's rows. table and column are always this package's constants.
func (s *Store) ownerOfRecord(ctx context.Context, table, column, id string) (string, error) {
	var owner string
	err := postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SET TRANSACTION READ ONLY`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config($1, 'on', true)`, readsEveryOwnerSetting); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT owner_principal_id FROM `+table+` WHERE `+column+` = $1`, id).Scan(&owner)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("budget-store: finding who keeps %s: %w", id, err)
	}
	return owner, nil
}

func (s *Ledger) unixNow() int64 { return s.now().Unix() }

// queryer is what both a Ledger's connection and a *sql.Tx offer.
type queryer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// arguments numbers the placeholders of one statement built in parts, as
// they are added.
type arguments struct{ values []any }

func (a *arguments) add(value any) string {
	a.values = append(a.values, value)
	return "$" + strconv.Itoa(len(a.values))
}

// mintID takes the next number of table's identity column and returns it with
// its id. The table name and prefix are constants from this package, never
// caller input. Numbers are shared by every owner, so an id names one record
// wherever it is cited; a number a rolled-back transaction took is never
// handed out again.
func mintID(tx queryer, table, prefix string) (int64, string, error) {
	var seq int64
	if err := tx.QueryRow(`SELECT nextval(pg_get_serial_sequence($1, 'seq'))`, table).Scan(&seq); err != nil {
		return 0, "", fmt.Errorf("numbering a new row of %s: %w", table, err)
	}
	return seq, postgresstorage.FormatRecordID(prefix, seq), nil
}

// isUniqueViolation reports a write a uniqueness rule refused.
func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

// roleCategoryNames are the names the role categories get when first made.
// The owner may rename them; the store finds them by role, never by name.
var roleCategoryNames = map[string]struct {
	name string
	kind string
}{
	"uncategorized":    {"Uncategorized", "uncategorized"},
	"opening_balances": {"Opening balances", "equity"},
}

func (s *Ledger) ensureRoleCategories() error {
	var present int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM categories WHERE role IS NOT NULL`).Scan(&present); err != nil {
		return err
	}
	if present == len(categoryRoles) {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, role := range categoryRoles {
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM categories WHERE role = $1`, role).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		spec, found := roleCategoryNames[role]
		if !found {
			return fmt.Errorf("role %q has no starting name", role)
		}
		seq, id, err := mintID(tx, "categories", "category")
		if err != nil {
			return err
		}
		now := s.unixNow()
		if _, err := tx.Exec(`INSERT INTO categories (seq, id, name, kind, role, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			seq, id, spec.name, spec.kind, role, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func categoryIDForRole(q queryer, role string) (string, error) {
	var id string
	if err := q.QueryRow(`SELECT id FROM categories WHERE role = $1`, role).Scan(&id); err != nil {
		return "", fmt.Errorf("category with role %s: %w", role, err)
	}
	return id, nil
}
