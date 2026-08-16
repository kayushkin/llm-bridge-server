package aiauth

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The fixture is 16 bytes: "sk" + U+20AC (3 bytes) + "middle" + U+1F600
// (4 bytes) + "z". Byte 4 — where the 4-byte prefix cut falls — is a
// continuation byte of U+20AC, and byte len-4 is a continuation byte of
// U+1F600, so the unrepaired MaskKey splits a rune at BOTH ends. A fixture
// whose rune widths happened to divide the budgets would land both cuts on a
// boundary and pass against the very code it was written to condemn, so the
// test guards that it still straddles.
func TestMaskKeyNeverSplitsARuneAtEitherCut(t *testing.T) {
	const key = "sk€middle\U0001F600z"

	if len(key) <= 12 {
		t.Fatalf("fixture is only %d bytes, so MaskKey returns all-stars and the cut is never reached", len(key))
	}
	if utf8.RuneStart(key[4]) {
		t.Fatalf("fixture does not straddle the PREFIX cut: byte 4 (%#x) starts a rune, so this test would pass against the byte cut", key[4])
	}
	if utf8.RuneStart(key[len(key)-4]) {
		t.Fatalf("fixture does not straddle the SUFFIX cut: byte len-4 (%#x) starts a rune, so this test would pass against the byte cut", key[len(key)-4])
	}

	got := MaskKey(key)

	if !utf8.ValidString(got) {
		t.Errorf("MaskKey(%q) = %q, which is not valid UTF-8", key, got)
	}
	// The byte cut would return "sk\xe2\x82...\x9f\x98\x80z". Dropping the two
	// partial runes leaves both halves short of their budgets, which is the
	// intended trade: a shorter hint over an invalid one.
	if want := "sk...z"; got != want {
		t.Errorf("MaskKey(%q) = %q, want %q", key, got, want)
	}
}

// TestMaskKey in aiauth_test.go is the ASCII known-negative control: it pins
// MaskKey("sk-ant-api03-abcdefghijklmnop") == "sk-a...mnop", which is the case
// that actually runs in production and which this repair must not move. It
// predates this file; do not duplicate it here.

func TestMaskKeySlidesAFourByteRuneAcrossThePrefixCut(t *testing.T) {
	straddled := 0
	for lead := 0; lead < 8; lead++ {
		key := strings.Repeat("a", lead) + "\U0001F600" + strings.Repeat("b", 16)
		got := MaskKey(key)

		if !utf8.ValidString(got) {
			t.Errorf("lead=%d: MaskKey(%q) = %q, which is not valid UTF-8", lead, key, got)
		}
		if !utf8.RuneStart(key[4]) {
			straddled++
		}
	}
	// Leads 1, 2 and 3 put a continuation byte at index 4.
	if straddled != 3 {
		t.Fatalf("the slide straddled the prefix cut %d times, want 3 — the fixture no longer exercises the defect", straddled)
	}
}

func TestMaskKeyStarsOutAShortKeyWithoutCutting(t *testing.T) {
	for _, key := range []string{"", "short", "€€€€"} {
		if got := MaskKey(key); got != strings.Repeat("*", len(key)) {
			t.Errorf("MaskKey(%q) = %q, want %d stars", key, got, len(key))
		}
	}
}

func TestSuffixAtRuneBoundaryDropsThePartialRuneRatherThanOverrunTheBudget(t *testing.T) {
	cases := []struct {
		in       string
		maxBytes int
		want     string
	}{
		{"abc\U0001F600", 4, "\U0001F600"}, // aligned: the whole rune fits exactly
		{"abc\U0001F600", 3, ""},           // straddles: no whole rune fits in 3 bytes
		{"ab\U0001F600cd", 4, "cd"},        // walks forward past 3 continuation bytes
		{"abcdef", 0, ""},
		{"abcdef", -1, ""},
		{"ab", 9, "ab"}, // budget longer than the string
	}
	for _, c := range cases {
		if got := suffixAtRuneBoundary(c.in, c.maxBytes); got != c.want {
			t.Errorf("suffixAtRuneBoundary(%q, %d) = %q, want %q", c.in, c.maxBytes, got, c.want)
		}
	}
}

func TestTruncateAtRuneBoundaryDropsThePartialRune(t *testing.T) {
	cases := []struct {
		in       string
		maxBytes int
		want     string
	}{
		{"\U0001F600abc", 4, "\U0001F600"},
		{"\U0001F600abc", 3, ""},
		{"ab\U0001F600", 4, "ab"},
		{"abcdef", 0, ""},
		{"abcdef", -1, ""},
		{"ab", 9, "ab"},
	}
	for _, c := range cases {
		if got := truncateAtRuneBoundary(c.in, c.maxBytes); got != c.want {
			t.Errorf("truncateAtRuneBoundary(%q, %d) = %q, want %q", c.in, c.maxBytes, got, c.want)
		}
	}
}
