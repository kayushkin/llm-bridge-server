// Package grants is the registry of grants: which principal (a human, a
// group, a peer or a service from principal-store) stands in which relation to
// which resource — an agent, harness instance, machine, skill, tool, board and
// the rest of GET /resource-types.
//
// It exists because "who may use what" had no home. principal-store is a
// directory, permission-store matches tool-call patterns with no subject, and
// tool-store's per-instance opt-ins say where a tool is wired, not who may
// have it. This store holds the tuples; llm-bridge-server reads a principal's
// effective set at session start and offers only what it names.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New.
package grants

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
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

// GrantIDPrefix is the type part of a grant's public id: grant_000001.
const GrantIDPrefix = "grant"

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its grants in, with
// the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "grants", Role: "grants_module", Migrations: migrations}, nil
}

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

// Store keeps the grants in the module's schema.
type Store struct {
	db *sql.DB
}

// New returns the store on db, a pool Module's schema has been migrated on.
func New(db *sql.DB) *Store { return &Store{db: db} }

// Counts answers /health.
func (s *Store) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE revoked_at = 0),
		       COUNT(*) FILTER (WHERE revoked_at <> 0),
		       COUNT(DISTINCT principal_id) FILTER (WHERE revoked_at = 0)
		FROM grants`).Scan(&c.Grants, &c.Active, &c.Revoked, &c.Principals)
	return c, err
}

func now() int64 { return time.Now().Unix() }

// formatID renders a grant's number as its public id. The prefix is not
// decoration: dash's resolver probes every registry row whose id pattern
// matches, and noteboard already claims the bare-uuid shape, so an id that
// looked like a uuid would make every uuid in every chat message probe this
// store too. The prefix also keeps '/' out of the id.
func formatID(seq int64) string { return postgresstorage.FormatRecordID(GrantIDPrefix, seq) }
