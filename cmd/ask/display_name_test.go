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

	"github.com/slava/scheduler/internal/textcut"
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
	if len(rs) != 25 {
		t.Fatalf("swept %d whitespace runes, want 25 (delta %+d) — Go's whitespace table changed, or the sweep is not finding the class", len(rs), len(rs)-25)
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

// main refuses a question with no readable text in it and does it with
// hasReadableText, which is the shaping steps themselves — so a question main
// accepted can never arrive here and shape into an empty display name. Swept over
// both classes because a rule built on a list of characters would break the
// agreement for whichever members it forgot.
//
// This used to be an agreement between strings.TrimSpace and strings.Fields, and
// it held only while whitespace was the sole thing removed here. Once the shaping
// also removed the non-text characters the two came apart: TrimSpace tests
// unicode.IsSpace, which is false for every one of them, so "\x00\x00" and a lone
// U+202E are questions main accepts and the shaping empties.
func TestDisplayNameFromQuestion_NeverEmptiesAQuestionMainAccepted(t *testing.T) {
	for _, r := range append(whitespaceRunes(), nonTextRunes()...) {
		for _, question := range []string{
			string(r) + "x" + string(r),
			string(r) + string(r) + "hello" + string(r) + "world",
			"x" + string(r),
		} {
			if !hasReadableText(question) {
				t.Fatalf("U+%04X: fixture %q has no readable text, so it tests nothing", r, question)
			}
			if got := displayNameFromQuestion(question); got == "" {
				t.Errorf("U+%04X: main accepted %q, but the display name came out empty", r, question)
			}
		}
	}
	// The other half of the agreement: what the guard rejects, the shaping empties.
	for _, r := range append(whitespaceRunes(), nonTextRunes()...) {
		blank := strings.Repeat(string(r), 4)
		if hasReadableText(blank) {
			t.Fatalf("U+%04X: the guard does not consider %q blank", r, blank)
		}
		if got := displayNameFromQuestion(blank); got != "" {
			t.Errorf("U+%04X: the guard calls it blank but the display name is %q", r, got)
		}
	}
}

// The swept classes below are spelled out from Unicode's tables HERE, in the
// test, rather than read back out of isNonText.
//
// ⚠️ This is not duplication for its own sake and the first version of this file
// got it wrong. A sweep derived from the function under test narrows in step with
// the defect: with the bidi half of isNonText deleted, every behavioural test
// below still passed, because nonTextRunes() stopped yielding bidi controls and
// the loops simply had nothing to check. TestDisplayNameFromQuestion_ABidiOverride‐
// CannotReorderTheRow — whose entire subject is bidi — passed VACUOUSLY, having
// skipped all of its iterations. Only a hardcoded count noticed, and a count is
// one number that the next person to change the rule legitimately would update.
//
// So: the code answers "what does isNonText do", the test answers "what should be
// removed", and TestNonTextClasses_MatchTheCodesRule compares them. That is the
// one place the two definitions are allowed to meet.
func controlRunes() []rune {
	var rs []rune
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			rs = append(rs, r)
		}
	}
	return rs
}

func bidiControlRunes() []rune {
	var rs []rune
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.Is(unicode.Bidi_Control, r) {
			rs = append(rs, r)
		}
	}
	return rs
}

// nonTextRunes is the union of the two classes, defined independently of the code.
func nonTextRunes() []rune {
	return append(controlRunes(), bidiControlRunes()...)
}

func TestNonTextClasses_MatchTheCodesRule(t *testing.T) {
	want := map[rune]bool{}
	for _, r := range nonTextRunes() {
		want[r] = true
	}
	if len(want) != 71 {
		t.Errorf("the test's own classes sweep to %d runes, want 71 (59 non-whitespace controls + 12 bidi controls)", len(want))
	}
	// Every rune in the universe, both directions — this is what makes the two
	// definitions agree rather than merely overlap on the members someone listed.
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if isNonText(r) != want[r] {
			t.Errorf("U+%04X: isNonText = %v, the test's classes say %v", r, isNonText(r), want[r])
		}
	}
	// The characters the defect was reported with must be in there.
	for _, r := range []rune{0x0000, 0x0007, 0x0008, 0x001B, 0x007F, 0x009B, 0x202E, 0x2066} {
		if !want[r] {
			t.Errorf("U+%04X is not in the swept class, so no test below covers it", r)
		}
	}
	// The two classes are disjoint from whitespace, which a different rule handles.
	for r := range want {
		if unicode.IsSpace(r) {
			t.Errorf("U+%04X is in both classes; whitespace is collapseWhitespaceRuns's job", r)
		}
	}
	// The sweeps are not empty, so a loop over either cannot pass vacuously.
	if len(controlRunes()) != 59 || len(bidiControlRunes()) != 12 {
		t.Errorf("class sizes are %d controls and %d bidi controls, want 59 and 12",
			len(controlRunes()), len(bidiControlRunes()))
	}
}

// The over-reach guard, and the reason this rule is not "all of category Cf".
//
// Cf is 170 runes and contains the two join controls, which are not decoration:
// Arabic, Persian and the Indic scripts need them to shape, and an emoji family is
// one glyph only because ZWJ holds it together. A rule that took all of Cf would
// pass every test above and quietly corrupt legitimate text.
func TestNonTextSweep_SparesTheJoinersAndOrdinaryText(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.Is(unicode.Join_Control, r) && isNonText(r) {
			t.Errorf("U+%04X is a join control and must survive — scripts need it to render", r)
		}
	}
	for _, r := range []rune{'a', 'Z', '0', ' ', 'é', '日', '🟢', 0x200C, 0x200D} {
		if isNonText(r) {
			t.Errorf("U+%04X is ordinary text and must survive", r)
		}
	}
	// Spelled out end to end: a ZWJ family arrives as the family, not as parts.
	const family = "Approve \U0001F468‍\U0001F469‍\U0001F467 now"
	if got := displayNameFromQuestion(family); got != family {
		t.Errorf("displayNameFromQuestion(%q) = %q — a ZWJ emoji sequence was broken up", family, got)
	}
}

// No control or bidi character reaches a display name. The neighbours are distinct
// words so that a rule which dropped the separator, or kept the characters either
// side in the wrong order, fails here; "a<R>a" would pass under both.
func TestDisplayNameFromQuestion_NoControlOrBidiCharacterSurvives(t *testing.T) {
	for _, r := range nonTextRunes() {
		got := displayNameFromQuestion("alpha" + string(r) + "beta")
		if got != "alpha beta" {
			t.Errorf("U+%04X separating two words gave %q, want %q", r, got, "alpha beta")
		}
		if strings.ContainsRune(got, r) {
			t.Errorf("U+%04X survived into the display name: %q", r, got)
		}
	}
}

// The decision this change makes, pinned so a later tidy-up cannot reverse it
// silently: a removed character becomes a space, it is not dropped.
//
// Dropping is the one operation the rest of the shaping never performs — it would
// invent a token that appeared nowhere in the input ("alpha\x00beta" -> "alphabeta")
// and a reader cannot see where the seam was. This test fails under a drop and
// passes under a replace, which is the whole reason it is written as a word count
// rather than as an absence of the character.
func TestDisplayNameFromQuestion_NeverWeldsTwoWordsIntoOne(t *testing.T) {
	for _, r := range nonTextRunes() {
		got := displayNameFromQuestion("alpha" + string(r) + "beta")
		if n := len(strings.Fields(got)); n != 2 {
			t.Errorf("U+%04X between two words gave %q — %d word(s), want 2; a dropped character welds them",
				r, got, n)
		}
	}
}

// Removal happens before the collapse, so a character next to existing whitespace
// disappears into that run rather than widening it. Swept over where the whitespace
// sits, because a rule that ran after the collapse would leave a double space on
// one side only.
func TestDisplayNameFromQuestion_RemovalComposesWithTheCollapse(t *testing.T) {
	for _, r := range nonTextRunes() {
		for _, f := range []struct{ name, question string }{
			{"space before", "alpha " + string(r) + "beta"},
			{"space after", "alpha" + string(r) + " beta"},
			{"space both sides", "alpha " + string(r) + " beta"},
			{"run of them", "alpha" + strings.Repeat(string(r), 4) + "beta"},
			{"at both ends", string(r) + "alpha beta" + string(r)},
		} {
			if got := displayNameFromQuestion(f.question); got != "alpha beta" {
				t.Errorf("U+%04X %s: got %q, want %q", r, f.name, got, "alpha beta")
			}
		}
	}
}

// The bidi case spelled out, because it is the one whose damage is to the
// characters AROUND it rather than to itself: an override reverses the rendered
// order of the rest of the row, so a display name reads as something nobody sent.
func TestDisplayNameFromQuestion_ABidiOverrideCannotReorderTheRow(t *testing.T) {
	rs := bidiControlRunes()
	// The loop must actually run. This test filtered a sweep taken from the code
	// itself once, and when the code stopped calling bidi controls non-text the
	// filter emptied and the test passed having checked nothing.
	if len(rs) == 0 {
		t.Fatal("no bidi controls swept — this test would pass without checking anything")
	}
	for _, r := range rs {
		got := displayNameFromQuestion("Approve " + string(r) + "deploy to prod")
		if got != "Approve deploy to prod" {
			t.Errorf("U+%04X: got %q, want %q", r, got, "Approve deploy to prod")
		}
	}
}

// ---------------------------------------------------------------- explicit titles
//
// Everything above pins the name derived from a question. These pin the other half:
// the name a caller states with --title, which until now was stored exactly as
// typed and so had none of the properties above.
//
// The sweeps are the same ones — whitespaceRunes and nonTextRunes, spelled out of
// Unicode's tables rather than read back out of the code — for the reason given at
// TestNonTextClasses_MatchTheCodesRule: a sweep taken from the function under test
// narrows in step with the defect and passes vacuously.

// A title gets the same well-formedness as a derived name: nothing that is not text
// survives it. Neighbours are distinct words so a rule that dropped the separator,
// or reordered what sits either side, fails here.
func TestDisplayNameFromTitle_NoControlOrBidiCharacterSurvives(t *testing.T) {
	rs := nonTextRunes()
	if len(rs) == 0 {
		t.Fatal("no non-text runes swept — this test would pass without checking anything")
	}
	for _, r := range rs {
		got := displayNameFromTitle("alpha" + string(r) + "beta")
		if got != "alpha beta" {
			t.Errorf("U+%04X separating two words in a title gave %q, want %q", r, got, "alpha beta")
		}
		if strings.ContainsRune(got, r) {
			t.Errorf("U+%04X survived into a display name given as a title: %q", r, got)
		}
	}
}

// And it is replaced, not dropped — the same decision the derived path makes, pinned
// separately so a later edit cannot give the two paths different answers.
func TestDisplayNameFromTitle_NeverWeldsTwoWordsIntoOne(t *testing.T) {
	for _, r := range nonTextRunes() {
		got := displayNameFromTitle("alpha" + string(r) + "beta")
		if n := len(strings.Fields(got)); n != 2 {
			t.Errorf("U+%04X between two words in a title gave %q — %d word(s), want 2; a dropped character welds them",
				r, got, n)
		}
	}
}

// A title is one row of a list, so its line breaks and padding do not travel either.
func TestDisplayNameFromTitle_IsPutOnOneLine(t *testing.T) {
	for _, r := range whitespaceRunes() {
		got := displayNameFromTitle("  alpha" + string(r) + "beta  ")
		if got != "alpha beta" {
			t.Errorf("U+%04X inside a title gave %q, want %q", r, got, "alpha beta")
		}
	}
}

// THE DECISION, pinned: a title is not cut at the derived name's bound.
//
// A caller passing --title stated the name they want and display_name is the only
// copy of it, so shortening it here would lose the remainder for good. The derived
// name is cut because deriving one has to choose how much of the question to take;
// a title is an excerpt of nothing.
//
// Swept over script and over length. The scripts matter because a byte-based cut
// bites at different character counts in each, and the lengths straddle the bound
// from both sides — a fixture only above it cannot tell "not cut" from "cut at a
// number larger than 60".
func TestDisplayNameFromTitle_IsNotCutAtTheDerivedNamesBound(t *testing.T) {
	for _, script := range []struct{ name, rune_ string }{
		{"ascii", "a"},
		{"latin1-accent", "é"},
		{"japanese", "日"},
		{"emoji", "🟢"},
	} {
		for _, length := range []int{1, 59, 60, 61, 120, 300} {
			title := strings.Repeat(script.rune_, length)
			got := displayNameFromTitle(title)

			if got != title {
				t.Errorf("%s: a %d-character title came back as %d characters (%q) — an explicit title was shortened",
					script.name, length, utf8.RuneCountInString(got), got)
			}
			if strings.HasSuffix(got, "…") {
				t.Errorf("%s: a %d-character title was marked as shortened: %q", script.name, length, got)
			}
		}
	}
}

// The same input through both paths, to state the difference as one property: the
// two agree except that the derived one spends the reading budget. This is what
// fails if someone adds a shaping step to one path and forgets the other.
//
// It cannot stand alone — both paths broken identically would satisfy it — which is
// why every property above is also pinned directly.
func TestDisplayNameFromTitle_DiffersFromTheDerivedNameOnlyByTheCut(t *testing.T) {
	for _, input := range []string{
		"Approve the deploy?",
		"  Approve\tthe\r\ndeploy   the\vllm-bridge-gemini deploy\fon prod now  ",
		"Approve \x1b[31mprod\x1b[0m \u202ehctiws now please, this one runs past the sixty character bound",
		strings.Repeat("alpha beta ", 30),
		"日本語で書かれた質問をここに置いて、六十文字の境界をきちんと越えるようにする",
	} {
		title := displayNameFromTitle(input)
		want := textcut.TruncateToRuneLimit(title, displayNameRuneLimit)
		if got := displayNameFromQuestion(input); got != want {
			t.Errorf("for %q:\n  title path then cut = %q\n  question path       = %q\nthe two paths disagree about something other than the cut", input, want, got)
		}
	}
}

// The fixture in the test above must actually exercise the cut, or it would be
// asserting that two uncut values match and calling that a contract.
func TestDisplayNameFromTitle_TheComparisonFixtureStraddlesTheBound(t *testing.T) {
	var over, under int
	for _, input := range []string{
		"Approve the deploy?",
		"  Approve\tthe\r\ndeploy   the\vllm-bridge-gemini deploy\fon prod now  ",
		"Approve \x1b[31mprod\x1b[0m \u202ehctiws now please, this one runs past the sixty character bound",
		strings.Repeat("alpha beta ", 30),
		"日本語で書かれた質問をここに置いて、六十文字の境界をきちんと越えるようにする",
	} {
		if utf8.RuneCountInString(displayNameFromTitle(input)) > displayNameRuneLimit {
			over++
		} else {
			under++
		}
	}
	if over == 0 {
		t.Error("no fixture exceeds the bound, so the comparison never exercises the cut")
	}
	if under == 0 {
		t.Error("every fixture exceeds the bound, so the comparison never checks an uncut value")
	}
}

// The words of a title, and their order, come through untouched.
//
// The fixture is non-uniform in both directions on purpose — distinct words so a
// rule that reorders them fails, and a repeated word so a rule that deduplicates
// them fails. Either uniformity alone is blind to exactly what the other catches.
func TestDisplayNameFromTitle_KeepsEveryWordAndTheirOrder(t *testing.T) {
	const want = "Approve the deploy the llm-bridge-gemini deploy on prod now please"
	got := displayNameFromTitle("  Approve\tthe\r\ndeploy   the\vllm-bridge-gemini deploy\fon prod now please  ")
	if got != want {
		t.Errorf("displayNameFromTitle = %q, want %q", got, want)
	}

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
	// And it must be longer than the bound, so this also witnesses the no-cut rule
	// on a fixture whose content is varied rather than repeated.
	if utf8.RuneCountInString(want) <= displayNameRuneLimit {
		t.Errorf("fixture is %d characters, want more than %d so that a cut would show here too",
			utf8.RuneCountInString(want), displayNameRuneLimit)
	}
}

// ---------------------------------------------------------------- which name wins
//
// resolveDisplayName is where the defect lived: the choice used to be made on the
// RAW title, so supplying one skipped the shaping entirely.

func TestResolveDisplayName_AStatedTitleWins(t *testing.T) {
	got, rejected := resolveDisplayName("Personal todos — Aug 14", "Approve the deploy?")
	if got != "Personal todos — Aug 14" {
		t.Errorf("resolveDisplayName = %q, want the title", got)
	}
	if rejected {
		t.Error("a perfectly good title was reported as holding no readable text")
	}
}

func TestResolveDisplayName_NoTitleDerivesFromTheQuestion(t *testing.T) {
	got, rejected := resolveDisplayName("", "Approve the deploy?")
	if got != "Approve the deploy?" {
		t.Errorf("resolveDisplayName = %q, want the name derived from the question", got)
	}
	if rejected {
		t.Error("no title at all was reported as a title that held no readable text")
	}
}

// The title that wins is the SHAPED one. This is the card's defect stated as a test:
// a title carrying an ESC or a bidi override used to be chosen and stored raw.
func TestResolveDisplayName_TheChosenTitleIsShaped(t *testing.T) {
	rs := nonTextRunes()
	if len(rs) == 0 {
		t.Fatal("no non-text runes swept — this test would pass without checking anything")
	}
	for _, r := range rs {
		got, rejected := resolveDisplayName("alpha"+string(r)+"beta", "Approve the deploy?")
		if got != "alpha beta" {
			t.Errorf("U+%04X in a title: resolveDisplayName = %q, want %q — the title was taken unshaped", r, got, "alpha beta")
		}
		if rejected {
			t.Errorf("U+%04X: a title with readable text either side was rejected", r)
		}
	}
}

// A title that shapes away to nothing must not become the display name, and the
// caller must be told rather than quietly given the derived one.
//
// Swept over both classes: this is the agreement between the guard and the shaping
// that TestDisplayNameFromQuestion_NeverEmptiesAQuestionMainAccepted pins for the
// question, and it holds for the same reason — both are defined as the shaping.
func TestResolveDisplayName_ATitleOfNoReadableTextFallsBackAndSaysSo(t *testing.T) {
	rs := append(whitespaceRunes(), nonTextRunes()...)
	if len(rs) == 0 {
		t.Fatal("no runes swept — this test would pass without checking anything")
	}
	for _, r := range rs {
		title := strings.Repeat(string(r), 4)
		got, rejected := resolveDisplayName(title, "Approve the deploy?")
		if got == "" {
			t.Errorf("U+%04X: a title of only that character gave an EMPTY display name", r)
		}
		if got != "Approve the deploy?" {
			t.Errorf("U+%04X: resolveDisplayName = %q, want the name derived from the question", r, got)
		}
		if !rejected {
			t.Errorf("U+%04X: a title that shaped away to nothing was substituted silently", r)
		}
	}
}

// The two returns cannot disagree: a name is never empty, and the flag is set
// exactly when a non-empty title held no readable text.
func TestResolveDisplayName_NeverReturnsAnEmptyName(t *testing.T) {
	const question = "Approve the deploy?"
	for _, title := range []string{
		"", " ", "\x1b", "\x00\x00", "\u202e", "\r\n\t", "\u200b", "ok", strings.Repeat("x", 300),
	} {
		got, rejected := resolveDisplayName(title, question)
		if got == "" {
			t.Errorf("title %q gave an empty display name", title)
		}
		if want := title != "" && !hasReadableText(title); rejected != want {
			t.Errorf("title %q: reported rejected=%v, want %v (hasReadableText=%v)",
				title, rejected, want, hasReadableText(title))
		}
	}
}
