package memorystore

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A four-byte rune, so a cut can land one, two or three bytes into it. Three
// bytes of slack is what makes the straddle slide: with a two-byte rune only one
// of the two offsets is wrong, and a test that happens to pick the other one
// passes against unfixed code.
const fourByteRune = "\U0001D11E" // MUSICAL SYMBOL G CLEF

// TestTruncateAtRuneBoundary slides runes of every multi-byte width across the
// cut. The helper is shared by two callers already and will attract more, so it
// is pinned on its own rather than only through them.
//
// The length assertions are what stop this passing against a helper that always
// returns "": trimming to nothing is valid UTF-8 and within budget, so validity
// alone is not falsifiable. Losing fewer than runeLen bytes is the property that
// says the walk-back stopped at the first boundary rather than somewhere earlier.
func TestTruncateAtRuneBoundary(t *testing.T) {
	for _, r := range []string{"é", "€", fourByteRune} { // 2, 3 and 4 bytes
		runeLen := len(r)
		s := strings.Repeat(r, 50)
		for maxBytes := 1; maxBytes <= len(s); maxBytes++ {
			got := truncateAtRuneBoundary(s, maxBytes)

			if !utf8.ValidString(got) {
				t.Fatalf("rune %q maxBytes=%d: result is not valid UTF-8", r, maxBytes)
			}
			if len(got) > maxBytes {
				t.Fatalf("rune %q maxBytes=%d: result is %d bytes, over budget", r, maxBytes, len(got))
			}
			if maxBytes-len(got) >= runeLen {
				t.Fatalf("rune %q maxBytes=%d: trimmed %d bytes, more than the %d-byte rune "+
					"straddling the cut — the walk-back went too far",
					r, maxBytes, maxBytes-len(got), runeLen)
			}
			if !strings.HasPrefix(s, got) {
				t.Fatalf("rune %q maxBytes=%d: result is not a prefix of the input", r, maxBytes)
			}
		}
	}
}

func TestTruncateAtRuneBoundaryEdgeCases(t *testing.T) {
	if got := truncateAtRuneBoundary("hello", 0); got != "" {
		t.Errorf("maxBytes=0: got %q, want empty", got)
	}
	if got := truncateAtRuneBoundary("hello", -1); got != "" {
		t.Errorf("maxBytes=-1: got %q, want empty", got)
	}
	if got := truncateAtRuneBoundary("hello", 5); got != "hello" {
		t.Errorf("maxBytes equal to length: got %q, want the input unchanged", got)
	}
	if got := truncateAtRuneBoundary("hello", 99); got != "hello" {
		t.Errorf("maxBytes past the end: got %q, want the input unchanged", got)
	}
	// Input that is already invalid UTF-8: nothing but continuation bytes, so the
	// walk-back finds no boundary. It must terminate and give back an empty
	// string rather than run off the front of the slice.
	if got := truncateAtRuneBoundary("\x80\x80\x80", 2); got != "" {
		t.Errorf("all-continuation-byte input: got % x, want empty", []byte(got))
	}
}
