package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"github.com/slava/scheduler/internal/db"
)

// A job's reference id is its row id written the way every store here writes
// one, scheduler_job_000076, so that text naming a job can say which job and a
// chat can draw it as a chip. A bare number cannot be told from any other.
var jobReferencePattern = regexp.MustCompile(`^scheduler_job_(\d{6,})$`)

// JobReferenceID is the reference id of job id.
func JobReferenceID(id int64) string { return fmt.Sprintf("scheduler_job_%06d", id) }

// jobReference is what GET /api/job-references/{reference} answers: the job,
// its reference id, and how its newest run went, without that run's output.
type jobReference struct {
	db.Job
	ReferenceID string        `json:"reference_id"`
	LastRun     *db.RunHeader `json:"last_run"`
}

// handleJobReference looks a job up by its reference id, for reference-store.
func (s *Server) handleJobReference(w http.ResponseWriter, r *http.Request) {
	reference := r.PathValue("reference")
	found := jobReferencePattern.FindStringSubmatch(reference)
	if found == nil {
		writeError(w, 400, "a job reference is scheduler_job_ and at least six digits", reference)
		return
	}
	id, err := strconv.ParseInt(found[1], 10, 64)
	if err != nil {
		writeError(w, 400, "the job reference's number is too large", reference)
		return
	}
	job, err := s.store.GetJob(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "job not found", "")
		return
	}
	if err != nil {
		writeError(w, 500, "failed to get job", err.Error())
		return
	}
	lastRun, err := s.store.LatestRun(id)
	if err != nil {
		writeError(w, 500, "failed to get the job's newest run", err.Error())
		return
	}
	writeJSON(w, jobReference{Job: *job, ReferenceID: JobReferenceID(id), LastRun: lastRun})
}
