// Package budgetstore is the operator's personal finance ledger: accounts,
// cards, loans, retirement accounts, every transaction, categories and
// budgets, kept by double entry.
//
// Every transaction holds two or more postings that sum to zero. A posting
// lands on an account (money held or owed) or on a category (where money came
// from or went), so paying a card from checking is two account postings and
// no spending, and each account's balance is the sum of its postings.
package budgetstore

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var (
	// ErrNotFound is a 404.
	ErrNotFound = errors.New("not found")
	// ErrInvalid is a 400: the caller sent something this store refuses.
	ErrInvalid = errors.New("invalid")
	// ErrConflict is a 409: the write contradicts a row that exists.
	ErrConflict = errors.New("conflict")
)

// Store owns the database.
type Store struct {
	db      *sql.DB
	dataDir string
	now     func() time.Time
}

// Open creates the data directory if needed (mode 700: it holds finances),
// opens the database, applies the schema and makes the categories the store
// itself posts to.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("open: data directory is empty")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dbPath := filepath.Join(dataDir, "budget-store.db")
	// _txlock=immediate takes the write lock when a transaction begins, so two
	// writers cannot both read the same MAX(seq) and mint one id twice.
	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("restrict db file: %w", err)
	}
	store := &Store{db: db, dataDir: dataDir, now: time.Now}
	if err := store.ensureRoleCategories(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create role categories: %w", err)
	}
	return store, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DataDir is where the database lives.
func (s *Store) DataDir() string { return s.dataDir }

func (s *Store) unixNow() int64 { return s.now().Unix() }

func formatID(prefix string, seq int64) string { return fmt.Sprintf("%s_%06d", prefix, seq) }

// queryer is what both *sql.DB and *sql.Tx offer.
type queryer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// mintID reserves the next seq of table and returns it with its id. The table
// name and prefix are constants from this package, never caller input.
func mintID(tx queryer, table, prefix string) (int64, string, error) {
	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM ` + table).Scan(&maxSeq); err != nil {
		return 0, "", err
	}
	seq := maxSeq.Int64 + 1
	return seq, formatID(prefix, seq), nil
}

// roleCategoryNames are the names the role categories get when first made.
// The operator may rename them; the store finds them by role, never by name.
var roleCategoryNames = map[string]struct {
	name string
	kind string
}{
	"uncategorized":    {"Uncategorized", "uncategorized"},
	"opening_balances": {"Opening balances", "equity"},
}

func (s *Store) ensureRoleCategories() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, role := range categoryRoles {
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM categories WHERE role = ?`, role).Scan(&count); err != nil {
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
		if _, err := tx.Exec(`INSERT INTO categories (seq, id, name, kind, role, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
			seq, id, spec.name, spec.kind, role, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func categoryIDForRole(q queryer, role string) (string, error) {
	var id string
	if err := q.QueryRow(`SELECT id FROM categories WHERE role = ?`, role).Scan(&id); err != nil {
		return "", fmt.Errorf("category with role %s: %w", role, err)
	}
	return id, nil
}
