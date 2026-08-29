#!/usr/bin/env python3
"""Score cmd/kanban-curator's degradation tests by breaking the code they pin.

A green suite is a claim about a suite, not about the code, and this suite was
green on its first run — which is exactly when it is worth nothing. This plants
one defect at a time and asks whether anything goes red.

Three outcomes, not two. A mutation that does not COMPILE exits non-zero exactly
as a red suite does, so a scorer reading only the exit code scores a build
failure as a catch and reports a test that tested nothing. Every arm here is
judged on the output as well: `build failed`, `undefined:`, `declared and not
used` and friends are ABORTED, and an ABORTED arm is a hole in the scorer, not a
win.

Both directions are armed, because this command's whole risk is over-recording.
Arms that turn a deliberate skip into a shortfall must be CAUGHT by the
not-a-shortfall tests, or a curator tick on an empty board reports failure every
five minutes and the exit code stops meaning anything.

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
MAIN = REPO / "cmd" / "kanban-curator" / "main.go"
PKG = "./cmd/kanban-curator/"

# name -> (old, new, expectation). expectation "caught" = at least one test must
# go red; "survives" = the suite must stay green.
ARMS = [
    # --- each Record site silenced on its own
    ("drop-the-missing-item-record",
     '\t\t\tdegraded.Record("card %s: its noteboard item did not resolve, so the card was not examined", c.Placement.CardID)\n',
     '',
     "caught"),
    ("drop-the-unreadable-links-record",
     '\t\t\tdegraded.Record("card %s (%s): its links could not be read (%v), so it was not examined", c.Placement.CardID, title, err)\n',
     '',
     "caught"),
    ("drop-the-unreadable-session-record",
     '\t\t\tdegraded.Record("card %s (%s): session %s could not be read from the bridge (%v), so it was not examined", c.Placement.CardID, title, sessionID, err)\n',
     '',
     "caught"),
    ("drop-the-unasked-sign-off-record",
     '\t\t\t\t\tdegraded.Record("card %s (%s): the review sign-off for session %s could not be looked up (%v), so closing was deferred without asking", c.Placement.CardID, title, sessionID, err)\n',
     '',
     "caught"),
    ("drop-the-missing-closing-note-record",
     '\t\t\t\t\tdegraded.Record("card %s (%s): closed to %s with no note saying why — the body patch was refused (%v)", c.Placement.CardID, title, targetName, err)\n',
     '',
     "caught"),
    ("drop-the-refused-move-record",
     '\t\t\tdegraded.Record("card %s (%s): judged %s and the move was refused (%v) — it is still In Progress", c.Placement.CardID, title, targetName, err)\n',
     '',
     "caught"),

    # --- the verdict itself
    ("the-run-always-exits-zero",
     "\treturn degraded.ExitCode()\n}",
     "\t_ = degraded\n\treturn 0\n}",
     "caught"),

    # --- a report that does not identify the card it is about. The log.Printf
    # immediately above every Record here names the same card, so an assertion
    # reading the whole captured log would stay green on all of these.
    ("the-refused-move-does-not-name-the-card",
     'degraded.Record("card %s (%s): judged %s and the move was refused (%v) — it is still In Progress", c.Placement.CardID, title, targetName, err)',
     'degraded.Record("a card was judged %s and the move was refused (%v)", targetName, err)',
     "caught"),
    ("the-unreadable-links-report-does-not-name-the-card",
     'degraded.Record("card %s (%s): its links could not be read (%v), so it was not examined", c.Placement.CardID, title, err)',
     'degraded.Record("a card\'s links could not be read (%v), so it was not examined", err)',
     "caught"),
    ("the-unreadable-session-report-does-not-name-the-session",
     'degraded.Record("card %s (%s): session %s could not be read from the bridge (%v), so it was not examined", c.Placement.CardID, title, sessionID, err)',
     'degraded.Record("card %s (%s): its session could not be read from the bridge (%v), so it was not examined", c.Placement.CardID, title, err)',
     "caught"),
    ("only-the-first-shortfall-is-kept",
     "\treturn degraded.ExitCode()\n}",
     "\tif len(degraded.Reasons()) > 1 {\n\t\tdegraded = degradations.New(\"curator\")\n\t\tdegraded.Record(\"%s\", \"several cards fell short\")\n\t}\n\treturn degraded.ExitCode()\n}",
     "caught"),

    # --- OVER-recording: a deliberate skip turned into a shortfall
    ("a-card-with-no-session-link-is-a-shortfall",
     '\t\t\tlog.Printf("[%s] no session link — nothing to close against (card_id=%s)", title, c.Placement.CardID)\n',
     '\t\t\tlog.Printf("[%s] no session link — nothing to close against (card_id=%s)", title, c.Placement.CardID)\n\t\t\tdegraded.Record("card %s has no session link", c.Placement.CardID)\n',
     "caught"),
    ("a-card-awaiting-sign-off-is-a-shortfall",
     '\t\t\t\t\tlog.Printf("[%s] completed; awaiting autoworker-reviewer sign-off — leaving In Progress (session_id=%s)", title, sessionID)\n',
     '\t\t\t\t\tlog.Printf("[%s] completed; awaiting autoworker-reviewer sign-off — leaving In Progress (session_id=%s)", title, sessionID)\n\t\t\t\t\tdegraded.Record("card %s is awaiting sign-off", c.Placement.CardID)\n',
     "caught"),

    # --- the skip goes silent again
    ("the-no-session-skip-goes-back-to-silence",
     '\t\t\tlog.Printf("[%s] no session link — nothing to close against (card_id=%s)", title, c.Placement.CardID)\n',
     '',
     "caught"),

    # --- behaviour the shortfalls sit on top of
    ("a-sign-off-that-could-not-be-read-counts-as-signed-off",
     "\t\treturn false, err\n\t}",
     "\t\treturn true, err\n\t}",
     "caught"),
    ("a-refused-closing-note-stops-the-move",
     '\t\t\t\t\tlog.Printf("[%s] patch body: %v (continuing to move)", title, err)\n',
     '\t\t\t\t\tlog.Printf("[%s] patch body: %v (continuing to move)", title, err)\n\t\t\t\t\tcontinue\n',
     "caught"),
    ("a-dry-run-writes-anyway",
     "\t\tif cfg.dryRun {",
     "\t\tif false && cfg.dryRun {",
     "caught"),
    # Neutralised, not deleted. `return false` here leaves `t` declared and not
    # used, so the package stops compiling and the arm exits non-zero for exactly
    # the reason a red suite does -- it came back ABORTED on the first run and
    # read like the strongest arm in the set until the output was checked.
    ("the-grace-window-never-fires",
     "\treturn time.Since(t) > reviewGrace",
     "\treturn t.IsZero()",
     "caught"),
    ("the-grace-window-is-three-hours",
     "\treviewGrace = 30 * time.Minute",
     "\treviewGrace = 3 * time.Hour",
     "caught"),
    ("a-vanished-session-is-an-error-not-a-terminal-state",
     "\t\tif err != nil && !notFound {",
     "\t\tif err != nil {",
     "caught"),

    # --- controls. These must NOT redden, or the suite is going red on anything.
    ("reword-a-log-line-no-test-reads",
     'log.Printf("[curator] board=%q id=%s in-progress=%d", board.Name, board.ID, len(inProgress))',
     'log.Printf("[curator] curating %q (%s); %d cards in progress", board.Name, board.ID, len(inProgress))',
     "survives"),
    ("change-the-stale-after-flag-default",
     '\t\tstaleAfter   = flag.Duration("stale-after", 2*time.Hour,',
     '\t\tstaleAfter   = flag.Duration("stale-after", 3*time.Hour,',
     "survives"),
]

ABORT_MARKERS = ("build failed", "undefined:", "imported and not used",
                 "declared and not used", "syntax error", "cannot use",
                 "[build failed]", "no required module", "not enough arguments",
                 "too many arguments")


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
