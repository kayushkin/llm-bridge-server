package permissions

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newStandaloneDatabase writes a SQLite file the way the standalone store kept
// one: an edited default, a disabled rule people added, a default left as it
// was, and more audit rows than one insert carries, two of them with the same
// ts.
func newStandaloneDatabase(t *testing.T, auditRows int, extraStatements ...string) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "permission-store.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("standalone schema: %v", err)
	}
	statements := append([]string{
		`INSERT INTO rules VALUES ('seed-v1-allow-Read', 'global', 100, 'Read', '', 'allow', 'default seed: read-only tool', 1, '2026-05-05T10:00:00-07:00', '2026-05-05T10:00:00-07:00')`,
		`INSERT INTO rules VALUES ('seed-v1-deny-bash-curl-external', 'global', 50, 'Bash', '^curl\b', 'deny', 'edited', 0, '2026-05-05T10:00:00-07:00', '2026-06-01T09:30:00.5-07:00')`,
		`INSERT INTO rules VALUES ('multichat-send-message-ask', 'global', 10, 'mcp__multichat__send_message', '', 'ask', 'a person reads it', 1, '2026-08-01T12:00:00Z', '2026-08-01T12:00:00Z')`,
		`INSERT INTO rules VALUES ('rule_1754000000000000000_abcd1234', 'instance:inst-a', 200, 'Bash', '^make\b', 'allow', '', 0, '2026-08-02T12:00:00Z', '2026-08-02T12:00:00Z')`,
	}, extraStatements...)
	for i := 1; i <= auditRows; i++ {
		ts := fmt.Sprintf("2026-09-01T10:%02d:%02d-07:00", i/60%60, i%60)
		if i == 2 {
			ts = "2026-09-01T10:00:01-07:00" // the same ts as row 1
		}
		statements = append(statements, fmt.Sprintf(`INSERT INTO audit VALUES ('aud_%d', '%s', 'br_%d', '', 'Bash', '{"command":"ls"}', '[{"kind":"call"}]', 'allow', 'seed-v1-allow-bash-ls', '')`, i, ts, i))
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

func TestImportReplacesTheDefaultsWithTheFilesRulesAndCopiesEveryAuditRow(t *testing.T) {
	s := newSeededTestStore(t)
	counts, err := ImportStandaloneDatabase(testContext, s.db, newStandaloneDatabase(t, 1201))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if want := (ImportCounts{Rules: 4, AuditEntries: 1201, DefaultRulesReplaced: 18}); counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}
	rules, err := s.ListRules(testContext, "")
	if err != nil || len(rules) != 4 {
		t.Fatalf("rules = %+v, %v", rules, err)
	}
	edited, err := s.GetRule(testContext, "seed-v1-deny-bash-curl-external")
	if err != nil || edited.Enabled || edited.Message != "edited" || edited.UpdatedAt != "2026-06-01T09:30:00.5-07:00" || edited.CreatedAt != "2026-05-05T10:00:00-07:00" || edited.Pattern != `^curl\b` {
		t.Fatalf("edited default = %+v, %v", edited, err)
	}
	if _, err := s.GetRule(testContext, "seed-v1-allow-Glob"); err != sql.ErrNoRows {
		t.Fatalf("a default the file did not hold is gone: %v", err)
	}
	entries, err := s.ListAudit(testContext, ListAuditFilter{Limit: 2000})
	if err != nil || len(entries) != 1201 {
		t.Fatalf("audit = %d rows, %v", len(entries), err)
	}
	// Newest first; rows 1 and 2 share a ts, and the later-written comes first.
	last, secondLast := entries[len(entries)-1], entries[len(entries)-2]
	if last.ID != "aud_1" || secondLast.ID != "aud_2" {
		t.Fatalf("the tie = %s, %s; want aud_2 before aud_1", secondLast.ID, last.ID)
	}
	if string(last.InputJSON) != `{"command":"ls"}` || last.BridgeID != "br_1" || last.MatchedRuleID != "seed-v1-allow-bash-ls" {
		t.Fatalf("an audit row = %+v", last)
	}
	if _, err := ImportStandaloneDatabase(testContext, s.db, newStandaloneDatabase(t, 1)); err == nil || !strings.Contains(err.Error(), "freshly migrated module only") {
		t.Fatalf("a second import: %v", err)
	}
}

func TestImportRefusesWhatItCannotCopyWhole(t *testing.T) {
	s := newSeededTestStore(t)
	extraColumn := newStandaloneDatabase(t, 1, `ALTER TABLE rules ADD COLUMN principal_id TEXT NOT NULL DEFAULT ''`)
	if _, err := ImportStandaloneDatabase(testContext, s.db, extraColumn); err == nil || !strings.Contains(err.Error(), "copy whole") {
		t.Fatalf("an unknown column: %v", err)
	}
	badFlag := newStandaloneDatabase(t, 1, `UPDATE rules SET enabled = 2 WHERE id = 'seed-v1-allow-Read'`)
	if _, err := ImportStandaloneDatabase(testContext, s.db, badFlag); err == nil || !strings.Contains(err.Error(), "neither 0 nor 1") {
		t.Fatalf("a bad enabled flag: %v", err)
	}
	if rules, _ := s.ListRules(testContext, ""); len(rules) != 18 {
		t.Fatalf("a refused import left %d rules, want the 18 defaults untouched", len(rules))
	}

	added := newSeededTestStore(t)
	if _, err := added.CreateRule(testContext, Rule{Scope: ScopeGlobal, Tool: "Bash", Outcome: OutcomeAsk, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportStandaloneDatabase(testContext, added.db, newStandaloneDatabase(t, 1)); err == nil || !strings.Contains(err.Error(), "not its untouched defaults") {
		t.Fatalf("a module with a rule someone added: %v", err)
	}
}
