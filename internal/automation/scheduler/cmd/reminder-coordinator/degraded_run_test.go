package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/model"
)

// The calendar read is the ONE failure this command survives rather than
// reports. Everything else it can get wrong — a noteboard read, the digest
// write, the calendar mirror, the herald relay — returns an error that reaches
// log.Fatalf and exits 1. That asymmetry is deliberate and right: the reminders
// matter more than the schedule annotation, so a Google hiccup must not leave
// the user with no digest at all.
//
// What was wrong is what happened next. The run carried on and exited 0, so
// scheduler run history recorded `success`. Measured on the live job before
// these tests existed: 12 of the last 100 ticks of job 32 logged
// "calendar read failed" — auth-store held no google credential for two hours
// on 2026-08-28 — and every one of the 100 was `success`. The outage was
// legible only to somebody already reading the log body of a run they had no
// reason to open.
func TestACalendarReadFailureIsRecordedAsAShortfall(t *testing.T) {
	loc := pacific(t)
	digestNote := &model.Item{
		ID: "digest", Type: "note", Title: digestTitle, Tags: []string{"personal", digestTag},
	}
	nb := newFakeNoteboard(digestNote)
	coordinator, _, calendar := newTestCoordinator(t, time.Date(2026, 7, 20, 8, 0, 0, 0, loc), nb)
	calendar.listErr = fmt.Errorf("auth-store error (404): no credentials for provider google")

	if err := coordinator.Run(); err != nil {
		t.Fatalf("a calendar read failure must not take the run down: %v", err)
	}

	if !coordinator.Degraded.HasShortfall() {
		t.Fatal("the calendar read failed and the run recorded no shortfall — " +
			"scheduler run history would show this tick as `success`")
	}
	if got := coordinator.Degraded.ExitCode(); got != 1 {
		t.Errorf("exit code %d, want 1: the scheduler records a zero exit as `success`", got)
	}
	// Assert what the row REPORTS, not only that a row exists. A shortfall the
	// reader cannot act on costs one more session than no shortfall at all.
	joined := strings.Join(coordinator.Degraded.Reasons(), "; ")
	if !strings.Contains(joined, "calendar") {
		t.Errorf("the shortfall does not say what fell short:\n%s", joined)
	}
	if !strings.Contains(joined, "no credentials for provider google") {
		t.Errorf("the shortfall drops the underlying cause, so the reader has to go find the log line anyway:\n%s", joined)
	}
}

// The other half of the same claim, and the half that decides whether the exit
// code is worth anything. A job that goes red on every tick carries as little as
// the green it replaced — the card that asked for this says so in as many words:
// "a deliberate skip is not a shortfall".
//
// This is also the distinction the card names for THIS binary: a tick that
// nudged nobody because it had nothing to say must stay green. Nothing is due,
// nothing is late, no nag is firing and the digest hour has not arrived, so the
// run correctly sends no herald session at all.
func TestATickWithNothingToSayIsNotAShortfall(t *testing.T) {
	loc := pacific(t)
	digestNote := &model.Item{
		ID: "digest", Type: "note", Title: digestTitle, Tags: []string{"personal", digestTag},
	}
	nb := newFakeNoteboard(digestNote)
	// 06:00 is before the 07:30 digest hour, so nudgeOnce returns early.
	coordinator, herald, _ := newTestCoordinator(t, time.Date(2026, 7, 20, 6, 0, 0, 0, loc), nb)

	if err := coordinator.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(herald.sessions) != 0 {
		t.Fatalf("this case is only meaningful if nothing was sent; got %d session(s)", len(herald.sessions))
	}
	if coordinator.Degraded.HasShortfall() {
		t.Errorf("a tick with nothing to say was recorded as falling short: %v", coordinator.Degraded.Reasons())
	}
	if got := coordinator.Degraded.ExitCode(); got != 0 {
		t.Errorf("exit code %d, want 0 — a job red every tick says no more than a job green every tick", got)
	}
}

// The second deliberate skip in this command, and the one that reads most like a
// failure in the log: "rolling series exhausted, leaving done". A rolling item
// whose rule has run out is finished, not dropped. Recording it would put job 32
// permanently red for as long as one exhausted series sat on the board.
func TestAnExhaustedRollingSeriesIsNotAShortfall(t *testing.T) {
	loc := pacific(t)
	anchor := time.Date(2026, 7, 1, 9, 0, 0, 0, loc)
	digestNote := &model.Item{
		ID: "digest", Type: "note", Title: digestTitle, Tags: []string{"personal", digestTag},
	}
	exhausted := &model.Item{
		ID: "rolling", Type: "todo", Title: "Renew the parking permit", Status: "done",
		Tags:  []string{"personal"},
		DueAt: ptr(anchor),
		Schedule: &model.Schedule{
			DTStart: &anchor,
			TZID:    "America/Los_Angeles",
			RRule:   "FREQ=WEEKLY;COUNT=1",
			Mode:    model.ScheduleModeRolling,
		},
	}
	nb := newFakeNoteboard(exhausted, digestNote)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 20, 6, 0, 0, 0, loc), nb)

	if err := coordinator.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if exhausted.Status != "done" {
		t.Fatalf("this case is only meaningful if the series really was exhausted; status is %q", exhausted.Status)
	}
	if coordinator.Degraded.HasShortfall() {
		t.Errorf("an exhausted rolling series was recorded as a shortfall: %v", coordinator.Degraded.Reasons())
	}
}

// A clean tick that DID do its whole job — digest written, nudge sent — must
// also be green. Without this, the two tests above are both satisfied by a
// recorder that records nothing at all, and the calendar case would be the only
// thing keeping the wiring honest.
func TestAFullTickThatDidItsWholeJobIsGreen(t *testing.T) {
	loc := pacific(t)
	digestNote := &model.Item{
		ID: "digest", Type: "note", Title: digestTitle, Tags: []string{"personal", digestTag},
	}
	overdue := &model.Item{
		ID: "todo", Type: "todo", Title: "Call the plumber", Status: "open",
		Tags: []string{"personal"}, DueAt: ptr(time.Date(2026, 7, 18, 9, 0, 0, 0, loc)),
	}
	nb := newFakeNoteboard(overdue, digestNote)
	coordinator, herald, _ := newTestCoordinator(t, time.Date(2026, 7, 20, 8, 0, 0, 0, loc), nb)

	if err := coordinator.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(herald.sessions) != 1 {
		t.Fatalf("this case is only meaningful if the nudge went out; got %d session(s)", len(herald.sessions))
	}
	if coordinator.Degraded.HasShortfall() {
		t.Errorf("a tick that did its whole job was recorded as falling short: %v", coordinator.Degraded.Reasons())
	}
	if got := coordinator.Degraded.ExitCode(); got != 0 {
		t.Errorf("exit code %d, want 0", got)
	}
}
