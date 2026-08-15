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

The engine was extracted from scripts/sabotage-kanban-curator.py TWICE, on two
branches, by two passes that could not see each other: 2ab59cc on
test/first-tests-for-kanban-dispatcher and 338ec17 on test/first-tests-for-ask.
Ten unattended passes had each rebuilt this logic from prose and hit the same
traps doing it; the two extractions were the eleventh and twelfth. A per-binary
file supplies TARGETS, PACKAGES and CASES and calls score() — nothing else
needs restating.

This file is the union of both, landed by the 73rd pass. It is meant to be the
SAME BLOB on every branch that carries a scorer; if you find two, they have
forked again and the section below says what that costs.
"""

import contextlib
import os
import signal
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent


@dataclass
class Case:
    name: str
    # (find, replace) pairs. Every find must appear in exactly one target file.
    edits: list = field(default_factory=list)
    # A case the suite is NOT expected to catch, and why.
    expected_unnoticed: str = ""


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
    in both directions by scripts/sabotage-selftest.py, which runs without a Go
    tree.
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
    """
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

    This is the one definition of "wrong". score() prints it, and a caller that
    needs an exit status asks the same question rather than re-deriving it: a
    caller's own copy of these rules drifts silently, and always toward green,
    because the copy is the half nobody re-reads when a rule is added here.

    In particular it is NOT `caught < real`. A caller with those two numbers to
    hand will reach for that comparison, and it misses a known-negative control
    that WAS caught — the suite red for an unrelated reason, every CAUGHT above
    it worthless, and the run still scoring a perfect caught == real.

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
    for case, verdict, _ in results:
        is_control = case.name.startswith("CONTROL")
        if case.name.startswith("CONTROL known-positive") and verdict != "CAUGHT":
            found.append("the known-positive control was NOT caught — the suite is not running")
        if case.name.startswith("CONTROL known-negative") and verdict == "CAUGHT":
            found.append("the known-negative control WAS caught — the suite is red for a "
                         "reason unrelated to behaviour, so every CAUGHT above is suspect")
        if verdict == "compile error" and not case.expected_unnoticed:
            found.append("compile error, not a score: %s" % case.name)
        if verdict == "UNNOTICED" and not case.expected_unnoticed:
            found.append("UNNOTICED: %s" % case.name)
        if verdict == "CAUGHT" and case.expected_unnoticed and not is_control:
            found.append("STALE declaration — declared unnoticed and CAUGHT anyway, so the "
                         "claim has expired: %s (%s)" % (case.name, case.expected_unnoticed))
    return found


def score(targets, packages, cases, unreddened=None):
    """Apply each case to the target files, run packages, print a scored table,
    and return a process exit status: 0 when both controls behaved and every
    real mechanism was caught, 1 otherwise. Callers end with sys.exit(score(...)).

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
    name you gave it, not the edit you made, and mislabelled cases have twice
    been read as coverage holes that were not there — so read the diffs at
    least once per case family.

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

    show_diffs = "--diffs" in sys.argv
    if isinstance(targets, Path):
        targets = [targets]
    results = []

    with _sabotage_session(targets, packages) as (rels, restore, original):
        for case in cases:
            text = _apply_case(targets, original, case)
            for t in targets:
                if text[t] != original[t]:
                    t.write_text(text[t])
            verdict, _ = _run_tests(packages)
            # Read the diff the harness actually applied, not the label it was given.
            diff_cmd = ["git", "diff"] + ([] if show_diffs else ["--stat"]) + ["--"] + rels
            diff = subprocess.run(diff_cmd, cwd=REPO,
                                  capture_output=True, text=True).stdout.strip()
            restore()
            results.append((case, verdict, diff))
            print("%-14s %s" % (verdict, case.name))
            if show_diffs:
                body = "\n".join(l for l in diff.split("\n")
                                 if l.startswith(("+", "-")) and not l.startswith(("+++", "---")))
                print("\n".join("        " + l for l in body.split("\n")) + "\n")

    print("\n================ score ================")
    caught = sum(1 for c, v, _ in results
                 if v == "CAUGHT" and not c.name.startswith("CONTROL"))
    real = [c for c, _, _ in results if not c.name.startswith("CONTROL")]
    print("%d/%d real mechanisms caught" % (caught, len(real)))

    found = problems(results)
    for p in found:
        print("  ⚠️  " + p)
    if not found:
        print("  both controls behaved; every real mechanism is pinned")
    return 1 if found else 0
