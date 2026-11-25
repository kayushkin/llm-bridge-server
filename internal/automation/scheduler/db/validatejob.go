package db

import (
	"fmt"
	"sort"
	"strings"

	"github.com/robfig/cron/v3"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/eventtriggers"
)

// This file holds the one definition of what a runnable job is. It lives in db
// rather than in either caller because a job can be created from two unrelated
// places — POST /api/jobs in internal/api and the create_job bus command in
// internal/bus — and when the rule lived in internal/api the bus hand-rolled a
// subset of it. Measured before the move: the bus accepted the schedule
// "not-a-cron" and the type "banana", stored both, and answered success. The
// cron one then failed at load time with a log line nobody reads, so the job
// existed, read back fine over every API, and never ran.
//
// internal/db is the lowest package both edges already import, and Job is
// defined here, so this is the only place the rule can sit without either edge
// depending on the other.

// requiredField is a field a job type cannot run without, paired with the way
// to read it off a job.
type requiredField struct {
	Name string
	Read func(Job) string
}

// jobTypeRequirements is the closed set of job types, each mapped to the fields
// that type cannot run without.
var jobTypeRequirements = map[string][]requiredField{
	"shell": {{"command", func(j Job) string { return j.Command }}},
	"agent": {
		{"agent", func(j Job) string { return j.Agent }},
		{"prompt", func(j Job) string { return j.Prompt }},
	},
}

// ValidateJob reports why a job is unrunnable, or "" if it is fine. Every edge
// that can produce a job runs it: POST /api/jobs, PATCH /api/jobs/{id}, and the
// create_job bus command. So a job cannot be created invalid over either
// transport, nor edited into an invalid state — PATCH validates the merged
// result, not the patch.
//
// It returns a reason rather than an error because both HTTP callers put the
// string straight into a 400 body, and the bus caller wraps it. An error type
// would buy nothing neither of them would immediately unwrap.
func ValidateJob(job Job) string {
	if job.Name == "" {
		return "name is required"
	}
	// This one buys a MESSAGE, not a refusal, and the distinction is worth
	// writing down because it looks like dead weight otherwise. Deleting it
	// still refuses an empty schedule — cron.ParseStandard("") fails two lines
	// below with `invalid schedule "": empty spec string`. Measured. Keep it for
	// the wording, and do not cite it as the thing that stops an empty schedule.
	// A job runs on its schedule or on its event trigger. Both would leave the
	// prompt a template on some runs and plain text on others.
	if job.EventTrigger != nil && job.Schedule != "" {
		return "a job runs on a schedule or on an event_trigger, not both"
	}
	if job.EventTrigger == nil && job.Schedule == "" {
		return "schedule or event_trigger is required"
	}
	// The cron expression used to go unchecked, so a job with an unparseable
	// schedule was accepted, then skipped with a log line at load time and never
	// ran again. Reject it at the edge instead.
	if job.EventTrigger == nil {
		if _, err := cron.ParseStandard(job.Schedule); err != nil {
			return fmt.Sprintf("invalid schedule %q: %s", job.Schedule, err)
		}
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
	// Only an agent job starts a session, so a role on any other job would be
	// stored, shown and never used — the fate of the model column it replaced.
	if job.ModelRole != "" && job.Type != "agent" {
		return fmt.Sprintf("model_role is only for agent jobs; a %s job starts no session", job.Type)
	}
	if job.EventTrigger != nil {
		// A shell command filled from a stranger's chat message would run
		// whatever they typed.
		if job.Type != "agent" {
			return fmt.Sprintf("event_trigger is only for agent jobs; a %s job's command cannot be filled from a message", job.Type)
		}
		if problem := eventtriggers.Validate(*job.EventTrigger, job.Prompt); problem != "" {
			return problem
		}
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

// DefaultJobType is what an omitted type means. Both creation edges default the
// same way, and they have to agree: ValidateJob rejects "" as an unknown type,
// so an edge that forgot to default would refuse every request that left the
// field out — which is most of them.
const DefaultJobType = "shell"
