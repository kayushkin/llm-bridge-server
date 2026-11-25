// Package peoplestore is the registry of the people in one person's own life —
// friends, family, coworkers — for a journal and for messages.
//
// principal-store stays the registry of who may log in, hold a grant or be
// assigned a card. A person here may point at a principal by its id; nothing
// here copies a principal's name or email. The ids this store hands out
// (person_000001) are what everything else joins on; names ride alongside for
// display and are never a join key.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New. Every record carries the principal who keeps it
// (owner_principal_id), and every call reaches only the records of the scope
// it is given (connectedrecords/ownership).
package peoplestore

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var (
	// ErrNotFound is a 404.
	ErrNotFound = errors.New("not found")
	// ErrInvalid is a 400: the caller sent something this store refuses.
	ErrInvalid = errors.New("invalid")
	// ErrConflict is a 409: the write contradicts a row that exists.
	ErrConflict = errors.New("conflict")
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
	return postgresstorage.Module{Schema: "people", Role: "people_module", Migrations: migrations}, nil
}

// Store keeps the records in the module's pool.
type Store struct {
	db         *sql.DB
	principals PrincipalChecker
}

// New is the store on db, the module's pool from postgresstorage.Open.
// principals is asked about every principal_id written to a person.
func New(db *sql.DB, principals PrincipalChecker) *Store {
	return &Store{db: db, principals: principals}
}

func now() int64 { return time.Now().Unix() }

// The id prefixes. A prefix is not decoration: a resolver probes every entity
// type whose id pattern matches a string found in chat, and noteboard already
// claims the bare-uuid shape, so person_000001 can be claimed by a pattern of
// its own without making every uuid probe this store.
const (
	personIDPrefix       = "person"
	identityIDPrefix     = "identity"
	relationshipIDPrefix = "relationship"
	suggestionIDPrefix   = "suggestion"
	revisionIDPrefix     = "revision"
	refusalIDPrefix      = "refusal"
)

func formatID(prefix string, seq int64) string { return postgresstorage.FormatRecordID(prefix, seq) }

// numberOf reads the number out of an id of kind prefix. A malformed id names
// no record, so it is a 404, as a missing one.
func numberOf(prefix, id string) (int64, error) {
	seq, err := postgresstorage.ParseRecordID(prefix, id)
	if err != nil {
		return 0, fmt.Errorf("%w: %s %s", ErrNotFound, prefix, id)
	}
	return seq, nil
}

// formatOptionalPersonID is "" for no person, or the person's id.
func formatOptionalPersonID(seq sql.NullInt64) string {
	if !seq.Valid {
		return ""
	}
	return formatID(personIDPrefix, seq.Int64)
}

// queryer is what both *sql.DB and *sql.Tx offer for reads.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// arguments numbers the placeholders of one statement as they are added.
type arguments struct{ values []any }

func (a *arguments) add(value any) string {
	a.values = append(a.values, value)
	return "$" + strconv.Itoa(len(a.values))
}

// within is the condition that column is in scope, numbered after the
// arguments added so far.
func (a *arguments) within(scope ownership.Scope, column string) string {
	condition, values := scope.Condition(column, len(a.values)+1)
	a.values = append(a.values, values...)
	return condition
}

// checkScope refuses the zero scope, which reaches nothing: a caller that
// never set one has a bug, and must not be read as "everyone".
func checkScope(scope ownership.Scope) error {
	if err := scope.Check(); err != nil {
		return fmt.Errorf("peoplestore: %w", err)
	}
	return nil
}

// checkOwner refuses an owner that is not a principal id.
func checkOwner(ownerPrincipalID string) error {
	if err := ownership.CheckPrincipalID("owner_principal_id", ownerPrincipalID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

// lockOwner serializes the writes of one owner, as the SQLite store's
// immediate transactions serialized every write: two importers, two review
// claims or two links that read before they write never both act on the same
// state. Held until the transaction ends.
func lockOwner(ctx context.Context, tx *sql.Tx, ownerPrincipalID string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "peoplestore.owner."+ownerPrincipalID); err != nil {
		return fmt.Errorf("locking the records of %s: %w", ownerPrincipalID, err)
	}
	return nil
}

// isUniqueViolation reports a write a uniqueness rule refused: a concurrent
// write got there first.
func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

// beginForPerson starts a write on an existing person: it finds the person's
// owner within scope (404 outside it), takes the owner's lock, and answers the
// transaction and the owner. The caller rolls back or commits.
func (s *Store) beginForPerson(ctx context.Context, scope ownership.Scope, personID string) (*sql.Tx, int64, string, error) {
	if err := checkScope(scope); err != nil {
		return nil, 0, "", err
	}
	seq, err := numberOf(personIDPrefix, personID)
	if err != nil {
		return nil, 0, "", err
	}
	owner, err := ownerOfRow(ctx, s.db, scope, "persons", seq, personIDPrefix)
	if err != nil {
		return nil, 0, "", err
	}
	tx, err := s.lockedTransaction(ctx, owner)
	if err != nil {
		return nil, 0, "", err
	}
	return tx, seq, owner, nil
}

// lockedTransaction begins a transaction holding owner's lock.
func (s *Store) lockedTransaction(ctx context.Context, owner string) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := lockOwner(ctx, tx, owner); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// ownerOfRow is the owner of row seq of table, when it is in scope. The table
// name is always one of this package's constants, never caller input.
func ownerOfRow(ctx context.Context, q queryer, scope ownership.Scope, table string, seq int64, prefix string) (string, error) {
	var args arguments
	query := `SELECT owner_principal_id FROM ` + table + ` WHERE seq = ` + args.add(seq) + ` AND ` + args.within(scope, "owner_principal_id")
	var owner string
	err := q.QueryRowContext(ctx, query, args.values...).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s %s", ErrNotFound, prefix, formatID(prefix, seq))
	}
	return owner, err
}

// reindexPerson rewrites the person's identity search text from their
// identities, inside the transaction that changed them. The rest of the
// search document follows the person's own columns by itself.
func reindexPerson(ctx context.Context, tx *sql.Tx, personSeq int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE persons SET identity_search_text = COALESCE((
		SELECT string_agg(i.value || ' ' || i.label, ' ' ORDER BY i.seq) FROM identities i
		WHERE i.owner_principal_id = persons.owner_principal_id AND i.person_seq = persons.seq), '')
		WHERE seq = $1`, personSeq)
	if err != nil {
		return fmt.Errorf("reindex %s: %w", formatID(personIDPrefix, personSeq), err)
	}
	return nil
}

// recordRevision snapshots the person, identities included, as it is inside
// tx before action changes it. The revision takes the person's owner.
func recordRevision(ctx context.Context, tx *sql.Tx, personSeq int64, action string) error {
	person, err := getPerson(ctx, tx, ownership.EveryOwner(), personSeq)
	if err != nil {
		return err
	}
	snapshot := PersonSnapshot{Person: *person}
	if snapshot.Identities, err = listIdentities(ctx, tx, personSeq); err != nil {
		return err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO revisions (owner_principal_id, person_seq, action, snapshot, created_at) VALUES ($1, $2, $3, $4, $5)`,
		person.OwnerPrincipalID, personSeq, action, string(encoded), now())
	return err
}

// Revisions lists a person's revisions, oldest first.
func (s *Store) Revisions(ctx context.Context, scope ownership.Scope, personID string) ([]Revision, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	seq, err := numberOf(personIDPrefix, personID)
	if err != nil {
		return nil, err
	}
	if _, err := getPerson(ctx, s.db, scope, seq); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, owner_principal_id, person_seq, action, snapshot, created_at FROM revisions WHERE person_seq = $1 ORDER BY seq`, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []Revision{}
	for rows.Next() {
		var revision Revision
		var revisionSeq, personSeq int64
		var snapshot string
		if err := rows.Scan(&revisionSeq, &revision.OwnerPrincipalID, &personSeq, &revision.Action, &snapshot, &revision.CreatedAt); err != nil {
			return nil, err
		}
		revision.ID, revision.PersonID = formatID(revisionIDPrefix, revisionSeq), formatID(personIDPrefix, personSeq)
		if err := json.Unmarshal([]byte(snapshot), &revision.Snapshot); err != nil {
			return nil, fmt.Errorf("revision %s: %w", revision.ID, err)
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

// Counts reports the size of each table within scope, for GET /health.
func (s *Store) Counts(ctx context.Context, scope ownership.Scope) (Counts, error) {
	var counts Counts
	if err := checkScope(scope); err != nil {
		return counts, err
	}
	condition, values := scope.Condition("owner_principal_id", 1)
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM persons WHERE deleted_at = 0 AND `+condition+`),
		(SELECT COUNT(*) FROM persons WHERE deleted_at <> 0 AND `+condition+`),
		(SELECT COUNT(*) FROM identities WHERE `+condition+`),
		(SELECT COUNT(*) FROM relationships WHERE `+condition+`),
		(SELECT COUNT(*) FROM suggestions WHERE status = 'proposed' AND `+condition+`),
		(SELECT COUNT(*) FROM identity_refusals WHERE `+condition+`)`, values...).
		Scan(&counts.Persons, &counts.DeletedPersons, &counts.Identities, &counts.Relationships, &counts.ProposedSuggestions, &counts.Refusals)
	return counts, err
}
