#!/usr/bin/env python3
"""Score cmd/autoworker-reviewer's degradation tests by breaking the code they pin.

A green suite is a claim about a suite, not about the code, and this suite was
green on its first run -- which is exactly when it is worth nothing. This plants
one defect at a time and asks whether anything goes red.

Three outcomes, not two. A mutation that does not COMPILE exits non-zero exactly
as a red suite does, so a scorer reading only the exit code scores a build
failure as a catch and reports a test that tested nothing. Every arm here is
judged on the output as well: `build failed`, `undefined:`, `declared and not
used` and friends are ABORTED, and an ABORTED arm is a hole in the scorer, not a
win.

Both directions are armed, and on this command over-recording is the larger risk
of the two. Measured 2026-08-29: 231 of the 234 In Progress cards on "Agent runs"
are skipped by the origin gate on every tick, so an arm that turns that skip into
a shortfall must be CAUGHT or job 19 reports failure every five minutes and its
exit code stops meaning anything.

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
MAIN = REPO / "cmd" / "autoworker-reviewer" / "main.go"
PKG = "./cmd/autoworker-reviewer/"

# name -> (old, new, expectation). expectation "caught" = at least one test must
# go red; "survives" = the suite must stay green.
ARMS = [
    # --- each Record site silenced on its own. Re-aimed 2026-09-25 when this
    # suite was ported onto main, where the per-session review lives in
    # reviewCompletedSession and reports through a `record` func that prefixes
    # the card.
    ("drop-the-missing-item-record",
     '\t\t\tdegraded.Record("card %s: its noteboard item did not resolve, so the card was not examined", c.Placement.CardID)\n',
     '',
     "caught"),
    ("drop-the-unreadable-links-record",
     '\t\t\trecord("its links could not be read (%v), so it was not examined", err)\n',
     '',
     "caught"),
    ("drop-the-unreadable-session-record",
     '\t\t\trecord("session %s could not be read from the bridge (%v), so it was not reviewed", sessionID, err)\n',
     '',
     "caught"),
    ("drop-the-unreadable-passes-record",
     '\t\trecord("the review passes on session %s could not be looked up (%v), so it was not reviewed", sessionID, err)\n',
     '',
     "caught"),
    ("drop-the-refused-loop-guard-settle-record",
     '\t\t\trecord("session %s hit the %d-pass loop guard and the settle tag was refused (%v) — the curator cannot close it", sessionID, maxPasses, err)\n',
     '',
     "caught"),
    ("drop-the-undelivered-nudge-record",
     '\t\trecord("self-review nudge %d/%d for session %s was not delivered (%v), so the session was closed unreviewed", pass, maxPasses, sessionID, err)\n',
     '',
     "caught"),
    ("drop-the-wedged-card-record",
     '\t\t\trecord("the nudge failed AND the settle that covers for it was refused (%v) — it is still In Progress with nothing left to move it", serr)\n',
     '',
     "caught"),
    # The sharp one: the pass tag is the only record a nudge happened. Main
    # withholds the nudge when the tag is refused, so this is now a review that
    # did not happen rather than a nudge the loop guard cannot see.
    ("drop-the-withheld-nudge-record",
     '\t\trecord("the review-pass-%d tag for session %s was refused (%v), so nudge %d was withheld to keep the loop guard counting and the session was closed unreviewed", pass, sessionID, err, pass)\n',
     '',
     "caught"),
    ("drop-the-withheld-nudge-wedge-record",
     '\t\t\trecord("the settle that covers for the withheld nudge was refused too (%v) — it is still In Progress with nothing left to move it", serr)\n',
     '',
     "caught"),
    ("drop-the-unstamped-note-record",
     '\t\t\t\t\trecord("the reviewer\'s note %q was not written to the card body (%v)", note, err)\n',
     '',
     "caught"),

    # --- the verdict itself
    ("the-run-always-exits-zero",
     "\t\tnudged, settled, counts.total(), counts.breakdown(), errors)\n\treturn degraded.ExitCode()\n}",
     "\t\tnudged, settled, counts.total(), counts.breakdown(), errors)\n\t_ = degraded\n\treturn 0\n}",
     "caught"),
    ("only-the-first-shortfall-is-kept",
     "\t\tnudged, settled, counts.total(), counts.breakdown(), errors)\n\treturn degraded.ExitCode()\n}",
     "\t\tnudged, settled, counts.total(), counts.breakdown(), errors)\n\tif len(degraded.Reasons()) > 1 {\n\t\tdegraded = degradations.New(\"reviewer\")\n\t\tdegraded.Record(\"%s\", \"several cards fell short\")\n\t}\n\treturn degraded.ExitCode()\n}",
     "caught"),

    # --- a report that does not identify what it is about. This command logs a
    # line naming the same card immediately above every Record, so an assertion
    # reading the whole captured log would stay green on all of these.
    ("the-record-prefix-drops-the-card",
     'degraded.Record("card %s (%s): "+format, append([]any{c.Placement.CardID, title}, args...)...)',
     'degraded.Record(format, args...)',
     "caught"),
    ("the-withheld-nudge-report-does-not-name-the-session",
     'record("the review-pass-%d tag for session %s was refused (%v), so nudge %d was withheld to keep the loop guard counting and the session was closed unreviewed", pass, sessionID, err, pass)',
     'record("the review-pass-%d tag was refused (%v), so nudge %d was withheld to keep the loop guard counting and the session was closed unreviewed", pass, err, pass)',
     "caught"),
    ("the-withheld-nudge-report-does-not-say-why",
     'so nudge %d was withheld to keep the loop guard counting and the session was closed unreviewed", pass, sessionID, err, pass)',
     'so nudge %d was withheld", pass, sessionID, err, pass)',
     "caught"),
    ("the-unreadable-session-report-does-not-name-the-session",
     'record("session %s could not be read from the bridge (%v), so it was not reviewed", sessionID, err)',
     'record("its session could not be read from the bridge (%v), so it was not reviewed", err)',
     "caught"),
    ("the-wedged-card-report-does-not-say-it-is-wedged",
     'record("the nudge failed AND the settle that covers for it was refused (%v) — it is still In Progress with nothing left to move it", serr)',
     'record("the nudge failed and so did the settle after it (%v)", serr)',
     "caught"),

    # --- OVER-recording: a deliberate skip turned into a shortfall. The first is
    # the population case -- 231 of 234 live cards, every tick.
    ("a-classifier-minted-session-is-a-shortfall",
     "\t\tif !createdByAutoworker(sess) {\n\t\t\tcounts.notAutoworker++\n\t\t\tcontinue\n\t\t}",
     "\t\tif !createdByAutoworker(sess) {\n\t\t\tcounts.notAutoworker++\n\t\t\trecord(\"was not reviewed\")\n\t\t\tcontinue\n\t\t}",
     "caught"),
    ("a-card-with-no-session-link-is-a-shortfall",
     '\t\t\tlog.Printf("[%s] no session link — nothing to review against (card_id=%s)", title, c.Placement.CardID)\n',
     '\t\t\tlog.Printf("[%s] no session link — nothing to review against (card_id=%s)", title, c.Placement.CardID)\n\t\t\tdegraded.Record("card %s has no session link", c.Placement.CardID)\n',
     "caught"),
    ("a-still-running-session-is-a-shortfall",
     "\t\tif sess.State != \"completed\" {\n\t\t\tcounts.notCompleted++\n\t\t\tcontinue\n\t\t}",
     "\t\tif sess.State != \"completed\" {\n\t\t\tcounts.notCompleted++\n\t\t\trecord(\"was not reviewed\")\n\t\t\tcontinue\n\t\t}",
     "caught"),
    ("an-already-settled-session-is-a-shortfall",
     "\tif hasTag(tags, tagSettled) {\n\t\treturn actionAlreadySettled // awaiting curator close\n\t}",
     "\tif hasTag(tags, tagSettled) {\n\t\trecord(\"was not reviewed\")\n\t\treturn actionAlreadySettled // awaiting curator close\n\t}",
     "caught"),

    # --- the skip goes silent again
    ("the-no-session-skip-goes-back-to-silence",
     '\t\t\tlog.Printf("[%s] no session link — nothing to review against (card_id=%s)", title, c.Placement.CardID)\n',
     '',
     "caught"),

    # --- behaviour the shortfalls sit on top of
    ("the-origin-gate-lets-everything-through",
     "\t\tif !createdByAutoworker(sess) {",
     "\t\tif false && !createdByAutoworker(sess) {",
     "caught"),
    ("a-failed-nudge-no-longer-settles",
     "\t\tif serr := settleSession(store, stamp, sessionID,\n\t\t\tfmt.Sprintf(\"review nudge send failed: %v\", err)); serr != nil {",
     "\t\tif serr := error(nil); serr != nil {",
     "caught"),
    ("the-loop-guard-never-fires",
     "\tif passes >= maxPasses {",
     "\tif passes >= maxPasses && false {",
     "caught"),
    ("a-dry-run-writes-anyway",
     "\tlogf(\"completed, sending self-review nudge %d/%d (session_id=%s)\", pass, maxPasses, sessionID)\n\tif dryRun {",
     "\tlogf(\"completed, sending self-review nudge %d/%d (session_id=%s)\", pass, maxPasses, sessionID)\n\tif false && dryRun {",
     "caught"),
    # stampBody swallowing the board's refusal is exactly what it used to do, and
    # it silences every stamp report at once.
    ("a-stamp-failure-is-invisible-again",
     "\t\tlog.Printf(\"[%s] stamp body: %v (continuing)\", title, err)\n\t\treturn err",
     "\t\tlog.Printf(\"[%s] stamp body: %v (continuing)\", title, err)\n\t\treturn nil",
     "caught"),

    # --- controls. These must NOT redden, or the suite is going red on anything.
    ("reword-a-log-line-no-test-reads",
     'log.Printf("[reviewer] board=%q in-progress=%d", cfg.boardName, len(inProgress))',
     'log.Printf("[reviewer] reviewing %q; %d cards in progress", cfg.boardName, len(inProgress))',
     "survives"),
    ("change-the-job-name-flag-default",
     '\t\tjobName      = flag.String("job-name", "autoworker-reviewer-tick",',
     '\t\tjobName      = flag.String("job-name", "reviewer-tick",',
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
