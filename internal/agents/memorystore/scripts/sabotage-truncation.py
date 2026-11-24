"""Sabotage-score the rune-boundary truncation tests and their budgets.

A suite that passes tells you nothing on its own. This puts a defect back one at
a time and checks that the tests notice it — and, for the two known-negative
controls, that they do NOT. A scorer with no control reports CAUGHT for
everything and looks perfect while measuring nothing.

The list has two halves and they ask different questions.

MECHANISM rows break the fix itself: a walk-back that stops running, a cut that
overshoots, a call site that reverts to a plain byte cut.

VALUE rows move a numeric literal by one unit — a budget, a guard's threshold, a
divisor. Neither a mutation score nor a function-name census asks whether the
NUMBER a mechanism compares against is observed, and this list scored 6/6 on its
mechanisms while 15 of 18 value moves went unnoticed. A far move (2000 -> 20000)
is a deletion wearing a number and does not count; move the literal by one.

Two of these cases have to be written as drifted comparisons rather than
deletions: removing the walk-back orphans the utf8 import, and `go test` runs
vet, so the case reports a compile error instead of a score.

Run from anywhere:  python3 scripts/sabotage-truncation.py
"""

import os
import pathlib
import re
import signal
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent

# ---------------------------------------------------------------------------
# Telling detection from the fixture falling over.
#
# `go test` exits non-zero when the suite catches the mutation and when a
# reach guard fires — the 33rd's `t.Fatalf` for "the input no longer reaches
# the code under test" — and this scorer read only that exit code. So a
# mutation that resizes the fixture out from under a guard scored CAUGHT with
# nothing having checked the mechanism the row names. Measured in multichat by
# the 42nd: a row reading CAUGHT while no assertion looked at the mechanism,
# and the catch would vanish the moment the fixture was resized, with the table
# still reading CAUGHT.
#
# GUARD_MARKERS names this suite's guards by message, because only this file
# knows which of its own t.Fatalf lines are guards rather than assertions.
GUARD_MARKERS = (
    'content was not truncated, so this test is not exercising the cut',
)

# A `go test` failure line: "    truncation_test.go:65: input no longer reaches".
# Anchored on the _test.go filename so a stack frame cannot be read as a message.
_FAIL_LINE = re.compile(r"^\s*(\S+_test\.go):(\d+): (.*)$", re.M)
# A stack frame naming a .go file. The absolute path is captured because the Go
# runtime's own frames (runtime/panic.go) would pass a bare-filename filter.
_FRAME = re.compile(r"^\s+(/\S+\.go):(\d+)", re.M)


def counts_as_coverage(verdict):
    """Whether a verdict means an assertion actually looked at the mechanism."""
    return verdict == "CAUGHT" or verdict.startswith("CAUGHT (panic in ")


def classify_caught(output):
    """Split a red run into detection and the fixture falling over.

    Evidence in descending order of strength. An assertion message is read
    before the stack, because a run can carry both — a suite that asserts the
    defect in one test and crashes on it in another is covered either way.
    """
    messages = [m for _, _, m in _FAIL_LINE.findall(output)]
    guard = [m for m in messages if any(g in m for g in GUARD_MARKERS)]
    real = [m for m in messages if m not in guard]
    if real:
        return "CAUGHT", real[0][:100]
    if "panic:" in output:
        # A mutation the test drove into a crash IS detection. A panic in the
        # fixture is the test falling over before asserting, which is not.
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
    """Drive classify_caught() against every verdict it can return.

    The 43rd's rule: when you automate a check, the check is the next unmeasured
    claim, and no case table can score its own scorer. The 48th shipped a
    scorer whose unreachable branch read as a perfect run.
    """
    probes = [
        ("--- FAIL: T\n    x_test.go:9: values differ: got 1 want 2\n", "CAUGHT"),
        ("--- FAIL: T\n    x_test.go:9: content was not truncated, so this test is not exercising the cut\n", "CAUGHT (guard)"),
        ("--- FAIL: T\n    x_test.go:9: content was not truncated, so this test is not exercising the cut\n    x_test.go:12: got 1 want 2\n",
         "CAUGHT"),
        ("panic: boom\n\t/usr/lib/go/src/runtime/panic.go:8\n\t%s/util.go:3\n" % REPO,
         "CAUGHT (panic in util.go)"),
        ("panic: boom\n\t%s/x_test.go:3\n" % REPO, "CAUGHT (fixture panicked)"),
        ("--- FAIL: T\nno test line at all\n", "CAUGHT (no message)"),
    ]
    ok = True
    for output, want in probes:
        got, _ = classify_caught(output)
        if got != want:
            print("SELF-TEST FAIL: classify_caught -> %r, want %r" % (got, want))
            ok = False
    for verdict, want in [("CAUGHT", True), ("CAUGHT (panic in util.go)", True),
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
  ("the walk-back never runs, so the cut splits a rune again",
   "util.go", "for cut > 0 && !utf8.RuneStart(s[cut]) {", "for cut > len(s) && !utf8.RuneStart(s[cut]) {", True),
  ("the helper trims to nothing instead of to the boundary",
   "util.go", "return s[:cut]", "return s[:0]", True),
  ("the walk-back overshoots the boundary by one byte",
   "util.go", "\t}\n\treturn s[:cut]", "\t}\n\treturn s[:cut-1]", True),
  ("only the compaction call site reverts to a byte cut",
   "compaction.go", "truncateAtRuneBoundary(combined, 2000)", "combined[:2000]", True),
  ("only the preview call site reverts to a byte cut",
   "builder.go", "truncateAtRuneBoundary(content, previewChars)", "content[:previewChars]", True),
  # Known-NEGATIVE control. maxBytes==0 already returns "" via s[:0] on the
  # second branch, so narrowing the guard is a behavioral no-op. A harness that
  # reports CAUGHT for this is reporting CAUGHT for everything.
  ("CONTROL (no-op): the maxBytes guard narrows from <=0 to <0",
   "util.go", "if maxBytes <= 0 {", "if maxBytes < 0 {", False),

  # -----------------------------------------------------------------------
  # Boundary VALUES, added by the 184th nightly pass under card 23be5012.
  #
  # Every row above moves a MECHANISM: a walk-back that stops running, a call
  # site that reverts to a byte cut. None of them asks whether the NUMBER the
  # code compares against is observed. A mutation score and a function-name
  # census both answer the first question and neither answers the second, so a
  # list can read 6/6 while every literal in its three targets is free to drift.
  #
  # Each row below moves ONE literal by ONE unit, or swaps one comparison for
  # its adjacent neighbor. A far move (2000 -> 20000) is a deletion wearing a
  # number and is already covered by the mechanism rows.
  ("VALUE: the maxBytes guard widens from <=0 to <=1, so a 1-byte budget yields nothing",
   "util.go", "if maxBytes <= 0 {", "if maxBytes <= 1 {", True),
  ("VALUE: the already-fits test admits one byte over budget",
   "util.go", "if len(s) <= maxBytes {", "if len(s) <= maxBytes+1 {", True),
  ("VALUE: the already-fits test rejects an exact fit",
   "util.go", "if len(s) <= maxBytes {", "if len(s) < maxBytes {", True),
  ("VALUE: the walk-back stops one byte short of the front",
   "util.go", "for cut > 0 && !utf8.RuneStart(s[cut]) {", "for cut > 1 && !utf8.RuneStart(s[cut]) {", True),

  ("VALUE: the compaction cut fires one byte later (2000 -> 2001)",
   "compaction.go", "if len(combined) > 2000 {", "if len(combined) > 2001 {", True),
  ("VALUE: the compaction budget shrinks by one byte (2000 -> 1999)",
   "compaction.go", "truncateAtRuneBoundary(combined, 2000)", "truncateAtRuneBoundary(combined, 1999)", True),
  ("VALUE: the compaction budget grows past its own guard (2000 -> 2001)",
   "compaction.go", "truncateAtRuneBoundary(combined, 2000)", "truncateAtRuneBoundary(combined, 2001)", True),
  ("VALUE: the minimum compactable group size moves 2 -> 3",
   "compaction.go", "if len(members) < 2 {", "if len(members) < 3 {", True),
  ("VALUE: the compactable importance ceiling moves 0.7 -> 0.6",
   "compaction.go", "importance < 0.7", "importance < 0.6", True),
  ("VALUE: the compactable importance floor moves 0 -> 0.1",
   "compaction.go", "importance > 0", "importance > 0.1", True),
  ("VALUE: the compacted memory's own importance moves 0.5 -> 0.6",
   "compaction.go", "Importance: 0.5,", "Importance: 0.6,", True),

  # Known-NEGATIVE control, and it took a sweep to earn the classification.
  # `len(content) <= previewChars` is a fast path with nothing under it: for
  # every previewChars >= 1 the >50% rule four lines below returns the memory
  # unchanged for exactly the same inputs. Swept over previewChars 0..60 at the
  # only length where the two guards can disagree — ONE input differs,
  # previewChars=0, and BuildContext (the sole call site) substitutes 300 for a
  # TruncatePreview of zero before it gets here.
  #
  # FALSIFICATION: TestPreviewLeavesContentOneByteOverTheBudgetWhole holds the
  # claim. If the >50% rule is narrowed so it stops dominating this guard, that
  # test goes red and this row must become expect=True.
  ("CONTROL (dominated): the already-small-enough fast path admits one byte over",
   "builder.go", "if len(content) <= previewChars {", "if len(content) <= previewChars+1 {", False),
  ("VALUE: the default preview budget moves 300 -> 301",
   "builder.go", "truncatePreview = 300", "truncatePreview = 301", True),
  ("VALUE: the default preview budget moves 300 -> 299",
   "builder.go", "truncatePreview = 300", "truncatePreview = 299", True),
  ("VALUE: the default-preview guard widens from <=0 to <=1",
   "builder.go", "if truncatePreview <= 0 {", "if truncatePreview <= 1 {", True),
  ("VALUE: the truncate threshold fires on equality instead of above it",
   "builder.go", "m.Tokens > truncateThreshold", "m.Tokens >= truncateThreshold", True),
  ("VALUE: the >50%-of-content skip rule widens to >33%",
   "builder.go", "if previewChars*2 >= len(content) {", "if previewChars*3 >= len(content) {", True),
  ("VALUE: the >50% skip rule loses its equality, so an exact half truncates",
   "builder.go", "if previewChars*2 >= len(content) {", "if previewChars*2 > len(content) {", True),
  ("VALUE: the newline walk-back accepts a break exactly on halfway",
   "builder.go", "lastNewline > previewChars/2", "lastNewline >= previewChars/2", True),
  ("VALUE: the space walk-back's halfway mark becomes a third",
   "builder.go", "lastSpace > previewChars/2", "lastSpace > previewChars/3", True),
  ("VALUE: the omitted-token estimate divides by 4 instead of 3",
   "builder.go", "m.Tokens - (len(preview)+2)/3", "m.Tokens - (len(preview)+2)/4", True),
  ("VALUE: the truncated memory's token estimate divides by 4 instead of 3",
   "builder.go", "(len(m.Content) + 2) / 3", "(len(m.Content) + 2) / 4", True),
]

# Go's -run matches unanchored, so a prefix here pulls in every test that starts
# with it. Each name is still spelled out: a list that relies on prefix matching
# silently gains and loses targets as tests are renamed, and the score would move
# with no case-list edit to explain it.
TESTS = "|".join((
    # The mechanism suite.
    "TestTruncateAtRuneBoundary",
    "TestTruncateAtRuneBoundaryEdgeCases",
    "TestCompactionContentStaysValidUTF8",
    "TestTruncateMemoryToPreviewStaysValidUTF8",
    # The boundary-VALUE suite, boundary_values_test.go.
    "TestTruncateAtRuneBoundaryKeepsASingleByteBudget",
    "TestCompactionKeepsExactlyTwoThousandBytes",
    "TestCompactionCutFiresExactlyAboveItsBudget",
    "TestCompactionNeedsExactlyTwoMembers",
    "TestCompactionImportanceWindowIsPinnedToItsValues",
    "TestCompactedMemoryTakesImportanceOneHalf",
    "TestPreviewCutIsPinnedToItsBudget",
    "TestPreviewSkipRuleStraddlesHalfTheContent",
    "TestPreviewWordBreakStraddlesHalfway",
    "TestTruncatedPreviewSpellsItsTokenCounts",
    "TestPreviewLeavesContentOneByteOverTheBudgetWhole",
    "TestBuildContextPreviewDefaultsToThreeHundredBytes",
    "TestBuildContextHonorsAOneBytePreview",
    "TestTruncateThresholdStraddlesItsOwnValue",
))

MUTATED_FILES = ["util.go","compaction.go","builder.go"]


# `restore()` below is `git checkout --`, so it puts these files back to HEAD. It
# cannot tell a mutation this scorer wrote from work somebody has not committed
# yet, and it runs at the TOP of every case, before anything is read. So scoring a
# fix that is written but not yet committed deletes the fix and scores HEAD.
#
# The symptom accuses the wrong file. Every case then prints
# `SETUP FAIL: pattern not found`, which reads as a stale case list — so the
# obvious next move is to edit the case list, against a source file the scorer has
# already reverted. Nothing in that output mentions the checkout. Measured in
# memory-store by the 239th nightly pass: eight rows read SETUP FAIL and the ninth
# read ok, and the loss was found by being bitten rather than by reading.
#
# The shared engine (tool-store scripts/sabotage.py) has refused this for a long
# time. This scorer does not import the engine, so it never inherited the refusal.
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
# The file under test holds a deliberately broken version of itself from the write
# in the loop below until the next restore, and this script used to have no way out
# of that window except the ones it chooses to take. A killed run left the mutated
# file behind as ordinary-looking uncommitted work — a semantic edit to a tracked
# source file, which `git status` reports the same way it reports real work in
# progress, and which this box's standing rule tells the next agent not to throw
# away.
#
# A try/finally alone does NOT close this, and measuring it is how you find that
# out. Python raises KeyboardInterrupt for SIGINT, so a finally is on the way out
# for that one and for nothing else. SIGTERM and SIGHUP kill the process between
# the write and the restore — and those are exactly what a wall-clock cap, systemd
# and a process-group kill send. So the one signal a finally covers is the one you
# press by hand while watching, and the ones it misses are the ones an unattended
# run actually receives. Measured by kill on this scorer before these handlers
# existed: SIGTERM and SIGHUP each left the mutated file behind.
#
# The handler restores, reinstates the disposition it replaced and re-raises, so
# the process dies BY the signal (rc 128+signum). A handler that restores and
# exits 0 tells every caller a killed run succeeded.
#
# SIGKILL cannot be caught by the process that receives it. It is the one gap left
# here, and it is named rather than papered over.
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
        p = REPO/fname
        text = p.read_text()
        if old not in text:
            print(f"  SETUP FAIL   {label}\n      pattern not found in {fname}"); continue
        p.write_text(text.replace(old, new, 1))
        r = subprocess.run(["go","test","-count=1","-run",TESTS,"."],
                           cwd=REPO, capture_output=True, text=True)
        out = r.stdout + r.stderr
        detail = ""
        if "build failed" in out or "cannot use" in out or "declared and not used" in out or "[build failed]" in out:
            verdict = "COMPILE ERROR"
        elif r.returncode != 0:
            verdict, detail = classify_caught(out)
        else:
            verdict = "UNNOTICED"
        # counts_as_coverage, not `verdict == "CAUGHT"`: a row that went red
        # because a fixture guard fired is not a mechanism this suite pins.
        ok = counts_as_coverage(verdict) == expect
        score += ok
        which = ""
        if verdict.startswith("CAUGHT"):
            which = " by: " + ",".join(sorted({l.split()[2].split("/")[0]
                     for l in out.splitlines() if l.startswith("--- FAIL:")}))
        if detail:
            which += "\n        \u21b3 " + detail
        print(f"  {'ok  ' if ok else 'BAD '} {verdict:<22} (want {'CAUGHT' if expect else 'UNNOTICED'}) {label}{which}")
finally:
    restore()
    for _sig, _handler in _previous_handlers.items():
        signal.signal(_sig, _handler)
print(f"\nscore {score}/{len(CASES)}")
sys.exit(0 if score == len(CASES) else 1)
