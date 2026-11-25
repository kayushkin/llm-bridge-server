// Package schedulerjobs holds the one client this repo's own binaries use to
// register their cron job with the scheduler.
//
// Seven binaries in cmd/ used to carry a copy of it: autoworker,
// autoworker-reviewer, email-classifier, kanban-classifier, kanban-curator,
// kanban-dispatcher and kanban-scoper. The copies had drifted. Three refused a
// job list they could not read; four read a 500 or a non-array body as "no jobs
// registered" and then registered a second copy of a live cron job. A fix made
// to one copy (244987e, f119a711) did not reach the others. Stated once here, it
// cannot drift.
//
// What is left in each main.go is that binary's own literals: the job name, the
// schedule, the description, the timeout and how it builds its command line.
// The scheduler URL stays the caller's too: each binary reads it from its
// servicesettings, and this package does not guess one.
//
// Ported 2026-09-26 from refactor/one-scheduler-job-registration-client
// (2339268e), whose version also resolved SCHEDULER_URL itself. Main had since
// moved that setting into servicesettings, so that half was dropped.
package schedulerjobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Job is everything that differs between the binaries' registrations. The job
// is always a shell job, so the type is not a field.
type Job struct {
	// Name is what the scheduler stores and what the duplicate check matches on.
	Name string
	// Schedule is a standard 5-field cron expression.
	Schedule string
	// Description is the prose the scheduler shows next to the job.
	Description string
	// Command is the shell command line the job runs.
	Command string
	// TimeoutSeconds is how long a run may take before the scheduler kills it.
	// Zero leaves it out of the payload, which means the scheduler's 5-minute
	// default.
	TimeoutSeconds int
}

// Payload is the JSON object POST /api/jobs receives for job.
func (job Job) Payload() map[string]any {
	payload := map[string]any{
		"name":        job.Name,
		"description": job.Description,
		"schedule":    job.Schedule,
		"type":        "shell",
		"command":     job.Command,
	}
	if job.TimeoutSeconds != 0 {
		payload["timeout_seconds"] = job.TimeoutSeconds
	}
	return payload
}

// RegisterIfAbsent creates job on the scheduler at schedulerURL unless a job of
// that name is already registered, and reports what it did on stdout.
//
// It reads the job list first because the scheduler will not do this for the
// caller: the jobs table has no unique constraint on name, so two registrations
// of one name both answer 201 and the box then runs the job twice.
//
// That makes the list read load-bearing, so a list read that did not succeed is
// never read as "no such job". Every way it can fail returns an error and
// creates nothing.
func RegisterIfAbsent(schedulerURL string, job Job) error {
	registered, err := registeredJobNames(schedulerURL)
	if err != nil {
		return err
	}
	for _, name := range registered {
		if name == job.Name {
			fmt.Printf("%s already registered; no changes made\n", job.Name)
			return nil
		}
	}
	return create(schedulerURL, job)
}

// registeredJobNames reads the name of every job the scheduler holds. It returns
// an error rather than an empty list for every failure, because the caller
// cannot tell those two answers apart and would register a duplicate.
func registeredJobNames(schedulerURL string) ([]string, error) {
	response, err := http.Get(schedulerURL + "/api/jobs")
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("list jobs: read body: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list jobs: scheduler returned %d: %s",
			response.StatusCode, strings.TrimSpace(string(body)))
	}
	// A failure that answers a JSON object instead of an array must not decode
	// into an empty list, which reads exactly like a scheduler holding no jobs.
	var jobs []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &jobs); err != nil {
		return nil, fmt.Errorf("list jobs: decode job list: %w", err)
	}
	names := make([]string, len(jobs))
	for i, listed := range jobs {
		names[i] = listed.Name
	}
	return names, nil
}

// create POSTs job. Any status of 300 or above is a refusal: the scheduler
// answers 201 on success, and a redirect has created nothing.
func create(schedulerURL string, job Job) error {
	data, err := json.Marshal(job.Payload())
	if err != nil {
		return fmt.Errorf("encode job %s: %w", job.Name, err)
	}
	request, err := http.NewRequest("POST", schedulerURL+"/api/jobs", bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("POST /api/jobs: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode >= 300 {
		return fmt.Errorf("scheduler returned %d: %s", response.StatusCode, string(body))
	}
	fmt.Printf("registered %s: %s\n", job.Name, strings.TrimSpace(string(body)))
	return nil
}
