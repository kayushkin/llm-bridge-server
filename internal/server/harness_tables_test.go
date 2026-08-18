package server

import (
	"os"
	"regexp"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// TestHarnessTablesCoverCanonicalSet pins the invariant that this gateway's
// per-harness tables stay in step with the canonical harness list in llm-bridge.
//
// These tables used to be hand-copied duplicates of msg.AllHarnesses. When
// HarnessCopilotCLI was added upstream, nobody updated them, so the harness
// silently disappeared from /harnesses, from conformance runs, and from
// session-create validation — a missing entry read exactly like a deliberate
// omission. This test removes that failure mode: a harness added upstream now
// breaks the build here until it is either described or explicitly disabled.
func TestHarnessTablesCoverCanonicalSet(t *testing.T) {
	for _, h := range msg.AllHarnesses {
		if reason, disabled := disabledHarnesses[h]; disabled {
			if reason == "" {
				t.Errorf("harness %q is disabled with an empty reason; state why so the omission stays a decision", h)
			}
			continue
		}
		if _, ok := harnessMetadata[h]; !ok {
			t.Errorf("harness %q is enabled but missing from harnessMetadata; add an entry or list it in disabledHarnesses", h)
		}
		if _, ok := harnessCapabilities[h]; !ok {
			t.Errorf("harness %q is enabled but missing from harnessCapabilities; add an entry or list it in disabledHarnesses", h)
		}
	}
}

// TestCompactCapabilityMatchesTheHarnesses pins the "compact" column to what
// each harness's dispatcher was measured to do on 2026-07-31. The chat UI
// gates its Compact button on this column, so an entry that drifts from the
// harness is a button that lies in one direction or the other — and nothing
// else in this repo can notice, because the harnesses live in other repos.
//
// Each name below is a claim about a specific implementation. If one of them
// changes, re-measure that harness and move it, rather than adjusting the
// expectation to match whatever the table happens to say.
func TestCompactCapabilityMatchesTheHarnesses(t *testing.T) {
	// Harnesses that send a real compaction request to their backend.
	compacts := map[msg.Harness]string{
		msg.HarnessClaudeCode: "writes /compact to the CC process",
		msg.HarnessCodex:      "HandleCompact on the codex app-server",
		msg.HarnessInber:      "POST /sessions/{id}/compact",
		msg.HarnessKiloCode:   "server.Summarize",
		msg.HarnessJig:        "writes /compact to the CC process (llm-bridge-jig 9ac0b0e)",
		msg.HarnessMock:       "reference harness, acknowledges with the summary",
	}
	// Harnesses that do not, whether they refuse honestly (hermes, cline,
	// aider, forgecode) or claim a delegation that never happens (openclaw,
	// nanoclaw). Either way the user must not be shown a Compact button.
	doesNotCompact := map[msg.Harness]string{
		msg.HarnessHermes:    "emits an UNSUPPORTED error event",
		msg.HarnessCline:     "compact_unsupported ack; cline manages context internally",
		msg.HarnessAider:     "compact_ack no-op",
		msg.HarnessForgecode: "compact_ack no-op",
		msg.HarnessOpenClaw:  `says "compaction delegated to OpenClaw" and writes nothing`,
		msg.HarnessNanoClaw:  `says "compaction delegated to NanoClaw" and writes nothing`,
	}

	has := func(h msg.Harness) bool {
		for _, c := range harnessCapabilities[h] {
			if c == "compact" {
				return true
			}
		}
		return false
	}

	for h, impl := range compacts {
		if !has(h) {
			t.Errorf("harness %q compacts for real (%s) but the table withholds \"compact\"; the UI hides a working button", h, impl)
		}
	}
	for h, why := range doesNotCompact {
		if has(h) {
			t.Errorf("harness %q does not compact (%s) but the table grants \"compact\"; the UI shows a button that does nothing", h, why)
		}
	}
}

// harnessesWhoseRemovalMustBeDeliberate is an owned literal, and owning it is
// the whole point: it is the canonical set as measured on 2026-08-14, written
// down here so that a harness leaving msg.AllHarnesses has to be a decision
// somebody typed rather than a row that quietly stopped existing.
//
// It must not be derived from msg.AllHarnesses, or from any table keyed by it.
// TestAllHarnessesExcludesDisabled already computes its expectation as
// len(msg.AllHarnesses) - len(disabledHarnesses); both sides shrink together,
// so it pins the derivation and cannot notice the population moving. A count
// checked against the table it guards agrees with itself no matter how many
// rows leave.
//
// The list is a floor, not an equality: a harness added upstream is not this
// test's business. TestHarnessTablesCoverCanonicalSet already forces a new one
// to be described or explicitly disabled, so growth is handled, and leaving
// growth alone here is what keeps this from having to be edited every time the
// canonical list legitimately grows.
var harnessesWhoseRemovalMustBeDeliberate = []msg.Harness{
	msg.HarnessClaudeCode,
	msg.HarnessCodex,
	msg.HarnessOpenClaw,
	msg.HarnessInber,
	msg.HarnessHermes,
	msg.HarnessAider,
	msg.HarnessGoose,
	msg.HarnessAutohand,
	msg.HarnessJig,
	msg.HarnessDexto,
	msg.HarnessCommander,
	msg.HarnessNanoClaw,
	msg.HarnessCline,
	msg.HarnessRooCode,
	msg.HarnessKiloCode,
	msg.HarnessOpenCode,
	msg.HarnessForgecode,
	msg.HarnessGemini,
	msg.HarnessCopilotCLI,
	msg.HarnessMock,
}

// TestNoKnownHarnessLeavesTheCanonicalListSilently is the floor under the
// tables in this file, and it exists because the guard around them was only
// ever built to face one direction.
//
// The header on TestHarnessTablesCoverCanonicalSet describes the failure it
// prevents: a harness added upstream that nobody described here, which is how
// copilot_cli went missing from /harnesses. That test walks msg.AllHarnesses
// and checks each row is described — so it sees additions, and a harness
// *removed* upstream is simply one fewer iteration. Measured 2026-08-14 by
// deleting each row in turn: 17 of the 20 canonical harnesses can leave
// msg.AllHarnesses without reddening anything in this package, and 11 of 19
// can be removed from the canonical list and both local tables together — the
// way a real retirement lands — in total silence.
//
// What that costs is the same thing the original drift cost: the harness stops
// being surfaced by /harnesses, stops being validated at session create, and
// stops being run by conformance. The tables here would keep describing it,
// which reads exactly like a harness that is still supported.
func TestNoKnownHarnessLeavesTheCanonicalListSilently(t *testing.T) {
	canonical := make(map[msg.Harness]bool, len(msg.AllHarnesses))
	for _, h := range msg.AllHarnesses {
		canonical[h] = true
	}
	for _, h := range harnessesWhoseRemovalMustBeDeliberate {
		if !canonical[h] {
			t.Errorf("harness %q has left msg.AllHarnesses; if it was retired on purpose, drop it from "+
				"harnessesWhoseRemovalMustBeDeliberate and from every table in health.go that still "+
				"describes it, so the retirement is one edit rather than a stale row", h)
		}
	}
}

// harnessKeyedTable is one table from health.go reduced to its keys, carrying
// its own name so a stale row can say which table it sits in.
type harnessKeyedTable struct {
	name string
	keys []msg.Harness
}

func harnessTableKeys[V any](name string, table map[msg.Harness]V) harnessKeyedTable {
	keys := make([]msg.Harness, 0, len(table))
	for h := range table {
		keys = append(keys, h)
	}
	return harnessKeyedTable{name: name, keys: keys}
}

// harnessKeyedTables is every table in health.go keyed by msg.Harness. All
// eight are listed. A ninth added later and left out of this list would be
// unguarded — which is the state seven of these eight were in until
// 2026-08-14 — so TestEveryHarnessKeyedTableIsListedHere reads the source and
// goes red rather than leaving that as a sentence nobody re-checks.
func harnessKeyedTables() []harnessKeyedTable {
	return []harnessKeyedTable{
		harnessTableKeys("harnessMetadata", harnessMetadata),
		harnessTableKeys("harnessSupportedProviders", harnessSupportedProviders),
		harnessTableKeys("harnessHookEvents", harnessHookEvents),
		harnessTableKeys("harnessSupportsPTY", harnessSupportsPTY),
		harnessTableKeys("harnessSupportsDisableNetwork", harnessSupportsDisableNetwork),
		harnessTableKeys("harnessSupportedPermissionModes", harnessSupportedPermissionModes),
		harnessTableKeys("harnessCapabilities", harnessCapabilities),
		harnessTableKeys("disabledHarnesses", disabledHarnesses),
	}
}

// TestHarnessKeyedTablesNameOnlyCanonicalHarnesses catches a row that outlives
// the harness it names — one left behind after a harness is renamed or removed
// upstream, which would otherwise sit here forever describing nothing.
//
// This used to be TestDisabledHarnessesAreCanonical and covered disabledHarnesses
// alone. The same hazard applies to every table keyed by msg.Harness, and there
// are eight of them: a stale entry in harnessCapabilities or harnessMetadata is
// what makes a harness retired upstream still look supported here, and a stale
// entry in harnessSupportsPTY or harnessSupportedPermissionModes is a spawn-time
// answer about a harness that no longer exists. Only disabledHarnesses was
// checked, so the other seven were unguarded.
func TestHarnessKeyedTablesNameOnlyCanonicalHarnesses(t *testing.T) {
	canonical := make(map[msg.Harness]bool, len(msg.AllHarnesses))
	for _, h := range msg.AllHarnesses {
		canonical[h] = true
	}
	for _, table := range harnessKeyedTables() {
		for _, h := range table.keys {
			if !canonical[h] {
				t.Errorf("%s names %q, which is not in msg.AllHarnesses; drop the stale entry", table.name, h)
			}
		}
	}
}

// TestEveryHarnessKeyedTableIsListedHere keeps the list above honest by reading
// health.go instead of trusting it. A table that is keyed by msg.Harness but
// missing from harnessKeyedTables is not guarded by the test above, and nothing
// about adding one would say so — the omission would look exactly like a table
// that had been considered and cleared.
func TestEveryHarnessKeyedTableIsListedHere(t *testing.T) {
	source, err := os.ReadFile("health.go")
	if err != nil {
		t.Fatalf("read health.go: %v", err)
	}
	declared := regexp.MustCompile(`(?m)^var (\w+) = map\[msg\.Harness\]`).FindAllStringSubmatch(string(source), -1)
	if len(declared) == 0 {
		t.Fatal("found no map[msg.Harness] tables in health.go; the pattern this test scans with has gone stale")
	}

	listed := make(map[string]bool)
	for _, table := range harnessKeyedTables() {
		listed[table.name] = true
	}
	for _, decl := range declared {
		if !listed[decl[1]] {
			t.Errorf("health.go declares %s, keyed by msg.Harness, and harnessKeyedTables does not list it; "+
				"add it there so TestHarnessKeyedTablesNameOnlyCanonicalHarnesses can see its rows", decl[1])
		}
	}
	for name := range listed {
		if !regexp.MustCompile(`(?m)^var ` + name + ` = map\[msg\.Harness\]`).Match(source) {
			t.Errorf("harnessKeyedTables lists %s, which health.go no longer declares; drop the stale entry", name)
		}
	}
}

// TestAllHarnessesExcludesDisabled checks the derivation itself: everything the
// gateway will surface, validate and spawn is canonical and enabled. isValidHarness
// gates session create, so a disabled harness leaking in here is what would let
// POST /sessions spawn its binary.
//
// The axis it is checked on, written down so nobody re-reads its count as more
// than it is: the len() comparison below pins the DERIVATION and not the
// population. Its expectation is computed from msg.AllHarnesses and
// disabledHarnesses, the same two tables it is checking, so both sides move
// together and a harness leaving msg.AllHarnesses keeps it green. Measured
// 2026-08-14 by deleting each canonical row in turn. The floor over the
// population is TestNoKnownHarnessLeavesTheCanonicalListSilently; the two
// owned literals at the bottom of this test (copilot_cli must stay invalid,
// claude_code must stay valid) are what make those two rows the exception.
func TestAllHarnessesExcludesDisabled(t *testing.T) {
	seen := make(map[msg.Harness]bool, len(allHarnesses))
	for _, h := range allHarnesses {
		if _, off := disabledHarnesses[h]; off {
			t.Errorf("harness %q is in disabledHarnesses but still present in allHarnesses; isValidHarness would accept it and spawn its binary", h)
		}
		if seen[h] {
			t.Errorf("harness %q appears twice in allHarnesses", h)
		}
		seen[h] = true
	}
	if want := len(msg.AllHarnesses) - len(disabledHarnesses); len(allHarnesses) != want {
		t.Errorf("allHarnesses has %d entries, want %d (canonical %d minus %d disabled)",
			len(allHarnesses), want, len(msg.AllHarnesses), len(disabledHarnesses))
	}
	// This assertion pins that copilot_cli is OFF, for the reason recorded in
	// disabledHarnesses: a stale binary on PATH. It says nothing about
	// permissions, and it is the line someone deletes to enable the harness.
	// TestEnablingAHarnessKeepsItsPermissionGate covers the other half —
	// enabling it also needs a case in injectHookSettings, or its sessions
	// spawn with no permission hook. Deleting this one does not excuse that.
	if isValidHarness(msg.HarnessCopilotCLI) {
		t.Error("isValidHarness accepts copilot_cli; session create would spawn the stale llm-bridge-copilotcli binary on PATH")
	}
	if !isValidHarness(msg.HarnessClaudeCode) {
		t.Error("isValidHarness rejects claude_code; the derivation dropped an enabled harness")
	}
}
