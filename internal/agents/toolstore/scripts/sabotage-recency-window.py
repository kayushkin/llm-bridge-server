#!/usr/bin/env python3
"""Sabotage cases for the recency window in tools/recent_files.go.

`findRecentlyModified` answers "which files changed inside this window" twice,
by two strategies with two separate windows:

    recent_files.go:100  findRecentlyModifiedGit    sinceTime := time.Now().Add(-since)
    recent_files.go:146  findRecentlyModifiedMtime  cutoff    := time.Now().Add(-since)

Both are executed on every run of the suite, from tools/cancellation_test.go,
three calls, all at `time.Hour`, all in tests about cancellation. Nothing puts a
file near the edge of either window, so the mechanism runs and its position is
pinned by nothing. That is card `3c18632a`'s thesis and card `319a55bf` predicted
these rows would SURVIVE; this file is what turns the prediction into a result.

The two windows are mutated SEPARATELY. Moved together, a pass cannot say which
strategy its test actually holds — and on this file that matters more than usual,
because the two strategies do not filter on the same clock. `git log --since`
selects on COMMIT time; the mtime walk selects on the file's mtime. tool-store's
git path never re-checks mtime (memory-store's does), so the two windows are
genuinely independent cuts and one test cannot pin both.

Run with --diffs at least once and read each applied edit against its label. A
row prints the name it was given, not the edit it made.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import (Case, REPO, counts_as_coverage, print_score,  # noqa: E402
                      run_cases)

PACKAGES = ["./tools/"]

# The fixture guards in ./tools/, by message. A guard fires when a mutation stops
# the test input reaching the code under test; `go test` exits non-zero for that
# exactly as it does for a real assertion, so without these the engine counts the
# test falling over as coverage. See sabotage.classify_caught().
#
# The three recency_window_test.go entries are the strategies' own error returns.
# None of the cases below can make either strategy fail, so none of them should
# ever fire — but a later case that breaks the call rather than moving the window
# would otherwise be counted as pinning the window, which is the inflation this
# split exists to stop.
GUARD_MARKERS = (
    "input was not truncated at all, so the test proves nothing",
    "findRecentlyModifiedGit: ",
    "findRecentlyModifiedMtime: ",
    "chtimes ",
)

TARGET = REPO / "tools" / "recent_files.go"

CASES = [
    # ---- the git strategy's window ----
    #
    # Halving and doubling, not a one-unit drift: `since` is a caller-supplied
    # duration with no adjacent value, so the only honest move is a factor. A
    # test that straddles the window catches both; a test that merely names the
    # window in a fixture catches neither.
    Case("git: the window halves",
         [("\tsinceTime := time.Now().Add(-since)",
           "\tsinceTime := time.Now().Add(-since / 2)")]),
    Case("git: the window doubles",
         [("\tsinceTime := time.Now().Add(-since)",
           "\tsinceTime := time.Now().Add(-since * 2)")]),

    # ---- the mtime strategy's window ----
    Case("mtime: the window halves",
         [("\tcutoff := time.Now().Add(-since)",
           "\tcutoff := time.Now().Add(-since / 2)")]),
    Case("mtime: the window doubles",
         [("\tcutoff := time.Now().Add(-since)",
           "\tcutoff := time.Now().Add(-since * 2)")]),

    # ---- the git strategy's window is not applied at all ----
    #
    # Dropping `--since` entirely is the crude version of the two moves above and
    # asks a different question: whether anything notices the window vanishing,
    # as opposed to moving.
    #
    # Both lines that derive the argument are replaced by the one literal. Swapping
    # only the call site orphans `sinceArg`, and an orphaned identifier is a Go
    # compile error, which scores as `compile error` rather than as a result —
    # measured, that is exactly what this row did on its first run.
    Case("git: the window is never passed to git log",
         [('\tsinceTime := time.Now().Add(-since)\n\tsinceArg := sinceTime.Format("2006-01-02 15:04:05")',
           '\tsinceArg := "1970-01-01 00:00:00"')]),

    Case("CONTROL known-positive: the mtime walk keeps nothing",
         [("\t\tif info.ModTime().After(cutoff) {",
           "\t\tif info.ModTime().Before(cutoff) {")]),
    Case("CONTROL known-negative: After(cutoff) rewritten as !Before(cutoff)",
         [("\t\tif info.ModTime().After(cutoff) {",
           "\t\tif !info.ModTime().Before(cutoff) {")],
         expected_unnoticed="the two differ only for a file whose mtime is EXACTLY cutoff at the "
                            "instant of comparison, and no fixture can hold a wall-clock instant "
                            "still. Carried from card 319a55bf, which measured the identical row "
                            "as equivalent in memory-store's copy of this walk. FALSIFY: produce a "
                            "file whose mtime equals time.Now().Add(-since) at the moment the walk "
                            "reaches it."),
]


def main():
    print("target: %s" % TARGET.relative_to(REPO))
    # run_cases + print_score, not score(): the unioned engine's score() returns
    # an exit status where the fork this file was written against returned the
    # rows, and the rows are what the total below counts. GUARD_MARKERS goes by
    # KEYWORD — the two forks each ended score() in a fourth positional
    # parameter and they were different ones (guard_markers here, unreddened
    # there), so the union made both keyword-only and a positional call is now a
    # TypeError rather than a silent mis-binding.
    results = run_cases(TARGET, PACKAGES, CASES, guard_markers=GUARD_MARKERS)
    found = print_score(results)

    caught = 0
    real = 0
    for case, verdict, _, _ in results:
        if case.name.startswith("CONTROL"):
            continue
        real += 1
        # counts_as_coverage, not `verdict == "CAUGHT"`: a row that went red
        # because a fixture guard fired is not a mechanism this suite pins.
        if counts_as_coverage(verdict):
            caught += 1

    print("\nTOTAL: %d/%d real mechanisms caught" % (caught, real))
    if found:
        print("  ⚠️  %d problem(s) in the table above" % len(found))
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
