#!/usr/bin/env python3
"""The sabotage-scoring engine, shared by every per-binary case file.

A test suite that passes tells you nothing on its own. A scorer applies one
edit per mechanism to the file under test, runs the suite, and records whether
anything went red.

Rules this engine carries, each one inherited from a night that lost time to
its absence:

  - The needle must be found. A replacement whose search string is not in the
    file is a case that silently did nothing and scored UNNOTICED (30th pass).
  - The file's bytes must actually change. An edit that changed nothing scores
    UNNOTICED for free, which is the same false result (33rd).
  - A case may carry a SECOND edit, so an orphaned import or variable reports a
    score instead of `compile error` (32nd, 35th).
  - Two controls. A known-positive (an edit every test must catch) and a
    known-NEGATIVE (an edit no test should catch). Without the negative, a
    harness that reports CAUGHT for everything looks perfect (33rd).
  - No needle text is handed to a shell (25th).
  - Restores from git, so the tree must be committed before running (27th).
  - Prints the diff it actually applied, because the row prints the name you
    gave it, not the edit you made (33rd).

The engine was extracted in the scheduler repo when cmd/ask
became the second binary to need it. Ten unattended passes had each rebuilt
this same logic from prose and hit the same traps doing it; a second copy would
have been the eleventh. A per-binary file supplies TARGET, PACKAGES and CASES
and calls score() — nothing else needs restating.
"""

import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent


@dataclass
class Case:
    name: str
    # (find, replace) pairs. Every find must appear in the file.
    edits: list = field(default_factory=list)
    # A case the suite is NOT expected to catch, and why.
    expected_unnoticed: str = ""


def _run_tests(packages):
    proc = subprocess.run(
        ["go", "test", "-count=1"] + packages,
        cwd=REPO, capture_output=True, text=True,
    )
    out = proc.stdout + proc.stderr
    if ("build failed" in out or "[build failed]" in out
            or "cannot find" in out or "syntax error" in out):
        return "compile error", out
    if proc.returncode == 0:
        return "UNNOTICED", out
    return "CAUGHT", out


def score(target: Path, packages, cases):
    """Apply each case to target, run packages, and print a scored table.

    Pass --diffs to print the edit each case actually applied. A row prints the
    name you gave it, not the edit you made, and mislabelled cases have twice
    been read as coverage holes that were not there — so read the diffs at
    least once per case family.
    """
    show_diffs = "--diffs" in sys.argv
    rel = str(target.relative_to(REPO))

    dirty = subprocess.run(["git", "status", "--porcelain", rel],
                           cwd=REPO, capture_output=True, text=True).stdout.strip()
    if dirty:
        sys.exit("REFUSING: %s has uncommitted changes; this harness restores "
                 "from git and would delete them." % target)

    def restore():
        subprocess.run(["git", "checkout", "--", rel], cwd=REPO, check=True)

    baseline, out = _run_tests(packages)
    if baseline != "UNNOTICED":
        sys.exit("REFUSING: the suite is not green before sabotage (%s)\n%s" % (baseline, out))
    print("baseline: green\n")

    original = target.read_text()
    results = []
    for case in cases:
        text = original
        for find, replace in case.edits:
            if find not in text:
                restore()
                sys.exit("ABORT [%s]: needle not found in %s.\n"
                         "A case whose needle is missing changes nothing and scores "
                         "UNNOTICED, which reads as a coverage hole that is not there.\n"
                         "Needle was:\n%s" % (case.name, target.name, find))
            text = text.replace(find, replace, 1)
        if text == original:
            restore()
            sys.exit("ABORT [%s]: the edit produced an identical file." % case.name)
        target.write_text(text)
        verdict, _ = _run_tests(packages)
        # Read the diff the harness actually applied, not the label it was given.
        diff_cmd = ["git", "diff"] + ([] if show_diffs else ["--stat"]) + ["--", rel]
        diff = subprocess.run(diff_cmd, cwd=REPO, capture_output=True, text=True).stdout.strip()
        restore()
        results.append((case, verdict, diff))
        print("%-14s %s" % (verdict, case.name))
        if show_diffs:
            body = "\n".join(l for l in diff.split("\n")
                             if l.startswith(("+", "-")) and not l.startswith(("+++", "---")))
            print("\n".join("        " + l for l in body.split("\n")) + "\n")

    print("\n================ score ================")
    caught = sum(1 for c, v, _ in results
                 if v == "CAUGHT" and not c.name.startswith("CONTROL"))
    real = [c for c, _, _ in results if not c.name.startswith("CONTROL")]
    print("%d/%d real mechanisms caught" % (caught, len(real)))

    problems = []
    for case, verdict, _ in results:
        if case.name.startswith("CONTROL known-positive") and verdict != "CAUGHT":
            problems.append("the known-positive control was NOT caught — the suite is not running")
        if case.name.startswith("CONTROL known-negative") and verdict == "CAUGHT":
            problems.append("the known-negative control WAS caught — the suite is red for a "
                            "reason unrelated to behaviour, so every CAUGHT above is suspect")
        if verdict == "compile error" and not case.expected_unnoticed:
            problems.append("compile error, not a score: %s" % case.name)
        if verdict == "UNNOTICED" and not case.expected_unnoticed:
            problems.append("UNNOTICED: %s" % case.name)
    for p in problems:
        print("  ⚠️  " + p)
    if not problems:
        print("  both controls behaved; every real mechanism is pinned")
    return results
