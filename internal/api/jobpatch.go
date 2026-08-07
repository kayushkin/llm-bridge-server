package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/robfig/cron/v3"
	"github.com/slava/scheduler/internal/db"
)

// jobPatchRequest is the JSON body of PATCH /api/jobs/{id}. Every field is a
// pointer so an absent field ("leave it alone") is distinguishable from a zero
// value ("set it to empty") — clearing a job's model or session id is a real
// edit and has to be expressible.
//
// It must name every column db.PatchableColumns() reports; a column the request
// cannot carry is a field the API silently refuses to change.
// TestJobPatchRequestCoversEveryPatchableColumn holds the two in step.
type jobPatchRequest struct {
	Name         *string `json:"name"`
	Description  *string `json:"description"`
	Schedule     *string `json:"schedule"`
	Command      *string `json:"command"`
	Type         *string `json:"type"`
	Agent        *string `json:"agent"`
	Prompt       *string `json:"prompt"`
	Model        *string `json:"model"`
	Orchestrator *string `json:"orchestrator"`
	SessionID    *string `json:"session_id"`
	WorkspaceID  *string `json:"workspace_id"`
	TimeoutSecs  *int    `json:"timeout_seconds"`
	Enabled      *bool   `json:"enabled"`
}

func (r jobPatchRequest) toJobPatch() db.JobPatch {
	return db.JobPatch{
		Name:         r.Name,
		Description:  r.Description,
		Schedule:     r.Schedule,
		Command:      r.Command,
		Type:         r.Type,
		Agent:        r.Agent,
		Prompt:       r.Prompt,
		Model:        r.Model,
		Orchestrator: r.Orchestrator,
		SessionID:    r.SessionID,
		WorkspaceID:  r.WorkspaceID,
		TimeoutSecs:  r.TimeoutSecs,
		Enabled:      r.Enabled,
	}
}

// applyJobPatch returns the job the patch would produce. Used to validate the
// result rather than the patch — most of the rules below are cross-field, and a
// patch read alone cannot see the fields it is not changing.
func applyJobPatch(job db.Job, patch db.JobPatch) db.Job {
	assign(&job.Name, patch.Name)
	assign(&job.Description, patch.Description)
	assign(&job.Schedule, patch.Schedule)
	assign(&job.Command, patch.Command)
	assign(&job.Type, patch.Type)
	assign(&job.Agent, patch.Agent)
	assign(&job.Prompt, patch.Prompt)
	assign(&job.Model, patch.Model)
	assign(&job.Orchestrator, patch.Orchestrator)
	assign(&job.SessionID, patch.SessionID)
	assign(&job.WorkspaceID, patch.WorkspaceID)
	assign(&job.TimeoutSecs, patch.TimeoutSecs)
	assign(&job.Enabled, patch.Enabled)
	return job
}

func assign[T any](target *T, value *T) {
	if value != nil {
		*target = *value
	}
}

// requiredField is a field a job type cannot run without, paired with the
// reader that pulls it off a job.
type requiredField struct {
	Name string
	Read func(db.Job) string
}

// jobTypeRequirements is the closed set of job types, each mapped to the fields
// that type cannot run without.
var jobTypeRequirements = map[string][]requiredField{
	"shell": {{"command", func(j db.Job) string { return j.Command }}},
	"agent": {
		{"agent", func(j db.Job) string { return j.Agent }},
		{"prompt", func(j db.Job) string { return j.Prompt }},
	},
}

// validateJob reports why a job is unrunnable, or "" if it is fine. Both POST
// and PATCH run it, so a job cannot be created invalid nor edited into an
// invalid state — PATCH validates the merged result, not the patch.
func validateJob(job db.Job) string {
	if job.Name == "" {
		return "name is required"
	}
	if job.Schedule == "" {
		return "schedule is required"
	}
	// The cron expression used to go unchecked, so a job with an unparseable
	// schedule was accepted, then skipped with a log line at load time and never
	// ran again. Reject it at the edge instead.
	if _, err := cron.ParseStandard(job.Schedule); err != nil {
		return fmt.Sprintf("invalid schedule %q: %s", job.Schedule, err)
	}
	if job.TimeoutSecs < 0 {
		return "timeout_seconds must be >= 0 (0 means the default)"
	}
	required, known := jobTypeRequirements[job.Type]
	if !known {
		return fmt.Sprintf("type must be one of %s", strings.Join(knownJobTypes(), ", "))
	}
	names := make([]string, 0, len(required))
	missing := false
	for _, field := range required {
		names = append(names, field.Name)
		if field.Read(job) == "" {
			missing = true
		}
	}
	if missing {
		return fmt.Sprintf("%s is required for %s jobs", strings.Join(names, " and "), job.Type)
	}
	return ""
}

// knownJobTypes lists the accepted types, sorted — map iteration order is
// random and an error message that reshuffles itself is not worth debugging.
func knownJobTypes() []string {
	types := make([]string, 0, len(jobTypeRequirements))
	for jobType := range jobTypeRequirements {
		types = append(types, "'"+jobType+"'")
	}
	sort.Strings(types)
	return types
}

// decodeStrict rejects a body carrying a field the request type does not have,
// instead of dropping it and answering 200. Silently ignoring `prompt` on a
// PATCH is what let a prompt edit report success and change nothing.
func decodeStrict(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
