#!/usr/bin/env python3
"""Score cmd/spend-ceiling-guard's degradation tests by breaking the code they pin.

A green suite is a claim about a suite, not about the code. This plants one
defect at a time and asks whether anything goes red.

Three outcomes, not two. A mutation that does not COMPILE exits non-zero exactly
as a red suite does, so a scorer reading only the exit code scores a build
failure as a catch and reports a test that tested nothing. Every arm here is
therefore judged on the output as well: `build failed`, `undefined:`,
`imported and not used` and friends are ABORTED, and an ABORTED arm is a hole in
the scorer, not a win.

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
MAIN = REPO / "cmd" / "spend-ceiling-guard" / "main.go"
PKG = "./cmd/spend-ceiling-guard/"

# name -> (old, new, expectation). expectation "caught" = at least one test must
# go red; "survives" = the suite must stay green.
ARMS = [
    ("drop-the-unpriceable-subtree-record",
     '\t\t\tdegraded.Record("item %s: spend unreadable (%v) — its ceiling went unenforced this tick", it.ID, err)\n',
     '',
     "caught"),
    ("drop-the-failed-hold-record",
     '\t\t\tdegraded.Record("item %s is OVER CEILING ($%.2f of $%.2f) and the hold FAILED (%v) — it is still dispatchable",\n\t\t\t\tit.ID, spent, ceiling, err)\n',
     '',
     "caught"),
    ("drop-the-truncation-record",
     '\t\tdegraded.Record("read only %d noteboard items before hitting the %d-page safety bound: the spend tree is incomplete and every ceiling below it is priced too LOW",\n\t\t\tlen(items), maxItemPages)\n',
     '',
     "caught"),
    ("the-run-always-exits-zero",
     "\treturn degraded.ExitCode()\n}",
     "\t_ = degraded\n\treturn 0\n}",
     "caught"),
    ("the-failed-hold-does-not-name-the-task",
     'degraded.Record("item %s is OVER CEILING ($%.2f of $%.2f) and the hold FAILED (%v) — it is still dispatchable",\n\t\t\t\tit.ID, spent, ceiling, err)',
     'degraded.Record("a task is OVER CEILING ($%.2f of $%.2f) and the hold FAILED (%v) — it is still dispatchable",\n\t\t\t\tspent, ceiling, err)',
     "caught"),
    ("the-read-stops-after-one-page",
     "\tfor page := 0; page < maxItemPages; page++ {",
     "\tfor page := 0; page < 1; page++ {",
     "caught"),
    ("a-short-page-is-not-the-end",
     "\t\tif len(batch) < itemPageSize {\n\t\t\treturn items, false, nil\n\t\t}",
     "\t\tif false {\n\t\t\treturn items, false, nil\n\t\t}",
     "caught"),
    ("the-bound-is-reached-quietly",
     "\treturn items, true, nil\n}",
     "\treturn items, false, nil\n}",
     "caught"),
    ("an-item-seen-twice-is-counted-twice",
     "\t\t\tif seen[it.ID] {\n\t\t\t\tcontinue\n\t\t\t}",
     "\t\t\tif false && seen[it.ID] {\n\t\t\t\tcontinue\n\t\t\t}",
     "caught"),
    ("the-read-forgets-include_held",
     '"%s/api/items?include_held=true&limit=%d&offset=%d"',
     '"%s/api/items?limit=%d&offset=%d"',
     "caught"),
    ("an-already-parked-task-is-priced-again",
     "\t\tif it.HeldAt != nil {\n\t\t\tcontinue // already parked; no reason to price it out again\n\t\t}",
     "\t\tif false && it.HeldAt != nil {\n\t\t\tcontinue // already parked; no reason to price it out again\n\t\t}",
     "caught"),
    ("a-dry-run-writes-anyway",
     "\t\tif cfg.dryRun {",
     "\t\tif false && cfg.dryRun {",
     "caught"),
    # --- controls. These must NOT redden, or the suite is going red on anything.
    ("reword-a-log-line-no-test-reads",
     'log.Printf("loaded cost for %d sessions, %d items", len(cost), len(items))',
     'log.Printf("cost table: %d sessions; items: %d", len(cost), len(items))',
     "survives"),
    ("halve-the-page-size",
     "\titemPageSize = 1000",
     "\titemPageSize = 500",
     "survives"),
]

ABORT_MARKERS = ("build failed", "undefined:", "imported and not used",
                 "declared and not used", "syntax error", "cannot use",
                 "[build failed]", "no required module")


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
    dirty = subprocess.run(["git", "status", "--porcelain", "--", str(MAIN)],
                           cwd=REPO, capture_output=True, text=True).stdout.strip()
    print(f"scoring commit:     {head}{' (main.go DIRTY -- you are scoring uncommitted bytes)' if dirty else ''}")

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
                print(f"  WRONG    {name}  -- a control went red ({', '.join(names)}): the suite is pinning the constant, not the behaviour")
                wrong += 1
            else:
                print(f"  SURVIVES {name}  (control, as intended)")
                survived += 1

    print(f"\nCAUGHT {caught} / MISSED {missed} / ABORTED {aborted}"
          f"  |  controls SURVIVED {survived} / WRONG {wrong}")
    return 0 if (missed == 0 and aborted == 0 and wrong == 0) else 1


if __name__ == "__main__":
    sys.exit(main())
