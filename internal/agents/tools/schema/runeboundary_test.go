package schema

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A four-byte rune is the one that finds this defect. With a two-byte rune only
// one of the two straddling offsets is wrong, so a test that happens to pick
// the other offset stays green against a plain byte cut.
const fourByteRune = "\U0001D11E" // 𝄞 MUSICAL SYMBOL G CLEF

// Sliding the rune across the cut is what pins the defect. A single hand-picked
// string lands off the boundary and passes against unfixed code, which is how
// this survived in twenty repos.
//
// Every lead length is asserted, not just the straddling ones. The leads where
// the rune sits clear of the cut are the known-negative control: they pass
// against a plain byte cut too, so they are what separates "the test detects a
// straddle" from "the test detects non-ASCII input at all".
func TestTruncateAtRuneBoundarySlidesARuneAcrossTheCut(t *testing.T) {
	const maxBytes = 10
	for lead := 0; lead <= 14; lead++ {
		s := strings.Repeat("a", lead) + fourByteRune + strings.Repeat("b", 20)
		got := TruncateAtRuneBoundary(s, maxBytes)

		if !utf8.ValidString(got) {
			t.Errorf("lead=%d: result is not valid UTF-8: %q", lead, got)
		}
		if len(got) > maxBytes {
			t.Errorf("lead=%d: result is %d bytes, over the %d-byte budget", lead, len(got), maxBytes)
		}
		if !strings.HasPrefix(s, got) {
			t.Errorf("lead=%d: result is not a prefix of the input: %q", lead, got)
		}
		// Validity and budget alone are not falsifiable: a helper that returns
		// "" for everything satisfies both. Pin how much was given up as well —
		// a walk back off a four-byte rune can never cost more than three bytes.
		if min := maxBytes - 3; len(got) < min {
			t.Errorf("lead=%d: result is %d bytes, lost more than the straddling rune is wide (want >= %d)",
				lead, len(got), min)
		}

		// The rune occupies bytes [lead, lead+4). It straddles the cut only
		// when it starts before the cut and ends after it.
		straddles := lead < maxBytes && lead+4 > maxBytes
		if !straddles && len(got) != maxBytes {
			t.Errorf("lead=%d: cut is already on a boundary, want the full %d bytes, got %d",
				lead, maxBytes, len(got))
		}
		if straddles && len(got) != lead {
			t.Errorf("lead=%d: want the cut walked back to the rune start at %d, got %d bytes",
				lead, lead, len(got))
		}
	}
}

// The suffix cut walks the other way, so it needs its own slide. Its failure
// looks different too: the partial rune lands at the START of the result.
func TestSuffixAtRuneBoundarySlidesARuneAcrossTheCut(t *testing.T) {
	const maxBytes = 10
	for trail := 0; trail <= 14; trail++ {
		s := strings.Repeat("a", 20) + fourByteRune + strings.Repeat("b", trail)
		got := SuffixAtRuneBoundary(s, maxBytes)

		if !utf8.ValidString(got) {
			t.Errorf("trail=%d: result is not valid UTF-8: %q", trail, got)
		}
		if len(got) > maxBytes {
			t.Errorf("trail=%d: result is %d bytes, over the %d-byte budget", trail, len(got), maxBytes)
		}
		if !strings.HasSuffix(s, got) {
			t.Errorf("trail=%d: result is not a suffix of the input: %q", trail, got)
		}
		if min := maxBytes - 3; len(got) < min {
			t.Errorf("trail=%d: result is %d bytes, lost more than the straddling rune is wide (want >= %d)",
				trail, len(got), min)
		}

		// Counting from the end: the rune occupies the bytes at distance
		// [trail+1, trail+4] from the end, so it straddles a cut that keeps
		// maxBytes when it starts further back than the cut and ends inside it.
		straddles := trail < maxBytes && trail+4 > maxBytes
		if !straddles && len(got) != maxBytes {
			t.Errorf("trail=%d: cut is already on a boundary, want the full %d bytes, got %d",
				trail, maxBytes, len(got))
		}
		if straddles && len(got) != trail {
			t.Errorf("trail=%d: want the cut walked forward past the partial rune to %d bytes, got %d",
				trail, trail, len(got))
		}
	}
}

// Both helpers must leave a string that already fits completely alone, budget
// zero must not panic, and a budget smaller than the first rune must give up
// rather than emit half of it.
func TestRuneBoundaryHelpersHandleTheEdges(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxBytes int
		prefix   string
		suffix   string
	}{
		{"already fits", "abc", 10, "abc", "abc"},
		{"exactly fits", "abc", 3, "abc", "abc"},
		{"zero budget", "abc", 0, "", ""},
		{"negative budget", "abc", -1, "", ""},
		// The give-up guard is `maxBytes <= 0`, and 1 is the adjacent value on
		// the live side of it. Without this row the guard could be widened to
		// swallow a budget of one byte with the whole suite green: every other
		// row here is either at 0 and below, or at 3 and above.
		{"budget of exactly one byte", "abc", 1, "a", "c"},
		{"budget of exactly two bytes", "abc", 2, "ab", "bc"},
		{"empty input", "", 10, "", ""},
		{"budget narrower than the only rune", fourByteRune, 3, "", ""},
		{"budget exactly the only rune", fourByteRune, 4, fourByteRune, fourByteRune},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TruncateAtRuneBoundary(c.in, c.maxBytes); got != c.prefix {
				t.Errorf("TruncateAtRuneBoundary(%q, %d) = %q, want %q", c.in, c.maxBytes, got, c.prefix)
			}
			if got := SuffixAtRuneBoundary(c.in, c.maxBytes); got != c.suffix {
				t.Errorf("SuffixAtRuneBoundary(%q, %d) = %q, want %q", c.in, c.maxBytes, got, c.suffix)
			}
		})
	}
}
