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
rediscovered: every case in the first three families edits configuration — the
flag set, the endpoint wiring, the two home-relative paths and the spend-cap
guard. The tick's decision logic (evaluate, interpolateTarget, insideSafeWindow,
the cooldown comparison) and the rest of the kanban queue path (selectQueuedCard,
buildQueuePrompt, markQueueCardDispatched) are NOT sabotaged here. That is the
seam the 197th pass opened and nothing more; it is not a statement that the rest
is covered.

---- 2026-08-15, 217th pass: one mechanism added, and the scope above widens by
exactly that much ----

initQueueClient is now sabotaged, and it was excluded above under "the kanban
queue path". The exclusion is narrowed rather than dropped: its three siblings
are still not scored here.

Why it was worth adding, and this is a measurement rather than a judgement: on
2026-08-15 an unconditional `panic()` at initQueueClient's first line left
`go test ./...` GREEN across the whole repository (card e9b0b89c, the 202nd
pass). Nothing executed the function that decides whether an unattended tick may
WRITE to kanban-store.

Its mechanism is `dryRun`, and the value never exercised was the one that
suppresses writes. It fails two ways that look alike from outside:

  - a dry run that bootstraps the board is a switch that does not switch;
  - a dry run that writes nothing AND resolves nothing returns nil, which is
    also what a fresh install returns. The caller falls back to the default
    prompt in both, so the difference reaches nobody except through the log.

Three separate conditions answer nil here — board absent, boards unreadable,
columns unreadable — and the log line is the only thing that tells them apart.
So the log lines ARE the interface on those paths, and the rows below treat them
as behaviour. ⚠️ That is why the known-negative control for this family rewords
prose while KEEPING the `%v` that carries the error: a control that reworded a
line the tests read would be expired on arrival, which is exactly what
sabotage-email-classifier-sweep.py predicted when it wrote "falsified by a test
that captures log output".

Score after the family was added: 29/29 real mechanisms caught, both controls
behaved, exit 0. The reach guard was re-taken by hand first — `panic()` at
initQueueClient's first line now reddens cmd/autoworker, where in the 202nd
pass's measurement it reddened nothing.

⚠️ One row was UNNOTICED on the first run, and it is the most reusable thing
here: dropping "In Progress" from kanbanDefaultColumns changed nothing the suite
could see, because the test ranged over that same constant to build its
expectation. The expectation moved with the code. A test that reads the constant
it is meant to pin asserts only that the constant equals itself. The five names
are now spelled out literally, and a second test ties the set to the two column
constants the binary later addresses by name — which is the invariant that was
actually at risk, since those live in a different declaration entirely.

---- 2026-09-25: evaluate is scored; six dead rows re-aimed or deleted ----

The region note above no longer holds for evaluate, elapsedFraction, interpolate,
inSafeWindow or the cooldown comparison. fire_test.go, ported from e0f686a5,
pins them, and the family at the foot of CASES scores them: the same 41 cases
were 30/41 on main before the port and 41/41 after. selectQueuedCard,
buildQueuePrompt and markQueueCardDispatched are still not scored here.

Main moved this binary's configuration into internal/config's settings
registry, so six rows named code that no longer existed and the whole file
aborted (card 2d1112ca). Four are re-aimed at parseFlags, where the registry is
read; two are deleted because what they edited now lives in llm-bridge's
servicesettings, outside both targets.
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
        # Re-aimed 2026-09-25: the default moved into internal/config's settings
        # registry, so the drift that can happen HERE is the flag not reading it.
        [('settings.Decimal(schedulerconfig.SettingDefaultMaxBudgetUSD),', '5,')],
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
        [('c.llmBridgeURL = settings.String(schedulerconfig.SettingBridgeURL)',
          'c.llmBridgeURL = settings.String(schedulerconfig.SettingNoteboardURL)')],
    ),
    Case(
        "usageStoreURL points at kanban-store's port: the limits fetch that gates every fire",
        [('c.usageStoreURL = settings.String(schedulerconfig.SettingUsageStoreURL)',
          'c.usageStoreURL = settings.String(schedulerconfig.SettingKanbanStoreURL)')],
    ),
    # Deleted 2026-09-25: "envOr prefers its fallback over the environment".
    # envOr is gone; which of the environment and the default wins is decided
    # by llm-bridge's servicesettings, outside both targets.
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
    # Deleted 2026-09-25: "a malformed spend cap silently becomes the default".
    # The number is parsed by servicesettings now, outside both targets; the
    # half of the refusal that lives here is the sentinel, below.
    Case(
        "a malformed setting loses its sentinel: main exits 2 where it used to exit 1",
        [('return config{}, fmt.Errorf("%w: %v", errMalformedEnvironmentValue, err)',
          'return config{}, fmt.Errorf("%v", err)')],
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

    # ================= initQueueClient: may this tick write? =================
    #
    # Added by the 217th pass. See the note at the foot of the docstring for why
    # the log lines are treated as behaviour in this family and nowhere else.
    Case(
        "CONTROL known-negative: reword the bootstrap failure line, keeping the error it carries",
        [('log.Printf("queue: bootstrap %q: %v (using default prompt)", autoworkerQueueBoard, err)',
          'log.Printf("queue: could not bootstrap %q: %v — falling back to the default prompt", autoworkerQueueBoard, err)')],
        expected_unnoticed="prose around an error the tests DO read. The outage test asserts the "
                           "store's failure reaches the log and that it is not mistaken for an "
                           "absent board; neither depends on the wording around %v. Falsified by a "
                           "test that matches this line literally, which would pin prose instead of "
                           "the error.",
    ),

    # -- the switch itself --
    Case(
        "the dry-run gate inverts: a dry run bootstraps the board and a real tick refuses to",
        [("\tqc := kanbanclient.New(kanbanStoreURL, kanbanStoreServiceToken)\n\tif dryRun {",
          "\tqc := kanbanclient.New(kanbanStoreURL, kanbanStoreServiceToken)\n\tif !dryRun {")],
    ),
    Case(
        "the dry-run gate is dead: -dry-run creates the queue board on a fresh install",
        [("\tqc := kanbanclient.New(kanbanStoreURL, kanbanStoreServiceToken)\n\tif dryRun {",
          "\tqc := kanbanclient.New(kanbanStoreURL, kanbanStoreServiceToken)\n\tif false {")],
    ),

    # -- a dry run that resolves nothing prints as a fresh install --
    Case(
        "the dry run stops recording the board id: every later lookup addresses no board",
        [("\t\tqc.BoardID = board.ID\n\t\tcols, err := qc.ListColumns(board.ID)",
          "\t\tcols, err := qc.ListColumns(board.ID)")],
    ),
    Case(
        "the board id is set to the board NAME: kanban-store is addressed by a label, not an id",
        [("\t\tqc.BoardID = board.ID\n", "\t\tqc.BoardID = board.Name\n")],
    ),
    Case(
        "the dry run resolves no columns: it reports an empty board and a full one identically",
        [("\t\tfor _, col := range cols {\n\t\t\tqc.ColumnsByName[col.Name] = col.ID\n\t\t}",
          "\t\tfor range cols {\n\t\t}")],
    ),
    Case(
        "the column map is keyed by id: every lookup by name misses on a board that has the column",
        [("\t\t\tqc.ColumnsByName[col.Name] = col.ID", "\t\t\tqc.ColumnsByName[col.ID] = col.ID")],
    ),

    # -- the three nils, and telling them apart --
    Case(
        "an absent board is reported as an outage: the operator is sent to a service that is up",
        [('log.Printf("queue: board %q does not exist yet — first non-dry-run will bootstrap it", autoworkerQueueBoard)',
          'log.Printf("queue: board %q unavailable", autoworkerQueueBoard)')],
    ),
    Case(
        "an unreadable board list is swallowed: a half-built client is handed to the dispatch path",
        [('log.Printf("queue: find board %q: %v", autoworkerQueueBoard, err)\n\t\t\treturn nil',
          'log.Printf("queue: find board %q: %v", autoworkerQueueBoard, err)\n\t\t\treturn qc')],
    ),
    Case(
        "unreadable columns are swallowed: the caller gets a board id and an empty column map",
        [('log.Printf("queue: list columns: %v", err)\n\t\t\treturn nil',
          'log.Printf("queue: list columns: %v", err)\n\t\t\treturn qc')],
    ),
    Case(
        "a failed bootstrap returns a client anyway: a kanban-store outage becomes a dispatch against nothing",
        [("\t\tlog.Printf(\"queue: bootstrap %q: %v (using default prompt)\", autoworkerQueueBoard, err)\n\t\treturn nil",
          "\t\tlog.Printf(\"queue: bootstrap %q: %v (using default prompt)\", autoworkerQueueBoard, err)\n\t\treturn qc")],
    ),

    # -- which board, and with what on it --
    Case(
        "the dry run resolves the telemetry board: -dry-run previews cards autoworker itself wrote",
        [("board, err := qc.FindBoard(autoworkerQueueBoard)", "board, err := qc.FindBoard(kanbanBoardName)")],
    ),
    Case(
        "the real run bootstraps the telemetry board: dispatch reads the board it writes to",
        [("if err := qc.EnsureBoard(autoworkerQueueBoard, kanbanDefaultColumns); err != nil {",
          "if err := qc.EnsureBoard(kanbanBoardName, kanbanDefaultColumns); err != nil {")],
    ),
    Case(
        "the queue board is created with no columns: nothing can ever be queued on it",
        [("if err := qc.EnsureBoard(autoworkerQueueBoard, kanbanDefaultColumns); err != nil {",
          "if err := qc.EnsureBoard(autoworkerQueueBoard, nil); err != nil {")],
    ),
    Case(
        "the column a dispatched card moves to is dropped from the default set",
        [('var kanbanDefaultColumns = []string{"Queued", "In Progress", "Blocked", "Done", "Failed"}',
          'var kanbanDefaultColumns = []string{"Queued", "Blocked", "Done", "Failed"}')],
    ),

    # ================= evaluate: usage against the reset curve =================
    #
    # Ported from e0f686a5 (test/first-tests-for-autoworker), whose fire_test.go
    # compiled on main unchanged. These are the rows the region note in the
    # docstring excluded: evaluate, the curve, the safe window and the cooldown.
    Case(
        "the slack gate inverts, so it fires only when usage is ALREADY above target",
        [("if d.actual >= d.target-slackPP {", "if d.actual < d.target-slackPP {")],
    ),
    Case(
        "slack is ignored, so a session fires for a hair under target on every tick",
        [("if d.actual >= d.target-slackPP {", "if d.actual >= d.target {")],
    ),
    Case(
        "the cooldown stops applying, so every tick fires another session",
        [("if time.Since(time.Unix(last, 0)) < p.Cooldown {",
          "if time.Since(time.Unix(last, 0)) < 0 {")],
    ),
    Case(
        "the safe window stops applying, so codex fires in the middle of the working day",
        [("if !inSafeWindow(p, nowLocal) {", "if false && !inSafeWindow(p, nowLocal) {")],
    ),
    Case(
        "the safe window's end is inclusive, so a window ending at 06:00 fires at 06:00",
        [("return minutesOfDay >= p.SafeStart && minutesOfDay < p.SafeEnd",
          "return minutesOfDay >= p.SafeStart && minutesOfDay <= p.SafeEnd")],
    ),
    Case(
        "the safe window's start is exclusive, so a window opening at 22:00 stays shut at 22:00",
        [("return minutesOfDay >= p.SafeStart && minutesOfDay < p.SafeEnd",
          "return minutesOfDay > p.SafeStart && minutesOfDay < p.SafeEnd")],
    ),
    Case(
        "the weekday-only window fires at weekends too",
        [("if wd == time.Saturday || wd == time.Sunday {", "if wd == time.Sunday && false {")],
    ),
    Case(
        "a stale snapshot is treated as current, so it budgets against numbers from days ago",
        [("if lim.StaleAfter != nil && time.Now().Unix() > *lim.StaleAfter {",
          "if lim.StaleAfter != nil && time.Now().Unix() > *lim.StaleAfter+86400 {")],
    ),
    Case(
        "a missing provider snapshot fires instead of skipping",
        [("\tif lim == nil {\n\t\td.reason = \"no snapshot available — skip\"\n\t\treturn d\n\t}",
          "\tif lim == nil {\n\t\td.reason = \"no snapshot available — skip\"\n\t\td.fire = true\n\t\treturn d\n\t}")],
    ),
    Case(
        "elapsedFraction measures from the wrong end, so a fresh week reads as a spent one",
        [("windowStart := resetsAt - weekSec", "windowStart := resetsAt + weekSec")],
    ),
    Case(
        # e0f686a5 declared this expected_unnoticed; 4acd749e struck the
        # declaration once TestElapsedFractionStaysInsideTheCurvesDomain existed.
        "elapsedFraction returns 1.0 at the boundary, so the curve reads past its last anchor",
        [("\t\treturn 0.999", "\t\treturn 1.0")],
    ),
    Case(
        "the curve's quiet opening is deleted, so day 1 of the week already budgets",
        [("\t{0.286, 0},  // ~day 2    → still blank", "\t{0.286, 40}, // ~day 2    → still blank")],
    ),
    Case(
        # `return a.pct` alone orphans b and t and reports a compile error.
        "interpolate returns the segment's lower anchor, so target lags a whole day behind",
        [("return a.pct + t*(b.pct-a.pct)", "return a.pct + 0*t*(b.pct-a.pct)")],
    ),
    Case(
        "interpolate reads the segment above the fraction, so target runs a day ahead",
        [("if frac <= targetCurve[i].frac {", "if frac <= targetCurve[i-1].frac {")],
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
