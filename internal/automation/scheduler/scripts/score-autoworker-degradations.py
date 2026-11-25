#!/usr/bin/env python3
"""Score cmd/autoworker's degradation tests by breaking the code they pin.

A green suite is a claim about a suite, not about the code, and this suite was
green on its first run -- which is exactly when it is worth nothing. This plants
one defect at a time and asks whether anything goes red.

Three outcomes, not two. A mutation that does not COMPILE exits non-zero exactly
as a red suite does, so a scorer reading only the exit code scores a build
failure as a catch and reports a test that tested nothing. Every arm here is
judged on the output as well: `build failed`, `undefined:`, `declared and not
used` and friends are ABORTED, and an ABORTED arm is a hole in the scorer, not a
win.

Both directions are armed, and on this command over-recording is the sharper
risk. Measured 2026-08-29 against the live scheduler: job 5 logs
`[codex] snapshot stale ... skip` on 100 of 100 retained runs, because the codex
snapshot has been stale for five days. An arm that turns that documented skip
into a shortfall must be CAUGHT, or job 5 reports failure every five minutes and
its exit code stops meaning anything.

The repository is resolved from __file__ and printed, so running this from one
checkout can never score another (card cdb2e1fc). Files are restored from bytes
captured in this process, never with `git checkout` -- a harness that restores
with git can only score COMMITTED work, and an uncommitted repair scores as a
repair that failed.
"""
import pathlib
import re
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
MAIN = REPO / "cmd" / "autoworker" / "main.go"
PKG = "./cmd/autoworker/"

# name -> (old, new, expectation). expectation "caught" = at least one test must
# go red; "survives" = the suite must stay green.
ARMS = [
    # ---------------------------------------------------- each Record silenced
    ("drop-the-missing-window-shortfall",
     '\t\td.shortfall = fmt.Sprintf("%s was never evaluated: its snapshot is fresh but carries no %q window", p.Name, p.WindowKey)\n',
     '',
     "caught"),
    ("drop-the-missing-resets-at-shortfall",
     '\t\td.shortfall = fmt.Sprintf("%s was never evaluated: its %q window has no resets_at, so the elapsed fraction is uncomputable", p.Name, p.WindowKey)\n',
     '',
     "caught"),
    ("drop-the-failed-publish-record",
     '\t\t\tdegraded.Record("%s passed every gate and no worker was dispatched: the publish failed (%v)", d.provider, err)\n',
     '',
     "caught"),
    ("drop-the-card-left-in-queued-record",
     '\t\t\t\tdegraded.Record("card %s was dispatched as session %s and the board was not updated (%v) — it is still in %q and can be dispatched again",\n\t\t\t\t\tqueueCard.cardID, sessionID, err, autoworkerQueueColumnFrom)\n',
     '',
     "caught"),
    ("drop-the-no-telemetry-card-record",
     '\t\t\t\tdegraded.Record("session %s was dispatched with no card on %q (%v), so no reviewer or curator will ever see it", sessionID, kanbanBoardName, err)\n',
     '',
     "caught"),
    ("drop-the-unreadable-queue-record",
     '\t\t\t\tdegraded.Record("the %s dispatch used the default prompt because the queue could not be read (%v)", d.provider, err)\n',
     '',
     "caught"),
    ("drop-the-unparked-ceiling-record",
     '\t\t\t\tif degraded != nil {\n\t\t\t\t\tdegraded.Record("card %s is over its spend ceiling and the hold was refused (%v), so it is skipped but not parked", cand.cardID, err)\n\t\t\t\t}\n',
     '',
     "caught"),
    ("drop-the-evaluate-shortfall-relay",
     '\t\tif d.shortfall != "" {\n\t\t\tdegraded.Record("%s", d.shortfall)\n\t\t}\n',
     '',
     "caught"),
    # Neutralised rather than deleted: removing the block leaves `why` declared
    # and not used, so the package stops compiling and exits non-zero for exactly
    # the reason a red suite does. Scored on two outcomes it read as the
    # strongest arm in the set while testing nothing. This is the three-outcomes
    # rule arriving from the mutation side (card cd03d6f3).
    ("drop-the-unreachable-queue-board-record",
     '\t\t\tif why != "" {\n\t\t\t\tdegraded.Record("%s", why)\n\t\t\t}\n',
     '\t\t\t_ = why\n',
     "caught"),
    ("drop-the-unsaved-cooldown-record",
     '\t\t\tdegraded.Record("the cooldown state was not saved (%v), so the next tick may dispatch again inside this one\'s cooldown", err)\n',
     '',
     "caught"),

    # ------------------------------------------------------- the verdict itself
    ("the-run-always-exits-zero",
     "\treturn degraded.ExitCode()\n}",
     "\t_ = degraded\n\treturn 0\n}",
     "caught"),
    ("only-the-first-shortfall-is-kept",
     "\treturn degraded.ExitCode()\n}",
     "\tif len(degraded.Reasons()) > 1 {\n\t\tdegraded = degradations.New(\"autoworker\")\n\t\tdegraded.Record(\"%s\", \"several things fell short\")\n\t}\n\treturn degraded.ExitCode()\n}",
     "caught"),

    # ---- a report that does not identify what it is about. This command logs a
    # line naming the same provider or card immediately beside every Record, so
    # an assertion reading the whole captured log would stay green on all of
    # these. That trap was measured on cmd/spend-ceiling-guard and again six
    # times over on cmd/kanban-curator, both on 2026-08-29.
    ("the-left-in-queued-report-does-not-name-the-card",
     'degraded.Record("card %s was dispatched as session %s and the board was not updated (%v) — it is still in %q and can be dispatched again",\n\t\t\t\t\tqueueCard.cardID, sessionID, err, autoworkerQueueColumnFrom)',
     'degraded.Record("a card was dispatched as session %s and the board was not updated (%v) — it is still in %q and can be dispatched again",\n\t\t\t\t\tsessionID, err, autoworkerQueueColumnFrom)',
     "caught"),
    ("the-left-in-queued-report-does-not-say-where-it-was-left",
     'and the board was not updated (%v) — it is still in %q and can be dispatched again",\n\t\t\t\t\tqueueCard.cardID, sessionID, err, autoworkerQueueColumnFrom)',
     'and the board was not updated (%v)",\n\t\t\t\t\tqueueCard.cardID, sessionID, err)',
     "caught"),
    ("the-no-telemetry-report-does-not-name-the-session",
     'degraded.Record("session %s was dispatched with no card on %q (%v), so no reviewer or curator will ever see it", sessionID, kanbanBoardName, err)',
     'degraded.Record("a session was dispatched with no card on %q (%v), so no reviewer or curator will ever see it", kanbanBoardName, err)',
     "caught"),
    ("the-failed-publish-report-does-not-name-the-provider",
     'degraded.Record("%s passed every gate and no worker was dispatched: the publish failed (%v)", d.provider, err)',
     'degraded.Record("a provider passed every gate and no worker was dispatched: the publish failed (%v)", err)',
     "caught"),
    ("the-missing-window-shortfall-does-not-name-the-window",
     'd.shortfall = fmt.Sprintf("%s was never evaluated: its snapshot is fresh but carries no %q window", p.Name, p.WindowKey)',
     'd.shortfall = fmt.Sprintf("%s was never evaluated: its snapshot is fresh but is missing a window", p.Name)',
     "caught"),
    ("the-unparked-ceiling-report-does-not-name-the-card",
     'degraded.Record("card %s is over its spend ceiling and the hold was refused (%v), so it is skipped but not parked", cand.cardID, err)',
     'degraded.Record("a card is over its spend ceiling and the hold was refused (%v), so it is skipped but not parked", err)',
     "caught"),
    ("the-unreadable-queue-board-report-does-not-name-the-board",
     'return nil, fmt.Sprintf("the %q board could not be reached (%v), so this tick dispatches the default prompt instead of queued work", autoworkerQueueBoard, err)',
     'return nil, fmt.Sprintf("a board could not be reached (%v), so this tick dispatches the default prompt instead of queued work", err)',
     "caught"),

    # ---- OVER-recording: a deliberate skip turned into a shortfall. The first
    # is the live population case -- 100 of 100 ticks, every five minutes.
    ("a-stale-snapshot-is-a-shortfall",
     '\t\td.reason = fmt.Sprintf("snapshot stale (snapshot_at=%s, %s old) — skip",\n\t\t\tsnapshotAt.Format(time.RFC3339), time.Since(snapshotAt).Truncate(time.Minute))\n\t\treturn d',
     '\t\td.reason = fmt.Sprintf("snapshot stale (snapshot_at=%s, %s old) — skip",\n\t\t\tsnapshotAt.Format(time.RFC3339), time.Since(snapshotAt).Truncate(time.Minute))\n\t\td.shortfall = "the snapshot is stale"\n\t\treturn d',
     "caught"),
    ("a-provider-within-slack-is-a-shortfall",
     '\t\td.reason = fmt.Sprintf("target=%.0f%% actual=%.0f%% — within slack (%0.fpp) of target, skip",\n\t\t\td.target, d.actual, slackPP)\n\t\treturn d',
     '\t\td.reason = fmt.Sprintf("target=%.0f%% actual=%.0f%% — within slack (%0.fpp) of target, skip",\n\t\t\td.target, d.actual, slackPP)\n\t\td.shortfall = "no room to dispatch into"\n\t\treturn d',
     "caught"),
    ("a-held-card-is-a-shortfall",
     '\t\t\tlog.Printf("queue: skip card %s — HELD since %s (%s)", c.Placement.CardID, heldAt, reason)\n\t\t\tcontinue',
     '\t\t\tlog.Printf("queue: skip card %s — HELD since %s (%s)", c.Placement.CardID, heldAt, reason)\n\t\t\tif degraded != nil {\n\t\t\t\tdegraded.Record("card %s was held", c.Placement.CardID)\n\t\t\t}\n\t\t\tcontinue',
     "caught"),
    ("a-board-that-does-not-exist-yet-is-a-shortfall",
     '\t\t\tlog.Printf("queue: board %q does not exist yet — first non-dry-run will bootstrap it", autoworkerQueueBoard)\n\t\t\treturn nil, ""',
     '\t\t\tlog.Printf("queue: board %q does not exist yet — first non-dry-run will bootstrap it", autoworkerQueueBoard)\n\t\t\treturn nil, "the queue board does not exist"',
     "caught"),

    # ---- the loud half of the stale decision. Not recording it is only
    # defensible because the line says how stale it is instead.
    ("the-stale-line-stops-saying-how-old-it-is",
     '\t\td.reason = fmt.Sprintf("snapshot stale (snapshot_at=%s, %s old) — skip",\n\t\t\tsnapshotAt.Format(time.RFC3339), time.Since(snapshotAt).Truncate(time.Minute))',
     '\t\td.reason = fmt.Sprintf("snapshot stale (snapshot_at=%s) — skip",\n\t\t\tsnapshotAt.Format(time.RFC3339))',
     "caught"),

    # ---- behavior the shortfalls sit on top of
    ("the-hold-gate-lets-held-cards-through",
     '\t\tif heldAt, _ := c.Item["held_at"].(string); heldAt != "" {',
     '\t\tif heldAt, _ := c.Item["held_at"].(string); false && heldAt != "" {',
     "caught"),
    ("a-dry-run-dispatches-anyway",
     "\t\tif !d.fire || cfg.dryRun {\n\t\t\tcontinue\n\t\t}",
     "\t\tif !d.fire {\n\t\t\tcontinue\n\t\t}",
     "caught"),
    ("the-queued-card-never-reaches-the-prompt",
     "\t\tworkerPrompt := cfg.prompt\n\t\tif queueCard != nil {\n\t\t\tworkerPrompt = buildQueuePrompt(queueCard)\n\t\t}",
     "\t\tworkerPrompt := cfg.prompt\n\t\tif queueCard != nil && false {\n\t\t\tworkerPrompt = buildQueuePrompt(queueCard)\n\t\t}",
     "caught"),
    ("a-dispatched-card-is-never-moved",
     "\t\tif queueCard != nil && queueClient != nil {\n\t\t\tif err := markQueueCardDispatched(queueClient, queueCard, p, sessionID, now); err != nil {",
     "\t\tif queueCard != nil && queueClient != nil && false {\n\t\t\tif err := markQueueCardDispatched(queueClient, queueCard, p, sessionID, now); err != nil {",
     "caught"),

    # ---- controls. These must NOT redden, or the suite is pinning wording.
    ("reword-a-log-line-no-test-reads",
     'log.Printf("[%s] FIRED worker → bridge.inbound (harness=%s instance=%s target=%.0f%% actual=%.0f%% queue_card=%s)",',
     'log.Printf("[%s] dispatched worker → bridge.inbound (harness=%s instance=%s target=%.0f%% actual=%.0f%% queue_card=%s)",',
     "survives"),
    ("change-the-telemetry-card-title-format",
     'title := fmt.Sprintf("autoworker · %s · %s", p.Name, now.Format("Jan 2 15:04"))',
     'title := fmt.Sprintf("autoworker/%s/%s", p.Name, now.Format("2006-01-02 15:04"))',
     "survives"),
]

ABORT_MARKERS = ("build failed", "undefined:", "imported and not used",
                 "declared and not used", "syntax error", "cannot use",
                 "[build failed]", "no required module", "not enough arguments",
                 "too many arguments", "missing return")


def run_tests():
    p = subprocess.run(["go", "test", "-count=1", PKG], cwd=REPO,
                       capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def main():
    if "--list-sabotages" in sys.argv:
        for name, _, _, _ in ARMS:
            print(name)
        return 0

    print(f"scoring repository: {REPO}")
    head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=REPO,
                          capture_output=True, text=True).stdout.strip()
    branch = subprocess.run(["git", "rev-parse", "--abbrev-ref", "HEAD"], cwd=REPO,
                            capture_output=True, text=True).stdout.strip()
    dirty = subprocess.run(["git", "status", "--porcelain", "--", str(MAIN)],
                           cwd=REPO, capture_output=True, text=True).stdout.strip()
    print(f"scoring commit:     {head} on {branch}"
          f"{' (main.go DIRTY -- you are scoring uncommitted bytes)' if dirty else ''}")

    original = MAIN.read_text()

    code, out = run_tests()
    if code != 0:
        print("BASELINE IS RED -- fix that before scoring any arm:\n" + out)
        return 2
    print("baseline: GREEN\n")

    only = None
    if "--sabotage" in sys.argv:
        only = sys.argv[sys.argv.index("--sabotage") + 1]

    caught = missed = aborted = survived = wrong = 0
    for name, old, new, expect in ARMS:
        if only and name != only:
            continue
        if old not in original:
            print(f"  ABORTED  {name}  -- anchor not found in main.go; the arm points at code that moved")
            aborted += 1
            continue
        if original.count(old) != 1:
            print(f"  ABORTED  {name}  -- anchor matches {original.count(old)} times; the edit would be one nobody chose")
            aborted += 1
            continue
        MAIN.write_text(original.replace(old, new, 1))
        try:
            code, out = run_tests()
        finally:
            MAIN.write_text(original)

        if any(m in out for m in ABORT_MARKERS):
            print(f"  ABORTED  {name}  -- the mutation did not compile, so nothing was tested")
            aborted += 1
            continue

        red = code != 0
        if expect == "caught":
            if red:
                names = sorted(set(re.findall(r"--- FAIL: (\w+)", out)))
                print(f"  CAUGHT   {name}  by {', '.join(names) or 'the package'}")
                caught += 1
            else:
                print(f"  MISSED   {name}  -- no test noticed")
                missed += 1
        else:
            if red:
                names = sorted(set(re.findall(r"--- FAIL: (\w+)", out)))
                print(f"  WRONG    {name}  -- a control went red ({', '.join(names)}): the suite is pinning the wording, not the behavior")
                wrong += 1
            else:
                print(f"  SURVIVES {name}  (control, as intended)")
                survived += 1

    print(f"\nCAUGHT {caught} / MISSED {missed} / ABORTED {aborted}"
          f"  |  controls SURVIVED {survived} / WRONG {wrong}")
    return 0 if (missed == 0 and aborted == 0 and wrong == 0) else 1


if __name__ == "__main__":
    sys.exit(main())
