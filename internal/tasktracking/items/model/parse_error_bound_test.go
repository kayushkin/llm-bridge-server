package model

import (
	"strings"
	"testing"
)

// ParseISO8601Duration is exported, so the length of the error it returns is
// part of its contract and not an internal detail of Validate.
//
// This case exists because a sabotage that unbounded the site inside it came
// back MISSED against the API suite. Validate is today the only caller and it
// bounds the error again on the way out, so the outer bound answers for the
// inner one and every request-level ceiling stays green while this function
// hands back 10 KB. The next caller does not inherit that outer bound.
//
// The general form, which cost the noteboard sweep one arm: when one cause can
// be observed at several sites, an assertion at the outermost is satisfied by
// the outermost alone. Assert the consequence each site is uniquely for.
func TestParseISO8601DurationBoundsItsOwnErrorForCallersWithNoOuterBound(t *testing.T) {
	// A digit run long enough to overflow Atoi. strconv quotes its whole input
	// back inside the range error, so an unbounded site renders it twice: once
	// as the value and once inside the wrapped error.
	lead := "-P" + strings.Repeat("9", 5000) + "D"

	_, err := ParseISO8601Duration(lead)
	if err == nil {
		t.Fatal("expected an out-of-range duration to be refused")
	}
	if n := len(err.Error()); n > 500 {
		t.Errorf("a %d-byte duration produced a %d-byte error; a caller that renders it "+
			"without a bound of its own hands the whole value back", len(lead), n)
	}
	if err.Error() == "" {
		t.Error("the error is empty, so a caller is told the duration is bad and not why")
	}
}

// The discriminating negative for the same site: an ordinary bad duration has
// to keep saying what was wrong with it.
func TestParseISO8601DurationStillSaysWhyForAnOrdinaryBadValue(t *testing.T) {
	_, err := ParseISO8601Duration("-P1Q")
	if err == nil {
		t.Fatal("expected an unsupported unit to be refused")
	}
	if !strings.Contains(err.Error(), "unsupported unit") {
		t.Errorf("error %q dropped the diagnosis", err.Error())
	}
	if !strings.Contains(err.Error(), "Q") {
		t.Errorf("error %q dropped the offending unit, so a caller cannot tell which one", err.Error())
	}
}
