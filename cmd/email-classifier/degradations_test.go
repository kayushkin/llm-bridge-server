package main

import (
	"strings"
	"testing"
)

// TestCleanRunExitsZero — the common case. Every tick that does its whole job
// must stay green, or the signal is worthless.
func TestCleanRunExitsZero(t *testing.T) {
	var d degradations
	if d.any() {
		t.Error("a fresh recorder reports a shortfall")
	}
	if got := d.exitCode(); got != 0 {
		t.Errorf("clean run exited %d, want 0", got)
	}
}

// TestSkippedAccountExitsNonZero is the whole point of the file: imap-pretender
// timed out for two days while every run recorded `success`, because the command
// logged the skip and exited 0.
func TestSkippedAccountExitsNonZero(t *testing.T) {
	var d degradations
	d.record("account %s unreachable, filed nothing from it", "imap-pretender")
	if !d.any() {
		t.Fatal("a skipped account did not register as a shortfall")
	}
	if got := d.exitCode(); got != 1 {
		t.Errorf("degraded run exited %d, want 1 so the scheduler records `error`", got)
	}
}

// TestReportNamesEveryShortfall — scheduler run history keeps the combined
// output, so the summary is what a person reads. A count alone would say a run
// failed without saying at what.
func TestReportNamesEveryShortfall(t *testing.T) {
	var d degradations
	d.record("account %s unreachable, filed nothing from it", "imap-pretender")
	d.record("model could not classify %s, left unfiled", "gmail-personal:abc123")

	joined := strings.Join(d.reasons, "; ")
	for _, want := range []string{"imap-pretender", "gmail-personal:abc123"} {
		if !strings.Contains(joined, want) {
			t.Errorf("summary %q does not name %q", joined, want)
		}
	}
	if len(d.reasons) != 2 {
		t.Errorf("recorded %d shortfalls, want 2", len(d.reasons))
	}
}

// TestShortfallsAccumulateAcrossSites — one run can fall short in several ways
// at once, and each must survive into the verdict rather than the last winning.
func TestShortfallsAccumulateAcrossSites(t *testing.T) {
	var d degradations
	for _, acct := range []string{"imap-pretender", "gmail-personal", "outlook-work"} {
		d.record("account %s unreachable, filed nothing from it", acct)
	}
	if len(d.reasons) != 3 {
		t.Fatalf("recorded %d shortfalls, want 3", len(d.reasons))
	}
	if d.exitCode() != 1 {
		t.Error("three failed accounts did not produce a failing exit")
	}
}
