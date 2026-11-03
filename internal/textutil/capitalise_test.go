package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestApostrophesDoNotBreakWords is the defect this function was written for.
// Every case here fails against strings.Title, which is what makes the test
// worth having: run it with strings.Title substituted and all of it goes red.
func TestApostrophesDoNotBreakWords(t *testing.T) {
	cases := []struct{ in, want string }{
		{"don't panic", "Don't Panic"},
		{"claxon's quest", "Claxon's Quest"},
		{"don't", "Don't"},
		{"claxon's", "Claxon's"},
		{"rock'n'roll", "Rock'n'roll"},
		{"o'brien", "O'brien"},
	}
	for _, c := range cases {
		got := TitleFirstRuneOfEachWord(c.in)
		if got != c.want {
			t.Errorf("TitleFirstRuneOfEachWord(%q) = %q, want %q", c.in, got, c.want)
		}
		// The case is only meaningful if strings.Title actually differs here.
		// A case both agree on would pass even if this function were replaced
		// by the deprecated one it exists to remove.
		if old := strings.Title(c.in); old == got { //nolint:staticcheck // SA1019: asserting the difference is the point
			t.Errorf("case %q does not distinguish this function from strings.Title (both %q) — it cannot detect a regression", c.in, old)
		}
	}
}

// TestEverythingWithoutAnApostropheMatchesStringsTitle pins the other half of
// the contract. The replacement is only safe to drop into thirteen existing
// call sites if it changes nothing except the defect, so this asserts
// equivalence rather than trusting the doc comment.
func TestEverythingWithoutAnApostropheMatchesStringsTitle(t *testing.T) {
	inputs := []string{
		"", "search", "filesystem", "general", "bronze", "legendary",
		"code-introspection", "code_introspection", "code.introspection",
		"http/api", "HTTP-api", "the ELF guard", "x2go", "a1b2c3", "mp3 player",
		"émile", "héllo world", "ü-ber", "naïve—dash", "it’s", "ǆdict",
		"a  b", "-lead", "brigid", "homepage", " leading space", "trailing ",
	}
	for _, in := range inputs {
		if strings.ContainsRune(in, '\'') {
			t.Fatalf("input %q contains an ASCII apostrophe — it belongs in the other test", in)
		}
		want := strings.Title(in) //nolint:staticcheck // SA1019: strings.Title is the behavior being preserved
		if got := TitleFirstRuneOfEachWord(in); got != want {
			t.Errorf("TitleFirstRuneOfEachWord(%q) = %q, want %q (strings.Title)", in, got, want)
		}
	}
	if len(inputs) < 20 {
		t.Errorf("only %d equivalence inputs; this test is meant to be a broad net", len(inputs))
	}
}

// TestAnUnflankedApostropheStillSeparates pins the boundary of the change. An
// apostrophe only joins a word when a word rune sits on both sides of it, so a
// leading or trailing one must still behave exactly as strings.Title has it.
// Without this the fix would quietly widen into "an apostrophe is never a
// separator", which is a different and larger claim.
func TestAnUnflankedApostropheStillSeparates(t *testing.T) {
	inputs := []string{"'lead", "dogs' bowls", "'", "''", "a ' b", "trailing'"}
	for _, in := range inputs {
		want := strings.Title(in) //nolint:staticcheck // SA1019: strings.Title is the behavior being preserved
		if got := TitleFirstRuneOfEachWord(in); got != want {
			t.Errorf("TitleFirstRuneOfEachWord(%q) = %q, want %q (strings.Title)", in, got, want)
		}
	}
}

// TestInvalidUTF8IsCopiedNotReplaced covers the one place this function
// deliberately does NOT match strings.Title, which maps undecodable bytes to
// U+FFFD and so silently corrupts them.
func TestInvalidUTF8IsCopiedNotReplaced(t *testing.T) {
	inputs := []string{"\xff\xfe", "ab\xffcd", "\xffx", "caf\xe9"}

	differed := 0
	for _, in := range inputs {
		got := TitleFirstRuneOfEachWord(in)
		// strings.ContainsRune is the wrong instrument here and reports a
		// false positive: it decodes, so every undecodable byte in the input
		// reads back as U+FFFD whether or not one was ever written. Search for
		// the encoded bytes instead.
		if strings.Contains(got, string(utf8.RuneError)) {
			t.Errorf("TitleFirstRuneOfEachWord(%q) = %q, which introduced U+FFFD", in, got)
		}
		if len(got) != len(in) {
			t.Errorf("TitleFirstRuneOfEachWord(%q) changed byte length %d -> %d", in, len(in), len(got))
		}
		if old := strings.Title(in); old != got { //nolint:staticcheck // SA1019: documenting the divergence
			differed++
		}
	}
	// Rule: a loop is a claim about a range; assert the claim. If these inputs
	// ever stopped diverging from strings.Title, this test would be asserting
	// nothing and would still pass.
	if differed != len(inputs) {
		t.Errorf("expected all %d invalid inputs to diverge from strings.Title, only %d did", len(inputs), differed)
	}
}

// TestWordStartIsTitleCaseNotUpperCase pins the digraph behavior, which is the
// difference between this function and a ToUpper-based one.
func TestWordStartIsTitleCaseNotUpperCase(t *testing.T) {
	if got, want := TitleFirstRuneOfEachWord("ǆdict"), "ǅdict"; got != want {
		t.Errorf("TitleFirstRuneOfEachWord(%q) = %q, want %q (unicode.ToTitle, not ToUpper)", "ǆdict", got, want)
	}
}
