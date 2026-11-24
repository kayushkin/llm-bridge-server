// Package peoplestore is the registry of the people in the operator's own
// life — friends, family, coworkers — for a journal and for messages.
//
// principal-store stays the registry of who may log in, hold a grant or be
// assigned a card. A person here may point at a principal by its id; nothing
// here copies a principal's name or email. The ids this store hands out
// (person_000001) are what everything else joins on; names ride alongside for
// display and are never a join key.
package peoplestore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	db         *sql.DB
	dataDir    string
	principals PrincipalChecker
}

// DefaultDataDir is where the database lives when nothing says otherwise.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "people-store")
}

// Open creates the data directory if needed, opens the database and applies
// the schema, which is create-only and so safe on every boot. principals is
// asked about every principal_id written.
func Open(dataDir string, principals PrincipalChecker) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("open: data directory is empty")
	}
	if principals == nil {
		return nil, errors.New("open: no principal checker")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dbPath := filepath.Join(dataDir, "people-store.db")
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
		if strings.Contains(err.Error(), "no such module: fts5") {
			return nil, fmt.Errorf("migrate: %w — this binary was built without FTS5; rebuild with: go build -tags sqlite_fts5 ./cmd/people-store", err)
		}
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, dataDir: dataDir, principals: principals}, nil
}

// identityProvenanceBackfillReason is the reason the migration gives the
// identities that existed before identities recorded who linked them.
const identityProvenanceBackfillReason = "linked before people-store recorded who linked a handle (2026-10-01)"

// migrate makes the additive changes schema.sql cannot, since an edited
// CREATE TABLE does nothing to a database that already exists. Each step
// checks before it acts, so it is safe on every boot.
func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	columns := map[string]bool{}
	rows, err := tx.Query(`SELECT name FROM pragma_table_info('identities')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	rows.Close()
	if !columns["linked_by"] {
		// Rows from before provenance get source import and a reason that says
		// so, rather than a source nobody recorded.
		for _, statement := range []string{
			`ALTER TABLE identities ADD COLUMN linked_by TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE identities ADD COLUMN reason TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE identities ADD COLUMN linked_at INTEGER NOT NULL DEFAULT 0`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("%s: %w", statement, err)
			}
		}
		if _, err := tx.Exec(`UPDATE identities SET linked_by = ?, reason = ?, linked_at = created_at`, LinkSourceImport, identityProvenanceBackfillReason); err != nil {
			return err
		}
	}
	// suggestion_identities held one row per handle across all suggestions;
	// suggestion_handles holds one per suggestion and handle. Copy once.
	var handles, legacy int
	if err := tx.QueryRow(`SELECT (SELECT COUNT(*) FROM suggestion_handles), (SELECT COUNT(*) FROM suggestion_identities)`).Scan(&handles, &legacy); err != nil {
		return err
	}
	if handles == 0 && legacy > 0 {
		if _, err := tx.Exec(`INSERT INTO suggestion_handles (suggestion_id, kind, value) SELECT suggestion_id, kind, value FROM suggestion_identities`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DataDir is the directory the database lives in.
func (s *Store) DataDir() string { return s.dataDir }

func now() int64 { return time.Now().Unix() }

// The id prefixes. A prefix is not decoration: dash's resolver probes every
// entity type whose id pattern matches a string found in chat, and noteboard
// already claims the bare-uuid shape, so person_000001 can be claimed by a
// pattern of its own without making every uuid probe this store.
const (
	personIDPrefix       = "person_"
	identityIDPrefix     = "identity_"
	relationshipIDPrefix = "relationship_"
	suggestionIDPrefix   = "suggestion_"
	revisionIDPrefix     = "revision_"
	refusalIDPrefix      = "refusal_"
)

func formatID(prefix string, seq int64) string { return fmt.Sprintf("%s%06d", prefix, seq) }

// nextSeq reads the next sequence number of table inside tx. The table name is
// always one of this file's constants, never caller input.
func nextSeq(tx *sql.Tx, table string) (int64, error) {
	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM ` + table).Scan(&maxSeq); err != nil {
		return 0, err
	}
	return maxSeq.Int64 + 1, nil
}

// queryer is what both *sql.DB and *sql.Tx offer for reads.
type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

// reindexPerson rewrites the person's search row from the tables, inside the
// transaction that changed them.
func reindexPerson(tx *sql.Tx, personID string) error {
	var seq int64
	var displayName, howMet, tags string
	err := tx.QueryRow(`SELECT seq, display_name, how_met, tags FROM persons WHERE id = ?`, personID).Scan(&seq, &displayName, &howMet, &tags)
	if err != nil {
		return fmt.Errorf("reindex %s: %w", personID, err)
	}
	var tagList []string
	if err := json.Unmarshal([]byte(tags), &tagList); err != nil {
		return fmt.Errorf("reindex %s: tags: %w", personID, err)
	}
	rows, err := tx.Query(`SELECT value, label FROM identities WHERE person_id = ? ORDER BY seq`, personID)
	if err != nil {
		return err
	}
	var identityText []string
	for rows.Next() {
		var value, label string
		if err := rows.Scan(&value, &label); err != nil {
			rows.Close()
			return err
		}
		identityText = append(identityText, value, label)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM persons_fts WHERE rowid = ?`, seq); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO persons_fts(rowid, display_name, how_met, tags, identities) VALUES (?,?,?,?,?)`,
		seq, displayName, howMet, strings.Join(tagList, " "), strings.Join(identityText, " "))
	return err
}

// recordRevision snapshots the person, identities included, as it is inside
// tx before action changes it.
func recordRevision(tx *sql.Tx, personID, action string) error {
	person, err := getPerson(tx, personID)
	if err != nil {
		return err
	}
	snapshot := PersonSnapshot{Person: *person}
	if snapshot.Identities, err = listIdentities(tx, personID); err != nil {
		return err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	seq, err := nextSeq(tx, "revisions")
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO revisions (id, seq, person_id, action, snapshot, created_at) VALUES (?,?,?,?,?,?)`,
		formatID(revisionIDPrefix, seq), seq, personID, action, string(encoded), now())
	return err
}

// Revisions lists a person's revisions, oldest first.
func (s *Store) Revisions(personID string) ([]Revision, error) {
	if _, err := getPerson(s.db, personID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id, person_id, action, snapshot, created_at FROM revisions WHERE person_id = ? ORDER BY seq`, personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []Revision{}
	for rows.Next() {
		var revision Revision
		var snapshot string
		if err := rows.Scan(&revision.ID, &revision.PersonID, &revision.Action, &snapshot, &revision.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(snapshot), &revision.Snapshot); err != nil {
			return nil, fmt.Errorf("revision %s: %w", revision.ID, err)
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

// Counts reports the size of each table, for GET /health.
func (s *Store) Counts() (Counts, error) {
	var counts Counts
	err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM persons WHERE deleted_at = 0),
		(SELECT COUNT(*) FROM persons WHERE deleted_at <> 0),
		(SELECT COUNT(*) FROM identities),
		(SELECT COUNT(*) FROM relationships),
		(SELECT COUNT(*) FROM suggestions WHERE status = 'proposed'),
		(SELECT COUNT(*) FROM identity_refusals)`).
		Scan(&counts.Persons, &counts.DeletedPersons, &counts.Identities, &counts.Relationships, &counts.ProposedSuggestions, &counts.Refusals)
	return counts, err
}
