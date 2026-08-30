package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// Two sites in this command parse a remote RFC3339 string and log the parser's
// error beside the value. Neither value is bounded by anything in this process:
// `updated_at` comes from llm-bridge-server and a noteboard timestamp comes from
// noteboard, and time.Parse quotes its whole input back inside its own message,
// twice.
//
// These are ceiling assertions on the rendered log line, because that is the
// only thing that tells you the bound holds. Both halves have to be bounded —
// bounding the value alone leaves the line just as long.
const (
	oversizedTimestampLength = 5000
	// One WARNING line's ceiling. Wide enough for the ids, the field name, a
	// truncated value and a real parser message; far below a 5 KB echo.
	logLineCeiling = 400
)

func oversizedTimestamp() string { return strings.Repeat("x", oversizedTimestampLength) }

// captureLog returns everything written to the standard logger while fn runs.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	fn()
	return buf.String()
}

// lineContaining pulls out the single log line the claim is about. Measuring the
// whole capture would let a neighbouring line answer for this one, and asserting
// a ceiling over the whole capture would grow with unrelated logging.
func lineContaining(t *testing.T, capture, needle string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(capture, "\n"), "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("no logged line contains %q; capture was %.300q", needle, capture)
	return ""
}

func TestAnUnreadableNoteboardTimestampIsBoundedInTheWarning(t *testing.T) {
	var got *time.Time
	capture := captureLog(t, func() {
		got = timeField(map[string]any{"due_at": oversizedTimestamp()}, "due_at", "card-1")
	})
	if got != nil {
		t.Fatalf("an unparseable due_at was read as a time: %v", got)
	}

	line := lineContaining(t, capture, "card-1")
	if len(line) > logLineCeiling {
		t.Errorf("a %d-byte due_at produced a %d-byte log line (ceiling %d)",
			oversizedTimestampLength, len(line), logLineCeiling)
	}
	if !strings.Contains(line, "due_at") {
		t.Errorf("the bounded line %q lost the field name", line)
	}
}

func TestAnUnreadableSessionTimestampIsBoundedInTheWarning(t *testing.T) {
	gate := &sessionGate{now: func() time.Time { return time.Unix(0, 0) }}

	var owns bool
	var reason string
	capture := captureLog(t, func() {
		owns, reason = gate.dormantStillOwns("br_long", "idle", oversizedTimestamp())
	})
	if !owns {
		t.Fatalf("an unreadable updated_at released the card (reason %q); the safe side is to keep it", reason)
	}

	line := lineContaining(t, capture, "br_long")
	if len(line) > logLineCeiling {
		t.Errorf("a %d-byte updated_at produced a %d-byte log line (ceiling %d)",
			oversizedTimestampLength, len(line), logLineCeiling)
	}
	if !strings.Contains(line, "idle") {
		t.Errorf("the bounded line %q lost the session state", line)
	}
}

// TestAnOrdinaryBadTimestampStillSaysWhyInTheWarning is the discriminating
// negative for both sites above. A repair that drops the parser's error, or
// replaces it with a fixed string, passes every ceiling and leaves the reader
// unable to tell a bad month from a bad format.
func TestAnOrdinaryBadTimestampStillSaysWhyInTheWarning(t *testing.T) {
	capture := captureLog(t, func() {
		timeField(map[string]any{"due_at": "2026-13-01T00:00:00Z"}, "due_at", "card-2")
	})
	line := lineContaining(t, capture, "card-2")
	if !strings.Contains(line, "month out of range") {
		t.Errorf("the line %q dropped the parser's diagnosis", line)
	}
}
