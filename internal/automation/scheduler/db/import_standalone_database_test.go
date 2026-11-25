package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// standaloneSchema is the standalone scheduler's tables as its last version
// left them.
const standaloneSchema = `
CREATE TABLE jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, schedule TEXT NOT NULL,
	command TEXT NOT NULL DEFAULT '', type TEXT NOT NULL DEFAULT 'shell', agent TEXT NOT NULL DEFAULT '',
	prompt TEXT NOT NULL DEFAULT '', model_role TEXT NOT NULL DEFAULT '', orchestrator TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	workspace_id TEXT NOT NULL DEFAULT '', hold_until DATETIME, description TEXT NOT NULL DEFAULT '',
	timeout_seconds INTEGER NOT NULL DEFAULT 0, event_trigger TEXT NOT NULL DEFAULT '');
CREATE TABLE runs (
	id INTEGER PRIMARY KEY AUTOINCREMENT, job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	status TEXT NOT NULL DEFAULT 'running', output TEXT NOT NULL DEFAULT '',
	started_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, finished_at DATETIME);
CREATE TABLE service_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE event_trigger_deliveries (job_id INTEGER NOT NULL, idempotency_key TEXT NOT NULL, run_id INTEGER NOT NULL,
	created_at DATETIME NOT NULL, PRIMARY KEY (job_id, idempotency_key));
`

// writeStandaloneDatabase makes a standalone scheduler's file: job 3 held and
// switched off, job 7 triggered, job 9 deleted after it ran, runs that hold
// bytes that are not text, and AUTOINCREMENT counters past every id held.
func writeStandaloneDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scheduler.db")
	file, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	statements := []string{
		standaloneSchema,
		`INSERT INTO jobs (id, name, schedule, command, enabled, created_at, updated_at, hold_until, description, timeout_seconds)
			VALUES (3, 'disk check', '*/5 * * * *', 'echo disk', 0, '2026-04-06 07:36:12.613934098 +0000 UTC', '2026-04-07 08:00:00.5 +0000 UTC',
			'2026-12-01 00:00:00 +0000 UTC', 'checks the disk', 600)`,
		`INSERT INTO jobs (id, name, schedule, type, agent, prompt, model_role, created_at, updated_at, workspace_id, event_trigger)
			VALUES (7, 'triggered', '', 'agent', 'claude-code', 'hello {{.body}}', 'balanced', '2026-06-06 04:28:13.76357553 +0000 UTC',
			'2026-06-06 04:28:13.76357553 +0000 UTC', 'workspace-1',
			'{"subject":"messaging.inbound","conditions":[{"field":"platform","equals":"discord"}]}')`,
		`INSERT INTO runs (id, job_id, status, output, started_at, finished_at) VALUES
			(10, 3, 'success', 'first', '2026-04-06 07:07:00.085459689 +0000 UTC', '2026-04-06 07:07:01.1 +0000 UTC'),
			(11, 3, 'error', CAST(X'6E6F7420746578743A00FF' AS TEXT), '2026-04-06 07:12:00.0642 +0000 UTC', '2026-04-06 07:12:02 +0000 UTC'),
			(12, 3, 'success', 'third', '2026-04-06 07:17:00 +0000 UTC', '2026-04-06 07:17:01 +0000 UTC'),
			(13, 9, 'success', 'of a deleted job', '2026-04-06 07:20:00 +0000 UTC', '2026-04-06 07:20:01 +0000 UTC'),
			(14, 7, 'running', '', '2026-11-11 22:04:53.882917262 +0000 UTC', NULL)`,
		`INSERT INTO service_settings (key, value, updated_at) VALUES ('kanban_scoper_model_role', 'efficient', '2026-09-30 01:21:44')`,
		`INSERT INTO event_trigger_deliveries VALUES (7, 'event-1', 14, '2026-11-11 22:04:53.882917262 +0000 UTC')`,
		`UPDATE sqlite_sequence SET seq = 147 WHERE name = 'jobs'`,
		`UPDATE sqlite_sequence SET seq = 1250967 WHERE name = 'runs'`,
	}
	for _, statement := range statements {
		if _, err := file.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

func TestImportStandaloneDatabaseKeepsEveryIdHoldAndRun(t *testing.T) {
	store := testStore(t)
	counts, err := ImportStandaloneDatabase(context.Background(), store.db, writeStandaloneDatabase(t), ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := ImportCounts{Jobs: 2, Runs: 5, ServiceSettings: 1, EventTriggerDeliveries: 1, LastJobNumber: 147, LastRunNumber: 1250967}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}

	held, err := store.GetJob(3)
	if err != nil {
		t.Fatal(err)
	}
	if held.Enabled || held.HoldUntil == nil || !held.HoldUntil.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) ||
		held.Description != "checks the disk" || held.TimeoutSecs != 600 ||
		!held.CreatedAt.Equal(time.Date(2026, 4, 6, 7, 36, 12, 613934000, time.UTC)) {
		t.Errorf("job 3 = %+v", held)
	}
	triggered, err := store.GetJob(7)
	if err != nil {
		t.Fatal(err)
	}
	if triggered.EventTrigger == nil || triggered.EventTrigger.Subject != "messaging.inbound" || triggered.ModelRole != "balanced" || triggered.WorkspaceID != "workspace-1" {
		t.Errorf("job 7 = %+v", triggered)
	}

	runs, err := store.ListRuns(3, MaxRunsListed)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 || runs[0].ID != 12 || runs[1].Output != "not text:\x00\xff" {
		t.Errorf("runs of job 3 = %+v", runs)
	}
	latest, err := store.LatestRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 3 || latest[2].JobID != 9 || latest[2].ID != 13 {
		t.Errorf("latest runs = %+v; the deleted job's run must stay", latest)
	}
	delivered, err := store.EventTriggerDelivered(7, "event-1")
	if err != nil || !delivered {
		t.Errorf("delivery of event-1 = %t, %v", delivered, err)
	}
	stored, err := store.ServiceSettingValues().Load()
	if err != nil || stored["kanban_scoper_model_role"] != "efficient" {
		t.Errorf("stored settings = %v, %v", stored, err)
	}

	// Numbering goes on after the standalone counters, which count deleted rows.
	created, err := store.CreateJob("new", "* * * * *", "echo")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != 148 {
		t.Errorf("a new job took id %d, want 148", created.ID)
	}
	run, err := store.CreateRun(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != 1250968 {
		t.Errorf("a new run took id %d, want 1250968", run.ID)
	}
	// The run job 7 had going is still going: the next start closes it.
	interrupted, err := store.ReconcileOrphanedRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(interrupted) != 2 || interrupted[0].RunID != 14 {
		t.Errorf("interrupted = %+v", interrupted)
	}
}

func TestImportStandaloneDatabaseCanCopyOnlyTheNewestRunsOfEachJob(t *testing.T) {
	store := testStore(t)
	counts, err := ImportStandaloneDatabase(context.Background(), store.db, writeStandaloneDatabase(t), ImportOptions{NewestRunsPerJob: 2})
	if err != nil {
		t.Fatal(err)
	}
	if counts.Runs != 4 || counts.RunsLeftBehind != 1 {
		t.Fatalf("copied %d runs and left %d behind, want 4 and 1", counts.Runs, counts.RunsLeftBehind)
	}
	runs, err := store.ListRuns(3, MaxRunsListed)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != 12 || runs[1].ID != 11 {
		t.Errorf("runs of job 3 = %+v, want 12 and 11", runs)
	}
}

func TestImportStandaloneDatabaseRefusesAModuleThatHoldsRows(t *testing.T) {
	store := testStore(t)
	if _, err := store.CreateJob("already here", "* * * * *", "echo"); err != nil {
		t.Fatal(err)
	}
	_, err := ImportStandaloneDatabase(context.Background(), store.db, writeStandaloneDatabase(t), ImportOptions{})
	if err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("err = %v, want a refusal of a module that holds rows", err)
	}
}

// An older file still has session_target, which the scheduler stopped
// reading; it is left behind when every row holds its default, and refused
// otherwise.
func TestImportStandaloneDatabaseLeavesSessionTargetBehindOnlyWhenEveryRowHoldsItsDefault(t *testing.T) {
	path := writeStandaloneDatabase(t)
	file, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Exec(`ALTER TABLE jobs ADD COLUMN session_target TEXT NOT NULL DEFAULT 'isolated'`); err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	if counts, err := ImportStandaloneDatabase(context.Background(), store.db, path, ImportOptions{}); err != nil || counts.Jobs != 2 {
		t.Fatalf("import with session_target at its default = %+v, %v", counts, err)
	}
	if _, err := file.Exec(`UPDATE jobs SET session_target = 'shared' WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := ImportStandaloneDatabase(context.Background(), testStore(t).db, path, ImportOptions{}); err == nil || !strings.Contains(err.Error(), "session_target") {
		t.Errorf("import with a session_target that is not its default = %v, want a refusal", err)
	}
}

func TestImportStandaloneDatabaseRefusesAColumnItDoesNotKnow(t *testing.T) {
	path := writeStandaloneDatabase(t)
	file, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Exec(`ALTER TABLE jobs ADD COLUMN surprise TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	file.Close()
	store := testStore(t)
	if _, err := ImportStandaloneDatabase(context.Background(), store.db, path, ImportOptions{}); err == nil {
		t.Fatal("a jobs table with a column the import does not know was imported")
	}
	jobs, err := store.ListJobs()
	if err != nil || len(jobs) != 0 {
		t.Errorf("after a refused import the module holds %d jobs (%v)", len(jobs), err)
	}
}
