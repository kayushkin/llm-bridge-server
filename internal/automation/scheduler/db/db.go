package db

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/eventtriggers"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

type Job struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Schedule     string `json:"schedule"`
	Command      string `json:"command"`
	Type         string `json:"type"`                   // "shell" or "agent"
	Agent        string `json:"agent,omitempty"`        // agent name (for type=agent)
	Prompt       string `json:"prompt,omitempty"`       // prompt text (for type=agent)
	ModelRole    string `json:"model_role,omitempty"`   // model-store role the session starts on (for type=agent); empty takes the agent_job_model_role setting
	Orchestrator string `json:"orchestrator,omitempty"` // "claude-code", "inber", etc. (for type=agent)
	SessionID    string `json:"session_id,omitempty"`   // session to resume (for type=agent)
	WorkspaceID  string `json:"workspace_id,omitempty"` // noteboard workspace holding this job's working memory (for type=agent)
	// EventTrigger, when set, runs the job on each bus message it matches
	// instead of on Schedule, which is then empty. See internal/eventtriggers.
	EventTrigger *eventtriggers.Trigger `json:"event_trigger,omitempty"`
	TimeoutSecs  int                    `json:"timeout_seconds"` // per-job wall-clock cap; 0 means executor.DefaultTimeout
	Enabled      bool                   `json:"enabled"`
	HoldUntil    *time.Time             `json:"hold_until,omitempty"`
	CreatedAt    time.Time              `json:"created_at"`
	UpdatedAt    time.Time              `json:"updated_at"`
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

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the scheduler keeps its jobs and runs
// in, with the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "scheduler", Role: "scheduler_module", Migrations: migrations}, nil
}

// JobReferencePrefix is the type part of a job's reference id:
// scheduler_job_000076. The API serves the bare number as id.
const JobReferencePrefix = "scheduler_job"

// New keeps the scheduler's records in pool, the module's own connection
// pool: provisioned, migrated and opened by the caller.
func New(pool *sql.DB) *Store {
	return &Store{db: pool}
}

func (s *Store) Close() error { return s.db.Close() }

// inUTC answers t in UTC. The driver reads a timestamptz in the process's
// zone, and the API has always answered times in UTC.
func inUTC(t time.Time) time.Time { return t.UTC() }

// storedNow is the time now as Postgres keeps it, to the microsecond, so a
// record answered from a write says what a later read of it says.
func storedNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func inUTCOrNil(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// InterruptedRunOutput is stored as the output of a run that startup
// reconciliation closed out and that produced no output of its own.
//
// It is a constant because reconciliation writes it in SQL and reports it back
// to the caller in Go, and those two must say the same thing: the caller
// publishes what it is handed, so a second spelling here would be published as
// the run's output while a different one sat in the database.
const InterruptedRunOutput = "interrupted: scheduler exited while this run was in flight"

// InterruptedRun is a run that startup reconciliation closed out, carrying the
// job fields the finish logger needs alongside the run's own.
//
// JobName and Schedule are empty when the run's job no longer exists: runs
// outlive their jobs (see the runs table). They are reported with the name
// left empty rather than invented, and JobID still identifies the row that is
// gone.
type InterruptedRun struct {
	RunID      int64
	JobID      int64
	JobName    string
	Schedule   string
	Output     string
	StartedAt  time.Time
	FinishedAt time.Time
}

// ReconcileOrphanedRuns closes out runs left mid-flight by a process that died
// (a crash, an OOM kill, a deploy restart). Nothing else ever clears them, so
// without this they stay "running" forever and read as concurrent duplicates of
// the job. Safe to call only at startup, before the cron loop can create runs.
//
// It returns the runs it closed rather than a count of them, because closing a
// run is a finish and the caller has to be able to report each one the way every
// other finish is reported. A count can only be summarized, and a summary of n
// killed runs does not say which jobs they were.
//
// FinishedAt is when reconciliation noticed, not when the process actually died
// — that moment is unrecorded and unknowable here. A run interrupted long ago
// therefore reports a long duration, which is the truth about the row and not
// about how long the job ran.
func (s *Store) ReconcileOrphanedRuns() ([]InterruptedRun, error) {
	now := storedNow()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("reconcile orphaned runs: begin: %w", err)
	}
	defer tx.Rollback()

	// LEFT JOIN, so a run whose job is gone is still reported. An inner join
	// would drop it from this set while the UPDATE below still closed it, and
	// the caller would publish fewer finishes than the number of runs it ended.
	rows, err := tx.Query(`
		SELECT r.id, r.job_id, COALESCE(j.name, ''), COALESCE(j.schedule, ''), r.output, r.started_at
		FROM runs r LEFT JOIN jobs j ON j.id = r.job_id
		WHERE r.status = 'running'
		FOR UPDATE OF r`)
	if err != nil {
		return nil, fmt.Errorf("reconcile orphaned runs: select: %w", err)
	}

	var interrupted []InterruptedRun
	for rows.Next() {
		var r InterruptedRun
		var output []byte
		if err := rows.Scan(&r.RunID, &r.JobID, &r.JobName, &r.Schedule, &output, &r.StartedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reconcile orphaned runs: scan: %w", err)
		}
		r.Output = string(output)
		r.StartedAt = inUTC(r.StartedAt)
		if r.Output == "" {
			r.Output = InterruptedRunOutput
		}
		r.FinishedAt = now
		interrupted = append(interrupted, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("reconcile orphaned runs: rows: %w", err)
	}
	rows.Close()

	res, err := tx.Exec(`
		UPDATE runs SET status = 'interrupted', finished_at = $1,
		  output = CASE WHEN output = ''::bytea THEN $2::bytea ELSE output END
		WHERE status = 'running'`, now, []byte(InterruptedRunOutput))
	if err != nil {
		return nil, fmt.Errorf("reconcile orphaned runs: update: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("reconcile orphaned runs: rows affected: %w", err)
	}

	// The SELECT and the UPDATE carry the same WHERE clause written twice, and
	// nothing but this check ties them together. If they ever select different
	// sets, runs are being closed that no caller will report — the exact silence
	// this function was changed to end — so refuse rather than return a set that
	// is quietly short.
	if affected != int64(len(interrupted)) {
		return nil, fmt.Errorf("reconcile orphaned runs: closed %d runs but captured %d", affected, len(interrupted))
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("reconcile orphaned runs: commit: %w", err)
	}
	return interrupted, nil
}

func (s *Store) CreateJob(name, schedule, command string) (*Job, error) {
	return s.CreateJobFull(Job{
		Name:     name,
		Schedule: schedule,
		Command:  command,
		Type:     "shell",
		Enabled:  true,
	})
}

// EnabledOnCreate resolves the optional "enabled" field of a job-creation
// request. Absent means true, so every caller written before the field existed
// keeps creating armed jobs; false means create it in the off position.
//
// Both creation edges — POST /api/jobs and the bus create_job command — share
// this so the two cannot drift apart on what an omitted field means.
func EnabledOnCreate(requested *bool) bool {
	if requested == nil {
		return true
	}
	return *requested
}

// CreateJobFull writes every field of j, Enabled included.
//
// It used to bind a literal 1 for enabled and then overwrite j.Enabled with
// true on the way out, so there was no way to create a job in the off
// position: the only path was create-then-PATCH, which arms the job for the
// window between the two calls. On a */5 schedule that window is long enough
// to fire.
//
// The default now lives at the edge — the HTTP handler and the bus handler
// each read an absent "enabled" as true — because that is where a caller's
// intent is known. This function has no default and no opinion; it stores
// what it is handed. A caller that builds a Job literal and forgets Enabled
// therefore gets a DISABLED job, which is the safe direction to be wrong in.
func (s *Store) CreateJobFull(j Job) (*Job, error) {
	now := storedNow()
	if j.Type == "" {
		j.Type = "shell"
	}
	eventTrigger, err := EncodeEventTrigger(j.EventTrigger)
	if err != nil {
		return nil, err
	}
	var id int64
	err = s.db.QueryRow(
		`INSERT INTO jobs (name, description, schedule, command, type, agent, prompt, model_role, orchestrator, session_id, workspace_id, event_trigger, timeout_seconds, enabled, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id`,
		j.Name, j.Description, j.Schedule, j.Command, j.Type, j.Agent, j.Prompt, j.ModelRole, j.Orchestrator, j.SessionID, j.WorkspaceID, eventTrigger, j.TimeoutSecs, j.Enabled, now, now,
	).Scan(&id)
	if err != nil {
		return nil, err
	}
	j.ID = id
	j.CreatedAt = now
	j.UpdatedAt = now
	return &j, nil
}

// jobColumns is the column list every read of a job selects, in the order
// scanJob reads it.
const jobColumns = `id, name, description, schedule, command, type, agent, prompt, model_role, orchestrator, session_id, workspace_id, event_trigger, timeout_seconds, enabled, hold_until, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var j Job
	var eventTrigger string
	if err := row.Scan(&j.ID, &j.Name, &j.Description, &j.Schedule, &j.Command, &j.Type, &j.Agent, &j.Prompt, &j.ModelRole, &j.Orchestrator, &j.SessionID, &j.WorkspaceID, &eventTrigger, &j.TimeoutSecs, &j.Enabled, &j.HoldUntil, &j.CreatedAt, &j.UpdatedAt); err != nil {
		return Job{}, err
	}
	j.HoldUntil = inUTCOrNil(j.HoldUntil)
	j.CreatedAt = inUTC(j.CreatedAt)
	j.UpdatedAt = inUTC(j.UpdatedAt)
	var err error
	if j.EventTrigger, err = DecodeEventTrigger(eventTrigger); err != nil {
		return Job{}, fmt.Errorf("job %d: %w", j.ID, err)
	}
	return j, nil
}

func (s *Store) ListJobs() ([]Job, error) {
	rows, err := s.db.Query(`SELECT ` + jobColumns + ` FROM jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *Store) GetJob(id int64) (*Job, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE id=$1`, id))
	if err != nil {
		return nil, err
	}
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
	ModelRole    *string
	Orchestrator *string
	SessionID    *string
	WorkspaceID  *string
	// EventTrigger is the trigger as JSON, as EncodeEventTrigger writes it;
	// "" removes it.
	EventTrigger *string
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
	{"model_role", func(p JobPatch) (any, bool) { return patchValue(p.ModelRole) }},
	{"orchestrator", func(p JobPatch) (any, bool) { return patchValue(p.Orchestrator) }},
	{"session_id", func(p JobPatch) (any, bool) { return patchValue(p.SessionID) }},
	{"workspace_id", func(p JobPatch) (any, bool) { return patchValue(p.WorkspaceID) }},
	{"event_trigger", func(p JobPatch) (any, bool) { return patchValue(p.EventTrigger) }},
	{"timeout_seconds", func(p JobPatch) (any, bool) { return patchValue(p.TimeoutSecs) }},
	{"enabled", func(p JobPatch) (any, bool) { return patchValue(p.Enabled) }},
}

func patchValue[T any](field *T) (any, bool) {
	if field == nil {
		return nil, false
	}
	return *field, true
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
		values = append(values, change.Value)
		assignments = append(assignments, fmt.Sprintf("%s=$%d", change.Column, len(values)))
	}
	if len(assignments) == 0 {
		return ErrEmptyJobPatch
	}
	values = append(values, storedNow())
	assignments = append(assignments, fmt.Sprintf("updated_at=$%d", len(values)))
	values = append(values, id)

	res, err := s.db.Exec(`UPDATE jobs SET `+strings.Join(assignments, ", ")+fmt.Sprintf(` WHERE id=$%d`, len(values)), values...)
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
	res, err := s.db.Exec(`DELETE FROM jobs WHERE id=$1`, id)
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
	now := storedNow()
	var id int64
	if err := s.db.QueryRow(`INSERT INTO runs (job_id, status, started_at) VALUES ($1,'running',$2) RETURNING id`, jobID, now).Scan(&id); err != nil {
		return nil, err
	}
	return &Run{ID: id, JobID: jobID, Status: "running", StartedAt: now}, nil
}

func (s *Store) FinishRun(id int64, status, output string) error {
	now := storedNow()
	_, err := s.db.Exec(`UPDATE runs SET status=$1, output=$2, finished_at=$3 WHERE id=$4`, status, []byte(output), now, id)
	return err
}

// MaxRunsListed is the most runs one read of a job's history answers.
const MaxRunsListed = 100

// ListRuns answers a job's newest runs, newest first, at most limit of them
// (1 to MaxRunsListed).
func (s *Store) ListRuns(jobID int64, limit int) ([]Run, error) {
	if limit < 1 || limit > MaxRunsListed {
		return nil, fmt.Errorf("limit %d is outside 1 to %d", limit, MaxRunsListed)
	}
	rows, err := s.db.Query(`SELECT id, job_id, status, output, started_at, finished_at FROM runs WHERE job_id=$1 ORDER BY id DESC LIMIT $2`, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		var r Run
		var output []byte
		if err := rows.Scan(&r.ID, &r.JobID, &r.Status, &output, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		r.Output = string(output)
		r.StartedAt = inUTC(r.StartedAt)
		r.FinishedAt = inUTCOrNil(r.FinishedAt)
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// RunHeader is a run without its output, for reads that only need to know
// how each job last ended.
type RunHeader struct {
	ID         int64      `json:"id"`
	JobID      int64      `json:"job_id"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// LatestRuns is the newest run of every job that has run, without output,
// ordered by job id.
func (s *Store) LatestRuns() ([]RunHeader, error) {
	rows, err := s.db.Query(`SELECT r.id, r.job_id, r.status, r.started_at, r.finished_at
		FROM runs r JOIN (SELECT MAX(id) AS id FROM runs GROUP BY job_id) latest ON latest.id = r.id
		ORDER BY r.job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []RunHeader{}
	for rows.Next() {
		var r RunHeader
		if err := rows.Scan(&r.ID, &r.JobID, &r.Status, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		r.StartedAt = inUTC(r.StartedAt)
		r.FinishedAt = inUTCOrNil(r.FinishedAt)
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// LatestRun is job id's newest run without its output, or nil when it has
// never run.
func (s *Store) LatestRun(jobID int64) (*RunHeader, error) {
	var r RunHeader
	err := s.db.QueryRow(`SELECT id, job_id, status, started_at, finished_at FROM runs WHERE job_id=$1 ORDER BY id DESC LIMIT 1`, jobID).
		Scan(&r.ID, &r.JobID, &r.Status, &r.StartedAt, &r.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.StartedAt = inUTC(r.StartedAt)
	r.FinishedAt = inUTCOrNil(r.FinishedAt)
	return &r, nil
}

func (s *Store) SetHold(id int64, until time.Time) error {
	now := storedNow()
	res, err := s.db.Exec(`UPDATE jobs SET hold_until=$1, updated_at=$2 WHERE id=$3`, until.UTC().Truncate(time.Microsecond), now, id)
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
	now := storedNow()
	res, err := s.db.Exec(`UPDATE jobs SET hold_until=NULL, updated_at=$1 WHERE id=$2`, now, id)
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
	now := storedNow()
	res, err := s.db.Exec(`UPDATE jobs SET hold_until=NULL, updated_at=$1 WHERE hold_until IS NOT NULL AND hold_until <= $1`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// IsOnHold returns true if the job has an active (non-expired) hold.
func (j *Job) IsOnHold() bool {
	return j.HoldUntil != nil && j.HoldUntil.After(time.Now())
}
