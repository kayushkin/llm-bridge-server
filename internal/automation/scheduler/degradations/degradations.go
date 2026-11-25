// Package degradations turns the ways one run fell short of its job into a
// process exit status the scheduler can see.
//
// ==================== partial failure is not success ====================
//
// A tick can fall short of its job without failing at it. An unreachable mail
// account is skipped so the other accounts still get triaged; a model call that
// errors leaves its work for the next tick. Both are the right local choice and
// both used to be invisible: the command logged a line and exited 0, so the
// scheduler recorded `success`.
//
// That cost three outages. imap-pretender timed out for roughly two days and
// filed nothing, and the Anthropic key ran out of credit for eight consecutive
// runs — 100 of 100 runs green throughout both, because "some accounts worked"
// and "no mail arrived" produce an identical run record. Then on 2026-08-29 the
// credit balance ran out again: `email-classifier`, which had by then been given
// this recorder, went red on the first tick, while `kanban-classifier` — same
// repo, same cmd/ directory, no recorder — reported success on 40 of 40 runs
// straight through the outage (card `b4130d06-47d5-4187-8bb3-54367f20f67b`).
//
// That third outage is why this lives in internal/ rather than in one command's
// package. It was not a new failure mode and it did not need a new answer; it
// needed the answer that already existed to be reachable from the binary next
// door. Every other command under cmd/ that files work on a cron tick has the
// same shape and can take this as it is.
//
// So degradations are collected here and turned into a non-zero exit at the end
// of the run. The work still happens; only the verdict changes. A run that
// silently dropped a quarter of its input is not a success.
package degradations

import (
	"fmt"
	"log"
	"strings"
)

// Recorder is the list of ways one run fell short, and the subject that names
// the run in the summary line. Construct it with New: a recorder with no
// subject would print a shortfall report that does not say which binary fell
// short, and on a scheduler that runs eleven commands that report is unusable.
type Recorder struct {
	subject string
	reasons []string
}

// New returns a recorder for one run of one command. The subject is the short
// name that prefixes the summary line — "email" for the email classifier,
// "classifier" for the kanban classifier — and matches the prefix that
// command's own progress lines already carry, so a reader of scheduler run
// history sees one voice rather than two.
//
// An empty subject panics rather than being tolerated. It is a constant at
// every call site, so the panic can only fire on the developer who introduced
// it, and the alternative — reporting a shortfall against a nameless run — is
// exactly the kind of quiet uselessness this package exists to remove.
func New(subject string) *Recorder {
	if subject == "" {
		panic("degradations.New: empty subject — the summary line must name the run that fell short")
	}
	return &Recorder{subject: subject}
}

// Record notes one shortfall. It does not log: the call sites already log the
// specific failure with the context to act on, and repeating it here would
// double every line.
func (r *Recorder) Record(format string, args ...any) {
	r.reasons = append(r.reasons, fmt.Sprintf(format, args...))
}

// HasShortfall reports whether the run fell short at all.
func (r *Recorder) HasShortfall() bool {
	return len(r.reasons) > 0
}

// Reasons returns the shortfalls recorded so far, in the order they happened.
// It is what a test asserts against, and what Report renders.
func (r *Recorder) Reasons() []string {
	return r.reasons
}

// Report writes the summary that explains a non-zero exit. Scheduler run history
// keeps combined output, so this is what a person reads when the run shows
// `error` — it must name every shortfall, not just count them.
func (r *Recorder) Report() {
	log.Printf("[%s] run degraded: %d shortfall(s) — %s",
		r.subject, len(r.reasons), strings.Join(r.reasons, "; "))
}

// ExitCode turns the run's shortfalls into a process exit status. The scheduler
// records a non-zero exit as `error`, which is the whole point: the failure
// becomes visible in run history instead of living only in the log body.
func (r *Recorder) ExitCode() int {
	if !r.HasShortfall() {
		return 0
	}
	r.Report()
	return 1
}
