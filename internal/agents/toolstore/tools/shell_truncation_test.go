package tools

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// truncateShellOutput used to cut with `s[:headChars]` and `s[len(s)-tailChars:]`,
// both byte offsets, while every constant involved is named for characters and
// the notice between the two halves reports "characters omitted". Two separate
// defects followed.
//
// The head cut is a prefix form and drops the trailing bytes of a split
// character. The tail cut is a SUFFIX form and is worse in kind: it keeps the
// orphan continuation bytes at the START of what it returns, which breaks a
// consumer scanning forward from byte 0 for a character to begin. Measured on
// 60000 Japanese characters, the tail opened with the bytes 97 a5.
//
// The second defect is the one that survives being laundered. Every local tool
// leaves through one writeJSON, and encoding/json rewrites invalid UTF-8 to
// U+FFFD with a nil error, so the split characters are cosmetic by the time a
// reader sees them. The COUNT is not: `len(s) - headChars - tailChars` is a byte
// arithmetic printed as characters, and it announced 135000 omitted when 44998
// were — a 3x overstatement in a sentence written for a model to act on.
//
// The expectations below are written against those contract literals — 25000,
// 20000, and the omitted count they imply — and never against the constants the
// code reads, so editing a constant moves the code without moving the
// expectation. (Asserting `wantCount: headChars` would be a restatement, not an
// assertion.)

const (
	// The budget truncateShellOutput advertises, restated here as literals.
	contractHeadCharacters = 25000
	contractTailCharacters = 20000

	// fourByteCharacter is U+1D11E MUSICAL SYMBOL G CLEF. Four bytes wide, so
	// sliding it across a cut exercises every one of the three ways a byte
	// offset can land inside a character.
	fourByteCharacter = "\U0001D11E"
)

// truncatedParts splits a truncateShellOutput result back into its head, its
// reported omission count, and its tail.
func truncatedParts(t *testing.T, out string) (head string, omitted int, tail string) {
	t.Helper()
	const marker = " characters omitted "
	open := strings.Index(out, "\n\n[... ")
	if open < 0 {
		t.Fatalf("result carries no omission notice; got %d bytes", len(out))
	}
	close := strings.Index(out, marker)
	if close < 0 {
		t.Fatalf("omission notice does not report characters: %q", out[open:min(open+80, len(out))])
	}
	if _, err := fmt.Sscanf(out[open+len("\n\n[... "):close], "%d", &omitted); err != nil {
		t.Fatalf("omission count is not a number: %v", err)
	}
	head = out[:open]
	tail = out[close+len(marker)+len("...]\n\n"):]
	return head, omitted, tail
}

// straddlingInput builds an input whose head cut and tail cut each land inside a
// four-byte character, or not, according to the two padding widths. The body is
// made of four-byte characters; padFront ASCII bytes shift where byte offset
// 25000 falls within one, and padBack shifts byte offset len(s)-20000 the same
// way. A padding of 0 leaves both cuts aligned to a character boundary, which is
// the control.
func straddlingInput(padFront, padBack int) string {
	// Over the 50000-CHARACTER budget, not merely the byte one: a body of 30000
	// four-byte characters is 120000 bytes and looks oversized, but the fixed
	// code counts characters and would return it untouched.
	const bodyCharacters = 60000
	return strings.Repeat("a", padFront) +
		strings.Repeat(fourByteCharacter, bodyCharacters) +
		strings.Repeat("z", padBack)
}

func TestNeitherCutSplitsAFourByteCharacter(t *testing.T) {
	for padFront := 0; padFront < 4; padFront++ {
		for padBack := 0; padBack < 4; padBack++ {
			name := fmt.Sprintf("padFront=%d/padBack=%d", padFront, padBack)
			t.Run(name, func(t *testing.T) {
				in := straddlingInput(padFront, padBack)
				out := truncateShellOutput(in)

				if !utf8.ValidString(out) {
					t.Errorf("result is not valid UTF-8; a cut landed inside a character")
				}
				head, _, tail := truncatedParts(t, out)

				if got := utf8.RuneCountInString(head); got != contractHeadCharacters {
					t.Errorf("head = %d characters, want %d", got, contractHeadCharacters)
				}
				if got := utf8.RuneCountInString(tail); got != contractTailCharacters {
					t.Errorf("tail = %d characters, want %d", got, contractTailCharacters)
				}
				// The suffix cut's own failure mode, asserted directly rather
				// than inferred from validity: a byte cut leaves the tail
				// beginning mid-character.
				if r, _ := utf8.DecodeRuneInString(tail); r == utf8.RuneError {
					t.Errorf("tail begins with an orphan continuation byte: % x", tail[:min(4, len(tail))])
				}
			})
		}
	}
}

// The count is the half that survives encoding/json rewriting the split
// characters, so it is the half a reader actually acts on.
func TestTheOmittedCountIsCharactersAndNotBytes(t *testing.T) {
	// 60000 Japanese characters: 180000 bytes. This is the exact input the
	// defect was measured on, and the byte arithmetic printed 135000 here.
	in := strings.Repeat("日", 60000)

	_, omitted, _ := truncatedParts(t, truncateShellOutput(in))

	const want = 60000 - contractHeadCharacters - contractTailCharacters // 15000
	if omitted != want {
		t.Errorf("notice reports %d characters omitted, want %d", omitted, want)
	}
	if omitted == len(in)-contractHeadCharacters-contractTailCharacters {
		t.Errorf("notice is still reporting a byte count (%d)", omitted)
	}
}

// The reported count has to describe the two halves actually returned, whatever
// the input: head + omitted + tail must reconstruct the original character
// count, or the notice is describing some other truncation than the one shipped.
func TestTheThreePartsAccountForEveryCharacter(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("日", 60000),
		straddlingInput(1, 3),
		straddlingInput(3, 1),
		strings.Repeat("a", 200000),
	} {
		head, omitted, tail := truncatedParts(t, truncateShellOutput(in))
		got := utf8.RuneCountInString(head) + omitted + utf8.RuneCountInString(tail)
		if want := utf8.RuneCountInString(in); got != want {
			t.Errorf("head+omitted+tail = %d characters, input holds %d", got, want)
		}
	}
}

// CONTROL. Byte and character counts coincide for ASCII, so a unit defect
// cannot reach this case and it must stay green when the unit is sabotaged. An
// off-by-one at either cut is unit-independent, so it must NOT stay green — that
// is what makes this control evidence the suite is aimed at the unit axis rather
// than merely hair-trigger.
func TestAnASCIIOutputIsCutAtTheSameBudget(t *testing.T) {
	in := strings.Repeat("a", 200000)

	head, omitted, tail := truncatedParts(t, truncateShellOutput(in))

	if len(head) != contractHeadCharacters {
		t.Errorf("head = %d bytes, want %d", len(head), contractHeadCharacters)
	}
	if len(tail) != contractTailCharacters {
		t.Errorf("tail = %d bytes, want %d", len(tail), contractTailCharacters)
	}
	if want := 200000 - contractHeadCharacters - contractTailCharacters; omitted != want {
		t.Errorf("notice reports %d omitted, want %d", omitted, want)
	}
}

// CONTROL. A budget counted in characters must not truncate output that fits,
// including output whose BYTE length exceeds the budget while its character
// count does not. Under the old code this string was truncated; it should now
// come back whole.
func TestOutputThatFitsInCharactersIsReturnedWhole(t *testing.T) {
	// 40000 Japanese characters = 120000 bytes: over the 50000-byte reading of
	// the budget, under the 50000-character one, and few enough lines that the
	// line branch does not fire.
	in := strings.Repeat("日", 40000)

	if out := truncateShellOutput(in); out != in {
		t.Errorf("output of %d characters was truncated to %d; it fits the budget",
			utf8.RuneCountInString(in), utf8.RuneCountInString(out))
	}
}

// CONTROL for the branch that was already correct. The line branch counts lines
// and reports lines, so its unit already matched; this pins that the character
// fix did not disturb it, and that its count is a true line count.
func TestTheLineBranchStillReportsLinesOmitted(t *testing.T) {
	const lines = 2000
	in := strings.TrimSuffix(strings.Repeat("日本語\n", lines), "\n")

	out := truncateShellOutput(in)
	if !strings.Contains(out, " lines omitted ") {
		t.Fatalf("expected the line branch to fire")
	}
	var omitted int
	open := strings.Index(out, "\n\n[... ")
	if _, err := fmt.Sscanf(out[open+len("\n\n[... "):], "%d", &omitted); err != nil {
		t.Fatalf("line count is not a number: %v", err)
	}
	if want := lines - 250 - 200; omitted != want {
		t.Errorf("notice reports %d lines omitted, want %d", omitted, want)
	}
	if !utf8.ValidString(out) {
		t.Errorf("line branch produced invalid UTF-8")
	}
}

// The helpers are total: every input has an answer, including the degenerate
// ones, so neither carries a guard that can never fire.
func TestTheCharacterHelpersAreTotal(t *testing.T) {
	cases := []struct {
		s        string
		n        int
		wantHead string
		wantTail string
	}{
		{"", 5, "", ""},
		{"", 0, "", ""},
		{"abc", 0, "", ""},
		{"abc", 3, "abc", "abc"},
		{"abc", 99, "abc", "abc"},
		{"abc", -1, "", ""},
		{"日本語", 1, "日", "語"},
		{"日本語", 2, "日本", "本語"},
		{fourByteCharacter + "a", 1, fourByteCharacter, "a"},
	}
	for _, c := range cases {
		if got := headCharacters(c.s, c.n); got != c.wantHead {
			t.Errorf("headCharacters(%q, %d) = %q, want %q", c.s, c.n, got, c.wantHead)
		}
		if got := tailCharacters(c.s, c.n); got != c.wantTail {
			t.Errorf("tailCharacters(%q, %d) = %q, want %q", c.s, c.n, got, c.wantTail)
		}
	}
}
