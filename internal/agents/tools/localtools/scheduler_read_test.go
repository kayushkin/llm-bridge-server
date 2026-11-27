package localtools

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The read half of the same defect. The write paths were folded onto one field
// table and the two READ paths were left hand-written, which made them a third
// authoring of the same list with their own omissions — measured 2026-08-10:
//
//   - handleList named neither Description, TimeoutSecs nor WorkspaceID at all,
//     so a job's note, its wall-clock cap and its durable-memory binding were
//     invisible in a listing. Per ~/CLAUDE.md a shell job's cap is per-job and
//     several live jobs run at 1800 or 3600; a listing that never shows it makes
//     every job look like it runs at the five-minute default.
//   - handleGet printed WorkspaceID inside its `job.Type == "agent"` branch,
//     while the scheduler stores workspace_id on a job of any type and the tool
//     now sends it on any type. So a shell job with a workspace showed it on
//     neither read path.
//
// Both are silent. A caller who set a field and reads it back sees a job that
// simply does not have it, which is what a job that never got one looks like.
//
// ⚠️ The interesting fields here belong to agent jobs, so a case list written
// without thinking defaults to agent jobs throughout — and the type-gate bug
// above is then invisible to every case in it. Shell-job cases are the point.

// A shell job holding every field the gate used to hide: a description, a
// non-default timeout, and a workspace on a job whose type is not "agent".
func shellJobWithEveryFieldTheReadPathsHid() Job {
	return Job{
		ID:          61,
		Name:        "nightly repo guard",
		Description: "walks every repo and reports drift",
		Schedule:    "0 3 * * *",
		Command:     "repo-guard --all",
		Type:        "shell",
		WorkspaceID: "ws-guard",
		TimeoutSecs: 3600,
		Enabled:     true,
	}
}

// Every field set at once, command included. The scheduler stores command on a
// job of any type, so an agent job holding one is not a contradiction — it is
// the cross-type case that fails if any read path grows a type gate back.
func agentJobWithEveryField() Job {
	return Job{
		ID:           62,
		Name:         "nightly summary",
		Description:  "summarizes yesterday",
		Schedule:     "0 8 * * *",
		Command:      "echo left over from when this was a shell job",
		Type:         "agent",
		Agent:        "claude-code",
		Prompt:       "summarize yesterday",
		ModelRole:    "balanced",
		Orchestrator: "claude-code",
		SessionID:    "sess-7",
		WorkspaceID:  "ws-42",
		TimeoutSecs:  1800,
		Enabled:      true,
	}
}

// The type gate, stated on its own. This is the case the card was filed for and
// the one an agent-only case list cannot contain.
func TestGetShowsTheWorkspaceOfAShellJob(t *testing.T) {
	job := shellJobWithEveryFieldTheReadPathsHid()
	baseURL, _ := fakeScheduler(t, job, nil)

	report, err := handleGet(context.Background(), baseURL, http.DefaultClient, job.ID)
	if err != nil {
		t.Fatalf("handleGet returned an error: %s", err)
	}

	if !strings.Contains(report, "Workspace ID: ws-guard") {
		t.Errorf("a shell job's workspace binding is missing from get, so it reads as a job that never had one:\n%s", report)
	}
	if !strings.Contains(report, "Command: repo-guard --all") {
		t.Errorf("get dropped the shell job's command:\n%s", report)
	}
}

func TestListShowsTheWorkspaceOfAShellJob(t *testing.T) {
	job := shellJobWithEveryFieldTheReadPathsHid()
	baseURL, _ := fakeScheduler(t, job, nil)

	report, err := handleList(context.Background(), baseURL, http.DefaultClient)
	if err != nil {
		t.Fatalf("handleList returned an error: %s", err)
	}

	if !strings.Contains(report, "Workspace ID: ws-guard") {
		t.Errorf("a shell job's workspace binding is missing from the listing:\n%s", report)
	}
}

// The three fields the listing never mentioned, on both job types at once so
// that a fix which only reaches one type fails here.
func TestListShowsTheDescriptionTimeoutAndWorkspaceOfEveryJobType(t *testing.T) {
	shellJob := shellJobWithEveryFieldTheReadPathsHid()
	agentJob := agentJobWithEveryField()
	baseURL, _ := fakeScheduler(t, shellJob, nil, agentJob)

	report, err := handleList(context.Background(), baseURL, http.DefaultClient)
	if err != nil {
		t.Fatalf("handleList returned an error: %s", err)
	}

	for _, want := range []string{
		"Description: walks every repo and reports drift",
		"Timeout (seconds): 3600",
		"Workspace ID: ws-guard",
		"Description: summarizes yesterday",
		"Timeout (seconds): 1800",
		"Workspace ID: ws-42",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the listing never shows %q:\n%s", want, report)
		}
	}
}

// A listing that shows a job's cap only sometimes is worse than one that never
// does, because the times it stays silent then read as the default. 0 IS the
// default, so it is the one value worth leaving out.
func TestListLeavesOutATimeoutOfZeroBecauseZeroMeansTheDefault(t *testing.T) {
	job := shellJobWithEveryFieldTheReadPathsHid()
	job.TimeoutSecs = 0
	job.Description = ""
	job.WorkspaceID = ""
	baseURL, _ := fakeScheduler(t, job, nil)

	report, err := handleList(context.Background(), baseURL, http.DefaultClient)
	if err != nil {
		t.Fatalf("handleList returned an error: %s", err)
	}

	for _, unwanted := range []string{"Timeout", "Description", "Workspace ID"} {
		if strings.Contains(report, unwanted) {
			t.Errorf("a job that holds no %s got a line claiming otherwise:\n%s", unwanted, report)
		}
	}
	// The job is still there — the case above must fail for the right reason.
	if !strings.Contains(report, "Name: nightly repo guard") {
		t.Errorf("the listing lost the job entirely:\n%s", report)
	}
}

// The guard against the next field being forgotten, which is the whole reason
// the read paths were folded onto the write paths' table. Every field the tool
// can write must come back out on both read paths.
//
// The fixture completeness check above the assertions is what keeps this
// honest: a field added to the table but not to the fixture would otherwise
// pass by holding its zero value and being legitimately hidden.
func TestBothReadPathsShowEveryFieldTheWritePathsCanSet(t *testing.T) {
	// A floor on a case list generated from a shipped table. Without it a
	// row deleted from schedulerJobFields silently deletes its coverage here
	// and the suite still passes.
	if len(schedulerJobFields) < 13 {
		t.Fatalf("schedulerJobFields is down to %d rows; it had 13 when this case was written, so coverage has been deleted rather than the table shrunk on purpose", len(schedulerJobFields))
	}

	job := agentJobWithEveryField()
	baseURL, _ := fakeScheduler(t, job, nil)

	listing, err := handleList(context.Background(), baseURL, http.DefaultClient)
	if err != nil {
		t.Fatalf("handleList returned an error: %s", err)
	}
	single, err := handleGet(context.Background(), baseURL, http.DefaultClient, job.ID)
	if err != nil {
		t.Fatalf("handleGet returned an error: %s", err)
	}

	for _, field := range schedulerJobFields {
		value := field.ReadEchoed(job)
		if isZeroFieldValue(value) {
			t.Errorf("the fixture leaves %s at its zero value, so this case cannot tell whether the read paths show it", field.Name)
			continue
		}
		want := fmt.Sprintf("%s: %v", field.Label, value)
		if !strings.Contains(listing, want) {
			t.Errorf("handleList never shows %s — expected a %q line:\n%s", field.Name, want, listing)
		}
		if !strings.Contains(single, want) {
			t.Errorf("handleGet never shows %s — expected a %q line:\n%s", field.Name, want, single)
		}
	}
}

// The two read paths describing the same job must not disagree about it. They
// diverged once already — get grew description and timeout while list did not —
// and nothing related the two.
func TestTheTwoReadPathsDescribeAJobIdentically(t *testing.T) {
	job := shellJobWithEveryFieldTheReadPathsHid()
	baseURL, _ := fakeScheduler(t, job, nil)

	listing, err := handleList(context.Background(), baseURL, http.DefaultClient)
	if err != nil {
		t.Fatalf("handleList returned an error: %s", err)
	}
	single, err := handleGet(context.Background(), baseURL, http.DefaultClient, job.ID)
	if err != nil {
		t.Fatalf("handleGet returned an error: %s", err)
	}

	described := describeJobFields(job)
	if !strings.Contains(listing, described) {
		t.Errorf("the listing does not describe the job the way get does:\nwant block:\n%s\ngot:\n%s", described, listing)
	}
	if !strings.Contains(single, described) {
		t.Errorf("get does not describe the job the way the shared renderer does:\nwant block:\n%s\ngot:\n%s", described, single)
	}
}

// A hold is the reason an enabled job is not running, so both read paths have to
// say so. The listing used to append "(on hold)" to a status word and never name
// the time, which cannot answer "for how long".
func TestBothReadPathsNameTheTimeAHoldExpires(t *testing.T) {
	heldUntil := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	job := shellJobWithEveryFieldTheReadPathsHid()
	job.HoldUntil = &heldUntil
	baseURL, _ := fakeScheduler(t, job, nil)

	listing, err := handleList(context.Background(), baseURL, http.DefaultClient)
	if err != nil {
		t.Fatalf("handleList returned an error: %s", err)
	}
	single, err := handleGet(context.Background(), baseURL, http.DefaultClient, job.ID)
	if err != nil {
		t.Fatalf("handleGet returned an error: %s", err)
	}

	want := "On hold until: " + heldUntil.Format(time.RFC3339)
	if !strings.Contains(listing, want) {
		t.Errorf("the listing does not say when the hold expires:\n%s", listing)
	}
	if !strings.Contains(single, want) {
		t.Errorf("get does not say when the hold expires:\n%s", single)
	}
	// An expired hold is not a hold, and reporting one would park a job that
	// is actually due to run.
	if strings.Contains(listing, "On hold") != strings.Contains(single, "On hold") {
		t.Errorf("the two read paths disagree about whether the job is held")
	}
}

func TestAReadPathDoesNotReportAnExpiredHold(t *testing.T) {
	expired := time.Now().Add(-time.Hour).UTC()
	job := shellJobWithEveryFieldTheReadPathsHid()
	job.HoldUntil = &expired
	baseURL, _ := fakeScheduler(t, job, nil)

	single, err := handleGet(context.Background(), baseURL, http.DefaultClient, job.ID)
	if err != nil {
		t.Fatalf("handleGet returned an error: %s", err)
	}

	if strings.Contains(single, "On hold") {
		t.Errorf("a job whose hold has already expired was reported as held:\n%s", single)
	}
}

// The verb-and-route axis. Every other case in this file reads whatever the
// fake serves, so a handler that asked the wrong route would still be handed a
// job and would still print it.
func TestGetSaysNotFoundRatherThanDescribingSomeOtherJob(t *testing.T) {
	baseURL, _ := fakeScheduler(t, shellJobWithEveryFieldTheReadPathsHid(), nil)

	report, err := handleGet(context.Background(), baseURL, http.DefaultClient, 999)
	if err != nil {
		t.Fatalf("handleGet returned an error: %s", err)
	}

	if !strings.Contains(report, "not found") {
		t.Errorf("asking for a job that does not exist did not say so:\n%s", report)
	}
	if strings.Contains(report, "nightly repo guard") {
		t.Errorf("get answered with a job other than the one asked for:\n%s", report)
	}
}
