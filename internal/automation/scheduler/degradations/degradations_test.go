package degradations

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// TestCleanRunExitsZero — the common case. Every tick that does its whole job
// must stay green, or the signal is worthless.
func TestCleanRunExitsZero(t *testing.T) {
	d := New("email")
	if d.HasShortfall() {
		t.Error("a fresh recorder reports a shortfall")
	}
	if got := d.ExitCode(); got != 0 {
		t.Errorf("clean run exited %d, want 0", got)
	}
}

// TestSkippedAccountExitsNonZero is the whole point of the package: imap-pretender
// timed out for two days while every run recorded `success`, because the command
// logged the skip and exited 0.
func TestSkippedAccountExitsNonZero(t *testing.T) {
	d := New("email")
	d.Record("account %s unreachable, filed nothing from it", "imap-pretender")
	if !d.HasShortfall() {
		t.Fatal("a skipped account did not register as a shortfall")
	}
	if got := d.ExitCode(); got != 1 {
		t.Errorf("degraded run exited %d, want 1 so the scheduler records `error`", got)
	}
}

// TestReportNamesEveryShortfall — scheduler run history keeps the combined
// output, so the summary is what a person reads. A count alone would say a run
// failed without saying at what.
func TestReportNamesEveryShortfall(t *testing.T) {
	d := New("email")
	d.Record("account %s unreachable, filed nothing from it", "imap-pretender")
	d.Record("model could not classify %s, left unfiled", "gmail-personal:abc123")

	joined := strings.Join(d.Reasons(), "; ")
	for _, want := range []string{"imap-pretender", "gmail-personal:abc123"} {
		if !strings.Contains(joined, want) {
			t.Errorf("summary %q does not name %q", joined, want)
		}
	}
	if len(d.Reasons()) != 2 {
		t.Errorf("recorded %d shortfalls, want 2", len(d.Reasons()))
	}
}

// TestShortfallsAccumulateAcrossSites — one run can fall short in several ways
// at once, and each must survive into the verdict rather than the last winning.
func TestShortfallsAccumulateAcrossSites(t *testing.T) {
	d := New("email")
	for _, acct := range []string{"imap-pretender", "gmail-personal", "outlook-work"} {
		d.Record("account %s unreachable, filed nothing from it", acct)
	}
	if len(d.Reasons()) != 3 {
		t.Fatalf("recorded %d shortfalls, want 3", len(d.Reasons()))
	}
	if d.ExitCode() != 1 {
		t.Error("three failed accounts did not produce a failing exit")
	}
}

// TestReportedLineNamesTheSubject is the assertion the move into internal/ made
// necessary and the per-command copy never needed. With one recorder serving
// several binaries, a summary that does not say WHICH run degraded sends a
// reader of scheduler history to the wrong job — and the subject is the only
// thing in the line that can say so.
func TestReportedLineNamesTheSubject(t *testing.T) {
	for _, subject := range []string{"email", "classifier", "curator"} {
		var captured bytes.Buffer
		restore := redirectLog(t, &captured)
		d := New(subject)
		d.Record("model could not classify %s, left unfiled", "sess-1")
		if got := d.ExitCode(); got != 1 {
			t.Fatalf("subject %q: exited %d, want 1", subject, got)
		}
		restore()

		line := captured.String()
		if !strings.Contains(line, "["+subject+"]") {
			t.Errorf("subject %q: summary %q does not carry the subject", subject, strings.TrimSpace(line))
		}
	}
}

// TestReportCountsAndListsEveryReason pins what Report actually prints, not just
// what Reasons holds. The count and the list are two separate renderings of the
// same slice and either could drift alone: a count that disagrees with the list
// is how a reader concludes the log was truncated.
func TestReportCountsAndListsEveryReason(t *testing.T) {
	var captured bytes.Buffer
	restore := redirectLog(t, &captured)
	d := New("classifier")
	d.Record("session %s: %s", "sess-a", "list tags failed")
	d.Record("session %s: %s", "sess-b", "model refused")
	d.Record("session %s: %s", "sess-c", "get messages failed")
	d.Report()
	restore()

	line := captured.String()
	if !strings.Contains(line, "3 shortfall(s)") {
		t.Errorf("summary %q does not report the count 3", strings.TrimSpace(line))
	}
	for _, want := range []string{"sess-a", "sess-b", "sess-c"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary %q does not name %q", strings.TrimSpace(line), want)
		}
	}
}

// TestCleanRunPrintsNothing — ExitCode is called on every path, including the
// ones that did the whole job. A clean run that printed a degradation summary
// would teach readers to ignore the line that matters.
func TestCleanRunPrintsNothing(t *testing.T) {
	var captured bytes.Buffer
	restore := redirectLog(t, &captured)
	d := New("classifier")
	_ = d.ExitCode()
	restore()

	if captured.Len() != 0 {
		t.Errorf("a clean run printed %q", strings.TrimSpace(captured.String()))
	}
}

// TestEmptySubjectPanics — a recorder with no subject can only report a
// shortfall against a nameless run, which is unreadable on a box running eleven
// scheduled commands. The subject is a constant at every call site, so failing
// here is failing in front of the developer who introduced it.
func TestEmptySubjectPanics(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("New(\"\") returned a recorder instead of panicking")
		}
		if !strings.Contains(fmtRecovered(recovered), "subject") {
			t.Errorf("panic %v does not say the subject is what is missing", recovered)
		}
	}()
	New("")
}

func fmtRecovered(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if err, ok := v.(error); ok {
		return err.Error()
	}
	return ""
}

// redirectLog points the standard logger at buf and returns the call that puts
// it back. The flags are cleared too: a timestamp prefix would make every
// assertion below depend on the clock.
func redirectLog(t *testing.T, buf *bytes.Buffer) func() {
	t.Helper()
	priorWriter := log.Writer()
	priorFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		log.SetOutput(priorWriter)
		log.SetFlags(priorFlags)
	}
	t.Cleanup(restore)
	return restore
}
