#!/usr/bin/env python3
"""Score cmd/kanban-scoper's degradation tests by breaking the code they pin.

A green suite is a claim about a suite, not about the code, and this suite was
green on its first run -- which is exactly when it is worth nothing. This plants
one defect at a time and asks whether anything goes red.

Three outcomes, not two. A mutation that does not COMPILE exits non-zero exactly
as a red suite does, so a scorer reading only the exit code scores a build
failure as a catch and reports a test that tested nothing. Every arm here is
judged on the output as well: `build failed`, `undefined:`, `declared and not
used` and friends are ABORTED, and an ABORTED arm is a hole in the scorer, not a
win. (card cd03d6f3)

Both directions are armed. On THIS command the over-recording risk is the larger
one and it is measured, not assumed: job 15 logs `parents-needing-scope=0` on
100 of 100 retained runs. An arm that turns any of its five deliberate skips into
a shortfall must be CAUGHT, or the job reports failure every five minutes and its
exit code stops meaning anything.

The six anonymity arms exist because this binary prints the same card id and
title from its own `[%s] scope failed` line one call ABOVE most of the Records.
An assertion reading the whole captured log would stay green on every one of
them; the suite reads the single `run degraded:` line instead. Measured as a live
MISSED on cmd/spend-ceiling-guard and again on cmd/kanban-curator, 2026-08-29.

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
MAIN = REPO / "cmd" / "kanban-scoper" / "main.go"
PKG = "./cmd/kanban-scoper/"

# name -> (old, new, expectation). expectation "caught" = at least one test must
# go red; "survives" = the suite must stay green.
ARMS = [
    # ------------------------------------------------- each Record silenced (8)
    ("drop-the-unreadable-tag-record",
     '\t\t\tdegraded.Record("goal card %s (%s): its scoped tag could not be read (%v), so it was never considered for decomposition",\n\t\t\t\tc.Placement.CardID, cardTitle(c.Item), err)\n',
     '',
     "caught"),
    ("drop-the-decomposer-failure-record",
     '\t\tdegraded.Record("goal card %s (%s): the decomposer failed (%v), so it was not broken down and stays unscoped",\n\t\t\tp.cardID, p.title(), err)\n',
     '',
     "caught"),
    ("drop-the-zero-subtask-record",
     '\t\tdegraded.Record("goal card %s (%s): the decomposer returned 0 sub-tasks, so nothing was written and it stays unscoped",\n\t\t\tp.cardID, p.title())\n',
     '',
     "caught"),
    ("drop-the-missing-column-record",
     '\t\tdegraded.Record("goal card %s (%s): the board has no %q column, so its %d sub-tasks had nowhere to go",\n\t\t\tp.cardID, p.title(), columnBacklog, len(plan.Subtasks))\n',
     '',
     "caught"),
    ("drop-the-empty-title-record",
     '\t\t\tdegraded.Record("goal card %s (%s): sub-task %d came back with an empty title and was dropped, and the parent is stamped scoped regardless — that slice of the goal is lost",\n\t\t\t\tp.cardID, p.title(), i)\n',
     '',
     "caught"),
    ("drop-the-refused-link-record",
     '\t\t\t\tdegraded.Record("goal card %s (%s): sub-card %s (%q) was created with no parent_card link (%v), so kanban-dispatcher\'s closure roll-up will never find it",\n\t\t\t\t\tp.cardID, p.title(), childID, draft.Title, err)\n',
     '',
     "caught"),
    ("drop-the-refused-subcard-record",
     '\t\t\tdegraded.Record("goal card %s (%s): sub-card %q was refused by the board (%v) — the parent is left unstamped, so the next tick decomposes it again and re-creates every sub-card that did land",\n\t\t\t\tp.cardID, p.title(), draft.Title, err)\n',
     '',
     "caught"),
    ("drop-the-refused-stamp-record",
     '\t\tdegraded.Record("goal card %s (%s): its %d sub-cards were written but the scoped stamp was refused (%v), so every later tick decomposes it again and writes a duplicate set",\n\t\t\tp.cardID, p.title(), len(plan.Subtasks), err)\n',
     '',
     "caught"),

    # ------------------------------------------------------- the verdict itself
    ("the-run-always-exits-zero",
     "\tlog.Printf(\"[scoper] done: scoped=%d\", scoped)\n\treturn degraded.ExitCode()\n}",
     "\tlog.Printf(\"[scoper] done: scoped=%d\", scoped)\n\t_ = degraded\n\treturn 0\n}",
     "caught"),
    ("only-the-first-shortfall-is-kept",
     "\tlog.Printf(\"[scoper] done: scoped=%d\", scoped)\n\treturn degraded.ExitCode()\n}",
     "\tlog.Printf(\"[scoper] done: scoped=%d\", scoped)\n\tif len(degraded.Reasons()) > 1 {\n\t\tdegraded = degradations.New(\"scoper\")\n\t\tdegraded.Record(\"%s\", \"several things fell short\")\n\t}\n\treturn degraded.ExitCode()\n}",
     "caught"),
    # The early return on an empty board must report too, or a tick that could
    # not read a single Goals card's tags exits 0 -- and that is the ONLY shape
    # job 15 has had for its whole retained history.
    ("the-empty-board-path-skips-the-verdict",
     "\tif len(parents) == 0 {\n\t\treturn degraded.ExitCode()\n\t}",
     "\tif len(parents) == 0 {\n\t\t_ = degraded\n\t\treturn 0\n\t}",
     "caught"),

    # ---------------------------------------- a report that does not identify
    # what it is about. Every one of these stays green under a whole-log
    # assertion, because the line above the Record already names the card.
    ("the-tag-read-report-does-not-name-the-card",
     'degraded.Record("goal card %s (%s): its scoped tag could not be read (%v), so it was never considered for decomposition",\n\t\t\t\tc.Placement.CardID, cardTitle(c.Item), err)',
     'degraded.Record("a goal card (%s): its scoped tag could not be read (%v), so it was never considered for decomposition",\n\t\t\t\tcardTitle(c.Item), err)',
     "caught"),
    ("the-decomposer-report-does-not-name-the-card",
     'degraded.Record("goal card %s (%s): the decomposer failed (%v), so it was not broken down and stays unscoped",\n\t\t\tp.cardID, p.title(), err)',
     'degraded.Record("a goal card (%s): the decomposer failed (%v), so it was not broken down and stays unscoped",\n\t\t\tp.title(), err)',
     "caught"),
    ("the-refused-subcard-report-does-not-say-what-happens-next",
     '(%v) — the parent is left unstamped, so the next tick decomposes it again and re-creates every sub-card that did land",\n\t\t\t\tp.cardID, p.title(), draft.Title, err)',
     '(%v)",\n\t\t\t\tp.cardID, p.title(), draft.Title, err)',
     "caught"),
    ("the-refused-stamp-report-does-not-say-it-duplicates",
     'but the scoped stamp was refused (%v), so every later tick decomposes it again and writes a duplicate set",\n\t\t\tp.cardID, p.title(), len(plan.Subtasks), err)',
     'but the scoped stamp was refused (%v)",\n\t\t\tp.cardID, p.title(), len(plan.Subtasks), err)',
     "caught"),
    ("the-refused-link-report-does-not-name-the-roll-up",
     'was created with no parent_card link (%v), so kanban-dispatcher\'s closure roll-up will never find it",\n\t\t\t\t\tp.cardID, p.title(), childID, draft.Title, err)',
     'was created with no parent_card link (%v)",\n\t\t\t\t\tp.cardID, p.title(), childID, draft.Title, err)',
     "caught"),
    ("the-empty-title-report-does-not-say-it-is-lost",
     'and was dropped, and the parent is stamped scoped regardless — that slice of the goal is lost",\n\t\t\t\tp.cardID, p.title(), i)',
     'and was dropped",\n\t\t\t\tp.cardID, p.title(), i)',
     "caught"),

    # ---- OVER-recording: a deliberate skip turned into a shortfall. The first
    # is the live population case -- 100 of 100 ticks, every five minutes.
    ("an-empty-goals-column-is-a-shortfall",
     "\tlog.Printf(\"[scoper] board=%q id=%s parents-needing-scope=%d\", board.Name, board.ID, len(parents))\n\tif len(parents) == 0 {",
     "\tlog.Printf(\"[scoper] board=%q id=%s parents-needing-scope=%d\", board.Name, board.ID, len(parents))\n\tif len(parents) == 0 {\n\t\tdegraded.Record(\"nothing needed scoping\")",
     "caught"),
    ("an-already-scoped-card-is-a-shortfall",
     "\t\tif anyTagEquals(etags, tagScoped) {\n\t\t\tcontinue\n\t\t}",
     "\t\tif anyTagEquals(etags, tagScoped) {\n\t\t\tdegraded.Record(\"goal card %s was already scoped\", c.Placement.CardID)\n\t\t\tcontinue\n\t\t}",
     "caught"),
    ("a-card-without-the-trigger-tag-is-a-shortfall",
     "\t\tif !contains(tags, tagTrigger) {\n\t\t\tcontinue\n\t\t}",
     "\t\tif !contains(tags, tagTrigger) {\n\t\t\tdegraded.Record(\"goal card %s was not scoped\", c.Placement.CardID)\n\t\t\tcontinue\n\t\t}",
     "caught"),
    ("a-deferral-at-the-per-tick-cap-is-a-shortfall",
     '\t\tif scoped >= cfg.MaxScope {\n\t\t\t// Deliberate, and not a shortfall: the deferred parents keep their\n\t\t\t// unstamped state and the next tick five minutes later takes them.\n\t\t\tlog.Printf("[scoper] hit per-tick cap (%d); deferring %d parents to next tick",\n\t\t\t\tcfg.MaxScope, len(parents)-scoped)\n',
     '\t\tif scoped >= cfg.MaxScope {\n\t\t\t// Deliberate, and not a shortfall: the deferred parents keep their\n\t\t\t// unstamped state and the next tick five minutes later takes them.\n\t\t\tlog.Printf("[scoper] hit per-tick cap (%d); deferring %d parents to next tick",\n\t\t\t\tcfg.MaxScope, len(parents)-scoped)\n\t\t\tdegraded.Record("%d parents were not scoped this tick", len(parents)-scoped)\n',
     "caught"),
    ("a-dry-run-is-a-shortfall",
     "\tif cfg.DryRun {\n\t\treturn nil\n\t}",
     "\tif cfg.DryRun {\n\t\tdegraded.Record(\"goal card %s was not written: dry run\", p.cardID)\n\t\treturn nil\n\t}",
     "caught"),

    # ---- behaviour the shortfalls sit on top of
    ("a-refused-parent-link-abandons-the-rest-of-the-plan",
     "\t\t\tif errors.Is(err, errParentLinkRefused) {",
     "\t\t\tif false && errors.Is(err, errParentLinkRefused) {",
     "caught"),
    ("a-subcard-is-created-with-no-parent_id",
     "\t\t\"parent_id\": parentID,",
     "\t\t\"parent_id\": \"\",",
     "caught"),
    ("an-empty-titled-subtask-is-written-anyway",
     "\t\tif draft.Title == \"\" {",
     "\t\tif false && draft.Title == \"\" {",
     "caught"),

    # ---- controls. These must NOT redden, or the suite is pinning wording.
    ("reword-the-closing-line-no-test-reads",
     "\tlog.Printf(\"[scoper] done: scoped=%d\", scoped)",
     "\tlog.Printf(\"[scoper] finished: scoped=%d\", scoped)",
     "survives"),
    # Re-aimed 2026-09-25: the sub-card footer is pinned on main by
    # TestBuildSubcardDraft_GroupedSubtaskGetsGroupTagAndBacktickedBody, on
    # purpose, so rewording it is no longer a control.
    ("reword-the-subcard-created-line",
     '\t\tlog.Printf("[%s] subcard %s → %q", p.title(), childID, draft.Title)\n',
     '\t\tlog.Printf("[%s] created subcard %s: %q", p.title(), childID, draft.Title)\n',
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
                print(f"  WRONG    {name}  -- a control went red ({', '.join(names)}): the suite is pinning the wording, not the behaviour")
                wrong += 1
            else:
                print(f"  SURVIVES {name}  (control, as intended)")
                survived += 1

    print(f"\nCAUGHT {caught} / MISSED {missed} / ABORTED {aborted}"
          f"  |  controls SURVIVED {survived} / WRONG {wrong}")
    return 0 if (missed == 0 and aborted == 0 and wrong == 0) else 1


if __name__ == "__main__":
    sys.exit(main())
