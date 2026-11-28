package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Ported from 145c5bae (test/the-no-secret-rule-is-pinned-by-its-message). On
// main registerSchedulerJob takes the scheduler URL as an argument, so the
// fixture no longer has to override the environment and a compiled-in default.
//
// -register is how this binary gets onto the cron schedule at all, and until
// this file nothing had ever run it. The suite drives selectQueuedCard,
// evaluate and startWorker — every one of them downstream of the tick.
// registerSchedulerJob is reached only from main, no test runs main, and the
// scorer's denominator never mentioned it, so all three of the standing
// instruments reported the gap as an absence of risk.

// schedulerFixture stands in for the scheduler's /api/jobs surface and records
// what -register posted.
type schedulerFixture struct {
	// existing names the jobs the scheduler already carries, which is what the
	// idempotence check reads.
	existing []string
	// refuse makes the create return 500, which is how a scheduler that will
	// not take the job presents itself.
	refuse bool

	posted []map[string]any
	url    string
}

// startSchedulerFixture starts the fixture and records its URL. Tests pass that
// URL to registerSchedulerJob explicitly, so nothing can reach the scheduler
// running on this box.
func startSchedulerFixture(t *testing.T, f *schedulerFixture) *schedulerFixture {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/jobs" && r.Method == http.MethodGet:
			rows := make([]map[string]any, 0, len(f.existing))
			for _, name := range f.existing {
				rows = append(rows, map[string]any{"name": name})
			}
			writeFakeJSON(w, rows)

		case r.URL.Path == "/api/jobs" && r.Method == http.MethodPost:
			if f.refuse {
				http.Error(w, "the scheduler refused it", http.StatusInternalServerError)
				return
			}
			var payload map[string]any
			json.NewDecoder(r.Body).Decode(&payload)
			f.posted = append(f.posted, payload)
			w.WriteHeader(http.StatusCreated)

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	f.url = srv.URL
	return f
}

// testRegisteredBinary stands in for ~/bin/autoworker; where that path comes
// from is pinned by TestHomeRelativePathsAreBuiltFromTheGivenHome.
const testRegisteredBinary = "/home/someone/bin/autoworker"

func (f *schedulerFixture) only(t *testing.T) map[string]any {
	t.Helper()
	if len(f.posted) != 1 {
		t.Fatalf("registered %d jobs, want exactly 1: %v", len(f.posted), f.posted)
	}
	return f.posted[0]
}

// TestRegisteringCreatesTheTickJobTheRestOfTheDesignAssumes. Every gate this
// binary owns — the usage curve, the per-provider cooldown, the safe window —
// is expressed in units of ticks, so the cadence is not a preference. A job
// registered on the wrong schedule fires a budgeted agent at a rate nothing
// else in the binary was written for, and it does it silently.
func TestRegisteringCreatesTheTickJobTheRestOfTheDesignAssumes(t *testing.T) {
	f := startSchedulerFixture(t, &schedulerFixture{})

	if err := registerSchedulerJob(f.url, testRegisteredBinary); err != nil {
		t.Fatalf("registerSchedulerJob: %v", err)
	}

	job := f.only(t)
	if job["name"] != "autoworker-tick" {
		t.Errorf("job name = %q, want autoworker-tick — the name a later -register "+
			"looks for, so a name that drifts registers a second dispatcher beside "+
			"the first rather than failing", job["name"])
	}
	if job["schedule"] != "*/30 * * * *" {
		t.Errorf("schedule = %q, want */30 * * * *", job["schedule"])
	}
	if job["type"] != "shell" {
		t.Errorf("type = %q, want shell", job["type"])
	}
	command, _ := job["command"].(string)
	if !strings.HasSuffix(command, "/bin/autoworker") {
		t.Errorf("command = %q, want the autoworker binary", command)
	}
}

// TestAnAlreadyRegisteredTickIsNotRegisteredTwice. -register is documented as
// idempotent and is run by hand on every deploy. The scheduler does not reject a
// duplicate name, so this check is the only thing between a re-run and two
// dispatchers firing budgeted agents on the same cadence, each unaware of the
// other's cooldown state — the state file is keyed by provider, not by job.
func TestAnAlreadyRegisteredTickIsNotRegisteredTwice(t *testing.T) {
	f := startSchedulerFixture(t, &schedulerFixture{existing: []string{"autoworker-tick"}})

	if err := registerSchedulerJob(f.url, testRegisteredBinary); err != nil {
		t.Fatalf("registerSchedulerJob: %v", err)
	}

	if len(f.posted) != 0 {
		t.Errorf("registered a second tick over one already there: %v", f.posted)
	}
}

// TestAnUnrelatedJobDoesNotLookLikeThisOne. The idempotence check walks every
// job the scheduler carries, and this box carries dozens. A comparison that
// matched loosely would read a neighbor as proof the tick is registered and
// leave the dispatcher off cron entirely, reporting success.
func TestAnUnrelatedJobDoesNotLookLikeThisOne(t *testing.T) {
	f := startSchedulerFixture(t, &schedulerFixture{
		existing: []string{"autoworker-reviewer", "autoworker-tick-hourly", "kanban-curator-tick"},
	})

	if err := registerSchedulerJob(f.url, testRegisteredBinary); err != nil {
		t.Fatalf("registerSchedulerJob: %v", err)
	}

	if job := f.only(t); job["name"] != "autoworker-tick" {
		t.Errorf("job name = %q, want autoworker-tick", job["name"])
	}
}

// TestASchedulerThatRefusesTheJobIsReportedAsAnError. -register is run by hand
// and its exit status is all the operator sees. A refusal reported as success
// leaves a dispatcher that was never scheduled and a deploy that said it was —
// and nothing else on this box notices an autoworker that simply never fires.
func TestASchedulerThatRefusesTheJobIsReportedAsAnError(t *testing.T) {
	f := startSchedulerFixture(t, &schedulerFixture{refuse: true})

	err := registerSchedulerJob(f.url, testRegisteredBinary)
	if err == nil {
		t.Fatal("no error from a scheduler that refused the job; -register would exit 0 " +
			"having registered nothing")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error does not carry what the scheduler said: %v", err)
	}
}
