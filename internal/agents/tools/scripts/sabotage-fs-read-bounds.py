#!/usr/bin/env python3
"""Sabotage cases for the two caps a whole-file read is cut by.

Card `3c18632a` — sweep the fleet's corpora for numeric boundaries no row
straddles. `tools/fs_read_test.go` is one of its 101 paths. The suite around
`readSingleFile` is careful: it pins the footer wording, the line window's real
range, the offset/limit ranges and the addressability of the last line. Two
numeric cuts run on every whole-file read and no row of it lands near either.

    schema/truncate.go:67   fileReadWholeFileLimit = 2000   lines before the window fires
    tools/fs.go:72          maxWholeFileReadBytes  = 100000 bytes before the byte cap fires

The corpus reaches both mechanisms — one row is 3000 lines, one is 500 000
bytes — and its line counts are {0, 1, 3, 3000} and its byte sizes {0..30,
500000}. So each cap is free to sit anywhere across a range thousands wide with
the whole suite green. That is the card's thesis exactly: documented, exercised,
unpinned.

⛔ The byte cap has a second defect on top of the first, and it is why widening
it survives even though a row sits well past it. `TestReadReportsByteCapAsPartial`
builds its expected footer with

    fmt.Sprintf("[partial read — %d of %d bytes ...", maxWholeFileReadBytes, len(body))

— the constant under test. Move the constant and the expectation moves with it,
so the assertion holds whatever the value is. Card `dfeb2992`'s question ("is the
fixture's value computed from the same expression as the code?") answered here in
the affirmative. A straddling row cannot help while the assertion is derived;
both halves have to be repaired together.

Run with --diffs at least once and read each applied edit against its label. A
row prints the name it was given, not the edit it made.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import (Case, REPO, counts_as_coverage, print_score,  # noqa: E402
                      run_cases)

# Both packages, because a mutation to schema/truncate.go can be caught by
# schema's own suite as well as by the tools suite that consumes it, and a run
# over ./tools/ alone would credit the tools tests for a catch schema made.
PACKAGES = ["./tools/", "./schema/"]

TARGETS = [REPO / "schema" / "truncate.go", REPO / "tools" / "fs.go"]

# No fixture in either package guards on "the input never reaches the cut" for
# these rows: every case below moves a cap, and a moved cap still returns a
# string. Declared empty on purpose rather than omitted, so the next reader
# knows the question was asked. See sabotage.classify_caught().
GUARD_MARKERS = ()

CASES = [
    # ---- the whole-file LINE limit -------------------------------------------
    #
    # Widen and narrow, not a one-line drift. The corpus's line counts are
    # {0, 1, 3, 3000}, so the limit is free anywhere in [3, 2999] and a
    # single-step mutation would be a weaker probe than the gap deserves.
    Case("line limit: 2000 -> 2500 (widen)",
         [("\tfileReadWholeFileLimit = 2000", "\tfileReadWholeFileLimit = 2500")]),
    Case("line limit: 2000 -> 1500 (narrow)",
         [("\tfileReadWholeFileLimit = 2000", "\tfileReadWholeFileLimit = 1500")]),
    # The off-by-one at the cut itself. `<=` and `<` differ for exactly one
    # file: a 2000-line one, which is complete under the first and windowed
    # under the second. Only a row AT 2000 can separate them.
    Case("line limit: <= becomes < , so a 2000-line file is windowed",
         [("\tif total <= fileReadWholeFileLimit {",
           "\tif total < fileReadWholeFileLimit {")]),

    # ---- the whole-file BYTE cap ---------------------------------------------
    Case("byte cap: 100000 -> 200000 (widen)",
         [("const maxWholeFileReadBytes = 100_000",
           "const maxWholeFileReadBytes = 200_000")]),
    Case("byte cap: 100000 -> 50000 (narrow)",
         [("const maxWholeFileReadBytes = 100_000",
           "const maxWholeFileReadBytes = 50_000")]),
    # The off-by-one at the cut itself, and it is not cosmetic. At exactly
    # maxWholeFileReadBytes the `>=` branch keeps the whole content, so
    # droppedBytes is 0 and the footer still says "[complete file — N lines]"
    # — while the body has gained a "... (truncated)" marker. A read that is
    # complete and says it is truncated is the same lie as the one this
    # suite's footer tests exist to stop, pointing the other way.
    Case("byte cap: > becomes >= , so a 100000-byte file gains a truncation marker",
         [("\tif len(content) > maxWholeFileReadBytes {",
           "\tif len(content) >= maxWholeFileReadBytes {")]),

    # ---- controls ------------------------------------------------------------
    #
    # Two, not one. The 181st pass's finding on this card: a lone UNNOTICED
    # control reads exactly like a broken harness, and it took a second control
    # coming back CAUGHT to show the instrument was fine and the first was a
    # finding.
    Case("CONTROL known-positive: the kept-head window shrinks to 400",
         [("\tfileReadKeepFirst      = 500", "\tfileReadKeepFirst      = 400")]),
    Case("CONTROL cry-wolf: the end-of-file test is written the other way round",
         [("\t\tif end < totalLines {", "\t\tif totalLines > end {")],
         expected_unnoticed="`end < totalLines` and `totalLines > end` are the same "
                            "comparison written in the other order. Nothing can tell "
                            "them apart, and a run that reddens here says the harness "
                            "is mutating something other than what its label claims. "
                            "FALSIFY: any red run on this row."),
]


def main():
    print("targets: %s" % ", ".join(str(t.relative_to(REPO)) for t in TARGETS))
    results = run_cases(TARGETS, PACKAGES, CASES, guard_markers=GUARD_MARKERS)
    found = print_score(results)

    caught = 0
    real = 0
    for case, verdict, _, _ in results:
        if case.name.startswith("CONTROL"):
            continue
        real += 1
        if counts_as_coverage(verdict):
            caught += 1

    print("\nTOTAL: %d/%d real mechanisms caught" % (caught, real))
    if found:
        print("  ⚠️  %d problem(s) in the table above" % len(found))
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
