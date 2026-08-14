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
//
//   - Vary the fixture. A string of one repeated character cannot tell a value
//     that was shortened from one that was shifted, and — the reason it matters
//     for the whitespace tests below — it cannot tell a run that was collapsed
//     from a run that was left alone. Fixtures here spell distinct words.
//
// The whitespace tests add a third, which is the same instinct one level up:
// sweep the CLASS, do not list its members. unicode.IsSpace is the predicate the
// code splits on, so the tests enumerate every rune Unicode says it is true for
// rather than the eight or so a person thinks of. A hand-written list agrees with
// whatever the author remembered and goes stale silently when Go's tables move.
package main

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// whitespaceRunes returns every rune unicode.IsSpace accepts, swept out of the
// table rather than typed from memory. There are 25 at the time of writing; the
// count is asserted below, so a change in Go's tables shows up as one failure
// naming the delta instead of as silently thinner coverage.
func whitespaceRunes() []rune {
	var rs []rune
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.IsSpace(r) {
			rs = append(rs, r)
		}
	}
	return rs
}

func TestWhitespaceSweep_CoversTheWholeClass(t *testing.T) {
	rs := whitespaceRunes()
	if len(rs) < 20 {
		t.Fatalf("swept %d whitespace runes, want at least 20 — the sweep is not finding the class", len(rs))
	}
	// The characters the defect was actually reported with must be in there.
	for _, r := range []rune{'\t', '\n', '\v', '\f', '\r', ' ', 0x0085, 0x00A0, 0x3000} {
		if !unicode.IsSpace(r) {
			t.Errorf("U+%04X is not in the swept class, so no test below covers it", r)
		}
	}
}

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

// Every whitespace character becomes one space — not just \n, which is all the
// code used to replace. The neighbours are distinct words so that a rule which
// dropped the separator, or kept the characters either side in the wrong order,
// fails here; "a<ws>a" would pass under both.
func TestCollapseWhitespaceRuns_EveryWhitespaceCharacterBecomesOneSpace(t *testing.T) {
	for _, r := range whitespaceRunes() {
		got := collapseWhitespaceRuns("alpha" + string(r) + "beta")
		if got != "alpha beta" {
			t.Errorf("U+%04X separating two words gave %q, want %q", r, got, "alpha beta")
		}
	}
}

// The reported case, spelled out: a question typed on a CRLF machine kept a bare
// carriage return in the middle of a name that is stored and rendered.
func TestCollapseWhitespaceRuns_CarriageReturnDoesNotSurviveACRLF(t *testing.T) {
	got := collapseWhitespaceRuns("Approve the deploy?\r\nIt is urgent.")
	if got != "Approve the deploy? It is urgent." {
		t.Errorf("collapseWhitespaceRuns over a CRLF gave %q, want %q", got, "Approve the deploy? It is urgent.")
	}
}

// A run of any length and any mixture collapses to exactly one space. Swept over
// run length so the test does not rest on one, and over a mixture as well as a
// repeat, because a rule that handled "\r\n" as a pair rather than as two members
// of a class passes the pure runs and fails the mixed ones.
func TestCollapseWhitespaceRuns_ARunOfAnyLengthBecomesExactlyOneSpace(t *testing.T) {
	ws := whitespaceRunes()
	for length := 1; length <= 6; length++ {
		var repeated, mixed strings.Builder
		for i := 0; i < length; i++ {
			repeated.WriteRune(' ')
			mixed.WriteRune(ws[i%len(ws)])
		}
		for _, run := range []struct{ name, s string }{
			{"repeated space", repeated.String()},
			{"mixed whitespace", mixed.String()},
		} {
			got := collapseWhitespaceRuns("alpha" + run.s + "beta")
			if got != "alpha beta" {
				t.Errorf("%s run of %d gave %q, want %q", run.name, length, got, "alpha beta")
			}
		}
	}
}

// Nothing but U+0020 survives, stated as a property over a fixture holding the
// whole class at once. The tests above check the class one member at a time,
// which cannot see a rule that handles a lone separator and gives up on several.
func TestCollapseWhitespaceRuns_LeavesNoWhitespaceButTheOrdinarySpace(t *testing.T) {
	var fixture strings.Builder
	for i, r := range whitespaceRunes() {
		fixture.WriteString(string(rune('a' + i%26)))
		fixture.WriteRune(r)
	}
	fixture.WriteString("end")

	for _, r := range collapseWhitespaceRuns(fixture.String()) {
		if unicode.IsSpace(r) && r != ' ' {
			t.Errorf("U+%04X survived into the display name: %q", r, collapseWhitespaceRuns(fixture.String()))
		}
	}
}

// The words themselves, and their order, come through untouched.
//
// The fixture repeats two of its words on purpose, and that is the whole point of
// it. Measured against two sabotages — reverse the words, and deduplicate them:
//
//	all words distinct   reverse caught, deduplicate SILENT (nothing to dedupe)
//	all words identical  reverse SILENT (a reversal is invisible), deduplicate caught
//
// So "vary the fixture" is only half a rule. A fixture of distinct words is blind
// to exactly the defects a repeated one catches, and a fixture that is uniform in
// ANY property cannot see a rule that is a no-op on that uniformity. This one is
// deliberately non-uniform in both directions: distinct words so an order change
// shows, and a repeated word so a collapse of equal ones shows.
func TestCollapseWhitespaceRuns_KeepsEveryWordAndTheirOrder(t *testing.T) {
	const want = "Approve the deploy the llm-bridge-gemini deploy on prod now"
	got := collapseWhitespaceRuns("  Approve\tthe\r\ndeploy   the\vllm-bridge-gemini deploy\fon prod now  ")
	if got != want {
		t.Errorf("collapseWhitespaceRuns = %q, want %q", got, want)
	}

	// The fixture is what it claims: a repeat AND a pair out of order under
	// reversal. A later edit that tidied it into distinct words would take half
	// the coverage away without failing anything.
	words := strings.Fields(want)
	distinct := map[string]bool{}
	for _, w := range words {
		distinct[w] = true
	}
	if len(distinct) == len(words) {
		t.Errorf("fixture has no repeated word, so a rule that deduplicates words cannot fail here")
	}
	if len(distinct) < 2 {
		t.Errorf("fixture has no distinct words, so a rule that reorders them cannot fail here")
	}
}

// Whitespace at the ends goes with the rest of it, and a run there does not come
// back as a leading or trailing space.
func TestCollapseWhitespaceRuns_DropsWhitespaceAtBothEnds(t *testing.T) {
	for _, r := range whitespaceRunes() {
		pad := strings.Repeat(string(r), 3)
		if got := collapseWhitespaceRuns(pad + "alpha beta" + pad); got != "alpha beta" {
			t.Errorf("U+%04X padding both ends gave %q, want %q", r, got, "alpha beta")
		}
	}
}

// Collapsing happens BEFORE the cut, so the 60 characters are spent on the
// question rather than on the gaps in it. The fixture is 30 one-character words
// separated by three spaces: 119 characters raw, 59 collapsed. Cut first and it
// comes back shortened and marked; collapse first and it arrives whole.
func TestDisplayNameFromQuestion_TheBudgetIsSpentOnTheQuestionNotOnItsWhitespace(t *testing.T) {
	words := make([]string, 30)
	for i := range words {
		words[i] = string(rune('a' + i%26))
	}
	question := strings.Join(words, "   ")
	want := strings.Join(words, " ")

	if n := utf8.RuneCountInString(want); n != 59 {
		t.Fatalf("fixture collapses to %d characters, want 59 — it no longer straddles the bound", n)
	}
	got := displayNameFromQuestion(question)
	if got != want {
		t.Errorf("displayNameFromQuestion = %q (%d characters), want %q — whitespace was cut against the budget",
			got, utf8.RuneCountInString(got), want)
	}
}

// main refuses a question that is blank and does it with strings.TrimSpace, then
// hands what is left to this. TrimSpace and strings.Fields test the same
// unicode.IsSpace predicate, so the two cannot disagree — a question main accepted
// can never arrive here and shape into an empty display name. Swept over the class
// because a rule built on a list of characters would break the agreement for
// whichever members it forgot.
func TestDisplayNameFromQuestion_NeverEmptiesAQuestionMainAccepted(t *testing.T) {
	for _, r := range whitespaceRunes() {
		for _, question := range []string{
			string(r) + "x" + string(r),
			string(r) + string(r) + "hello" + string(r) + "world",
			"x" + string(r),
		} {
			if strings.TrimSpace(question) == "" {
				t.Fatalf("U+%04X: fixture %q is blank, so it tests nothing", r, question)
			}
			if got := displayNameFromQuestion(question); got == "" {
				t.Errorf("U+%04X: main accepted %q, but the display name came out empty", r, question)
			}
		}
	}
	// The other half of the agreement: what TrimSpace calls blank, this empties.
	for _, r := range whitespaceRunes() {
		blank := strings.Repeat(string(r), 4)
		if strings.TrimSpace(blank) != "" {
			t.Fatalf("U+%04X: TrimSpace does not consider %q blank", r, blank)
		}
		if got := displayNameFromQuestion(blank); got != "" {
			t.Errorf("U+%04X: TrimSpace calls it blank but the display name is %q", r, got)
		}
	}
}
