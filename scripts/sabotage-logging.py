#!/usr/bin/env python3
"""Score internal/logging and internal/runstatus by breaking them on purpose.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism the
suite is meant to pin.

    python3 scripts/sabotage-logging.py [--diffs] [--crosstable]
    python3 scripts/sabotage-logging.py --guard-split
    python3 scripts/sabotage-logging.py --self-test-only
    python3 scripts/sabotage-logging.py --prove-self-test

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

Score under --guard-split, taken 2026-08-21 by the 296th nightly pass:
**14 CAUGHT rows, 0 caught by a reach-guard only, 1 caught by the Go test
runtime, 0 unreadable**, and the known-negative control still UNNOTICED. The
default table's 13/13 is unchanged; 14 against 13 because the split counts the
known-positive control as a CAUGHT row, the same arithmetic as the 269th and
281st passes.

Three things that zero is worth reading with, because a zero is also what a
classifier that can never fire would print.

**It is a measurement.** ~/.nightly-296/prove_the_mode.sh removes the one test
that makes the zero true and re-runs the known-positive control: the row flips
to guard-only on nine real guard lines. The 269th pass ended saying a self-test
proves the FUNCTION and only a probe proves the MODE, and that its probe was not
committed; this one is.

**It rests on a single test, and not the obvious one.** Every test in
logging_test.go reaches its assertion through `recordingPublisher.only`, which
is fatal, except TestTheFinishEntryGoesToTheSchedulerLogSubject — it reads the
recorder's subjects directly. So every mutation that stops the logger publishing
reddens that one test on an assertion while tripping the guard in all the
others. It is a test about the SUBJECT holding up the readability of rows about
the LEVEL, which is the 254th pass's finding seen from the other side: an
assertion behind a fatal guard is not reached, and here one assertion that is
not behind the guard is carrying eight rows that are.

**The third verdict is not exotic.** `new-drops-the-typed-nil-guard` is caught
by TestALoggerWithNoBusPublishesNothing, whose property is that a call does not
panic and which therefore contains no assertion at all. The 281st pass reached
this verdict class through `os.Exit` under a `-h` flag and called it a row its
suite happened to produce. Assert-no-panic is an ordinary Go idiom, so any suite
carrying one has a row a two-way split can only call unreadable.
"""

import ast
import re
import sys
from collections import namedtuple
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from sabotage import (  # noqa: E402
    REPO, Case, _apply_case, _run_tests, _sabotage_session, score,
)

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
        # `if false {` orphans the runstatus import and REFUSES to compile — a
        # scorer defect, not a suite miss (the 137th pass's build guard, tenth
        # consecutive night it has separated the two). Asking the predicate about
        # a constant instead keeps the import referenced and mutates the thing
        # that matters: the finish logger no longer consults the run's status.
        name="finish-never-escalates: the level decision ignores the run's status",
        edits=[("if runstatus.TerminalStatusIsFailure(status) {",
                "if runstatus.TerminalStatusIsFailure(runstatus.Success) {")],
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
    # The cut moved into truncateAtRuneBoundary at 60f4831, and the 500 became
    # outputSnippetBudget. The two rows below aimed at the inline slice until
    # 2026-09-26 and aborted on a needle that no longer existed.
    Case(
        name="truncation-boundary-moves: the cut lands at 600 instead of 500",
        edits=[("const outputSnippetBudget = 500", "const outputSnippetBudget = 600")],
    ),
    Case(
        name="truncation-keeps-the-wrong-end-of-the-output",
        edits=[("\treturn s[:cut]\n", "\treturn s[len(s)-cut:]\n")],
    ),
    Case(
        name="truncation-cuts-inside-a-rune: the walk back to a rune start is dead",
        edits=[("for cut > 0 && !utf8.RuneStart(s[cut]) {", "for false && !utf8.RuneStart(s[cut]) {")],
    ),
    Case(
        name="new-drops-the-typed-nil-guard",
        edits=[("if natsClient == nil {", "if false {")],
    ),
    Case(
        # Added after the cross-table reported TestTheVocabularyHasNoDuplicateSpellings
        # as reddened by no case. A test nothing reddens may be pinning nothing;
        # here it was pinning a real direction the case list had not expressed —
        # a second constant quietly taking an existing spelling, which is how the
        # vocabulary forked in the first place.
        name="vocabulary-forks: two constants share one spelling",
        edits=[('Timeout = "timeout"', 'Timeout = "error"')],
    ),
]


# ─────────────────────────────────────────────────────────────────────────────
# The guard/assertion split — card `3b6eba2a`
# ─────────────────────────────────────────────────────────────────────────────
#
# `--guard-split` re-scores every case and reads WHY each CAUGHT row went red:
# an assertion judging the mutated value, or a `t.Fatalf` reach guard tripping
# on a setup step that never got as far as judging anything. This is the fourth
# scorer in this repo to get the split, after `kanbanclient` (254th pass),
# `schedulerjobs` (269th) and `email-classifier` (281st).
#
# ⚠️ Hand-rolled per suite, not a fix to the shared engine. `scripts/sabotage.py`
# is untouched: card `55d41f34` owns the engine port, the marker list is
# per-suite by nature, and the 254th's finding — that the zero does not
# generalise, so the mode has to be run per scorer — is why the split is worth
# hand-rolling a fourth time rather than waiting for the port.

# The one reach guard in these two packages, as a prefix of the message it
# prints. `recordingPublisher.only` fetches the single published entry and
# aborts the test when there is not exactly one; every test in logging_test.go
# reaches its assertion through it, so a mutation that stops the logger
# publishing trips the guard and the assertion below it never runs.
#
# internal/runstatus contributes no guard at all: its three tests call the pure
# function directly and every t.Fatal in the file is judging its answer.
#
# ⛔ The marker stops at ", got " — BEFORE the `%d`. That is the rule the 269th
# pass wrote down as a trailing-colon discipline, and the colon is not what it
# is really about: this guard's message has no colon anywhere. What the marker
# must do is stop where the guard's text stops being fixed and starts being
# interpolated, and be long enough that no assertion shares the prefix. A marker
# carrying a format verb matches NOTHING, because `%d` is not what prints —
# which reads as "no guard fired" and understates the finding in silence.
# check_markers_against_the_suite() asserts both properties against the real
# format strings, so neither rests on this comment.
REACH_GUARDS = [
    # logging_test.go, recordingPublisher.only — t.Helper(), so the line it
    # reports is the caller's.
    "expected exactly one published entry, got ",
]

# Lines that mean the Go testing RUNTIME killed the run rather than a test
# reporting a failure. A row caught that way prints no `file.go:NN:` line at
# all, so a message-based classifier can only say "unreadable" unless it is
# told to look. The 281st pass met one for real (`os.Exit` under a `-h` flag)
# and named it a third verdict rather than laundering it into either neighbour.
#
# ⚠️ This suite produced NO runtime-fired row — declared, not assumed, and the
# `--guard-split` run below prints the count either way. It is carried anyway
# because the test double panics on purpose: recordingPublisher.Publish panics
# when it is handed something that is not a LogEntry, so a mutation that changed
# what the logger publishes would be caught with no failure line at all.
RUNTIME_KILLS = [
    # Go's testing package panics rather than let a test binary exit green
    # half way through a run.
    "panic: unexpected call to os.Exit(",
    # Any other panic out of the code under test, or out of the double.
    "panic: ",
]


def runtime_kill(out):
    """The runtime signal that killed a run, or "" when a test reported normally.

    Read from the raw output rather than from the failure messages, because the
    whole point is that there are none.
    """
    for line in out.splitlines():
        stripped = line.strip()
        for kill in RUNTIME_KILLS:
            if stripped.startswith(kill):
                return stripped
    return ""


# `go test` reports a failure as `file.go:NN: message`. Split rather than
# searched: the marker has to be judged against the MESSAGE, and a line-wide
# substring test would read an assertion that quotes a guard's wording as the
# guard (the 281st pass's property 1).
#
# The file part is NOT narrowed to `_test\.go` — the 269th pass's correction. A
# helper that forgets `t.Helper()` reports against its own file, and a row whose
# only visible lines were guards would then read as guard-fired where an
# assertion fired.
FAILURE_LINE = re.compile(r"^\s*[\w./-]+\.go:\d+:\s(?P<message>.*)$")

# A printf verb, so a marker can be checked for carrying one. `%%` is a literal
# percent and is deliberately not a verb.
PRINTF_VERB = re.compile(r"%[-+ #0-9.*\[\]]*[a-zA-Z]")


def failure_messages(out):
    """The message half of every `file.go:NN: message` line in a `go test` run.

    Separate from guard_only() so the classification can be driven by synthetic
    lines in self_test() without a Go toolchain in the loop. A classifier that
    can only be exercised by running the suite it classifies is a classifier
    nobody can sabotage.
    """
    found = []
    for line in out.splitlines():
        match = FAILURE_LINE.match(line)
        if match:
            found.append(match.group("message").strip())
    return found


def is_reach_guard(message):
    """True when `message` is one of this suite's setup guards.

    startswith, not `in`: an assertion that merely ends in a guard's wording is
    an assertion (the 281st pass's property 1).
    """
    return any(message.startswith(marker) for marker in REACH_GUARDS)


def guard_only(messages):
    """True when every failure message is a reach guard, and there is at least one.

    Pure, and pure on purpose: this is the whole finding, so it has to be the
    thing the self-test drives directly. `all()` over an empty list is True, and
    a row with no failure lines at all is not a guard-fired row — it is a run
    whose output this scorer could not read, which is a different answer and
    must not be laundered into this one.
    """
    return bool(messages) and all(is_reach_guard(m) for m in messages)


# ─── grounding the marker list in the suite, rather than in this comment ─────
#
# The three scorers before this one carry their markers as prose plus a list,
# and nothing checks the list against the code it claims to describe. Two ways
# that rots, both silent and both pushing the count the same way — toward
# "no guard fired", which is the reading that ends the enquiry:
#
#   * a marker that names nothing (a reworded guard, a copied format verb)
#     matches no line, so a guard-fired row reports as assertion-fired;
#   * a guard added later that no marker covers does the same.
#
# So the checks below read the suite's own `t.Fatal*`/`t.Error*` format strings
# and judge the marker list against them. They are a screen for the second
# direction, not a verdict: a helper may legitimately carry an assertion, and
# HELPER_ASSERTIONS is where that is declared rather than left to be argued
# about at the next reading.
#
# ⚠️ The limit worth knowing, measured against the three earlier scorers rather
# than reasoned about: a marker can be right about the PRINTED line and absent
# from the format string. `cmd/email-classifier`'s suite guards with
# `t.Fatalf("parsing %s: %v", max, err)` where `max` is "-max=0", so the 281st
# pass's markers "parsing -max=0: " and "parsing -max=-1: " match real output
# and appear nowhere in any source file. This check would call both of them
# dead. It fails LOUDLY when it does, which is the tolerable direction — the
# author looks and finds the interpolation — but it is a screen, not a verdict,
# and porting it to a suite with guards of that shape means teaching it to
# expand the interpolation first. ~/.nightly-296/audit_prior_markers.py is the
# out-of-tree version and carries the same limit.
# Every .go file in the two packages, not just the _test.go ones. A `t.Fatalf`
# can only appear where a *testing.T is in scope, so widening costs nothing and
# closes a real hole: the 269th pass's own guards print from
# internal/schedulerjobsprotocol/schedulerjobsprotocol.go, an ordinary source
# file, and a scan reading only _test.go reports them as naming nothing.
SUITE_FILES = sorted(
    path
    for directory in (REPO / "internal" / "logging", REPO / "internal" / "runstatus")
    for path in directory.glob("*.go"))

# Failure formats printed from a non-Test function that are assertions rather
# than reach guards, with the reason. Empty, and measured empty: the one helper
# in these two packages (recordingPublisher.only) prints one message and it is
# a guard. A declaration here is a claim that a helper judges a value, and it
# rots the moment the helper stops doing so — the same shape as the engine's
# `expected_unnoticed`, which is checked in both directions for that reason.
HELPER_ASSERTIONS = {}

GO_FAILURE_CALL = re.compile(r"\bt\.(?:Fatalf|Fatal|Errorf|Error)\(")
GO_FUNC_DECL = re.compile(r"^func\s+(?:\([^)]*\)\s*)?(?P<name>\w+)\s*\(")
GO_STRING = re.compile(r'"(?:[^"\\]|\\.)*"')


def _go_unquote(literal):
    """The text a Go interpreted string literal stands for.

    Only the escapes these two test files actually use are handled, and an
    unknown one raises rather than being passed through: a silently mangled
    literal would be compared against a marker and reported as a mismatch
    nobody could explain.
    """
    out = []
    i = 1
    body = literal[:-1]
    simple = {"n": "\n", "t": "\t", "r": "\r", '"': '"', "\\": "\\", "'": "'"}
    while i < len(body):
        char = body[i]
        if char != "\\":
            out.append(char)
            i += 1
            continue
        escape = body[i + 1]
        if escape not in simple:
            raise ValueError("unhandled Go escape \\%s in %s" % (escape, literal))
        out.append(simple[escape])
        i += 2
    return "".join(out)


def suite_failure_formats():
    """[(file, enclosing func, format string)] for every t.Fatal*/t.Error* call.

    Reads the FIRST argument, joining the `"..." + "..."` continuations Go
    wraps long messages in — four of this suite's thirteen messages are written
    that way, and reading only the first fragment would truncate a marker's
    subject mid-sentence.

    Deliberately a text scan and not a Go parse: this file must run without a Go
    toolchain, which is what lets --prove-self-test drive it under sabotage.
    """
    found = []
    for path in SUITE_FILES:
        enclosing = "?"
        text = path.read_text()
        lines = text.split("\n")
        for index, line in enumerate(lines):
            declared = GO_FUNC_DECL.match(line)
            if declared:
                enclosing = declared.group("name")
            call = GO_FAILURE_CALL.search(line)
            if not call:
                continue
            # Take the argument list as it continues across `+` line breaks,
            # then read the leading run of string literals out of it.
            argument_text = line[call.end():]
            following = index
            while argument_text.rstrip().endswith("+") and following + 1 < len(lines):
                following += 1
                argument_text += lines[following]
            fragments = []
            rest = argument_text
            while True:
                literal = GO_STRING.match(rest.lstrip())
                if not literal:
                    break
                fragments.append(_go_unquote(literal.group(0)))
                rest = rest.lstrip()[literal.end():].lstrip()
                if not rest.startswith("+"):
                    break
                rest = rest[1:]
            if fragments:
                found.append((path.name, enclosing, "".join(fragments)))
    return found


def check_markers_against_the_suite(formats=None):
    """Problems with the marker list, judged against the suite's real messages.

    Empty means every marker names exactly one printed message and every
    message printed from a helper is accounted for. `formats` is injectable so
    the self-test can drive both directions on messages that are not on disk.
    """
    problems = []
    formats = suite_failure_formats() if formats is None else formats

    for marker in REACH_GUARDS:
        verb = PRINTF_VERB.search(marker)
        if verb:
            problems.append(
                "marker %r carries the format verb %r, and a verb is not what "
                "prints — the marker would match no line at all and every row "
                "it should have caught would report as assertion-fired"
                % (marker, verb.group(0)))
        named = [f for _, _, f in formats if f.startswith(marker)]
        if len(named) != 1:
            problems.append(
                "marker %r is a prefix of %d of the suite's %d failure messages; "
                "exactly one is the only count that can tell a guard from an "
                "assertion" % (marker, len(named), len(formats)))
            continue
        # And it must reach ALL the way to the interpolation point. A marker cut
        # shorter than that is a prefix of every future message that opens the
        # same way, and this is the on-disk statement of the discipline the 269th
        # pass wrote down as a trailing colon — asked of the format string rather
        # than of the author's memory of it.
        head = PRINTF_VERB.search(named[0])
        fixed = named[0][:head.start()] if head else named[0]
        if marker != fixed:
            problems.append(
                "marker %r is not the fixed head of the message it names — that "
                "head is %r. A marker that stops short prefixes every later "
                "message opening the same way; one that runs past the first verb "
                "matches nothing at all" % (marker, fixed))

    for name, enclosing, fmt in formats:
        if enclosing.startswith("Test"):
            continue
        if is_reach_guard(fmt) or fmt in HELPER_ASSERTIONS:
            continue
        problems.append(
            "%s: %s prints %r from outside a Test function and no marker covers "
            "it — a guard the list has not learned reports as an assertion. "
            "Add a marker, or declare it in HELPER_ASSERTIONS with the reason it "
            "judges a value" % (name, enclosing, fmt))

    for declared in HELPER_ASSERTIONS:
        if not any(fmt == declared for _, _, fmt in formats):
            problems.append(
                "HELPER_ASSERTIONS declares %r, which this suite no longer "
                "prints — a declaration that outlives its subject is prose "
                "asserting something about code that is gone" % declared)
    return problems


def self_test():
    """Prove the classifier returns BOTH verdicts, for the right reasons.

    Returns problems; empty means sound. Every failure line below is real output
    from these two test files, taken from their `t.Fatal*` format strings rather
    than invented — a self-test written against lines the suite does not print
    measures the author's idea of the format.
    """
    problems = []

    def check(label, messages, want):
        got = guard_only(messages)
        if got != want:
            problems.append("%s: guard_only=%s, want %s" % (label, got, want))

    check("the one reach guard in this suite",
          ["expected exactly one published entry, got 0"],
          True)
    check("the same guard tripping twice",
          ["expected exactly one published entry, got 0",
           "expected exactly one published entry, got 2"],
          True)
    check("a real assertion on the level a finished run was filed at",
          ['a timed-out run was filed at level "info", want "error"'],
          False)
    check("one assertion alongside the guard — the mechanism WAS scored",
          ["expected exactly one published entry, got 0",
           'a timed-out run was filed at level "info", want "error"'],
          False)
    check("no failure lines at all is not a guard verdict", [], False)

    # ⚠️ The next three lines are NOT messages this suite prints, and are
    # labelled so rather than mixed in above. They state a property of the
    # CLASSIFIER, which is a different thing from characterising the suite: a
    # marker found anywhere but at the start of a message is not a guard.
    #
    # It is stated directly because this suite cannot state it any other way.
    # The 281st pass could redden `in`-instead-of-`startswith` with real output,
    # because its suite really did print an assertion quoting a guard's wording.
    # None of this suite's fourteen assertions contains the one marker, so every
    # real line agrees under both readings and a fixture built from them would
    # measure nothing. A discipline no case can redden is a comment rather than
    # a rule (the 269th pass), and this is the redden-able form of it.
    for marker in REACH_GUARDS:
        if is_reach_guard("the entry the logger published said it " + marker):
            problems.append(
                "is_reach_guard() found %r in the middle of a message and called "
                "it a guard; an assertion that quotes a guard's wording is an "
                "assertion" % marker)
    check("an assertion that opens the way the guard does and continues differently",
          ["expected exactly one published entry to carry the run's status, "
           "and it carried none"],
          False)

    # Two of this suite's assertions OPEN with an interpolated value. They are
    # the reason the 281st pass needed a pattern list for its guards — a guard
    # shaped this way cannot be expressed as a prefix at all — and the reason
    # this suite needs none: neither is a guard.
    check("an assertion opening with a verb, its value substituted",
          ['"cancelled" was not reported as a failure'],
          False)
    check("the duplicate-spelling assertion, also opening with a verb",
          ['Timeout and Error are both "error"'],
          False)

    # The marker list against the suite it describes, in both directions, on
    # messages that are not on disk. The on-disk answer is asked separately by
    # guard_split(); driving it here as well is what lets --prove-self-test
    # redden a broken marker without a Go tree in the loop.
    grounded = [("logging_test.go", "only",
                 "expected exactly one published entry, got %d"),
                ("logging_test.go", "TestTheStartEntryIsAlwaysInfo",
                 'job start was filed at level %q, want "info"')]
    if check_markers_against_the_suite(grounded):
        problems.append("check_markers_against_the_suite() rejected a marker list "
                        "that names exactly one real guard: %r"
                        % check_markers_against_the_suite(grounded))
    hidden_guard = grounded + [("logging_test.go", "mustPublish", "publishing: %v")]
    if not check_markers_against_the_suite(hidden_guard):
        problems.append("check_markers_against_the_suite() accepted a helper guard "
                        "no marker covers, which is how the list stops describing "
                        "the suite")
    verb_in_the_message = [("logging_test.go", "only",
                            "counted %d published entries, want exactly one")]
    if not check_markers_against_the_suite(verb_in_the_message):
        problems.append("check_markers_against_the_suite() accepted a marker that "
                        "names none of the suite's messages")

    # The third verdict, and the direction that matters most: an ordinary
    # reported failure must NOT read as a runtime kill, or a guard-fired row is
    # laundered into real coverage.
    killed = runtime_kill(
        "--- FAIL: TestTheFinishEntryGoesToTheSchedulerLogSubject (0.00s)\n"
        "panic: logging published something that is not a LogEntry "
        "[recovered, repanicked]\n\ngoroutine 7 [running]:\n")
    if not killed.startswith("panic: logging published something"):
        problems.append("runtime_kill() read %r from a run the test double's own "
                        "panic killed" % killed)
    if runtime_kill("--- FAIL: TestATimedOutRunIsNotAnInfoLevelEvent (0.00s)\n"
                    "    logging_test.go:77: expected exactly one published entry, got 0\n"
                    "FAIL\n"):
        problems.append("runtime_kill() called an ordinary reported failure a "
                        "runtime kill, which would launder a guard-fired row")

    # And the reader itself: the location half must be stripped, non-failure
    # lines dropped, and a helper's own .go file read rather than skipped.
    parsed = failure_messages(
        "--- FAIL: TestATimedOutRunIsNotAnInfoLevelEvent (0.00s)\n"
        "    logging_test.go:77: expected exactly one published entry, got 0\n"
        "    logging.go:52: a helper that forgot t.Helper()\n"
        "    not a failure line\n"
        "FAIL\tgithub.com/slava/scheduler/internal/logging\t0.01s\n")
    if parsed != ["expected exactly one published entry, got 0",
                  "a helper that forgot t.Helper()"]:
        problems.append("failure_messages() read %r" % (parsed,))

    return problems


SelfSabotage = namedtuple("SelfSabotage", "anchor replacement expect what")

# The anchors for the self-sabotages, kept here rather than in the prose because
# prose cannot hold an anchor: the docstring is the first thing in the file, so
# a quoted anchor is shadowed by its own documentation and a first-match replace
# edits the sentence instead of the code. That is card `bb6e5601`, filed by the
# 269th pass after exactly that produced a green run that had measured a comment.
#
# Each anchor is kept long enough to occur EXACTLY ONCE outside this table and
# NOWHERE in the module docstring. check_self_sabotage_anchors() asserts both.
SELF_SABOTAGES = [
    SelfSabotage(
        'REACH_GUARDS = [\n    # logging_test.go, recordingPublisher.only',
        'REACH_GUARDS = []\n_MARKERS_THE_SABOTAGE_TOOK_AWAY = [\n'
        '    # logging_test.go, recordingPublisher.only',
        "RED",
        "empty the marker list: the guard rows must fail, and the suite's one "
        "helper guard must report as uncovered",
    ),
    SelfSabotage(
        '    "expected exactly one published entry, got ",\n',
        '    "expected exactly one published entry, got %d",\n',
        "RED",
        "put the format verb back into the marker: it now matches no line the "
        "suite prints, so every guard-fired row would read as assertion-fired",
    ),
    SelfSabotage(
        '    "expected exactly one published entry, got ",\n]',
        '    "expected exactly one published entry",\n]',
        "RED",
        "cut the marker back before its interpolation point: it now also "
        "prefixes an assertion about the same count",
    ),
    SelfSabotage(
        "    return any(message.startswith(marker) for marker in REACH_GUARDS)\n",
        "    return any(marker in message for marker in REACH_GUARDS)\n",
        "RED",
        "match anywhere in the message instead of at its start: the assertion "
        "that quotes the guard's wording must stop being an assertion",
    ),
    SelfSabotage(
        "    return bool(messages) and all(is_reach_guard(m) for m in messages)\n",
        "    return all(is_reach_guard(m) for m in messages)\n",
        "RED",
        "drop the non-empty requirement: a row with no readable failure line "
        "would be laundered into a guard-fired one",
    ),
    SelfSabotage(
        "        if is_reach_guard(fmt) or fmt in HELPER_ASSERTIONS:\n",
        "        if True:\n",
        "RED",
        "stop asking whether a helper's message is covered: a guard added "
        "later would never be noticed",
    ),
    SelfSabotage(
        "RUNTIME_KILLS = [\n    # Go's testing package panics",
        "RUNTIME_KILLS = []\n_KILLS_THE_SABOTAGE_TOOK_AWAY = [\n"
        "    # Go's testing package panics",
        "RED",
        "empty the runtime-kill list: a row the double's panic caught would "
        "report as unreadable rather than as the coverage it is",
    ),
    SelfSabotage(
        "def guard_only(messages):\n",
        "def guard_only(messages):\n    return True\n",
        "RED",
        "return a constant true: the assertion rows and the empty-input row "
        "must fail",
    ),
    SelfSabotage(
        "# internal/runstatus contributes no guard at all",
        "# internal/runstatus contributes no reach guard at all",
        "GREEN",
        "CONTROL: reword a comment beside the marker list; nothing may change",
    ),
]


def _docstring_and_code_of(source):
    """`source` split at the end of its module docstring.

    Pure, so `_sabotage_source()` can use it on text that is not on disk.
    """
    docstring = ast.get_docstring(ast.parse(source), clean=False)
    end = source.find(docstring) + len(docstring)
    return source[:end], source[end:]


def _docstring_and_code():
    """This file's own source, split at the end of its module docstring.

    Read from disk rather than from `__doc__` so the table's text is visible
    too. The question these checks ask is what a hand-run string replace over
    the FILE would hit, and `__doc__` cannot answer it — it has already dropped
    everything the replace would have to compete with.
    """
    return _docstring_and_code_of(Path(__file__).resolve().read_text())


def _without_the_anchor_table(code):
    """`code` with the SELF_SABOTAGES literal cut out.

    The table quotes every anchor by definition, so counting occurrences with it
    still in place would answer 1 for an anchor that names nothing at all.
    """
    start = code.index("SELF_SABOTAGES = [")
    end = code.index("\n]\n", start) + len("\n]\n")
    return code[:start] + code[end:]


def check_self_sabotage_anchors():
    """Problems with the documented anchors themselves. Empty means each picks code.

    Two properties, the second of which is card `bb6e5601`:

      * exactly one occurrence outside the table, so a replace cannot pick a
        site nobody chose — the 261st pass's uniqueness rule;
      * no occurrence in the module docstring, which comes FIRST, so a
        first-match replace cannot edit the prose and print a control that
        behaved.
    """
    problems = []
    docstring, code = _docstring_and_code()
    outside = _without_the_anchor_table(code)
    for sabotage in SELF_SABOTAGES:
        count = outside.count(sabotage.anchor)
        if count != 1:
            problems.append(
                "self-sabotage %r: its anchor occurs %d times outside the table, "
                "so applying it picks a site nobody chose" % (sabotage.what, count))
        if sabotage.anchor in docstring:
            problems.append(
                "self-sabotage %r: its anchor also occurs in the module docstring, "
                "which comes FIRST — a first-match replace would edit the prose "
                "and the run would read as a control that behaved" % sabotage.what)
    return problems


def _sabotage_source(source, anchor, replacement):
    """`source` with the one occurrence of `anchor` that names real code replaced.

    Not `str.replace`. Unbounded, it also edits the table's own copy of the
    anchor, which for a single-line anchor is a Python string literal — and a
    replacement carrying a newline then cuts that literal in half, so the file
    stops parsing and the row reads RED for a reason that has nothing to do with
    the classifier. Bounded to the first match, it is the defect `bb6e5601` was
    filed for: the docstring's copy comes first.

    So neither. The occurrence to sabotage is the one outside both, and
    check_self_sabotage_anchors() has already established there is exactly one.
    Returns None when there is not.
    """
    docstring, code = _docstring_and_code_of(source)
    start = code.index("SELF_SABOTAGES = [")
    end = code.index("\n]\n", start) + len("\n]\n")
    at = code.find(anchor)
    while at != -1 and start <= at < end:
        at = code.find(anchor, at + 1)
    if at == -1:
        return None
    return docstring + code[:at] + replacement + code[at + len(anchor):]


def prove_self_test():
    """Apply every SELF_SABOTAGES row to this file and check the self-test answers.

    Status: 0 when each row reaches the verdict it claims. This is what makes
    the classifier's soundness a measurement rather than a story about a run
    somebody once did — and the 269th pass's third correction is why it exists
    at all: a self-test proves the FUNCTION, and only this proves the MODE.

    It restores this file with `git checkout`, so it refuses to start on an
    uncommitted one: a restore-from-git harness over unsaved work is a delete.

    ⚠️ A sabotaged file that does not PARSE exits non-zero and reads as RED,
    which is a SyntaxError grading itself as a caught mutation. Every row is
    parsed before it is run and reported BROKEN, never RED, when it will not.
    """
    import subprocess

    source_file = Path(__file__).resolve()
    dirty = subprocess.run(["git", "status", "--porcelain", "--", str(source_file)],
                           cwd=REPO, capture_output=True, text=True).stdout.strip()
    if dirty:
        print("REFUSED: %s has uncommitted changes, and this mode restores it with "
              "git checkout.\nCommit first, or the restore is a delete." % source_file.name)
        return 1

    def run_self_test():
        proc = subprocess.run(
            [sys.executable, "-I", str(source_file), "--self-test-only", "--under-sabotage"],
            cwd=REPO, capture_output=True, text=True)
        return proc.returncode, (proc.stdout + proc.stderr).strip()

    code, out = run_self_test()
    print("baseline: exit %d — %s\n" % (code, out.splitlines()[-1] if out else ""))
    if code != 0:
        print("REFUSED: the baseline is already red, which grades every row below "
              "as a broken control.")
        return 1

    original = source_file.read_text()
    misbehaved = []
    for sabotage in SELF_SABOTAGES:
        sabotaged = _sabotage_source(original, sabotage.anchor, sabotage.replacement)
        if sabotaged is None:
            misbehaved.append(sabotage.what)
            print("BROKEN   %s\n         anchor names nothing outside the docstring "
                  "and the table\n" % sabotage.what)
            continue
        try:
            ast.parse(sabotaged)
        except SyntaxError as bad:
            misbehaved.append(sabotage.what)
            print("BROKEN   %s\n         the sabotaged file does not parse (%s), so "
                  "its exit status would grade a SyntaxError as a red self-test\n"
                  % (sabotage.what, bad.msg))
            continue
        source_file.write_text(sabotaged)
        try:
            code, out = run_self_test()
        finally:
            subprocess.run(["git", "checkout", "--", str(source_file)],
                           cwd=REPO, check=True)
        got = "GREEN" if code == 0 else "RED"
        if got != sabotage.expect:
            misbehaved.append(sabotage.what)
        print("%s%-5s (want %-5s)  %s"
              % ("ok " if got == sabotage.expect else "!! ", got, sabotage.expect,
                 sabotage.what))
        for line in out.splitlines():
            if line.strip().startswith("⚠️"):
                print("         %s" % line.strip())
        print()

    if source_file.read_text() != original:
        print("⚠️  the restore did not put the file back — check `git diff` before "
              "trusting anything above")
        return 1
    print("=" * 70)
    if misbehaved:
        print("%d of %d rows did not behave:" % (len(misbehaved), len(SELF_SABOTAGES)))
        for what in misbehaved:
            print("  ⚠️  " + what)
        return 1
    print("all %d rows behaved: %d sabotages redden the self-test and the control "
          "does not" % (len(SELF_SABOTAGES), len(SELF_SABOTAGES) - 1))
    return 0


def guard_split():
    """Re-score every case, reading WHY each CAUGHT row went red.

    Status: 0 when the classifier is sound and no REAL row is guard-fired only,
    1 otherwise. A guard-fired row is a finding, not a footnote — it is a case
    whose CAUGHT in the default table is a number for something nobody measured.

    ⚠️ No row of this suite reaches the guard-only verdict, and the reason is
    structural rather than lucky. Every setup-breaking mutation — the
    known-positive control included — also reddens
    TestTheFinishEntryGoesToTheSchedulerLogSubject, which reads the recorder
    directly instead of through `recordingPublisher.only`. One test that
    observes the double without the guard is enough to keep every such row
    assertion-fired. That was measured, not assumed: with that one test removed
    the control row reports guard-only, which is also this mode's only proof
    over real output that it can say so at all. The probe is
    ~/.nightly-296/prove_the_mode.sh; the 269th pass ended with exactly that
    gap and left its own probe uncommitted.
    """
    problems = self_test()
    # The sabotage procedure is an instrument too, and the one here whose
    # failure mode is a GREEN run — so it gates the report exactly as the
    # classifier does.
    #
    # Skipped under `--under-sabotage`, which `--prove-self-test` passes to its
    # children: this check judges the file AT REST, and under a sabotage every
    # row would report its own anchor missing, so the control could never be
    # green.
    if "--under-sabotage" not in sys.argv:
        problems.extend(check_self_sabotage_anchors())
        problems.extend(check_markers_against_the_suite())
    if problems:
        print("SELF-TEST FAILED — the classifier cannot be trusted to report anything:")
        for p in problems:
            print("  ⚠️  " + p)
        return 1
    print("self-test: guard_only() returns both verdicts for the right reasons, "
          "and every marker names exactly one message this suite prints\n")
    if "--self-test-only" in sys.argv:
        return 0

    rows = []
    with _sabotage_session(TARGETS, PACKAGES) as (rels, restore, original):
        for case in CASES:
            text = _apply_case(TARGETS, original, case)
            for target in TARGETS:
                if text[target] != original[target]:
                    target.write_text(text[target])
            verdict, out = _run_tests(PACKAGES)
            restore()
            messages = failure_messages(out)
            killed = runtime_kill(out)
            if verdict != "CAUGHT":
                why = ""
            elif messages and guard_only(messages):
                why = "guard only -- NOT coverage"
            elif messages:
                why = "assertion-fired"
            elif killed:
                why = "runtime-fired"
            else:
                why = "CAUGHT with no readable failure line"
            rows.append((case, verdict, why, killed))
            print("%-14s %-32s %s" % (verdict, why, case.name))

    print("\n============ guard split ============")
    guarded = [r for r in rows if r[2].startswith("guard only")]
    runtime = [r for r in rows if r[2] == "runtime-fired"]
    unreadable = [r for r in rows if r[2].startswith("CAUGHT with no")]
    caught = [r for r in rows if r[1] == "CAUGHT"]
    print("%d CAUGHT rows, %d caught by a reach-guard only, %d caught by the Go "
          "test runtime, %d unreadable"
          % (len(caught), len(guarded), len(runtime), len(unreadable)))
    for case, _, _, _ in guarded:
        print("  ⚠️  guard-fired only, so this row scored nothing: %s" % case.name)
    # Printed, never swallowed. A verdict that does not fail the run still has
    # to be visible, or "0 guard-fired" reads as "every row was an assertion".
    for case, _, _, killed in runtime:
        print("  ·   real coverage, but no assertion ran — the Go test runtime "
              "caught it:\n      %s\n      %s" % (case.name, killed))
    for case, _, _, _ in unreadable:
        print("  ⚠️  no readable failure line, classify by hand: %s" % case.name)
    return 1 if guarded or unreadable else 0


if __name__ == "__main__":
    if "--prove-self-test" in sys.argv:
        sys.exit(prove_self_test())
    if "--guard-split" in sys.argv or "--self-test-only" in sys.argv:
        sys.exit(guard_split())
    sys.exit(score(TARGETS, PACKAGES, CASES))
