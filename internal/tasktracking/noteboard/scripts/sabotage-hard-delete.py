#!/usr/bin/env python3
"""Score internal/db's hard-delete tests by breaking the store on purpose.

    python3 scripts/sabotage-hard-delete.py [--diffs]

A passing suite is not evidence. These tests were written because
`DeleteItem(id, hard bool)` had never been called with `true` by any test on
this box, so the branch that runs `DELETE FROM items` — the only statement in
noteboard that destroys a row rather than tombstoning it — had never executed
under test. A suite written for that gap has to be shown capable of going red
for each mechanism it claims to pin, or it is one more thing that reports
success without measuring anything.

Two columns, and the difference between them is the point:

  MINE      only the four tests in internal/db/hard_delete_test.go
  PRIOR     the rest of ./internal/db/, with that file taken away entirely

MINE is the attribution: it answers "did the tests I wrote catch this?". PRIOR
answers the question that actually decides whether they were worth writing:
"would this have been caught anyway?". A row that is CAUGHT by MINE and
unnoticed by PRIOR is a mechanism nothing pinned before.

PRIOR is measured by moving hard_delete_test.go out of the package, not by a
`-run` filter. That is not fussiness. Comparing the filtered set against the
whole package cannot answer the question at all, because the whole package
CONTAINS the filtered set — it goes red whenever MINE does, so the comparison
reports "these tests were necessary" for every row by construction. The first
version of this scorer did exactly that and printed a number that could only
ever equal the number of caught cases.

Case-writing rules, inherited from the fleet's other scorers:

  - Prefer a DRIFTED VALUE to a deletion. It orphans no identifier, needs no
    second edit, and is the likelier real regression. `go test` runs vet, so an
    edit that fails to compile reports a build error instead of a score, and a
    build error hides whether any test would have caught the behavior.
  - Two controls, not one. A scorer with only a cry-wolf control reports CAUGHT
    for everything and looks perfect; a scorer with only a known-negative
    cannot tell a working suite from a harness that never runs the tests.
  - Read the applied diffs against their labels (`--diffs`). A row prints the
    name it was given, not the edit that was made.
  - Every needle below was checked unique in db.go before the run.

Score when filed: 7/7 real mechanisms CAUGHT by MINE, all 7 unnoticed by PRIOR,
both controls behaved, exit 0. Exit 0 when every real mechanism is caught by
MINE and both controls behave; exit 1 otherwise.

One case here exists because it corrected the suite it scores. "the snapshot is
taken after the row is destroyed" was written expecting the content assertions
to catch it; they do not, because `existing` is already in memory by then, and
the test comment claiming otherwise was wrong until this run measured it.

⚠️ Region this scorer does NOT cover, declared rather than left to be
rediscovered: the soft-delete path beyond telling it apart from a purge, the
restore/hold/unhold snapshots, the API layer's `?hard=true` plumbing, and
ListRevisions' own scan. Those are covered — or not — by db_test.go and
api_test.go, and no case here touches them.
"""

import argparse
import difflib
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
TARGET = REPO / "internal/db/db.go"
PACKAGE = "./internal/db/"
MINE = "TestAHardDelete|TestTheTwoDeletesAreDistinguishableInTheRevisionLog"


@dataclass
class Case:
    name: str
    find: str
    replace: str
    # Expected verdict for the MINE column. Real mechanisms must be caught.
    expect_caught: bool
    why: str


CASES = [
    # ---------------- controls ----------------
    Case(
        name="CONTROL known-negative: rename the revisions index",
        find="CREATE INDEX IF NOT EXISTS idx_item_revisions_item ON item_revisions(item_id, id DESC);",
        replace="CREATE INDEX IF NOT EXISTS idx_item_revisions_lookup ON item_revisions(item_id, id DESC);",
        expect_caught=False,
        why=(
            "An index name is invisible to behavior. If this reports CAUGHT the "
            "suite is failing for a reason unrelated to what it asserts, and every "
            "other CAUGHT below is suspect."
        ),
    ),
    Case(
        name="CONTROL cry-wolf: DeleteItem refuses every call",
        find="\treason := \"delete\"\n",
        replace="\treturn fmt.Errorf(\"sabotage: delete disabled\")\n\treason := \"delete\"\n",
        expect_caught=True,
        why=(
            "Breaks the function outright. If this reports UNNOTICED the harness "
            "is not running the tests at all and every UNNOTICED below is a lie."
        ),
    ),
    # ---------------- real mechanisms ----------------
    Case(
        name="hard delete tombstones instead of destroying the row",
        find='_, err := s.db.Exec("DELETE FROM items WHERE id = ?", id)',
        replace='_, err := s.db.Exec("UPDATE items SET deleted_at = ? WHERE id = ?", time.Now().UTC(), id)',
        expect_caught=True,
        why=(
            "The whole difference between the two values of `hard`. A read path "
            "that filters tombstones cannot tell this mutation from correct "
            "behavior, which is why the test counts the raw items table."
        ),
    ),
    Case(
        name="purge is recorded under the soft delete's reason",
        find='reason = "purge"',
        replace='reason = "delete"',
        expect_caught=True,
        why=(
            "The reason column is the only record of which delete happened. "
            "Collapsed, a reader of the revision log cannot tell a recoverable "
            "tombstone from a destroyed row."
        ),
    ),
    Case(
        name="the snapshot is written empty",
        find="s.snapshot(existing, reason)",
        replace="s.snapshot(&model.Item{ID: existing.ID}, reason)",
        expect_caught=True,
        why=(
            "A revision row still appears, with the right reason, at the right "
            "time — and carries none of the item. This is the case that justifies "
            "asserting content rather than existence: a test that only counted "
            "revisions, or compared timestamps, would pass."
        ),
    ),
    Case(
        name="a failed snapshot no longer stops the purge",
        find="if err := s.snapshot(existing, reason); err != nil {\n\t\treturn fmt.Errorf(\"snapshot before %s: %w\", reason, err)\n\t}",
        replace="_ = s.snapshot(existing, reason)",
        expect_caught=True,
        why=(
            "snapshot's own comment says the mutation must not proceed if the "
            "snapshot fails. For a soft delete that is a nicety; for a purge it "
            "is the entire guarantee, because the row does not come back."
        ),
    ),
    Case(
        name="the snapshot is taken after the row is destroyed",
        find="if err := s.snapshot(existing, reason); err != nil {\n\t\treturn fmt.Errorf(\"snapshot before %s: %w\", reason, err)\n\t}\n\n\tif hard {\n\t\t_, err := s.db.Exec(\"DELETE FROM items WHERE id = ?\", id)\n\t\treturn err\n\t}",
        replace="if hard {\n\t\tif _, err := s.db.Exec(\"DELETE FROM items WHERE id = ?\", id); err != nil {\n\t\t\treturn err\n\t\t}\n\t\treturn s.snapshot(existing, reason)\n\t}\n\tif err := s.snapshot(existing, reason); err != nil {\n\t\treturn fmt.Errorf(\"snapshot before %s: %w\", reason, err)\n\t}",
        expect_caught=True,
        why=(
            "The ordering the store documents, and the case that corrected this "
            "suite's own comment. `existing` is already in memory, so a snapshot "
            "written after the DELETE still carries every field: the content "
            "assertions cannot see this and neither could a timestamp. Only the "
            "failed-snapshot test detects it, by being the one place where the "
            "row's survival depends on the snapshot having gone first."
        ),
    ),
    Case(
        name="the purge takes its own snapshots with it",
        find='_, err := s.db.Exec("DELETE FROM items WHERE id = ?", id)\n\t\treturn err',
        replace='s.db.Exec("DELETE FROM item_revisions WHERE item_id = ?", id)\n\t\t_, err := s.db.Exec("DELETE FROM items WHERE id = ?", id)\n\t\treturn err',
        expect_caught=True,
        why=(
            "What an ON DELETE CASCADE on item_revisions.item_id would do. "
            "Nothing in the schema forbids one; the column carries no foreign "
            "key, so this stays a live regression until a test pins it."
        ),
    ),
    Case(
        name="the FTS delete trigger stops firing",
        find="CREATE TRIGGER IF NOT EXISTS items_ad AFTER DELETE ON items BEGIN",
        replace="CREATE TRIGGER IF NOT EXISTS items_ad AFTER DELETE ON items WHEN 0 BEGIN",
        expect_caught=True,
        why=(
            "items_ad fires on DELETE and nothing else, so a hard delete is the "
            "only thing that runs it. items_fts is external-content: a surviving "
            "index row points at a rowid that no longer resolves."
        ),
    ),
]


MY_TESTS = REPO / "internal/db/hard_delete_test.go"
PARKED = REPO / "internal/db/hard_delete_test.go.parked"


def run_tests(run_filter=None):
    """Return (passed, failing_test_names, raw_output)."""
    cmd = ["go", "test", PACKAGE, "-count=1", "-v"]
    if run_filter:
        cmd += ["-run", run_filter]
    proc = subprocess.run(cmd, cwd=REPO, capture_output=True, text=True)
    out = proc.stdout + proc.stderr
    # `--- FAIL: TestName (0.02s)` — the name is the third field. Taking the
    # last one yields the duration, which is what the first version printed:
    # a column of "(0.02s)" that looked like data and named no test.
    failing = sorted({
        line.split()[2]
        for line in out.splitlines()
        if line.strip().startswith("--- FAIL:") and len(line.split()) > 2
    })
    return proc.returncode == 0, failing, out


def run_without_my_tests():
    """Run the package with hard_delete_test.go taken out of it."""
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

    baseline_mine, _, _ = run_tests(MINE)
    baseline_prior, _, out = run_without_my_tests()
    if not (baseline_mine and baseline_prior):
        print("REFUSED: the suite is already red before any sabotage.\n" + out)
        return 2
    print(f"baseline: {PACKAGE} green with and without hard_delete_test.go\n")

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
    for case, mine_caught, mine_failing, prior_caught, prior_failing in results:
        if mine_caught:
            print(f"  {case.name}\n    reddened: {', '.join(mine_failing) or '(build/vet error — inspect)'}")

    real = [c for c in CASES if not c.name.startswith("CONTROL")]
    caught = sum(1 for c, m, _, _, _ in results if m and not c.name.startswith("CONTROL"))
    print(f"\n{caught}/{len(real)} real mechanisms caught by the tests under test.")

    exclusive = [c.name for c, m, _, p, _ in results
                 if m and not p and not c.name.startswith("CONTROL")]
    print(f"{len(exclusive)} of them go unnoticed with hard_delete_test.go removed, "
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
