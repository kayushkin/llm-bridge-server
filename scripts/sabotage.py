#!/usr/bin/env python3
"""The sabotage-scoring engine, shared by every per-binary case file.

A test suite that passes tells you nothing on its own. A scorer applies one
edit per mechanism to the file under test, runs the suite, and records whether
anything went red.

Rules this engine carries, each one inherited from a night that lost time to
its absence:

  - The needle must be found. A replacement whose search string is not in the
    file is a case that silently did nothing and scored UNNOTICED (30th pass).
  - The file's bytes must actually change. An edit that changed nothing scores
    UNNOTICED for free, which is the same false result (33rd).
  - A case may carry a SECOND edit, so an orphaned import or variable reports a
    score instead of `compile error` (32nd, 35th).
  - Two controls. A known-positive (an edit every test must catch) and a
    known-NEGATIVE (an edit no test should catch). Without the negative, a
    harness that reports CAUGHT for everything looks perfect (33rd).
  - No needle text is handed to a shell (25th).
  - Restores from git, so the tree must be committed before running (27th).
  - Prints the diff it actually applied, because the row prints the name you
    gave it, not the edit you made (33rd).

The engine was extracted in the scheduler repo when cmd/ask
became the second binary to need it. Ten unattended passes had each rebuilt
this same logic from prose and hit the same traps doing it; a second copy would
have been the eleventh. A per-binary file supplies TARGET, PACKAGES and CASES
and calls score() — nothing else needs restating.
"""

import os
import re
import signal
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent


@dataclass
class Case:
    name: str
    # (find, replace) pairs. Every find must appear in the file.
    edits: list = field(default_factory=list)
    # A case the suite is NOT expected to catch, and why.
    expected_unnoticed: str = ""
    # A case the suite only goes red on because a fixture guard fires, plus the
    # reason that is knowingly accepted. Declaring it records the inflation;
    # leaving it undeclared makes the row a problem. See classify_caught().
    expected_guard_caught: str = ""


# A `go test` failure line: "    truncate_test.go:51: input never reaches the cut".
# Anchored on the _test.go filename so a stack frame or a logged colon cannot be
# read as a failure message.
_FAIL_LINE = re.compile(r"^\s*(\S+_test\.go):(\d+): (.*)$", re.M)

# A stack frame naming a .go file: "\t/home/u/repo/internal/matrix/truncate.go:31".
# The absolute path is captured because the Go runtime's own frames
# (runtime/panic.go) would otherwise pass a bare-filename filter and be read as
# our source.
_FRAME = re.compile(r"^\s+(/\S+\.go):(\d+)", re.M)


def failure_messages(output):
    """Every assertion or guard message a red `go test` run printed, in order."""
    return [(f, int(n), msg) for f, n, msg in _FAIL_LINE.findall(output)]


def counts_as_coverage(verdict):
    """Whether a verdict means an assertion actually looked at the mechanism.

    The whole point of the split: `go test` exits non-zero for detection and for
    the fixture falling over alike, so "red" and "covered" are different
    questions and only this function answers the second one.
    """
    return verdict == "CAUGHT" or verdict.startswith("CAUGHT (panic in ")


def classify_caught(output, guard_markers):
    """Split a red run into detection and the fixture falling over.

    `go test` exits non-zero for both, and its exit code was all this engine
    read until now — so a mutation that trips a reach guard (the 33rd's
    `t.Fatalf` for "the input no longer reaches the code under test") scored a
    flat CAUGHT with no assertion having looked at the behaviour at all.
    Measured in multichat by the 42nd: a row reading CAUGHT while nothing
    checked the mechanism it named, and that catch would disappear the moment
    the fixture was resized, with the table still reading CAUGHT.

    Guards are named by the case file, because only it knows which of its
    suite's `t.Fatalf` messages are guards rather than assertions. That makes
    the automatic verdict opt-in, and an opt-in measures adoption rather than
    presence (the 82nd) — so the message itself is printed beside every CAUGHT
    row whether or not markers are declared. The evidence is on screen in the
    ordinary run; declaring a marker only promotes it to a verdict the next run
    enforces on its own.

    Returns (verdict, detail).
    """
    # Evidence in descending order of strength. An assertion message is read
    # before the stack, because a run can carry both — a suite that asserts the
    # defect in one test and crashes on it in another is covered either way, and
    # reporting the crash there hides the assertion that is the better answer.
    messages = [m for _, _, m in failure_messages(output)]
    guard = [m for m in messages if any(g in m for g in guard_markers)]
    real = [m for m in messages if m not in guard]
    if real:
        return "CAUGHT", real[0][:100]
    if "panic:" in output:
        # Read WHERE it panicked, not just that it did. A mutation the test drove
        # into a crash IS detection — the program died instead of returning a
        # wrong answer. A panic in the fixture is the test falling over before it
        # asserted anything, which is not. Take the first non-test repo frame
        # rather than requiring that no test frame is present: a panic in
        # production code always has the calling test frame below it.
        frames = [f for f, _ in _FRAME.findall(output.split("panic:", 1)[1])
                  if f.startswith(str(REPO) + "/")]
        source = next((f for f in frames if not f.endswith("_test.go")), None)
        if source:
            return ("CAUGHT (panic in %s)" % Path(source).name,
                    "the test drove the mutation into a crash")
        return "CAUGHT (fixture panicked)", "the test fell over before asserting"
    if guard:
        return "CAUGHT (guard)", guard[0][:100]
    # Red with no assertion, no guard and no panic: a timeout, or the suite
    # failing before it reached anything. Surfaced for reading, never counted.
    return "CAUGHT (no message)", ""


def _run_tests(packages):
    proc = subprocess.run(
        ["go", "test", "-count=1"] + packages,
        cwd=REPO, capture_output=True, text=True,
    )
    out = proc.stdout + proc.stderr
    if ("build failed" in out or "[build failed]" in out
            or "cannot find" in out or "syntax error" in out):
        return "compile error", out
    if proc.returncode == 0:
        return "UNNOTICED", out
    return "CAUGHT", out


def problems(results):
    """Everything wrong with a scored run, as printable lines. Empty means sound.

    This is the one definition of "wrong". score() prints it, and a caller that
    needs an exit status asks the same question rather than re-deriving it: a
    caller's own copy of these rules drifts silently, and always toward green,
    because the copy is the half nobody re-reads when a rule is added here.
    """
    found = []
    for case, verdict, _, _ in results:
        if case.name.startswith("CONTROL known-positive") and not counts_as_coverage(verdict):
            found.append("the known-positive control was NOT caught — the suite is not running")
        if case.name.startswith("CONTROL known-negative") and verdict.startswith("CAUGHT"):
            found.append("the known-negative control WAS caught — the suite is red for a "
                         "reason unrelated to behaviour, so every CAUGHT above is suspect")
        if verdict == "compile error" and not case.expected_unnoticed:
            found.append("compile error, not a score: %s" % case.name)
        if verdict == "UNNOTICED" and not case.expected_unnoticed:
            found.append("UNNOTICED: %s" % case.name)
        # Red without an assertion. Each of these reads as coverage in a table
        # scored on the exit code alone, and none of them is.
        if verdict == "CAUGHT (guard)" and not case.expected_guard_caught:
            found.append("guard-caught, NOT coverage: %s — the suite went red because a "
                         "fixture guard fired, so no assertion looked at this mechanism"
                         % case.name)
        if verdict == "CAUGHT (fixture panicked)" and not case.expected_guard_caught:
            found.append("the fixture panicked, NOT coverage: %s — the test fell over "
                         "before it asserted anything" % case.name)
        if verdict == "CAUGHT (no message)" and not case.expected_guard_caught:
            found.append("CAUGHT with no assertion text and no panic (a timeout?) — read "
                         "the run before counting it: %s" % case.name)
    return found


def control_summary(results):
    """The closing line for a target with nothing wrong, saying what was observed.

    This used to be the fixed sentence "both controls behaved; every real
    mechanism is pinned by an assertion", printed whenever problems() came back
    empty. problems() only ever inspects a control that EXISTS, so a target
    carrying no controls at all got the same reassuring sentence as one carrying
    two working ones — measured in llm-bridge-server, where three of five targets
    have no control and every one of them printed it.

    That is the 176th's rule inverted. A harness that reports success has to be
    shown capable of reporting failure, and this line claimed the showing had
    happened on targets where nothing had been shown at all.
    """
    kinds = {c.name.split(":")[0].strip() for c in results if c.name.startswith("CONTROL")}
    pinned = "every real mechanism is pinned by an assertion"
    if {"CONTROL known-positive", "CONTROL known-negative"} <= kinds:
        return "both controls behaved; " + pinned
    if kinds:
        missing = ("known-negative" if "CONTROL known-positive" in kinds else "known-positive")
        return ("%s, on ONE control — with no %s there is nothing here to tell a working "
                "instrument from a quiet one" % (pinned, missing))
    return ("%s — but this target carries NO control, so nothing in this run shows the "
            "suite can report a failure at all" % pinned)


def score(target: Path, packages, cases, guard_markers=()):
    """Apply each case to target, run packages, and print a scored table.

    Pass --diffs to print the edit each case actually applied. A row prints the
    name you gave it, not the edit you made, and mislabelled cases have twice
    been read as coverage holes that were not there — so read the diffs at
    least once per case family.

    guard_markers are substrings of this suite's fixture-guard messages. See
    classify_caught(): supplying them turns "which failure fired" from something
    the reader works out into something the next run enforces.
    """
    show_diffs = "--diffs" in sys.argv
    show_messages = "--messages" in sys.argv
    rel = str(target.relative_to(REPO))

    dirty = subprocess.run(["git", "status", "--porcelain", rel],
                           cwd=REPO, capture_output=True, text=True).stdout.strip()
    if dirty:
        sys.exit("REFUSING: %s has uncommitted changes; this harness restores "
                 "from git and would delete them." % target)

    def restore():
        subprocess.run(["git", "checkout", "--", rel], cwd=REPO, check=True)

    baseline, out = _run_tests(packages)
    if baseline != "UNNOTICED":
        sys.exit("REFUSING: the suite is not green before sabotage (%s)\n%s" % (baseline, out))
    print("baseline: green\n")

    original = target.read_text()
    results = []
    # The target is a deliberately broken file from the write below until the
    # restore after the suite runs, so every way out of this loop has to restore
    # — including the ways this engine does not choose to take. A killed run used
    # to leave the broken file in the tree as ordinary-looking uncommitted work:
    # a semantic edit to a tracked source file, which `git status` reports the
    # same way it reports real work in progress, and which this box's standing
    # rule tells the next agent not to throw away. The next run then refuses to
    # start, because of the mess the last one made.
    #
    # A try/finally alone does NOT close this, and measuring it is how you find
    # that out. Python raises KeyboardInterrupt for SIGINT, so a finally is on
    # the way out for that one and for nothing else. SIGTERM and SIGHUP kill the
    # process between the write and the restore — and those are exactly what a
    # wall-clock cap, systemd and a process-group kill send. So the one signal a
    # finally covers is the one you press by hand while watching, and the ones it
    # misses are the ones an unattended run actually receives. Measured by kill
    # on this engine before these handlers existed: SIGTERM and SIGHUP each left
    # the mutated file behind.
    #
    # The handler restores, reinstates the disposition it replaced and re-raises,
    # so the process dies BY the signal (rc 128+signum). A handler that restores
    # and exits 0 tells every caller a killed run succeeded.
    #
    # SIGKILL cannot be caught by the process that receives it. It is the one gap
    # left here, and it is named rather than papered over.
    previous_handlers = {}

    def restore_and_reraise(signum, frame):
        restore()
        signal.signal(signum, previous_handlers[signum])
        os.kill(os.getpid(), signum)

    for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        previous_handlers[sig] = signal.signal(sig, restore_and_reraise)

    try:
        for case in cases:
            text = original
            for find, replace in case.edits:
                if find not in text:
                    restore()
                    sys.exit("ABORT [%s]: needle not found in %s.\n"
                             "A case whose needle is missing changes nothing and scores "
                             "UNNOTICED, which reads as a coverage hole that is not there.\n"
                             "Needle was:\n%s" % (case.name, target.name, find))
                text = text.replace(find, replace, 1)
            if text == original:
                restore()
                sys.exit("ABORT [%s]: the edit produced an identical file." % case.name)
            target.write_text(text)
            verdict, out = _run_tests(packages)
            detail = ""
            fired = []
            if verdict == "CAUGHT":
                verdict, detail = classify_caught(out, guard_markers)
                fired = failure_messages(out)
            # Read the diff the harness actually applied, not the label it was given.
            diff_cmd = ["git", "diff"] + ([] if show_diffs else ["--stat"]) + ["--", rel]
            diff = subprocess.run(diff_cmd, cwd=REPO, capture_output=True, text=True).stdout.strip()
            restore()
            results.append((case, verdict, diff, detail))
            print("%-20s %s" % (verdict, case.name))
            # The message that fired, always — this is what says whether the row
            # is coverage or the fixture falling over, and re-running the
            # mutation by hand to read it is the expensive way to find out.
            if detail:
                print("%-20s   ↳ %s" % ("", detail))
            # --messages prints every line that went red, not just the first.
            # Classifying a row needs all of them: a run whose first failure is a
            # guard may still carry an assertion below it, and reading only the
            # top line files that row as guard-caught when it is covered.
            if show_messages and fired:
                for f, n, msg in fired:
                    mark = "guard" if any(g in msg for g in guard_markers) else "     "
                    print("%-20s     [%s] %s:%d: %s" % ("", mark, f, n, msg[:110]))
            if show_diffs:
                body = "\n".join(l for l in diff.split("\n")
                                 if l.startswith(("+", "-")) and not l.startswith(("+++", "---")))
                print("\n".join("        " + l for l in body.split("\n")) + "\n")
    finally:
        restore()
        for sig, handler in previous_handlers.items():
            signal.signal(sig, handler)

    print("\n================ score ================")
    real = [c for c, _, _, _ in results if not c.name.startswith("CONTROL")]
    caught = sum(1 for c, v, _, _ in results
                 if counts_as_coverage(v) and not c.name.startswith("CONTROL"))
    guard_only = sum(1 for c, v, _, _ in results
                     if v.startswith("CAUGHT") and not counts_as_coverage(v)
                     and not c.name.startswith("CONTROL"))
    print("%d/%d real mechanisms caught by an assertion" % (caught, len(real)))
    if guard_only:
        # Counted apart, never added in. A guard firing means the fixture stopped
        # reaching the code under test; the mechanism the row names is unpinned,
        # and folding these into the score is the inflation this split exists for.
        print("%d further row(s) went red without an assertion — not coverage" % guard_only)

    found = problems(results)
    for p in found:
        print("  ⚠️  " + p)
    if not found:
        print("  " + control_summary([c for c, _, _, _ in results]))
    return results


def self_test():
    """Drive classify_caught() and problems() against every verdict they return.

    The 43rd's rule: when you automate a check, the check is the next unmeasured
    claim. A classify_caught() that can never return "guard" prints the clean
    score you were hoping for, and the 48th shipped exactly that — a scorer
    whose unreachable branch read as a perfect run. No case table can score its
    own scorer, so these are driven from synthetic output instead.
    """
    markers = ("input never reaches the cut", "this test proves nothing")
    probes = [
        # An assertion fired: real coverage.
        ("--- FAIL: TestX\n    truncate_test.go:88: preview is not valid UTF-8: \"ab\"\n",
         "CAUGHT"),
        # Only a guard fired: the fixture stopped reaching the code under test.
        ("--- FAIL: TestX\n    truncate_test.go:51: input never reaches the cut: "
         "len(body)=40, budget=100\n", "CAUGHT (guard)"),
        # Both fired. One real assertion is enough to make the row coverage —
        # a guard tripping alongside it does not subtract from what was proven.
        ("--- FAIL: TestX\n    truncate_test.go:51: input never reaches the cut: x\n"
         "    truncate_test.go:88: preview is not valid UTF-8: \"ab\"\n", "CAUGHT"),
        # Both panic branches, separated only by which file the top repo frame
        # names. The runtime's own panic.go frame is present in each and must not
        # be mistaken for our source. The production-code probe carries a test
        # frame too, because a real one always does.
        ("panic: slice bounds out of range\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/internal/matrix/truncate.go:31\n"
         "\t%s/internal/matrix/truncate_test.go:41\n" % (REPO, REPO),
         "CAUGHT (panic in truncate.go)"),
        ("panic: index out of range\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/internal/matrix/truncate_test.go:149\n" % REPO,
         "CAUGHT (fixture panicked)"),
        # A guard fired AND production code crashed: the crash is the detection,
        # so the stack is read once no assertion message is left to read.
        ("--- FAIL: TestX\n    truncate_test.go:51: input never reaches the cut: x\n"
         "panic: slice bounds out of range\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/internal/matrix/truncate.go:31\n" % REPO, "CAUGHT (panic in truncate.go)"),
        # An assertion fired AND production code crashed. The assertion wins:
        # it names the behaviour, and both mean the mechanism is pinned.
        ("--- FAIL: TestX\n    truncate_test.go:75: preview overruns its budget: 101\n"
         "panic: slice bounds out of range\n"
         "\t%s/internal/matrix/truncate.go:31\n" % REPO, "CAUGHT"),
        # Red with no panic and nothing naming a test line.
        ("--- FAIL: TestX\npanic-free timeout, no test line\n", "CAUGHT (no message)"),
        # A path with a directory in it still parses; a bare colon in prose does not.
        ("--- FAIL: TestX\n    internal/matrix/truncate_test.go:12: want: got\n", "CAUGHT"),
    ]
    ok = True
    for output, want in probes:
        got, _ = classify_caught(output, markers)
        if got != want:
            print("SELF-TEST FAIL: classify_caught -> %r, want %r" % (got, want))
            ok = False

    # problems() must flag a guard-caught row and must fall silent once the case
    # file declares it. Both directions, because a rule that never fires and a
    # rule that always fires are equally useless and look the same from green.
    guard_row = [(Case("the budget drifts"), "CAUGHT (guard)", "", "")]
    if not problems(guard_row):
        print("SELF-TEST FAIL: an undeclared guard-caught row was not flagged")
        ok = False
    declared = [(Case("the budget drifts", expected_guard_caught="known"),
                 "CAUGHT (guard)", "", "")]
    if problems(declared):
        print("SELF-TEST FAIL: a declared guard-caught row was flagged anyway")
        ok = False

    # control_summary is the sentence a reader takes away from a clean target, so
    # each of its three states is pinned. The one that matters is the last: it
    # was previously indistinguishable from the first, which is how three
    # uncontrolled targets came to print "both controls behaved".
    both = [Case("CONTROL known-positive: x"), Case("CONTROL known-negative: y")]
    for cases_in, want in [(both, "both controls behaved"),
                           (both[:1], "on ONE control"),
                           (both[1:], "on ONE control"),
                           ([Case("the budget drifts")], "carries NO control")]:
        if want not in control_summary(cases_in):
            print("SELF-TEST FAIL: control_summary(%r) is missing %r"
                  % ([c.name for c in cases_in], want))
            ok = False
    # The single-control wording has to name the half that is absent, or it reads
    # the same either way and tells the reader nothing they can act on.
    if "no known-negative" not in control_summary(both[:1]):
        print("SELF-TEST FAIL: control_summary did not name the missing known-negative")
        ok = False
    if "no known-positive" not in control_summary(both[1:]):
        print("SELF-TEST FAIL: control_summary did not name the missing known-positive")
        ok = False

    # counts_as_coverage decides both the printed score and whether the
    # known-positive control is believed, so each verdict is pinned by name. A
    # panic in production code is coverage; every other non-plain CAUGHT is not.
    for verdict, want in [("CAUGHT", True), ("CAUGHT (panic in truncate.go)", True),
                          ("CAUGHT (guard)", False), ("CAUGHT (fixture panicked)", False),
                          ("CAUGHT (no message)", False), ("UNNOTICED", False),
                          ("compile error", False)]:
        if counts_as_coverage(verdict) != want:
            print("SELF-TEST FAIL: counts_as_coverage(%r) != %r" % (verdict, want))
            ok = False

    print("self-test: %s" % ("all verdicts reachable and separated" if ok else "FAILED"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(self_test())
