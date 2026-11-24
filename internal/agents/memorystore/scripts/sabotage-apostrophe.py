#!/usr/bin/env python3
"""Score the apostrophe tests in memory-store by injecting the defects they
exist for.

Verdict column, same inverted reading as the inber-party scorer:

    SOLE DETECTOR  only the new tests failed
    REDUNDANT      a pre-existing test also failed; the sibling is NAMED
    UNNOTICED      nothing failed
    VOID           the tree did not build, so no test ran

A REDUNDANT row must name its sibling. An unnamed one is indistinguishable
from the build-failure bug the forty-seventh pass hit, where two rows read
"a sibling covers this" and in fact no test had run at all.

A SOLE DETECTOR row also says whether an ASSERTION fired or only a fixture
REACH GUARD. Without that split this table cannot answer the question card
`44411ad9` asks — it matched `--- FAIL: <TestName>` and never read the message,
so a new test reddened by its own `t.Fatalf("LoadToolRegistry failed: ...")`
scored identically to one reddened by an assertion about apostrophes. The tests
this file scores really do carry such guards, so the ambiguity was live rather
than theoretical.

The split is the forty-second pass's, and it was already written -- in
`inber-party`'s copy of this same scorer, on a branch of the same name, for the
same fix. It was never propagated here. A fix to an instrument propagates no
better than a fix to a product, and worse, because nothing compiles an
instrument.
"""

import os
import re
import signal
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HELPER = "internal/textutil/capitalise.go"
REGISTRY = "tool_registry.go"

# Messages these tests emit from a t.Fatalf that only guards reachability -- the
# input never got to the code under test, or the fixture itself is wrong. A row
# reddened ONLY by these has scored nothing about apostrophes.
#
# The fourth entry is the judgment call and it is deliberate. "no divergence at
# all" asserts that the CORPUS exercised the fix, not that the function is
# correct, so a row it alone catches has been told only that some divergence
# exists. Its sibling on line 30 ("diverged with no flanked apostrophe") is a
# real differential assertion and is NOT listed here.
REACH_GUARDS = [
    "LoadToolRegistry failed:",
    "Get(tool-registry) failed:",
    "belongs in the other test",
    "the corpus never exercised the fix",
]

NEW_TESTS = {
    "TestToolRegistryHeadingKeepsAnApostropheInsideAWord",
    "TestToolRegistryHeadingStillBreaksOnAHyphen",
    "TestApostrophesDoNotBreakWords",
    "TestEverythingWithoutAnApostropheMatchesStringsTitle",
    "TestAnUnflankedApostropheStillSeparates",
    "TestInvalidUTF8IsCopiedNotReplaced",
    "TestWordStartIsTitleCaseNotUpperCase",
    "TestDifferentialAgainstStringsTitle",
}

MUTATIONS = [
    ("tool_registry heading reverts to strings.Title",
     REGISTRY,
     # Reverting the call site outright orphans the textutil import, which is a
     # compile error -- and unlike an orphaned variable, '&& false' cannot keep
     # an import live. Composing the two reintroduces the identical defect
     # ("agent's memory" -> "Agent'S Memory") with both symbols still referenced.
     "textutil.TitleFirstRuneOfEachWord(category)",
     "strings.Title(textutil.TitleFirstRuneOfEachWord(category))",
     "caught"),

    ("helper degraded to a first-rune-only capitaliser (the tempting simpler one)",
     HELPER,
     "\t\t\tatWordStart = !isWordRune(r)",
     "\t\t\tatWordStart = !isWordRune(r) && false",
     "caught"),

    ("apostrophe is a separator again (the original defect)",
     HELPER,
     "\t\tif r == '\\'' {",
     "\t\tif r == '\\'' && false {",
     "caught"),

    ("KNOWN POSITIVE: helper returns its input unchanged",
     HELPER,
     "\tvar b strings.Builder",
     "\tif true {\n\t\treturn s\n\t}\n\tvar b strings.Builder",
     "caught"),

    ("KNOWN NEGATIVE: drop the Grow pre-allocation (behavior-neutral)",
     HELPER,
     "\tb.Grow(len(s))",
     "",
     "unnoticed"),
]


def run(cmd):
    return subprocess.run(cmd, cwd=REPO, shell=True, capture_output=True, text=True)


def restore():
    run("git checkout -- .")


def refuse_a_dirty_tree():
    """Stop before the first restore() if it would delete uncommitted work.

    restore() is `git checkout -- .`, and main() calls it BEFORE the first case,
    so scoring a fix that is written but not yet committed reverts the fix and
    then scores HEAD. The 239th nightly pass measured that here: eight rows read
    `SETUP FAIL: pattern not found` and the ninth read `ok`, and nothing in the
    output mentioned the checkout -- so the symptom accuses the case list.

    ⚠️ The check is the whole repository, not a target list, because
    `git checkout -- .` is the whole repository. A per-file check would clear a
    tree this still destroys.

    Untracked files are excluded: `git checkout` cannot touch them, so refusing
    over one is a cry-wolf, and a guard that fires on damage it did not do
    teaches the next reader to skip it.
    """
    dirty = run("git status --porcelain --untracked-files=no").stdout.strip()
    if dirty:
        sys.exit("REFUSING TO RUN: uncommitted changes, and this script restores "
                 "the whole tree from git:\n" + dirty)


def guard_only(output):
    """True when every failure message in a red run came from a reach guard.

    Split out of classify() so it can be exercised directly. classify() shells
    out to `go test`, so a self-test cannot reach this decision through it, and
    an unexercised branch that reports "assertion-fired" for everything is
    indistinguishable from a suite that really is well asserted.
    """
    fail_lines = [l for l in output.splitlines() if re.search(r"_test\.go:\d+:", l)]
    return bool(fail_lines) and all(
        any(g in l for g in REACH_GUARDS) for l in fail_lines)


def self_test():
    """Prove guard_only() can return BOTH answers before either is believed.

    The 101st pass's rule: prove your instrument can say "yes" before you trust
    it saying "no". This scorer's whole new column is a claim that no row is
    guard-fired, and that claim is worth nothing from a predicate that cannot
    say otherwise.
    """
    guard = "    tool_registry_apostrophe_test.go:20: LoadToolRegistry failed: no such file\n"
    assertion = ('    capitalise_test.go:24: TitleFirstRuneOfEachWord("don\'t") = '
                 '"Don\'T", want "Don\'t"\n')
    cases = [
        ("guard alone", guard, True),
        ("assertion alone", assertion, False),
        ("both -- an assertion anywhere means the row scored something", guard + assertion, False),
        ("no failure lines at all", "ok  \tgithub.com/kayushkin/memory-store\n", False),
    ]
    bad = [name for name, output, want in cases if guard_only(output) != want]
    if bad:
        sys.exit("guard_only() SELF-TEST FAILED: " + ", ".join(bad))
    print("  guard_only() self-test: both verdicts reachable")


def classify():
    build = run("go build ./... && go vet ./...")
    if build.returncode != 0:
        return "VOID", "did not build"
    test = run("go test -count=1 ./...")
    if test.returncode == 0:
        return "UNNOTICED", ""
    out = test.stdout + test.stderr
    if "[build failed]" in out:
        return "VOID", "test binary did not build"
    failing = set(re.findall(r"--- FAIL: (\w+)", out))
    if not failing:
        return "VOID", "non-zero exit with no FAIL line"
    siblings = failing - NEW_TESTS
    if siblings:
        return "REDUNDANT", "sibling: " + ", ".join(sorted(siblings))
    fired = "guard-fired only -- NOT coverage" if guard_only(out) else "assertion-fired"
    return "SOLE DETECTOR", "%s: %s" % (fired, ", ".join(sorted(failing)))


def main():
    refuse_a_dirty_tree()
    self_test()
    restore()
    if run("go test -count=1 ./...").returncode != 0:
        print("BASELINE NOT GREEN -- every row would be meaningless")
        return 1
    print("baseline green\n")

    rows = []
    # The mutated file holds a deliberately broken version of itself from the write
    # in the loop below until the restore that follows it, so every way out of that
    # window has to restore — including the ways this script does not choose to
    # take. A killed run left the mutated file behind as ordinary-looking
    # uncommitted work: a semantic edit to a tracked source file, which
    # `git status` reports the same way it reports real work in progress, and which
    # this box's standing rule tells the next agent not to throw away.
    #
    # A try/finally alone does NOT close this, and measuring it is how you find that
    # out. Python raises KeyboardInterrupt for SIGINT, so a finally is on the way
    # out for that one and for nothing else. SIGTERM and SIGHUP kill the process
    # between the write and the restore — and those are exactly what a wall-clock
    # cap, systemd and a process-group kill send. So the one signal a finally covers
    # is the one you press by hand while watching, and the ones it misses are the
    # ones an unattended run actually receives. Measured by kill on this scorer
    # before these handlers existed: SIGTERM left the mutated file behind.
    #
    # The handler restores, reinstates the disposition it replaced and re-raises, so
    # the process dies BY the signal (rc 128+signum). A handler that restores and
    # exits 0 tells every caller a killed run succeeded.
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
        for label, path, old, new, expectation in MUTATIONS:
            full = os.path.join(REPO, path)
            src = open(full).read()
            if src.count(old) != 1:
                restore()
                rows.append((label, "VOID", f"site appears {src.count(old)}x, not once", expectation))
                continue
            open(full, "w").write(src.replace(old, new, 1))
            verdict, detail = classify()
            restore()
            rows.append((label, verdict, detail, expectation))
    finally:
        restore()
        for sig, handler in previous_handlers.items():
            signal.signal(sig, handler)

    print(f"{'MUTATION':<72} | {'VERDICT':<14} | DETAIL")
    print("-" * 140)
    bad = 0
    for label, verdict, detail, expectation in rows:
        ok = (expectation == "caught" and verdict in ("SOLE DETECTOR", "REDUNDANT")) or (
            expectation == "unnoticed" and verdict == "UNNOTICED")
        bad += 0 if ok else 1
        print(f"{('* ' if not ok else '  ') + label:<72} | {verdict:<14} | {detail[:52]}")

    helper = open(os.path.join(REPO, HELPER)).read()
    print(f"\nfix still present after the run: {'unicode.ToTitle(r)' in helper}")
    print(f"tree clean after the run: {not run('git status --porcelain --untracked-files=no').stdout.strip()}")
    print(f"rows disagreeing with expectation: {bad}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
