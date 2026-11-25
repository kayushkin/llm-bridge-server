#!/usr/bin/env python3
"""Score the shortfall-reporting tests by breaking the binary on purpose.

Three outcomes, never two: CAUGHT (the named test failed), MISSED (it passed
with the defect planted), ABORTED (the package did not build, or the mutation
did not apply, so nothing was measured). An ABORT scored as a catch is the
guard defect card 03789f03 records; it is reported separately here.
"""
import pathlib, subprocess, sys

# The tree this script sits in, so it scores the checkout it was run from and
# never a worktree somebody else parked (card cdb2e1fc).
REPO = pathlib.Path(__file__).resolve().parent.parent

# (arm name, file, find, replace, test that MUST go red)
ARMS = [
 ("model-refusal-not-recorded", "cmd/kanban-classifier/main.go",
  '\t\tdegraded.Record("session %s not classified: the model call failed", sess.SessionID)\n', "",
  "TestARefusedModelCallDegradesTheRun"),
 ("card-move-failure-not-recorded", "cmd/kanban-classifier/main.go",
  '\t\t\tdegraded.Record("session %s: card %s not moved to %q", sess.SessionID, ec.CardID, colName)\n', "",
  "TestACardThatCannotMoveDegradesTheRun"),
 ("lost-signature-not-recorded", "cmd/kanban-classifier/main.go",
  '\t\tdegraded.Record("session %s: check signature not written, it will be re-classified and re-billed next tick", sess.SessionID)\n', "",
  "TestAnUnwritableSignatureDegradesTheRun"),
 ("unreadable-bridge-not-recorded", "cmd/kanban-classifier/main.go",
  '\t\tdegraded.Record("session %s not classified: reading its messages failed", sess.SessionID)\n', "",
  "TestAnUnreadableSessionDegradesTheRun"),
 ("unreadable-board-not-recorded", "cmd/kanban-classifier/main.go",
  '\t\tdegraded.Record("session %s not classified: list tags failed", sess.SessionID)\n', "",
  "TestAnUnreadableBoardDegradesTheRun"),
 ("a-deliberate-skip-is-recorded", "cmd/kanban-classifier/main.go",
  "\tcurSig := buildSig(page.newestFinishedResponseID, eventCount)\n",
  '\tcurSig := buildSig(page.newestFinishedResponseID, eventCount)\n\tdegraded.Record("session %s: skipped", sess.SessionID)\n',
  "TestASkippedSessionIsNotAShortfall"),
 ("sweep-exits-zero-regardless", "cmd/kanban-classifier/main.go",
  '\t// model call is one of the shortfalls the recorder holds.\n\treturn degraded.ExitCode()\n',
  '\t// model call is one of the shortfalls the recorder holds.\n\treturn 0\n',
  "TestEveryExitPathAfterTheRecorderSettlesTheVerdict"),
 ("main-drops-the-status", "cmd/kanban-classifier/main.go",
  "func main() {\n\tos.Exit(run())\n}", "func main() {\n\t_ = run()\n\tos.Exit(0)\n}",
  "TestMainExitsWithTheStatusRunReturned"),
 ("exitcode-always-zero", "internal/degradations/degradations.go",
  "\tif !r.HasShortfall() {\n\t\treturn 0\n\t}\n\tr.Report()\n\treturn 1",
  "\tr.Report()\n\treturn 0",
  "TestSkippedAccountExitsNonZero"),
 ("report-drops-the-subject", "internal/degradations/degradations.go",
  '\tlog.Printf("[%s] run degraded: %d shortfall(s) — %s",\n\t\tr.subject, len(r.reasons), strings.Join(r.reasons, "; "))',
  '\tlog.Printf("[classifier] run degraded: %d shortfall(s) — %s",\n\t\tlen(r.reasons), strings.Join(r.reasons, "; "))',
  "TestReportedLineNamesTheSubject"),
 ("report-lists-only-the-first", "internal/degradations/degradations.go",
  'strings.Join(r.reasons, "; "))', 'strings.Join(r.reasons[:1], "; "))',
  "TestReportCountsAndListsEveryReason"),
 ("a-clean-run-still-reports", "internal/degradations/degradations.go",
  "\tif !r.HasShortfall() {\n\t\treturn 0\n\t}\n\tr.Report()", "\tr.Report()\n\tif !r.HasShortfall() {\n\t\treturn 0\n\t}",
  "TestCleanRunPrintsNothing"),
 ("empty-subject-tolerated", "internal/degradations/degradations.go",
  '\tif subject == "" {\n\t\tpanic("degradations.New: empty subject — the summary line must name the run that fell short")\n\t}\n', "",
  "TestEmptySubjectPanics"),
]

def run_test(pkg, name):
    """Returns (verdict, detail). Never dereferences a value it did not get."""
    proc = subprocess.run(["go", "test", pkg, "-run", "^"+name+"$", "-count=1"],
                          cwd=REPO, capture_output=True, text=True)
    out = (proc.stdout or "") + (proc.stderr or "")
    if "build failed" in out or "[build failed]" in out or "cannot use" in out or "undefined:" in out:
        return "ABORTED", "package did not build"
    if "no tests to run" in out or "warning: no tests to run" in out:
        return "ABORTED", "the named test does not exist"
    if proc.returncode == 0:
        return "PASSED", ""
    return "FAILED", out.strip().splitlines()[-1][:120] if out.strip() else ""

def package_of(rel):
    return "./" + str(pathlib.Path(rel).parent)

results = []
for name, rel, find, repl, test in ARMS:
    path = REPO / rel
    original = path.read_text()
    if original.count(find) != 1:
        results.append((name, "ABORTED", f"anchor appears {original.count(find)} times, want 1"))
        continue
    pkg = package_of(rel)
    # the case must be able to go GREEN before it is asked to go red
    baseline, detail = run_test(pkg, test)
    if baseline != "PASSED":
        results.append((name, "ABORTED", f"baseline is not green: {baseline} {detail}"))
        continue
    path.write_text(original.replace(find, repl, 1))
    try:
        verdict, detail = run_test(pkg, test)
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
