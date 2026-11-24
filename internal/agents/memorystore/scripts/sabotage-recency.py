"""Sabotage-score the recency and file-age buckets that rank what reaches a prompt.

Card `3c18632a` (nightly worker): a suite can name a numeric boundary, exercise
the mechanism that boundary guards on every row, and still have every row land
on the same side of it. This scorer asks that question of the two places in
memory-store that bucket a timestamp:

  builder.go     calculateScore   daysSinceAccess < 1 -> +0.2, < 7 -> +0.1
  prepare.go     loadRecentFiles  ageMinutes < 60 -> 0.7, < 360 -> 0.6, else 0.5
  management.go  DecayImportance  last_accessed < now-24h -> importance * 0.99

The first and the third are one threshold written twice, in different units,
with no shared constant. The card named the first two; the mechanism is all
three, and the 222nd pass's rule — scope on the mechanism, not the row title —
is why the third is here.

Each row moves ONE literal by ONE unit. A far move is a deletion wearing a
number and asks a different, easier question.

Two controls, because one cannot separate a broken harness from a finding — the
181st pass's rule, learned when an UNNOTICED control turned out to be the night's
defect. POSITIVE rows must come back CAUGHT or the harness is measuring nothing;
NO-OP rows must come back UNNOTICED or it is reporting CAUGHT for everything.

No `-run` filter: the whole package runs on every mutation, so the score is not
also a claim about which tests were selected.

Run from anywhere:  python3 scripts/sabotage-recency.py
"""

import os
import pathlib
import re
import signal
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent

# This suite's own reach guards, by message. Only this file knows which of its
# t.Fatalf lines guard the fixture rather than assert the mechanism, and a
# mutation that resizes a fixture out from under a guard would otherwise score
# CAUGHT with nothing having looked at the boundary the row names.
GUARD_MARKERS = (
    'the fixture no longer straddles',
)

_FAIL_LINE = re.compile(r"^\s*(\S+_test\.go):(\d+): (.*)$", re.M)
_FRAME = re.compile(r"^\s+(/\S+\.go):(\d+)", re.M)


def counts_as_coverage(verdict):
    """Whether a verdict means an assertion actually looked at the mechanism."""
    return verdict == "CAUGHT" or verdict.startswith("CAUGHT (panic in ")


def classify_caught(output):
    """Split a red run into detection and the fixture falling over."""
    messages = [m for _, _, m in _FAIL_LINE.findall(output)]
    guard = [m for m in messages if any(g in m for g in GUARD_MARKERS)]
    real = [m for m in messages if m not in guard]
    if real:
        return "CAUGHT", real[0][:100]
    if "panic:" in output:
        frames = [f for f, _ in _FRAME.findall(output.split("panic:", 1)[1])
                  if f.startswith(str(REPO) + "/")]
        source = next((f for f in frames if not f.endswith("_test.go")), None)
        if source:
            return ("CAUGHT (panic in %s)" % pathlib.Path(source).name,
                    "the test drove the mutation into a crash")
        return "CAUGHT (fixture panicked)", "the test fell over before asserting"
    if guard:
        return "CAUGHT (guard)", guard[0][:100]
    return "CAUGHT (no message)", ""


def self_test():
    """Drive classify_caught() against every verdict it can return."""
    probes = [
        ("--- FAIL: T\n    x_test.go:9: values differ: got 1 want 2\n", "CAUGHT"),
        ("--- FAIL: T\n    x_test.go:9: the fixture no longer straddles the boundary\n", "CAUGHT (guard)"),
        ("--- FAIL: T\n    x_test.go:9: the fixture no longer straddles the boundary\n    x_test.go:12: got 1 want 2\n",
         "CAUGHT"),
        ("panic: boom\n\t/usr/lib/go/src/runtime/panic.go:8\n\t%s/builder.go:3\n" % REPO,
         "CAUGHT (panic in builder.go)"),
        ("panic: boom\n\t%s/x_test.go:3\n" % REPO, "CAUGHT (fixture panicked)"),
        ("--- FAIL: T\nno test line at all\n", "CAUGHT (no message)"),
    ]
    ok = True
    for output, want in probes:
        got, _ = classify_caught(output)
        if got != want:
            print("SELF-TEST FAIL: classify_caught -> %r, want %r" % (got, want))
            ok = False
    for verdict, want in [("CAUGHT", True), ("CAUGHT (panic in builder.go)", True),
                          ("CAUGHT (guard)", False), ("CAUGHT (fixture panicked)", False),
                          ("CAUGHT (no message)", False), ("UNNOTICED", False)]:
        if counts_as_coverage(verdict) != want:
            print("SELF-TEST FAIL: counts_as_coverage(%r) != %r" % (verdict, want))
            ok = False
    print("self-test: %s" % ("all verdicts reachable and separated" if ok else "FAILED"))
    return 0 if ok else 1


if "--self-test" in sys.argv:
    sys.exit(self_test())

CASES = [
  # (label, file, old, new, expect_caught)

  # ---- builder.go calculateScore: the two recency buckets -----------------
  ("VALUE: the one-day recency bonus reaches back two days",
   "builder.go", "if daysSinceAccess < 1 {", "if daysSinceAccess < 2 {", True),
  ("VALUE: the one-day recency bonus shrinks to half a day",
   "builder.go", "if daysSinceAccess < 1 {", "if daysSinceAccess < 0.5 {", True),
  ("VALUE: the one-week recency bonus reaches back eight days",
   "builder.go", "} else if daysSinceAccess < 7 {", "} else if daysSinceAccess < 8 {", True),
  ("VALUE: the one-week recency bonus shrinks to six days",
   "builder.go", "} else if daysSinceAccess < 7 {", "} else if daysSinceAccess < 6 {", True),
  ("VALUE: the fresh-memory bonus moves 0.2 -> 0.3",
   "builder.go", "score += 0.2", "score += 0.3", True),
  ("VALUE: the this-week bonus moves 0.1 -> 0.15",
   "builder.go", "score += 0.1", "score += 0.15", True),
  ("MECHANISM: the fresh-memory bonus stops being paid at all",
   "builder.go", "score += 0.2", "score += 0.0", True),
  ("MECHANISM: the this-week bonus stops being paid at all",
   "builder.go", "score += 0.1", "score += 0.0", True),

  # ---- prepare.go loadRecentFiles: the age string -------------------------
  ("VALUE: the minutes/hours cut in the age string moves 60 -> 61",
   "prepare.go", "if ageMinutes < 60 {\n\t\t\tageStr", "if ageMinutes < 61 {\n\t\t\tageStr", True),
  ("VALUE: the minutes/hours cut in the age string moves 60 -> 59",
   "prepare.go", "if ageMinutes < 60 {\n\t\t\tageStr", "if ageMinutes < 59 {\n\t\t\tageStr", True),
  ("VALUE: the hours conversion divides by 61 instead of 60",
   "prepare.go", "hours := ageMinutes / 60", "hours := ageMinutes / 61", True),
  # Re-aimed. Written first as the hours branch counting minutes
  # (`hour%s ago", ageMinutes`), which orphans `hours` — and `go test` runs vet,
  # so the case reported a compile error instead of a score. An unbuildable
  # mutation is a defect in the scorer, not a miss by the suite. The unit word
  # is the same mechanism and mutating it on the minutes branch compiles.
  ("MECHANISM: a minutes-old file is described in hours",
   "prepare.go", 'ageStr = fmt.Sprintf("%d minute%s ago", ageMinutes, pluralString(ageMinutes))',
   'ageStr = fmt.Sprintf("%d hour%s ago", ageMinutes, pluralString(ageMinutes))', True),

  # ---- prepare.go loadRecentFiles: the importance buckets -----------------
  ("VALUE: the last-hour importance cut moves 60 -> 61",
   "prepare.go", "if ageMinutes < 60 {\n\t\t\timportance = 0.7", "if ageMinutes < 61 {\n\t\t\timportance = 0.7", True),
  ("VALUE: the last-hour importance cut moves 60 -> 59",
   "prepare.go", "if ageMinutes < 60 {\n\t\t\timportance = 0.7", "if ageMinutes < 59 {\n\t\t\timportance = 0.7", True),
  ("VALUE: the six-hour importance cut moves 360 -> 361",
   "prepare.go", "} else if ageMinutes < 360 {", "} else if ageMinutes < 361 {", True),
  ("VALUE: the six-hour importance cut moves 360 -> 359",
   "prepare.go", "} else if ageMinutes < 360 {", "} else if ageMinutes < 359 {", True),
  ("VALUE: a file modified in the last hour is worth 0.75, not 0.7",
   "prepare.go", "importance = 0.7", "importance = 0.75", True),
  ("VALUE: a file modified in the last six hours is worth 0.65, not 0.6",
   "prepare.go", "importance = 0.6", "importance = 0.65", True),
  ("VALUE: the baseline recent-file importance moves 0.5 -> 0.55",
   "prepare.go", "importance := 0.5", "importance := 0.55", True),
  ("MECHANISM: every recent file is worth the same, whatever its age",
   "prepare.go", "importance = 0.7 // modified in last hour", "importance = 0.5 // modified in last hour", True),

  # ---- management.go DecayImportance: the same one-day line, written again --
  ("VALUE: the decay cut moves from a day back to half a day",
   "management.go", "dayAgo := now.Add(-24 * time.Hour).Unix()", "dayAgo := now.Add(-12 * time.Hour).Unix()", True),
  ("VALUE: the decay cut moves from a day back to two days",
   "management.go", "dayAgo := now.Add(-24 * time.Hour).Unix()", "dayAgo := now.Add(-48 * time.Hour).Unix()", True),
  ("VALUE: the daily decay factor moves 0.99 -> 0.991",
   "management.go", "s.db.Exec(query, 0.99, dayAgo)", "s.db.Exec(query, 0.991, dayAgo)", True),
  ("VALUE: the daily decay factor moves 0.99 -> 0.98",
   "management.go", "s.db.Exec(query, 0.99, dayAgo)", "s.db.Exec(query, 0.98, dayAgo)", True),
  ("MECHANISM: decay stops firing at all",
   "management.go", "s.db.Exec(query, 0.99, dayAgo)", "s.db.Exec(query, 1.0, dayAgo)", True),

  # ---- Controls ------------------------------------------------------------
  # POSITIVE. The tag bonus is the score input this package pins hardest
  # (TestOrdinaryMemoriesAreStillRankedByScore builds ids that contradict
  # importance so a matching tag has to overrule them). A harness that cannot
  # report CAUGHT for this is measuring nothing, and every UNNOTICED below
  # would be an artifact rather than a finding.
  ("CONTROL (positive): the tag-match bonus stops being paid",
   "builder.go", "score += float64(matchCount) * 0.3", "score += float64(matchCount) * 0.0", True),
  # POSITIVE, on the other file, because a control reads the package it runs in
  # and prepare.go's rows are scored by different tests than builder.go's.
  ("CONTROL (positive): recent files lose the tag the suite finds them by",
   "prepare.go", 'tags := []string{"recent", "file:" + f.RelativePath}',
   'tags := []string{"file:" + f.RelativePath}', True),
  # NO-OP. Same value, written differently. A row that comes back CAUGHT here
  # means the suite is reacting to the edit rather than to the behavior.
  ("CONTROL (no-op): the one-day cut is spelled 1.0 instead of 1",
   "builder.go", "if daysSinceAccess < 1 {", "if daysSinceAccess < 1.0 {", False),
  ("CONTROL (no-op): the age subtraction gains a redundant pair of parentheses",
   "prepare.go", "ageMinutes := int(time.Since(f.ModTime).Minutes())",
   "ageMinutes := int((time.Since(f.ModTime)).Minutes())", False),
]

MUTATED_FILES = ["builder.go", "prepare.go", "management.go"]

# --only SUBSTR runs just the rows whose label contains SUBSTR, and may be
# repeated. It exists so a row added after a full run can be measured against
# the old tree without paying for the whole list again; the full list is what
# a score should be quoted from.
#
# What it skips is printed. A harness that narrows its own scope silently
# reports a partial run in the same shape as a complete one.
_only = [sys.argv[i + 1] for i, a in enumerate(sys.argv) if a == "--only"]
if _only:
    _kept = [c for c in CASES if any(s in c[0] for s in _only)]
    print("--only %s: running %d of %d rows, skipping %d"
          % (", ".join(repr(s) for s in _only), len(_kept), len(CASES), len(CASES) - len(_kept)))
    if not _kept:
        sys.exit("no row matched --only; nothing would be measured")
    CASES = _kept

# restore() is `git checkout --`, and it runs at the top of every case. It cannot
# tell a mutation this scorer wrote from work nobody has committed yet, so a
# dirty target would be deleted rather than scored. Refuse instead.
_dirty = subprocess.run(["git", "status", "--porcelain", "--"] + MUTATED_FILES,
                        cwd=REPO, capture_output=True, text=True,
                        check=True).stdout.strip()
if _dirty:
    sys.exit("REFUSING: these have uncommitted changes; this harness restores "
             "from git and would delete them:\n%s" % _dirty)


def restore():
    subprocess.run(["git", "checkout", "--"] + MUTATED_FILES,
                   cwd=REPO, check=True)


score = 0
# Between the write below and the next restore, a tracked source file holds a
# deliberately broken version of itself. A try/finally covers SIGINT alone —
# the signal you press by hand while watching — and misses SIGTERM and SIGHUP,
# which are what a wall-clock cap and a process-group kill actually send. The
# handler restores, reinstates the previous disposition and re-raises, so the
# process dies BY the signal instead of telling its caller a killed run passed.
# SIGKILL cannot be caught here, and that gap is named rather than papered over.
_previous_handlers = {}


def _restore_and_reraise(signum, frame):
    restore()
    signal.signal(signum, _previous_handlers[signum])
    os.kill(os.getpid(), signum)


for _sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
    _previous_handlers[_sig] = signal.signal(_sig, _restore_and_reraise)

try:
    for label, fname, old, new, expect in CASES:
        restore()
        p = REPO / fname
        text = p.read_text()
        if old not in text:
            print(f"  SETUP FAIL   {label}\n      pattern not found in {fname}")
            continue
        p.write_text(text.replace(old, new, 1))
        r = subprocess.run(["go", "test", "-count=1", "."],
                           cwd=REPO, capture_output=True, text=True)
        out = r.stdout + r.stderr
        detail = ""
        if "build failed" in out or "cannot use" in out or "declared and not used" in out or "[build failed]" in out:
            verdict = "COMPILE ERROR"
        elif r.returncode != 0:
            verdict, detail = classify_caught(out)
        else:
            verdict = "UNNOTICED"
        ok = counts_as_coverage(verdict) == expect
        score += ok
        which = ""
        if verdict.startswith("CAUGHT"):
            which = " by: " + ",".join(sorted({l.split()[2].split("/")[0]
                     for l in out.splitlines() if l.startswith("--- FAIL:")}))
        if detail:
            which += "\n        ↳ " + detail
        print(f"  {'ok  ' if ok else 'BAD '} {verdict:<22} (want {'CAUGHT' if expect else 'UNNOTICED'}) {label}{which}")
finally:
    restore()
    for _sig, _handler in _previous_handlers.items():
        signal.signal(_sig, _handler)
print(f"\nscore {score}/{len(CASES)}")
sys.exit(0 if score == len(CASES) else 1)
