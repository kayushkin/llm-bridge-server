#!/usr/bin/env python3
"""Score cmd/autoworker's tests by breaking the binary on purpose.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-autoworker.py [--diffs] [--crosstable]

Why this binary is worth a scorer: it is the only thing on this box that spends
money without being asked. Every ~30 minutes it decides whether to dispatch a
budgeted agent session, and how much that session may cost. Its two worst
failures are both silent — a `-dry-run` that ships ON evaluates every provider,
logs a full decision for each, publishes nothing and exits 0, which is
indistinguishable from a week where no gate opened; and a spend cap that
silently resolves to 0 means "uncapped" to the harness, which is the exact
failure the ceiling mechanism exists to prevent.

TWO targets, and that is deliberate. This binary decides WHEN to fire in
main.go and HOW MUCH the fire may cost in spend.go, and a score covering only
one of those would be a number for half a binary — the engine's own score()
docstring names this binary as the reason it takes a list.

Case-writing rules inherited from the reminder-coordinator plan and re-earned
here:

  - Prefer a DRIFTED VALUE to a deletion. It orphans no variable, needs no
    second edit, and is the likelier real-world regression.
  - Read every applied diff against its label (`--diffs`). A row prints the name
    you gave it, not the edit you made.
  - An UNNOTICED row is not automatically a coverage hole. It can be a
    behavioural no-op, or a fake in the test file that swallows the value the
    mutation moved.

Score when filed by the 197th pass: 15/15 real mechanisms caught, both controls
behaved, exit 0. Every one of the fifteen was verified to redden a NAMED test
before this file existed — the run was done by hand first and this file is that
run made durable, because a mutation script in /tmp proves nothing to the next
pass.

⚠️ `--crosstable` has NEVER been taken for this binary. This plan is new, and
the claim "no test in cmd/autoworker has a silent column" has not been measured
and is not made here. The next pass on this binary owns that. There is also no
`unreddened` declaration below for the same reason: an empty one would read as
"measured and found none" rather than "not measured".

⚠️ Region this plan does NOT cover, declared rather than left to be
rediscovered: every case below edits configuration — the flag set, the endpoint
wiring, the two home-relative paths and the spend-cap guard. The tick's decision
logic (evaluate, interpolateTarget, insideSafeWindow, the cooldown comparison)
and the kanban queue path (selectQueuedCard, buildQueuePrompt,
markQueueCardDispatched) are NOT sabotaged here. That is the seam the 197th
pass opened and nothing more; it is not a statement that the rest is covered.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

TARGETS = [
    REPO / "cmd/autoworker/main.go",
    REPO / "cmd/autoworker/spend.go",
]
PACKAGES = ["./cmd/autoworker/"]


CASES = [
    # ---- controls ----
    Case(
        "CONTROL known-positive: the fleet default no longer reaches a discovery session",
        [("return fleetDefaultUSD, \"fleet default; noteboard-discovery session, which has no card to carry a ceiling\"",
          "return 0, \"fleet default; noteboard-discovery session, which has no card to carry a ceiling\"")],
    ),
    Case(
        "CONTROL known-negative: reword the -slack usage string",
        [('"Percent points below target before firing."',
          '"Percentage points below the target curve before a fire is allowed."')],
        expected_unnoticed="flag usage text for a human reading -h. No test reads it, and nothing "
                           "downstream parses it. This control is what proves the rows below redden "
                           "on the VALUES that ship and not on the source lines carrying them. "
                           "Falsified by a test that asserts on fs.PrintDefaults() output.",
    ),

    # ---- the six flag defaults ----
    Case(
        "-dry-run ships ON: every tick reports success and dispatches nothing",
        [('fs.BoolVar(&c.dryRun, "dry-run", false,', 'fs.BoolVar(&c.dryRun, "dry-run", true,')],
    ),
    Case(
        "-register ships ON: every tick registers a cron job and exits before evaluating",
        [('fs.BoolVar(&c.registerJob, "register", false,', 'fs.BoolVar(&c.registerJob, "register", true,')],
    ),
    Case(
        "-slack drifts 15 -> 20: fires five percent points earlier, every tick, forever",
        [('"slack", 15,', '"slack", 20,')],
    ),
    Case(
        "-tz drops to UTC: the safe window moves seven or eight hours off local time",
        [('"tz", "America/Los_Angeles"', '"tz", "UTC"')],
    ),
    Case(
        "-default-max-budget-usd drifts 0 -> 5: caps sessions the user never asked to cap",
        [('"AUTOWORKER_DEFAULT_MAX_BUDGET_USD", 0)', '"AUTOWORKER_DEFAULT_MAX_BUDGET_USD", 5)')],
    ),
    Case(
        "-prompt default emptied: the dispatched worker gets no instructions at all",
        [('fs.StringVar(&c.prompt, "prompt", defaultPrompt,', 'fs.StringVar(&c.prompt, "prompt", "",')],
    ),

    # ---- the wiring: six endpoints and two home-relative paths ----
    #
    # These are the half of the configuration the card did not count. All six
    # endpoints are strings, and two of them reach budgetForCard positionally,
    # side by side, where a swap compiles clean and is read by nothing else.
    Case(
        "llmBridgeURL and noteboardURL swapped: budgetForCard asks each service the other's question",
        [('c.llmBridgeURL = envOr(lookupEnvironment, "LLM_BRIDGE_URL", "http://localhost:8160")',
          'c.llmBridgeURL = envOr(lookupEnvironment, "NOTEBOARD_URL", "http://localhost:8191")')],
    ),
    Case(
        "usageStoreURL points at kanban-store's port: the limits fetch that gates every fire",
        [('"USAGE_STORE_URL", "http://localhost:8185"', '"USAGE_STORE_URL", "http://localhost:8305"')],
    ),
    Case(
        "envOr prefers its fallback over the environment: every endpoint pins to localhost",
        [('if v := lookupEnvironment(key); v != "" {', 'if v := lookupEnvironment(key); v == "" {')],
    ),
    Case(
        "the state path loses its .config segment: no last-fire time, so the cooldown is always expired",
        [('filepath.Join(homeDirectory, ".config", "autoworker", "state.json")',
          'filepath.Join(homeDirectory, "autoworker", "state.json")')],
    ),
    Case(
        "the registered command points at a binary that is not there",
        [('filepath.Join(homeDirectory, "bin", "autoworker")',
          'filepath.Join(homeDirectory, "bin", "autoworker-tick")')],
    ),

    # ---- the spend cap's own guards ----
    #
    # This family is why the binary was worth scoring. Each of these turns a
    # refusal into a number, and the number they turn into is "uncapped".
    Case(
        "a malformed spend cap silently becomes the default: runs uncapped on a typo",
        [('return 0, fmt.Errorf("%w: %s=%q is not a number — refusing to run uncapped on a typo", errMalformedEnvironmentValue, key, raw)',
          'return def, nil')],
    ),
    Case(
        "a malformed spend cap loses its sentinel: main exits 2 where it used to exit 1",
        [('return 0, fmt.Errorf("%w: %s=%q is not a number — refusing to run uncapped on a typo", errMalformedEnvironmentValue, key, raw)',
          'return 0, fmt.Errorf("%s=%q is not a number", key, raw)')],
    ),
    Case(
        "a negative spend cap validates: the guard that rejects it is dead",
        [("if c.defaultMaxBudgetUSD < 0 {", "if false {")],
    ),

    # ---- the property that makes the seam usable at all ----
    Case(
        "the flag set goes back to ExitOnError: a bad argument takes the test binary down with it",
        [('flag.NewFlagSet("autoworker", flag.ContinueOnError)', 'flag.NewFlagSet("autoworker", flag.ExitOnError)')],
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
