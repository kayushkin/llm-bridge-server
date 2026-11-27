package rules

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// standaloneSchema is the standalone rule-store's tables as its last version
// made them.
const standaloneSchema = `
CREATE TABLE rules (id TEXT PRIMARY KEY, seq INTEGER NOT NULL UNIQUE, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL, state TEXT NOT NULL, active_revision_id TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE rule_revisions (id TEXT PRIMARY KEY, seq INTEGER NOT NULL UNIQUE, rule_id TEXT NOT NULL REFERENCES rules(id),
	revision INTEGER NOT NULL, definition TEXT NOT NULL, note TEXT NOT NULL DEFAULT '', created_by TEXT NOT NULL,
	created_at INTEGER NOT NULL, UNIQUE (rule_id, revision));
CREATE TABLE rule_activations (id TEXT PRIMARY KEY, seq INTEGER NOT NULL UNIQUE, rule_id TEXT NOT NULL REFERENCES rules(id),
	action TEXT NOT NULL, revision_id TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL DEFAULT '', acted_by TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '', acted_at INTEGER NOT NULL);
CREATE TABLE subjects (subject_type TEXT NOT NULL, subject_id TEXT NOT NULL, state TEXT NOT NULL, state_episode INTEGER NOT NULL,
	state_observed_at INTEGER NOT NULL, snapshot TEXT NOT NULL, observed_at INTEGER NOT NULL, PRIMARY KEY (subject_type, subject_id));
CREATE TABLE rule_runs (id TEXT PRIMARY KEY, seq INTEGER NOT NULL UNIQUE, rule_id TEXT NOT NULL REFERENCES rules(id),
	rule_revision_id TEXT NOT NULL REFERENCES rule_revisions(id), subject_type TEXT NOT NULL, subject_id TEXT NOT NULL,
	trigger_key TEXT NOT NULL, mode TEXT NOT NULL, cause TEXT NOT NULL, facts TEXT NOT NULL, planned_effects TEXT NOT NULL,
	outcome TEXT NOT NULL, decided_at INTEGER NOT NULL, UNIQUE (rule_revision_id, subject_type, subject_id, trigger_key));
`

func writeStandaloneRuleStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rule-store.db")
	file, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, statement := range []string{
		standaloneSchema,
		`INSERT INTO rules VALUES ('rule_000004', 4, 'curator done', '', 'session', 'active', 'rule_revision_000007', 'shadow', 'principal_000001', 1795000000, 1795000100)`,
		`INSERT INTO rule_revisions VALUES ('rule_revision_000007', 7, 'rule_000004', 1, '{"conditions":[],"effects":[]}', '', 'principal_000001', 1795000050)`,
		`INSERT INTO rule_activations VALUES ('rule_activation_000009', 9, 'rule_000004', 'activate', 'rule_revision_000007', 'shadow', 'principal_000001', 'compare', 1795000100)`,
		`INSERT INTO subjects VALUES ('session', 'br_1', 'completed', 3, 1795000200, '{"session_id":"br_1","z":1,"a":2}', 1795000300)`,
		`INSERT INTO rule_runs VALUES ('rule_run_000012', 12, 'rule_000004', 'rule_revision_000007', 'session', 'br_1', 'completed#3', 'shadow', 'feed', '{}', '[]', 'shadowed', 1795000400)`,
	} {
		if _, err := file.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

func TestImportStandaloneDatabaseKeepsEveryIdAndGoesOnNumbering(t *testing.T) {
	store := newTestStore(t)
	counts, err := ImportStandaloneDatabase(context.Background(), store.db, writeStandaloneRuleStore(t))
	if err != nil {
		t.Fatal(err)
	}
	if counts != (ImportCounts{Rules: 1, Revisions: 1, Activations: 1, Subjects: 1, Runs: 1}) {
		t.Fatalf("counts = %+v", counts)
	}
	rule, err := store.GetRule("rule_000004")
	if err != nil {
		t.Fatal(err)
	}
	if rule.ActiveRevisionID != "rule_revision_000007" || rule.Mode != "shadow" || len(rule.Revisions) != 1 || len(rule.Activations) != 1 {
		t.Errorf("rule = %+v", rule)
	}
	subject, err := store.GetSubject(SubjectTypeSession, "br_1")
	if err != nil {
		t.Fatal(err)
	}
	if subject.StateEpisode != 3 || string(subject.Snapshot) != `{"session_id":"br_1","z":1,"a":2}` {
		t.Errorf("subject = %+v; the snapshot must be kept verbatim", subject)
	}
	created, err := store.CreateRule("next", "", SourceSession, testAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "rule_000005" {
		t.Errorf("a new rule took id %s, want rule_000005", created.ID)
	}
	if _, err := ImportStandaloneDatabase(context.Background(), store.db, writeStandaloneRuleStore(t)); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("a second import = %v, want a refusal of a module that holds rows", err)
	}
}
