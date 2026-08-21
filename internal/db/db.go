package db

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Job struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Description  string     `json:"description,omitempty"`
	Schedule     string     `json:"schedule"`
	Command      string     `json:"command"`
	Type         string     `json:"type"`                   // "shell" or "agent"
	Agent        string     `json:"agent,omitempty"`        // agent name (for type=agent)
	Prompt       string     `json:"prompt,omitempty"`       // prompt text (for type=agent)
	Model        string     `json:"model,omitempty"`        // model override (for type=agent)
	Orchestrator string     `json:"orchestrator,omitempty"` // "claude-code", "inber", etc. (for type=agent)
	SessionID    string     `json:"session_id,omitempty"`   // session to resume (for type=agent)
	WorkspaceID  string     `json:"workspace_id,omitempty"` // noteboard workspace holding this job's working memory (for type=agent)
	TimeoutSecs  int        `json:"timeout_seconds"`        // per-job wall-clock cap; 0 means executor.DefaultTimeout
	Enabled      bool       `json:"enabled"`
	HoldUntil    *time.Time `json:"hold_until,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type Run struct {
	ID         int64      `json:"id"`
	JobID      int64      `json:"job_id"`
	Status     string     `json:"status"`
	Output     string     `json:"output"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type Store struct {
	db *sql.DB
}

// busyTimeoutMillisecondsWanted is how long a connection waits for a lock
// before giving up. Named so the DSN that sets it and the check that proves it
// took effect cannot drift apart.
const busyTimeoutMillisecondsWanted = 5000

// dataSourceName builds the sqlite DSN for a database path.
//
// busy_timeout MUST be set here rather than with a PRAGMA statement after
// Open: it is a per-connection setting, and *sql.DB is a pool. A one-shot
// db.Exec("PRAGMA busy_timeout=5000") reaches only whichever connection the
// pool happened to hand out, so every other connection keeps SQLite's default
// of 0 and fails instantly on a lock instead of waiting. Measured on this
// driver holding 8 connections open at once: 7 of the 8 never saw it.
//
// The SetMaxOpenConns(1) cap below is what let the old Exec form work here —
// one connection, so the pragma reached all of it. That is a property of the
// cap, not of the pragma, and it stops holding the moment that connection is
// replaced. Cron ticks racing API writes is exactly the case the cap exists
// for, so the timeout going missing would fail job-run inserts silently.
//
// _pragma=... is modernc's syntax and it is the only one that works here. The
// mattn/go-sqlite3 spelling (_busy_timeout=5000) is silently ignored by this
// driver, as is any other unrecognised key — hence the check in New.
func dataSourceName(dbPath string) string {
	return fmt.Sprintf("%s?_pragma=busy_timeout(%d)", dbPath, busyTimeoutMillisecondsWanted)
}

func New(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	d, err := sql.Open("sqlite", dataSourceName(dbPath))
	if err != nil {
		return nil, err
	}
	// journal_mode is the one pragma that belongs here rather than in the DSN:
	// it is a property of the database file, so setting it once is permanent.
	// busy_timeout is per-connection and lives in dataSourceName.
	if _, err := d.Exec("PRAGMA journal_mode=WAL"); err != nil {
		d.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	// An unrecognised DSN key is silently ignored by this driver, so prove the
	// setting actually took effect instead of trusting the connection string.
	var busyTimeoutMilliseconds int
	if err := d.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeoutMilliseconds); err != nil {
		d.Close()
		return nil, fmt.Errorf("read busy_timeout pragma: %w", err)
	}
	if busyTimeoutMilliseconds != busyTimeoutMillisecondsWanted {
		d.Close()
		return nil, fmt.Errorf("busy_timeout is %d, want %d: the DSN did not take effect",
			busyTimeoutMilliseconds, busyTimeoutMillisecondsWanted)
	}
	// Single connection serializes writes through Go's sql pool. Without this,
	// modernc.org/sqlite hits SQLITE_BUSY (5) when the cron tick races user
	// API writes (create run / clear holds), failing job-run inserts silently.
	d.SetMaxOpenConns(1)

	s := &Store{db: d}
	if err := s.migrate(); err != nil {
		d.Close()
		return nil, err
	}
	return s, nil
}

func NewWithDB(d *sql.DB) (*Store, error) {
	s := &Store{db: d}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			schedule TEXT NOT NULL,
			command TEXT NOT NULL DEFAULT '',
			type TEXT NOT NULL DEFAULT 'shell',
			agent TEXT NOT NULL DEFAULT '',
			prompt TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			orchestrator TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
			status TEXT NOT NULL DEFAULT 'running',
			output TEXT NOT NULL DEFAULT '',
			started_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			finished_at DATETIME
		);
	`)
	if err != nil {
		return err
	}
	// Add new columns to existing tables (ignore errors if columns already exist)
	for _, col := range []string{
		"ALTER TABLE jobs ADD COLUMN type TEXT NOT NULL DEFAULT 'shell'",
		"ALTER TABLE jobs ADD COLUMN agent TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE jobs ADD COLUMN prompt TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE jobs ADD COLUMN model TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE jobs ADD COLUMN orchestrator TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE jobs ADD COLUMN session_id TEXT NOT NULL DEFAULT ''",
		// A recurring agent job's DURABLE memory, as a noteboard workspace id.
		// session_id above is its conversation, which compaction erodes and a
		// reap discards; the workspace is what actually survives, so the
		// workspace is the source of truth and the session is an optimisation.
		"ALTER TABLE jobs ADD COLUMN workspace_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE jobs ADD COLUMN hold_until DATETIME",
		"ALTER TABLE jobs ADD COLUMN description TEXT NOT NULL DEFAULT ''",
		// Per-job wall-clock cap. 0 keeps the executor default; a long-running
		// job (autoworker, the nightly guards) sets its own instead of being
		// held to one global 5 minutes.
		"ALTER TABLE jobs ADD COLUMN timeout_seconds INTEGER NOT NULL DEFAULT 0",
	} {
		s.db.Exec(col)
	}
	return nil
}

// ReconcileOrphanedRuns closes out runs left mid-flight by a process that died
// (a crash, an OOM kill, a deploy restart). Nothing else ever clears them, so
// without this they stay "running" forever and read as concurrent duplicates of
// the job. Safe to call only at startup, before the cron loop can create runs.
func (s *Store) ReconcileOrphanedRuns() (int64, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`UPDATE runs SET status='interrupted', finished_at=?,
		 output = CASE WHEN output='' THEN 'interrupted: scheduler exited while this run was in flight' ELSE output END
		 WHERE status='running'`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) CreateJob(name, schedule, command string) (*Job, error) {
	return s.CreateJobFull(Job{
		Name:     name,
		Schedule: schedule,
		Command:  command,
		Type:     "shell",
	})
}

func (s *Store) CreateJobFull(j Job) (*Job, error) {
	now := time.Now().UTC()
	if j.Type == "" {
		j.Type = "shell"
	}
	res, err := s.db.Exec(
		`INSERT INTO jobs (name, description, schedule, command, type, agent, prompt, model, orchestrator, session_id, workspace_id, timeout_seconds, enabled, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,1,?,?)`,
		j.Name, j.Description, j.Schedule, j.Command, j.Type, j.Agent, j.Prompt, j.Model, j.Orchestrator, j.SessionID, j.WorkspaceID, j.TimeoutSecs, now, now,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	j.ID = id
	j.Enabled = true
	j.CreatedAt = now
	j.UpdatedAt = now
	return &j, nil
}

func (s *Store) ListJobs() ([]Job, error) {
	rows, err := s.db.Query(`SELECT id, name, description, schedule, command, type, agent, prompt, model, orchestrator, session_id, workspace_id, timeout_seconds, enabled, hold_until, created_at, updated_at FROM jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		var j Job
		var enabled int
		if err := rows.Scan(&j.ID, &j.Name, &j.Description, &j.Schedule, &j.Command, &j.Type, &j.Agent, &j.Prompt, &j.Model, &j.Orchestrator, &j.SessionID, &j.WorkspaceID, &j.TimeoutSecs, &enabled, &j.HoldUntil, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		j.Enabled = enabled != 0
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *Store) GetJob(id int64) (*Job, error) {
	var j Job
	var enabled int
	err := s.db.QueryRow(`SELECT id, name, description, schedule, command, type, agent, prompt, model, orchestrator, session_id, workspace_id, timeout_seconds, enabled, hold_until, created_at, updated_at FROM jobs WHERE id=?`, id).
		Scan(&j.ID, &j.Name, &j.Description, &j.Schedule, &j.Command, &j.Type, &j.Agent, &j.Prompt, &j.Model, &j.Orchestrator, &j.SessionID, &j.WorkspaceID, &j.TimeoutSecs, &enabled, &j.HoldUntil, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return nil, err
	}
	j.Enabled = enabled != 0
	return &j, nil
}

// ErrEmptyJobPatch means the caller asked to update a job but named no column.
// It is an error rather than a silent no-op because the only way to send one is
// to have meant something the store could not see — the exact failure that let
// `PATCH {"prompt": "..."}` return success while changing nothing.
var ErrEmptyJobPatch = errors.New("job patch names no field to update")

// JobPatch is a partial update to a job: a nil field leaves that column alone.
// It carries every column a caller may change. The ones it deliberately omits:
// id, created_at and updated_at belong to the store, and hold_until has its own
// SetHold/ClearHold pair because a hold is a state with its own lifecycle, not
// an ordinary field edit.
type JobPatch struct {
	Name         *string
	Description  *string
	Schedule     *string
	Command      *string
	Type         *string
	Agent        *string
	Prompt       *string
	Model        *string
	Orchestrator *string
	SessionID    *string
	WorkspaceID  *string
	TimeoutSecs  *int
	Enabled      *bool
}

// patchColumns is the patchable column set, paired with the reader that pulls
// each column's new value out of a JobPatch. It is the single place that maps a
// JobPatch field to its column: a field missing from here is a field UpdateJob
// silently declines to write, which is the bug this type replaced. The api
// package's TestJobPatchRequestCoversEveryPatchableColumn holds this list and
// the HTTP request shape in step.
var patchColumns = []struct {
	Name string
	Read func(JobPatch) (any, bool)
}{
	{"name", func(p JobPatch) (any, bool) { return patchValue(p.Name) }},
	{"description", func(p JobPatch) (any, bool) { return patchValue(p.Description) }},
	{"schedule", func(p JobPatch) (any, bool) { return patchValue(p.Schedule) }},
	{"command", func(p JobPatch) (any, bool) { return patchValue(p.Command) }},
	{"type", func(p JobPatch) (any, bool) { return patchValue(p.Type) }},
	{"agent", func(p JobPatch) (any, bool) { return patchValue(p.Agent) }},
	{"prompt", func(p JobPatch) (any, bool) { return patchValue(p.Prompt) }},
	{"model", func(p JobPatch) (any, bool) { return patchValue(p.Model) }},
	{"orchestrator", func(p JobPatch) (any, bool) { return patchValue(p.Orchestrator) }},
	{"session_id", func(p JobPatch) (any, bool) { return patchValue(p.SessionID) }},
	{"workspace_id", func(p JobPatch) (any, bool) { return patchValue(p.WorkspaceID) }},
	{"timeout_seconds", func(p JobPatch) (any, bool) { return patchValue(p.TimeoutSecs) }},
	{"enabled", func(p JobPatch) (any, bool) { return patchValue(p.Enabled) }},
}

func patchValue[T any](field *T) (any, bool) {
	if field == nil {
		return nil, false
	}
	return *field, true
}

// sqlValue converts a patch value to what the column stores. SQLite has no
// boolean type — jobs.enabled is INTEGER 0/1 — so a Go bool has to become an
// int on the way in. Everything else already matches its column.
func sqlValue(value any) any {
	enabled, isBool := value.(bool)
	if !isBool {
		return value
	}
	if enabled {
		return 1
	}
	return 0
}

// PatchableColumns names every column UpdateJob can write, in statement order.
// Exported so callers can assert their own request shape covers all of them.
func PatchableColumns() []string {
	names := make([]string, 0, len(patchColumns))
	for _, col := range patchColumns {
		names = append(names, col.Name)
	}
	return names
}

// JobFieldChange is one column a patch writes, with the value it writes.
type JobFieldChange struct {
	Column string
	Value  any
}

// Changes lists the columns this patch would write, with their new values, in
// statement order. Callers use it to log what actually moved rather than
// guessing.
func (p JobPatch) Changes() []JobFieldChange {
	var changes []JobFieldChange
	for _, col := range patchColumns {
		value, set := col.Read(p)
		if !set {
			continue
		}
		changes = append(changes, JobFieldChange{Column: col.Name, Value: value})
	}
	return changes
}

// UpdateJob writes every field the patch sets and leaves the rest alone.
// Returns ErrEmptyJobPatch if the patch names nothing, sql.ErrNoRows if the job
// does not exist.
func (s *Store) UpdateJob(id int64, patch JobPatch) error {
	changes := patch.Changes()
	assignments := make([]string, 0, len(changes)+1)
	values := make([]any, 0, len(changes)+2)
	for _, change := range changes {
		assignments = append(assignments, change.Column+"=?")
		values = append(values, sqlValue(change.Value))
	}
	if len(assignments) == 0 {
		return ErrEmptyJobPatch
	}
	assignments = append(assignments, "updated_at=?")
	values = append(values, time.Now().UTC(), id)

	res, err := s.db.Exec(`UPDATE jobs SET `+strings.Join(assignments, ", ")+` WHERE id=?`, values...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteJob(id int64) error {
	res, err := s.db.Exec(`DELETE FROM jobs WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) CreateRun(jobID int64) (*Run, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(`INSERT INTO runs (job_id, status, started_at) VALUES (?,'running',?)`, jobID, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Run{ID: id, JobID: jobID, Status: "running", StartedAt: now}, nil
}

func (s *Store) FinishRun(id int64, status, output string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE runs SET status=?, output=?, finished_at=? WHERE id=?`, status, output, now, id)
	return err
}

func (s *Store) ListRuns(jobID int64) ([]Run, error) {
	rows, err := s.db.Query(`SELECT id, job_id, status, output, started_at, finished_at FROM runs WHERE job_id=? ORDER BY id DESC LIMIT 100`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.JobID, &r.Status, &r.Output, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func (s *Store) SetHold(id int64, until time.Time) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE jobs SET hold_until=?, updated_at=? WHERE id=?`, until.UTC(), now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ClearHold(id int64) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE jobs SET hold_until=NULL, updated_at=? WHERE id=?`, now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ClearExpiredHolds() (int64, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE jobs SET hold_until=NULL, updated_at=? WHERE hold_until IS NOT NULL AND hold_until <= ?`, now, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// IsOnHold returns true if the job has an active (non-expired) hold.
func (j *Job) IsOnHold() bool {
	return j.HoldUntil != nil && j.HoldUntil.After(time.Now())
}
