"""Sabotage-score the tests that stop ListByOrchestrator swallowing a tag failure.

A suite that passes tells you nothing on its own. This puts the defect back one
way at a time and checks that the tests notice it — and, for the two
known-negative controls, that they do NOT. A scorer with no control reports
CAUGHT for everything and looks perfect while measuring nothing.

The engine is the one on `fix/truncation-never-splits-a-rune`, copied verbatim;
only the case list, the guard markers, the test list and the restore set below
are new. Card 55d41f34 tracks unifying the copies.

The rows ask three questions.

SWALLOW rows put the discarded error back, in each of the shapes it can take: the
error dropped outright, the error returned alongside a partial page, the lookup
skipped for a page too small to bother with.

ATTRIBUTION rows keep the error handling and break which memory gets which tags —
a batched lookup can return every tag it was asked for and still hand them to the
wrong row, and nothing about the error contract observes that.

REACH rows panic at the function's first line. Before these tests existed the
whole orchestrator family took that guard with the suite still green, which is
how the census that found this card describes it. A reach row that reads
UNNOTICED means the tests below are asserting against nothing.

Run from anywhere:  python3 scripts/sabotage-orchestrator-tags.py
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
    'the untagged memory did not come back at all',
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
        ("--- FAIL: T\n    x_test.go:9: the untagged memory did not come back at all\n", "CAUGHT (guard)"),
        ("--- FAIL: T\n    x_test.go:9: the untagged memory did not come back at all\n    x_test.go:12: got 1 want 2\n",
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

  # ---------------------------------------------------------------- SWALLOW
  ("SWALLOW: the tag-load error is dropped again, exactly as it was",
   "management.go",
   "tagsByMemory, err := s.loadTagsForMemories(ids)\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}",
   "tagsByMemory, _ := s.loadTagsForMemories(ids)",
   True),
  ("SWALLOW: the error is returned WITH the partial page, so a caller can use both",
   "management.go",
   "tagsByMemory, err := s.loadTagsForMemories(ids)\n\t\tif err != nil {\n\t\t\treturn nil, err",
   "tagsByMemory, err := s.loadTagsForMemories(ids)\n\t\tif err != nil {\n\t\t\treturn result, err",
   True),
  ("SWALLOW: the lookup is skipped for a one-memory page, which then cannot fail",
   "management.go", "if len(result) > 0 {", "if len(result) > 1 {", True),

  # ------------------------------------------------------------ ATTRIBUTION
  ("ATTRIBUTION: every memory is handed the first memory's tags",
   "management.go", "result[i].Tags = tagsByMemory[result[i].ID]",
   "result[i].Tags = tagsByMemory[ids[0]]", True),
  ("ATTRIBUTION: only the first memory is asked about, so the rest come back bare",
   "management.go", "tagsByMemory, err := s.loadTagsForMemories(ids)",
   "tagsByMemory, err := s.loadTagsForMemories(ids[:1])", True),
  ("ATTRIBUTION: an untagged memory keeps a nil list, which marshals to null",
   "management.go",
   "if result[i].Tags == nil {\n\t\t\t\tresult[i].Tags = []string{}\n\t\t\t}",
   "_ = i",
   True),

  # ------------------------------------------------------------------ REACH
  ("REACH: the function panics at its first line",
   "management.go",
   "func (s *Store) ListByOrchestrator(orchestrator string, limit int, minImportance float64) ([]Memory, error) {",
   "func (s *Store) ListByOrchestrator(orchestrator string, limit int, minImportance float64) ([]Memory, error) {\n\tpanic(\"reach guard\")",
   True),

  # --------------------------------------------------------------- CONTROLS
  # Known-NEGATIVE. loadTagsForMemories over zero ids returns an empty map and
  # no error, and the loop under it does not run, so admitting an empty page to
  # the block changes nothing. A harness that reports CAUGHT here is reporting
  # CAUGHT for everything.
  ("CONTROL (dominated): the page-size guard admits an empty page",
   "management.go", "if len(result) > 0 {", "if len(result) >= 0 {", False),
  # Known-NEGATIVE. Two spellings of the same empty slice.
  ("CONTROL (no-op): the empty tag list is built with make instead of a literal",
   "management.go", "result[i].Tags = []string{}", "result[i].Tags = make([]string, 0)", False),
]

# Go's -run matches unanchored, so a prefix here pulls in every test that starts
# with it. Each name is still spelled out: a list that relies on prefix matching
# silently gains and loses targets as tests are renamed, and the score would move
# with no case-list edit to explain it.
TESTS = "|".join((
    "TestListByOrchestratorReturnsATagLookupFailureRatherThanUntaggedMemories",
    "TestListByOrchestratorReportsATagFailureEvenWhenNoMemoryHasTags",
    "TestListByOrchestratorGivesEachMemoryItsOwnTags",
    "TestListByOrchestratorGivesAnUntaggedMemoryAnEmptySliceNotNil",
    "TestListByOrchestratorTagsSurviveAPageLargerThanOneChunk",
))

TARGETS = ["management.go"]


def restore():
    subprocess.run(["git", "checkout", "--"] + TARGETS, cwd=REPO, check=True)


def refuse_a_dirty_target():
    """Stop before the first restore() throws away uncommitted work.

    restore() is `git checkout --`, so it resets a target file to HEAD. Run this
    scorer against a fix that is written but not committed and the FIRST restore
    deletes the fix, and every case after it scores the old code. Measured on
    this script the night it was written: eight rows read `SETUP FAIL: pattern
    not found` and the ninth read `ok`, which is what a scorer looks like when it
    has silently eaten the thing it was pointed at. `SETUP FAIL` reads as a stale
    case list, so the symptom accuses the wrong file.

    A scorer that destroys its own subject has to say so at the top, not leave a
    diagnosis in the output.
    """
    r = subprocess.run(["git", "status", "--porcelain", "--"] + TARGETS,
                       cwd=REPO, capture_output=True, text=True, check=True)
    if r.stdout.strip():
        print("REFUSING TO RUN: uncommitted changes in a file this scorer restores:\n")
        print(r.stdout.rstrip())
        print("\nrestore() is `git checkout --`, so the first case would delete them and")
        print("the whole run would score HEAD instead. Commit or stash first.")
        sys.exit(2)


refuse_a_dirty_target()

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
