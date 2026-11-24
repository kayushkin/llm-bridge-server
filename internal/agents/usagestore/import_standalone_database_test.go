package usagestore

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

// writeStandaloneDatabase makes a standalone usage-store file holding rows
// written by statements, all made-up data.
func writeStandaloneDatabase(t *testing.T, statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
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

func TestImportCopiesEveryTableAndNumbersOnFromTheStandaloneStore(t *testing.T) {
	s := newTestStore(t)
	path := writeStandaloneDatabase(t,
		`INSERT INTO limit_snapshots (id, snapshot_at, provider, window_key, used_percent, window_minutes, resets_at, plan_type, tier, source, raw_json)
			VALUES (7, 100, 'anthropic', 'seven_day', 42.5, NULL, 900, 'max', 'tier-a', 'api', '{"a":1}'),
			       (9, 100, 'anthropic', 'five_hour', 12, 300, NULL, 'max', 'tier-a', 'api', '{"a":1}')`,
		`INSERT INTO spend_keys VALUES ('anthropic', 'key_1', 'build key', 'sk-…abcd', 'active', '{"k":1}', 50)`,
		`INSERT INTO spend_daily VALUES ('anthropic', 'key_1', '2026-01-02', 1.25, 60)`,
		`INSERT INTO spend_topups (id, provider, amount_usd, occurred_at, note, created_at) VALUES (4, 'anthropic', 20, 70, 'made up', 71)`,
		`INSERT INTO spend_snapshots (id, fetched_at, provider, window, period_start, period_end, total_usd, currency, source, raw_json)
			VALUES (2, 80, 'anthropic', '30d', 1, 2, 3.5, 'USD', 'api', NULL)`,
		// A topup the standalone store handed out and then deleted.
		`UPDATE sqlite_sequence SET seq = 6 WHERE name = 'spend_topups'`,
	)
	counts, err := ImportStandaloneDatabase(context.Background(), s.db, path)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want := ImportCounts{LimitSnapshots: 2, LastLimitSnapshotNumber: 9, SpendKeys: 1, SpendDays: 1, SpendTopups: 1, LastSpendTopupNumber: 6, SpendSnapshots: 1}
	if counts != want {
		t.Fatalf("counts: got %+v, want %+v", counts, want)
	}

	latest, err := s.LatestLimits("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if latest.SnapshotAt != 100 || latest.Windows["seven_day"].UsedPercent != 42.5 || *latest.Windows["seven_day"].ResetsAt != 900 ||
		*latest.Windows["five_hour"].WindowMinutes != 300 || latest.Tier != "tier-a" {
		t.Fatalf("latest limits after the import: %+v", latest)
	}
	raw, err := s.LatestKeyRaw("anthropic", "key_1")
	if err != nil || raw != `{"k":1}` {
		t.Fatalf("raw key json: %q, %v", raw, err)
	}
	total, err := s.SumSpendSince("anthropic", "", 0)
	if err != nil || total != 1.25 {
		t.Fatalf("spend: %v, %v", total, err)
	}
	topups, err := s.ListTopups("anthropic")
	if err != nil || len(topups) != 1 || topups[0].ID != 4 || topups[0].Note != "made up" {
		t.Fatalf("topups: %+v, %v", topups, err)
	}
	next, err := s.AddTopup(Topup{Provider: "anthropic", AmountUSD: 5, OccurredAt: 90})
	if err != nil || next != 7 {
		t.Fatalf("the next topup must be 7, after the 6 the standalone store handed out: %d, %v", next, err)
	}
	var snapshotID int64
	if err := s.db.QueryRow(`INSERT INTO limit_snapshots (snapshot_at, provider, window_key, used_percent, source) VALUES (1, 'codex', 'weekly', 1, 'app-server') RETURNING id`).Scan(&snapshotID); err != nil || snapshotID != 10 {
		t.Fatalf("the next snapshot must be 10: %d, %v", snapshotID, err)
	}

	if _, err := ImportStandaloneDatabase(context.Background(), s.db, path); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("a second import must be refused, got %v", err)
	}
}

func TestImportRefusesTokenUsageRowsItWouldDrop(t *testing.T) {
	s := newTestStore(t)
	path := writeStandaloneDatabase(t, `INSERT INTO usage (date, agent, model) VALUES ('2026-01-01', 'agent-a', 'model-a')`)
	_, err := ImportStandaloneDatabase(context.Background(), s.db, path)
	if err == nil || !strings.Contains(err.Error(), "token usage") {
		t.Fatalf("want the token usage rows refused, got %v", err)
	}
}

func TestImportRefusesATableItDoesNotKnow(t *testing.T) {
	s := newTestStore(t)
	path := writeStandaloneDatabase(t, `CREATE TABLE notes (id INTEGER)`)
	_, err := ImportStandaloneDatabase(context.Background(), s.db, path)
	if err == nil || !strings.Contains(err.Error(), "notes") {
		t.Fatalf("want the unknown table refused, got %v", err)
	}
}
