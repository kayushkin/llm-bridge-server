#!/usr/bin/env python3
"""Score cmd/kanban-dispatcher's degradation tests by breaking the code they pin.

Ported from 20d9d87's scorer and re-aimed at main's dispatcher, which rebuilt
the tick around runOptions and backlogDispatcher after that branch forked.

A green suite is a claim about a suite, not about the code. This plants one
defect at a time and asks whether anything goes red.

Three outcomes, not two. A mutation that does not COMPILE exits non-zero exactly
as a red suite does, so every arm is judged on the output as well: `build
failed`, `undefined:` and friends are ABORTED, and an ABORTED arm is a hole in
the scorer, not a win. Silencing a Record turns it into `_ = fmt.Sprintf(` and
dropping a name uses `%.0s`, so both keep every argument used and still build.

Both directions are armed. Job 16 logs `scoped-parents=0` and `dispatched=0` on
nearly every tick, so an arm that turns a deliberate skip into a shortfall must
be CAUGHT, or the job paints red every five minutes and its exit code stops
meaning anything. The sharpest is the 404: a sibling session that no longer
EXISTS is not one the tick could not read.

The repository is resolved from __file__ and printed, so running this from one
checkout can never score another. Files are restored from bytes captured in
this process, never with `git checkout`, so uncommitted work scores as itself.
"""
import pathlib
import re
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
MAIN = REPO / "cmd" / "kanban-dispatcher" / "main.go"
PKG = "./cmd/kanban-dispatcher/"


def silence(message_start):
    """An arm that turns the one Record whose format starts so into a no-op."""
    return ('degraded.Record("' + message_start, '_ = fmt.Sprintf("' + message_start)


def record_after(anchor, reason, recorder="degraded"):
    """An arm that records a shortfall right after anchor — over-recording."""
    indent = re.match(r"\t*", anchor.split("\n")[-2] if anchor.endswith("\n") else anchor).group(0)
    return (anchor, anchor + indent + recorder + '.Record("' + reason + '")\n')


# name -> (old, new, expectation)
#   "caught"   = at least one test must go red
#   "survives" = the suite must stay green (a control)
ARMS = [
    # ------------------------------------------------ each Record silenced
    ("drop-the-unreadable-scoped-tag-record",
     *silence("goal %s (%q): its scoped tag could not be read"), "caught"),
    ("drop-the-unlistable-children-record",
     *silence("goal %s: its children could not be listed"), "caught"),
    ("drop-the-unreadable-sibling-links-record",
     *silence("goal %s group %q: sibling card %s could not be read"), "caught"),
    ("drop-the-unreadable-session-state-record",
     *silence("goal %s group %q: the state of sibling session"), "caught"),
    ("drop-the-unlistable-siblings-record",
     *silence("goal %s group %q: its sibling cards could not be listed"), "caught"),
    ("drop-the-closure-children-record",
     *silence("goal %s (%s): its children could not be listed for the closure pass"), "caught"),
    ("drop-the-unreadable-placement-record",
     *silence("goal %s (%s): child %s is %s but its placement could not be read"), "caught"),
    ("drop-the-refused-closure-move-record",
     *silence("goal %s (%s): child %s is %s but the board refused to move it"), "caught"),
    ("drop-the-rollup-children-record",
     *silence("goal %s (%s): its children could not be listed for the rollup"), "caught"),
    ("drop-the-refused-rollup-note-record",
     *silence("goal %s (%s): the board refused its rollup note"), "caught"),
    ("drop-the-refused-rollup-move-record",
     *silence("goal %s (%s): all %d of its children are settled"), "caught"),
    ("drop-the-undispatched-card-record",
     *silence("card %s %q was not dispatched"), "caught"),

    # ------------------------------------------------ the verdict
    ("the-run-always-succeeds",
     "\tif degraded.HasShortfall() {", "\tif false && degraded.HasShortfall() {", "caught"),
    ("only-the-first-shortfall-is-kept",
     "reasons := degraded.Reasons()", "reasons := degraded.Reasons()[:1]", "caught"),

    # ------------------------------------------------ a report that names nothing
    ("the-scoped-tag-report-does-not-name-the-goal",
     '"goal %s (%q): its scoped tag', '"goal %.0s(%q): its scoped tag', "caught"),
    ("the-unreadable-sibling-report-does-not-name-the-sibling",
     "sibling card %s could not be read", "sibling card %.0scould not be read", "caught"),
    ("the-unreadable-session-state-report-does-not-name-the-session",
     "the state of sibling session %s could not be read", "the state of sibling session %.0scould not be read", "caught"),
    ("the-unreadable-placement-report-does-not-name-the-child",
     "child %s is %s but its placement", "child %.0sis %s but its placement", "caught"),
    ("the-refused-closure-move-report-does-not-name-the-child",
     "child %s is %s but the board refused", "child %.0sis %s but the board refused", "caught"),
    ("the-undispatched-report-does-not-name-the-card",
     '"card %s %q was not dispatched', '"card %.0s%.0q was not dispatched', "caught"),

    # ------------------------------------------------ nothing to do is not a shortfall
    ("a-gone-session-is-a-shortfall",
     "\t\tif errors.Is(err, errSessionGone) {", "\t\tif false && errors.Is(err, errSessionGone) {", "caught"),
    ("a-busy-session-is-a-shortfall",
     *record_after('\t\tif session.State != "idle" {\n', "busy sibling"), "caught"),
    ("an-unsettled-child-is-a-shortfall",
     *record_after("\t\t\t\t// Still being worked on.\n", "unsettled child"), "caught"),
    ("a-child-not-on-this-board-is-a-shortfall",
     *record_after("nothing to move\", p.title(), shortID(e.CardID), status)\n\t\t\t\t", "not on this board"), "caught"),
    ("the-per-tick-cap-is-a-shortfall",
     *record_after("\t\t\tif dispatched >= d.maxDispatch {\n", "cap", recorder="d.degraded"), "caught"),
    ("a-goal-with-no-children-is-a-shortfall",
     "\t\tif total == 0 || settled < total {\n",
     "\t\tif total == 0 {\n\t\t\tdegraded.Record(\"no children\")\n\t\t}\n\t\tif total == 0 || settled < total {\n", "caught"),

    # ------------------------------------------------ behaviour the records sit on
    ("a-failed-children-read-reads-as-no-children",
     '\t\treturn 0, 0, "", err', '\t\treturn 0, 0, "", nil', "caught"),
    ("a-refused-rollup-note-stops-the-move",
     "p.cardID, p.title(), err, total)\n\t\t}", "p.cardID, p.title(), err, total)\n\t\t\tcontinue\n\t\t}", "caught"),
    ("the-closure-pass-writes-on-a-dry-run",
     "\t\t\tif options.DryRun {", "\t\t\tif false && options.DryRun {", "caught"),
    ("the-rollup-writes-on-a-dry-run",
     "children settled\", p.title(), settled, total)\n\t\tif options.DryRun {",
     "children settled\", p.title(), settled, total)\n\t\tif false && options.DryRun {", "caught"),
    ("the-placement-read-drops-the-service-token",
     "placements, err := kc.ListCardPlacements(cardID)",
     'placements, err := kanbanclient.New(kc.BaseURL, "").ListCardPlacements(cardID)', "caught"),

    # ------------------------------------------------ controls
    ("control-reword-the-rollup-progress-line",
     '"[%s] rollup: %d/%d children settled"', '"[%s] rollup ready: %d of %d children settled"', "survives"),
    ("control-reword-the-not-on-this-board-line",
     "is not placed on this board — nothing to move", "has no placement on this board, so there is nothing to move", "survives"),
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
        names = sorted(set(re.findall(r"--- FAIL: (\w+)", out)))
        if expect == "caught":
            if red:
                print(f"  CAUGHT   {name}  by {', '.join(names) or 'the package'}")
                caught += 1
            else:
                print(f"  MISSED   {name}  -- no test noticed")
                missed += 1
        else:
            if red:
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
