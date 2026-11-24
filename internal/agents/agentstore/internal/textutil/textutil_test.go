package textutil

import (
	"testing"
	"unicode/utf8"
)

// TestUpperFirstRuneKeepsTheFirstRuneWhole is the reason this package exists.
//
// The cases are chosen so that each rune WIDTH is covered separately: the old
// spelling, strings.ToUpper(s[:1]) + s[1:], takes exactly one byte whatever the
// leading rune's width, so a suite testing only two-byte runes would leave the
// three- and four-byte paths unpinned while looking complete.
//
// The ASCII case is a known-negative control. The old code handles it correctly,
// so it must stay green when the fix is reverted. A suite whose every case goes
// red under sabotage is not distinguishing the fix from the fixture.
func TestUpperFirstRuneKeepsTheFirstRuneWhole(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		want      string
		leadWidth int // bytes in the leading rune; 1 == the control
	}{
		{"ascii_control", "claxon", "Claxon", 1},
		{"two_byte_lead", "émile", "Émile", 2},
		{"three_byte_lead", "日本語", "日本語", 3}, // no upper-case form; must pass through intact
		{"four_byte_lead", "🎯quest", "🎯quest", 4},
		{"two_byte_lead_with_tail", "ñandú-agent", "Ñandú-agent", 2},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := utf8.RuneLen([]rune(c.in)[0]); got != c.leadWidth {
				t.Fatalf("fixture is wrong: %q leads with a %d-byte rune, case declares %d",
					c.in, got, c.leadWidth)
			}

			got := UpperFirstRune(c.in)

			if got != c.want {
				t.Errorf("UpperFirstRune(%q) = %q, want %q", c.in, got, c.want)
			}
			// Checked separately from the equality above: a wrong-but-valid
			// result and a corrupt result are different defects, and the byte
			// cut produces the second one.
			if !utf8.ValidString(got) {
				t.Errorf("UpperFirstRune(%q) = % x, which is not valid UTF-8", c.in, got)
			}
		})
	}
}

// TestUpperFirstRuneOnEmptyStringDoesNotPanic pins the defect the card that
// commissioned this fix did not mention. The old spelling indexes ""[:1] and
// panics with "slice bounds out of range"; both call sites build a display name
// from an ID read out of a JSON config file, so an empty ID is reachable by
// hand-editing that file rather than by anything exotic.
func TestUpperFirstRuneOnEmptyStringDoesNotPanic(t *testing.T) {
	if got := UpperFirstRune(""); got != "" {
		t.Errorf("UpperFirstRune(%q) = %q, want %q", "", got, "")
	}
}

// TestUpperFirstRuneLeavesAnAlreadyInvalidLeadingByteAlone pins a deliberate
// choice rather than a bug fix. Given input that is ALREADY not valid UTF-8,
// upper-casing the stray byte would substitute a three-byte U+FFFD and change
// the string's length. Passing it through unchanged keeps the function from
// inventing data, and keeps its output length predictable for a caller that
// budgets bytes.
func TestUpperFirstRuneLeavesAnAlreadyInvalidLeadingByteAlone(t *testing.T) {
	in := "\xa9mile" // the tail of "émile" with its lead byte already lost
	if got := UpperFirstRune(in); got != in {
		t.Errorf("UpperFirstRune(% x) = % x, want it returned unchanged", in, got)
	}
}
