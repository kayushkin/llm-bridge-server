#!/usr/bin/env python3
"""Score the boundaries in Store.Compact (compaction.go).

Compact carries five numeric cuts in one function -- the age cutoff, the
access-count cut, and the two hardcoded importance literals, all in one WHERE
clause, plus the len(members) < 2 group-size skip. This walks each cut in both
directions and reports whether the package suite notices.

Every mutation is an exact-string replacement whose anchor must appear EXACTLY
once in the file; a non-unique or missing anchor is reported as SKIPPED rather
than counted, because an anchor that matches nothing reads exactly like a
mutation nothing caught.

Two controls run alongside the real rows: a known-positive that must be CAUGHT
and a no-op that must stay UNNOTICED. If either misbehaves the run is a broken
control and the scores mean nothing.

Usage: python3 scripts/sabotage-compaction.py [--only SUBSTRING]
"""

import argparse
import pathlib
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
TARGET = REPO / "compaction.go"

# (label, anchor, replacement)
MUTATIONS = [
    # --- Cut A: the age cutoff (created_at < now-minAge) ---
    ("A1 age window halved (admits newer memories)",
     "cutoff := time.Now().Add(-minAge).Unix()",
     "cutoff := time.Now().Add(-minAge / 2).Unix()"),
    ("A2 age window doubled (excludes older memories)",
     "cutoff := time.Now().Add(-minAge).Unix()",
     "cutoff := time.Now().Add(-minAge * 2).Unix()"),
    ("A3 age cut boundary < -> <= (exact-cutoff row only)",
     "WHERE created_at < ? AND",
     "WHERE created_at <= ? AND"),
    ("A4 age cut removed entirely",
     "cutoff := time.Now().Add(-minAge).Unix()",
     "cutoff := time.Now().Add(24 * 365 * time.Hour).Unix()"),

    # --- Cut B: access_count < minCount ---
    ("B1 access cut boundary < -> <=",
     "AND access_count < ? AND",
     "AND access_count <= ? AND"),
    ("B2 access cut loosened by one (minCount+1)",
     "rows, err := s.db.Query(query, cutoff, minCount)",
     "rows, err := s.db.Query(query, cutoff, minCount+1)"),
    ("B3 access cut tightened by one (minCount-1)",
     "rows, err := s.db.Query(query, cutoff, minCount)",
     "rows, err := s.db.Query(query, cutoff, minCount-1)"),
    ("B4 access cut removed entirely",
     "rows, err := s.db.Query(query, cutoff, minCount)",
     "rows, err := s.db.Query(query, cutoff, 1000000)"),

    # --- Cut C: the importance < 0.7 literal ---
    ("C1 importance ceiling raised 0.7 -> 0.8",
     "AND importance < 0.7 AND",
     "AND importance < 0.8 AND"),
    ("C2 importance ceiling lowered 0.7 -> 0.6",
     "AND importance < 0.7 AND",
     "AND importance < 0.6 AND"),
    ("C3 importance ceiling boundary < -> <=",
     "AND importance < 0.7 AND",
     "AND importance <= 0.7 AND"),
    ("C4 importance ceiling removed entirely",
     "AND importance < 0.7 AND",
     "AND importance < 1000.0 AND"),

    # --- Cut D: the importance > 0 literal (the soft-delete floor) ---
    ("D1 soft-delete floor > 0 -> >= 0 (re-admits deleted memories)",
     "AND importance > 0\n",
     "AND importance >= 0\n"),
    ("D2 soft-delete floor raised 0 -> 0.1",
     "AND importance > 0\n",
     "AND importance > 0.1\n"),

    # --- Cut E: the group-size skip ---
    ("E1 group-size skip 2 -> 1 (compacts singletons)",
     "if len(members) < 2 {",
     "if len(members) < 1 {"),
    ("E2 group-size skip 2 -> 3 (skips pairs)",
     "if len(members) < 2 {",
     "if len(members) < 3 {"),

    # --- controls ---
    ("CONTROL-POSITIVE candidates always treated as empty (must be CAUGHT)",
     "if len(candidates) == 0 {",
     "if len(candidates) >= 0 {"),
    ("CONTROL-NOOP -minAge -> -1 * minAge (must stay UNNOTICED)",
     "cutoff := time.Now().Add(-minAge).Unix()",
     "cutoff := time.Now().Add(-1 * minAge).Unix()"),
]


def run_suite():
    """Return (verdict, detail). verdict is CAUGHT, UNNOTICED or BROKEN."""
    proc = subprocess.run(
        ["go", "test", "./..."],
        cwd=REPO, capture_output=True, text=True, timeout=900,
    )
    out = proc.stdout + proc.stderr
    if "build failed" in out or "[build failed]" in out or "cannot use" in out or "syntax error" in out:
        return "BROKEN", "does not compile -- re-aim this mutation, it is a scorer defect"
    if proc.returncode == 0:
        return "UNNOTICED", ""
    failed = [ln.strip() for ln in out.splitlines() if ln.strip().startswith("--- FAIL")]
    return "CAUGHT", "; ".join(sorted(set(failed))) or "suite red"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--only", default="", help="run only mutations whose label contains this")
    args = parser.parse_args()

    original = TARGET.read_text()
    rows = [m for m in MUTATIONS if args.only.lower() in m[0].lower()]

    print(f"instrument: {pathlib.Path(__file__).name}, {len(rows)} rows, target {TARGET.name}")
    print("whole package per row, no -run filter\n")

    caught = unnoticed = 0
    try:
        for label, anchor, replacement in rows:
            occurrences = original.count(anchor)
            if occurrences != 1:
                print(f"SKIPPED   {label}\n          anchor occurs {occurrences}x, must be exactly 1")
                continue
            TARGET.write_text(original.replace(anchor, replacement))
            verdict, detail = run_suite()
            TARGET.write_text(original)
            if verdict == "CAUGHT":
                caught += 1
            elif verdict == "UNNOTICED":
                unnoticed += 1
            print(f"{verdict:9s} {label}")
            if detail:
                print(f"          {detail}")
    finally:
        TARGET.write_text(original)

    print(f"\nscore: {caught} CAUGHT / {caught + unnoticed} scored")
    return 0


if __name__ == "__main__":
    sys.exit(main())
