// Tests for the session display name ask derives from a question.
//
// The unit under test is a shaping step whose output is persisted on a created
// session and read by a human in the dash. Nothing downstream re-derives it, so
// what these tests are protecting is the only copy.
//
// Two rules, both learned from scoring earlier suites rather than reading them:
//
//   - Spell the bound out rather than driving the declaration. 60 is a choice
//     about how much of a question a person reads in a session list, not a value
//     another program computes, so a test that reads displayNameRuneLimit agrees
//     with the constant through any drift of it. One test below asserts the
//     number itself; the rest describe behaviour at that number.
//
//   - Sweep the rune alignment. 60 is divisible by 2, 3 and 4 — every rune width
//     Go has — so a fixture repeating one wide rune from offset zero cuts cleanly
//     even under a byte slice, and comes out green against the defect these tests
//     exist to pin.
package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDisplayNameRuneLimit_IsSixtyCharacters(t *testing.T) {
	if displayNameRuneLimit != 60 {
		t.Errorf("displayNameRuneLimit = %d, want 60 — the length of question a person reads in the dash session list",
			displayNameRuneLimit)
	}
}

func TestDisplayNameFromQuestion_ShortQuestionsArriveWhole(t *testing.T) {
	const question = "Approve updating llm-bridge-gemini?"
	if got := displayNameFromQuestion(question); got != question {
		t.Errorf("displayNameFromQuestion(%q) = %q, want it unchanged", question, got)
	}
}

// The display name is one line in a list, so the question's own line breaks and
// its surrounding whitespace must not travel with it.
func TestDisplayNameFromQuestion_PutsTheQuestionOnOneLine(t *testing.T) {
	if got := displayNameFromQuestion("  Approve this?\nIt is urgent.  "); got != "Approve this? It is urgent." {
		t.Errorf("displayNameFromQuestion = %q, want %q", got, "Approve this? It is urgent.")
	}
}

// The headline contract: 60 means 60 characters for every alphabet. Cut in bytes,
// the same 60-character question survives as 30 in accented Latin, 20 in Japanese
// and 15 in emoji — a name shortened to a third of itself for asking a question in
// the wrong script.
func TestDisplayNameFromQuestion_SixtyCharactersOfAnyScriptArriveWhole(t *testing.T) {
	for _, script := range []struct{ name, rune_ string }{
		{"ascii", "a"},
		{"latin1-accent", "é"},
		{"japanese", "日"},
		{"emoji", "🟢"},
	} {
		question := strings.Repeat(script.rune_, 60)
		got := displayNameFromQuestion(question)
		if got != question {
			t.Errorf("%s: a 60-character question came back as %d characters (%d bytes): %q",
				script.name, utf8.RuneCountInString(got), len(got), got)
		}
	}
}

// And a question over the bound is cut to it, on a rune boundary, marked, at every
// alignment. The ellipsis counts against the 60 rather than riding outside it.
func TestDisplayNameFromQuestion_LongerQuestionsAreCutToTheLimitAndMarked(t *testing.T) {
	for _, script := range []struct{ name, rune_ string }{
		{"ascii", "a"},
		{"latin1-accent", "é"},
		{"japanese", "日"},
		{"emoji", "🟢"},
	} {
		for shift := 0; shift < 3; shift++ {
			question := strings.Repeat("a", shift) + strings.Repeat(script.rune_, 200)
			got := displayNameFromQuestion(question)

			if n := utf8.RuneCountInString(got); n != 60 {
				t.Errorf("%s shift=%d: display name is %d characters, want exactly 60", script.name, shift, n)
			}
			if !utf8.ValidString(got) {
				t.Errorf("%s shift=%d: display name is not valid UTF-8: %q", script.name, shift, got)
			}
			if !strings.HasSuffix(got, "…") {
				t.Errorf("%s shift=%d: a shortened display name does not say so: %q", script.name, shift, got)
			}
			if !strings.HasPrefix(question, strings.TrimSuffix(got, "…")) {
				t.Errorf("%s shift=%d: display name is not a prefix of the question: %q", script.name, shift, got)
			}
		}
	}
}
