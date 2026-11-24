// Package predictionstore is the calibration ledger: resolvable claims, the
// odds stated for them, and what actually happened.
//
// It exists to make one line of the house directives measurable. "Start from
// the base rate" is unfollowable without a record of how often claims of a kind
// turn out true, and until this store existed that number was always invented.
// GET /base-rates answers it from resolved rows.
package predictionstore

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
	ErrNotFound          = errors.New("not found")
	ErrInvalidPrediction = errors.New("invalid prediction")
	ErrInvalidEstimate   = errors.New("invalid estimate")
	ErrInvalidLink       = errors.New("invalid link")
	ErrAlreadyResolved   = errors.New("already resolved")
)

// Prediction is one resolvable claim and the odds on it.
type Prediction struct {
	ID                 string   `json:"id"`
	Seq                int64    `json:"seq"`
	Claim              string   `json:"claim"`
	ResolutionCriteria string   `json:"resolution_criteria"`
	Category           string   `json:"category"`
	Tags               []string `json:"tags"`
	Provenance         string   `json:"provenance"`
	Author             string   `json:"author"`
	Probability        float64  `json:"probability"`
	InitialProbability float64  `json:"initial_probability"`
	DueAt              int64    `json:"due_at"`
	Status             string   `json:"status"`
	Outcome            string   `json:"outcome"`
	ResolvedAt         int64    `json:"resolved_at"`
	ResolutionNote     string   `json:"resolution_note"`
	CreatedAt          int64    `json:"created_at"`
	UpdatedAt          int64    `json:"updated_at"`
	DeletedAt          int64    `json:"deleted_at"`

	// Computed on read, ignored on write.
	Estimates []*Estimate `json:"estimates,omitempty"`
	Links     []*Link     `json:"links,omitempty"`
	Brier     *float64    `json:"brier,omitempty"`
}

// Estimate is one revision of the odds. Append-only.
type Estimate struct {
	ID           int64   `json:"id"`
	PredictionID string  `json:"prediction_id"`
	Probability  float64 `json:"probability"`
	Rationale    string  `json:"rationale"`
	Provenance   string  `json:"provenance"`
	Author       string  `json:"author"`
	CreatedAt    int64   `json:"created_at"`
}

// Link points at the thing the prediction is about, by id in the store that
// owns it. Label is display only and is never read back to find the row.
type Link struct {
	ID           int64  `json:"id"`
	PredictionID string `json:"prediction_id"`
	EntityType   string `json:"entity_type"`
	EntityRef    string `json:"entity_ref"`
	Label        string `json:"label"`
	CreatedAt    int64  `json:"created_at"`
}

// Filter selects predictions. The zero value filters nothing.
type Filter struct {
	Status         string
	Category       string
	Tag            string
	Author         string
	Provenance     string
	Outcome        string
	EntityType     string
	EntityRef      string
	Query          string
	Overdue        bool
	Since          int64
	Until          int64
	IncludeDeleted bool
	Expand         bool
	Limit          int
	Offset         int
}

// Patch carries only the fields a caller mentioned. Probability and outcome are
// deliberately absent: the first moves through POST /estimates so the trail
// survives, the second through POST /resolve so it can only happen once.
//
// ResolutionNote is the one field here that describes an outcome, and it is
// editable for a reason the others are not: the note is prose written in a
// hurry at the moment of resolving, and a wrong note is a wrong record. What
// must not move is the outcome itself, and this cannot move it. Correcting why
// a row resolved is bookkeeping; changing whether it resolved true is cheating.
type Patch struct {
	Claim              *string   `json:"claim"`
	ResolutionCriteria *string   `json:"resolution_criteria"`
	Category           *string   `json:"category"`
	Tags               *[]string `json:"tags"`
	Provenance         *string   `json:"provenance"`
	Author             *string   `json:"author"`
	DueAt              *int64    `json:"due_at"`
	ResolutionNote     *string   `json:"resolution_note"`
}

// TagCount is one tag and how many live predictions carry it.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
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
	return filepath.Join(home, ".config", "prediction-store")
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
	dbPath := filepath.Join(dataDir, "prediction-store.db")
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
				"migrate: %w — this binary was built without FTS5; rebuild with: go build -tags sqlite_fts5 ./cmd/prediction-store", err)
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

func now() int64 { return time.Now().Unix() }

// formatID renders a sequence number as the public id. The prefix is not
// decoration: dash's resolver probes every registry row whose id pattern
// matches, and noteboard already claims the bare-uuid shape, so an id that
// looked like a uuid would make every uuid in every chat message probe this
// store too. The prefix also keeps '/' out of the id, which kanban-store's
// reverse lookup splits on.
func formatID(seq int64) string { return fmt.Sprintf("prediction_%06d", seq) }

func marshalTags(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	cleaned := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t != "" {
			cleaned = append(cleaned, t)
		}
	}
	b, err := json.Marshal(cleaned)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalTags(raw string) []string {
	if raw == "" {
		return []string{}
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return []string{}
	}
	if tags == nil {
		return []string{}
	}
	return tags
}
