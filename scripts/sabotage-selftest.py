#!/usr/bin/env python3
"""Score the scorer: exercise sabotage.problems() in BOTH directions.

`problems()` is the single definition of "wrong" for every scorer in this repo
(b11bd30). Nothing tested it. That is the same shape as the defect this file was
written alongside — a rule that only ever fires one way, with no instrument
positioned to notice the other way had gone missing.

Every check here asserts a verdict AND its opposite: the rule must fire on the
input it is for, and must stay silent on the neighbouring input it is not for. A
one-sided check would pass against a `problems()` that returned every line
unconditionally, which is the degenerate scorer this repo already keeps a
known-negative control to rule out.

Run it: python3 scripts/sabotage-selftest.py     (exit 0 clean, 1 with failures)

It imports the engine and calls one pure function, so it needs no git tree, no
Go toolchain and no committed state — unlike every other scorer here it is safe
to run from a dirty worktree.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from sabotage import (Case, packages_no_edit_to_a_target_can_redden,  # noqa: E402
                      problems)

DECLARATION = "an operator log line no test reads"

failures = []
checks_run = 0


def check(description, results, expect_substring, expect_count=1):
    """Assert that problems() reports exactly `expect_count` lines containing
    `expect_substring`. Pass expect_count=0 for the silent direction."""
    global checks_run
    checks_run += 1
    found = problems(results)
    hits = [line for line in found if expect_substring in line]
    if len(hits) != expect_count:
        failures.append(
            "%s\n      expected %d line(s) containing %r, got %d\n      all lines: %r"
            % (description, expect_count, expect_substring, len(hits), found))


def row(name, verdict, declaration=""):
    return (Case(name, [("find", "replace")], declaration), verdict, "")


# ---- the new direction: a declaration that is CAUGHT anyway has expired ----
check("a declared case that is CAUGHT is reported as STALE",
      [row("elapsedFraction returns 1.0 at the boundary", "CAUGHT", DECLARATION)],
      "STALE declaration")

check("the STALE line carries the declaration text, so the reader sees what expired",
      [row("elapsedFraction returns 1.0 at the boundary", "CAUGHT", DECLARATION)],
      DECLARATION)

check("a declared case that is UNNOTICED is silent — the declaration still holds",
      [row("elapsedFraction returns 1.0 at the boundary", "UNNOTICED", DECLARATION)],
      "STALE declaration", expect_count=0)

check("an UNDECLARED case that is CAUGHT is silent — that is an ordinary good row",
      [row("the hold gate stops firing", "CAUGHT")],
      "STALE declaration", expect_count=0)

# ---- the exemption: a control's declaration is not a claim that can expire ----
check("a known-negative control that is CAUGHT is NOT reported as stale",
      [row("CONTROL known-negative: reword a log line", "CAUGHT", DECLARATION)],
      "STALE declaration", expect_count=0)

check("...it is reported as a red suite instead, which is the accurate finding",
      [row("CONTROL known-negative: reword a log line", "CAUGHT", DECLARATION)],
      "the known-negative control WAS caught")

# ---- the four rules that were already here, each in both directions ----
check("a known-positive control that is not CAUGHT is reported",
      [row("CONTROL known-positive: break the thing every test watches", "UNNOTICED")],
      "the known-positive control was NOT caught")

check("a known-positive control that IS caught is silent",
      [row("CONTROL known-positive: break the thing every test watches", "CAUGHT")],
      "the known-positive control was NOT caught", expect_count=0)

check("a known-negative control that is UNNOTICED is silent",
      [row("CONTROL known-negative: reword a log line", "UNNOTICED", DECLARATION)],
      "the known-negative control WAS caught", expect_count=0)

check("an undeclared UNNOTICED row is reported",
      [row("the hold gate stops firing", "UNNOTICED")],
      "UNNOTICED: the hold gate stops firing")

check("an undeclared compile error is reported",
      [row("the hold gate stops firing", "compile error")],
      "compile error, not a score")

check("a declared compile error is silent",
      [row("the hold gate stops firing", "compile error", DECLARATION)],
      "compile error, not a score", expect_count=0)

# ---- reachability: which PACKAGES entries no edit to a TARGET can redden ----
#
# The cross-table's finding is "no case reddens this test", which reads as a gap
# in the case list. A test the mutation cannot reach is not a gap at all, and the
# predicate below is what tells the two apart. Both directions matter and the
# wrong one is the expensive one: a predicate that called everything unreachable
# would suppress every real finding the mode exists to produce, silently.

CURATOR = "github.com/slava/scheduler/cmd/kanban-curator"
VOCAB = "github.com/slava/scheduler/internal/kanbanvocab"
JOBS = "github.com/slava/scheduler/internal/schedulerjobs"


def reach_check(description, target_packages, package_dependencies, expected):
    global checks_run
    checks_run += 1
    got = packages_no_edit_to_a_target_can_redden(target_packages, package_dependencies)
    if got != expected:
        failures.append("%s\n      expected %r, got %r" % (description, expected, got))


reach_check("a library listed beside a cmd/ target is unreachable — nothing imports a main",
            [CURATOR],
            {"./cmd/kanban-curator/": [CURATOR, VOCAB],
             "./internal/kanbanvocab/": [VOCAB]},
            ["./internal/kanbanvocab/"])

reach_check("...and the package holding the target is NOT reported with it",
            [CURATOR],
            {"./cmd/kanban-curator/": [CURATOR, VOCAB]},
            [])

reach_check("a caller listed beside a LIBRARY target is reachable — it compiles against it",
            [JOBS],
            {"./internal/schedulerjobs/": [JOBS],
             "./cmd/autoworker/": ["github.com/slava/scheduler/cmd/autoworker", JOBS]},
            [])

reach_check("reachability is decided per entry, not for the whole list at once",
            [JOBS],
            {"./cmd/autoworker/": ["github.com/slava/scheduler/cmd/autoworker", JOBS],
             "./internal/kanbanvocab/": [VOCAB]},
            ["./internal/kanbanvocab/"])

reach_check("an entry reaching a SECOND target is reachable — one match is enough",
            [CURATOR, VOCAB],
            {"./internal/kanbanvocab/": [VOCAB]},
            [])

reach_check("no packages at all reports nothing rather than failing",
            [CURATOR], {}, [])

# Written out of order on purpose. Python dicts keep insertion order, so a
# fixture already in sorted order cannot tell sorted() from list() and the
# ordering is unpinned — measured: dropping sorted() scored UNNOTICED against
# every check above this one.
reach_check("the answer is ordered, so two runs of the same input read the same",
            [CURATOR],
            {"./internal/kanbanvocab/": [VOCAB],
             "./cmd/kanban-dispatcher/": ["github.com/slava/scheduler/cmd/kanban-dispatcher"],
             "./cmd/kanban-curator/": [CURATOR]},
            ["./cmd/kanban-dispatcher/", "./internal/kanbanvocab/"])

# ---- the whole point of the return value: empty means sound ----
sound = [row("CONTROL known-positive: break the thing every test watches", "CAUGHT"),
         row("CONTROL known-negative: reword a log line", "UNNOTICED", DECLARATION),
         row("the hold gate stops firing", "CAUGHT"),
         row("a behavioural no-op", "UNNOTICED", DECLARATION)]
checks_run += 1
if problems(sound):
    failures.append("a sound run must report nothing, got: %r" % problems(sound))

print("sabotage-selftest: %d checks" % checks_run)
for f in failures:
    print("  ⚠️  " + f)
if failures:
    print("\n%d FAILED" % len(failures))
    sys.exit(1)
print("  every rule fires on its own input and stays silent on its neighbour")
sys.exit(0)
