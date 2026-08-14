package main

import (
	"testing"
	"time"

	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/runstatus"
)

type finishEntry struct {
	jobID    int64
	jobName  string
	schedule string
	runID    int64
	status   string
	output   string
	duration time.Duration
}

type recordingLogger struct{ finishes []finishEntry }

func (l *recordingLogger) LogJobFinish(jobID int64, jobName, schedule string, runID int64, status, output string, duration time.Duration) {
	l.finishes = append(l.finishes, finishEntry{jobID, jobName, schedule, runID, status, output, duration})
}

func started(minutesAgo int) time.Time {
	return time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC).Add(-time.Duration(minutesAgo) * time.Minute)
}

// Startup reconciliation does not run through the cron loop, so nothing else
// can report the runs it closes. Before this, they were corrected in SQLite and
// published nowhere: a deploy restart that killed a job mid-flight was visible
// only to somebody reading /api/jobs/<id>/runs by hand.
func TestPublishInterruptedRunsReportsEveryClosedRun(t *testing.T) {
	finishedAt := started(0)
	runs := []db.InterruptedRun{
		{
			RunID: 41, JobID: 7, JobName: "nightly-guard", Schedule: "0 3 * * *",
			Output: db.InterruptedRunOutput, StartedAt: started(90), FinishedAt: finishedAt,
		},
		{
			RunID: 42, JobID: 9, JobName: "every-five", Schedule: "*/5 * * * *",
			Output: "half a log line", StartedAt: started(3), FinishedAt: finishedAt,
		},
	}

	logger := &recordingLogger{}
	publishInterruptedRuns(logger, runs)

	// One entry per run, not one summary. A consumer counting finishes has to
	// see these the way it sees every other finish.
	if len(logger.finishes) != 2 {
		t.Fatalf("published %d finishes for 2 closed runs, want 2", len(logger.finishes))
	}

	first := logger.finishes[0]
	if first.runID != 41 || first.jobID != 7 || first.jobName != "nightly-guard" || first.schedule != "0 3 * * *" {
		t.Errorf("first finish reported run %d job %d %q %q, want run 41 job 7 %q %q",
			first.runID, first.jobID, first.jobName, first.schedule, "nightly-guard", "0 3 * * *")
	}
	if first.output != db.InterruptedRunOutput {
		t.Errorf("first finish published output %q, want the stored explanation %q",
			first.output, db.InterruptedRunOutput)
	}
	// Duration is start until reconciliation noticed. The moment the process
	// actually died is unrecorded, so this is the only honest number available.
	if first.duration != 90*time.Minute {
		t.Errorf("first finish reported duration %v, want %v", first.duration, 90*time.Minute)
	}

	// A second run with different values, so the loop cannot pass by publishing
	// the first run twice.
	second := logger.finishes[1]
	if second.runID != 42 || second.jobID != 9 || second.jobName != "every-five" {
		t.Errorf("second finish reported run %d job %d %q, want run 42 job 9 %q",
			second.runID, second.jobID, second.jobName, "every-five")
	}
	if second.output != "half a log line" {
		t.Errorf("second finish published output %q, want the run's own output", second.output)
	}
	if second.duration != 3*time.Minute {
		t.Errorf("second finish reported duration %v, want %v", second.duration, 3*time.Minute)
	}
}

// The status is what decides the level. Publishing these as anything the
// vocabulary calls a success would file a killed run at info, which is the
// defect one layer down that runstatus exists to prevent.
func TestPublishInterruptedRunsPublishesAStatusThatEscalates(t *testing.T) {
	logger := &recordingLogger{}
	publishInterruptedRuns(logger, []db.InterruptedRun{
		{RunID: 1, JobID: 1, JobName: "j", StartedAt: started(1), FinishedAt: started(0)},
	})

	if len(logger.finishes) != 1 {
		t.Fatalf("published %d finishes, want 1", len(logger.finishes))
	}
	if got := logger.finishes[0].status; got != runstatus.Interrupted {
		t.Errorf("published status %q, want %q", got, runstatus.Interrupted)
	}
	if !runstatus.TerminalStatusIsFailure(logger.finishes[0].status) {
		t.Errorf("published status %q is filed as a success, so a killed run logs at info level",
			logger.finishes[0].status)
	}
}

// Cry-wolf control: a boot with nothing stranded must stay silent. A reconciler
// that announced itself on every clean start would make the real event
// unfindable.
func TestPublishInterruptedRunsSaysNothingWhenNothingWasStranded(t *testing.T) {
	logger := &recordingLogger{}
	publishInterruptedRuns(logger, nil)
	if len(logger.finishes) != 0 {
		t.Errorf("a clean boot published %d finishes, want 0", len(logger.finishes))
	}
}
