package bus

import (
	"strings"
	"testing"

	"github.com/slava/scheduler/internal/db"

	_ "modernc.org/sqlite"
)

// The bus and the HTTP handler are the only two ways a job gets created, and
// they used to disagree about what a valid job is. internal/api ran validateJob
// — name, schedule, a real cron parse, timeout, a known type, the fields that
// type needs. handleCreateJob hand-rolled a subset of the same list and skipped
// the two checks that cost the most: it never parsed the cron expression and
// never rejected an unknown type, because it only branched on "shell" and
// "agent" and let anything else fall through into the database.
//
// Both cases below were measured against the old handler before the fix; each
// returned err=nil and left a row behind.

// TestCreateJobRejectsAnUnparseableCron is the more expensive of the two,
// because the job is stored and then fails at a completely different place. The
// scheduler logs `failed to add job N with schedule "...": expected exactly 5
// fields` at load time and moves on, so the job exists, reads back fine over
// every API, and never runs. That is the exact failure validateJob's own
// comment says was fixed — it was fixed at one edge of two.
func TestCreateJobRejectsAnUnparseableCron(t *testing.T) {
	store, _, _, handler, err := setupTest()
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	if _, err := handler.handleCreateJob(Command{
		Action: "create_job", Name: "j", Schedule: "not-a-cron", Command: "echo",
	}); err == nil {
		t.Error("an unparseable cron expression was accepted")
	}
	assertNoJobsStored(t, store)
}

// TestCreateJobRejectsAnUnknownType pins the fall-through. The old handler
// asked "is it shell?" and "is it agent?" and required nothing of a job that
// was neither, so "banana" was stored with no command and no prompt — a job no
// executor has a branch for.
func TestCreateJobRejectsAnUnknownType(t *testing.T) {
	store, _, _, handler, err := setupTest()
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	if _, err := handler.handleCreateJob(Command{
		Action: "create_job", Name: "j", Schedule: "* * * * *", Type: "banana",
	}); err == nil {
		t.Error("an unknown job type was accepted")
	}
	assertNoJobsStored(t, store)
}

// TestCreateJobStillRejectsWhatItAlreadyRejected covers the checks the
// hand-rolled subset did have, so swapping it for db.ValidateJob cannot quietly
// drop one.
//
// Each case asserts the MESSAGE, not just that something was refused, and that
// is not belt-and-braces. Deleting ValidateJob's `schedule is required` check
// reddens nothing if you only assert refusal: cron.ParseStandard("") fails too,
// so the job is still refused, by a different rule, with a worse message. The
// check buys legibility rather than a refusal, and a test that cannot tell the
// two apart would report coverage it does not have. Verified by deleting each
// check separately — with the message assertions in place, all five move.
func TestCreateJobStillRejectsWhatItAlreadyRejected(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		command Command
		want    string
	}{
		{"a job with no name", Command{Schedule: "* * * * *", Command: "echo"}, "name is required"},
		{"a job with no schedule", Command{Name: "j", Command: "echo"}, "schedule is required"},
		{"a shell job with no command", Command{Name: "j", Schedule: "* * * * *", Type: "shell"}, "command is required for shell jobs"},
		{"an agent job with no agent", Command{Name: "j", Schedule: "* * * * *", Type: "agent", Prompt: "hi"}, "agent and prompt is required for agent jobs"},
		{"an agent job with no prompt", Command{Name: "j", Schedule: "* * * * *", Type: "agent", Agent: "claude-code"}, "agent and prompt is required for agent jobs"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, _, _, handler, err := setupTest()
			if err != nil {
				t.Fatalf("setup failed: %v", err)
			}
			testCase.command.Action = "create_job"
			_, err = handler.handleCreateJob(testCase.command)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("refused for the wrong reason:\n got %q\nwant it to contain %q", err, testCase.want)
			}
			assertNoJobsStored(t, store)
		})
	}
}

// TestCreateJobStillAcceptsARunnableJob is the other half of the refusals
// above: a tightened edge that rejects everything would pass every test in this
// file and break the bus completely. An omitted type still means shell.
func TestCreateJobStillAcceptsARunnableJob(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		command Command
	}{
		{"an explicit shell job", Command{Name: "j", Schedule: "0 3 * * *", Command: "echo", Type: "shell"}},
		{"a job with no type at all", Command{Name: "j", Schedule: "0 3 * * *", Command: "echo"}},
		{"an agent job", Command{Name: "j", Schedule: "0 3 * * *", Type: "agent", Agent: "claude-code", Prompt: "hi"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, _, _, handler, err := setupTest()
			if err != nil {
				t.Fatalf("setup failed: %v", err)
			}
			testCase.command.Action = "create_job"
			if _, err := handler.handleCreateJob(testCase.command); err != nil {
				t.Fatalf("refused a runnable job: %v", err)
			}
			jobs, err := store.ListJobs()
			if err != nil {
				t.Fatal(err)
			}
			if len(jobs) != 1 {
				t.Fatalf("expected 1 stored job, got %d", len(jobs))
			}
		})
	}
}

// assertNoJobsStored is the half of a refusal that matters. Returning an error
// while writing the row anyway would leave the caller with a job it was told it
// did not create — worse than the defect, not better.
func assertNoJobsStored(t *testing.T, store *db.Store) {
	t.Helper()
	jobs, err := store.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("refused the job but stored %d anyway: %+v", len(jobs), jobs)
	}
}
