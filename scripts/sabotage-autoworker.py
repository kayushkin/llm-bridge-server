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
        [("if err := qc.EnsureBoard(autoworkerQueueBoard, kanbanvocab.AutoworkerQueueColumns()); err != nil {",
          "if err := qc.EnsureBoard(kanbanBoardName, kanbanvocab.AutoworkerQueueColumns()); err != nil {")],
    ),
    Case(
        "the queue board is created with no columns: nothing can ever be queued on it",
        [("if err := qc.EnsureBoard(autoworkerQueueBoard, kanbanvocab.AutoworkerQueueColumns()); err != nil {",
          "if err := qc.EnsureBoard(autoworkerQueueBoard, nil); err != nil {")],
    ),
    Case(
        "the column a dispatched card moves to is dropped from the queue board's column set",
        [("if err := qc.EnsureBoard(autoworkerQueueBoard, kanbanvocab.AutoworkerQueueColumns()); err != nil {",
          "if err := qc.EnsureBoard(autoworkerQueueBoard, []string{kanbanvocab.ColumnQueued, kanbanvocab.ColumnBlocked, kanbanvocab.ColumnDone, kanbanvocab.ColumnFailed}); err != nil {")],
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

    # ================= dry run and the cooldown file =================
    #
    # Ported from 3fec1ffd and e0f686a5. On the branch the first three rows were
    # declared unnoticed because the guards sat in main() with no seam; main's
    # run(cfg, publish) is that seam, and dryrun_test.go pins them through it.
    Case(
        "dry-run publishes and updates state, so a rehearsal spends money",
        [("\t\tif !d.fire || cfg.dryRun {", "\t\tif !d.fire {")],
    ),
    Case(
        "dry-run writes the cooldown state, so a rehearsal suppresses the next real fire",
        [("\tif !cfg.dryRun {\n\t\tif err := saveState(cfg.statePath, st); err != nil {",
          "\tif true {\n\t\tif err := saveState(cfg.statePath, st); err != nil {")],
    ),
    Case(
        "dry-run bootstraps the queue board at the call site in run()",
        [("initQueueClient(cfg.kanbanStoreURL, cfg.kanbanStoreServiceToken, cfg.dryRun)",
          "initQueueClient(cfg.kanbanStoreURL, cfg.kanbanStoreServiceToken, false)")],
    ),
    Case(
        "a real tick never saves the cooldown, so every tick fires another session",
        [("\tif !cfg.dryRun {\n\t\tif err := saveState(cfg.statePath, st); err != nil {",
          "\tif false {\n\t\tif err := saveState(cfg.statePath, st); err != nil {")],
    ),
    Case(
        "a missing state file is an error instead of an empty state, so a fresh install never fires",
        [("\tif os.IsNotExist(err) {\n\t\treturn &state{LastFire: map[string]int64{}}, nil\n\t}",
          "\tif os.IsNotExist(err) {\n\t\treturn nil, err\n\t}")],
    ),
    Case(
        "a state file with a null map is not repaired, so the first fire panics on write",
        [("\tif st.LastFire == nil {\n\t\tst.LastFire = map[string]int64{}\n\t}\n\treturn &st, nil",
          "\treturn &st, nil")],
    ),
    Case(
        "a corrupt state file loads as an empty one, so every tick fires",
        [("\tif err := json.Unmarshal(data, &st); err != nil {\n\t\treturn nil, err\n\t}\n\tif st.LastFire == nil",
          "\t_ = json.Unmarshal(data, &st)\n\tif st.LastFire == nil")],
    ),
    Case(
        "the cooldown state is written somewhere the next tick does not read",
        [("os.WriteFile(path, data, 0644)", 'os.WriteFile(path+".tmp", data, 0644)')],
    ),

    # ================= the queue, the spend ceiling, publish, limits, register =================
    #
    # Ported from e0f686a5, 4e4a0b93 and 145c5bae: the branch's rows re-aimed
    # at main's code. Main names the kanban vocabulary with its own constants
    # (autoworkerQueueColumnFrom, "session") rather than internal/kanbanvocab.

    # -- the hold gate: the one place the queue path re-applies it --
    Case(
        "the hold gate stops firing, so a card the user parked is dispatched anyway",
        [('if heldAt, _ := c.Item["held_at"].(string); heldAt != "" {',
          'if heldAt, _ := c.Item["held_at"].(string); heldAt == "" && false {')],
    ),
    Case(
        "the hold gate reads a key nothing writes, so every card looks unheld",
        [('c.Item["held_at"].(string); heldAt != ""', 'c.Item["hold_at"].(string); heldAt != ""')],
    ),
    Case(
        "a held card is skipped but the loop returns it anyway instead of moving on",
        [('log.Printf("queue: skip card %s — HELD since %s (%s)", c.Placement.CardID, heldAt, reason)\n\t\t\tcontinue',
          'log.Printf("queue: skip card %s — HELD since %s (%s)", c.Placement.CardID, heldAt, reason)')],
    ),

    # -- selection --
    Case(
        "a card with no noteboard item is dispatched, so the prompt has no task in it",
        [("if c.Item == nil {", "if c.Item == nil && false {")],
        expected_unnoticed="a behavioural no-op, not a coverage hole. Indexing a nil map in "
                           "Go yields the zero value rather than panicking, so a card with no "
                           "item has an empty title and the NEXT guard skips it for that "
                           "reason instead. Only the log line tells the two guards apart",
    ),
    Case(
        "a card with a blank title is dispatched",
        [('if strings.TrimSpace(title) == "" {', 'if strings.TrimSpace(title) == "" && false {')],
    ),
    Case(
        "selection reads the wrong column, so anything already In Progress is re-dispatched",
        [("if cv.Column.Name == autoworkerQueueColumnFrom {",
          "if cv.Column.Name == autoworkerQueueColumnInProgress {")],
    ),
    Case(
        "a card over its ceiling is skipped but never held, so it is re-examined every tick forever",
        [('if err := qc.HoldCard(cand.cardID, "spend ceiling reached"); err != nil {',
          'if err := error(nil); err != nil {')],
    ),
    Case(
        "the resolved remaining budget is dropped, so a capped card dispatches uncapped",
        [("\t\tcand.maxBudgetUSD = budget\n\t\treturn cand, nil", "\t\t_ = budget\n\t\treturn cand, nil")],
    ),

    # -- the spend ceiling: the money path --
    Case(
        "a spent-out ceiling still dispatches, because the remaining check drifts",
        [("if remaining <= 0 {", "if remaining < -1000 {")],
    ),
    Case(
        "spend is counted over the CARD's subtree, not the ceiling owner's, so each sibling gets the whole pot",
        [("spent, err := tree.SubtreeSpendUSD(ownerID, sessionsForItem(qc), cost)",
          "spent, err := tree.SubtreeSpendUSD(cand.cardID, sessionsForItem(qc), cost)")],
    ),
    Case(
        "already-spent money stops being subtracted, so every session gets the full ceiling again",
        [("remaining := ceiling - spent", "remaining := ceiling - spent*0")],
    ),
    Case(
        "an unreadable spend tree dispatches a CAPPED card uncapped instead of holding it",
        [("if _, hasOwn := cardSpendCeiling(cand.item); !hasOwn {",
          "if _, hasOwn := cardSpendCeiling(cand.item); hasOwn || !hasOwn {")],
    ),
    Case(
        "an unreadable spend tree holds work that never had a ceiling to honour",
        [("\t\t\treturn 0, true\n\t\t}\n\t\tlog.Printf(\"queue: card %s is capped but the spend tree is unreadable",
          "\t\t\treturn 0, false\n\t\t}\n\t\tlog.Printf(\"queue: card %s is capped but the spend tree is unreadable")],
    ),
    Case(
        "an unreadable SPEND (tree fine, links not) dispatches uncapped instead of holding",
        [("\t\t\tcand.cardID, ceiling, err)\n\t\treturn 0, false", "\t\t\tcand.cardID, ceiling, err)\n\t\treturn 0, true")],
    ),
    Case(
        "the ancestor walk is skipped, so an inherited ceiling stops applying to a child",
        [("ceiling, ownerID, ok := tree.EffectiveCeiling(cand.cardID)",
          "ceiling, ownerID, ok := tree.EffectiveCeiling(cand.cardID)\n\tif ownerID != cand.cardID {\n\t\treturn 0, true\n\t}")],
    ),
    Case(
        "held ancestors drop out of the tree, so a held parent's ceiling stops covering its children",
        [('noteboardURL + "/api/items?include_held=true&limit=10000"', 'noteboardURL + "/api/items?limit=10000"')],
    ),
    Case(
        "the item page shrinks to noteboard's default, so most of the parent chain is missing",
        [("include_held=true&limit=10000", "include_held=true&limit=50")],
    ),
    Case(
        "the parent chain is dropped, so a child never inherits its ancestor's ceiling",
        [("\t\tif it.ParentID != nil {\n\t\t\tparent = *it.ParentID\n\t\t}", "\t\t_ = it.ParentID")],
    ),
    Case(
        "a bridge that cannot report spend is read as a bridge reporting no spend",
        [("\tif resp.StatusCode != http.StatusOK {\n\t\treturn nil, fmt.Errorf(\"bridge aggregates: %s\", resp.Status)",
          "\tif resp.StatusCode != http.StatusOK && false {\n\t\treturn nil, fmt.Errorf(\"bridge aggregates: %s\", resp.Status)")],
    ),
    Case(
        "the spend read asks llm-bridge for a route it does not serve",
        [('bridgeURL + "/sessions/aggregates"', 'bridgeURL + "/sessions/aggregate"')],
    ),
    Case(
        "a card's own ceiling of null reads as a ceiling of zero",
        [("\tif !ok || raw == nil {\n\t\treturn 0, false\n\t}", "\tif !ok {\n\t\treturn 0, false\n\t}")],
        expected_unnoticed="a behavioural no-op. A JSON null decodes to a nil interface, which "
                           "fails the float64 assertion two lines down and returns the same "
                           "(0, false); no test can tell the two spellings apart",
    ),
    Case(
        # 4e4a0b93: the fixture must give the non-session link a cost of its own,
        # or the sum deduplicates and the mutation walks through.
        "links of every kind are counted as sessions, so unrelated links inflate the spend",
        [('if l.EntityType == kanbanvocab.LinkTypeSession {', 'if l.EntityType == kanbanvocab.LinkTypeSession || l.EntityType != "" {')],
    ),

    # -- resolveSessionBudget precedence --
    Case(
        "the fleet default overrides the card's own ceiling",
        [("if cand != nil && cand.maxBudgetUSD > 0 {\n\t\treturn cand.maxBudgetUSD,",
          "if cand != nil && cand.maxBudgetUSD > 0 && fleetDefaultUSD == 0 {\n\t\treturn cand.maxBudgetUSD,")],
    ),

    # -- applySessionSpendCeiling: both enforcers, never a zero --
    Case(
        "only the harness-native ceiling is written, so codex sessions run uncapped",
        [("\tcreate.MaxBudget = &maxBudgetUSD\n", "")],
    ),
    Case(
        "only the server-side ceiling is written, so Claude Code never stops itself mid-call",
        [("\tcreate.HarnessConfig = harnessConfig\n", "\t_ = harnessConfig\n")],
    ),
    Case(
        "a zero ceiling is written through, which both enforcers read as UNLIMITED",
        [("if maxBudgetUSD <= 0 {", "if maxBudgetUSD < 0 {")],
    ),
    Case(
        "the harness-native ceiling is spelled with a key llm-bridge-claudecode does not read",
        [('map[string]any{"max_budget_usd": maxBudgetUSD}', 'map[string]any{"maxBudgetUSD": maxBudgetUSD}')],
    ),

    # -- the dispatched prompt and the session it mints --
    Case(
        "the queue prompt loses the card id, so the worker cannot close what it was given",
        [("\t\tc.cardID, c.title, body, c.cardID)", "\t\t\"\", c.title, body, \"\")")],
    ),
    Case(
        "the queue prompt loses the card body, so only the title survives",
        [("\tbody := strings.TrimSpace(c.body)", "\tbody := \"\"")],
    ),
    Case(
        "an empty card body renders as nothing instead of saying so",
        [('body = "(no body — see title)"', 'body = ""')],
    ),
    Case(
        "the minted session id loses the nanosecond, so two fires in one second collide",
        [('fmt.Sprintf("autoworker-%s-%d", p.Name, now.UnixNano())',
          'fmt.Sprintf("autoworker-%s-%d", p.Name, now.Unix())')],
    ),
    Case(
        "the published session is not marked autonomous, so it is filed as a session a human is watching",
        [("Type:        msg.SessionTypeAutonomous,", "Type:        msg.SessionTypeInteractive,")],
    ),
    Case(
        "the session id published on the bus is not the one returned, so the kanban link points at nothing",
        [("\t\treturn \"\", err\n\t}\n\treturn sessionID, nil", "\t\treturn \"\", err\n\t}\n\treturn sessionID + \"-published\", nil")],
    ),

    # -- fetchLimits: the numbers every fire is decided on --
    Case(
        "usage-store answering an error is read as a set of limits, so the tick fires on nothing",
        [("\tif resp.StatusCode != 200 {\n\t\treturn nil, fmt.Errorf(\"usage-store returned %d: %s\", resp.StatusCode, string(body))",
          "\tif resp.StatusCode != 200 && false {\n\t\treturn nil, fmt.Errorf(\"usage-store returned %d: %s\", resp.StatusCode, string(body))")],
    ),
    Case(
        "the limits read asks for a route usage-store does not serve",
        [('url := usageStoreURL + "/api/usage/limits"', 'url := usageStoreURL + "/api/usage/limit"')],
    ),
    Case(
        "a 200 carrying something other than limits is read as a set of limits",
        [("\tif err := json.Unmarshal(body, &lim); err != nil {\n\t\treturn nil, fmt.Errorf(\"unmarshal: %w\", err)",
          "\tif err := json.Unmarshal(body, &lim); err != nil && false {\n\t\treturn nil, fmt.Errorf(\"unmarshal: %w\", err)")],
    ),

    # -- markQueueCardDispatched: recording that the fire happened --
    Case(
        "a dispatched card is not moved out of Queued, so the next tick dispatches it again",
        [("inProgressID, ok := qc.ColumnsByName[autoworkerQueueColumnInProgress]",
          "inProgressID, ok := qc.ColumnsByName[autoworkerQueueColumnFrom]")],
    ),
    Case(
        "the dispatch attaches a link the spend tree does not count, so the session is free",
        [('qc.AddCardLink(cand.cardID, kanbanvocab.LinkTypeSession, sessionID, kanbanCardOrigin)',
          'qc.AddCardLink(cand.cardID, "autoworker_provider", sessionID, kanbanCardOrigin)')],
    ),
    Case(
        "the dispatch note replaces the card body instead of appending, destroying the task",
        [("qc.PatchCardBody(cand.cardID, cand.body+note)", "qc.PatchCardBody(cand.cardID, note)")],
    ),
    Case(
        "a session link that could not be attached is reported as a clean dispatch",
        [("\tif err := qc.AddCardLink(cand.cardID, kanbanvocab.LinkTypeSession, sessionID, kanbanCardOrigin); err != nil {\n\t\treturn fmt.Errorf(\"add session link: %w\", err)\n\t}",
          "\t_ = qc.AddCardLink(cand.cardID, kanbanvocab.LinkTypeSession, sessionID, kanbanCardOrigin)")],
    ),
    Case(
        "a card that could not be moved out of Queued is reported as a clean dispatch",
        [("\tmoveResult, err := qc.MoveCard(cand.cardID, qc.BoardID, inProgressID, 0)\n\tif err != nil {\n\t\treturn fmt.Errorf(\"move to In Progress: %w\", err)\n\t}",
          "\tmoveResult, _ := qc.MoveCard(cand.cardID, qc.BoardID, inProgressID, 0)")],
    ),
    Case(
        "the fire is stamped on the local clock, so the audit trail cannot be lined up with the bridge",
        [("now.UTC().Format(time.RFC3339)", "now.Format(time.RFC3339)")],
    ),
    Case(
        "the log id for a fire with no queue card renders empty instead of naming the absence",
        [('\tif c == nil {\n\t\treturn "(none)"', '\tif c == nil {\n\t\treturn ""')],
    ),

    # -- -register: how this binary gets onto cron at all --
    Case(
        "-register registers under a name a later -register will not recognise",
        [('"name":        "autoworker-tick",', '"name":        "autoworker-tick-v2",')],
    ),
    Case(
        "-register is no longer idempotent, so every deploy adds another dispatcher",
        [('if j.Name == "autoworker-tick" {', 'if j.Name == "autoworker-tick" && false {')],
    ),
    Case(
        "the idempotence check matches loosely, so a neighbouring job reads as proof this one is registered",
        [('if j.Name == "autoworker-tick" {', 'if strings.HasPrefix(j.Name, "autoworker") {')],
    ),
    Case(
        "the tick is registered on a cadence the gates were not written for",
        [('"schedule":    "*/30 * * * *",', '"schedule":    "*/5 * * * *",')],
    ),
    Case(
        "a scheduler that refuses the job is reported as a successful registration",
        [("\tif resp.StatusCode >= 300 {\n\t\treturn fmt.Errorf(\"scheduler returned %d: %s\", resp.StatusCode, string(body))",
          "\tif resp.StatusCode >= 300 && false {\n\t\treturn fmt.Errorf(\"scheduler returned %d: %s\", resp.StatusCode, string(body))")],
    ),

    # -- three guards from b305c9de's cross-table: a fresh snapshot missing its
    # -- window, or its resets_at, is a shortfall with its own cause; an empty
    # -- queue is a quiet night, not an error.
    Case(
        "a window with no resets_at is scored anyway, dividing the curve by a zero anchor",
        [("\tif win.ResetsAt == nil {\n\t\td.reason = \"weekly window has no resets_at — skip\"\n\t\td.shortfall = fmt.Sprintf(\"%s was never evaluated: its %q window has no resets_at, so the elapsed fraction is uncomputable\", p.Name, p.WindowKey)\n\t\treturn d\n\t}",
           "\tif win.ResetsAt == nil {\n\t\tzero := int64(0)\n\t\twin.ResetsAt = &zero\n\t}")],
    ),
    Case(
        "a snapshot with no seven-day window at all is scored on an empty one, so the report names the wrong reason and the operator chases the wrong bug",
        [("\tif !ok || win == nil {\n\t\td.reason = fmt.Sprintf(\"weekly window %q not in snapshot — skip\", p.WindowKey)\n\t\td.shortfall = fmt.Sprintf(\"%s was never evaluated: its snapshot is fresh but carries no %q window\", p.Name, p.WindowKey)\n\t\treturn d\n\t}",
           "\tif !ok || win == nil {\n\t\twin = &limitWindow{}\n\t}\n\t_ = ok")],
    ),
    Case(
        "an empty queue is an error rather than a quiet night, so a tick with nothing to do reports a broken one",
        [("\tif len(queued) == 0 {\n\t\tlog.Printf(\"queue: %q column empty\", autoworkerQueueColumnFrom)\n\t\treturn nil, nil\n\t}",
           "\tif len(queued) == 0 {\n\t\treturn nil, fmt.Errorf(\"queue: %q column empty\", autoworkerQueueColumnFrom)\n\t}")],
    ),

    # ---- boundary VALUES ------------------------------------------------------
    #
    # Ported from df8f26f8 (branch test/autoworker-boundary-values-are-pinned).
    # Its port and flag-default rows are not carried: those values now live in
    # parseFlags and internal/config, and flag_defaults_test.go pins them.
    #
    # Every case above this line asks whether a MECHANISM is observed. None of
    # them asks whether the NUMBER it compares against is the number anybody
    # chose. The list scored 68/73 with both controls behaving and printed "every
    # real mechanism is pinned" — truthfully — while 29 of the 36 literals below
    # could each move one unit with the suite green.
    #
    # Rules these rows are written to, and they are not interchangeable:
    #
    #   - Move the literal by ONE unit. A far move (22h → 18h, >= 3 → >= 300)
    #     proves the mechanism runs and pins the bound to a band, not a value.
    #   - Where a value is read by an INEQUALITY, the fixture pair has to straddle
    #     it by half a unit. A pair spaced a full unit apart catches the cut
    #     narrowing and misses it widening, because the far fixture then sits
    #     exactly ON the mutated boundary and the comparison's strictness decides
    #     it. Measured here: 14m/16m around a 15-minute cooldown scored UNNOTICED
    #     for 15m → 16m; 14m30s/15m30s catches both directions.
    #   - Where a value is QUOTED IN FULL by an equality (the curve's anchors), one row is enough: an adjacent move and a far move
    #     redden the same assertion, so a second direction scores one line twice.
    #
    # Expect insurance, not bugs. Every hole below was a correct value that
    # nothing was holding in place; cmd/autoworker/main.go and spend.go are
    # unchanged. This is not a defect count.

    # -- the shipped provider table. Every gate test in fire_test.go builds its
    # -- own providerSpec, on purpose, so that some other gate cannot decide the
    # -- outcome. Nothing read the two specs the binary actually runs on.
    Case(
        "anthropic's catch-up window loses its last minute, so the hour before the reset is shut",
        [("SafeEnd:      24 * 60, // 24:00 PT", "SafeEnd:      24*60 - 1, // 24:00 PT")],
    ),
    Case(
        "codex's overnight window runs a minute into the working day",
        [("SafeEnd:      6 * 60, // 06:00 PT", "SafeEnd:      6*60 + 1, // 06:00 PT")],
    ),
    Case(
        "codex's overnight window closes a minute early",
        [("SafeEnd:      6 * 60, // 06:00 PT", "SafeEnd:      6*60 - 1, // 06:00 PT")],
    ),
    # Both directions kept for the cooldowns: narrowing one is the direction that
    # costs money, since the cooldown is the only thing between an under-used week
    # and a stream of sessions.
    Case(
        "anthropic's cooldown widens, so a week that should catch up dispatches less often",
        [("Cooldown:     15 * time.Minute,", "Cooldown:     16 * time.Minute,")],
    ),
    Case(
        "anthropic's cooldown narrows, so sessions come closer together than the design allows",
        [("Cooldown:     15 * time.Minute,", "Cooldown:     14 * time.Minute,")],
    ),
    Case(
        "codex's cooldown widens past the two hours it was given",
        [("Cooldown:     2 * time.Hour,", "Cooldown:     2*time.Hour + time.Minute,")],
    ),
    Case(
        "codex's cooldown narrows, so the overnight window fits an extra session",
        [("Cooldown:     2 * time.Hour,", "Cooldown:     2*time.Hour - time.Minute,")],
    ),

    # -- the target curve. The four existing curve tests assert that it starts
    # -- flat, rises, interpolates rather than snapping, and clamps at both ends.
    # -- All four hold with every percentage on it moved a point.
    Case(
        "the curve's flat opening swallows another day, so the week starts budgeting late",
        [("{0.286, 0},", "{0.287, 0},")],
    ),
    Case(
        "the curve's flat opening ends early, so an untouched week is already behind",
        [("{0.286, 0},", "{0.285, 0},")],
    ),
    Case(
        "the day-3 anchor asks for a point more budget than it was given",
        [("{0.429, 25},", "{0.429, 26},")],
    ),
    Case(
        "the day-3 anchor moves later in the week",
        [("{0.429, 25},", "{0.430, 25},")],
    ),
    Case(
        "the day-4 anchor asks for a point more budget than it was given",
        [("{0.571, 50},", "{0.571, 51},")],
    ),
    Case(
        "the day-5 anchor asks for a point more budget than it was given",
        [("{0.714, 70},", "{0.714, 71},")],
    ),
    Case(
        "the day-6 anchor asks for a point more budget than it was given",
        [("{0.857, 85},", "{0.857, 86},")],
    ),

    # -- elapsedFraction's clamp, which keeps the curve's domain closed
    Case(
        "the clamp lands short of the curve's last fraction, so the reset reads as an earlier day",
        [("return 0.999", "return 0.998")],
    ),
    Case(
        "the clamp stops applying at the instant the window ends, so the fraction reads 1.0",
        [("if elapsed >= weekSec {", "if elapsed > weekSec {")],
    ),

    # -- the fire gate
    Case(
        "usage level with the band edge fires, so the quietest possible week dispatches",
        [("if d.actual >= d.target-slackPP {", "if d.actual > d.target-slackPP {")],
    ),

    # -- the money. Both of these hand the harness a cap of 0, which
    # -- llm-bridge-claudecode omits from the command line and the CLI reads as
    # -- UNLIMITED: the tightest imaginable ceiling and no ceiling are one wire
    # -- value. The existing spent-out fixture overshoots its ceiling by a dollar,
    # -- so it never reached the input that separates "spent" from "exactly spent".
    Case(
        "a card that has spent EXACTLY its ceiling is dispatched, at a cap of 0, which means uncapped",
        [("\tif remaining <= 0 {", "\tif remaining < 0 {")],
    ),
    Case(
        "an unarmed fleet default reports itself as an armed one, so an uncapped session reads as capped",
        [("\tif fleetDefaultUSD > 0 {", "\tif fleetDefaultUSD >= 0 {")],
    ),

    # -- the dispatched card's landing position, which the recorder captured all
    # -- along and no assertion read
    Case(
        "a dispatched card lands somewhere other than the head of In Progress",
        [("qc.MoveCard(cand.cardID, qc.BoardID, inProgressID, 0)",
          "qc.MoveCard(cand.cardID, qc.BoardID, inProgressID, 1)")],
    ),

    # -- the fire path's announcement of the cap. The C3 branch declared this row
    # -- unreachable because the line sat inside main()'s tick loop; main has since
    # -- become run(cfg, publish), and TestTheFirePathAnnouncesAnUncappedSessionAsUncapped
    # -- reaches it.
    Case(
        "the fire path logs an uncapped session as a capped one",
        [("\t\tif maxBudgetUSD > 0 {", "\t\tif maxBudgetUSD >= 0 {")],
    ),

    # -- declared: values this suite genuinely cannot see
    Case(
        "the noteboard read's timeout drifts by a second",
        [("http.Client{Timeout: 30 * time.Second}", "http.Client{Timeout: 31 * time.Second}")],
        expected_unnoticed="an httptest server answers in microseconds, so no fixture can tell "
                           "a 30-second client timeout from a 31-second one. A fixture that "
                           "could would have to sleep for thirty seconds, trading a real "
                           "half-minute of suite time for a bound that only matters when "
                           "noteboard hangs. Falsified by a fake clock on the http client",
    ),
    Case(
        "the bridge read's timeout drifts by a second",
        [("http.Client{Timeout: 15 * time.Second}", "http.Client{Timeout: 16 * time.Second}")],
        expected_unnoticed="the same reason as the noteboard timeout above",
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
