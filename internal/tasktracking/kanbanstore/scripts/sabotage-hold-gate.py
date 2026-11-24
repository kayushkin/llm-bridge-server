#!/usr/bin/env python3
"""Score internal/api's hold-gate tests by breaking the API on purpose.

    python3 scripts/sabotage-hold-gate.py [--diffs]

A passing suite is not evidence. These tests were written because `holdCard(w, r,
cardID, hold bool)` had never been called by any test: POST /cards/{id}/hold and
POST /cards/{id}/unhold reach that one function and are told apart by that one
argument, so nothing pinned which of the two a request actually performed.

What the gap costs is specific. noteboard withholds held items from the agent
read paths, so a hold is how the user parks work an unattended agent must not
pick up. An inverted gate does not fail loudly — pressing hold clears the hold,
and the next unattended pass sees the card as ordinary open work.

Two columns, and the difference between them is the point:

  MINE      only the hold-gate tests in internal/api/hold_gate_test.go
  PRIOR     the rest of ./internal/api/, with that file taken away entirely

MINE answers "did the tests I wrote catch this?". PRIOR answers the question
that decides whether they were worth writing: "would this have been caught
anyway?". PRIOR is measured by moving the file out of the package, not with a
`-run` filter: the unfiltered package CONTAINS the new tests, so it reddens
whenever MINE does and the comparison would report "necessary" for every row by
construction.

Case-writing rules, inherited from the fleet's other scorers:

  - Prefer a DRIFTED VALUE to a deletion: it orphans no identifier and is the
    likelier real regression. `go test` runs vet, so an edit that fails to
    compile reports a build error instead of a score.
  - Two controls, not one. Only a cry-wolf control and everything reports
    CAUGHT; only a known-negative and you cannot tell a working suite from a
    harness that never runs the tests.
  - Every needle is asserted to occur exactly once before the run. A needle that
    matches nothing is indistinguishable from a surviving mutation, and zero
    matches is green.

⚠️ Region this scorer does NOT cover, declared rather than left to be
rediscovered: noteboard's own hold semantics (this suite drives a fake), the
roll-up of a hold over parent_id, the spend ceiling, and every other card route.
"""

import argparse
import difflib
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
TARGET = REPO / "internal/api/api.go"
PACKAGE = "./internal/api/"
MINE = "TestHoldParksTheCardAndUnholdReleasesIt|TestHoldGateRejects"

MY_TESTS = REPO / "internal/api/hold_gate_test.go"
PARKED = REPO / "internal/api/hold_gate_test.go.parked"


@dataclass
class Case:
    name: str
    find: str
    replace: str
    expect_caught: bool
    why: str


CASES = [
    # ---------------- controls ----------------
    Case(
        name="CONTROL known-negative: reword holdCard's 405 message",
        find='hold bool) {\n\tif r.Method != "POST" {\n\t\twriteError(w, 405, "method not allowed")',
        replace='hold bool) {\n\tif r.Method != "POST" {\n\t\twriteError(w, 405, "method not supported")',
        expect_caught=False,
        why=(
            "The status code carries the behavior; the sentence does not. If "
            "this reports CAUGHT the suite is asserting on prose, and every "
            "other CAUGHT below is suspect."
        ),
    ),
    Case(
        name="CONTROL cry-wolf: holdCard refuses every request",
        find='\tvar item noteboard.Item\n\tvar err error\n',
        replace='\twriteError(w, 500, "sabotage: hold disabled")\n\treturn\n\tvar item noteboard.Item\n\tvar err error\n',
        expect_caught=True,
        why=(
            "Breaks the endpoint outright. If this reports UNNOTICED the harness "
            "is not running the tests and every UNNOTICED below is a lie."
        ),
    ),
    # ---------------- real mechanisms ----------------
    Case(
        name="holdCard ignores its argument and always holds",
        find="\tif hold {\n\t\tvar req struct {",
        replace="\tif true {\n\t\tvar req struct {",
        expect_caught=True,
        why=(
            "This card's thesis in its cleanest form: the boolean parameter can "
            "be deleted outright. Unhold silently becomes a second way to hold, "
            "so the user's stop button never releases."
        ),
    ),
    Case(
        name="holdCard inverts its argument",
        find="\tif hold {\n\t\tvar req struct {",
        replace="\tif !hold {\n\t\tvar req struct {",
        expect_caught=True,
        why=(
            "The failure that is silent in the dangerous direction: pressing "
            "hold CLEARS the hold, and the parked card is handed to the next "
            "unattended pass as ordinary open work."
        ),
    ),
    Case(
        name="the unhold route asks for a hold",
        find='\tcase "unhold":\n\t\ta.holdCard(w, r, cardID, false)',
        replace='\tcase "unhold":\n\t\ta.holdCard(w, r, cardID, true)',
        expect_caught=True,
        why=(
            "The same defect one layer up. holdCard can be perfect and the gate "
            "still one-way, because the argument is chosen by the router."
        ),
    ),
    Case(
        name="the hold reason is dropped",
        find="a.noteboard.HoldItem(cardID, req.Reason)",
        replace='a.noteboard.HoldItem(cardID, "")',
        expect_caught=True,
        why=(
            "The reason is the only record of WHY work was parked. Dropping it "
            "leaves a hold that still holds, so nothing about the gate's "
            "behavior reveals the loss."
        ),
    ),
    Case(
        name="a noteboard failure is reported as success",
        find="item, err = a.noteboard.UnholdItem(cardID)\n\t}\n\tif err != nil {\n\t\twriteError(w, 502, err.Error())\n\t\treturn",
        replace="item, err = a.noteboard.UnholdItem(cardID)\n\t}\n\tif err != nil {\n\t\twriteJSON(w, 200, map[string]any{})\n\t\treturn",
        expect_caught=True,
        why=(
            "The gate is remote: the hold lives on the noteboard item, not on "
            "this board. A hold that failed upstream but answered 200 reads to "
            "the caller as parked work that was never parked."
        ),
    ),
    Case(
        name="a non-POST request is accepted",
        find='hold bool) {\n\tif r.Method != "POST" {',
        replace='hold bool) {\n\tif r.Method == "NEVER" {',
        expect_caught=True,
        why=(
            "Makes GET /cards/{id}/hold mutate. A gate that a link-follower or "
            "a prefetch can close is not a gate."
        ),
    ),
]


def run_tests(run_filter=None):
    cmd = ["go", "test", PACKAGE, "-count=1", "-v"]
    if run_filter:
        cmd += ["-run", run_filter]
    proc = subprocess.run(cmd, cwd=REPO, capture_output=True, text=True)
    out = proc.stdout + proc.stderr
    failing = sorted({
        line.split()[2]
        for line in out.splitlines()
        if line.strip().startswith("--- FAIL:") and len(line.split()) > 2
    })
    return proc.returncode == 0, failing, out


def run_without_my_tests():
    MY_TESTS.rename(PARKED)
    try:
        return run_tests()
    finally:
        PARKED.rename(MY_TESTS)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--diffs", action="store_true",
                        help="print the applied diff for every case")
    args = parser.parse_args()

    original = TARGET.read_text()

    for case in CASES:
        found = original.count(case.find)
        if found != 1:
            print(f"REFUSED: needle for {case.name!r} occurs {found} times in "
                  f"{TARGET.name}; a scorer that edits the wrong line measures nothing.")
            return 2

    baseline_mine, _, out_mine = run_tests(MINE)
    baseline_prior, _, out_prior = run_without_my_tests()
    if not (baseline_mine and baseline_prior):
        print("REFUSED: the suite is already red before any sabotage.\n"
              + out_mine + out_prior)
        return 2
    print(f"baseline: {PACKAGE} green with and without hold_gate_test.go\n")

    results = []
    try:
        for case in CASES:
            TARGET.write_text(original.replace(case.find, case.replace, 1))
            if args.diffs:
                diff = difflib.unified_diff(
                    case.find.splitlines(), case.replace.splitlines(),
                    lineterm="", n=0, fromfile="before", tofile="after")
                print(f"--- {case.name}\n" + "\n".join(diff) + "\n")
            mine_ok, mine_failing, _ = run_tests(MINE)
            prior_ok, prior_failing, _ = run_without_my_tests()
            results.append((case, not mine_ok, mine_failing, not prior_ok, prior_failing))
    finally:
        TARGET.write_text(original)
        if PARKED.exists() and not MY_TESTS.exists():
            PARKED.rename(MY_TESTS)

    width = max(len(c.name) for c in CASES)
    print(f"{'case'.ljust(width)}  MINE       PRIOR")
    print("-" * (width + 21))
    problems = []
    for case, mine_caught, mine_failing, prior_caught, _ in results:
        mine = "CAUGHT" if mine_caught else "unnoticed"
        prior = "CAUGHT" if prior_caught else "unnoticed"
        print(f"{case.name.ljust(width)}  {mine.ljust(9)}  {prior}")
        if mine_caught != case.expect_caught:
            want = "caught" if case.expect_caught else "unnoticed"
            problems.append(f"{case.name}: MINE={mine}, expected {want}. {case.why}")

    print()
    for case, mine_caught, mine_failing, _, _ in results:
        if mine_caught:
            print(f"  {case.name}\n    reddened: "
                  f"{', '.join(mine_failing) or '(build/vet error — inspect)'}")

    real = [c for c in CASES if not c.name.startswith("CONTROL")]
    caught = sum(1 for c, m, _, _, _ in results if m and not c.name.startswith("CONTROL"))
    print(f"\n{caught}/{len(real)} real mechanisms caught by the tests under test.")

    exclusive = [c.name for c, m, _, p, _ in results
                 if m and not p and not c.name.startswith("CONTROL")]
    print(f"{len(exclusive)} of them go unnoticed with hold_gate_test.go removed, "
          f"so nothing pinned them before:")
    for name in exclusive:
        print("  - " + name)

    if problems:
        print("\nPROBLEMS:")
        for p in problems:
            print("  - " + p)
        return 1
    print("\nBoth controls behaved; every real mechanism is pinned.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
