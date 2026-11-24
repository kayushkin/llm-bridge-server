package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// newTokensDB builds the legacy tokens database handleUsage reads. The live schema
// (model-store's store.db) declares orchestrator NOT NULL, so this helper takes the
// column definition as an argument: the NULL-splitting defect is only reachable on a
// schema where the column is nullable.
func newTokensDB(t *testing.T, orchestratorCol string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE usage (
		date          TEXT NOT NULL,
		agent         TEXT NOT NULL,
		orchestrator  ` + orchestratorCol + `,
		model         TEXT NOT NULL,
		provider      TEXT NOT NULL DEFAULT '',
		input_tokens  INTEGER DEFAULT 0,
		output_tokens INTEGER DEFAULT 0,
		requests      INTEGER DEFAULT 0,
		cost_usd      REAL DEFAULT 0
	)`)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return path
}

func insertUsage(t *testing.T, path string, rows [][]any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO usage
			(date, agent, orchestrator, model, input_tokens, output_tokens, requests, cost_usd)
			VALUES (date('now'), ?, ?, ?, ?, ?, ?, ?)`, r...); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
}

type usageRow struct {
	Agent        string  `json:"agent"`
	Orchestrator string  `json:"orchestrator"`
	Model        string  `json:"model"`
	Messages     int     `json:"messages"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

func fetchUsage(t *testing.T, path string) map[string][]usageRow {
	t.Helper()
	srv := &Server{tokensDBPath: path}
	rec := httptest.NewRecorder()
	srv.handleUsage(rec, httptest.NewRequest(http.MethodGet, "/api/usage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string][]usageRow
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

// The defect: the projection substituted 'inber' for NULL but the grouping used the
// raw column, so NULL rows and literal-'inber' rows formed two groups that both
// rendered as "inber" — the same agent/orchestrator/model twice, totals divided.
func TestUsageStatsDoesNotSplitNullAndLiteralOrchestrator(t *testing.T) {
	path := newTokensDB(t, "TEXT")
	insertUsage(t, path, [][]any{
		{"worker", nil, "sonnet", 100, 10, 1, 0.50},
		{"worker", "inber", "sonnet", 200, 20, 2, 1.25},
	})

	day := fetchUsage(t, path)["day"]
	if len(day) != 1 {
		t.Fatalf("got %d rows %+v, want 1 — NULL and 'inber' must not form separate groups", len(day), day)
	}
	got := day[0]
	if got.Orchestrator != "inber" {
		t.Errorf("orchestrator = %q, want %q", got.Orchestrator, "inber")
	}
	// Totals must be the sum of both rows, not either half.
	if got.InputTokens != 300 || got.OutputTokens != 30 || got.Messages != 3 {
		t.Errorf("totals = in %d/out %d/msgs %d, want 300/30/3 — the group was split",
			got.InputTokens, got.OutputTokens, got.Messages)
	}
	if got.TotalTokens != 330 {
		t.Errorf("total_tokens = %d, want 330", got.TotalTokens)
	}
	if got.CostUSD != 1.75 {
		t.Errorf("cost_usd = %v, want 1.75", got.CostUSD)
	}
}

// Distinct orchestrators must still be reported separately — the fix must merge the
// placeholder with its literal, not collapse everything.
func TestUsageStatsKeepsDistinctOrchestratorsApart(t *testing.T) {
	path := newTokensDB(t, "TEXT NOT NULL DEFAULT 'inber'")
	insertUsage(t, path, [][]any{
		{"worker", "inber", "sonnet", 100, 10, 1, 0.5},
		{"worker", "claude-code", "sonnet", 200, 20, 2, 1.0},
	})

	day := fetchUsage(t, path)["day"]
	if len(day) != 2 {
		t.Fatalf("got %d rows %+v, want 2 distinct orchestrators", len(day), day)
	}
	seen := map[string]int64{}
	for _, r := range day {
		seen[r.Orchestrator] = r.InputTokens
	}
	if seen["inber"] != 100 || seen["claude-code"] != 200 {
		t.Errorf("per-orchestrator totals = %v, want inber 100 / claude-code 200", seen)
	}
}

func TestUsageStatsReportsAllThreePeriods(t *testing.T) {
	path := newTokensDB(t, "TEXT NOT NULL DEFAULT 'inber'")
	insertUsage(t, path, [][]any{{"worker", "inber", "sonnet", 1, 1, 1, 0.1}})

	out := fetchUsage(t, path)
	for _, period := range []string{"day", "week", "month"} {
		rows, ok := out[period]
		if !ok {
			t.Errorf("period %q missing from response", period)
			continue
		}
		if len(rows) != 1 {
			t.Errorf("period %q: got %d rows, want 1", period, len(rows))
		}
	}
}

// A row that cannot be scanned must not be dropped from the totals — that is the
// `continue` commit 6382825 removed elsewhere, and it under-reports spend silently.
//
// The induced failure is a NULL in a column scanned into a string, i.e. exactly the
// schema mismatch that commit names. Note it cannot be induced through the numeric
// columns: they are read through SUM(), and SQLite coerces a non-numeric value to 0
// rather than failing, so a corrupt integer column under-reports with no error at
// all — a separate hole this endpoint cannot currently see.
func TestUsageStatsFailsLoudlyOnAnUnscannableRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE usage (
		date TEXT NOT NULL, agent TEXT, orchestrator TEXT, model TEXT NOT NULL,
		input_tokens INTEGER, output_tokens INTEGER, requests INTEGER, cost_usd REAL)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO usage VALUES (date('now'), NULL, 'inber', 'sonnet', 1, 1, 1, 0.1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	db.Close()

	srv := &Server{tokensDBPath: path}
	rec := httptest.NewRecorder()
	srv.handleUsage(rec, httptest.NewRequest(http.MethodGet, "/api/usage", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("unscannable row reported as %d %s; want an error rather than a silently smaller total",
			rec.Code, rec.Body.String())
	}
}

// A broken tokens DB must not read as "no usage this month". Silent emptiness on a
// spend surface is indistinguishable from a quiet week.
func TestUsageStatsFailsLoudlyOnAnUnreadableTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated (x INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	db.Close()

	srv := &Server{tokensDBPath: path}
	rec := httptest.NewRecorder()
	srv.handleUsage(rec, httptest.NewRequest(http.MethodGet, "/api/usage", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("missing usage table reported as %d %s; want an error status",
			rec.Code, rec.Body.String())
	}
}
