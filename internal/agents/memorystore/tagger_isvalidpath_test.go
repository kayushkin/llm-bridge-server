package memorystore

import "testing"

// isValidPath is the only thing standing between the file-path regexes and the
// tag set, and it has two ways to say no: a path shorter than three characters,
// and a path ending in a domain suffix. Every existing tagger test asserts only
// that a wanted path WAS tagged, so both refusals were unpinned — measured, not
// assumed: replacing the whole function with `return true` left the full suite
// green. These tests assert the refusals themselves.
//
// Each case names which of the two producers it reaches, because "the tagger
// rejected it" is the assertion that cannot tell them apart, and telling them
// apart is the point.

// A bare domain in prose is not a file the session was working on. The regex
// that finds `main.go` cannot help matching `example.com` — the suffix list is
// what separates them, and nothing was asking it to.
func TestADomainIsNotTaggedAsAFile(t *testing.T) {
	tagger := NewPatternTagger()

	for _, tt := range []struct {
		text    string
		refused string
	}{
		{"Visit example.com today", "example.com"},
		{"Try foo.org for that", "foo.org"},
		{"Mirrored at example.net overnight", "example.net"},
		{"Read the site at kayushkin.io please", "kayushkin.io"},
	} {
		if tagged := tagger.Tag(tt.text, "user"); containsTag(tagged, tt.refused) {
			t.Errorf("Tag(%q) = %v, which tags the domain %q as a file", tt.text, tagged, tt.refused)
		}
	}
}

// The known-negative control, and it is reached rather than merely stated: a
// hostname-shaped token whose suffix is NOT in the list runs the whole loop and
// comes out valid. Without it, a mutation that made isValidPath refuse every
// dotted token would pass the test above — the suite would be asserting "reject
// things" instead of "reject these things".
func TestADottedNameOutsideTheSuffixListIsStillTagged(t *testing.T) {
	tagger := NewPatternTagger()

	for _, tt := range []struct {
		text   string
		wanted string
	}{
		{"See docs.rs for more", "docs.rs"},
		{"and go.dev for more", "go.dev"},
	} {
		if tagged := tagger.Tag(tt.text, "user"); !containsTag(tagged, tt.wanted) {
			t.Errorf("Tag(%q) = %v, want it to contain %q — the suffix list is a list, not a ban on dots",
				tt.text, tagged, tt.wanted)
		}
	}
}

// The length refusal, straddled from both sides one character apart. A test that
// only supplied `/a` could not tell `len < 3` from `len < 4`, or from `len < 40`;
// the pair is what pins the number.
func TestThePathLengthFloorIsStraddled(t *testing.T) {
	tagger := NewPatternTagger()

	// Two characters: refused. `/a` yields the base name `a`, so that is the
	// tag its acceptance would produce.
	if tagged := tagger.Tag("The path /a is short", "user"); containsTag(tagged, "a") {
		t.Errorf("Tag(...) = %v, which tags the two-character path /a", tagged)
	}

	// Three characters: accepted. One character longer, opposite verdict.
	if tagged := tagger.Tag("Look at /ab now", "user"); !containsTag(tagged, "ab") {
		t.Errorf("Tag(...) = %v, want it to contain \"ab\" — /ab is three characters and valid", tagged)
	}
}

func containsTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}
