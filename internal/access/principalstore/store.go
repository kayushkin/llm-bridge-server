// Package principalstore is the registry of principals: the humans and groups
// a permission can be granted to and a card can be assigned to.
//
// It exists because nothing on this box models a person. The operator appears
// as `slava`, `kanban` and `vlad` in three different stores with nothing
// joining them, and kanban has no assignee concept at all. This store hands out
// the id everything else joins on; display names and emails ride alongside for
// display and are never a join key.
package principalstore

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var (
	ErrNotFound          = errors.New("not found")
	ErrInvalidPrincipal  = errors.New("invalid principal")
	ErrInvalidMembership = errors.New("invalid membership")
	ErrNotAMember        = errors.New("not a member")
)

// Filter selects principals. The zero value lists every active principal.
type Filter struct {
	Kind            string
	Query           string
	IncludeDisabled bool
	Limit           int
	Offset          int
	// AvailableAt, when set, keeps only principals available at that instant.
	// It implies Kind = human, because a group has no hours of its own.
	AvailableAt *time.Time
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
	return filepath.Join(home, ".config", "principal-store")
}

// Open creates the data directory if needed, opens the database and applies the
// schema. The schema is create-only, so this is safe on every boot.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dbPath := filepath.Join(dataDir, "principal-store.db")
	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		if strings.Contains(err.Error(), "no such module: fts5") {
			return nil, fmt.Errorf(
				"migrate: %w — this binary was built without FTS5; rebuild with: go build -tags sqlite_fts5 ./cmd/principal-store", err)
		}
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
		// The declared working week, JSON of Availability; '' = none.
		{"principals", "availability", "TEXT NOT NULL DEFAULT ''"},
		// 1 = this human administers the deployment. Default 0: nobody is an
		// administrator until someone says so.
		{"principals", "is_administrator", "INTEGER NOT NULL DEFAULT 0"},
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

func now() int64 { return time.Now().Unix() }

// formatID renders a sequence number as the public id. The prefix is not
// decoration: dash's resolver probes every registry row whose id pattern
// matches, and noteboard already claims the bare-uuid shape, so an id that
// looked like a uuid would make every uuid in every chat message probe this
// store too. The prefix also keeps '/' out of the id, which kanban-store's
// reverse lookup splits on.
func formatID(seq int64) string { return fmt.Sprintf("principal_%06d", seq) }
