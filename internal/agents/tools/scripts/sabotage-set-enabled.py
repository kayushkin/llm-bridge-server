#!/usr/bin/env python3
"""Sabotage scorer for tool-store's SetEnabled, both values.

Written by the 201st nightly pass, 2026-08-15, working step 2 of noteboard card
25ac6a72-a5a0-4ada-b4a9-aeeb4e688177 — the sweep of boolean parameters that no
test ever passes both values to.

A test that passes is not a test that works. This script breaks the code the new
tests were written to pin, one edit at a time, and reports whether the suite
noticed. Run it from the repo root:

    python3 scripts/sabotage-set-enabled.py

Exit 0 means every mutation was caught. Exit 3 means at least one survived, and
the survivor is named — a surviving mutation is a hole in the tests, not a
failure of this script.

Two rules are enforced here because earlier passes lost time to both:

  * Every needle must occur EXACTLY ONCE in its file, checked before the write.
    A needle that matches nothing produces a green run indistinguishable from a
    surviving mutation. The 201st pass hit exactly this on its first attempt and
    briefly recorded a false survivor.
  * The unmutated tree must be green first. Scoring a red suite measures nothing.

The MINE column below (`-run` filtered to the new tests) is not attribution on
its own. To ask what the new tests added, move the new test file out of the
package and re-run: that is the PRIOR column, and it is the only one that can
distinguish "these tests catch it" from "something in the package already did".
Comparing a filtered run against the whole package cannot — the package contains
the filter.
"""
import json
import os
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PKG = '.'
RUN = 'TestSetEnabled|TestGlobalDisable|TestReEnabling|TestDeleteAndSetEnabled'
CASES = [
    {
        "name": "SetEnabled writes the inverse value",
        "file": "store.go",
        "find": "res, err := s.db.Exec(`UPDATE tools SET enabled=?, updated_at=? WHERE id=?`,\n\t\tboolToInt(enabled), now(), id)",
        "replace": "res, err := s.db.Exec(`UPDATE tools SET enabled=?, updated_at=? WHERE id=?`,\n\t\tboolToInt(!enabled), now(), id)"
    },
    {
        "name": "SetEnabled ignores the argument and always disables",
        "file": "store.go",
        "find": "boolToInt(enabled), now(), id)",
        "replace": "boolToInt(false), now(), id)"
    },
    {
        "name": "SetEnabled narrows its WHERE so a no-op write reads as not-found",
        "file": "store.go",
        "find": "`UPDATE tools SET enabled=?, updated_at=? WHERE id=?`",
        "replace": "`UPDATE tools SET enabled=?, updated_at=? WHERE id=? AND enabled != ?`"
    },
    {
        "name": "a global disable cascades a DELETE over instance opt-ins",
        "file": "store.go",
        "find": "func (s *Store) SetEnabled(id int64, enabled bool) error {\n\tres, err := s.db.Exec(",
        "replace": "func (s *Store) SetEnabled(id int64, enabled bool) error {\n\tif !enabled {\n\t\ts.db.Exec(`DELETE FROM instance_tools WHERE tool_id=?`, id)\n\t}\n\tres, err := s.db.Exec("
    },
    {
        "name": "EnableForInstance stops refusing a globally-disabled tool",
        "file": "store.go",
        "find": "\tif !t.Enabled {\n\t\treturn ErrGloballyDisabled\n\t}",
        "replace": "\tif false {\n\t\treturn ErrGloballyDisabled\n\t}"
    },
    {
        "name": "ListInstanceTools stops filtering on the global flag",
        "file": "store.go",
        "find": "WHERE instance_tools.instance_id = ? AND tools.enabled = 1",
        "replace": "WHERE instance_tools.instance_id = ?"
    }
]


def run_tests(selector):
    r = subprocess.run(
        ["go", "test", PKG, "-run", selector, "-count=1"],
        cwd=REPO, capture_output=True, text=True,
    )
    return r.returncode == 0


def main():
    originals = {}
    for case in CASES:
        path = os.path.join(REPO, case["file"])
        if case["file"] not in originals:
            originals[case["file"]] = open(path).read()

    def restore():
        for name, content in originals.items():
            open(os.path.join(REPO, name), "w").write(content)

    if not run_tests(RUN):
        print("CONTROL FAILED - the suite is red before any mutation. Score is void.")
        return 1
    print("control (unmutated) ..... GREEN\n")

    caught, unnoticed = [], []
    try:
        for case in CASES:
            src = originals[case["file"]]
            n = src.count(case["find"])
            if n != 1:
                print("  ABORT  %s: needle occurs %d times, want exactly 1" % (case["name"], n))
                return 2
            path = os.path.join(REPO, case["file"])
            open(path, "w").write(src.replace(case["find"], case["replace"], 1))
            green = run_tests(RUN)
            restore()
            if green:
                unnoticed.append(case["name"])
                print("  UNNOTICED  " + case["name"])
            else:
                caught.append(case["name"])
                print("  caught     " + case["name"])
    finally:
        restore()

    print("\n%d/%d caught" % (len(caught), len(CASES)))
    if unnoticed:
        print("UNNOTICED:")
        for name in unnoticed:
            print("  - " + name)
        return 3
    return 0


if __name__ == "__main__":
    sys.exit(main())
