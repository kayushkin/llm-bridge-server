#!/usr/bin/env python3
"""The sabotage-scoring engine, shared by every per-binary case file.

A test suite that passes tells you nothing on its own. A scorer applies one
edit per mechanism to the file under test, runs the suite, and records whether
anything went red.

Rules this engine carries, each one inherited from a night that lost time to
its absence:

  - The needle must be found. A replacement whose search string is not in the
    file is a case that silently did nothing and scored UNNOTICED (30th pass).
  - The needle must be found ONCE. Appearing twice in one file passed the
    file-level check and sabotaged the first occurrence, scoring the wrong
    mechanism while reading as a verdict on the right one (218th).
  - The file's bytes must actually change. An edit that changed nothing scores
    UNNOTICED for free, which is the same false result (33rd).
  - A case may carry a SECOND edit, so an orphaned import or variable reports a
    score instead of `compile error` (32nd, 35th).
  - Two controls. A known-positive (an edit every test must catch) and a
    known-NEGATIVE (an edit no test should catch). Without the negative, a
    harness that reports CAUGHT for everything looks perfect (33rd).
  - No needle text is handed to a shell (25th).
  - Restores from git, so the tree must be committed before running (27th).
  - Restores on the way out even when the run is KILLED, not only on the exit
    paths the engine chooses to take (60th). SIGKILL is the one gap and cannot
    be closed by any process that receives it.
  - Prints the diff it actually applied, because the row prints the name you
    gave it, not the edit you made (33rd).
  - Contradicts a declaration in BOTH directions. An `expected_unnoticed` that
    is CAUGHT anyway has expired, and nothing used to say so (47th, 69th, 94th).
  - Splits a red run into detection and the fixture falling over. `go test`
    exits non-zero for both, so a mutation that only trips a reach guard scored
    a flat CAUGHT with no assertion having looked at the mechanism (42nd).

The engine was extracted from scripts/sabotage-kanban-curator.py TWICE, on two
branches, by two passes that could not see each other: 2ab59cc on
test/first-tests-for-kanban-dispatcher and 338ec17 on test/first-tests-for-ask.
Ten unattended passes had each rebuilt this logic from prose and hit the same
traps doing it; the two extractions were the eleventh and twelfth. A per-binary
file supplies TARGETS, PACKAGES and CASES and calls score() — nothing else
needs restating.

## This blob is the union of a THIRD fork, and here is what it cost

Landed by the 73rd pass as the union of the two extractions above, then forked
again. Measured by the 236th pass and unioned here by the 240th:

    9a81a32e  forge, inber-party, mailstack, skill-store, and the tool-store
              branch test/the-repo-map-and-recent-files-tools-are-unreached
    2932bd4d  tool-store main
    9d5ad6a0  scheduler
    3a7741d9  bundle-store

Neither side of the tool-store collision was simply stale. `2932bd4d` alone
carried the 42nd's guard classification — classify_caught, counts_as_coverage,
failure_messages, Case.expected_guard_caught — and lacked the 218th's
needle-once rule; `9a81a32e` carried the needle-once rule, the multi-target
shape, the cross-table and the stale-declaration check, and had never heard of
a reach guard. This file is both. Propagating it to the other five repos is a
separate decision and has NOT been done.

⚠️ **The fork reached the CALLERS, not only this file, and slot 4 is where it
shows.** `2932bd4d`'s signature ended `score(target, packages, cases,
guard_markers=())` and `9a81a32e`'s ended `score(targets, packages, cases,
unreddened=None)`. Two case files on this box already pass a fourth positional
argument, and they pass DIFFERENT things: tool-store's sabotage-truncation.py
passes GUARD_MARKERS, scheduler's sabotage-reminder-coordinator.py passes
UNREDDENED. A union that kept either in slot 4 would silently bind the other
caller's argument to the wrong parameter, and the direction that fails is the
quiet one — guard_markers landing in unreddened leaves guard_markers empty, so
classify_caught can no longer recognize a guard and every guard-caught row
reads as coverage. That is precisely the inflation the 42nd's split exists to
stop, reintroduced by an argument list. Both are keyword-only here, so a
positional caller raises TypeError at the call instead.
"""

import contextlib
import inspect
import os
import re
import signal
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import tree_hold  # noqa: E402  vendored; see tree_hold.py on keeping copies identical

REPO = Path(__file__).resolve().parent.parent


@dataclass
class Case:
    name: str
    # (find, replace) pairs. Every find must appear in exactly one target file,
    # exactly once.
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
    flat CAUGHT with no assertion having looked at the behavior at all.
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


def _run_tests(packages, run=None):
    cmd = ["go", "test", "-count=1"]
    if run is not None:
        cmd += ["-run", run]
    proc = subprocess.run(cmd + packages, cwd=REPO, capture_output=True, text=True)
    out = proc.stdout + proc.stderr
    if ("build failed" in out or "[build failed]" in out
            or "cannot find" in out or "syntax error" in out):
        return "compile error", out
    if proc.returncode == 0:
        return "UNNOTICED", out
    return "CAUGHT", out


def _apply_case(targets, original, case):
    """Return {target: new text} for one case, or exit with the engine's refusals.

    Extracted from score() so the cross-table below applies cases through the
    SAME needle rules rather than restating them. Two files that each decide what
    a missing needle means is the fork this engine was landed to end.
    """
    text = dict(original)
    for find, replace in case.edits:
        holders = [t for t in targets if find in text[t]]
        if not holders:
            sys.exit("ABORT [%s]: needle not found in %s.\n"
                     "A case whose needle is missing changes nothing and scores "
                     "UNNOTICED, which reads as a coverage hole that is not there.\n"
                     "Needle was:\n%s"
                     % (case.name, ", ".join(t.name for t in targets), find))
        if len(holders) > 1:
            sys.exit("ABORT [%s]: needle appears in %s.\n"
                     "The engine would sabotage whichever it looked at first, and the "
                     "row would name a file nobody chose. Lengthen the needle until it "
                     "picks one.\nNeedle was:\n%s"
                     % (case.name, " and ".join(t.name for t in holders), find))
        t = holders[0]
        # The same hazard one level down, and it was unguarded until the 218th
        # pass met it. `holders` counts FILES, so a needle appearing twice in
        # ONE file passed both checks above and `.replace(..., 1)` silently
        # sabotaged the first occurrence. In bundle-store,
        # `if n, _ := res.RowsAffected(); n == 0 {` appears in both SetEnabled
        # and DeleteBundle: a case aimed at the first mutated the second and
        # scored UNNOTICED — a true verdict about a function nobody chose, which
        # reads as a coverage hole in the function they did choose. That is the
        # same false result the "needle must be found" rule exists to prevent.
        occurrences = text[t].count(find)
        if occurrences > 1:
            sys.exit("ABORT [%s]: needle appears %d times in %s.\n"
                     "The engine would sabotage the first occurrence, and the row would name a "
                     "mechanism nobody chose — scoring the wrong function while reading as a "
                     "verdict on the right one. Lengthen the needle until it picks one.\n"
                     "Needle was:\n%s"
                     % (case.name, occurrences, t.name, find))
        text[t] = text[t].replace(find, replace, 1)
    if text == original:
        sys.exit("ABORT [%s]: the edit produced an identical file." % case.name)
    return text


def _discover_tests(packages):
    """[(package, test name)] for every top-level Test func the packages compile.

    Asked of the toolchain rather than grepped for: a test behind a build tag, or
    in a _test package the grep's glob missed, is a test the cross-table would
    silently never account for — and an unlisted test looks exactly like a test
    no case reddens, which is the verdict this whole mode exists to produce.
    """
    found = []
    for pkg in packages:
        proc = subprocess.run(["go", "test", "-list", ".*", pkg],
                              cwd=REPO, capture_output=True, text=True)
        if proc.returncode != 0:
            sys.exit("REFUSING: cannot list tests in %s\n%s"
                     % (pkg, proc.stdout + proc.stderr))
        for line in proc.stdout.splitlines():
            line = line.strip()
            if line.startswith("Test"):
                found.append((pkg, line))
    return found


def packages_no_edit_to_a_target_can_redden(target_packages, package_dependencies):
    """The PACKAGES entries whose tests no edit to a TARGET can ever redden.

    target_packages       import paths of the packages the TARGET files live in.
    package_dependencies  PACKAGES entry -> every import path that entry's
                          packages and their test binaries compile against,
                          themselves included.

    An entry is reachable when anything it covers is a target package: the edit
    lands either in that package's own code or in code it compiles against.
    An entry that covers no target package cannot go red for ANY edit to a
    TARGET, so every test in it is silent by construction.

    That distinction is the whole point. The cross-table's finding is "no case
    reddens this test", which reads as a gap in the case list — a mechanism the
    author pinned and nobody scored. A test the mutation cannot reach is not a
    gap in the case list at all, and reporting it as one names N findings for
    one fact and buries the real gaps among them. The 168th pass measured the
    ratio on the older kanban-curator lineage: ten tests in the silent column,
    nine of them this, one of them real.

    The common shape is a cmd/ TARGET listed beside a library the binary uses.
    A cmd/ package is `package main` and NOTHING in Go can import it, so the
    dependency runs from the binary to the library and never back — the
    library's tests are unreachable from the binary's code by construction, not
    by accident. Listing a library's callers beside a LIBRARY target is the
    sound version of the same idea and stays reachable: those callers do
    compile against the target.

    Pure, and separated from the toolchain query for that reason: it is checked
    in both directions by self_test(), which runs without a Go tree.
    """
    targets = set(target_packages)
    return sorted(entry for entry, covered in package_dependencies.items()
                  if not targets & set(covered))


def _package_reach(targets, packages):
    """(target package import paths, {PACKAGES entry -> import paths it covers}).

    The toolchain half of packages_no_edit_to_a_target_can_redden. `-deps`
    covers the named packages as well as what they import, and `-test` is what
    makes the answer sound: a package's test files can compile against imports
    the package itself does not have, and a reachability answer that missed
    those would call a reachable package unreachable and hide a real finding.
    """
    target_packages = set()
    for t in targets:
        rel = t.parent.relative_to(REPO)
        proc = subprocess.run(["go", "list", "-f", "{{.ImportPath}}", "./%s/" % rel],
                              cwd=REPO, capture_output=True, text=True)
        if proc.returncode != 0:
            sys.exit("REFUSING: cannot resolve the package holding target %s\n%s"
                     % (t, proc.stdout + proc.stderr))
        target_packages.update(proc.stdout.split())

    package_dependencies = {}
    for pkg in packages:
        proc = subprocess.run(["go", "list", "-deps", "-test", pkg],
                              cwd=REPO, capture_output=True, text=True)
        if proc.returncode != 0:
            sys.exit("REFUSING: cannot list the dependencies of %s\n%s"
                     % (pkg, proc.stdout + proc.stderr))
        package_dependencies[pkg] = proc.stdout.split()
    return target_packages, package_dependencies


@contextlib.contextmanager
def _sabotage_session(targets, packages):
    """Refuse on a dirty or already-red tree, then guarantee the target is put
    back — on the way out the engine chooses AND on the way out it does not.

    Yields (rels, restore, original).

    A target is a deliberately broken file from each write until the restore
    after the suite runs, so every way out of the loop has to restore. A killed
    run used to leave the broken file in the tree as ordinary-looking
    uncommitted work: measured on the 60th pass, a scoring run of cmd/scheduler
    killed at a wall-clock cap left `case <-d.signals:` rewritten to `default:`
    in a daemon's shutdown path, and the next run then refused to start because
    of the mess the last one made. The finally here is the only restore path;
    the signal handlers exist so a SIGINT/SIGTERM/SIGHUP reaches it instead of
    killing the process between the write and the restore. SIGKILL cannot be
    caught and is the one case no engine can cover.

    Every mode shares this — the 62nd pass's point was that a `finally` covers
    the signal you press by hand and misses the ones an unattended run receives,
    so a second mode with its own hand-rolled cleanup would reopen exactly that.

    It also takes the tree exclusively first (card `d869d2be`). A verdict here is
    the SUITE'S exit code, which belongs to the whole tree, so a second run
    mutating the same files hands this one a red suite it did not cause and this
    one scores it CAUGHT: a collision inflates the score. The hold refuses rather
    than waits, and exits non-zero so a refusal cannot read as a measurement.
    It sits here and not in score() because two of this repo's three scorers
    call run_cases() directly, and it is not re-entrant, so it must be taken at
    exactly one level. The `git status` check below cannot stand in for it: the
    other run restores each file between cases, so the tree looks clean exactly
    when trusting it is most dangerous.
    """
    with tree_hold.exclusive_hold_on_tree(
            REPO, purpose=os.path.basename(sys.argv[0] or "sabotage")) as refusal:
        if refusal:
            sys.exit("REFUSING: " + refusal)
        with _sabotage_session_on_a_held_tree(targets, packages) as session:
            yield session


@contextlib.contextmanager
def _sabotage_session_on_a_held_tree(targets, packages):
    """The session itself, on a tree this process already holds. Enter
    _sabotage_session instead: on its own this will mutate a tree another run
    is mutating, which is what the hold exists to stop."""
    rels = [str(t.relative_to(REPO)) for t in targets]

    dirty = subprocess.run(["git", "status", "--porcelain"] + rels,
                           cwd=REPO, capture_output=True, text=True).stdout.strip()
    if dirty:
        sys.exit("REFUSING: these have uncommitted changes; this harness restores "
                 "from git and would delete them:\n%s" % dirty)

    def restore():
        subprocess.run(["git", "checkout", "--"] + rels, cwd=REPO, check=True)

    baseline, out = _run_tests(packages)
    if baseline != "UNNOTICED":
        sys.exit("REFUSING: the suite is not green before sabotage (%s)\n%s" % (baseline, out))
    print("baseline: green\n")

    original = {t: t.read_text() for t in targets}
    previous_handlers = {}

    def restore_and_reraise(signum, frame):
        restore()
        signal.signal(signum, previous_handlers[signum])
        os.kill(os.getpid(), signum)

    for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        previous_handlers[sig] = signal.signal(sig, restore_and_reraise)

    try:
        yield rels, restore, original
    finally:
        restore()
        for sig, handler in previous_handlers.items():
            signal.signal(sig, handler)


def crosstable(targets, packages, cases, unreddened=None):
    """Run every case against every test individually and report the tests that
    NO case reddens. Reached with --crosstable through any scorer's score() call.

    The 59th and 64th passes read this table one way: a test nothing reddens may
    pin nothing, so look for tests to DELETE. Read the same table the other way
    and it finds cases to ADD. A test the author wrote deliberately, that no case
    reaches, is evidence the author understood a failure direction the case list
    does not express — and a case list inherits the worry of the card it was
    drafted from, so that gap is the normal condition, not an anomaly.

    Each test runs in its OWN `go test -run` invocation (the 71st): a mutation
    that panics kills the test binary, and every test that had not run yet then
    reports as passing. Sharing one invocation would attribute that whole tail to
    "reddened by nothing", which is the exact verdict this mode produces.

    A case whose package stays green cannot have reddened any test, so its row is
    empty without running the tests one by one. That shortcut is only sound
    because a panic FAILS the package — a green package really does mean every
    test passed.

    `unreddened` maps a test name to why it is expected in that column: a
    degenerate-input or reach guard no single mutation can isolate. It is checked
    in BOTH directions. An undeclared test in the column is the finding. A
    declared test that some case now reddens is a stale declaration, and the 47th
    pass filed a card because nothing on this box ever contradicts one of those.

    ⚠️ Guard classification is deliberately NOT applied here. This mode asks
    which tests a mutation reddens, and a reach guard firing is a red test by
    that question's own terms; whether the red counts as coverage is score()'s
    question, and answering it twice in two places is how the two halves drift.
    Read a cross-table row as "this test noticed", not as "this test pins it".

    The silent column marks each row with why it is there:

        ???  undeclared — the finding: a test the case list does not reach
        (b)  declared unreddened, with the reason printed under it
        (u)  in a package no edit to a TARGET can reach, so silent by
             construction. Not a finding about the case list, and reported once
             against the PACKAGES entry instead of once per test — see
             packages_no_edit_to_a_target_can_redden.
    """
    if isinstance(targets, Path):
        targets = [targets]
    unreddened = dict(unreddened or {})
    tests = _discover_tests(packages)

    target_packages, package_dependencies = _package_reach(targets, packages)
    out_of_reach = packages_no_edit_to_a_target_can_redden(target_packages, package_dependencies)
    unreachable_tests = {name for pkg, name in tests if pkg in out_of_reach}

    print("cross-table: %d cases x %d tests\n" % (len(cases), len(tests)))
    if out_of_reach:
        print("%d of those %d tests are in %d package(s) no edit to a TARGET can reach, "
              "and are reported below as structural rather than as findings:\n    %s\n"
              % (len(unreachable_tests), len(tests), len(out_of_reach), ", ".join(out_of_reach)))

    rows = {}          # case name -> sorted [test name]
    verdicts = {}      # case name -> package-level verdict
    unattributed = []  # cases red at package level that no single test reproduces

    with _sabotage_session(targets, packages) as (rels, restore, original):
        for case in cases:
            text = _apply_case(targets, original, case)
            for t in targets:
                if text[t] != original[t]:
                    t.write_text(text[t])
            verdict, _ = _run_tests(packages)
            hits = []
            if verdict == "CAUGHT":
                for pkg, name in tests:
                    per_test, _ = _run_tests([pkg], run="^%s$" % name)
                    if per_test == "CAUGHT":
                        hits.append(name)
            restore()
            verdicts[case.name] = verdict
            rows[case.name] = sorted(hits)
            if verdict == "CAUGHT" and not hits:
                unattributed.append(case.name)
            print("%-14s %-2d %s" % (verdict, len(hits), case.name))

    covered = {name for hits in rows.values() for name in hits}
    all_names = [name for _, name in tests]
    silent = [name for name in all_names if name not in covered]

    print("\n================ cross-table ================")
    for case in cases:
        print("\n%s  [%s]" % (case.name, verdicts[case.name]))
        for name in rows[case.name]:
            print("    red: %s" % name)

    print("\n================ reddened by NO case ================")
    crosstable_problems = []
    for name in silent:
        why = unreddened.get(name)
        if name in unreachable_tests:
            print("  %-6s %s" % ("(u)", name))
            continue
        print("  %-6s %s" % ("(b)" if why else "???", name))
        if why:
            print("         declared: %s" % why)
        else:
            crosstable_problems.append("undeclared in the silent column: %s" % name)
    if not silent:
        print("  none — every test is reddened by at least one case")

    # One line per unreachable PACKAGES entry, not one per test in it. The entry
    # is the thing that is wrong and the thing somebody can fix; the tests are
    # only how many times the same fact would otherwise be printed.
    for entry in out_of_reach:
        n = len([name for pkg, name in tests if pkg == entry])
        crosstable_problems.append(
            "%s is in PACKAGES but no edit to a TARGET can reach it, so all %d of its "
            "tests are silent by construction and none of them is a gap in the case "
            "list — drop the entry, or give it a TARGET" % (entry, n))

    for name, why in sorted(unreddened.items()):
        if name not in all_names:
            crosstable_problems.append("declared unreddened but no such test exists: %s" % name)
        elif name in covered:
            crosstable_problems.append("STALE declaration — a case now reddens %s, which is "
                            "declared unreddened (%s)" % (name, why))

    for name in unattributed:
        crosstable_problems.append("package went red but no single test reproduces it, so this "
                        "row credits nothing: %s" % name)

    reachable_silent = [name for name in silent if name not in unreachable_tests]
    print("\n%d of %d tests reddened by no case (%d of them unreachable by construction)"
          % (len(silent), len(all_names), len(silent) - len(reachable_silent)))
    for p in crosstable_problems:
        print("  ⚠️  " + p)
    if not crosstable_problems:
        print("  no undeclared silent test, and no declaration has gone stale")
    return 1 if crosstable_problems else 0


def problems(results):
    """Everything wrong with a scored run, as printable lines. Empty means sound.

    `results` is what run_cases() returns: (case, verdict, diff, detail) rows.

    This is the one definition of "wrong". print_score() prints it, and a caller
    that needs an exit status asks the same question rather than re-deriving it:
    a caller's own copy of these rules drifts silently, and always toward green,
    because the copy is the half nobody re-reads when a rule is added here.

    In particular it is NOT `caught < real`. A caller with those two numbers to
    hand will reach for that comparison, and it misses a known-negative control
    that WAS caught — the suite red for an unrelated reason, every CAUGHT above
    it worthless, and the run still scoring a perfect caught == real.

    The known-positive control is believed through counts_as_coverage(), not
    through `verdict == "CAUGHT"`. A control that only tripped a reach guard
    proves the fixture falls over, not that the suite is running, and that is
    the one row where reading red as running defeats every row below it.

    An `expected_unnoticed` is checked in BOTH directions, and until the 94th
    pass it was checked in one. A row that is UNNOTICED without a declaration is
    reported; a row that carries a declaration and is CAUGHT anyway used to be
    reported by nothing. The declaration is prose asserting that no test can see
    the mutation, so the moment someone writes the test that sees it the prose is
    false — and it is the one statement in a case file that can only ever rot,
    because nothing else in the run re-reads it. Measured on the 69th pass:
    cmd/autoworker carried seven declarations and two had been false for some
    time. The score arithmetic hides them, because the denominator counts every
    non-CONTROL case whether declared or not, so 51/53 reads the same either way.

    crosstable() has had the same check on its `unreddened` declarations since
    the 71st. This is the older path, and it is the half that did not learn.

    A CONTROL is exempt: a known-negative carries a declaration by construction,
    and when one is CAUGHT the control rule above already says so, more
    accurately — the suite is red for an unrelated reason, which is not the same
    finding as a declaration that has expired.
    """
    found = []
    for case, verdict, _, _ in results:
        is_control = case.name.startswith("CONTROL")
        if case.name.startswith("CONTROL known-positive") and not counts_as_coverage(verdict):
            found.append("the known-positive control was NOT caught — the suite is not running")
        if case.name.startswith("CONTROL known-negative") and verdict.startswith("CAUGHT"):
            found.append("the known-negative control WAS caught — the suite is red for a "
                         "reason unrelated to behavior, so every CAUGHT above is suspect")
        if verdict == "compile error" and not case.expected_unnoticed:
            found.append("compile error, not a score: %s" % case.name)
        if verdict == "UNNOTICED" and not case.expected_unnoticed:
            found.append("UNNOTICED: %s" % case.name)
        if counts_as_coverage(verdict) and case.expected_unnoticed and not is_control:
            found.append("STALE declaration — declared unnoticed and CAUGHT anyway, so the "
                         "claim has expired: %s (%s)" % (case.name, case.expected_unnoticed))
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


def run_cases(targets, packages, cases, guard_markers=()):
    """Apply each case to the target files, run packages, print the table, and
    return the (case, verdict, diff, detail) rows.

    Split out of score() so a case file that drives the engine once per target
    and adds the scores up — sabotage-truncation.py scores one mechanism spread
    over six files — can read the rows without either re-deriving the needle
    rules or forcing score() to hand back a report where a caller needs an exit
    status. The 73rd pass measured what that second choice costs: an engine
    returning a list under a `sys.exit(score(...))` caller exits 1 always,
    healthy or not, and a permanently red scorer reads as a defect in the binary
    rather than in the harness.

    `targets` is one Path or a list of them. A binary whose mechanisms live in
    more than one file needs more than one file sabotaged, and splitting that
    across two scorers splits the score with it — cmd/autoworker decides when to
    fire in main.go and how much the fire may cost in spend.go, and a score that
    covered only one of those would be a number for half a binary.

    With several targets a case still writes `(find, replace)` and says nothing
    about which file it means: the engine locates the needle. Finding it in TWO
    targets is an abort, not a choice — the case would silently score whichever
    file the engine happened to look at first.

    Pass --diffs to print the edit each case actually applied. A row prints the
    name you gave it, not the edit you made, and mislabeled cases have twice
    been read as coverage holes that were not there — so read the diffs at
    least once per case family.

    Pass --messages to print every line that went red, not just the first.

    `guard_markers` are substrings of this suite's fixture-guard messages. See
    classify_caught(): supplying them turns "which failure fired" from something
    the reader works out into something the next run enforces.
    """
    show_diffs = "--diffs" in sys.argv
    show_messages = "--messages" in sys.argv
    if isinstance(targets, Path):
        targets = [targets]
    results = []

    with _sabotage_session(targets, packages) as (rels, restore, original):
        for case in cases:
            text = _apply_case(targets, original, case)
            for t in targets:
                if text[t] != original[t]:
                    t.write_text(text[t])
            verdict, out = _run_tests(packages)
            detail = ""
            fired = []
            if verdict == "CAUGHT":
                verdict, detail = classify_caught(out, guard_markers)
                fired = failure_messages(out)
            # Read the diff the harness actually applied, not the label it was given.
            diff_cmd = ["git", "diff"] + ([] if show_diffs else ["--stat"]) + ["--"] + rels
            diff = subprocess.run(diff_cmd, cwd=REPO,
                                  capture_output=True, text=True).stdout.strip()
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
    return results


def print_score(results):
    """Print the score block for one table and return problems(results).

    Returns the problem lines rather than a status so a caller scoring several
    tables can add them up; the caller ends with `1 if found else 0`.
    """
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
        print("  both controls behaved; every real mechanism is pinned by an assertion")
    return found


def score(targets, packages, cases, *, guard_markers=(), unreddened=None):
    """Score one table and return a process exit status: 0 when both controls
    behaved and every real mechanism was caught by an assertion, 1 otherwise.
    Callers end with sys.exit(score(...)).

    See run_cases() for what `targets`, `guard_markers`, --diffs and --messages
    mean, and crosstable() for `unreddened` and --crosstable.

    ⚠️ `guard_markers` and `unreddened` are KEYWORD-ONLY, and that is a repair
    rather than a style. Two forks of this engine each put a different parameter
    in positional slot 4 and two case files on this box already fill that slot,
    with different things — see the fork section in the module docstring. Bind
    the wrong one and nothing raises: empty guard_markers just makes
    classify_caught blind, and every guard-caught row reads as coverage again.

    ⚠️ The exit status is the whole point of the return value and it was once
    lost. The 338ec17 extraction returned the results list instead of a status.
    Measured on the 54th nightly pass: a scorer whose known-positive control
    went UNNOTICED printed "the suite is not running" and still exited 0, so
    nothing running this from a script or a guard could tell a clean sweep from
    a broken one. Return a status, not a report.

    Re-measured on the 73rd, and the mechanism is worth stating exactly, because
    the engine's return type alone does not decide it — the CALLER does:

      - `score(...)` bare, no sys.exit  -> exits 0 always. This is what the 54th
        found, and it is still live on test/first-tests-for-ask.
      - `sys.exit(score(...))` with this engine returning a list -> Python
        prints the repr and exits 1 ALWAYS, healthy or not. A permanently red
        scorer is as useless as a permanently green one and looks like a defect
        in the binary rather than in the harness.

    So a scorer is only trustworthy when a status-returning engine and a
    sys.exit caller are BOTH present. Verify in both directions: healthy suite
    -> 0, un-catchable known-positive control -> 1.
    """
    if "--crosstable" in sys.argv:
        return crosstable(targets, packages, cases, unreddened)

    results = run_cases(targets, packages, cases, guard_markers=guard_markers)
    return 1 if print_score(results) else 0


def self_test():
    """Drive the engine's pure rules against every verdict and refusal they own.

    The 43rd's rule: when you automate a check, the check is the next unmeasured
    claim. A classify_caught() that can never return "guard" prints the clean
    score you were hoping for, and the 48th shipped exactly that — a scorer
    whose unreachable branch read as a perfect run. No case table can score its
    own scorer, so these are driven from synthetic output instead.

    Covers both halves of the 240th's union: the guard classification that only
    tool-store main carried, and the needle-once, stale-declaration and
    reachability rules that only the canonical blob carried. A union tested on
    one half is a union whose other half nobody re-ran.
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
        # it names the behavior, and both mean the mechanism is pinned.
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

    def row(case, verdict):
        return [(case, verdict, "", "")]

    # problems() must flag a guard-caught row and must fall silent once the case
    # file declares it. Both directions, because a rule that never fires and a
    # rule that always fires are equally useless and look the same from green.
    if not problems(row(Case("the budget drifts"), "CAUGHT (guard)")):
        print("SELF-TEST FAIL: an undeclared guard-caught row was not flagged")
        ok = False
    if problems(row(Case("the budget drifts", expected_guard_caught="known"), "CAUGHT (guard)")):
        print("SELF-TEST FAIL: a declared guard-caught row was flagged anyway")
        ok = False

    # The stale-declaration rule, both directions. This is the half the guard
    # fork never had, and a union that only re-ran the guard probes above would
    # not have noticed it missing.
    if problems(row(Case("a drift nothing sees", expected_unnoticed="no test reaches it"),
                    "UNNOTICED")):
        print("SELF-TEST FAIL: a declared UNNOTICED row was flagged")
        ok = False
    if not problems(row(Case("a drift nothing sees", expected_unnoticed="no test reaches it"),
                        "CAUGHT")):
        print("SELF-TEST FAIL: an expired expected_unnoticed declaration was not flagged")
        ok = False
    if problems(row(Case("CONTROL known-negative: a comment moves",
                         expected_unnoticed="behavior is unchanged"), "UNNOTICED")):
        print("SELF-TEST FAIL: a well-behaved known-negative control was flagged")
        ok = False

    # The known-positive control is believed through counts_as_coverage, so a
    # control that only tripped a guard must still read as "the suite is not
    # running". Scoring that row as running is what makes every row below it a
    # number nobody can use.
    #
    # ⚠️ The declaration is what makes this a probe of that rule and not of its
    # neighbor. Without `expected_guard_caught` the guard-caught rule flags the
    # same row, so the probe passes whether or not the known-positive rule reads
    # counts_as_coverage — measured while writing it: the obvious undeclared
    # version stayed green under a mutation that put `verdict == "CAUGHT"` back.
    # A dominated probe is not a test of the rule it is named for.
    believed = Case("CONTROL known-positive: the answer is wrong",
                    expected_guard_caught="declared here only to isolate the rule under test")
    if not problems(row(believed, "CAUGHT (guard)")):
        print("SELF-TEST FAIL: a known-positive control that only tripped a guard was believed")
        ok = False
    if problems(row(Case("CONTROL known-positive: the answer is wrong"),
                    "CAUGHT (panic in tool.go)")):
        print("SELF-TEST FAIL: a known-positive control caught by a production panic was flagged")
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

    # _apply_case's three refusals, each driven to the exit it owns. The
    # needle-once rule is the one the guard fork lacked entirely: without it a
    # needle appearing twice in one file scores a function nobody chose.
    one, two = Path("one.go"), Path("two.go")

    def apply(text_by_target, edits):
        """(result, exit message). Exactly one of the two is None."""
        targets = list(text_by_target)
        try:
            return _apply_case(targets, dict(text_by_target), Case("probe", edits)), None
        except SystemExit as stop:
            return None, str(stop)

    got, stop = apply({one: "alpha beta\n"}, [("alpha", "gamma")])
    if got != {one: "gamma beta\n"}:
        print("SELF-TEST FAIL: a sound single edit did not apply: %r / %r" % (got, stop))
        ok = False
    _, stop = apply({one: "alpha\n"}, [("delta", "gamma")])
    if not stop or "needle not found" not in stop:
        print("SELF-TEST FAIL: a missing needle was not refused: %r" % stop)
        ok = False
    _, stop = apply({one: "alpha\n", two: "alpha\n"}, [("alpha", "gamma")])
    if not stop or "needle appears in" not in stop:
        print("SELF-TEST FAIL: a needle in two targets was not refused: %r" % stop)
        ok = False
    _, stop = apply({one: "alpha alpha\n"}, [("alpha", "gamma")])
    if not stop or "needle appears 2 times" not in stop:
        print("SELF-TEST FAIL: a needle twice in one file was not refused: %r" % stop)
        ok = False
    _, stop = apply({one: "alpha\n"}, [("alpha", "alpha")])
    if not stop or "identical file" not in stop:
        print("SELF-TEST FAIL: an edit that changed nothing was not refused: %r" % stop)
        ok = False

    # packages_no_edit_to_a_target_can_redden, both directions. An entry that
    # covers a target package is reachable; one that covers none is silent by
    # construction and must not be reported as a gap in the case list.
    reach = packages_no_edit_to_a_target_can_redden(
        ["repo/tools"],
        {"./tools": ["repo/tools", "fmt"], "./schema": ["repo/schema", "fmt"]})
    if reach != ["./schema"]:
        print("SELF-TEST FAIL: reachability -> %r, want ['./schema']" % reach)
        ok = False
    if packages_no_edit_to_a_target_can_redden(["repo/tools"], {"./tools": ["repo/tools"]}):
        print("SELF-TEST FAIL: a package holding the target was called unreachable")
        ok = False

    # score()'s keyword-only barrier. A fourth positional argument is the one
    # mistake this union can be handed, because two forks trained two case files
    # to pass one — and the failure it would otherwise produce is silent.
    #
    # Asked of the SIGNATURE, not by calling score(). Calling it does raise
    # TypeError while the barrier holds, but with the barrier removed the call
    # runs the body and dies in the fixture instead — a probe that goes red
    # either way and proves nothing (the 175th). bind() reproduces exactly the
    # error the real caller would get and executes nothing.
    signature = inspect.signature(score)
    try:
        signature.bind(Path("x.go"), ["./tools"], [], ("a marker",))
    except TypeError:
        pass
    else:
        print("SELF-TEST FAIL: score() accepted a fourth positional argument")
        ok = False
    if signature.bind(Path("x.go"), ["./tools"], [], guard_markers=("a marker",)) is None:
        print("SELF-TEST FAIL: score() rejected guard_markers by keyword")
        ok = False
    for name in ("guard_markers", "unreddened"):
        if signature.parameters[name].kind is not inspect.Parameter.KEYWORD_ONLY:
            print("SELF-TEST FAIL: score()'s %s is not keyword-only" % name)
            ok = False

    print("self-test: %s" % ("all verdicts reachable and separated" if ok else "FAILED"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(self_test())
