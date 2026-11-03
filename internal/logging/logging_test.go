package logging

import (
	"testing"
	"time"

	"github.com/slava/scheduler/internal/runstatus"
)

// recordingPublisher stands in for the bus and keeps what it was handed. It
// records rather than matches: nothing here decides whether an entry is
// acceptable, so an assertion cannot be satisfied by the double agreeing with
// the code under test.
type recordingPublisher struct {
	subjects []string
	entries  []LogEntry
	err      error
}

func (p *recordingPublisher) Publish(subject string, data any) error {
	entry, ok := data.(LogEntry)
	if !ok {
		panic("logging published something that is not a LogEntry")
	}
	p.subjects = append(p.subjects, subject)
	p.entries = append(p.entries, entry)
	return p.err
}

func loggerWithRecorder() (*Logger, *recordingPublisher) {
	rec := &recordingPublisher{}
	return &Logger{client: rec}, rec
}

func (p *recordingPublisher) only(t *testing.T) LogEntry {
	t.Helper()
	if len(p.entries) != 1 {
		t.Fatalf("expected exactly one published entry, got %d", len(p.entries))
	}
	return p.entries[0]
}

// TestARunThatDidNotSucceedIsFiledAtErrorLevel drives LogJobFinish itself
// rather than the predicate it calls. The predicate being right is not the
// property that matters; the property that matters is that the finish logger
// asks it.
func TestARunThatDidNotSucceedIsFiledAtErrorLevel(t *testing.T) {
	for _, status := range []string{
		runstatus.Error,
		runstatus.Timeout,
		runstatus.Interrupted,
		runstatus.Running,
	} {
		t.Run(status, func(t *testing.T) {
			logger, rec := loggerWithRecorder()
			logger.LogJobFinish(7, "nightly", "0 3 * * *", 42, status, "out", time.Second)

			if got := rec.only(t).Level; got != "error" {
				t.Fatalf("a run that finished %q was filed at level %q, want \"error\" — "+
					"anyone filtering the log bus by level would never see it", status, got)
			}
		})
	}
}

// TestATimedOutRunIsNotAnInfoLevelEvent is the defect this change was made for,
// pinned on its own so a regression names itself. The deployed scheduler ends a
// run that outlived its timeout_seconds cap as "timeout"; the finish logger used
// to escalate only on "error" and "failed", so the killed run was filed
// indistinguishably from one that succeeded.
func TestATimedOutRunIsNotAnInfoLevelEvent(t *testing.T) {
	logger, rec := loggerWithRecorder()
	logger.LogJobFinish(1, "repo-guard", "0 4 * * *", 9, runstatus.Timeout, "killed after 1h0m0s", time.Hour)

	entry := rec.only(t)
	if entry.Level != "error" {
		t.Fatalf("a timed-out run was filed at level %q, want \"error\"", entry.Level)
	}
	if entry.Metadata["status"] != runstatus.Timeout {
		t.Fatalf("finish entry carried status %v, want %q", entry.Metadata["status"], runstatus.Timeout)
	}
}

// TestASuccessfulRunStaysAtInfoLevel is the control. The failure mode of every
// repair in this family is crying wolf, so the quiet case is asserted rather
// than assumed.
func TestASuccessfulRunStaysAtInfoLevel(t *testing.T) {
	logger, rec := loggerWithRecorder()
	logger.LogJobFinish(3, "disk-root health check", "*/5 * * * *", 11, runstatus.Success, "ok", 250*time.Millisecond)

	if got := rec.only(t).Level; got != "info" {
		t.Fatalf("a successful run was filed at level %q, want \"info\" — "+
			"escalating the quiet case is how an error level stops meaning anything", got)
	}
}

// TestAStatusTheVocabularyDoesNotKnowIsFiledAtErrorLevel pins the reason
// TerminalStatusIsFailure is written as "not success" rather than as a list.
// A status added elsewhere and never mentioned here must surface, not vanish.
func TestAStatusTheVocabularyDoesNotKnowIsFiledAtErrorLevel(t *testing.T) {
	logger, rec := loggerWithRecorder()
	logger.LogJobFinish(4, "job", "* * * * *", 12, "cancelled-by-operator", "", time.Second)

	if got := rec.only(t).Level; got != "error" {
		t.Fatalf("an unrecognized terminal status was filed at level %q, want \"error\"", got)
	}
}

// TestTheStartEntryIsAlwaysInfo guards the escalation from spreading: only the
// finish entry consults the run's status.
func TestTheStartEntryIsAlwaysInfo(t *testing.T) {
	logger, rec := loggerWithRecorder()
	logger.LogJobStart(5, "job", "* * * * *", 13)

	entry := rec.only(t)
	if entry.Level != "info" {
		t.Fatalf("job start was filed at level %q, want \"info\"", entry.Level)
	}
	if entry.Metadata["action"] != "start" {
		t.Fatalf("job start recorded action %v, want \"start\"", entry.Metadata["action"])
	}
}

// TestTheFinishEntryGoesToTheSchedulerLogSubject pins the wire detail the
// recorder is able to see. Without it the double would be observing the level
// and nothing else.
func TestTheFinishEntryGoesToTheSchedulerLogSubject(t *testing.T) {
	logger, rec := loggerWithRecorder()
	logger.LogJobFinish(6, "job", "* * * * *", 14, runstatus.Error, "boom", time.Second)

	if len(rec.subjects) != 1 || rec.subjects[0] != "logs.scheduler" {
		t.Fatalf("published to %v, want [logs.scheduler]", rec.subjects)
	}
}

// TestALongOutputIsTruncatedInTheFinishEntry straddles the 500-character cut
// rather than exceeding it: a row far over the line pins the boundary to a band
// instead of to a value. The marker is a character at the end of the kept
// region, so a cut that keeps the wrong end returns the right length and still
// fails.
func TestALongOutputIsTruncatedInTheFinishEntry(t *testing.T) {
	atTheCut := make([]byte, 500)
	for i := range atTheCut {
		atTheCut[i] = 'a'
	}
	atTheCut[499] = 'Z'

	logger, rec := loggerWithRecorder()
	logger.LogJobFinish(8, "job", "* * * * *", 15, runstatus.Success, string(atTheCut), time.Second)
	if got := rec.only(t).Metadata["output_snippet"]; got != string(atTheCut) {
		t.Fatalf("output of exactly 500 chars was altered; want it kept verbatim")
	}

	overTheCut := string(atTheCut) + "b"
	logger, rec = loggerWithRecorder()
	logger.LogJobFinish(8, "job", "* * * * *", 16, runstatus.Success, overTheCut, time.Second)
	got, _ := rec.only(t).Metadata["output_snippet"].(string)
	if got != string(atTheCut)+"... (truncated)" {
		t.Fatalf("output of 501 chars truncated to %q, want the first 500 followed by the marker", got)
	}
}

// TestALoggerWithNoBusPublishesNothing pins that New(nil) does not panic. The
// client field is an interface, and a typed nil in an interface is not a nil
// interface — the guard in publish only means something because New keeps the
// field untyped.
func TestALoggerWithNoBusPublishesNothing(t *testing.T) {
	logger := New(nil)
	logger.LogJobFinish(9, "job", "* * * * *", 17, runstatus.Error, "boom", time.Second)
}
