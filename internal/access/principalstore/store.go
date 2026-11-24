// Package principalstore is the registry of principals: the humans and groups
// a permission can be granted to and a card can be assigned to.
//
// It exists because nothing on this box models a person. The operator appears
// as `dana`, `kanban` and `vlad` in three different stores with nothing
// joining them, and kanban has no assignee concept at all. This store hands out
// the id everything else joins on; display names and emails ride alongside for
// display and are never a join key.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New.
package principalstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var (
	ErrNotFound          = errors.New("not found")
	ErrInvalidPrincipal  = errors.New("invalid principal")
	ErrInvalidMembership = errors.New("invalid membership")
	ErrNotAMember        = errors.New("not a member")
)

// The type parts of the public ids this store hands out: principal_000001,
// timeoff_000001, identity_binding_000001.
const (
	PrincipalIDPrefix       = "principal"
	TimeOffIDPrefix         = "timeoff"
	IdentityBindingIDPrefix = "identity_binding"
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
	return postgresstorage.Module{Schema: "principals", Role: "principals_module", Migrations: migrations}, nil
}

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

// Store reads and writes the module's tables.
type Store struct {
	db *sql.DB
}

// New returns a store on db, the module's pool from postgresstorage.Open,
// migrated.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

func now() int64 { return time.Now().Unix() }

// formatID renders a principal's number as its public id. The prefix is not
// decoration: dash's resolver probes every registry row whose id pattern
// matches, and noteboard already claims the bare-uuid shape, so an id that
// looked like a uuid would make every uuid in every chat message probe this
// store too. The prefix also keeps '/' out of the id, which kanban-store's
// reverse lookup splits on.
func formatID(seq int64) string { return postgresstorage.FormatRecordID(PrincipalIDPrefix, seq) }

// numberOfID reads the number out of a public id of the type prefix names. Only
// the exact form FormatRecordID writes names a row: principal_0000001 is not
// principal_000001, so a caller can never reach a row through a spelling of
// its id that no store handed out.
func numberOfID(prefix, id string) (int64, bool) {
	number, err := postgresstorage.ParseRecordID(prefix, id)
	if err != nil || postgresstorage.FormatRecordID(prefix, number) != id {
		return 0, false
	}
	return number, true
}

// principalNumberOf is numberOfID for a principal id.
func principalNumberOf(id string) (int64, bool) { return numberOfID(PrincipalIDPrefix, id) }

// inTransaction runs work in one transaction.
func (s *Store) inTransaction(ctx context.Context, work func(tx *sql.Tx) error) error {
	return postgresstorage.InTransaction(ctx, s.db, work)
}
