// Package journalstore keeps the user's own writing: journal entries, blog
// posts, notes, and notes about one person or thing.
//
// It is kept apart from noteboard on purpose. noteboard is mostly agent
// output and is open to every agent on the host; this is private writing, so
// it listens on loopback only and leaves auth to the front door (dash).
//
// What an entry points at, it points at by id: a [text](ref:<id>) link in the
// body is a mention of <id>, recorded in the mentions table on every write so
// GET /mentions?ref= can answer "which entries talk about this?". The link
// text is only what was shown when it was written.
package journalstore

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrInvalidEntry = errors.New("invalid entry")
	// ErrConflict is a write that would break a uniqueness rule: two
	// published entries with one slug, or a checklist line already linked.
	ErrConflict = errors.New("conflict")
	// ErrUpstream is a service this one calls (noteboard) failing.
	ErrUpstream = errors.New("upstream failed")
)

// Filter selects entries for GET /entries. The zero value lists live entries,
// newest written first.
type Filter struct {
	Query string
	Kind  string
	// ExcludeKinds are kinds left out (`exclude_kind`, repeatable).
	ExcludeKinds []string
	Tag          string
	Visibility   string
	AboutRef     string
	// Source is an import source: only entries POST /imports wrote under it.
	Source string
	// WrittenFrom and WrittenTo bound written_at, epoch seconds: from is
	// inclusive, to exclusive. 0 = unbounded.
	WrittenFrom    int64
	WrittenTo      int64
	IncludeDeleted bool
	Limit          int
	Offset         int
}

// Store owns the database.
type Store struct {
	db        *sql.DB
	dataDir   string
	registry  *EntityTypeRegistry
	noteboard *NoteboardClient
	// writeMutex serializes every write. The next id is MAX(seq)+1 read inside
	// the write's transaction, and promote-todo calls noteboard between
	// reading the body and rewriting it; one writer at a time keeps both
	// honest. This is one person's journal, so the cost is nothing.
	writeMutex sync.Mutex
}

// DefaultDataDir is where the database lives when the environment says nothing.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "journal-store")
}

// Open creates the data directory if needed, opens the database and applies
// the schema. registry types mentions; noteboard receives promoted todos.
// Neither may be nil.
func Open(dataDir string, registry *EntityTypeRegistry, noteboard *NoteboardClient) (*Store, error) {
	if registry == nil || noteboard == nil {
		return nil, errors.New("open store: an entity-type registry and a noteboard client are both required")
	}
	if dataDir == "" {
		return nil, errors.New("open store: no data directory given")
	}
	// 0700: this is private writing.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dbPath := filepath.Join(dataDir, "journal-store.db")
	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on&_busy_timeout=5000")
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
				"migrate: %w — this binary was built without FTS5; rebuild with: go build -tags sqlite_fts5 ./cmd/journal-store", err)
		}
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("restrict %s to its owner: %w", dbPath, err)
	}
	return &Store{db: db, dataDir: dataDir, registry: registry, noteboard: noteboard}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DataDir is the directory the database lives in.
func (s *Store) DataDir() string { return s.dataDir }

// Registry is the entity-type registry the store types mentions with.
func (s *Store) Registry() *EntityTypeRegistry { return s.registry }

func now() int64 { return time.Now().Unix() }

// formatID renders a sequence number as the public id. The prefix lets
// reference-store's list of types claim entry ids with a pattern of their own
// (entry_\d{6,}) without touching noteboard's uuids, so dash's resolver
// probes this store only for ids that can be entries.
func formatID(seq int64) string { return fmt.Sprintf("entry_%06d", seq) }

// queryer is what both *sql.DB and *sql.Tx offer, so a read can run inside a
// write's transaction or outside one.
type queryer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}
