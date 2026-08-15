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

// TestTruncateToRuneLimitOfOneKeepsExactlyOneRune is the other half of the test
// above, and it exists because that one alone cannot say where the guard sits.
//
// `maxRunes <= 0` and `maxRunes <= 1` differ on exactly one input: a limit of 1.
// Every limit the rest of this file passes — 0, -1, -100, 5, 8, 80 — answers the
// same under both, so rewriting the guard to swallow a limit of 1 left the whole
// package green. A limit of 1 is not a hypothetical: it is what a caller asks for
// when a display budget is computed rather than spelled out.
//
// The limits here are literals on purpose. A test that names the boundary through
// the thing it is testing moves with it and can never fail for a wrong value.
func TestTruncateToRuneLimitOfOneKeepsExactlyOneRune(t *testing.T) {
	// Multi-byte first, so a byte cut at offset 1 is distinguishable from a
	// rune cut at 1 as well.
	if got := TruncateToRuneLimit(fourByteRune+"rest", 1); got != fourByteRune {
		t.Errorf("TruncateToRuneLimit(_, 1) = %q, want the single rune %q", got, fourByteRune)
	}
	if got := TruncateToRuneLimit("abc", 1); got != "a" {
		t.Errorf(`TruncateToRuneLimit("abc", 1) = %q, want "a"`, got)
	}
	// The straddle. 0 is the largest limit that yields nothing and 1 is the
	// smallest that yields something, so this pair reddens whichever way the
	// guard's boundary moves.
	if got := TruncateToRuneLimit("abc", 0); got != "" {
		t.Errorf(`TruncateToRuneLimit("abc", 0) = %q, want empty`, got)
	}
}
