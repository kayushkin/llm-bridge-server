package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A four-byte rune. With a two-byte rune only one of the two byte offsets falls
// inside it, so a test that picks the other one is green against a byte cut.
const fourByteRune = "\U0001D11E" // 𝄞 MUSICAL SYMBOL G CLEF

// TestDisplayNameNeverSplitsARune slides the rune across the cut rather than
// hand-picking one string. The delegate name reaches the dash as JSON, so a
// split rune arrives as a replacement character with nothing reporting a fault.
//
// Paddings that put the rune across byte offset 80 are the ones a byte cut
// corrupts; the others are boundary-aligned known-negative controls and pass
// against the unfixed code too.
func TestDisplayNameNeverSplitsARune(t *testing.T) {
	const maxRunes = 80
	straddles, controls := 0, 0

	for padding := maxRunes - 3; padding <= maxRunes+1; padding++ {
		// No spaces: displayName collapses whitespace through strings.Fields
		// before truncating, which would otherwise move the cut.
		prompt := strings.Repeat("a", padding) + fourByteRune + strings.Repeat("b", 40)
		if padding < maxRunes && maxRunes < padding+4 {
			straddles++
		} else {
			controls++
		}

		got := displayName(prompt)

		if !utf8.ValidString(got) {
			t.Errorf("padding=%d: result is not valid UTF-8: %q", padding, got)
		}
		want := "delegate: " + string([]rune(prompt)[:maxRunes]) + "…"
		if got != want {
			t.Errorf("padding=%d:\n got %q\nwant %q", padding, got, want)
		}
	}

	if straddles != 3 {
		t.Fatalf("exercised %d straddling offsets, want 3 — the test no longer reaches the defect", straddles)
	}
	if controls != 2 {
		t.Fatalf("exercised %d boundary-aligned controls, want 2", controls)
	}
}

func TestDisplayNameLeavesAShortPromptAlone(t *testing.T) {
	if got, want := displayName("do the thing"), "delegate: do the thing"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Short but multi-byte: no ellipsis, nothing lost.
	if got, want := displayName("héllo "+fourByteRune), "delegate: héllo "+fourByteRune; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDisplayNameCollapsesWhitespaceBeforeTruncating(t *testing.T) {
	if got, want := displayName("  a\n\tb   c  "), "delegate: a b c"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
