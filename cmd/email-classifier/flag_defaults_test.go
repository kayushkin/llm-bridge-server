package main

import (
	"errors"
	"flag"
	"path/filepath"
	"testing"
)

// The defaults that ship, and the wiring that carries them to the tick.
//
// Every one of these was unreachable until parseFlags was lifted out of main().
// The suite in priority_test.go, session_test.go and text_test.go builds its own
// fixtures, so every value it asserts is one the tests chose; the eighteen
// values this binary actually starts with had never been read by anything.
//
// Two families are worth more than the rest here:
//
//   - The SIX switches that turn a tick into a no-op. Five are early exits
//     (-register, -merge, -backfill-due, -refile-unread-since, -dump-digest)
//     and the sixth (-dry-run) neuters the writes. Any one of them shipping ON leaves a binary that runs
//     every 15 minutes, reads the board, logs, exits 0 and files nothing — which
//     from outside is a quiet mailbox.
//   - The TWO that decide spend. -model picks which model reads every digest,
//     and -max bounds how much mail one tick pays to classify.

// noEnvironment is the lookup for a process started with none of these
// variables set, which is how the scheduler runs this binary: jobs run as
// `sh -c` with only the scheduler's own thin environment.
func noEnvironment(string) string { return "" }

// environmentWith answers only for the keys given to it, so a test states the
// environment it means rather than inheriting the one the test binary happens
// to have.
func environmentWith(values map[string]string) environmentLookup {
	return func(key string) string { return values[key] }
}

func TestTheDefaultsThatShipAreTheOnesThisBinaryStartsWith(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}

	// Spelled out one per line rather than as a struct literal comparison so a
	// failure names the flag that moved instead of printing two twenty-field
	// structs and leaving the reader to diff them.
	for _, check := range []struct {
		name string
		got  any
		want any
	}{
		{"-register", cfg.RegisterJob, false},
		{"-dry-run", cfg.DryRun, false},
		{"-dump-digest", cfg.DumpDigest, false},
		{"-board-id", cfg.BoardID, ""},
		{"-board", cfg.BoardName, "Email"},
		{"-kanban-url", cfg.KanbanURL, "http://localhost:8305"},
		{"-mail-url", cfg.MailURL, "http://localhost:8195"},
		{"-mail-token", cfg.MailToken, "changeme"},
		{"-bridge-url", cfg.BridgeURL, "http://localhost:8160"},
		{"-auth-store-url", cfg.AuthStoreURL, "http://localhost:8303"},
		{"-auth-token", cfg.AuthToken, "changeme"},
		{"-model", cfg.Model, "claude-sonnet-4-6"},
		{"-max", cfg.MaxEmails, 50},
		{"-account", cfg.OnlyAccount, ""},
		{"-backfill", cfg.Backfill, false},
		{"-merge", cfg.MergePass, false},
		{"-backfill-due", cfg.BackfillDue, false},
		{"-refile-unread-since", cfg.RefileUnreadSince, ""},

		{"SCHEDULER_URL", cfg.SchedulerURL, "http://localhost:8092"},
		{"registered command", cfg.BinaryPath, "/home/someone/bin/email-classifier"},
	} {
		if check.got != check.want {
			t.Errorf("%s default = %v, want %v", check.name, check.got, check.want)
		}
	}
}

// TestTheDefaultBoardIsResolvedByNameOnlyAsAFallback pins the pair resolveBoard
// takes positionally. Swapping them compiles clean: both are strings, and the
// binary would then look for a board whose id is "Email", fail to find it, and
// refuse to run — or worse, with -board-id set, treat the id as a name and
// auto-create a second board, which is the exact fork pinning the id prevents.
func TestTheDefaultBoardIsResolvedByNameOnlyAsAFallback(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}
	if cfg.BoardID != "" {
		t.Errorf("-board-id ships with a value (%q): every host would pin the same board", cfg.BoardID)
	}
	if cfg.BoardName != defaultBoardName {
		t.Errorf("-board default = %q, want %q", cfg.BoardName, defaultBoardName)
	}

	// And the id has to win when both are given, which is the whole reason the
	// id exists.
	cfg, err = parseFlags([]string{"-board-id=abc123", "-board=Somewhere Else"}, noEnvironment, "/home/someone")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if cfg.BoardID != "abc123" || cfg.BoardName != "Somewhere Else" {
		t.Fatalf("board flags reached the wrong fields: id=%q name=%q", cfg.BoardID, cfg.BoardName)
	}
}

// TestEveryNoOpSwitchShipsOff is separate from the table above even though it
// repeats six of its rows, because the reason these values matter is not that
// they are off but what ON would look like from outside: a tick that reads the
// board, logs a full pass, files nothing, and exits 0.
//
// This binary has six of them where its siblings have one. Five are early exits
// that return before any new mail is read at all, and -refile-unread-since is
// the one whose no-op is not harmless: shipped ON it would strip mail off the
// board every fifteen minutes.
func TestEveryNoOpSwitchShipsOff(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}
	for _, check := range []struct {
		flag       string
		on         bool
		whatItDoes string
	}{
		{"-register", cfg.RegisterJob, "every tick tries to register a cron job and exits before reading the board"},
		{"-merge", cfg.MergePass, "every tick runs the merge pass and returns, filing no new mail at all"},
		{"-backfill-due", cfg.BackfillDue, "every tick runs the one-off deadline recovery and returns, filing no new mail"},
		{"-refile-unread-since", cfg.RefileUnreadSince != "", "every tick unfiles mail off the board and returns, filing no new mail"},
		{"-dump-digest", cfg.DumpDigest, "every tick prints the digest to stdout and never calls the model or writes a card"},
		{"-dry-run", cfg.DryRun, "every tick classifies at full model cost and writes nothing to kanban-store"},
	} {
		if check.on {
			t.Errorf("%s ships ON: %s", check.flag, check.whatItDoes)
		}
	}
}

// TestTheTwoValuesThatDecideSpend covers what one tick can cost.
//
// -model is the larger of the two: this binary deliberately uses a better model
// than its sibling classifiers, and the reason is written down in the constant's
// comment. A default that drifted to a cheaper model would degrade filing
// quality with nothing failing, and one that drifted to a more expensive model
// would multiply the bill for the same work.
func TestTheTwoValuesThatDecideSpend(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}
	if cfg.Model != defaultModel {
		t.Errorf("-model default = %q, want defaultModel (%q)", cfg.Model, defaultModel)
	}
	if cfg.MaxEmails != maxEmailsPerTick {
		t.Errorf("-max default = %d, want maxEmailsPerTick (%d)", cfg.MaxEmails, maxEmailsPerTick)
	}
}

// TestEachEnvironmentVariableReachesItsOwnField is the test the flag set was
// registered into named fields for.
//
// Nine variables land in this config and seven of them are strings that would
// be accepted in each other's place. Two pairs are consumed positionally —
// newMailClient takes (MailURL, MailToken) and resolveBoard takes (BoardID,
// BoardName) — so a swap compiles clean and passes every other test in the
// package, because nothing else reads them. Giving each variable a distinct
// value and asserting the field it lands in is what makes a swap fail.
func TestEachEnvironmentVariableReachesItsOwnField(t *testing.T) {
	cfg, err := parseFlags(nil, environmentWith(map[string]string{
		"EMAIL_BOARD_ID":         "board-from-the-environment",
		"KANBAN_STORE_URL":       "http://kanban.example",
		"MAILSTACK_URL":          "http://mailstack.example",
		"MAILSTACK_TOKEN":        "mail-token-value",
		"LLM_BRIDGE_URL":         "http://bridge.example",
		"AUTH_STORE_URL":         "http://auth.example",
		"AUTH_STORE_TOKEN":       "auth-token-value",
		"EMAIL_CLASSIFIER_MODEL": "model-from-the-environment",
		"SCHEDULER_URL":          "http://scheduler.example",
	}), "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}

	for _, check := range []struct {
		variable string
		got      string
		want     string
	}{
		{"EMAIL_BOARD_ID", cfg.BoardID, "board-from-the-environment"},
		{"KANBAN_STORE_URL", cfg.KanbanURL, "http://kanban.example"},
		{"MAILSTACK_URL", cfg.MailURL, "http://mailstack.example"},
		{"MAILSTACK_TOKEN", cfg.MailToken, "mail-token-value"},
		{"LLM_BRIDGE_URL", cfg.BridgeURL, "http://bridge.example"},
		{"AUTH_STORE_URL", cfg.AuthStoreURL, "http://auth.example"},
		{"AUTH_STORE_TOKEN", cfg.AuthToken, "auth-token-value"},
		{"EMAIL_CLASSIFIER_MODEL", cfg.Model, "model-from-the-environment"},
		{"SCHEDULER_URL", cfg.SchedulerURL, "http://scheduler.example"},
	} {
		if check.got != check.want {
			t.Errorf("%s reached the wrong field: got %q, want %q", check.variable, check.got, check.want)
		}
	}
}

// TestTheTwoTokensDoNotReachEachOthersService is the swap this package can
// actually afford to get wrong.
//
// mailstack's token and auth-store's token are both strings called "changeme"
// by default, and on a host where both are still the default a swap is
// invisible. Handing auth-store the mailstack token means the Anthropic key
// resolve fails and the tick dies loudly; handing mailstack the auth-store
// token means every message fetch 401s and the tick files on headers alone,
// quietly and forever.
func TestTheTwoTokensDoNotReachEachOthersService(t *testing.T) {
	cfg, err := parseFlags(nil, environmentWith(map[string]string{
		"MAILSTACK_TOKEN":  "for-mailstack",
		"AUTH_STORE_TOKEN": "for-auth-store",
	}), "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}
	if cfg.MailToken != "for-mailstack" {
		t.Errorf("MailToken = %q, want the mailstack token", cfg.MailToken)
	}
	if cfg.AuthToken != "for-auth-store" {
		t.Errorf("AuthToken = %q, want the auth-store token", cfg.AuthToken)
	}
}

// TestAnUnsetVariableFallsBackAndASetOneDoesNot pins envOr's direction. It has
// been the wrong way round in this family before: preferring the fallback over
// the environment leaves every endpoint on localhost no matter what the unit
// file says, which on this box is a working configuration and therefore silent.
func TestAnUnsetVariableFallsBackAndASetOneDoesNot(t *testing.T) {
	// One variable set, the rest not, so the fallback and the override are
	// exercised against each other in a single parse.
	cfg, err := parseFlags(nil, environmentWith(map[string]string{
		"MAILSTACK_URL": "http://mailstack.example",
	}), "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}
	if cfg.MailURL != "http://mailstack.example" {
		t.Errorf("a set variable was ignored: MailURL = %q", cfg.MailURL)
	}
	if cfg.KanbanURL != "http://localhost:8305" {
		t.Errorf("an unset variable did not fall back: KanbanURL = %q", cfg.KanbanURL)
	}
}

func TestTheRegisteredCommandIsBuiltFromTheGivenHome(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/tmp/elsewhere")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}
	if want := filepath.Join("/tmp/elsewhere", "bin", "email-classifier"); cfg.BinaryPath != want {
		t.Errorf("registered command = %q, want %q", cfg.BinaryPath, want)
	}
}

func TestExplicitArgumentsBeatBothTheEnvironmentAndTheDefault(t *testing.T) {
	cfg, err := parseFlags(
		[]string{"-dry-run", "-max=7", "-model=claude-haiku-4-5", "-account=gmail-1", "-board-id=from-the-argument"},
		environmentWith(map[string]string{
			"EMAIL_CLASSIFIER_MODEL": "from-the-environment",
			"EMAIL_BOARD_ID":         "from-the-environment",
		}),
		"/home/someone",
	)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if !cfg.DryRun {
		t.Error("-dry-run was not applied")
	}
	if cfg.MaxEmails != 7 {
		t.Errorf("-max = %d, want 7", cfg.MaxEmails)
	}
	if cfg.OnlyAccount != "gmail-1" {
		t.Errorf("-account = %q, want gmail-1", cfg.OnlyAccount)
	}
	// These two have to win over the variable, not merely over the built-in
	// default: the variable is what supplies each flag's default, so a precedence
	// bug here looks like the flag being ignored only on hosts that set it — and
	// this host sets EMAIL_BOARD_ID.
	if cfg.Model != "claude-haiku-4-5" {
		t.Errorf("-model = %q, want the argument, not the environment", cfg.Model)
	}
	if cfg.BoardID != "from-the-argument" {
		t.Errorf("-board-id = %q, want the argument, not the environment", cfg.BoardID)
	}
}

// TestATickWithNoBudgetIsRefusedRatherThanReportingSuccess covers the one input
// in this configuration that fails silently instead of loudly.
//
// collectUnfiled's budget check is `len(out) >= cfg.MaxEmails`, which is already
// true at zero — so a -max of 0 walks no page, reads no message, logs "nothing
// new to file" and exits 0, every 15 minutes, while mail piles up.
func TestATickWithNoBudgetIsRefusedRatherThanReportingSuccess(t *testing.T) {
	for _, max := range []string{"-max=0", "-max=-1"} {
		cfg, err := parseFlags([]string{max}, noEnvironment, "/home/someone")
		if err != nil {
			t.Fatalf("parsing %s: %v", max, err)
		}
		if err := cfg.validate(); err == nil {
			t.Errorf("%s validated: the tick would file nothing and still report success", max)
		}
	}
}

func TestTheDefaultConfigurationValidates(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("the configuration this binary ships with does not validate: %v", err)
	}
}

// TestTheRegisteredJobIsTheOneTheSchedulerCanActuallyRun pins the payload
// -register POSTs.
//
// This is written exactly once per host, by hand, and CLAUDE.md records that
// there is no second chance to correct the command through the API. The timeout
// is the quiet one: 0 means the scheduler's 5-minute default, and a tick that
// makes one live provider round trip per message routinely outruns that, so it
// would be killed mid-run — reported as `timeout`, with the board half-written.
func TestTheRegisteredJobIsTheOneTheSchedulerCanActuallyRun(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}
	payload := schedulerJobPayload(cfg)

	for _, check := range []struct {
		key  string
		want any
	}{
		{"name", "email-classifier-tick"},
		{"schedule", "*/15 * * * *"},
		{"type", "shell"},
		{"timeout_seconds", 900},
		{"command", "/home/someone/bin/email-classifier"},
	} {
		if got := payload[check.key]; got != check.want {
			t.Errorf("registered job %s = %v, want %v", check.key, got, check.want)
		}
	}
	if payload["timeout_seconds"] == 0 {
		t.Error("the job registers with no timeout, so the scheduler's 5-minute default kills every tick that runs long")
	}
}

// TestAPinnedBoardIdReachesTheRegisteredCommand is the reason registeredCommand
// is a function.
//
// The scheduler runs jobs as `sh -c` with its own thin environment, so
// EMAIL_BOARD_ID does not reach the process — the id has to be baked into the
// command string or the job resolves the board by name, and a rename in the UI
// then forks a second board that every cron writer follows.
func TestAPinnedBoardIdReachesTheRegisteredCommand(t *testing.T) {
	if got := registeredCommand("/home/someone/bin/email-classifier", "board-42"); got != "/home/someone/bin/email-classifier -board-id board-42" {
		t.Errorf("registered command = %q, want the board id baked in", got)
	}
	if got := registeredCommand("/home/someone/bin/email-classifier", ""); got != "/home/someone/bin/email-classifier" {
		t.Errorf("registered command with no board id = %q, want the bare binary", got)
	}

	// And it has to arrive from the flag, not only from the environment: before
	// the seam, registerSchedulerJob read EMAIL_BOARD_ID directly and ignored
	// -board-id entirely, so `-register -board-id X` warned that no id was
	// pinned and then registered a job without one.
	cfg, err := parseFlags([]string{"-board-id=board-42"}, noEnvironment, "/home/someone")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if got := schedulerJobPayload(cfg)["command"]; got != "/home/someone/bin/email-classifier -board-id board-42" {
		t.Errorf("-board-id did not reach the registered command: %v", got)
	}
}

// TestHelpIsDistinguishableFromABadArgument pins the two error shapes main maps
// to different exit codes. Without the sentinel check, `-h` would exit 2.
func TestHelpIsDistinguishableFromABadArgument(t *testing.T) {
	_, err := parseFlags([]string{"-h"}, noEnvironment, "/home/someone")
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h did not return flag.ErrHelp, so main would exit 2 rather than 0: %v", err)
	}

	_, err = parseFlags([]string{"-nonsense"}, noEnvironment, "/home/someone")
	if err == nil {
		t.Fatal("an unknown flag parsed cleanly")
	}
	if errors.Is(err, flag.ErrHelp) {
		t.Fatal("an unknown flag was reported as help, so main would exit 0 on a typo")
	}
}

// TestParsingTwiceDoesNotPanic is the property that makes every test above
// possible. On the package-level flag.CommandLine the second call redefines
// every flag and panics, and the failure reads as a broken fixture rather than
// as the seam being unusable.
func TestParsingTwiceDoesNotPanic(t *testing.T) {
	if _, err := parseFlags(nil, noEnvironment, "/home/someone"); err != nil {
		t.Fatalf("first parse: %v", err)
	}
	if _, err := parseFlags(nil, noEnvironment, "/home/someone"); err != nil {
		t.Fatalf("second parse: %v", err)
	}
}
