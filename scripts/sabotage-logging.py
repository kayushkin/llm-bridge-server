#!/usr/bin/env python3
"""Score internal/logging and internal/runstatus by breaking them on purpose.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism the
suite is meant to pin.

    python3 scripts/sabotage-logging.py [--diffs] [--crosstable]

Why these two packages. `internal/logging` had no test file at all, and the one
decision it makes is the level a finished run is published at. That decision was
a hand-listed pair of status literals, one of which (`"failed"`) nothing in the
repository has ever written, and which did not include `"timeout"` — the status
the deployed scheduler gives a run it killed at its wall-clock cap. The failure
mode is a job dying quietly at info level, which is indistinguishable downstream
from a job that worked.

Case-writing rules re-earned here:

  - Prefer a DRIFTED COMPARISON to a deletion: it orphans no variable and needs
    no second edit, so a case reports a score instead of a compile error.
  - Mutate the CALL SITE as well as the predicate. A repair that extracts a
    correct helper and tests only the helper leaves the broken line unheld, and
    a clean score is what that looks like from inside — so `finish-*` and
    `predicate-*` rows exist as separate families on purpose.
  - Move a boundary to a different VALUE rather than deleting the guard. A
    deletion is caught for the wrong reason; a drifted literal is the regression
    that actually happens.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from sabotage import Case, score  # noqa: E402

REPO = Path(__file__).resolve().parent.parent
TARGETS = [
    REPO / "internal" / "logging" / "logging.go",
    REPO / "internal" / "runstatus" / "runstatus.go",
]
PACKAGES = ["./internal/logging/...", "./internal/runstatus/..."]

CASES = [
    Case(
        name="CONTROL known-positive: nothing is ever published",
        edits=[("if l.client == nil {", "if true {")],
    ),
    Case(
        name="CONTROL known-negative: the finish entry's prose is reworded",
        edits=[(
            'Content: fmt.Sprintf("Job \'%s\' finished with status: %s", jobName, status),',
            'Content: fmt.Sprintf("Job %s ended with status %s", jobName, status),',
        )],
        expected_unnoticed="no test asserts the human-readable Content of a finish entry, "
                           "only the level, the subject and the metadata",
    ),

    # ── the call site: does LogJobFinish consult the status at all? ──────────
    Case(
        name="finish-never-escalates: the level decision is skipped",
        edits=[("if runstatus.TerminalStatusIsFailure(status) {", "if false {")],
    ),
    Case(
        name="finish-always-escalates: every run is filed at error level",
        edits=[('level := "info"', 'level := "error"')],
    ),
    Case(
        name="finish-drops-the-status-from-the-entry",
        edits=[('"status":         status,', '"status":         "",')],
    ),

    # ── the predicate ────────────────────────────────────────────────────────
    Case(
        name="predicate-inverted: only success counts as a failure",
        edits=[("return status != Success", "return status == Success")],
    ),
    Case(
        name="predicate-always-true: a successful run is a failure too",
        edits=[("return status != Success", "return true")],
    ),
    Case(
        name="predicate-returns-to-the-hand-listed-pair (the original defect)",
        edits=[("return status != Success", 'return status == Error || status == "failed"')],
    ),
    Case(
        name="predicate-lists-timeout-but-forgets-interrupted",
        edits=[("return status != Success", "return status == Error || status == Timeout")],
    ),

    # ── the rest of the entry ────────────────────────────────────────────────
    Case(
        name="subject-drifts: entries go to a subject nothing reads",
        edits=[('l.client.Publish("logs.scheduler", entry)',
                'l.client.Publish("logs.scheduler.v2", entry)')],
    ),
    Case(
        name="start-entry-escalates: job start is filed at error level",
        edits=[('Level:   "info",\n\t\tType:    "job_execution",\n\t\tContent: fmt.Sprintf("Job \'%s\' started execution", jobName),',
                'Level:   "error",\n\t\tType:    "job_execution",\n\t\tContent: fmt.Sprintf("Job \'%s\' started execution", jobName),')],
    ),
    Case(
        name="truncation-boundary-moves: the cut lands at 600 instead of 500",
        edits=[("if len(output) > 500 {", "if len(output) > 600 {")],
    ),
    Case(
        name="truncation-keeps-the-wrong-end-of-the-output",
        edits=[('outputSnippet = output[:500] + "... (truncated)"',
                'outputSnippet = output[len(output)-500:] + "... (truncated)"')],
    ),
    Case(
        name="new-drops-the-typed-nil-guard",
        edits=[("if natsClient == nil {", "if false {")],
    ),
]

if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
