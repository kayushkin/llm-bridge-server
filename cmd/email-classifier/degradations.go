package main

import (
	"fmt"
	"log"
	"strings"
)

// ==================== partial failure is not success ====================
//
// A tick can fall short of its job without failing at it. An unreachable mail
// account is skipped so the other accounts still get triaged; a model call that
// errors leaves its emails for the next tick. Both are the right local choice
// and both used to be invisible: the command logged a line and exited 0, so the
// scheduler recorded `success`.
//
// That cost two outages. imap-pretender timed out for roughly two days and
// filed nothing, and the Anthropic key ran out of credit for eight consecutive
// runs — 100 of 100 runs green throughout both, because "some accounts worked"
// and "no mail arrived" produce an identical run record.
//
// So degradations are collected here and turned into a non-zero exit at the end
// of the run. The work still happens; only the verdict changes. A run that
// silently dropped a quarter of the mail is not a success.

// degradations is the list of ways one run fell short. The zero value is usable
// and means the run did its whole job.
type degradations struct {
	reasons []string
}

// record notes one shortfall. It does not log: the call sites already log the
// specific failure with the context to act on, and repeating it here would
// double every line.
func (d *degradations) record(format string, args ...any) {
	d.reasons = append(d.reasons, fmt.Sprintf(format, args...))
}

// any reports whether the run fell short at all.
func (d *degradations) any() bool {
	return len(d.reasons) > 0
}

// report writes the summary that explains a non-zero exit. Scheduler run history
// keeps combined output, so this is what a person reads when the run shows
// `error` — it must name every shortfall, not just count them.
func (d *degradations) report() {
	log.Printf("[email] run degraded: %d shortfall(s) — %s",
		len(d.reasons), strings.Join(d.reasons, "; "))
}

// exitCode turns the run's shortfalls into a process exit status. The scheduler
// records a non-zero exit as `error`, which is the whole point: the failure
// becomes visible in run history instead of living only in the log body.
func (d *degradations) exitCode() int {
	if !d.any() {
		return 0
	}
	d.report()
	return 1
}
