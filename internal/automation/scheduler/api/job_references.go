package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db"
)

// A job's reference id is its row id written the way every store here writes
// one, scheduler_job_000076, so that text naming a job can say which job and a
// chat can draw it as a chip. A bare number cannot be told from any other.
var jobReferencePattern = regexp.MustCompile(`^scheduler_job_(\d{6,})$`)

// JobReferenceID is the reference id of job id.
func JobReferenceID(id int64) string { return fmt.Sprintf("scheduler_job_%06d", id) }

// JobReference is what GET /api/job-references/{reference} answers: the job,
// its reference id, and how its newest run went, without that run's output.
type JobReference struct {
	db.Job
	ReferenceID string        `json:"reference_id"`
	LastRun     *db.RunHeader `json:"last_run"`
}

// ErrNotAJobReference is a reference that is not scheduler_job_ and a
// number.
var ErrNotAJobReference = errors.New("not a job reference")

// ErrJobNotFound is a reference to a job the scheduler does not have.
var ErrJobNotFound = errors.New("job not found")

// JobReferenceOf reads the job a reference id names, with its newest run.
func (s *Server) JobReferenceOf(reference string) (JobReference, error) {
	found := jobReferencePattern.FindStringSubmatch(reference)
	if found == nil {
		return JobReference{}, fmt.Errorf("%w: a job reference is scheduler_job_ and at least six digits", ErrNotAJobReference)
	}
	id, err := strconv.ParseInt(found[1], 10, 64)
	if err != nil {
		return JobReference{}, fmt.Errorf("%w: the job reference's number is too large", ErrNotAJobReference)
	}
	job, err := s.store.GetJob(id)
	if errors.Is(err, sql.ErrNoRows) {
		return JobReference{}, ErrJobNotFound
	}
	if err != nil {
		return JobReference{}, fmt.Errorf("failed to get job: %w", err)
	}
	lastRun, err := s.store.LatestRun(id)
	if err != nil {
		return JobReference{}, fmt.Errorf("failed to get the job's newest run: %w", err)
	}
	return JobReference{Job: *job, ReferenceID: JobReferenceID(id), LastRun: lastRun}, nil
}

// handleJobReference looks a job up by its reference id, for reference-store.
func (s *Server) handleJobReference(w http.ResponseWriter, r *http.Request) {
	reference := r.PathValue("reference")
	answer, err := s.JobReferenceOf(reference)
	switch {
	case errors.Is(err, ErrNotAJobReference):
		writeError(w, 400, strings.TrimPrefix(err.Error(), ErrNotAJobReference.Error()+": "), reference)
	case errors.Is(err, ErrJobNotFound):
		writeError(w, 404, "job not found", "")
	case err != nil:
		message, details, _ := strings.Cut(err.Error(), ": ")
		writeError(w, 500, message, details)
	default:
		writeJSON(w, answer)
	}
}
