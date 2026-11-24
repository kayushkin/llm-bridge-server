package predictionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"

	_ "modernc.org/sqlite"
)

// standaloneColumns are the columns of each table of the SQLite database the
// standalone prediction-store kept. The import refuses a file whose tables have
// any other columns: a column it does not know is data it would drop.
var standaloneColumns = map[string][]string{
	"predictions": {"id", "seq", "claim", "resolution_criteria", "category", "tags", "provenance", "author",
		"probability", "initial_probability", "due_at", "status", "outcome", "resolved_at", "resolution_note",
		"created_at", "updated_at", "deleted_at"},
	"estimates": {"id", "prediction_id", "probability", "rationale", "provenance", "author", "created_at"},
	"links":     {"id", "prediction_id", "entity_type", "entity_ref", "label", "created_at"},
}

// ImportCounts says how many rows of each table an import copied.
type ImportCounts struct {
	Predictions        int   `json:"predictions"`
	DeletedPredictions int   `json:"deleted_predictions"`
	Estimates          int   `json:"estimates"`
	Links              int   `json:"links"`
	LastPredictionSeq  int64 `json:"last_prediction_seq"`
	LastEstimateID     int64 `json:"last_estimate_id"`
	LastLinkID         int64 `json:"last_link_id"`
}

// ImportStandaloneDatabase copies every prediction, estimate and link from the
// SQLite file of a standalone prediction-store into the module behind db, as
// they are: numbers, ids, times, outcomes and soft-deleted rows. It runs in one
// transaction, so it copies everything or nothing.
//
// It refuses a module that already holds any row, so it can never mix two
// ledgers or run twice, and it refuses a file it cannot copy whole: a table
// with a column it does not know, an id that is not the form its number makes,
// tags that are not a list of strings, or a row pointing at a prediction the
// file does not hold. Afterwards each table numbers on from the highest number
// copied, so no id that chats, notes and links already carry is handed out
// again.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	if _, err := os.Stat(sqlitePath); err != nil {
		return counts, fmt.Errorf("the standalone database: %w", err)
	}
	source, err := sql.Open("sqlite", "file:"+sqlitePath+"?mode=ro")
	if err != nil {
		return counts, fmt.Errorf("opening %s: %w", sqlitePath, err)
	}
	defer source.Close()
	for table, want := range standaloneColumns {
		if err := checkStandaloneColumns(ctx, source, table, want); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		for _, table := range []string{"predictions", "estimates", "links"} {
			var held int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&held); err != nil {
				return fmt.Errorf("counting %s: %w", table, err)
			}
			if held > 0 {
				return fmt.Errorf("the predictions module already holds %d rows in %s; the import copies into an empty module only", held, table)
			}
		}
		var err error
		if counts.Predictions, counts.DeletedPredictions, counts.LastPredictionSeq, err = importPredictions(ctx, source, tx); err != nil {
			return err
		}
		if counts.Estimates, counts.LastEstimateID, err = importEstimates(ctx, source, tx); err != nil {
			return err
		}
		if counts.Links, counts.LastLinkID, err = importLinks(ctx, source, tx); err != nil {
			return err
		}
		for _, numbering := range []struct {
			table, column string
			last          int64
		}{
			{"predictions", "seq", counts.LastPredictionSeq},
			{"estimates", "id", counts.LastEstimateID},
			{"links", "id", counts.LastLinkID},
		} {
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, numbering.table, numbering.column, numbering.last); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

func checkStandaloneColumns(ctx context.Context, source *sql.DB, table string, want []string) error {
	rows, err := source.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		found = append(found, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(found) == 0 {
		return fmt.Errorf("it has no table %s, so it is not a prediction-store database", table)
	}
	sortedWant := append([]string{}, want...)
	sort.Strings(sortedWant)
	sort.Strings(found)
	if strings.Join(found, ",") != strings.Join(sortedWant, ",") {
		return fmt.Errorf("table %s has columns %s, and the import knows %s; it copies only a table it can copy whole",
			table, strings.Join(found, ", "), strings.Join(sortedWant, ", "))
	}
	return nil
}

func importPredictions(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied, deleted int, lastSeq int64, err error) {
	rows, err := source.QueryContext(ctx, `
		SELECT id, seq, claim, resolution_criteria, category, tags, provenance, author,
			probability, initial_probability, due_at, status, outcome, resolved_at, resolution_note,
			created_at, updated_at, deleted_at
		FROM predictions ORDER BY seq`)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("reading predictions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p Prediction
		var tags string
		if err := rows.Scan(&p.ID, &p.Seq, &p.Claim, &p.ResolutionCriteria, &p.Category, &tags, &p.Provenance, &p.Author,
			&p.Probability, &p.InitialProbability, &p.DueAt, &p.Status, &p.Outcome, &p.ResolvedAt, &p.ResolutionNote,
			&p.CreatedAt, &p.UpdatedAt, &p.DeletedAt); err != nil {
			return 0, 0, 0, fmt.Errorf("reading predictions: %w", err)
		}
		if p.ID != formatPredictionID(p.Seq) {
			return 0, 0, 0, fmt.Errorf("prediction %q has number %d, whose id is %s; the import keeps ids by their number and will not change one", p.ID, p.Seq, formatPredictionID(p.Seq))
		}
		storedTags, err := standaloneTags(tags)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("prediction %s: %w", p.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO predictions
				(seq, claim, resolution_criteria, category, tags, provenance, author,
				 probability, initial_probability, due_at, status, outcome, resolved_at, resolution_note,
				 created_at, updated_at, deleted_at)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
			p.Seq, p.Claim, p.ResolutionCriteria, p.Category, storedTags, p.Provenance, p.Author,
			p.Probability, p.InitialProbability, p.DueAt, p.Status, p.Outcome, p.ResolvedAt, p.ResolutionNote,
			p.CreatedAt, p.UpdatedAt, p.DeletedAt); err != nil {
			return 0, 0, 0, fmt.Errorf("copying prediction %s: %w", p.ID, err)
		}
		copied++
		if p.DeletedAt != 0 {
			deleted++
		}
		lastSeq = p.Seq
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("reading predictions: %w", err)
	}
	return copied, deleted, lastSeq, nil
}

// standaloneTags checks the stored tags are a JSON list of strings and returns
// them as written. The standalone store wrote "[]" for no tags, and its column
// defaulted to an empty string, which means the same.
func standaloneTags(raw string) (string, error) {
	if raw == "" {
		return "[]", nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return "", fmt.Errorf("its tags %q are not a JSON list of strings: %w", raw, err)
	}
	if tags == nil {
		return "", fmt.Errorf("its tags are %q, not a list", raw)
	}
	return raw, nil
}

func importEstimates(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied int, lastID int64, err error) {
	rows, err := source.QueryContext(ctx, `
		SELECT id, prediction_id, probability, rationale, provenance, author, created_at
		FROM estimates ORDER BY id`)
	if err != nil {
		return 0, 0, fmt.Errorf("reading estimates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e Estimate
		if err := rows.Scan(&e.ID, &e.PredictionID, &e.Probability, &e.Rationale, &e.Provenance, &e.Author, &e.CreatedAt); err != nil {
			return 0, 0, fmt.Errorf("reading estimates: %w", err)
		}
		seq, err := predictionNumberOf(e.PredictionID)
		if err != nil {
			return 0, 0, fmt.Errorf("estimate %d: %w", e.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO estimates (id, prediction_seq, probability, rationale, provenance, author, created_at)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			e.ID, seq, e.Probability, e.Rationale, e.Provenance, e.Author, e.CreatedAt); err != nil {
			return 0, 0, fmt.Errorf("copying estimate %d of %s: %w", e.ID, e.PredictionID, err)
		}
		copied++
		lastID = e.ID
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("reading estimates: %w", err)
	}
	return copied, lastID, nil
}

func importLinks(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied int, lastID int64, err error) {
	rows, err := source.QueryContext(ctx, `
		SELECT id, prediction_id, entity_type, entity_ref, label, created_at
		FROM links ORDER BY id`)
	if err != nil {
		return 0, 0, fmt.Errorf("reading links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.ID, &l.PredictionID, &l.EntityType, &l.EntityRef, &l.Label, &l.CreatedAt); err != nil {
			return 0, 0, fmt.Errorf("reading links: %w", err)
		}
		seq, err := predictionNumberOf(l.PredictionID)
		if err != nil {
			return 0, 0, fmt.Errorf("link %d: %w", l.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO links (id, prediction_seq, entity_type, entity_ref, label, created_at)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, $2, $3, $4, $5, $6)`,
			l.ID, seq, l.EntityType, l.EntityRef, l.Label, l.CreatedAt); err != nil {
			return 0, 0, fmt.Errorf("copying link %d of %s: %w", l.ID, l.PredictionID, err)
		}
		copied++
		lastID = l.ID
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("reading links: %w", err)
	}
	return copied, lastID, nil
}
