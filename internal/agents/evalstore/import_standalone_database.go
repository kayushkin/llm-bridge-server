package evalstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneTable is one table of the standalone eval-store's SQLite file:
// its columns, in the order they are copied, and for a table of numbered
// records the prefix every id carries.
type standaloneTable struct {
	name     string
	columns  []string
	orderBy  string
	idPrefix string
}

// standaloneTables are copied in this order, parents before the rows that
// point at them. Their columns are those the standalone store's schema and its
// one added column (cases.expected_answer) left.
var standaloneTables = []standaloneTable{
	{name: "suites", idPrefix: suiteIDPrefix, orderBy: "seq", columns: []string{"seq", "id", "name", "description", "task_kind",
		"judge_instructions", "judge_failure_kinds", "output_schema", "max_tokens", "judge_max_tokens", "checker_name", "baseline_model_id",
		"weekly_calls_source", "cost_model", "created_at", "updated_at"}},
	{name: "cases", idPrefix: caseIDPrefix, orderBy: "seq", columns: []string{"seq", "id", "suite_id", "source_kind", "source_ref",
		"system_prompt", "user_prompt", "original_model_id", "original_answer", "original_usage_known", "original_input_tokens",
		"original_cache_read_tokens", "original_cache_write_tokens", "original_output_tokens", "original_duration_ms", "original_at",
		"features", "created_at", "expected_answer"}},
	{name: "runs", idPrefix: runIDPrefix, orderBy: "seq", columns: []string{"seq", "id", "suite_id", "candidate_model_ids",
		"judge_model_ids", "original_answer_model_id", "case_selection", "case_ids", "status", "status_reason", "budget_limits",
		"concurrency", "organization_id", "model_prices", "prices_note", "prediction_id", "note", "origin", "created_at", "started_at",
		"finished_at", "updated_at"}},
	{name: "answers", idPrefix: answerIDPrefix, orderBy: "seq", columns: []string{"seq", "id", "run_id", "case_id", "model_id",
		"origin", "attempt", "parsed", "error", "input_tokens", "cache_read_tokens", "cache_write_tokens", "output_tokens", "usage_known",
		"duration_ms", "operation_id", "answered_by", "created_at"}},
	{name: "verdicts", idPrefix: verdictIDPrefix, orderBy: "seq", columns: []string{"seq", "id", "run_id", "case_id", "judge_model_id",
		"judge_provider", "attempt", "label_mapping", "parsed", "valid", "invalid_reason", "error", "input_tokens", "output_tokens",
		"duration_ms", "operation_id", "created_at"}},
	{name: "verdict_candidates", orderBy: "verdict_id, model_id", columns: []string{"verdict_id", "model_id", "label", "rank",
		"acceptable", "failure_kind", "reason"}},
	{name: "checks", orderBy: "run_id, case_id, model_id, check_name", columns: []string{"run_id", "case_id", "model_id",
		"check_name", "passed", "count", "detail"}},
}

// ImportCounts says how many rows of each table an import copied, by table.
type ImportCounts map[string]int64

// ImportStandaloneDatabase copies every suite, case, run, answer, verdict,
// verdict line and check from the SQLite file of a standalone eval-store into
// the module behind db, as they are: ids and their sequence numbers, prompts,
// answers, JSON exactly as written, flags and times. It runs in one
// transaction, so it copies everything or nothing.
//
// It refuses a module that already holds any row, a file holding a table it
// does not know, a table with columns it does not know, and a record whose id
// is not its prefix and sequence number. Afterwards each table numbers on
// from the highest number the standalone store handed out, so an id runs,
// predictions and prompts already name is never handed out again.
//
// Cases hold real mail. The import copies them and says only how many.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return nil, err
	}
	defer source.Close()

	known := map[string]bool{"sqlite_sequence": true}
	for _, table := range standaloneTables {
		known[table.name] = true
		if err := standalonesqlite.CheckColumns(ctx, source, "eval-store", table.name, table.columns); err != nil {
			return nil, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}
	tables, err := source.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, fmt.Errorf("%s: listing its tables: %w", sqlitePath, err)
	}
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			tables.Close()
			return nil, fmt.Errorf("%s: listing its tables: %w", sqlitePath, err)
		}
		if !known[name] {
			tables.Close()
			return nil, fmt.Errorf("%s has a table %s, which the import does not know; it copies only a file it can copy whole", sqlitePath, name)
		}
	}
	if err := tables.Close(); err != nil {
		return nil, err
	}

	lastNumbers := map[string]int64{}
	for _, table := range standaloneTables {
		if table.idPrefix == "" {
			continue
		}
		var last int64
		if err := source.QueryRowContext(ctx, `SELECT MAX(COALESCE((SELECT seq FROM sqlite_sequence WHERE name = $1), 0),
			(SELECT COALESCE(MAX(seq), 0) FROM `+table.name+`))`, table.name).Scan(&last); err != nil {
			return nil, fmt.Errorf("%s: reading the last number %s handed out: %w", sqlitePath, table.name, err)
		}
		lastNumbers[table.name] = last
	}

	counts := ImportCounts{}
	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		names := make([]string, len(standaloneTables))
		for index, table := range standaloneTables {
			names[index] = table.name
		}
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "evaluations", names...); err != nil {
			return err
		}
		for _, table := range standaloneTables {
			placeholders := make([]string, len(table.columns))
			for index := range placeholders {
				placeholders[index] = fmt.Sprintf("$%d", index+1)
			}
			overriding := ""
			if table.idPrefix != "" {
				overriding = " OVERRIDING SYSTEM VALUE"
			}
			copied, err := standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
				Name: table.name, Columns: table.columns, OrderBy: table.orderBy,
				Insert: fmt.Sprintf("INSERT INTO %s (%s)%s VALUES (%s)", table.name, strings.Join(table.columns, ", "), overriding,
					strings.Join(placeholders, ", ")),
				Convert: idMustBeItsNumber(table.idPrefix),
			})
			if err != nil {
				return err
			}
			counts[table.name] = copied
			if table.idPrefix != "" {
				if err := postgresstorage.ContinueNumberingAfter(ctx, tx, table.name, "seq", lastNumbers[table.name]); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return counts, nil
}

// idMustBeItsNumber refuses a row of a numbered table whose id is not its
// prefix and sequence number: the module makes every later id from the next
// number, and an id that is not its own number could be made again. The row
// passes unchanged. A table with no prefix is not checked.
func idMustBeItsNumber(prefix string) func(row []any) ([]any, error) {
	if prefix == "" {
		return nil
	}
	return func(row []any) ([]any, error) {
		seq, err := standalonesqlite.Integer("seq", row[0])
		if err != nil {
			return nil, err
		}
		id, err := standalonesqlite.Text("id", row[1])
		if err != nil {
			return nil, err
		}
		if want := postgresstorage.FormatRecordID(prefix, seq); id != want {
			return nil, fmt.Errorf("id %s is not %s, its sequence number %d", id, want, seq)
		}
		return row, nil
	}
}
