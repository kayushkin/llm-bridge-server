// Package grantstore is the registry of grants: which principal (a human or a
// group from principal-store) stands in which relation to which resource — an
// agent, harness instance, machine, skill or tool.
//
// It exists because "who may use what" had no home. principal-store is a
// directory, permission-store matches tool-call patterns with no subject, and
// tool-store's per-instance opt-ins say where a tool is wired, not who may
// have it. This store holds the tuples; llm-bridge-server reads a principal's
// effective set at session start and offers only what it names.
package grantstore

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
	ErrNotFound = errors.New("not found")

	// ErrInvalidGrant is a grant described wrongly: an unknown relation or
	// resource type, a pairing the relation does not allow, a malformed id, or
	// an id its owner says does not exist. The caller's to fix.
	ErrInvalidGrant = errors.New("invalid grant")

	// ErrPrincipalNotFound is what a PrincipalDirectory wraps when
	// principal-store answers that the principal does not exist.
	ErrPrincipalNotFound = errors.New("principal does not exist")
	// ErrResourceNotFound is what a ResourceChecker wraps when the owner
	// answers that the resource does not exist.
	ErrResourceNotFound = errors.New("resource does not exist")
	// ErrOwnerUnavailable is principal-store or a resource owner that could not
	// be asked, timed out, or answered anything but yes or no. Not the caller's
	// to fix.
	ErrOwnerUnavailable = errors.New("owner unavailable")
)

// Active reports whether the grant has not been revoked.
func (g *Grant) Active() bool { return g.RevokedAt == 0 }

// Filter selects grants. The zero value lists every active grant.
type Filter struct {
	PrincipalID    string
	Relation       string
	ResourceType   string
	ResourceID     string
	IncludeRevoked bool
	Limit          int
	Offset         int
}

// Store owns the database.
type Store struct {
	db      *sql.DB
	dataDir string
}

// DefaultDataDir is where the database lives when the env says nothing.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "grant-store")
}

// Open creates the data directory if needed, opens the database and applies the
// schema. The schema is create-only, so this is safe on every boot.
//
// _txlock=immediate makes every transaction take the write lock at BEGIN, so
// two concurrent creates cannot both read MAX(seq) and then collide on the
// UNIQUE — the second waits, then reads the first's row.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dbPath := filepath.Join(dataDir, "grant-store.db")
	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	s := &Store{db: db, dataDir: dataDir}
	if err := s.ensureColumns(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// ensureColumns is the additive migration path. A column added to a CREATE
// TABLE in schema.sql never reaches a database that already exists, so new
// columns are declared here instead, with their index (if any) created after.
func (s *Store) ensureColumns() error {
	additions := []struct{ table, column, ddl string }{
		// Nothing yet. Append here rather than editing schema.sql's CREATE TABLE.
	}
	for _, a := range additions {
		if err := s.ensureColumn(a.table, a.column, a.ddl); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", a.table, a.column, err)
		}
	}
	return nil
}

func (s *Store) ensureColumn(table, column, ddl string) error {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, ddl))
	return err
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DataDir is the directory the database lives in.
func (s *Store) DataDir() string { return s.dataDir }

// Counts answers /health.
func (s *Store) Counts() (Counts, error) {
	var c Counts
	err := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(revoked_at = 0), 0),
		       COALESCE(SUM(revoked_at <> 0), 0),
		       COUNT(DISTINCT CASE WHEN revoked_at = 0 THEN principal_id END)
		FROM grants`).Scan(&c.Grants, &c.Active, &c.Revoked, &c.Principals)
	return c, err
}

func now() int64 { return time.Now().Unix() }

// formatID renders a sequence number as the public id. The prefix is not
// decoration: dash's resolver probes every registry row whose id pattern
// matches, and noteboard already claims the bare-uuid shape, so an id that
// looked like a uuid would make every uuid in every chat message probe this
// store too. The prefix also keeps '/' out of the id.
func formatID(seq int64) string { return fmt.Sprintf("grant_%06d", seq) }
