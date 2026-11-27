#!/usr/bin/env python3
"""Sabotage cases for the rune-safe truncation work.

The defect this scores is one mechanism spread over six files: a shared pair of
helpers, and five tools that cut a string down to a byte budget. This drives the
engine once per file and adds the scores up.

That used to be forced — the fork of sabotage.py this repo carried scored one
file at a time. The unioned engine takes several targets in one table, and a
table per file is still what this file wants: each call site has its own case
list, the per-file heading is what makes a row's target readable, and a case
list flattened across six files would have to lengthen every needle that two
of them happen to share.

Scoring the helper alone would not be enough. The helper's own tests could be
perfect while a call site still byte-cuts, and that is exactly the state this
repo was in before tonight — so every call site gets a case that puts the plain
byte cut back, and it has to go red.

Run with --diffs at least once and read each applied edit against its label. A
row prints the name it was given, not the edit it made.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import Case, REPO, counts_as_coverage, print_score, run_cases  # noqa: E402

PACKAGES = ["./schema/", "./tools/"]

# The fixture guards in the packages above, by message. A guard fires when a
# mutation stops the test input reaching the code under test; `go test` exits
# non-zero for that exactly as it does for a real assertion, so without these
# the engine counts the test falling over as coverage. See
# sabotage.classify_caught().
#
# Only tools/runecut_test.go:121 is in scope here. The repo's other guard,
# provision_test.go:193 ("instance opt-in did not reach the config"), sits in
# the root package, which PACKAGES does not run.
GUARD_MARKERS = (
    "input was not truncated at all, so the test proves nothing",
)

HELPER = REPO / "schema" / "runeboundary.go"

HELPER_CASES = [
    # The walks themselves. Written as drifted comparisons rather than
    # deletions so no identifier is orphaned and the case scores instead of
    # reporting a compile error.
    Case("prefix walk never runs (comparison drifted)",
         [("for cut > 0 && !utf8.RuneStart(s[cut])",
           "for cut > len(s) && !utf8.RuneStart(s[cut])")]),
    Case("prefix walk runs the wrong way",
         [("\t\tcut--", "\t\tcut++")]),
    Case("suffix walk never runs (comparison drifted)",
         [("for cut < len(s) && !utf8.RuneStart(s[cut])",
           "for cut < 0 && !utf8.RuneStart(s[cut])")]),
    Case("suffix walk runs the wrong way",
         [("\t\tcut++", "\t\tcut--")]),

    # "Valid UTF-8 within budget" is satisfied by giving up entirely. These are
    # the cases the length assertions exist for.
    Case("prefix trims to nothing", [("\treturn s[:cut]", "\treturn \"\"")]),
    Case("suffix trims to nothing", [("\treturn s[cut:]", "\treturn \"\"")]),

    # The budget itself.
    Case("prefix ignores the budget when the string is under twice it",
         [("\tif len(s) <= maxBytes {\n\t\treturn s\n\t}\n\t// s[cut] is the first byte past",
           "\tif len(s) <= maxBytes*2 {\n\t\treturn s\n\t}\n\t// s[cut] is the first byte past")]),
    Case("suffix ignores the budget when the string is under twice it",
         [("\tif len(s) <= maxBytes {\n\t\treturn s\n\t}\n\t// s[cut] is the first byte of",
           "\tif len(s) <= maxBytes*2 {\n\t\treturn s\n\t}\n\t// s[cut] is the first byte of")]),

    Case("CONTROL known-positive: the prefix helper returns a fixed string",
         [("\treturn s[:cut]", "\treturn \"SABOTAGE\"")]),
    Case("CONTROL known-negative: <= 0 rewritten as < 1, identical for ints",
         [("\tif maxBytes <= 0 {\n\t\treturn \"\"\n\t}\n\tif len(s) <= maxBytes {\n\t\treturn s\n\t}\n\t// s[cut] is the first byte past",
           "\tif maxBytes < 1 {\n\t\treturn \"\"\n\t}\n\tif len(s) <= maxBytes {\n\t\treturn s\n\t}\n\t// s[cut] is the first byte past")],
         expected_unnoticed="a behavioral no-op; it must NOT be caught"),

    # ---- boundary VALUES ----
    #
    # Everything above this line moves a DIRECTION or deletes a MECHANISM. None
    # of it moves a number. Measured 2026-08-15: the 17 direction/mechanism cases
    # scored 17/17 with both controls behaving while 23 adjacent-value moves went
    # UNNOTICED — a perfect mutation score and a wholly unpinned set of budgets,
    # at the same moment. That run wrote "23 of 26"; a census on 2026-08-17 finds
    # 27 value cases, 6 here and 21 in CALL_SITES (46 cases total: 17 + 27 + 2
    # controls), so the denominator was one short of its own list even then, and
    # the 23 is the only half of it that was measured. Every move here is by ONE
    # unit; a far move (500 -> 50) pins a band rather than a value and is
    # frequently a deletion wearing a number.
    Case("prefix: the non-positive guard swallows a budget of exactly 1",
         [("\tif maxBytes <= 0 {\n\t\treturn \"\"\n\t}\n\tif len(s) <= maxBytes {\n\t\treturn s\n\t}\n\t// s[cut] is the first byte past",
           "\tif maxBytes <= 1 {\n\t\treturn \"\"\n\t}\n\tif len(s) <= maxBytes {\n\t\treturn s\n\t}\n\t// s[cut] is the first byte past")]),
    Case("suffix: the non-positive guard swallows a budget of exactly 1",
         [("\tif maxBytes <= 0 {\n\t\treturn \"\"\n\t}\n\tif len(s) <= maxBytes {\n\t\treturn s\n\t}\n\t// s[cut] is the first byte of",
           "\tif maxBytes <= 1 {\n\t\treturn \"\"\n\t}\n\tif len(s) <= maxBytes {\n\t\treturn s\n\t}\n\t// s[cut] is the first byte of")]),
    Case("prefix: a string of exactly maxBytes is cut instead of returned whole",
         [("\tif len(s) <= maxBytes {\n\t\treturn s\n\t}\n\t// s[cut] is the first byte past",
           "\tif len(s) < maxBytes {\n\t\treturn s\n\t}\n\t// s[cut] is the first byte past")]),
    Case("suffix: a string of exactly maxBytes is cut instead of returned whole",
         [("\tif len(s) <= maxBytes {\n\t\treturn s\n\t}\n\t// s[cut] is the first byte of",
           "\tif len(s) < maxBytes {\n\t\treturn s\n\t}\n\t// s[cut] is the first byte of")],
         expected_unnoticed="a behavioral no-op, unlike its prefix twin. Falling through with "
                            "len(s) == maxBytes gives cut = 0, and s[0] is a rune start in any "
                            "valid UTF-8 string, so the walk does not move and s[0:] is the whole "
                            "string — the same answer the guard returns. FALSIFY: find an input "
                            "where the two branches disagree, i.e. a valid UTF-8 string whose "
                            "first byte is a continuation byte. There is none."),
    Case("prefix: the cut starts one byte short of the budget",
         [("\tcut := maxBytes\n", "\tcut := maxBytes - 1\n")]),
    Case("suffix: the cut starts one byte inside the budget",
         [("\tcut := len(s) - maxBytes\n", "\tcut := len(s) - maxBytes + 1\n")]),
]

# Each call site gets the plain byte cut put back. If the suite stays green for
# any of these, that tool is unpinned no matter how well the helper scores.
CALL_SITES = {
    REPO / "tools" / "web_fetch.go": [
        # 0b81c49 (landed on main 2026-09-25) counts max_chars in characters,
        # and the cut is no longer behind a length check, so the byte cut is
        # clamped: a bare content[:maxChars] would panic on a short page and
        # score CAUGHT for a reason that has nothing to do with bytes.
        Case("web_fetch byte-cuts fetched page text again",
             [("truncateToRuneCount(content, maxChars)",
               "content[:min(len(content), maxChars)]")]),
        Case("web_fetch's default page budget drifts by one",
             [("\t\t\t\tmaxChars = 50000", "\t\t\t\tmaxChars = 50001")]),
        Case("web_fetch treats a requested budget of 1 as unset",
             [("if maxChars <= 0 {", "if maxChars <= 1 {")]),
    ],
    REPO / "tools" / "fs.go": [
        Case("read_files byte-cuts file content again",
             [("schema.TruncateAtRuneBoundary(content, maxWholeFileReadBytes)",
               "content[:maxWholeFileReadBytes]")]),
        Case("the read banner reports the cap instead of what was kept",
             [("\t\t\tkeptBytes, len(data), totalLines)",
               "\t\t\tmaxWholeFileReadBytes, len(data), totalLines)")]),
        Case("the whole-file byte cap drifts by one",
             [("const maxWholeFileReadBytes = 100_000", "const maxWholeFileReadBytes = 100_001")]),
        Case("a file of exactly the cap is truncated",
             [("if len(content) > maxWholeFileReadBytes {", "if len(content) >= maxWholeFileReadBytes {")]),
        # ⚠️ All three cases below were re-aimed by the 240th pass. `cf2a8be`
        # ("list_files: describe what it actually skips") renamed `maxEntries`
        # to `maxWalkedEntries`, split the shown-entries cap out as
        # `maxListedEntries`, and gave both TruncateList calls a population
        # argument. The three needles stopped matching, and nobody re-ran this
        # file — see the note this pass filed. The mechanisms are unchanged; only
        # the text naming them moved.
        Case("the directory-walk entry cap drifts by one",
             [("maxWalkedEntries = 1000", "maxWalkedEntries = 1001")]),
        # The shallow and recursive listings now share ONE named cap, so a case
        # on the constant would move both at once and could not say which
        # listing any test reached. These two edit the CALL SITES instead, which
        # is what the pair was always for — and the reason the old pair needed a
        # hand-built multi-line needle is gone with the duplicate literal.
        Case("the shallow listing's list cap drifts by one",
             [("schema.TruncateList(lines, maxListedEntries, schema.CompletePopulation(len(lines)))",
               "schema.TruncateList(lines, maxListedEntries+1, schema.CompletePopulation(len(lines)))")]),
        Case("the recursive listing's list cap drifts by one",
             [("schema.TruncateList(lines, maxListedEntries, population)",
               "schema.TruncateList(lines, maxListedEntries+1, population)")]),
    ],
    REPO / "tools" / "task_plan.go": [
        Case("the build output written to .task.md is byte-cut again",
             [("schema.TruncateAtRuneBoundary(buildOutput, 500)", "buildOutput[:500]")]),
        Case("the build result output is byte-cut again",
             [("schema.TruncateAtRuneBoundary(output, 2000)", "output[:2000]")]),
        Case("the build-result threshold drifts by one",
             [("if len(output) > 2000 {", "if len(output) > 2001 {")]),
        Case("the build-result cut keeps one byte more than its threshold",
             [("schema.TruncateAtRuneBoundary(output, 2000)", "schema.TruncateAtRuneBoundary(output, 2001)")]),
        Case("the build-output threshold drifts by one",
             [("if len(buildOutput) > 500 {", "if len(buildOutput) > 501 {")]),
        Case("the build-output cut keeps one byte more than its threshold",
             [("schema.TruncateAtRuneBoundary(buildOutput, 500)", "schema.TruncateAtRuneBoundary(buildOutput, 501)")]),
    ],
    REPO / "tools" / "scheduler.go": [
        Case("the scheduler tool byte-cuts run output again",
             [("schema.TruncateAtRuneBoundary(output, 500)", "output[:500]")]),
        Case("the run-output threshold drifts by one",
             [("if len(output) > 500 {", "if len(output) > 501 {")]),
        Case("the run-output cut keeps one byte more than its threshold",
             [("schema.TruncateAtRuneBoundary(output, 500)", "schema.TruncateAtRuneBoundary(output, 501)")]),
    ],
    REPO / "tools" / "shell.go": [
        # 46b3110 (landed on main 2026-09-25) moved every budget in
        # truncateShellOutput from bytes to characters. The head and tail cuts
        # are headCharacters/tailCharacters now; s is over maxChars characters
        # by the time either runs, so the byte slices below cannot go out of
        # range. The omitted count is correctly derived from the caps now,
        # because each cut keeps exactly its budget in characters — the
        # mistake left to make is counting the total in bytes.
        Case("shell_commands byte-cuts the head again",
             [("headCharacters(s, headChars)", "s[:headChars]")]),
        Case("shell_commands byte-cuts the tail again",
             [("tailCharacters(s, tailChars)", "s[len(s)-tailChars:]")]),
        Case("the omitted count is in bytes, not characters",
             [("omitted := totalChars - headChars - tailChars",
               "omitted := len(s) - headChars - tailChars")]),
        Case("shell: the character ceiling drifts by one",
             [("\t\tmaxChars  = 50000", "\t\tmaxChars  = 50001")]),
        Case("shell: the head character budget drifts by one",
             [("\t\theadChars = 25000", "\t\theadChars = 25001")]),
        Case("shell: the tail character budget drifts by one",
             [("\t\ttailChars = 20000", "\t\ttailChars = 20001")]),
        Case("shell: the line ceiling drifts by one",
             [("\t\tmaxLines  = 500", "\t\tmaxLines  = 501")]),
        Case("shell: the head line budget drifts by one",
             [("\t\theadLines = 250", "\t\theadLines = 251")]),
        Case("shell: the tail line budget drifts by one",
             [("\t\ttailLines = 200", "\t\ttailLines = 201")]),
        Case("shell: output of exactly the character ceiling is truncated",
             [("if totalChars > maxChars {", "if totalChars >= maxChars {")]),
        Case("shell: output of exactly head+tail is split instead of returned whole",
             [("if totalChars <= headChars+tailChars {", "if totalChars < headChars+tailChars {")],
             expected_unnoticed="the branch is unreachable, so no fixture can reach the comparison. "
                                "It is guarded by len(s) > maxChars, and headChars+tailChars is 45000 "
                                "against a ceiling of 50000 — any string that gets this far is already "
                                "over 50000 bytes and cannot be under 45000. FALSIFY: raise "
                                "headChars+tailChars above maxChars, or lower maxChars below 45000, "
                                "and this case starts scoring."),
    ],
}


def main():
    total_caught = 0
    total_real = 0
    found = []

    targets = [(HELPER, HELPER_CASES)] + list(CALL_SITES.items())
    for target, cases in targets:
        print("\n" + "=" * 62)
        print("target: %s" % target.relative_to(REPO))
        print("=" * 62)
        # run_cases + print_score, not score(): score() returns an exit status,
        # and this file needs the rows themselves to add seven tables into one
        # total. GUARD_MARKERS goes by KEYWORD — the engine's fourth positional
        # slot held guard_markers on one fork and unreddened on the other, and
        # binding the wrong one leaves classify_caught blind rather than loud.
        results = run_cases(target, PACKAGES, cases, guard_markers=GUARD_MARKERS)
        found += print_score(results)
        for case, verdict, _, _ in results:
            if case.name.startswith("CONTROL"):
                continue
            total_real += 1
            # counts_as_coverage, not `verdict == "CAUGHT"`: a row that went red
            # because a fixture guard fired is not a mechanism this suite pins,
            # and adding it in here is the inflation the split exists to stop.
            if counts_as_coverage(verdict):
                total_caught += 1

    print("\n" + "=" * 62)
    print("TOTAL: %d/%d real mechanisms caught across %d files"
          % (total_caught, total_real, len(targets)))
    if found:
        print("  ⚠️  %d problem(s) across the tables above" % len(found))

    # Ask the engine what went wrong rather than counting non-CAUGHT rows here.
    # The private counter this replaced was a second, weaker definition of the
    # same thing: it skipped every CONTROL, so a known-positive control that
    # went UNNOTICED — the suite not running at all — added nothing to it, and
    # it ignored expected_unnoticed, so the first KNOWN GAP case added here
    # would have counted as a failure. Two definitions of "wrong" drift, and
    # the copy is the half nobody re-reads.
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
