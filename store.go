// Package possessionstore keeps an inventory of the things a person owns:
// one core record per possession, a table of its own for each kind of thing
// (garments, computers, computer parts, audio devices, musical instruments,
// vehicles), outfits made of garments, purchases line by line, and the photo
// review in which two models settle what a photo shows.
//
// Every record is cited elsewhere by its id (possession_000001,
// garment_000001…), which kanban-store's registry claims, so a chat or a note
// can draw one as a chip.
package possessionstore

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaSQL string

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
)

// ReferenceChecker says whether an id another store owns exists. An error
// that wraps ErrInvalid means the record does not exist; any other error
// means the owning store could not be asked.
type ReferenceChecker interface {
	Check(ctx context.Context, kind ReferenceKind, id string) error
}

// Store owns the database.
type Store struct {
	db         *sql.DB
	dataDir    string
	references ReferenceChecker
	// writeMutex serializes every write; the next id is MAX(seq)+1 read inside
	// the write's transaction.
	writeMutex sync.Mutex
	now        func() time.Time
}

// Open creates the data directory (0700) if needed, opens the database and
// applies the schema.
func Open(dataDir string, references ReferenceChecker) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("open store: no data directory given")
	}
	if references == nil {
		return nil, errors.New("open store: no reference checker given")
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "originals"), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, err
	}
	databasePath := filepath.Join(dataDir, "possession-store.db")
	db, err := sql.Open("sqlite3", databasePath+"?_foreign_keys=on&_busy_timeout=5000")
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
	if err := addColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := os.Chmod(databasePath, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dataDir: dataDir, references: references, now: time.Now}, nil
}

// addedColumns are the columns added after a table was first made, in order.
// schema.sql never changes a CREATE TABLE; a new column goes here.
var addedColumns = []struct{ table, column, definition string }{
	// decision: the sighting waits on the answer. detail: the sighting is
	// recorded, and the answer only adds to what was recorded.
	{"owner_questions", "purpose", "TEXT NOT NULL DEFAULT 'decision'"},
}

func addColumns(db *sql.DB) error {
	for _, added := range addedColumns {
		rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, added.table)
		if err != nil {
			return err
		}
		present := false
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			present = present || name == added.column
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if !present {
			if _, err := db.Exec(`ALTER TABLE ` + added.table + ` ADD COLUMN ` + added.column + ` ` + added.definition); err != nil {
				return fmt.Errorf("add %s.%s: %w", added.table, added.column, err)
			}
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DataDir is where the database and originals live.
func (s *Store) DataDir() string { return s.dataDir }

// OriginalsDir holds the full-size photos possession-add-photos copied in,
// named <sha256>.<extension>.
func (s *Store) OriginalsDir() string { return filepath.Join(s.dataDir, "originals") }

func (s *Store) unixNow() int64 { return s.now().Unix() }

// nextID mints the next id of a table: <prefix>_<seq, six digits or more>.
func nextID(transaction *sql.Tx, table, prefix string) (string, int64, error) {
	var maximum sql.NullInt64
	if err := transaction.QueryRow(`SELECT MAX(seq) FROM ` + table).Scan(&maximum); err != nil {
		return "", 0, err
	}
	sequence := maximum.Int64 + 1
	return fmt.Sprintf("%s_%06d", prefix, sequence), sequence, nil
}

// saveRevision copies a whole row, as it stands, into revisions.
func saveRevision(transaction *sql.Tx, table, idColumn, id, changedBy string, changedAt int64) error {
	rows, err := transaction.Query(`SELECT * FROM `+table+` WHERE `+idColumn+` = ?`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s %s", ErrNotFound, table, id)
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for index := range values {
		pointers[index] = &values[index]
	}
	if err := rows.Scan(pointers...); err != nil {
		return err
	}
	record := map[string]any{"table": table}
	for index, name := range columns {
		if bytes, isBytes := values[index].([]byte); isBytes {
			record[name] = string(bytes)
		} else {
			record[name] = values[index]
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = transaction.Exec(`INSERT INTO revisions (record_id, record, changed_by, changed_at) VALUES (?, ?, ?, ?)`, id, string(encoded), changedBy, changedAt)
	return err
}

// Revision is one saved copy of a record before a change.
type Revision struct {
	ID        int64           `json:"id"`
	RecordID  string          `json:"record_id"`
	Record    json.RawMessage `json:"record"`
	ChangedBy string          `json:"changed_by"`
	ChangedAt int64           `json:"changed_at"`
}

// Revisions lists a record's saved copies, newest first.
func (s *Store) Revisions(recordID string) ([]Revision, error) {
	rows, err := s.db.Query(`SELECT id, record_id, record, changed_by, changed_at FROM revisions WHERE record_id = ? ORDER BY id DESC`, recordID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []Revision{}
	for rows.Next() {
		var revision Revision
		var record string
		if err := rows.Scan(&revision.ID, &revision.RecordID, &record, &revision.ChangedBy, &revision.ChangedAt); err != nil {
			return nil, err
		}
		revision.Record = json.RawMessage(record)
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

// checkReference checks one id a write is about to store: this store's own
// records in the transaction, another store's through the checker.
func (s *Store) checkReference(ctx context.Context, transaction *sql.Tx, kind ReferenceKind, id string) error {
	switch kind {
	case ReferenceComputer:
		return requireRow(transaction, "computers", id)
	case ReferencePurchaseLine:
		return requireRow(transaction, "purchase_lines", id)
	case ReferenceMusicInstrument:
		return nil
	}
	return s.references.Check(ctx, kind, id)
}

func requireRow(transaction *sql.Tx, table, id string) error {
	var found int
	err := transaction.QueryRow(`SELECT 1 FROM `+table+` WHERE id = ?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no %s %s", ErrInvalid, table, id)
	}
	return err
}

// ServiceReferences checks ids against the stores that own them.
type ServiceReferences struct {
	PhotoStoreURL        string
	LocationStoreURL     string
	HarnessStoreDatabase string
	HTTPClient           *http.Client
}

// Check asks the owning store whether id exists.
func (references ServiceReferences) Check(ctx context.Context, kind ReferenceKind, id string) error {
	switch kind {
	case ReferencePhoto:
		return references.checkURL(ctx, references.PhotoStoreURL+"/photos/"+url.PathEscape(id), "photo-store", id)
	case ReferencePlace:
		return references.checkURL(ctx, references.LocationStoreURL+"/places/"+url.PathEscape(id), "location-store", id)
	case ReferenceHarnessMachine:
		database, err := sql.Open("sqlite3", "file:"+references.HarnessStoreDatabase+"?mode=ro&_busy_timeout=5000")
		if err != nil {
			return fmt.Errorf("open harness-store database: %w", err)
		}
		defer database.Close()
		var found int
		err = database.QueryRowContext(ctx, `SELECT 1 FROM machines WHERE id = ?`, id).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: harness-store has no machine %s", ErrInvalid, id)
		}
		if err != nil {
			return fmt.Errorf("read harness-store machines: %w", err)
		}
		return nil
	}
	return fmt.Errorf("no way to check a %s reference", kind)
}

// LocalMachineName answers the name of the one machine harness-store records
// as this host (transport local), which photo-store calls a photo's
// source_machine.
func LocalMachineName(harnessStoreDatabase string) (string, error) {
	database, err := sql.Open("sqlite3", "file:"+harnessStoreDatabase+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return "", fmt.Errorf("open harness-store database: %w", err)
	}
	defer database.Close()
	names, err := stringColumn(database, `SELECT name FROM machines WHERE transport = 'local'`)
	if err != nil {
		return "", fmt.Errorf("read harness-store machines: %w", err)
	}
	if len(names) != 1 {
		return "", fmt.Errorf("harness-store records %d local machines, not one: %v", len(names), names)
	}
	return names[0], nil
}

func (references ServiceReferences) checkURL(ctx context.Context, address, service, id string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	response, err := references.HTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("ask %s about %s: %w", service, id, err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s has no %s", ErrInvalid, service, id)
	}
	return fmt.Errorf("ask %s about %s: status %d", service, id, response.StatusCode)
}
