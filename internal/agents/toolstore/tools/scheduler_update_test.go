package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The scheduler does not refuse a field it will not apply. It answers 200 and
// returns the row unchanged, so the only evidence of a dropped edit is the job
// in the reply. Measured live 2026-08-10 against the binary serving :8092:
//
//	PATCH /api/jobs/50 {"schedule":"0 6 * * *","command":"echo CHANGED"} -> 200
//
// with both fields unchanged in the echoed row and on re-read. The tool used to
// print "updated: schedule, command" for exactly that exchange.
//
// Which fields a real scheduler honors depends on which build is serving the
// port — HEAD takes all thirteen, older binaries take only the three below — so
// these tests drive both shapes rather than picking one and calling it the
// truth. Nothing here needs to know which binary is deployed, which is the
// point of reading the echo instead of the request.
var fieldsTheOlderSchedulerHonors = []string{"enabled", "description", "timeout_seconds"}

// recordedRequest is what the fake saw. The verb is recorded and not only the
// body because a case list made entirely of one verb cannot see a handler that
// hardcodes it: every case here used to be a PATCH, so nothing would have
// noticed create sending one too.
type recordedRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

// fakeScheduler serves the scheduler's two write verbs and its two read routes
// over a job it holds — POST /api/jobs, PATCH /api/jobs/{id}, GET /api/jobs and
// GET /api/jobs/{id}. On the write verbs it applies only the fields it was told
// to honor and silently keeps its own value for the rest. That is what
// schedulers built before 863868a did; since then PATCH takes all thirteen
// fields and answers an unknown one with a 400.
//
// The read routes serve `job` plus any alsoListed jobs. They exist because the
// read paths are a third authoring of the same field list and were measured to
// have their own omissions — extending this fake was cheaper and truer than a
// second one, which would have been free to disagree with this about the shape
// of a job.
func fakeScheduler(t *testing.T, job Job, honored []string, alsoListed ...Job) (baseURL string, lastRequest func() recordedRequest) {
	t.Helper()

	honors := map[string]bool{}
	for _, name := range honored {
		honors[name] = true
	}

	var seen recordedRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" && r.Method != "PATCH" && r.Method != "GET" {
			t.Errorf("fake scheduler got %s, want POST, PATCH or GET", r.Method)
		}
		seen = recordedRequest{Method: r.Method, Path: r.URL.Path}

		if r.Method == "GET" {
			served := append([]Job{job}, alsoListed...)
			w.Header().Set("Content-Type", "application/json")
			var reply any = served
			if r.URL.Path != "/api/jobs" {
				match, found := jobServedAt(served, r.URL.Path)
				if !found {
					http.Error(w, `{"error":"job not found"}`, 404)
					return
				}
				reply = match
			}
			if err := json.NewEncoder(w).Encode(reply); err != nil {
				t.Errorf("fake scheduler could not encode its reply: %s", err)
			}
			return
		}

		if err := json.NewDecoder(r.Body).Decode(&seen.Body); err != nil {
			t.Errorf("fake scheduler could not decode the request body: %s", err)
		}

		for name, value := range seen.Body {
			if !honors[name] {
				continue
			}
			switch name {
			case "name":
				job.Name = value.(string)
			case "description":
				job.Description = value.(string)
			case "schedule":
				job.Schedule = value.(string)
			case "command":
				job.Command = value.(string)
			case "type":
				job.Type = value.(string)
			case "agent":
				job.Agent = value.(string)
			case "prompt":
				job.Prompt = value.(string)
			case "model_role":
				job.ModelRole = value.(string)
			case "orchestrator":
				job.Orchestrator = value.(string)
			case "session_id":
				job.SessionID = value.(string)
			case "workspace_id":
				job.WorkspaceID = value.(string)
			case "timeout_seconds":
				job.TimeoutSecs = int(value.(float64))
			case "enabled":
				job.Enabled = value.(bool)
			default:
				t.Errorf("fake scheduler was told to honor %q but does not know how", name)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			w.WriteHeader(http.StatusCreated)
		}
		if err := json.NewEncoder(w).Encode(job); err != nil {
			t.Errorf("fake scheduler could not encode its reply: %s", err)
		}
	}))
	t.Cleanup(server.Close)

	return server.URL, func() recordedRequest { return seen }
}

// jobServedAt finds the job whose id ends the request path, so that GET of an
// id the fake was never given is a 404 and not somebody else's job.
func jobServedAt(served []Job, path string) (Job, bool) {
	for _, candidate := range served {
		if path == fmt.Sprintf("/api/jobs/%d", candidate.ID) {
			return candidate, true
		}
	}
	return Job{}, false
}

func stringPointer(value string) *string { return &value }
func boolPointer(value bool) *bool       { return &value }

// A job whose stored values differ from everything the tests ask for, so that
// "unchanged" and "applied" can never be confused for one another.
func jobBeforeUpdate() Job {
	return Job{
		ID:          50,
		Name:        "nightly thing",
		Schedule:    "0 0 31 2 *",
		Command:     "echo original",
		Description: "as filed",
		Type:        "shell",
		Enabled:     true,
	}
}

func TestUpdateReportsEveryFieldAppliedWhenTheServerAppliesThemAll(t *testing.T) {
	everyField := []string{"name", "description", "schedule", "command", "prompt", "timeout_seconds", "enabled"}
	baseURL, _ := fakeScheduler(t, jobBeforeUpdate(), everyField)

	report, err := handleUpdate(context.Background(), baseURL, "", 50, schedulerInput{
		Schedule: stringPointer("0 6 * * *"),
		Command:  stringPointer("echo CHANGED"),
	})
	if err != nil {
		t.Fatalf("handleUpdate returned an error: %s", err)
	}

	if !strings.Contains(report, "updated: schedule, command") {
		t.Errorf("a server that applied both fields was not reported as having applied both:\n%s", report)
	}
	if strings.Contains(report, "NOT applied") {
		t.Errorf("a server that applied both fields was reported as dropping one:\n%s", report)
	}
}

// The defect this card was filed for. Against the deployed scheduler these two
// fields are dropped, and the tool announced them as updated.
func TestUpdateDoesNotReportAFieldTheServerSilentlyDropped(t *testing.T) {
	baseURL, _ := fakeScheduler(t, jobBeforeUpdate(), fieldsTheOlderSchedulerHonors)

	report, err := handleUpdate(context.Background(), baseURL, "", 50, schedulerInput{
		Schedule: stringPointer("0 6 * * *"),
		Command:  stringPointer("echo CHANGED"),
	})
	if err != nil {
		t.Fatalf("handleUpdate returned an error: %s", err)
	}

	if strings.Contains(report, "updated: schedule, command") {
		t.Errorf("the tool announced two edits the server never applied:\n%s", report)
	}
	if !strings.Contains(report, "changed NOTHING") {
		t.Errorf("an update that applied no field at all did not say so:\n%s", report)
	}
	for _, want := range []string{"schedule", "command", `"0 6 * * *"`, `"0 0 31 2 *"`} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not mention %s, so the caller cannot see what was dropped:\n%s", want, report)
		}
	}
}

// The mixed case is the one a report has to get right in both directions at
// once: enabled lands, schedule does not, and saying either about the other is
// a lie.
func TestUpdateSeparatesTheAppliedFieldsFromTheDroppedOnes(t *testing.T) {
	baseURL, _ := fakeScheduler(t, jobBeforeUpdate(), fieldsTheOlderSchedulerHonors)

	report, err := handleUpdate(context.Background(), baseURL, "", 50, schedulerInput{
		Enabled:  boolPointer(false),
		Schedule: stringPointer("0 6 * * *"),
	})
	if err != nil {
		t.Fatalf("handleUpdate returned an error: %s", err)
	}

	if !strings.Contains(report, "PARTIALLY applied") {
		t.Errorf("an update that landed one field of two did not report itself as partial:\n%s", report)
	}
	if !strings.Contains(report, "Applied: enabled") {
		t.Errorf("enabled was applied by the server and the report does not say so:\n%s", report)
	}
	appliedSection, droppedSection, found := strings.Cut(report, "NOT applied:")
	if !found {
		t.Fatalf("no NOT applied section in a report with a dropped field:\n%s", report)
	}
	if !strings.Contains(droppedSection, "schedule") {
		t.Errorf("schedule was dropped and is not in the NOT applied section:\n%s", report)
	}
	if strings.Contains(droppedSection, "enabled") {
		t.Errorf("enabled was applied but appears in the NOT applied section:\n%s", report)
	}
	if strings.Contains(appliedSection, "Applied: enabled, schedule") {
		t.Errorf("schedule was dropped but is listed as applied:\n%s", report)
	}
}

// Reading the echo only works if the request carried the field in the first
// place. This is the half b21b4bb fixed, and it stays pinned so a later change
// cannot go back to sending nothing and reporting the echo as agreement.
func TestUpdateForwardsEveryFieldTheCallerNamed(t *testing.T) {
	baseURL, lastRequest := fakeScheduler(t, jobBeforeUpdate(), fieldsTheOlderSchedulerHonors)

	if _, err := handleUpdate(context.Background(), baseURL, "", 50, schedulerInput{
		Schedule: stringPointer("0 6 * * *"),
		Prompt:   stringPointer("summarize yesterday"),
	}); err != nil {
		t.Fatalf("handleUpdate returned an error: %s", err)
	}

	if method := lastRequest().Method; method != "PATCH" {
		t.Errorf("update sent %s, want PATCH", method)
	}
	sent := lastRequest().Body
	for field, want := range map[string]string{"schedule": "0 6 * * *", "prompt": "summarize yesterday"} {
		if got, present := sent[field]; !present {
			t.Errorf("the request never carried %q", field)
		} else if got != want {
			t.Errorf("the request sent %q for %q, want %q", got, field, want)
		}
	}
	if _, present := sent["command"]; present {
		t.Errorf("the request carried command, which the caller never named: %v", sent)
	}
}

// Every other case here compares a string or a bool. timeout_seconds is the
// only number among the thirteen, and the comparison is == on an `any`, which
// is type-sensitive: the value asked for arrives as an int while a number
// decoded straight out of JSON is a float64. If those two ever meet, an applied
// field reports itself as dropped forever and no string case can see it.
func TestUpdateComparesANumericFieldByValueAndNotByItsJSONType(t *testing.T) {
	timeout := 1800
	baseURL, _ := fakeScheduler(t, jobBeforeUpdate(), fieldsTheOlderSchedulerHonors)

	report, err := handleUpdate(context.Background(), baseURL, "", 50, schedulerInput{
		TimeoutSecs: &timeout,
	})
	if err != nil {
		t.Fatalf("handleUpdate returned an error: %s", err)
	}

	if !strings.Contains(report, "updated: timeout_seconds") {
		t.Errorf("the server applied timeout_seconds and the report denies it:\n%s", report)
	}
}

// An unchanged field is not a dropped one. Asking for the value a job already
// holds is a no-op the server applies by definition, and reporting it as
// refused would make every idempotent update look broken.
func TestUpdateCountsAFieldSetToItsCurrentValueAsApplied(t *testing.T) {
	baseURL, _ := fakeScheduler(t, jobBeforeUpdate(), fieldsTheOlderSchedulerHonors)

	report, err := handleUpdate(context.Background(), baseURL, "", 50, schedulerInput{
		Schedule: stringPointer("0 0 31 2 *"),
	})
	if err != nil {
		t.Fatalf("handleUpdate returned an error: %s", err)
	}

	if !strings.Contains(report, "updated: schedule") {
		t.Errorf("asking for the value the job already holds was not reported as applied:\n%s", report)
	}
}
