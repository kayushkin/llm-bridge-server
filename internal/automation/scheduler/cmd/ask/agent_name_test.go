// Tests for the originating-agent name ask takes from --from.
//
// The unit under test is a GUARD, not a shaping step, and that is the whole
// point of it: the same characters that a display name has repaired out of it
// are, in an agent name, grounds for refusing to run. See validateAgentName's
// comment for why the two differ — briefly, a display name has a second source
// to fall back to and an agent name has none, so repairing one costs nothing
// recoverable and repairing the other invents an attribution.
//
// The fixture rules at the top of display_name_test.go apply here unchanged, and
// two of them do most of the work below:
//
//   - Sweep the CLASS, do not list its members. These tests reuse
//     nonTextRunes() and whitespaceRunes() from that file. Both are derived from
//     Unicode's tables rather than from the code under test, which is what keeps
//     a sweep from narrowing in step with the defect it is meant to catch.
//
//   - Vary the fixture. Every fixture here spells distinct words, so a value
//     that was rewritten can be told from one that was merely re-ordered.
//
// The sweeps also carry a non-vacuity assertion each. A loop over an empty class
// passes having checked nothing, and reads in a scorer exactly like a loop that
// checked everything.
package main

import (
	"strings"
	"testing"
)

// namesRealCallersPass is the accepted corpus, and it is deliberately made of
// values that exist rather than values invented for a test: the flag's own
// default, the one live caller in this repo, and the example in the package doc.
// A guard this strict earns its keep only if it lets the real traffic through,
// and that claim goes stale silently unless a test spells the traffic out.
var namesRealCallersPass = []struct{ name, why string }{
	{"herald", "the --from flag's own default value"},
	{"personal-reminders", "the one live caller: reminder-coordinator's Relay"},
	{"approval-proposer", "the example in this package's doc comment"},
	{"kanban dispatcher", "one ordinary space is text, and a name may contain it"},
}

func TestValidateAgentName_AcceptsTheNamesRealCallersPass(t *testing.T) {
	if len(namesRealCallersPass) == 0 {
		t.Fatal("the accepted corpus is empty, so this test checks nothing")
	}
	// The fixture assertion: at least one accepted name must contain an ordinary
	// space, or the boundary this corpus is meant to witness is not in it.
	spaced := 0
	for _, c := range namesRealCallersPass {
		if strings.Contains(c.name, " ") {
			spaced++
		}
	}
	if spaced == 0 {
		t.Error("no accepted fixture contains a space, so none of them witnesses that a space survives")
	}

	for _, c := range namesRealCallersPass {
		if err := validateAgentName(c.name); err != nil {
			t.Errorf("validateAgentName(%q) = %v, want nil — %s", c.name, err, c.why)
		}
	}
}

// The positive half of the contract, and the one that says what "accepted"
// buys you downstream: a name that got through is a name nothing would have
// changed. Without this, every test in the file is a refusal, and a guard that
// refused everything would pass all of them.
func TestValidateAgentName_AnAcceptedNameIsAlreadyItsOwnShapedForm(t *testing.T) {
	for _, c := range namesRealCallersPass {
		if got := shapeIntoOneLineOfText(c.name); got != c.name {
			t.Errorf("shapeIntoOneLineOfText(%q) = %q — an accepted name must survive shaping unchanged", c.name, got)
		}
	}
}

func TestValidateAgentName_RefusesEveryNonTextRune(t *testing.T) {
	runes := nonTextRunes()
	if len(runes) == 0 {
		t.Fatal("the non-text sweep is empty, so the loop below checks nothing")
	}
	for _, r := range runes {
		// Distinct words either side, so a refusal cannot be confused with a
		// value that merely lost a repeated character.
		name := "approval" + string(r) + "proposer"
		if err := validateAgentName(name); err == nil {
			t.Errorf("validateAgentName(%q) = nil, want a refusal — U+%04X is not text and would reach the Agent tag raw", name, r)
		}
	}
}

// The rule is "one line of nothing but text", and the line half is not the text
// half. A newline is whitespace, so isNonText is false for it and a guard
// written as "contains no control characters" would let it through — which is
// what makes the Agent tag two lines and splits buildPrompt's backtick span.
func TestValidateAgentName_RefusesEveryWhitespaceRuneExceptTheOrdinarySpace(t *testing.T) {
	runes := whitespaceRunes()
	if len(runes) == 0 {
		t.Fatal("the whitespace sweep is empty, so the loop below checks nothing")
	}
	exempt := 0
	for _, r := range runes {
		name := "approval" + string(r) + "proposer"
		err := validateAgentName(name)
		if r == ' ' {
			exempt++
			if err != nil {
				t.Errorf("validateAgentName(%q) = %v, want nil — an ordinary space is text", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("validateAgentName(%q) = nil, want a refusal — U+%04X is whitespace that is not an ordinary space, so the name is not one line as typed", name, r)
		}
	}
	// Exactly one rune in the class may be exempt. If Go's tables ever grow a
	// second space-like rune this fails naming the delta, rather than quietly
	// widening what the guard lets through.
	if exempt != 1 {
		t.Errorf("%d whitespace runes were treated as exempt, want exactly 1 (U+0020)", exempt)
	}
}

func TestValidateAgentName_RefusesARunOfSpacesAndSpaceAtEitherEnd(t *testing.T) {
	for _, name := range []string{
		"approval  proposer", // collapsed to one space, so not the name as typed
		" approval-proposer",
		"approval-proposer ",
		"  approval-proposer  ",
	} {
		if err := validateAgentName(name); err == nil {
			t.Errorf("validateAgentName(%q) = nil, want a refusal — the stored tag would not be the string the caller passed", name)
		}
	}
}

// The card's second measured line, and the sharper half of it: --from $'\x1b\x1b'
// was ACCEPTED with exit 0, because the guard tested strings.TrimSpace and
// unicode.IsSpace is false for all 71 of these runes.
func TestValidateAgentName_RefusesANameHoldingNoReadableTextAtAll(t *testing.T) {
	for _, name := range []string{
		"",
		"\x1b\x1b",         // the card's measurement, verbatim
		"   ",              // what the old TrimSpace guard did catch
		"‮‭",               // bidi overrides alone
		"\x00\t\x07\n\x7f", // controls and whitespace mixed, no text
	} {
		err := validateAgentName(name)
		if err == nil {
			t.Fatalf("validateAgentName(%q) = nil, want a refusal — there is no agent name in there", name)
		}
		// It must be refused as UNREADABLE, not as merely needing a rewrite: the
		// two are different mistakes and the caller fixes them differently. The
		// rewrite branch would offer "" as the name to pass instead, which is
		// not advice.
		if !strings.Contains(err.Error(), "holds no readable text") {
			t.Errorf("validateAgentName(%q) refused with %q, want the no-readable-text reason", name, err)
		}
	}
}

func TestValidateAgentName_RefusalNamesTheFlagAndTheRewriteItDeclined(t *testing.T) {
	const name = "herald\x1b[31m"
	err := validateAgentName(name)
	if err == nil {
		t.Fatalf("validateAgentName(%q) = nil, want a refusal", name)
	}
	if !strings.Contains(err.Error(), "--from") {
		t.Errorf("error %q does not name the --from flag, so the caller is not told which value to fix", err)
	}
	// Naming the shaped form is the payoff of computing the repair and then
	// declining to apply it: the message tells the caller what to pass.
	if !strings.Contains(err.Error(), `"herald [31m"`) {
		t.Errorf("error %q does not name the rewrite it declined, which is the value the caller should pass instead", err)
	}
}

// The 141st pass's lesson, pinned rather than restated: printing a refused value
// raw writes its ESC to the terminal reading the error. Every message here goes
// through %q, and this is what notices if one is switched back to %s.
//
// There are TWO refusal messages, and which one a name gets is decided by
// whether it holds any readable text. A fixture that always spells words either
// side of the rune only ever reaches the rewrite message, so it pins one %q and
// leaves the other free — the scorer caught exactly that, this test having been
// written that way first. So the sweep runs each rune through both branches and
// counts that it reached both, rather than trusting that it did.
func TestValidateAgentName_NeitherRefusalWritesRawNonTextToTheTerminal(t *testing.T) {
	runes := nonTextRunes()
	if len(runes) == 0 {
		t.Fatal("the non-text sweep is empty, so the loop below checks nothing")
	}
	var reachedRewrite, reachedUnreadable int
	for _, r := range runes {
		// Distinct words either side: the name holds readable text, so it is the
		// rewrite branch that refuses it.
		// Nothing but the rune: no readable text at all, so it is the other one.
		for _, c := range []struct {
			name   string
			branch *int
			want   string
		}{
			{"approval" + string(r) + "proposer", &reachedRewrite, "is not one line of text"},
			{string(r) + string(r), &reachedUnreadable, "holds no readable text"},
		} {
			err := validateAgentName(c.name)
			if err == nil {
				t.Errorf("validateAgentName(%q) = nil, want a refusal (U+%04X)", c.name, r)
				continue
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("U+%04X: refusal %q is not the %q branch, so this case is not testing the message it names", r, err, c.want)
				continue
			}
			*c.branch++
			if strings.ContainsRune(err.Error(), r) {
				t.Errorf("the %q refusal for U+%04X contains that rune raw: %q — it must be quoted, or reading the error executes it", c.want, r, err.Error())
			}
		}
	}
	// Both messages must actually have been produced. Without this the test
	// passes when one branch is never entered, which is the state it was written
	// in and the state the scorer scored SILENT.
	if reachedRewrite == 0 || reachedUnreadable == 0 {
		t.Errorf("reached the rewrite refusal %d times and the unreadable refusal %d times, want both non-zero — a message this test never provokes is a message it does not pin",
			reachedRewrite, reachedUnreadable)
	}
}

// Why a line break in an agent name is a defect and not an untidiness: the name
// is interpolated into the relayed prompt inside a backtick span, on the first
// line. This asserts the harm the guard prevents, so the guard's strictness is
// pinned by a consequence rather than by taste.
func TestBuildPrompt_AttributionStaysInsideOneBacktickSpanOnOneLine(t *testing.T) {
	for _, c := range namesRealCallersPass {
		prompt := buildPrompt(c.name, "Approve this?", "")
		first := strings.SplitN(prompt, "\n", 2)[0]
		span := "`" + c.name + "`"
		if !strings.Contains(first, span) {
			t.Errorf("buildPrompt(%q, …) first line = %q, want it to hold the closed span %q", c.name, first, span)
		}
	}

	// And the counter-case, which is the whole reason for the line rule: a name
	// carrying a newline is refused, so this state is unreachable through main.
	// Spelled out here because buildPrompt itself has no guard — the boundary
	// does, and a test that only pinned buildPrompt would be pinning the wrong
	// function.
	const twoLines = "her\nald"
	if err := validateAgentName(twoLines); err == nil {
		t.Fatalf("validateAgentName(%q) = nil — buildPrompt would emit an unterminated backtick span", twoLines)
	}
	broken := buildPrompt(twoLines, "Approve this?", "")
	if strings.Contains(strings.SplitN(broken, "\n", 2)[0], "`"+twoLines+"`") {
		t.Error("the two-line fixture no longer splits buildPrompt's span, so it no longer witnesses the harm the guard prevents")
	}
}
