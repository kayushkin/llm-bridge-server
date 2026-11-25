#!/usr/bin/env python3
"""Score reminder-coordinator's shortfall-reporting tests by breaking it on purpose.

Same three outcomes as scripts/score-degradations.py, never two: CAUGHT (the
named test failed), MISSED (it passed with the defect planted), ABORTED (the
package did not build, the anchor was not unique, or the baseline was already
red — so nothing was measured). An ABORT scored as a catch is the guard defect
card 03789f03 records, and it is reported separately here.

The repository is resolved from this file's own location rather than named as a
constant, so the script scores the checkout it was run from and prints which
one. A named worktree path would silently score a different tree than the one
being edited (card cdb2e1fc).
"""
import pathlib, subprocess, sys

REPO = pathlib.Path(__file__).resolve().parent.parent
CMD = "cmd/reminder-coordinator/main.go"

# (arm name, file, find, replace, test that MUST go red)
ARMS = [
 # --- the shortfall the command exists to report ---
 ("calendar-shortfall-not-recorded", CMD,
  '\t\tc.Degraded.Record("today\'s calendar could not be read, so the digest and the nudge went out without the day\'s schedule: %v", calErr)\n', "",
  "TestACalendarReadFailureIsRecordedAsAShortfall"),
 ("shortfall-drops-the-cause", CMD,
  'c.Degraded.Record("today\'s calendar could not be read, so the digest and the nudge went out without the day\'s schedule: %v", calErr)',
  'c.Degraded.Record("today\'s calendar could not be read, so the digest and the nudge went out without the day\'s schedule")',
  "TestACalendarReadFailureIsRecordedAsAShortfall"),

 # --- the other direction: a deliberate skip must NOT be a shortfall ---
 ("a-quiet-tick-is-recorded", CMD,
  "\tif err := c.syncCalendar(); err != nil {\n",
  '\tc.Degraded.Record("planted: every tick falls short")\n\tif err := c.syncCalendar(); err != nil {\n',
  "TestATickWithNothingToSayIsNotAShortfall"),
 ("an-exhausted-series-is-recorded", CMD,
  '\t\t\tlog.Printf("rolling series exhausted, leaving done: %q", item.Title)\n',
  '\t\t\tlog.Printf("rolling series exhausted, leaving done: %q", item.Title)\n\t\t\tc.Degraded.Record("planted: %q is exhausted", item.Title)\n',
  "TestAnExhaustedRollingSeriesIsNotAShortfall"),
 ("a-delivered-nudge-is-recorded", CMD,
  "\t// Record what was said, so the next tick 15 minutes from now stays quiet.\n",
  '\tc.Degraded.Record("planted: the nudge went out")\n\t// Record what was said, so the next tick 15 minutes from now stays quiet.\n',
  "TestAFullTickThatDidItsWholeJobIsGreen"),

 # --- the verdict has to survive the trip out of the process ---
 ("run-exits-zero-regardless", CMD,
  "\treturn coordinator.Degraded.ExitCode()\n", "\treturn 0\n",
  "TestEveryExitPathAfterTheCoordinatorSettlesTheVerdict"),
 ("main-drops-the-status", CMD,
  "func main() {\n\tos.Exit(run())\n}", "func main() {\n\t_ = run()\n\tos.Exit(0)\n}",
  "TestMainExitsWithTheStatusRunReturned"),
]

PKG = "./cmd/reminder-coordinator"


def run_test(pkg, name):
    """Returns (verdict, detail). Never dereferences a value it did not get."""
    proc = subprocess.run(["go", "test", pkg, "-run", "^" + name + "$", "-count=1"],
                          cwd=REPO, capture_output=True, text=True)
    out = (proc.stdout or "") + (proc.stderr or "")
    if "build failed" in out or "cannot use" in out or "undefined:" in out or "syntax error" in out:
        return "ABORTED", "package did not build"
    if "no tests to run" in out:
        return "ABORTED", "the named test does not exist"
    if proc.returncode == 0:
        return "PASSED", ""
    lines = out.strip().splitlines()
    return "FAILED", (lines[-1][:120] if lines else "")


results = []
for name, rel, find, repl, test in ARMS:
    path = REPO / rel
    original = path.read_text()
    if original.count(find) != 1:
        results.append((name, "ABORTED", f"anchor appears {original.count(find)} times, want 1"))
        continue
    # the case must be able to go GREEN before it is asked to go red
    baseline, detail = run_test(PKG, test)
    if baseline != "PASSED":
        results.append((name, "ABORTED", f"baseline is not green: {baseline} {detail}"))
        continue
    path.write_text(original.replace(find, repl, 1))
    try:
        verdict, detail = run_test(PKG, test)
    finally:
        path.write_text(original)
    if verdict == "ABORTED":
        results.append((name, "ABORTED", detail))
    elif verdict == "FAILED":
        results.append((name, "CAUGHT", test))
    else:
        results.append((name, "MISSED", test + " passed with the defect planted"))

width = max(len(n) for n, _, _ in results)
for name, verdict, detail in results:
    print(f"{name:<{width}}  {verdict:<8} {detail}")
caught = sum(1 for _, v, _ in results if v == "CAUGHT")
missed = sum(1 for _, v, _ in results if v == "MISSED")
aborted = sum(1 for _, v, _ in results if v == "ABORTED")
print(f"\nscored in {REPO}")
print(f"CAUGHT {caught}  MISSED {missed}  ABORTED {aborted}  of {len(results)} arms")
sys.exit(0)
