// Package locationstore keeps where the user has been: named places, visits
// to them, trips between them, the raw location points underneath, and the
// imports that brought them in.
//
// Places, visits and trips are cited elsewhere by their ids (place_000001,
// visit_000001, trip_000001), which reference-store's list of types claims,
// so a journal timeline or a note can link one as a chip.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New. Every record carries the principal who keeps it
// (owner_principal_id), and every call reaches only the records of the scope
// it is given (connectedrecords/ownership).
package locationstore

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/locationstore/internal/geonames"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var (
	// ErrNotFound is a 404: no such record, or one outside the caller's reach.
	ErrNotFound = errors.New("not found")
	// ErrInvalid is a 400: the caller sent something this store refuses.
	ErrInvalid = errors.New("invalid")
	// ErrConflict is a 409: the write contradicts a record that exists.
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
	return postgresstorage.Module{Schema: "locations", Role: "locations_module", Migrations: migrations}, nil
}

// Store keeps the records in the module's pool.
type Store struct {
	db *sql.DB
	// geonamesDirectory holds GeoNames' town dump, which names localities and
	// matches calendar locations. It is read the first time something needs
	// a town named; a directory with no dump makes those calls a conflict.
	geonamesDirectory string
	townsMutex        sync.Mutex
	towns             *geonames.Index
}

// New is the store on db, the module's pool from postgresstorage.Open.
// geonamesDirectory is where location-name-localities downloads the GeoNames
// files.
func New(db *sql.DB, geonamesDirectory string) *Store {
	return &Store{db: db, geonamesDirectory: geonamesDirectory}
}

func now() int64 { return time.Now().Unix() }

// queryer is what both *sql.DB and *sql.Tx offer.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
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
		return fmt.Errorf("locationstore: %w", err)
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

// inOwnerTransaction runs work in one transaction holding owner's lock. The
// SQLite store serialized every write; this serializes the writes of one
// owner, which is all a write may touch, so an import batch, a segmenting run
// and a hand edit that read before they write never act on the same state at
// once. An error work returns rolls everything back and comes back unchanged.
func (s *Store) inOwnerTransaction(ctx context.Context, owner string, work func(tx *sql.Tx) error) error {
	if err := checkOwner(owner); err != nil {
		return err
	}
	return postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "locationstore.owner."+owner); err != nil {
			return fmt.Errorf("locking the records of %s: %w", owner, err)
		}
		return work(tx)
	})
}

// isUniqueViolation reports a write a uniqueness rule refused.
func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

// nextID takes the next number of a table's identity column and writes its
// id: prefix_000001. The table name is always one of this package's
// constants. A number taken by a transaction that rolls back is never handed
// out again.
func nextID(ctx context.Context, tx *sql.Tx, table, prefix string) (string, int64, error) {
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT nextval(pg_get_serial_sequence($1, 'seq'))`, table).Scan(&seq); err != nil {
		return "", 0, fmt.Errorf("numbering a new row of %s: %w", table, err)
	}
	return postgresstorage.FormatRecordID(prefix, seq), seq, nil
}

// ownerOfRecord is the owner of the record id of table, when it is in scope;
// ErrNotFound otherwise, as for a missing one. The table name is always one
// of this package's constants.
func ownerOfRecord(ctx context.Context, q queryer, scope ownership.Scope, table, label, id string) (string, error) {
	if err := checkScope(scope); err != nil {
		return "", err
	}
	var args arguments
	var owner string
	err := q.QueryRowContext(ctx, `SELECT owner_principal_id FROM `+table+` WHERE id = `+args.add(id)+` AND `+args.within(scope, "owner_principal_id"),
		args.values...).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: no %s %s", ErrNotFound, label, id)
	}
	return owner, err
}

// requireOwner refuses a record a write names when it is not owner's. One
// outside the caller's scope answers as a missing one does: missing (the
// error the write gives a record that does not exist), with field naming
// where the write named it. One the caller can reach but another owner keeps
// (an administrator naming two owners' records) is a 400.
func requireOwner(ctx context.Context, q queryer, scope ownership.Scope, owner, table, label, id string, missing error, field string) error {
	found, err := ownerOfRecord(ctx, q, scope, table, label, id)
	if errors.Is(err, ErrNotFound) {
		if field != "" {
			return fmt.Errorf("%w: %s: %v", missing, field, err)
		}
		if missing == ErrNotFound {
			return err
		}
		return fmt.Errorf("%w: %v", missing, err)
	}
	if err != nil {
		return err
	}
	if found != owner {
		return fmt.Errorf("%w: %s %s belongs to %s and the record it would join belongs to %s; records of two owners are never joined", ErrInvalid, label, id, found, owner)
	}
	return nil
}

// writeOwned finds the owner of record id of table in scope (404 outside
// it), then runs work in a transaction holding that owner's lock. A record's
// owner never changes, so finding it first is safe.
func (s *Store) writeOwned(ctx context.Context, scope ownership.Scope, table, label, id string, work func(tx *sql.Tx, owner string) error) error {
	owner, err := ownerOfRecord(ctx, s.db, scope, table, label, id)
	if err != nil {
		return err
	}
	return s.inOwnerTransaction(ctx, owner, func(tx *sql.Tx) error { return work(tx, owner) })
}

func encodeStrings(values []string) string {
	if values == nil {
		values = []string{}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func decodeStrings(encoded, what string) ([]string, error) {
	var values []string
	if err := json.Unmarshal([]byte(encoded), &values); err != nil {
		return nil, fmt.Errorf("%s has tags that are not a JSON array: %w", what, err)
	}
	return values, nil
}

func normalizeTags(tags []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return nil, fmt.Errorf("%w: a tag is empty", ErrInvalid)
		}
		if !seen[tag] {
			seen[tag] = true
			out = append(out, tag)
		}
	}
	return out, nil
}

func nullableFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func floatPointer(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

func stringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func sameFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func validLatitudeLongitude(latitude, longitude float64) bool {
	return latitude >= -90 && latitude <= 90 && longitude >= -180 && longitude <= 180
}

// checkCoordinatePair refuses half a coordinate or one out of range.
func checkCoordinatePair(what string, latitude, longitude *float64) error {
	if (latitude == nil) != (longitude == nil) {
		return fmt.Errorf("%w: %s latitude and longitude come together", ErrInvalid, what)
	}
	if latitude != nil && !validLatitudeLongitude(*latitude, *longitude) {
		return fmt.Errorf("%w: %s latitude or longitude out of range", ErrInvalid, what)
	}
	return nil
}

func checkTimeRange(startedAt, endedAt int64) error {
	if startedAt <= 0 || endedAt < startedAt {
		return fmt.Errorf("%w: started_at must be set and ended_at not before it (epoch seconds), got %d to %d", ErrInvalid, startedAt, endedAt)
	}
	return nil
}

func checkProbability(probability *float64) error {
	if probability != nil && (*probability < 0 || *probability > 1) {
		return fmt.Errorf("%w: probability must be between 0 and 1, got %v", ErrInvalid, *probability)
	}
	return nil
}

// Entity types, as revisions and the routes name them.
const (
	EntityPlace = "place"
	EntityVisit = "visit"
	EntityTrip  = "trip"
)

// entityTables is the table of each entity type.
var entityTables = map[string]string{EntityPlace: "places", EntityVisit: "visits", EntityTrip: "trips"}

// snapshot copies the whole current record into revisions before a change, in
// the change's own transaction. The revision takes the record's owner. The
// caller has already found the record in its scope.
func snapshot(ctx context.Context, tx *sql.Tx, entityType, id, reason string, at int64) error {
	var row any
	var owner string
	switch entityType {
	case EntityPlace:
		place, err := getPlaceRow(ctx, tx, ownership.EveryOwner(), id)
		if err != nil {
			return err
		}
		row, owner = place, place.OwnerPrincipalID
	case EntityVisit:
		visit, err := getVisitRow(ctx, tx, ownership.EveryOwner(), id)
		if err != nil {
			return err
		}
		row, owner = visit, visit.OwnerPrincipalID
	case EntityTrip:
		trip, err := getTripRow(ctx, tx, ownership.EveryOwner(), id)
		if err != nil {
			return err
		}
		row, owner = trip, trip.OwnerPrincipalID
	default:
		return fmt.Errorf("snapshot of unknown entity type %q", entityType)
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO revisions (owner_principal_id, entity_type, entity_id, reason, replaced_at, row_json) VALUES ($1, $2, $3, $4, $5, $6)`,
		owner, entityType, id, reason, at, string(encoded))
	return err
}

const (
	defaultLimit = 100
	maximumLimit = 1000
)

func clampLimit(limit, fallbackDefault, maximum int) int {
	if limit <= 0 {
		return fallbackDefault
	}
	return min(limit, maximum)
}

// Revisions is a place's, visit's or trip's history, newest first.
func (s *Store) Revisions(ctx context.Context, scope ownership.Scope, entityType, id string, limit, offset int) ([]Revision, error) {
	table, known := entityTables[entityType]
	if !known {
		return nil, fmt.Errorf("revisions of unknown entity type %q", entityType)
	}
	if _, err := ownerOfRecord(ctx, s.db, scope, table, entityType, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT revision_id, owner_principal_id, entity_type, entity_id, reason, replaced_at, row_json FROM revisions
		WHERE entity_id = $1 ORDER BY revision_id DESC LIMIT $2 OFFSET $3`, id, clampLimit(limit, defaultLimit, maximumLimit), max(offset, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []Revision{}
	for rows.Next() {
		var revision Revision
		var row string
		if err := rows.Scan(&revision.RevisionID, &revision.OwnerPrincipalID, &revision.EntityType, &revision.EntityID, &revision.Reason, &revision.ReplacedAt, &row); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(row), &revision.Row); err != nil {
			return nil, fmt.Errorf("revision %d of %s: %w", revision.RevisionID, id, err)
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

// Counts reports what GET /health shows, within scope.
func (s *Store) Counts(ctx context.Context, scope ownership.Scope) (Counts, error) {
	var counts Counts
	if err := checkScope(scope); err != nil {
		return counts, err
	}
	condition, values := scope.Condition("owner_principal_id", 1)
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM places WHERE deleted_at = 0 AND `+condition+`),
		(SELECT COUNT(*) FROM visits WHERE deleted_at = 0 AND `+condition+`),
		(SELECT COUNT(*) FROM trips WHERE deleted_at = 0 AND `+condition+`),
		(SELECT COUNT(*) FROM points WHERE `+condition+`),
		(SELECT COUNT(*) FROM imports WHERE `+condition+`),
		(SELECT COUNT(*) FROM places WHERE deleted_at <> 0 AND `+condition+`) + (SELECT COUNT(*) FROM visits WHERE deleted_at <> 0 AND `+condition+`) +
			(SELECT COUNT(*) FROM trips WHERE deleted_at <> 0 AND `+condition+`),
		(SELECT COUNT(*) FROM revisions WHERE `+condition+`)`, values...).
		Scan(&counts.Places, &counts.Visits, &counts.Trips, &counts.Points, &counts.Imports, &counts.Deleted, &counts.Revisions)
	return counts, err
}

// containsText is the condition that column holds text anywhere, ignoring
// the case of ASCII letters only, as SQLite's LIKE did: lower() under the C
// collation folds A to Z and nothing else. strpos matches the text itself, so
// % and _ in it are plain characters, as the escaped LIKE pattern made them.
func containsText(column string, args *arguments, text string) string {
	return `strpos(lower(` + column + ` COLLATE "C"), lower(` + args.add(text) + `::text COLLATE "C")) > 0`
}

func encodeLocality(locality *Locality) any {
	if locality == nil {
		return nil
	}
	encoded, err := json.Marshal(locality)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func decodeLocality(encoded sql.NullString, what string) (*Locality, error) {
	if !encoded.Valid {
		return nil, nil
	}
	var locality Locality
	if err := json.Unmarshal([]byte(encoded.String), &locality); err != nil {
		return nil, fmt.Errorf("%s has a locality that is not JSON: %w", what, err)
	}
	return &locality, nil
}
