// Package predictionstore is the calibration ledger: resolvable claims, the
// odds stated for them, and what actually happened.
//
// It exists to make one line of the house directives measurable. "Start from
// the base rate" is unfollowable without a record of how often claims of a kind
// turn out true, and until this store existed that number was always invented.
// GET /base-rates answers it from resolved rows.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New.
package predictionstore

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var (
	ErrNotFound          = errors.New("not found")
	ErrInvalidPrediction = errors.New("invalid prediction")
	ErrInvalidEstimate   = errors.New("invalid estimate")
	ErrInvalidLink       = errors.New("invalid link")
	ErrAlreadyResolved   = errors.New("already resolved")
)

// PredictionIDPrefix is the type part of a prediction's public id:
// prediction_000142.
const PredictionIDPrefix = "prediction"

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its records in, with
// the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "predictions", Role: "predictions_module", Migrations: migrations}, nil
}

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

// Store reads and writes the ledger through the module's connection pool.
type Store struct {
	db *sql.DB
}

// New returns the store over db, the pool postgresstorage.Open gave Module.
// The schema must already be migrated.
func New(db *sql.DB) *Store { return &Store{db: db} }

func now() int64 { return time.Now().Unix() }

// formatPredictionID renders a prediction's number as its public id. The prefix
// is not decoration: dash's resolver probes every registry row whose id
// pattern matches, and noteboard already claims the bare-uuid shape, so an id
// that looked like a uuid would make every uuid in every chat message probe
// this store too. The prefix also keeps '/' out of the id, which kanban-store's
// reverse lookup splits on.
func formatPredictionID(seq int64) string {
	return postgresstorage.FormatRecordID(PredictionIDPrefix, seq)
}

// predictionNumberOf reads the number out of a public id. An id that is not
// exactly the form this store writes names no prediction: prediction_0000001
// is not prediction_000001, as it was not when the id was a stored string.
func predictionNumberOf(id string) (int64, error) {
	seq, err := postgresstorage.ParseRecordID(PredictionIDPrefix, id)
	if err != nil {
		return 0, fmt.Errorf("%w: prediction %s: %v", ErrNotFound, id, err)
	}
	if formatPredictionID(seq) != id {
		return 0, fmt.Errorf("%w: prediction %s: the store writes that number as %s", ErrNotFound, id, formatPredictionID(seq))
	}
	return seq, nil
}

// cleanTags drops blank and surrounding space, and never returns nil, so an
// untagged row stores [] rather than null.
func cleanTags(tags []string) []string {
	cleaned := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag != "" {
			cleaned = append(cleaned, tag)
		}
	}
	return cleaned
}

func marshalTags(tags []string) (string, error) {
	encoded, err := json.Marshal(cleanTags(tags))
	if err != nil {
		return "", fmt.Errorf("encoding tags: %w", err)
	}
	return string(encoded), nil
}

func unmarshalTags(raw string) ([]string, error) {
	tags := []string{}
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return nil, fmt.Errorf("reading stored tags %q: %w", raw, err)
	}
	if tags == nil {
		return []string{}, nil
	}
	return tags, nil
}
