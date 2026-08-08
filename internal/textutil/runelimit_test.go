package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A four-byte rune. Two-byte runes are not good enough here: with a two-byte
// rune only one of the two possible cut offsets falls inside it, so a test
// that happens to pick the other one is green against a plain byte cut.
const fourByteRune = "\U0001D11E" // 𝄞 MUSICAL SYMBOL G CLEF

// TestTruncateToRuneLimitSlidesAFourByteRuneAcrossTheCut is the test that
// actually pins the defect. A single hand-picked string lands off the boundary
// and passes against a byte cut, which is how this survived across the fleet.
//
// With maxRunes=8 a byte cut lands on byte 8. The four-byte rune straddles
// that offset when the ASCII padding before it is 5, 6 or 7 bytes long; every
// other padding is a boundary-aligned known-negative control, which must pass
// against the unfixed code too. Without those controls we could not tell
// "detects the straddle" from "detects non-ASCII input".
func TestTruncateToRuneLimitSlidesAFourByteRuneAcrossTheCut(t *testing.T) {
	const maxRunes = 8

	straddlingPaddings := map[int]bool{5: true, 6: true, 7: true}
	sawStraddle, sawControl := 0, 0

	for padding := 0; padding <= 8; padding++ {
		text := strings.Repeat("a", padding) + fourByteRune + strings.Repeat("b", 10)

		if straddlingPaddings[padding] {
			sawStraddle++
		} else {
			sawControl++
		}

		got := TruncateToRuneLimit(text, maxRunes)

		if !utf8.ValidString(got) {
			t.Errorf("padding=%d: result is not valid UTF-8: %q", padding, got)
		}
		// Validity alone is not a falsifiable assertion — a helper that
		// returned "" for everything would satisfy it, and would satisfy a
		// within-budget check too. Pinning the exact expected prefix is what
		// gives this test teeth against a "trims to nothing" mutation.
		want := string([]rune(text)[:maxRunes])
		if got != want {
			t.Errorf("padding=%d: got %q (%d bytes), want %q (%d bytes)",
				padding, got, len(got), want, len(want))
		}
		if n := utf8.RuneCountInString(got); n != maxRunes {
			t.Errorf("padding=%d: got %d runes, want %d", padding, n, maxRunes)
		}
	}

	// Guard the instrument itself. If the loop above ever stops producing both
	// straddling and aligned inputs, it goes green having measured nothing.
	if sawStraddle != 3 {
		t.Fatalf("expected 3 straddling offsets, exercised %d — the test no longer reaches the defect", sawStraddle)
	}
	if sawControl != 6 {
		t.Fatalf("expected 6 boundary-aligned controls, exercised %d", sawControl)
	}
}

func TestTruncateToRuneLimitLeavesShortTextAlone(t *testing.T) {
	for _, text := range []string{"", "a", "abc", fourByteRune, "héllo " + fourByteRune} {
		if got := TruncateToRuneLimit(text, 80); got != text {
			t.Errorf("TruncateToRuneLimit(%q, 80) = %q, want it unchanged", text, got)
		}
	}
}

func TestTruncateToRuneLimitCutsExactlyAtTheLimit(t *testing.T) {
	// A string of exactly maxRunes runes must come back whole, and one rune
	// longer must lose exactly that one rune — not a byte more.
	exact := strings.Repeat(fourByteRune, 5)
	if got := TruncateToRuneLimit(exact, 5); got != exact {
		t.Errorf("exact-length input was altered: got %q, want %q", got, exact)
	}
	oneOver := strings.Repeat(fourByteRune, 6)
	if got := TruncateToRuneLimit(oneOver, 5); got != exact {
		t.Errorf("one-rune-over input: got %q (%d bytes), want %q (%d bytes)",
			got, len(got), exact, len(exact))
	}
}

func TestTruncateToRuneLimitOnNonPositiveLimit(t *testing.T) {
	for _, limit := range []int{0, -1, -100} {
		if got := TruncateToRuneLimit("abc"+fourByteRune, limit); got != "" {
			t.Errorf("TruncateToRuneLimit(_, %d) = %q, want empty", limit, got)
		}
	}
}
