package main

import (
	"errors"
	"flag"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The defaults that ship, and the wiring that carries them to the tick.
//
// Every one of these was unreachable until parseFlags was lifted out of main().
// The suite in spend_test.go and ceiling_test.go passes budgets in by hand, so
// it asserts values the tests themselves chose — the values the binary actually
// starts with had never been read by anything. That is sharper here than in
// most of these mains, because this binary decides how much money a dispatched
// session may spend, and its two worst defaults fail silently: -dry-run on
// evaluates every provider, logs what it would dispatch, starts nothing and
// exits 0, and a -default-max-budget-usd that drifted off 0 caps sessions the
// user never asked to cap.

// noEnvironment is a process started with none of these variables set, which
// is how the scheduler runs this binary.
var noEnvironment = servicesettings.MapEnvironment(map[string]string{})

// environmentWith answers only for the keys given to it, so a test states the
// environment it means rather than inheriting the one the test binary happens
// to have.
func environmentWith(values map[string]string) servicesettings.Environment {
	return servicesettings.MapEnvironment(values)
}

func TestTheDefaultsThatShipAreTheOnesThisBinaryStartsWith(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}

	// Spelled out one per line rather than as a struct literal comparison so a
	// failure names the flag that moved instead of printing two fourteen-field
	// structs and leaving the reader to diff them.
	for _, check := range []struct {
		name string
		got  any
		want any
	}{
		{"-register", cfg.registerJob, false},
		{"-dry-run", cfg.dryRun, false},
		{"-slack", cfg.slackPercentPoints, 15.0},
		{"-tz", cfg.timezone, "America/Los_Angeles"},
		{"-default-max-budget-usd", cfg.defaultMaxBudgetUSD, 0.0},

		{"LLM_BRIDGE_URL", cfg.llmBridgeURL, "http://localhost:8160"},
		{"LLM_BRIDGE_START_SESSIONS_AS_PRINCIPAL_ID", cfg.sessionPrincipalID, ""},
		{"issues under the bridge", cfg.issuesURL, "http://localhost:8160/issues"},
		{"usage under the bridge", cfg.usageURL, "http://localhost:8160/usage"},
		{"scheduler under the bridge", cfg.schedulerURL, "http://localhost:8160/scheduler"},
		{"models under the bridge", cfg.modelsURL, "http://localhost:8160/models"},
		{"LLM_BRIDGE_SERVICE_PRINCIPAL_TOKEN", cfg.bridgeServicePrincipalToken, ""},

		{"state path", cfg.statePath, "/home/someone/.config/autoworker/state.json"},
		{"registered command", cfg.binaryPath, "/home/someone/bin/autoworker"},
	} {
		if check.got != check.want {
			t.Errorf("%s default = %v, want %v", check.name, check.got, check.want)
		}
	}

	// -prompt is checked against defaultPrompt rather than against its text: it
	// is a 40-line operating manual for the dispatched worker, and pinning that
	// prose verbatim here would make every edit to it a failing test in a file
	// about flag defaults. What this row pins is that the flag is wired to
	// defaultPrompt at all, for the configured bridge's issues — an empty or
	// hardcoded default would dispatch a worker with no instructions, which
	// reads as a confused session rather than as a configuration bug.
	if cfg.prompt != defaultPrompt("http://localhost:8160/issues") {
		t.Error("-prompt default is not defaultPrompt for the default bridge's issues")
	}
}

// TestDryRunShipsOffBecauseOnIsIndistinguishableFromAQuietWeek is separate from
// the table above even though it repeats one row of it, because the reason this
// value matters is not that it is `false` but what `true` would look like from
// outside: a tick that discovers every provider, logs a full decision for each,
// writes no state, starts nothing, and exits 0.
func TestDryRunShipsOffBecauseOnIsIndistinguishableFromAQuietWeek(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}
	if cfg.dryRun {
		t.Fatal("-dry-run ships ON: every tick would report success and dispatch nothing")
	}
	if cfg.registerJob {
		t.Fatal("-register ships ON: every tick would try to register a cron job and exit before evaluating anything")
	}
}

// TestEachEnvironmentVariableReachesItsOwnField is the test the endpoints were
// gathered into one struct for.
//
// Every store endpoint is a path under the one bridge URL, and two of them —
// llmBridgeURL and issuesURL — are handed to budgetForCard positionally, side
// by side. Swapping any pair compiles clean and passes every other test in the
// package, because nothing else reads them. Asserting the path each one lands
// on is what makes a swap fail.
func TestEachEnvironmentVariableReachesItsOwnField(t *testing.T) {
	cfg, err := parseFlags(nil, environmentWith(map[string]string{
		"LLM_BRIDGE_START_SESSIONS_AS_PRINCIPAL_ID": "principal_000001",
		"LLM_BRIDGE_URL":                     "http://bridge.example",
		"LLM_BRIDGE_SERVICE_PRINCIPAL_TOKEN": "a-service-principal-token",
	}), "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}

	for _, check := range []struct {
		variable string
		got      string
		want     string
	}{
		{"LLM_BRIDGE_START_SESSIONS_AS_PRINCIPAL_ID", cfg.sessionPrincipalID, "principal_000001"},
		{"LLM_BRIDGE_URL", cfg.llmBridgeURL, "http://bridge.example"},
		{"LLM_BRIDGE_URL issues", cfg.issuesURL, "http://bridge.example/issues"},
		{"LLM_BRIDGE_URL usage", cfg.usageURL, "http://bridge.example/usage"},
		{"LLM_BRIDGE_URL scheduler", cfg.schedulerURL, "http://bridge.example/scheduler"},
		{"LLM_BRIDGE_URL models", cfg.modelsURL, "http://bridge.example/models"},
		{"LLM_BRIDGE_SERVICE_PRINCIPAL_TOKEN", cfg.bridgeServicePrincipalToken, "a-service-principal-token"},
	} {
		if check.got != check.want {
			t.Errorf("%s reached the wrong field: got %q, want %q", check.variable, check.got, check.want)
		}
	}
}

// TestAnUnsetVariableFallsBackAndASetOneDoesNot pins the default's direction. It has
// been the wrong way round in this family before: preferring the fallback over
// the environment leaves every endpoint on localhost no matter what the unit
// file says, which on this box is a working configuration and therefore silent.
func TestAnUnsetVariableFallsBackAndASetOneDoesNot(t *testing.T) {
	// One variable set, the rest not, so the fallback and the override are
	// exercised against each other in a single parse.
	cfg, err := parseFlags(nil, environmentWith(map[string]string{
		"LLM_BRIDGE_URL": "http://bridge.example",
	}), "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}
	if cfg.issuesURL != "http://bridge.example/issues" {
		t.Errorf("a set variable was ignored: issuesURL = %q", cfg.issuesURL)
	}
	if cfg.usageURL != "http://bridge.example/usage" {
		t.Errorf("an unset path did not follow the bridge: usageURL = %q", cfg.usageURL)
	}
	// The prompt's addresses follow the bridge too: a worker told to PATCH the
	// default bridge's issues would write to the wrong server.
	if cfg.prompt != defaultPrompt("http://bridge.example/issues") {
		t.Error("-prompt default does not name the configured bridge's issues")
	}
}

// TestHomeRelativePathsAreBuiltFromTheGivenHome pins the two paths that are not
// URLs. The state path is the cooldown record — resolve it somewhere else and
// every tick reads an empty file, finds no last-fire time, and treats the
// cooldown as expired.
func TestHomeRelativePathsAreBuiltFromTheGivenHome(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/tmp/elsewhere")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}
	if want := filepath.Join("/tmp/elsewhere", ".config", "autoworker", "state.json"); cfg.statePath != want {
		t.Errorf("state path = %q, want %q", cfg.statePath, want)
	}
	if want := filepath.Join("/tmp/elsewhere", "bin", "autoworker"); cfg.binaryPath != want {
		t.Errorf("registered command = %q, want %q", cfg.binaryPath, want)
	}
}

func TestExplicitArgumentsBeatBothTheEnvironmentAndTheDefault(t *testing.T) {
	cfg, err := parseFlags(
		[]string{"-dry-run", "-slack=40", "-tz=UTC", "-default-max-budget-usd=2.50", "-prompt=go"},
		environmentWith(map[string]string{"AUTOWORKER_DEFAULT_MAX_BUDGET_USD": "9.99"}),
		"/home/someone",
	)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if !cfg.dryRun {
		t.Error("-dry-run was not applied")
	}
	if cfg.slackPercentPoints != 40 {
		t.Errorf("-slack = %v, want 40", cfg.slackPercentPoints)
	}
	if cfg.timezone != "UTC" {
		t.Errorf("-tz = %q, want UTC", cfg.timezone)
	}
	if cfg.prompt != "go" {
		t.Errorf("-prompt = %q, want go", cfg.prompt)
	}
	// The argument has to win over the variable, not merely over the built-in 0:
	// the variable is what supplies this flag's default, so a precedence bug here
	// looks like the flag being ignored only on hosts that set the variable.
	if cfg.defaultMaxBudgetUSD != 2.50 {
		t.Errorf("-default-max-budget-usd = %v, want 2.50 (the argument, not the environment's 9.99)", cfg.defaultMaxBudgetUSD)
	}
}

func TestTheSpendCapCanBeSetFromTheEnvironmentAlone(t *testing.T) {
	cfg, err := parseFlags(nil, environmentWith(map[string]string{
		"AUTOWORKER_DEFAULT_MAX_BUDGET_USD": "12.5",
	}), "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}
	if cfg.defaultMaxBudgetUSD != 12.5 {
		t.Errorf("defaultMaxBudgetUSD = %v, want 12.5", cfg.defaultMaxBudgetUSD)
	}
}

// TestAMalformedSpendCapIsRefusedRatherThanTreatedAsUncapped covers the value
// this whole mechanism exists to protect.
//
// It used to be a log.Fatalf inside a package variable initializer, so it fired
// before main() ran and nothing could observe it. It is now an error, and the
// sentinel is what lets main keep exit code 1 for it while the flag package
// keeps 2 for a bad argument.
func TestAMalformedSpendCapIsRefusedRatherThanTreatedAsUncapped(t *testing.T) {
	_, err := parseFlags(nil, environmentWith(map[string]string{
		"AUTOWORKER_DEFAULT_MAX_BUDGET_USD": "3.50usd",
	}), "/home/someone")
	if err == nil {
		t.Fatal("a malformed spend cap parsed cleanly — it would have run uncapped on a typo")
	}
	if !errors.Is(err, errMalformedEnvironmentValue) {
		t.Errorf("error does not carry errMalformedEnvironmentValue, so main would exit 2 rather than 1: %v", err)
	}
}

func TestANegativeSpendCapIsRejected(t *testing.T) {
	cfg, err := parseFlags([]string{"-default-max-budget-usd=-1"}, noEnvironment, "/home/someone")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("a negative spend cap validated")
	}
}

func TestTheDefaultConfigurationValidatesOnceItNamesAPrincipal(t *testing.T) {
	cfg, err := parseFlags(nil, environmentWith(map[string]string{"LLM_BRIDGE_START_SESSIONS_AS_PRINCIPAL_ID": "principal_000001"}), "/home/someone")
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("the configuration this binary ships with does not validate: %v", err)
	}
}

// A run that may start a session refuses to begin with no principal to start
// it as; a dry run and a registration start none, and need none.
func TestARunWithNoPrincipalToStartSessionsAsIsRejected(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/home/someone")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "LLM_BRIDGE_START_SESSIONS_AS_PRINCIPAL_ID") {
		t.Fatalf("validate = %v, want a refusal naming the variable", err)
	}
	for _, args := range [][]string{{"-dry-run"}, {"-register"}} {
		cfg, err := parseFlags(args, noEnvironment, "/home/someone")
		if err != nil {
			t.Fatalf("parsing %v: %v", args, err)
		}
		if err := cfg.validate(); err != nil {
			t.Errorf("%v starts no session and needs no principal: %v", args, err)
		}
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

// A misspelling of the one variable this binary owns used to be ignored, and
// the spend cap it meant to set silently stayed off. It now stops the binary,
// with the exit code of any other bad environment value.
func TestAMisspelledAutoworkerVariableIsRefused(t *testing.T) {
	_, err := parseFlags(nil, environmentWith(map[string]string{
		"AUTOWORKER_DEFAULT_MAX_BUDGET": "5",
	}), "/home/someone")
	if err == nil {
		t.Fatal("AUTOWORKER_DEFAULT_MAX_BUDGET was accepted; the cap it meant to set would be off")
	}
	if !errors.Is(err, errMalformedEnvironmentValue) {
		t.Errorf("error does not carry errMalformedEnvironmentValue, so main would exit 2 rather than 1: %v", err)
	}
}
