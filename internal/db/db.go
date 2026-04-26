package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Job struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Description   string     `json:"description,omitempty"`
	Schedule      string     `json:"schedule"`
	Command       string     `json:"command"`
	Type          string     `json:"type"`                     // "shell" or "agent"
	Agent         string     `json:"agent,omitempty"`          // agent name (for type=agent)
	Prompt        string     `json:"prompt,omitempty"`         // prompt text (for type=agent)
	Model         string     `json:"model,omitempty"`          // model override (for type=agent)
	Orchestrator  string     `json:"orchestrator,omitempty"`   // "claude-code", "inber", etc. (for type=agent)
	SessionID     string     `json:"session_id,omitempty"`     // session to resume (for type=agent)
	Enabled       bool       `json:"enabled"`
	HoldUntil     *time.Time `json:"hold_until,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
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

func New(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	d, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;"); err != nil {
		d.Close()
		return nil, fmt.Errorf("sqlite pragmas: %w", err)
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
		"ALTER TABLE jobs ADD COLUMN hold_until DATETIME",
		"ALTER TABLE jobs ADD COLUMN description TEXT NOT NULL DEFAULT ''",
	} {
		s.db.Exec(col)
	}
	return nil
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
		`INSERT INTO jobs (name, description, schedule, command, type, agent, prompt, model, orchestrator, session_id, enabled, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,1,?,?)`,
		j.Name, j.Description, j.Schedule, j.Command, j.Type, j.Agent, j.Prompt, j.Model, j.Orchestrator, j.SessionID, now, now,
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
	rows, err := s.db.Query(`SELECT id, name, description, schedule, command, type, agent, prompt, model, orchestrator, session_id, enabled, hold_until, created_at, updated_at FROM jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		var j Job
		var enabled int
		if err := rows.Scan(&j.ID, &j.Name, &j.Description, &j.Schedule, &j.Command, &j.Type, &j.Agent, &j.Prompt, &j.Model, &j.Orchestrator, &j.SessionID, &enabled, &j.HoldUntil, &j.CreatedAt, &j.UpdatedAt); err != nil {
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
	err := s.db.QueryRow(`SELECT id, name, description, schedule, command, type, agent, prompt, model, orchestrator, session_id, enabled, hold_until, created_at, updated_at FROM jobs WHERE id=?`, id).
		Scan(&j.ID, &j.Name, &j.Description, &j.Schedule, &j.Command, &j.Type, &j.Agent, &j.Prompt, &j.Model, &j.Orchestrator, &j.SessionID, &enabled, &j.HoldUntil, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return nil, err
	}
	j.Enabled = enabled != 0
	return &j, nil
}

func (s *Store) UpdateJobEnabled(id int64, enabled bool) error {
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE jobs SET enabled=?, updated_at=? WHERE id=?`, enabledInt, now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) UpdateJobDescription(id int64, description string) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE jobs SET description=?, updated_at=? WHERE id=?`, description, now, id)
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
