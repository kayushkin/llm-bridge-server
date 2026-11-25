// Package journalstore keeps the user's own writing: journal entries, blog
// posts, notes, and notes about one person or thing.
//
// It is kept apart from noteboard on purpose. noteboard is mostly agent
// output and is open to every agent on the host; this is private writing, so
// every record has an owner, and a request reaches only its caller's own
// records unless the caller is an administrator.
//
// What an entry points at, it points at by id: a [text](ref:<id>) link in the
// body is a mention of <id>, recorded in the mentions table on every write so
// GET /mentions?ref= can answer "which entries talk about this?". The link
// text is only what was shown when it was written.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New.
package journalstore

import (
	"context"
	"database/sql"
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
	ErrNotFound     = errors.New("not found")
	ErrInvalidEntry = errors.New("invalid entry")
	// ErrConflict is a write that would break a uniqueness rule: two
	// published entries with one slug, or a checklist line already linked.
	ErrConflict = errors.New("conflict")
	// ErrForbidden is a caller this store will not let do what it asked,
	// for a reason it says (promote-todo by anyone but an administrator who
	// owns the entry).
	ErrForbidden = errors.New("forbidden")
	// ErrUpstream is a service this one calls (noteboard) failing.
	ErrUpstream = errors.New("upstream failed")
)

// EntryIDPrefix and TimelineItemIDPrefix are the type parts of the public
// ids: entry_000001, timeline_item_000001. reference-store's list of types
// claims entry ids with a pattern of their own (entry_\d{6,}), so a resolver
// probes this store only for ids that can be entries.
const (
	EntryIDPrefix        = "entry"
	TimelineItemIDPrefix = "timeline_item"
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
	return postgresstorage.Module{Schema: "journal", Role: "journal_module", Migrations: migrations}, nil
}

// Noteboard creates the todo promote-todo makes; it answers the todo's id.
// A failure wraps ErrUpstream (the route answers 502 and the entry is unchanged).
type Noteboard interface {
	CreateTodo(ctx context.Context, title, body string, tags, links []string) (string, error)
}

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

// Store keeps the entries in the module's database.
type Store struct {
	db        *sql.DB
	registry  *EntityTypeRegistry
	noteboard Noteboard
}

// New returns the store over db, the module's pool. registry types mentions;
// noteboard receives promoted todos. Neither may be nil.
func New(db *sql.DB, registry *EntityTypeRegistry, noteboard Noteboard) *Store {
	if db == nil || registry == nil || noteboard == nil {
		panic("journalstore.New: a database, an entity-type registry and a noteboard are all required")
	}
	return &Store{db: db, registry: registry, noteboard: noteboard}
}

// Registry is the entity-type registry the store types mentions with.
func (s *Store) Registry() *EntityTypeRegistry { return s.registry }

func now() int64 { return time.Now().Unix() }

// formatID renders an entry's number as its public id.
func formatID(seq int64) string { return postgresstorage.FormatRecordID(EntryIDPrefix, seq) }

// entryNumberOf reads an entry id's number. A malformed id names no entry, so
// it is not found, as a missing one is.
func entryNumberOf(id string) (int64, error) {
	seq, err := postgresstorage.ParseRecordID(EntryIDPrefix, id)
	if err != nil {
		return 0, fmt.Errorf("%w: entry %s", ErrNotFound, id)
	}
	return seq, nil
}

// queryer is what both *sql.DB and *sql.Tx offer, so a read can run inside a
// write's transaction or outside one.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// arguments numbers the placeholders of a query as its arguments are added.
type arguments struct{ values []any }

// add appends value and answers its placeholder.
func (a *arguments) add(value any) string {
	a.values = append(a.values, value)
	return "$" + strconv.Itoa(len(a.values))
}

// scope answers the condition that holds column to scope, adding its argument.
func (a *arguments) scope(scope ownership.Scope, column string) string {
	condition, values := scope.Condition(column, len(a.values)+1)
	a.values = append(a.values, values...)
	return condition
}

// inTransaction runs work in one transaction: committed when it returns nil,
// rolled back otherwise.
func (s *Store) inTransaction(ctx context.Context, work func(tx *sql.Tx) error) error {
	return postgresstorage.InTransaction(ctx, s.db, work)
}

// conflictOf turns a uniqueness violation the database caught, which the
// store's own checks lost a race to, into ErrConflict.
func conflictOf(err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "23505" {
		return fmt.Errorf("%w: %s", ErrConflict, postgresError.Message)
	}
	return err
}

func checkScope(scope ownership.Scope) error {
	if err := scope.Check(); err != nil {
		return fmt.Errorf("journalstore: %w", err)
	}
	return nil
}
