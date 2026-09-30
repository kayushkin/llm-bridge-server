package main

import (
	"errors"
	"flag"
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge/servicesettings"
	schedulerconfig "github.com/slava/scheduler/internal/config"
)

// The defaults that ship, and the wiring that carries them to the tick.
//
// Every one of these was unreachable until parseFlags was lifted out of main().
// classify_test.go, digest_test.go and provenance_test.go each build their own
// config fixtures, so every value they assert is one the test chose; the twelve
// values this binary actually starts with had never been read by anything.
//
// Two families are worth more than the rest here:
//
//   - The THREE switches that turn a tick into a no-op. -register exits after
//     touching the scheduler, -dry-run classifies at full model cost and writes
//     nothing, and -dump-digest prints every candidate's digest and classifies
//     none. Any of them shipping ON leaves a binary that runs every 15 minutes,
//     walks the session list, logs a full pass and exits 0 — which from outside
//     is a box with no active sessions.
//   - The TWO that decide spend. -model picks which model reads every digest,
//     and -max bounds how many sessions one tick pays to classify.

// noEnvironment is a process started with none of these
// variables set, which is how the scheduler runs this binary: jobs run as
// `sh -c` with only the scheduler's own thin environment.
var noEnvironment = servicesettings.MapEnvironment(map[string]string{})

// environmentWith answers only for the keys given to it, so a test states the
// environment it means rather than inheriting the one the test binary happens
// to have.
func environmentWith(values map[string]string) servicesettings.Environment {
	return servicesettings.MapEnvironment(values)
}

func TestTheDefaultsThatShipAreTheOnesThisBinaryStartsWith(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/home/agent")
	if err != nil {
		t.Fatalf("parseFlags with no arguments: %v", err)
	}

	t.Run("strings", func(t *testing.T) {
		for _, row := range []struct {
			flag string
			got  string
			want string
		}{
			{"-board", cfg.BoardName, "Agent runs"},
			{"-kanban-url", cfg.KanbanURL, "http://localhost:8305"},
			{"-bridge-url", cfg.BridgeURL, "http://localhost:8160"},
			{"-session", cfg.OneSession, ""},
		} {
			if row.got != row.want {
				t.Errorf("%s ships as %q, want %q", row.flag, row.got, row.want)
			}
		}
	})

	t.Run("integers", func(t *testing.T) {
		if cfg.MaxClassify != 10 {
			t.Errorf("-max ships as %d, want 10", cfg.MaxClassify)
		}
		if cfg.DigestBudget != 16000 {
			t.Errorf("-digest-budget ships as %d, want 16000", cfg.DigestBudget)
		}
	})

	t.Run("derived", func(t *testing.T) {
		// Not registered as a flag: it is set from -session, and the dedupe gates
		// that ration the cron sweep key off it. Shipping it true would bypass
		// them on every session of every tick.
		if cfg.ExplicitSession {
			t.Error("ExplicitSession ships true with no -session; the per-session dedupe gates would be bypassed on the cron path")
		}
		if cfg.SchedulerURL != "http://localhost:8092" {
			t.Errorf("SchedulerURL ships as %q, want http://localhost:8092", cfg.SchedulerURL)
		}
		if want := filepath.Join("/home/agent", "bin", "kanban-classifier"); cfg.BinaryPath != want {
			t.Errorf("BinaryPath ships as %q, want %q", cfg.BinaryPath, want)
		}
	})
}

// TestEveryNoOpSwitchShipsOff is separate from the table above even though it
// repeats three of its rows, because the reason these values matter is not that
// they are `false` but what `true` would look like from outside: a tick that
// walks the session list, logs a full pass, writes no card, and exits 0.
//
// -dump-digest is the quietest of the three despite being the loudest on
// stdout. -register and -dry-run leave a run history that says nothing
// happened; -dump-digest fills it with digests, so the job looks like it is
// working.
func TestEveryNoOpSwitchShipsOff(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/home/agent")
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	for _, row := range []struct {
		flag string
		on   bool
		harm string
	}{
		{"-register", cfg.RegisterJob, "every tick registers a cron job and exits before reading a single session"},
		{"-dry-run", cfg.DryRun, "every tick pays the full model bill and writes no card"},
		{"-dump-digest", cfg.DumpDigest, "every tick prints every candidate's digest, classifies nothing, and fills the run history with plausible output"},
	} {
		if row.on {
			t.Errorf("%s ships ON: %s", row.flag, row.harm)
		}
	}
}

// TestTheTwoValuesThatDecideSpend covers what one tick can cost.
//
// The model role is the larger of the two. This binary is the cheap-model
// curator by design — its package comment says so — so a seed that drifted to
// a frontier role would multiply the bill for the same work with nothing
// failing. The role is the scheduler's stored setting, seeded from the
// constant; no flag or variable of this binary can name a model any more.
func TestTheTwoValuesThatDecideSpend(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/home/agent")
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.ModelRole != "" {
		t.Errorf("parseFlags set the model role to %q; only run, reading the scheduler's settings, may", cfg.ModelRole)
	}
	if schedulerconfig.DefaultKanbanClassifierModelRole != "efficient" {
		t.Errorf("schedulerconfig.DefaultKanbanClassifierModelRole is %q: this binary is the cheap-model curator and a drift here is a silent bill", schedulerconfig.DefaultKanbanClassifierModelRole)
	}
	if _, err := parseFlags([]string{"-model", "claude-haiku-4-5-20251001"}, noEnvironment, "/home/agent"); err == nil {
		t.Error("-model was accepted: a flag would be a second source that silently beats the settings page")
	}
	if _, err := parseFlags(nil, environmentWith(map[string]string{"KANBAN_CLASSIFIER_MODEL": "claude-haiku-4-5-20251001"}), "/home/agent"); !errors.Is(err, errSettingsRefused) {
		t.Errorf("KANBAN_CLASSIFIER_MODEL left set = %v, want the settings refusal", err)
	}
	if cfg.MaxClassify != maxClassifyPerTick {
		t.Errorf("-max ships as %d, want the constant %d", cfg.MaxClassify, maxClassifyPerTick)
	}
	if maxClassifyPerTick != 10 {
		t.Errorf("maxClassifyPerTick is %d: this bounds model calls per tick, and the tick runs every 15 minutes", maxClassifyPerTick)
	}
}

// TestEachEnvironmentVariableReachesItsOwnField is the test the flag set was
// registered into named fields for.
//
// Three variables land in this config and all are strings that would be
// accepted in each other's place — base URLs. Nothing downstream would reject
// a swap: kanbanclient.New takes the kanban URL positionally, and oneshot.Call
// and listSessions both take the bridge URL. Giving each variable a
// distinct value and asserting the field it lands in is what makes a swap
// fail.
func TestEachEnvironmentVariableReachesItsOwnField(t *testing.T) {
	cfg, err := parseFlags(nil, environmentWith(map[string]string{
		"KANBAN_STORE_URL": "http://kanban.example",
		"LLM_BRIDGE_URL":   "http://bridge.example",
		"SCHEDULER_URL":    "http://scheduler.example",
	}), "/home/agent")
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	for _, row := range []struct {
		variable string
		got      string
		want     string
	}{
		{"KANBAN_STORE_URL", cfg.KanbanURL, "http://kanban.example"},
		{"LLM_BRIDGE_URL", cfg.BridgeURL, "http://bridge.example"},
		{"SCHEDULER_URL", cfg.SchedulerURL, "http://scheduler.example"},
	} {
		if row.got != row.want {
			t.Errorf("%s reached the wrong field: got %q, want %q", row.variable, row.got, row.want)
		}
	}
}

// TestAFlagBeatsTheEnvironmentAndTheEnvironmentBeatsTheDefault pins the
// precedence the whole seam rests on. envOr is consulted at registration time,
// so the environment supplies the flag's *default* — an explicit argument still
// has to win, and it does only because Parse runs after registration.
func TestAFlagBeatsTheEnvironmentAndTheEnvironmentBeatsTheDefault(t *testing.T) {
	environment := environmentWith(map[string]string{"KANBAN_STORE_URL": "http://from-environment"})

	fromEnvironment, err := parseFlags(nil, environment, "/home/agent")
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if fromEnvironment.KanbanURL != "http://from-environment" {
		t.Errorf("environment did not beat the built-in default: got %q", fromEnvironment.KanbanURL)
	}

	fromFlag, err := parseFlags([]string{"-kanban-url", "http://from-flag"}, environment, "/home/agent")
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if fromFlag.KanbanURL != "http://from-flag" {
		t.Errorf("-kanban-url did not beat the environment: got %q", fromFlag.KanbanURL)
	}

	fromDefault, err := parseFlags(nil, noEnvironment, "/home/agent")
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if fromDefault.KanbanURL != "http://localhost:8305" {
		t.Errorf("unset environment did not fall back to the built-in default: got %q", fromDefault.KanbanURL)
	}
}

// TestAnEmptyEnvironmentVariableIsTreatedAsUnset pins envOr's actual rule. It
// tests for emptiness, not presence, so `KANBAN_STORE_URL=` falls back to the
// default rather than sending every board write to the empty string — which
// would fail at the first request rather than silently, but with an error
// naming no host.
func TestAnEmptyEnvironmentVariableIsTreatedAsUnset(t *testing.T) {
	cfg, err := parseFlags(nil, environmentWith(map[string]string{"KANBAN_STORE_URL": ""}), "/home/agent")
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.KanbanURL != "http://localhost:8305" {
		t.Errorf("an empty KANBAN_STORE_URL was not treated as unset: got %q", cfg.KanbanURL)
	}
}

// TestTheSeamIsCallableTwiceAndReturnsBadArgumentsInsteadOfExiting is the
// property that makes every test above possible.
//
// Both halves are load-bearing. The package-level flag.CommandLine would panic
// on the second call with "flag redefined", and flag.ExitOnError would call
// os.Exit on a bad argument — taking the test binary down mid-run, which reads
// as a broken fixture rather than as a failed assertion.
func TestTheSeamIsCallableTwiceAndReturnsBadArgumentsInsteadOfExiting(t *testing.T) {
	if _, err := parseFlags(nil, noEnvironment, "/home/agent"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := parseFlags(nil, noEnvironment, "/home/agent"); err != nil {
		t.Fatalf("second call (a redefined flag would have panicked): %v", err)
	}

	if _, err := parseFlags([]string{"-no-such-flag"}, noEnvironment, "/home/agent"); err == nil {
		t.Error("a bad argument returned no error; ExitOnError would have exited the test binary here")
	}

	// -h is not a failure, and main() keys its exit code off exactly this.
	_, err := parseFlags([]string{"-h"}, noEnvironment, "/home/agent")
	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h returned %v, want flag.ErrHelp: main() exits 0 on this and 2 on everything else", err)
	}
}

// TestValidateRefusesTheConfigurationsThatClassifyNothingForever covers the two
// inputs that do not fail at the first HTTP call.
//
// Everything else in this config — a wrong URL, a wrong token, a wrong model id
// — dies on the first request and names itself in the log. These two run to a
// clean exit having done no work.
func TestValidateRefusesTheConfigurationsThatClassifyNothingForever(t *testing.T) {
	t.Run("a tick with no classification budget", func(t *testing.T) {
		// The cap is `classified >= cfg.MaxClassify`, already true at zero before
		// the first session is looked at.
		cfg, err := parseFlags([]string{"-max", "0"}, noEnvironment, "/home/agent")
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if err := cfg.validate(); err == nil {
			t.Error("-max=0 was accepted: the per-tick cap is checked before the first session, so no session is ever classified")
		}
	})

	t.Run("a digest budget the builder silently overrules", func(t *testing.T) {
		// buildDigest's own guard is `if charBudget <= 0 { charBudget = digestCharBudget }`,
		// so a zero here is not a small digest — it is the full-size default, at
		// full cost, with the operator ignored.
		cfg, err := parseFlags([]string{"-digest-budget", "0"}, noEnvironment, "/home/agent")
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if err := cfg.validate(); err == nil {
			t.Error("-digest-budget=0 was accepted: buildDigest substitutes the 16000 default for it and the operator's intent is silently discarded")
		}
	})

	t.Run("the shipped configuration passes", func(t *testing.T) {
		cfg, err := parseFlags(nil, noEnvironment, "/home/agent")
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if err := cfg.validate(); err != nil {
			t.Errorf("the defaults this binary ships with do not pass their own guard: %v", err)
		}
	})

	t.Run("dumping the whole candidate sweep is allowed", func(t *testing.T) {
		// -dump-digest without -session is a working debug path: processSession
		// prints each candidate's digest and returns false. The help text reads
		// "with -session", which is guidance, not a precondition.
		cfg, err := parseFlags([]string{"-dump-digest"}, noEnvironment, "/home/agent")
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if err := cfg.validate(); err != nil {
			t.Errorf("-dump-digest without -session was refused: %v", err)
		}
	})
}

// TestTheRegisteredJobMatchesTheOneThatRuns pins the payload -register writes.
//
// This payload is written once per host, and -register never rewrites an
// existing job, so a wrong value here stays until someone PATCHes the live job
// by hand. Every field below was compared
// against scheduler job 14 as it actually runs on this host, not inferred from
// this source.
func TestTheRegisteredJobMatchesTheOneThatRuns(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment, "/home/agent")
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	payload := schedulerJob(cfg).Payload()

	for _, row := range []struct {
		key  string
		want any
	}{
		{"name", "kanban-classifier-tick"},
		{"schedule", "*/15 * * * *"},
		{"type", "shell"},
		{"command", "/home/agent/bin/kanban-classifier"},
		// 0 would mean the scheduler's 5-minute default. A tick makes one model
		// call per candidate session and outruns it, and the process group is
		// killed mid-write when it does.
		{"timeout_seconds", 900},
	} {
		if got := payload[row.key]; got != row.want {
			t.Errorf("registered job %s = %v, want %v", row.key, got, row.want)
		}
	}

	// The dedupe -register does before POSTing keys off this exact name. A drift
	// means it stops recognising its own job and registers a second one beside
	// the first, and the box then classifies every session twice.
	if payload["name"] != schedulerJobName {
		t.Errorf("the payload name %v is not the constant %q the dedupe check reads", payload["name"], schedulerJobName)
	}
}

// A misspelled KANBAN_CLASSIFIER_MODEL used to be ignored, and the binary ran
// on the default model. It now stops the binary, and main prints why.
func TestAMisspelledKanbanClassifierVariableIsRefused(t *testing.T) {
	_, err := parseFlags(nil, environmentWith(map[string]string{"KANBAN_CLASSIFIER_MODLE": "claude-opus-5"}), "/home/agent")
	if !errors.Is(err, errSettingsRefused) {
		t.Fatalf("parseFlags = %v, want errSettingsRefused", err)
	}
}
