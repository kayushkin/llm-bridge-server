#!/usr/bin/env python3
"""Sabotage scorer for hook-store's SetHookEnabled and the enabled filter.

Written by the 201st nightly pass, 2026-08-15, working step 2 of noteboard card
25ac6a72-a5a0-4ada-b4a9-aeeb4e688177 — the sweep of boolean parameters that no
test ever passes both values to.

A test that passes is not a test that works. This script breaks the code the new
tests were written to pin, one edit at a time, and reports whether the suite
noticed. Run it from the repo root:

    python3 scripts/sabotage-set-hook-enabled.py

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

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import tree_hold  # noqa: E402  vendored; see tree_hold.py on keeping copies identical

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PKG = '.'
RUN = 'TestSetHookEnabled|TestListHooksEnabledFilter|TestListApplicable|TestCRUD'
CASES = [
    {
        "name": "SetHookEnabled writes the inverse value",
        "file": "hook.go",
        "find": "\t\tenabled, time.Now().UTC(), id)",
        "replace": "\t\t!enabled, time.Now().UTC(), id)"
    },
    {
        "name": "SetHookEnabled ignores its argument and always disables",
        "file": "hook.go",
        "find": "\t\tenabled, time.Now().UTC(), id)",
        "replace": "\t\tfalse, time.Now().UTC(), id)"
    },
    {
        "name": "SetHookEnabled deletes the row instead of flagging it",
        "file": "hook.go",
        "find": "\tres, err := s.db.Exec(`UPDATE hooks SET enabled = ?, updated_at = ? WHERE id = ?`,\n\t\tenabled, time.Now().UTC(), id)",
        "replace": "\tres, err := s.db.Exec(`DELETE FROM hooks WHERE id = ?`, id)\n\t_ = enabled"
    },
    {
        "name": "SetHookEnabled narrows its WHERE so a no-op write reads as not-found",
        "file": "hook.go",
        "find": "`UPDATE hooks SET enabled = ?, updated_at = ? WHERE id = ?`",
        "replace": "`UPDATE hooks SET enabled = ?, updated_at = ? WHERE id = ? AND enabled != ?`"
    },
    {
        "name": "scanHook reads the INTEGER column backwards",
        "file": "hook.go",
        "find": "\th.Enabled = enabled != 0",
        "replace": "\th.Enabled = enabled == 0"
    },
    {
        "name": "ListHooks applies the enabled filter even when EnabledSet is false",
        "file": "hook.go",
        "find": "\tif filter.EnabledSet {\n\t\tquery += ` AND enabled = ?`",
        "replace": "\tif true {\n\t\tquery += ` AND enabled = ?`"
    },
    {
        "name": "ListHooks ignores the enabled filter entirely",
        "file": "hook.go",
        "find": "\tif filter.EnabledSet {\n\t\tquery += ` AND enabled = ?`\n\t\targs = append(args, filter.Enabled)\n\t}",
        "replace": "\tif false {\n\t\tquery += ` AND enabled = ?`\n\t\targs = append(args, filter.Enabled)\n\t}"
    },
    {
        "name": "ListApplicable stops filtering on enabled",
        "file": "hook.go",
        "find": "\t\tWHERE enabled = 1\n\t\t  AND harness = ?",
        "replace": "\t\tWHERE harness = ?"
    }
]


def run_tests(selector):
    r = subprocess.run(
        ["go", "test", PKG, "-run", selector, "-count=1"],
        cwd=REPO, capture_output=True, text=True,
    )
    return r.returncode == 0


def main():
    """Take this tree exclusively, then run. Call this, not `main_on_a_held_tree`.

    Card `d869d2be`. Every verdict below is read off the SUITE'S exit code, and that
    exit code belongs to the whole tree rather than to the mutation this run wrote. A
    second run mutating the same tree hands this one a red suite it did not cause, and
    this one records it as CAUGHT — the collision does not add noise, it **inflates the
    score**, and these scores are the numbers the nightly write-ups quote.

    It refuses rather than waits. A run told to come back later can say so and exit;
    one silently blocked for the length of somebody else's suite looks hung. The
    refusal exits non-zero, because a caller reading exit 0 would read "measured, and
    clean" from a run that measured nothing.

    The restore-on-signal handling below is a different guard. It stops THIS run
    leaving a mutation behind. It cannot see a concurrent run at all, because the
    other run restores each file before its next case and the tree is clean between
    mutations exactly when it is most dangerous to trust.
    """
    with tree_hold.exclusive_hold_on_tree(
            REPO, purpose=os.path.basename(sys.argv[0] or "sabotage-set-hook-enabled")) as refusal:
        if refusal:
            sys.exit("REFUSING: " + refusal)
        return main_on_a_held_tree()


def main_on_a_held_tree():
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
