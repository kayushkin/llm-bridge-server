package localtools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/agents/tools/schema"
)

// Job represents a scheduler job (from scheduler/internal/db/db.go)
type Job struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Description  string     `json:"description,omitempty"`
	Schedule     string     `json:"schedule"` // cron expression
	Command      string     `json:"command"`  // shell command
	Type         string     `json:"type"`     // "shell" or "agent"
	Agent        string     `json:"agent,omitempty"`
	Prompt       string     `json:"prompt,omitempty"`
	ModelRole    string     `json:"model_role,omitempty"`   // model-store role the session starts on (type=agent)
	Orchestrator string     `json:"orchestrator,omitempty"` // "claude-code", "inber", etc.
	SessionID    string     `json:"session_id,omitempty"`   // session to resume
	WorkspaceID  string     `json:"workspace_id,omitempty"` // noteboard workspace holding durable memory
	TimeoutSecs  int        `json:"timeout_seconds"`        // per-job wall-clock cap; 0 means the default
	Enabled      bool       `json:"enabled"`
	HoldUntil    *time.Time `json:"hold_until,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// Run represents a job execution record
type Run struct {
	ID         int64      `json:"id"`
	JobID      int64      `json:"job_id"`
	Status     string     `json:"status"`
	Output     string     `json:"output"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// schedulerInput represents the input parameters for the scheduler tool
type schedulerInput struct {
	Action       string  `json:"action"`                    // "list", "create", "get", "update", "delete", "runs"
	ID           *int64  `json:"id,omitempty"`              // job ID for get/update/delete/runs actions
	Name         *string `json:"name,omitempty"`            // job name for create
	Schedule     *string `json:"schedule,omitempty"`        // cron expression for create
	Command      *string `json:"command,omitempty"`         // shell command for create (type=shell)
	Type         *string `json:"type,omitempty"`            // "shell" or "agent" for create
	Agent        *string `json:"agent,omitempty"`           // agent name for create (type=agent)
	Prompt       *string `json:"prompt,omitempty"`          // prompt text for create (type=agent)
	ModelRole    *string `json:"model_role,omitempty"`      // model-store role the session starts on (type=agent)
	Orchestrator *string `json:"orchestrator,omitempty"`    // "claude-code", "inber", etc. for create (type=agent)
	SessionID    *string `json:"session_id,omitempty"`      // session to resume for create (type=agent)
	WorkspaceID  *string `json:"workspace_id,omitempty"`    // noteboard workspace holding the job's durable memory (type=agent)
	TimeoutSecs  *int    `json:"timeout_seconds,omitempty"` // per-job wall-clock cap in seconds; 0 means the default
	Description  *string `json:"description,omitempty"`     // human-readable note about what the job does
	Enabled      *bool   `json:"enabled,omitempty"`         // enable/disable for update
}

// SchedulerConnection is where the scheduler tool finds the scheduler and what
// it presents there. The tool does not read the environment: whoever builds it
// says where the scheduler is.
type SchedulerConnection struct {
	// BaseURL is the scheduler's address, as HTTP reaches it.
	BaseURL string
	// HTTP carries every call. In llm-bridge-server the scheduler runs in the
	// same process, and HTTP serves each call with its handler.
	HTTP *http.Client
}

// Scheduler returns a tool that interacts with the scheduler HTTP API at
// connection.
func Scheduler(connection SchedulerConnection) Impl {
	return Impl{
		Name:        "scheduler",
		Description: "Interact with the scheduler HTTP API to manage cron jobs. Supports listing, creating, updating, deleting jobs, and viewing run history.",
		InputSchema: schema.Props([]string{"action"}, map[string]any{
			"action":          schema.Str("Action to perform: list, create, get, update, delete, runs"),
			"id":              schema.Integer("Job ID (required for get, update, delete, runs actions)"),
			"name":            schema.Str("Job name (required for create; also editable via update)"),
			"description":     schema.Str("Human-readable note about what the job does"),
			"schedule":        schema.Str("Cron expression (required for create; also editable via update)"),
			"command":         schema.Str("Shell command (required for create with type=shell)"),
			"type":            schema.Str("Job type: 'shell' or 'agent' (default: shell)"),
			"agent":           schema.Str("Agent name (required for create with type=agent)"),
			"prompt":          schema.Str("Prompt text (required for create with type=agent)"),
			"model_role":      schema.Str("Model-store role the session starts on, such as 'balanced' (model-store GET /api/roles lists them); not a model id. Optional, type=agent only; empty uses the scheduler's agent_job_model_role setting"),
			"orchestrator":    schema.Str("Orchestrator name: 'claude-code', 'inber', etc. (default: claude-code)"),
			"session_id":      schema.Str("Session ID to resume (empty for new session)"),
			"workspace_id":    schema.Str("Noteboard workspace holding the job's durable memory (type=agent)"),
			"timeout_seconds": schema.Integer("Per-job wall-clock cap in seconds; 0 means the scheduler default"),
			"enabled":         schema.Bool("Enable/disable job; on create, false makes the job switched off from the start (default: enabled)"),
		}),
		Run: func(ctx context.Context, raw string) (string, error) {
			in, err := schema.Parse[schedulerInput](raw)
			if err != nil {
				return "", err
			}

			if connection.BaseURL == "" || connection.HTTP == nil {
				return "", fmt.Errorf("the scheduler tool was built without the scheduler's address and client")
			}
			baseURL := connection.BaseURL
			client := connection.HTTP

			switch in.Action {
			case "list":
				return handleList(ctx, baseURL, client)
			case "create":
				return handleCreate(ctx, baseURL, client, in)
			case "get":
				if in.ID == nil {
					return "error: id is required for get action", nil
				}
				return handleGet(ctx, baseURL, client, *in.ID)
			case "update":
				if in.ID == nil {
					return "error: id is required for update action", nil
				}
				return handleUpdate(ctx, baseURL, client, *in.ID, in)
			case "delete":
				if in.ID == nil {
					return "error: id is required for delete action", nil
				}
				return handleDelete(ctx, baseURL, client, *in.ID)
			case "runs":
				if in.ID == nil {
					return "error: id is required for runs action", nil
				}
				return handleRuns(ctx, baseURL, client, *in.ID)
			default:
				return "error: invalid action. Must be one of: list, create, get, update, delete, runs", nil
			}
		},
	}
}

func makeRequest(ctx context.Context, method, url string, client *http.Client, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.Do(req)
}

func handleList(ctx context.Context, baseURL string, client *http.Client) (string, error) {
	resp, err := makeRequest(ctx, "GET", baseURL+"/api/jobs", client, nil)
	if err != nil {
		return fmt.Sprintf("error: %s", err), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("error reading response: %s", err), nil
	}

	if resp.StatusCode != 200 {
		return fmt.Sprintf("error: API returned %d: %s", resp.StatusCode, string(body)), nil
	}

	var jobs []Job
	if err := json.Unmarshal(body, &jobs); err != nil {
		return fmt.Sprintf("error parsing response: %s", err), nil
	}

	if len(jobs) == 0 {
		return "no jobs found", nil
	}

	result := fmt.Sprintf("Found %d jobs:\n\n", len(jobs))
	for _, job := range jobs {
		result += fmt.Sprintf("ID: %d\n%s\n", job.ID, describeJobFields(job))
		result += describeHold(job)
		result += "\n"
	}

	return result, nil
}

// describeHold names the gate that stops a job running even though it is
// enabled. It is not one of schedulerJobFields because neither write verb sets
// it — hold_until is moved by POST /api/jobs/{id}/hold and nothing else.
func describeHold(job Job) string {
	if job.HoldUntil == nil || !job.HoldUntil.After(time.Now()) {
		return ""
	}
	return fmt.Sprintf("On hold until: %s\n", job.HoldUntil.Format(time.RFC3339))
}

func handleCreate(ctx context.Context, baseURL string, client *http.Client, in schedulerInput) (string, error) {
	if in.Name == nil || in.Schedule == nil {
		return "error: name and schedule are required for create action", nil
	}

	jobType := "shell"
	if in.Type != nil {
		jobType = *in.Type
	}

	if jobType != "shell" && jobType != "agent" {
		return "error: type must be 'shell' or 'agent'", nil
	}

	if jobType == "shell" && (in.Command == nil || *in.Command == "") {
		return "error: command is required for shell jobs", nil
	}

	if jobType == "agent" && (in.Agent == nil || *in.Agent == "" || in.Prompt == nil || *in.Prompt == "") {
		return "error: agent and prompt are required for agent jobs", nil
	}

	// The caller may have named no type, and the job still gets one. Putting
	// the defaulted value back on the input means the field table forwards it
	// like any other, and the report names the type the job actually got
	// rather than staying silent about a value the caller never chose.
	normalizedInput := in
	normalizedInput.Type = &jobType

	reqBody := map[string]interface{}{}
	var requestedFields []requestedChange
	for _, field := range schedulerJobFields {
		value, set := field.ReadRequested(normalizedInput)
		if !set {
			continue
		}
		reqBody[field.Name] = value
		requestedFields = append(requestedFields, requestedChange{
			Name:       field.Name,
			Value:      value,
			ReadEchoed: field.ReadEchoed,
		})
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Sprintf("error marshaling request: %s", err), nil
	}

	resp, err := makeRequest(ctx, "POST", baseURL+"/api/jobs", client, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Sprintf("error: %s", err), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("error reading response: %s", err), nil
	}

	if resp.StatusCode != 201 {
		return fmt.Sprintf("error: API returned %d: %s", resp.StatusCode, string(body)), nil
	}

	var job Job
	if err := json.Unmarshal(body, &job); err != nil {
		return fmt.Sprintf("error parsing response: %s", err), nil
	}

	return describeCreateOutcome(job, requestedFields), nil
}

// describeCreateOutcome reports the created job and, for each field the caller
// named, whether the server actually stored it — judged against the row the
// server echoed back rather than against the request that was sent.
//
// The old report named four fields and nothing else, so a create that dropped
// a description, a timeout or a workspace looked identical to one that kept
// them. workspace_id is the one with teeth: an agent job's durable memory is
// bound by that field, and a job that lost it reads as a job that was never
// given one.
func describeCreateOutcome(job Job, requestedFields []requestedChange) string {
	stored, dropped := partitionByWhatTheServerStored(job, requestedFields)

	result := fmt.Sprintf("Created job %d: %s\nSchedule: %s\nType: %s\nEnabled: %t\n",
		job.ID, job.Name, job.Schedule, job.Type, job.Enabled)

	if len(dropped) > 0 {
		// The server answered 201 to something it did not fully do, so say
		// that plainly. A caller who reads only this line still learns a
		// field did not land.
		result += fmt.Sprintf("The server stored only %d of the %d fields sent.\n",
			len(stored), len(requestedFields))
	}
	result += fmt.Sprintf("Stored: %s\n", strings.Join(stored, ", "))
	if len(dropped) > 0 {
		result += fmt.Sprintf("NOT stored: %s\n", strings.Join(dropped, ", "))
	}

	return strings.TrimRight(result, "\n")
}

func handleGet(ctx context.Context, baseURL string, client *http.Client, id int64) (string, error) {
	url := fmt.Sprintf("%s/api/jobs/%d", baseURL, id)
	resp, err := makeRequest(ctx, "GET", url, client, nil)
	if err != nil {
		return fmt.Sprintf("error: %s", err), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("error reading response: %s", err), nil
	}

	if resp.StatusCode == 404 {
		return fmt.Sprintf("job %d not found", id), nil
	}
	if resp.StatusCode != 200 {
		return fmt.Sprintf("error: API returned %d: %s", resp.StatusCode, string(body)), nil
	}

	var job Job
	if err := json.Unmarshal(body, &job); err != nil {
		return fmt.Sprintf("error parsing response: %s", err), nil
	}

	result := fmt.Sprintf("Job %d:\n%s\n", job.ID, describeJobFields(job))
	result += describeHold(job)
	result += fmt.Sprintf("Created: %s\nUpdated: %s",
		job.CreatedAt.Format(time.RFC3339), job.UpdatedAt.Format(time.RFC3339))

	return result, nil
}

// schedulerJobField is one field of a scheduler job as this tool handles it:
// the wire name, the
// reader that pulls the value off the tool's input, and the reader that pulls
// the same field back off the job the server echoes in its reply.
//
// Both readers exist because a scheduler that will not apply a field does not
// say so: it answers 200 or 201 with a row that does not hold the value. Which
// fields PATCH honors depends on which build is serving :8092 — HEAD takes
// every field, while older binaries take only enabled, description and
// timeout_seconds and silently drop the rest. Reading the echo instead of
// trusting the request is correct against either, so this tool never has to
// know which one it is talking to.
// The same table also drives the two READ paths, which were a third authoring
// of the same field list until they were folded onto it. Hand-written, list
// showed neither the description, the timeout nor the workspace, and get showed
// the workspace only on agent jobs — so a shell job's workspace binding was
// invisible on both. Label and ShownWhenZero are what the read paths need from
// a field, and they sit here so a field added for writing cannot be forgotten
// for reading.
type schedulerJobField struct {
	Name string
	// Label is how the read paths name this field to the caller.
	Label string
	// ShownWhenZero prints the field on the read paths even when it holds its
	// zero value. Every job has a name, a schedule, a type and an enabled
	// flag, so a blank one is itself worth seeing. Every other field is
	// optional, and a line saying it is empty is noise — which is the
	// convention handleGet already followed for description and timeout.
	ShownWhenZero bool
	ReadRequested func(schedulerInput) (any, bool)
	ReadEchoed    func(Job) any
}

// schedulerJobFields is the single authoring of the field list both write
// paths work from. Create used to name its own nine keys inline and update
// forwarded only "enabled", so the two paths disagreed about the same job in
// two different directions: an agent could not correct a job it had created,
// and a job created with a description, a timeout or a workspace lost all
// three on the way out.
var schedulerJobFields = []schedulerJobField{
	{"name", "Name", true, func(in schedulerInput) (any, bool) { return valueOf(in.Name) }, func(j Job) any { return j.Name }},
	{"description", "Description", false, func(in schedulerInput) (any, bool) { return valueOf(in.Description) }, func(j Job) any { return j.Description }},
	{"schedule", "Schedule", true, func(in schedulerInput) (any, bool) { return valueOf(in.Schedule) }, func(j Job) any { return j.Schedule }},
	{"type", "Type", true, func(in schedulerInput) (any, bool) { return valueOf(in.Type) }, func(j Job) any { return j.Type }},
	// Sent on create since scheduler 271998d, which inserts a job with
	// enabled:false switched off. Earlier builds answered the key with a 400,
	// so this tool used to drop it and the caller got a live job.
	{"enabled", "Enabled", true, func(in schedulerInput) (any, bool) { return valueOf(in.Enabled) }, func(j Job) any { return j.Enabled }},

	{"command", "Command", false, func(in schedulerInput) (any, bool) { return valueOf(in.Command) }, func(j Job) any { return j.Command }},
	{"agent", "Agent", false, func(in schedulerInput) (any, bool) { return valueOf(in.Agent) }, func(j Job) any { return j.Agent }},
	{"prompt", "Prompt", false, func(in schedulerInput) (any, bool) { return valueOf(in.Prompt) }, func(j Job) any { return j.Prompt }},
	{"model_role", "Model role", false, func(in schedulerInput) (any, bool) { return valueOf(in.ModelRole) }, func(j Job) any { return j.ModelRole }},
	{"orchestrator", "Orchestrator", false, func(in schedulerInput) (any, bool) { return valueOf(in.Orchestrator) }, func(j Job) any { return j.Orchestrator }},
	{"session_id", "Session ID", false, func(in schedulerInput) (any, bool) { return valueOf(in.SessionID) }, func(j Job) any { return j.SessionID }},
	{"workspace_id", "Workspace ID", false, func(in schedulerInput) (any, bool) { return valueOf(in.WorkspaceID) }, func(j Job) any { return j.WorkspaceID }},
	{"timeout_seconds", "Timeout (seconds)", false, func(in schedulerInput) (any, bool) { return valueOf(in.TimeoutSecs) }, func(j Job) any { return j.TimeoutSecs }},
}

// describeJobFields renders every field of a job that carries a value, one
// labeled line each, in the order of the table above.
//
// It is deliberately NOT gated on the job's type. The scheduler stores every
// field on every job regardless of type, and the one type gate this tool used
// to have on a read path is exactly what hid a shell job's workspace binding:
// handleGet printed WorkspaceID inside its `type == "agent"` branch, so a shell
// job that had one showed it nowhere. A field a job does not hold is left out
// by being empty, which needs no branch and cannot single out a field to lose.
func describeJobFields(job Job) string {
	var lines []string
	for _, field := range schedulerJobFields {
		value := field.ReadEchoed(job)
		if !field.ShownWhenZero && isZeroFieldValue(value) {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %v", field.Label, value))
	}
	return strings.Join(lines, "\n")
}

// isZeroFieldValue reports whether a field holds the value that means the
// caller never set it. Every field read off the typed Job is a string, an int
// or a bool; anything else is a field added without a case here, and it is
// shown rather than silently dropped.
func isZeroFieldValue(value any) bool {
	switch typed := value.(type) {
	case string:
		return typed == ""
	case int:
		return typed == 0
	case bool:
		return !typed
	default:
		return false
	}
}

func valueOf[T any](field *T) (any, bool) {
	if field == nil {
		return nil, false
	}
	return *field, true
}

// describeValue quotes strings and leaves everything else alone, so that a
// field the server blanked reads as "" rather than as nothing at all.
func describeValue(value any) string {
	if text, isString := value.(string); isString {
		return fmt.Sprintf("%q", text)
	}
	return fmt.Sprintf("%v", value)
}

func handleUpdate(ctx context.Context, baseURL string, client *http.Client, id int64, in schedulerInput) (string, error) {
	reqBody := map[string]interface{}{}
	var requestedChanges []requestedChange
	for _, field := range schedulerJobFields {
		value, set := field.ReadRequested(in)
		if !set {
			continue
		}
		reqBody[field.Name] = value
		requestedChanges = append(requestedChanges, requestedChange{
			Name:       field.Name,
			Value:      value,
			ReadEchoed: field.ReadEchoed,
		})
	}
	if len(requestedChanges) == 0 {
		return "error: update needs at least one field to change", nil
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Sprintf("error marshaling request: %s", err), nil
	}

	url := fmt.Sprintf("%s/api/jobs/%d", baseURL, id)
	resp, err := makeRequest(ctx, "PATCH", url, client, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Sprintf("error: %s", err), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("error reading response: %s", err), nil
	}

	if resp.StatusCode == 404 {
		return fmt.Sprintf("job %d not found", id), nil
	}
	if resp.StatusCode != 200 {
		return fmt.Sprintf("error: API returned %d: %s", resp.StatusCode, string(body)), nil
	}

	var job Job
	if err := json.Unmarshal(body, &job); err != nil {
		return fmt.Sprintf("error parsing response: %s", err), nil
	}

	return describeUpdateOutcome(job, requestedChanges), nil
}

// requestedChange is one field the caller asked to change, carrying the value
// asked for and the reader that finds the same field on the echoed job. The
// reader travels with the value rather than being looked up again by name, so
// the two halves cannot drift apart.
type requestedChange struct {
	Name       string
	Value      any
	ReadEchoed func(Job) any
}

// partitionByWhatTheServerStored splits the requested fields into the ones the
// echoed job agrees with and the ones it does not, naming both values for each
// disagreement. Create and update share it because they share the hazard: both
// verbs answer success while quietly keeping a value the caller did not ask
// for.
//
// Every value compared here is a string, an int or a bool read off the typed
// Job, so == is both safe and exact. It would not be if either side arrived as
// a JSON-decoded any — an int and a float64 are never equal, and a numeric
// field would then report itself as dropped forever.
func partitionByWhatTheServerStored(job Job, requestedFields []requestedChange) (stored, dropped []string) {
	for _, requested := range requestedFields {
		echoed := requested.ReadEchoed(job)
		if echoed == requested.Value {
			stored = append(stored, requested.Name)
			continue
		}
		dropped = append(dropped, fmt.Sprintf("%s (asked for %s, job reads %s)",
			requested.Name, describeValue(requested.Value), describeValue(echoed)))
	}
	return stored, dropped
}

// describeUpdateOutcome reports which of the requested fields the server
// actually applied, judged by comparing each one against the job the server
// echoed back rather than against the request that was sent.
//
// A scheduler that refuses a field answers 200 and returns the row unchanged,
// so a report built from the request alone announces edits that never happened.
// Every value compared here is a string, an int or a bool, so == is both safe
// and exact.
func describeUpdateOutcome(job Job, requestedChanges []requestedChange) string {
	applied, dropped := partitionByWhatTheServerStored(job, requestedChanges)

	if len(dropped) == 0 {
		return fmt.Sprintf("Job %d (%s) updated: %s", job.ID, job.Name, strings.Join(applied, ", "))
	}

	// The server said 200 to something it did not do, so say that first and
	// plainly. A caller that reads only the opening line still learns the edit
	// did not land.
	headline := fmt.Sprintf("Job %d (%s) update was only PARTIALLY applied", job.ID, job.Name)
	if len(applied) == 0 {
		headline = fmt.Sprintf("Job %d (%s) update changed NOTHING", job.ID, job.Name)
	}

	result := fmt.Sprintf("%s — the server answered 200 but its reply still holds the old value for %d of the %d fields sent.\n",
		headline, len(dropped), len(requestedChanges))
	if len(applied) > 0 {
		result += fmt.Sprintf("Applied: %s\n", strings.Join(applied, ", "))
	}
	result += fmt.Sprintf("NOT applied: %s", strings.Join(dropped, ", "))
	return result
}

func handleDelete(ctx context.Context, baseURL string, client *http.Client, id int64) (string, error) {
	url := fmt.Sprintf("%s/api/jobs/%d", baseURL, id)
	resp, err := makeRequest(ctx, "DELETE", url, client, nil)
	if err != nil {
		return fmt.Sprintf("error: %s", err), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return fmt.Sprintf("job %d not found", id), nil
	}
	if resp.StatusCode != 204 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("error: API returned %d: %s", resp.StatusCode, string(body)), nil
	}

	return fmt.Sprintf("Job %d deleted successfully", id), nil
}

func handleRuns(ctx context.Context, baseURL string, client *http.Client, id int64) (string, error) {
	url := fmt.Sprintf("%s/api/jobs/%d/runs", baseURL, id)
	resp, err := makeRequest(ctx, "GET", url, client, nil)
	if err != nil {
		return fmt.Sprintf("error: %s", err), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("error reading response: %s", err), nil
	}

	if resp.StatusCode != 200 {
		return fmt.Sprintf("error: API returned %d: %s", resp.StatusCode, string(body)), nil
	}

	var runs []Run
	if err := json.Unmarshal(body, &runs); err != nil {
		return fmt.Sprintf("error parsing response: %s", err), nil
	}

	if len(runs) == 0 {
		return fmt.Sprintf("no runs found for job %d", id), nil
	}

	result := fmt.Sprintf("Found %d runs for job %d:\n\n", len(runs), id)
	for _, run := range runs {
		result += fmt.Sprintf("Run %d:\nStatus: %s\nStarted: %s\n",
			run.ID, run.Status, run.StartedAt.Format(time.RFC3339))

		if run.FinishedAt != nil {
			result += fmt.Sprintf("Finished: %s\n", run.FinishedAt.Format(time.RFC3339))
		}

		if run.Output != "" {
			// Truncate output if too long
			output := run.Output
			if len(output) > 500 {
				output = schema.TruncateAtRuneBoundary(output, 500) + "... (truncated)"
			}
			result += fmt.Sprintf("Output: %s\n", output)
		}
		result += "\n"
	}

	return result, nil
}
