package db

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// A database made before the rename has a model column. Opening it renames the
// column and keeps what it held, so GET shows the value the executor will now
// send, and a stale one fails its next run loudly instead of vanishing.
func TestOpeningAnOldDatabaseRenamesModelToModelRole(t *testing.T) {
	d, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	d.SetMaxOpenConns(1)
	if _, err := d.Exec(`CREATE TABLE jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, schedule TEXT NOT NULL,
		command TEXT NOT NULL DEFAULT '', type TEXT NOT NULL DEFAULT 'shell', agent TEXT NOT NULL DEFAULT '',
		prompt TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', orchestrator TEXT NOT NULL DEFAULT '',
		session_id TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO jobs (name, schedule, type, agent, prompt, model) VALUES ('old', '0 5 * * *', 'agent', 'claude-code', 'p', 'balanced');`); err != nil {
		t.Fatal(err)
	}
	store, err := NewWithDB(d)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	jobs, err := store.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ModelRole != "balanced" {
		t.Fatalf("jobs = %+v, want the old model value under model_role", jobs)
	}
	if has, _ := store.jobsColumnExists("model"); has {
		t.Error("the model column is still there")
	}
	// Opening it again is a no-op.
	if err := store.migrateModelToModelRole(); err != nil {
		t.Fatal(err)
	}
}
