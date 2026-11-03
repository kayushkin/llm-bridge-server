package tools

import (
	"strings"
	"testing"
)

// A tool description is a cost control, and it is the only one this tool has.
//
// Shell applies no timeout of its own and has no background mode, so nothing in
// this package can stop a command that decides to wait. The single lever on that
// behavior is the sentence the model reads on every turn, which is why these
// tests pin it: an editor trimming the description for tokens would otherwise
// remove a guard rail and leave a green suite behind.
//
// What made it worth its 52 tokens, measured 2026-08-01 rather than assumed: two
// `until [ -s FILE ]; do sleep N; done` calls on one host had been running for
// 90,830s and 90,362s — over 25 hours each — polling sentinel files that were
// zero bytes when the loops started and zero bytes a day later. Between them
// they had forked roughly 15,000 sleeps and produced nothing.

// pollingVerbs are the shapes the wedged calls above actually took. Naming each
// one costs a few tokens and buys a match the model cannot read past; "avoid
// busy-waiting" would have described both wedged loops without naming either.
var pollingVerbs = []string{"sleep", "until", "while", "watch"}

func TestShellDescriptionNamesEveryPollingVerb(t *testing.T) {
	description := Shell().Description
	for _, verb := range pollingVerbs {
		if !strings.Contains(description, verb) {
			t.Errorf("shell_commands description does not name %q as a thing not to do.\n"+
				"This is the only place the model is told; it reads the schema every turn and "+
				"reads a project's notes never. Description was:\n%s", verb, description)
		}
	}
}

func TestShellDescriptionGivesTheAlternativeAndTheReason(t *testing.T) {
	description := Shell().Description

	// A prohibition with nowhere to go is one the model trades away the moment
	// it wants the result: it still needs an answer about a thing that is not
	// ready yet. The description has to say what to do instead.
	if !strings.Contains(description, "check again on a later turn") {
		t.Errorf("shell_commands description forbids polling without naming the alternative "+
			"(checking again on a later turn), so a model that still needs the answer has "+
			"nowhere to go. Description was:\n%s", description)
	}

	// And the reason, because the cost of waiting is invisible from inside a
	// single call — the command returns eventually and looks like it worked.
	if !strings.Contains(description, "holds the session") {
		t.Errorf("shell_commands description forbids polling without saying what it costs. "+
			"A waiting command looks successful from inside the call; the price is the "+
			"session it holds. Description was:\n%s", description)
	}
}

// The description must not promise a bound this package does not enforce.
//
// Shell passes the caller's context straight to the child process and adds no
// deadline, so "this tool times out after N" would be false here and could be
// false in a consumer too. The rule is stated without a mechanism on purpose;
// this test keeps a later edit from helpfully adding one.
func TestShellDescriptionClaimsNoTimeoutItDoesNotEnforce(t *testing.T) {
	description := strings.ToLower(Shell().Description)
	for _, claim := range []string{"timeout", "times out", "time limit", "killed after"} {
		if strings.Contains(description, claim) {
			t.Errorf("shell_commands description contains %q, but Shell sets no deadline — "+
				"it hands the caller's context to the child unchanged. Saying otherwise "+
				"tells the model a runaway command will be stopped for it. Description was:\n%s",
				claim, Shell().Description)
		}
	}
}
