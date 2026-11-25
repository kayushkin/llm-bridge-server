package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// ImportCounts says what ImportStandaloneDatabase copied.
type ImportCounts struct {
	Jobs                   int64
	Runs                   int64
	RunsLeftBehind         int64
	ServiceSettings        int64
	EventTriggerDeliveries int64
	// LastJobNumber and LastRunNumber are where numbering goes on: the highest
	// number the standalone scheduler ever handed out, deleted rows included.
	LastJobNumber int64
	LastRunNumber int64
}

// ImportOptions narrows what ImportStandaloneDatabase copies.
type ImportOptions struct {
	// NewestRunsPerJob, when above zero, copies only each job's newest runs,
	// that many of them; the rest are left in the file and counted as left
	// behind. Zero copies every run. GET /api/jobs/{id}/runs answers at most
	// MaxRunsListed, so a value of at least that answers every route as the
	// standalone scheduler did.
	NewestRunsPerJob int
}

var standaloneJobColumns = []string{"id", "name", "schedule", "command", "type", "agent", "prompt", "model_role", "orchestrator",
	"session_id", "enabled", "created_at", "updated_at", "workspace_id", "hold_until", "description", "timeout_seconds", "event_trigger"}

// legacySessionTargetColumn is a jobs column the scheduler stopped reading
// when orchestrator and session_id replaced it, and never dropped. Every row
// of a database that still has it must hold its default, legacySessionTarget:
// then nothing is lost by leaving it behind.
const (
	legacySessionTargetColumn = "session_target"
	legacySessionTarget       = "isolated"
)

// standaloneJobColumnsHeld answers the jobs columns the file holds: the ones
// the import copies, and session_target when the file still has it and every
// row holds its default.
func standaloneJobColumnsHeld(ctx context.Context, source *sql.DB) ([]string, error) {
	var hasLegacyColumn bool
	if err := source.QueryRowContext(ctx, "SELECT COUNT(*) > 0 FROM pragma_table_info('jobs') WHERE name = ?", legacySessionTargetColumn).Scan(&hasLegacyColumn); err != nil {
		return nil, fmt.Errorf("reading the columns of jobs: %w", err)
	}
	if !hasLegacyColumn {
		return standaloneJobColumns, nil
	}
	var otherwise int64
	if err := source.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE "+legacySessionTargetColumn+" IS NOT ?", legacySessionTarget).Scan(&otherwise); err != nil {
		return nil, fmt.Errorf("reading jobs.%s: %w", legacySessionTargetColumn, err)
	}
	if otherwise > 0 {
		return nil, fmt.Errorf("%d jobs hold a %s other than %q; the scheduler no longer reads it, and the import will not drop a value it cannot account for", otherwise, legacySessionTargetColumn, legacySessionTarget)
	}
	return append(append([]string{}, standaloneJobColumns...), legacySessionTargetColumn), nil
}

var standaloneRunColumns = []string{"id", "job_id", "status", "output", "started_at", "finished_at"}

// ImportStandaloneDatabase copies a standalone scheduler's SQLite file (a copy
// made with sqlite3 .backup) into the module behind pool: every job with its
// id and hold, the runs options names with their ids, the stored settings and
// the event-trigger deliveries. Numbering goes on after the highest number the
// standalone scheduler handed out, so no job id that chats, notes and links
// already name is handed out again. It refuses a module that already holds a
// row, and copies everything or nothing.
func ImportStandaloneDatabase(ctx context.Context, pool *sql.DB, path string, options ImportOptions) (ImportCounts, error) {
	var counts ImportCounts
	if options.NewestRunsPerJob < 0 {
		return counts, fmt.Errorf("newest runs per job is %d; it is zero for every run, or above zero", options.NewestRunsPerJob)
	}
	source, err := standalonesqlite.Open(path)
	if err != nil {
		return counts, err
	}
	defer source.Close()
	jobColumnsHeld, err := standaloneJobColumnsHeld(ctx, source)
	if err != nil {
		return counts, err
	}
	for table, columns := range map[string][]string{
		"jobs":                     jobColumnsHeld,
		"runs":                     standaloneRunColumns,
		"service_settings":         {"key", "value", "updated_at"},
		"event_trigger_deliveries": {"job_id", "idempotency_key", "run_id", "created_at"},
	} {
		if err := standalonesqlite.CheckColumns(ctx, source, "scheduler", table, columns); err != nil {
			return counts, err
		}
	}

	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		return counts, err
	}
	defer tx.Rollback()
	if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "scheduler", "jobs", "runs", "service_settings", "event_trigger_deliveries"); err != nil {
		return counts, err
	}

	if counts.Jobs, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
		Name:    "jobs",
		Columns: standaloneJobColumns,
		OrderBy: "id",
		Insert: `INSERT INTO jobs (id, name, schedule, command, type, agent, prompt, model_role, orchestrator, session_id, enabled,
			created_at, updated_at, workspace_id, hold_until, description, timeout_seconds, event_trigger)
			OVERRIDING SYSTEM VALUE VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		Convert: convertStandaloneJob,
	}); err != nil {
		return counts, err
	}

	if counts.Runs, counts.RunsLeftBehind, err = copyStandaloneRuns(ctx, source, tx, options.NewestRunsPerJob); err != nil {
		return counts, err
	}

	if counts.ServiceSettings, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
		Name:    "service_settings",
		Columns: []string{"key", "value", "updated_at"},
		OrderBy: "key",
		Insert:  `INSERT INTO service_settings (key, value, updated_at) VALUES ($1,$2,$3)`,
		Convert: func(row []any) ([]any, error) {
			updatedAt, err := standaloneTime("updated_at", row[2])
			if err != nil {
				return nil, err
			}
			return []any{row[0], row[1], updatedAt}, nil
		},
	}); err != nil {
		return counts, err
	}

	if counts.EventTriggerDeliveries, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
		Name:    "event_trigger_deliveries",
		Columns: []string{"job_id", "idempotency_key", "run_id", "created_at"},
		OrderBy: "job_id, idempotency_key",
		Insert:  `INSERT INTO event_trigger_deliveries (job_id, idempotency_key, run_id, created_at) VALUES ($1,$2,$3,$4)`,
		Convert: func(row []any) ([]any, error) {
			createdAt, err := standaloneTime("created_at", row[3])
			if err != nil {
				return nil, err
			}
			return []any{row[0], row[1], row[2], createdAt}, nil
		},
	}); err != nil {
		return counts, err
	}

	if counts.LastJobNumber, err = lastStandaloneNumber(ctx, source, "jobs"); err != nil {
		return counts, err
	}
	if counts.LastRunNumber, err = lastStandaloneNumber(ctx, source, "runs"); err != nil {
		return counts, err
	}
	if err := postgresstorage.ContinueNumberingAfter(ctx, tx, "jobs", "id", counts.LastJobNumber); err != nil {
		return counts, err
	}
	if err := postgresstorage.ContinueNumberingAfter(ctx, tx, "runs", "id", counts.LastRunNumber); err != nil {
		return counts, err
	}
	return counts, tx.Commit()
}

func convertStandaloneJob(row []any) ([]any, error) {
	enabled, err := standalonesqlite.Boolean("enabled", row[10])
	if err != nil {
		return nil, err
	}
	createdAt, err := standaloneTime("created_at", row[11])
	if err != nil {
		return nil, err
	}
	updatedAt, err := standaloneTime("updated_at", row[12])
	if err != nil {
		return nil, err
	}
	var holdUntil any
	if row[14] != nil {
		if holdUntil, err = standaloneTime("hold_until", row[14]); err != nil {
			return nil, err
		}
	}
	eventTrigger, err := standalonesqlite.Text("event_trigger", row[17])
	if err != nil {
		return nil, err
	}
	if _, err := DecodeEventTrigger(eventTrigger); err != nil {
		return nil, err
	}
	converted := append([]any{}, row...)
	converted[10], converted[11], converted[12], converted[14] = enabled, createdAt, updatedAt, holdUntil
	return converted, nil
}

// copyStandaloneRuns copies every run, or each job's newest newestPerJob of
// them, and answers how many it copied and how many it left behind.
func copyStandaloneRuns(ctx context.Context, source *sql.DB, tx *sql.Tx, newestPerJob int) (int64, int64, error) {
	var total int64
	if err := source.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs").Scan(&total); err != nil {
		return 0, 0, fmt.Errorf("counting runs: %w", err)
	}
	// newestPerJob 0 reads every run.
	rows, err := source.QueryContext(ctx, `SELECT id, job_id, status, CAST(output AS BLOB), started_at, finished_at FROM (
			SELECT runs.*, ROW_NUMBER() OVER (PARTITION BY job_id ORDER BY id DESC) AS newness FROM runs)
		WHERE ? = 0 OR newness <= ? ORDER BY id`, newestPerJob, newestPerJob)
	if err != nil {
		return 0, 0, fmt.Errorf("reading runs: %w", err)
	}
	defer rows.Close()
	statement, err := tx.PrepareContext(ctx, `INSERT INTO runs (id, job_id, status, output, started_at, finished_at)
		OVERRIDING SYSTEM VALUE VALUES ($1,$2,$3,$4,$5,$6)`)
	if err != nil {
		return 0, 0, fmt.Errorf("preparing the copy of runs: %w", err)
	}
	defer statement.Close()
	var copied int64
	for rows.Next() {
		var id, jobID int64
		var status string
		var output []byte
		var startedAtValue, finishedAtValue any
		if err := rows.Scan(&id, &jobID, &status, &output, &startedAtValue, &finishedAtValue); err != nil {
			return copied, 0, fmt.Errorf("reading run %d of runs: %w", copied+1, err)
		}
		if output == nil {
			output = []byte{}
		}
		startedAt, err := standaloneTime("started_at", startedAtValue)
		if err != nil {
			return copied, 0, fmt.Errorf("run %d: %w", id, err)
		}
		var finishedAt any
		if finishedAtValue != nil {
			if finishedAt, err = standaloneTime("finished_at", finishedAtValue); err != nil {
				return copied, 0, fmt.Errorf("run %d: %w", id, err)
			}
		}
		if _, err := statement.ExecContext(ctx, id, jobID, status, output, startedAt, finishedAt); err != nil {
			return copied, 0, fmt.Errorf("copying run %d: %w", id, err)
		}
		copied++
	}
	if err := rows.Err(); err != nil {
		return copied, 0, fmt.Errorf("reading runs: %w", err)
	}
	return copied, total - copied, nil
}

// lastStandaloneNumber is the highest id table ever handed out: SQLite's
// AUTOINCREMENT counter, which counts deleted rows too, or the highest id held
// when the counter has no row.
func lastStandaloneNumber(ctx context.Context, source *sql.DB, table string) (int64, error) {
	var counter sql.NullInt64
	if err := source.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name = ?", table).Scan(&counter); err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("reading the numbering of %s: %w", table, err)
	}
	var highest sql.NullInt64
	if err := source.QueryRowContext(ctx, "SELECT MAX(id) FROM "+table).Scan(&highest); err != nil {
		return 0, fmt.Errorf("reading the highest id of %s: %w", table, err)
	}
	return max(counter.Int64, highest.Int64), nil
}

// standaloneTimeLayouts are the ways the standalone scheduler's file holds a
// time: Go's time.Time.String(), which the driver once wrote, the driver's
// own layout, and SQLite's CURRENT_TIMESTAMP, which is UTC.
var standaloneTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05",
}

// standaloneTime reads a DATETIME column, which the driver hands over parsed
// or as the text it holds. A NULL or text in no known layout is an error.
func standaloneTime(column string, value any) (time.Time, error) {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC(), nil
	case string:
		for _, layout := range standaloneTimeLayouts {
			if parsed, err := time.Parse(layout, typed); err == nil {
				return parsed.UTC(), nil
			}
		}
		return time.Time{}, fmt.Errorf("%s is %q, which is no time layout the scheduler wrote", column, typed)
	}
	return time.Time{}, fmt.Errorf("%s is %T %v, not a time", column, value, value)
}
