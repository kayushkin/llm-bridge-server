package evalstore

import (
	"context"
	"database/sql"
	_ "embed"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

//go:embed testdata/standalone_schema.sql
var standaloneSchema string

// writeStandaloneDatabase makes a standalone eval-store file from the
// statements, every row of them made up.
func writeStandaloneDatabase(t *testing.T, statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "eval-store.db")
	source, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for _, statement := range append([]string{standaloneSchema}, statements...) {
		if _, err := source.Exec(statement); err != nil {
			t.Fatalf("writing the standalone file: %v", err)
		}
	}
	return path
}

// syntheticStandaloneRows are one suite with one case, a run, an answer, a
// verdict with its line, and a check, as the standalone store wrote them.
var syntheticStandaloneRows = []string{
	`INSERT INTO suites (seq, id, name, task_kind, judge_instructions, judge_failure_kinds, output_schema, max_tokens, judge_max_tokens,
		checker_name, created_at, updated_at)
		VALUES (3, 'eval_suite_000003', 'synthetic', 'demo-mail', 'Judge it.', '["none","no_answer"]', '{"type":"object"}', 100, 200, '', 10, 11)`,
	`INSERT INTO cases (seq, id, suite_id, source_kind, source_ref, system_prompt, user_prompt, original_answer, original_usage_known,
		original_duration_ms, features, created_at, expected_answer)
		VALUES (5, 'eval_case_000005', 'eval_suite_000003', 'llm-bridge-operation', 'operation_synthetic', 'Be brief.', 'Say hello.',
		'{"text":  "hello"}', 1, NULL, '{}', 12, NULL)`,
	`INSERT INTO runs (seq, id, suite_id, candidate_model_ids, judge_model_ids, case_selection, case_ids, status, budget_limits,
		concurrency, model_prices, origin, created_at, updated_at)
		VALUES (2, 'eval_run_000002', 'eval_suite_000003', '["model-a","model-b"]', '["model-j"]', '{"mode":"all"}', '["eval_case_000005"]',
		'completed', '[]', 4, '{}', 'live', 13, 14)`,
	`INSERT INTO answers (seq, id, run_id, case_id, model_id, origin, parsed, usage_known, duration_ms, created_at)
		VALUES (8, 'eval_answer_000008', 'eval_run_000002', 'eval_case_000005', 'model-a', 'replayed', '{"text":"hi"}', 1, 1500, 15)`,
	`INSERT INTO verdicts (seq, id, run_id, case_id, judge_model_id, label_mapping, parsed, valid, created_at)
		VALUES (4, 'eval_verdict_000004', 'eval_run_000002', 'eval_case_000005', 'model-j', '{"A":"model-a","B":"model-b"}', '{}', 1, 16)`,
	`INSERT INTO verdict_candidates VALUES ('eval_verdict_000004', 'model-a', 'A', 1, 1, 'none', 'fine')`,
	`INSERT INTO checks VALUES ('eval_run_000002', 'eval_case_000005', 'model-a', 'keys_match', 1, 0, '')`,
	// A suite the standalone store handed out and then deleted.
	`UPDATE sqlite_sequence SET seq = 7 WHERE name = 'suites'`,
}

func TestImportCopiesEveryRecordWithItsIdAndNumbersOn(t *testing.T) {
	store := openTestStore(t)
	path := writeStandaloneDatabase(t, syntheticStandaloneRows...)
	counts, err := ImportStandaloneDatabase(context.Background(), store.db, path)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for table, want := range map[string]int64{"suites": 1, "cases": 1, "runs": 1, "answers": 1, "verdicts": 1, "verdict_candidates": 1, "checks": 1} {
		if counts[table] != want {
			t.Errorf("%s: copied %d, want %d", table, counts[table], want)
		}
	}
	c, err := store.GetCase("eval_case_000005")
	if err != nil {
		t.Fatal(err)
	}
	if string(c.OriginalAnswer) != `{"text":  "hello"}` || !c.OriginalUsageKnown || c.OriginalDurationMs != nil || c.SuiteID != "eval_suite_000003" {
		t.Errorf("the case read back differs: %+v", c)
	}
	answers, err := store.ListAnswers("eval_run_000002", AnswerFilter{})
	if err != nil || len(answers) != 1 || *answers[0].DurationMs != 1500 || !answers[0].UsageKnown {
		t.Fatalf("answers: %+v, %v", answers, err)
	}
	verdicts, err := store.ListVerdicts("eval_run_000002", VerdictFilter{})
	if err != nil || len(verdicts) != 1 || !verdicts[0].Valid || len(verdicts[0].Candidates) != 1 || verdicts[0].Candidates[0].Reason != "fine" {
		t.Fatalf("verdicts: %+v, %v", verdicts, err)
	}
	next, err := store.CreateSuite(&Suite{Name: "next", TaskKind: "demo-mail", JudgeInstructions: "x", JudgeFailureKinds: []string{"none", "no_answer"},
		OutputSchema: []byte(`{"type":"object"}`), MaxTokens: 1, JudgeMaxTokens: 1})
	if err != nil || next.ID != "eval_suite_000008" {
		t.Fatalf("the next suite must be eval_suite_000008, after the 7 the standalone store handed out: %+v, %v", next, err)
	}
	if _, err := ImportStandaloneDatabase(context.Background(), store.db, path); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("a second import must be refused, got %v", err)
	}
}

func TestImportRefusesAnIdThatIsNotItsNumber(t *testing.T) {
	store := openTestStore(t)
	path := writeStandaloneDatabase(t, `INSERT INTO suites (seq, id, name, task_kind, judge_instructions, judge_failure_kinds, output_schema,
		max_tokens, judge_max_tokens, created_at, updated_at) VALUES (1, 'eval_suite_000002', 'x', 'x', 'x', '[]', '{}', 1, 1, 1, 1)`)
	if _, err := ImportStandaloneDatabase(context.Background(), store.db, path); err == nil || !strings.Contains(err.Error(), "sequence number") {
		t.Fatalf("want the mismatched id refused, got %v", err)
	}
}

func TestImportRefusesATableItDoesNotKnow(t *testing.T) {
	store := openTestStore(t)
	path := writeStandaloneDatabase(t, `CREATE TABLE notes (id INTEGER)`)
	if _, err := ImportStandaloneDatabase(context.Background(), store.db, path); err == nil || !strings.Contains(err.Error(), "notes") {
		t.Fatalf("want the unknown table refused, got %v", err)
	}
}
