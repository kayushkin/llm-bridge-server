package api

import (
	"encoding/json"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db"
)

// jobPatchRequest is the JSON body of PATCH /api/jobs/{id}. Every field is a
// pointer so an absent field ("leave it alone") is distinguishable from a zero
// value ("set it to empty") — clearing a job's model role or session id is a real
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
	ModelRole    *string `json:"model_role"`
	Orchestrator *string `json:"orchestrator"`
	SessionID    *string `json:"session_id"`
	WorkspaceID  *string `json:"workspace_id"`
	// EventTrigger is raw so that null, which removes the trigger, differs
	// from leaving the field out: a pointer decodes both as nil.
	EventTrigger json.RawMessage `json:"event_trigger"`
	TimeoutSecs  *int            `json:"timeout_seconds"`
	Enabled      *bool           `json:"enabled"`
}

// toJobPatch converts the request; an event_trigger that does not decode is
// an error for the caller's 400.
func (r jobPatchRequest) toJobPatch() (db.JobPatch, error) {
	var eventTrigger *string
	if r.EventTrigger != nil {
		trigger, err := db.DecodeEventTrigger(string(r.EventTrigger))
		if err != nil {
			return db.JobPatch{}, err
		}
		encoded, err := db.EncodeEventTrigger(trigger)
		if err != nil {
			return db.JobPatch{}, err
		}
		eventTrigger = &encoded
	}
	return db.JobPatch{
		Name:         r.Name,
		Description:  r.Description,
		Schedule:     r.Schedule,
		Command:      r.Command,
		Type:         r.Type,
		Agent:        r.Agent,
		Prompt:       r.Prompt,
		ModelRole:    r.ModelRole,
		Orchestrator: r.Orchestrator,
		SessionID:    r.SessionID,
		WorkspaceID:  r.WorkspaceID,
		EventTrigger: eventTrigger,
		TimeoutSecs:  r.TimeoutSecs,
		Enabled:      r.Enabled,
	}, nil
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
	assign(&job.ModelRole, patch.ModelRole)
	assign(&job.Orchestrator, patch.Orchestrator)
	assign(&job.SessionID, patch.SessionID)
	assign(&job.WorkspaceID, patch.WorkspaceID)
	if patch.EventTrigger != nil {
		// toJobPatch wrote this string from a trigger it had just decoded.
		job.EventTrigger, _ = db.DecodeEventTrigger(*patch.EventTrigger)
	}
	assign(&job.TimeoutSecs, patch.TimeoutSecs)
	assign(&job.Enabled, patch.Enabled)
	return job
}

func assign[T any](target *T, value *T) {
	if value != nil {
		*target = *value
	}
}

// decodeStrict rejects a body carrying a field the request type does not have,
// instead of dropping it and answering 200. Silently ignoring `prompt` on a
// PATCH is what let a prompt edit report success and change nothing.
func decodeStrict(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
