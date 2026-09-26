"""Score internal/kanbanclient's tests by breaking the client on purpose.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-kanbanclient.py [--crosstable] [--diffs]
    python3 scripts/sabotage-kanbanclient.py --guard-split [--self-test-only]
    python3 scripts/sabotage-kanbanclient.py --prove-self-test

Filed by card 1495e79d and written by the eighty-seventh nightly pass. The
tests that closed its red were written by the eighty-ninth, against card
0bea051b.

## Ported onto main 2026-09-26 (card ab673fe9, slice C3g)

Taken from test/the-kanbanclient-scorer-tells-a-guard-from-an-assertion
(4e4eea5c, 021510ad, f8cae0e0, b816969b, 10cd6f9c, 5c0caf6d, ff006579,
1558b520, 36475f14). Main's client.go had doubled since, so seven needles were
re-aimed: every path id now goes through escapePathSegment, New builds a local
httpClient before it returns, and MoveCard returns a MoveResult. No case was
dropped or reworded.

Main's own tests scored **22/33** against this case list before the port. The
11 misses were board bootstrap (six), the two CreateCard guards, the empty-id
response, the timeout and the trailing-slash trim. The seven tests in
client_store_contract_test.go that close them are the only ones ported from
021510ad; its other 18, and b816969b's three, pin routes, statuses and field
names that main's client_protocol_test.go now asserts, and --guard-split
confirms each of those rows is reddened by an assertion, not a reach guard.

REACH_GUARDS and HELPER_ASSERTIONS were re-read against main's nine test
files: the self-test named every helper message the branch list did not know,
and each was judged by hand below.

The sections after this one are the branch's, kept as written. Their scores
are superseded by these, taken on main at d075058 plus this port:

    Score: 33/33 real mechanisms caught, both controls behaved, exit 0.
    Guard split: 34 CAUGHT rows, 0 guard-fired only, 0 unreadable, exit 0.

## Why this package has a scorer of its own

`internal/kanbanclient` sat in sabotage-kanban-dispatcher.py's PACKAGES without
being in its TARGET, so the dispatcher's run executed this package's tests and
no mutation could ever reach them: both were permanently in the silent column of
that scorer's cross-table. Adding kanbanclient targets to the dispatcher's list
would have hidden a shared package's cases inside one of its three callers —
kanban-curator and autoworker use this client too — so it gets its own file, and
the dispatcher's PACKAGES shrank to its own package on the same commit.

## The score

    Score: 33/33 real mechanisms caught, exit 0. Taken 2026-08-09, re-taken 2026-08-16.
    Guard split: 34 CAUGHT rows, 0 guard-fired only, 0 unreadable, exit 0.
                 Taken 2026-08-16, re-taken 2026-08-17 after the anchor repair.
    Self-sabotage set: 7 rows, all behaving, exit 0. Taken 2026-08-21.
    Marker list vs the suite: clean under all four branches, exit 0. Taken
                 2026-08-21, on the 30 t.Fatal*/t.Error* format strings in
                 internal/kanbanclient.

⚠️ **The fourth line is a check on THIS FILE, not on the client.** REACH_GUARDS
was prose plus a literal with nothing comparing either against the code they
describe, and the way a marker list fails is silent and one-directional: a marker
that stops naming anything matches no line, so a row it should have explained
reports as assertion-fired and the third line's `0 guard-fired only` becomes
partly an artefact of the list rather than a measurement of the suite.
check_markers_against_the_suite() reads the suite's own format strings and
answers it. The list is clean under all four branches today, which means landing
the check changed no score — so the four self-sabotage rows are the only proof it
works, and `--prove-self-test` is what takes it.

⚠️ **The second line is not decoration, and for a week the first line was worth
less than it read.** When `--guard-split` was first run, three of those 33 were
reddened by a `t.Fatalf("CreateCard: %v", err)` and nothing else — CAUGHT for
"an error came back", which a store being down produces just as well. They are
closed now; the point that survives is that **33/33 could not have told you**,
and neither could the cross-table or coverage. Run both modes.

Both controls behave: the known-positive is caught, the known-negative is not.
The 25 that were UNNOTICED when this file was written are pinned by
internal/kanbanclient/client_store_contract_test.go, which was written from this
case list — each case names a failure, and each test names the same failure back.

⛔ **Do not close a future red by declaring a case `expected_unnoticed`.** That
field means "a case the suite is NOT expected to catch" — a log line, a
degenerate-input guard no single mutation isolates. Using it on a real mechanism
converts a measured coverage hole into a green tick and destroys the only record
that the hole exists. A red here closes when a test is written, and nothing else
closes it honestly. This rule does not depend on the score: it is the one edit
that can make this file lie, and it stays a one-line change for as long as the
file exists.

**A green scorer says nothing until it has been seen red.** A 33/33 that has
never printed anything else is the same instrument as an 8/33 that can only
print that: in both, one branch has never executed. Both were run at this score
before it was committed. Deleting the single assertion from
TestHoldCardTreatsANonOKStatusAsARefusal drops this file to **32/33, exit 1**,
naming that mechanism and no other — so the red branch works, it is attributed to
the right test, and no other test is quietly covering for it. If you change this
file, do that again; it costs one run.

Cross-table re-taken 2026-08-16 by the 254th pass, after the three tests it added
(`--crosstable`, exit 0): **0 of 25 tests are reddened by no case**, and every
case that reddens the package is reproduced by at least one named test, so this
scorer still needs no `unreddened` declaration. Do not re-derive either number;
re-run the mode instead. The 2026-08-09 taking read 0 of 22 and is superseded
only because the denominator moved.

⚠️ **Read that table next to `--guard-split`, because on its own it is reassuring
about the wrong thing.** For "a card is posted to a route the store does not
serve" the cross-table names FOUR red tests — and before the 254th pass three of
them were reddened by a reach guard. The mode asks which tests no case reddens;
it cannot ask whether a test that DID go red went red for a reason. Four names in
that column read as four tests pinning the route, and one was.

## Two cases are credited to a test that was not written for them

Worth reading before treating the table as one case per test.

"The session link points at an id nobody minted" appends a suffix to the ref, and
that reddens TestACardWithNoSessionWritesNoSessionLink as well as the round-trip
test — because a card with NO session then carries the ref "-card", which is not
empty, so a link gets written where none should be. It is credited to two tests
for a reason neither test was written to express.

"An unreadable link list is reported as an empty one" reddens
TestAStoreErrorCarriesTheStoresOwnExplanationOfIt as well as its own test, for a
plainer reason: an error that is never returned cannot carry an explanation. The
second test is asserting something real, and it would go green again the moment
the status check came back.

## --guard-split: which CAUGHT rows actually scored a mechanism

Added by the 254th nightly pass against card `3b6eba2a`, which measured that
**41 sabotage blobs in this repo and none of them can tell a guard-fired row from
an assertion-fired one.** The 42nd pass made that split in inber-party by hand;
scheduler never got it, so every CAUGHT above is ambiguous. A `t.Fatalf` that only
checks the call under test reached and returned is a second sensitive thing in the
suite, and a row that trips ONLY those has scored nothing about the mechanism it
names — it has scored "an error came back", which any error anywhere produces.

⚠️ **This is deliberately a twelfth hand-rolled `REACH_GUARDS` list, not a fix to
the engine.** `scripts/sabotage.py` here is blob `9d5ad6a0`, and giving it the
classification would change all eleven of this repo's scorers at once and fork it
further from the union — which card `55d41f34` scopes as the merge nobody did, and
files as `decision-needed` and `needs-user-action`. One scorer is what is
decision-free; the sweep is not. Do not generalise this file into the engine
without reading `55d41f34` first.

⚠️ **`t.Fatal` is NOT the predicate, and reading it as one gets this wrong in both
directions.** 28 of these two files' `t.Fatal` sites were read by hand: 11 are
reach guards and 17 are real assertions that happen to be fatal
(`MoveCard reported success for a 500`, `New built a client with timeout %v`,
`the spend readers' filter selected ...`). Splitting on the call shape would have
called all 28 guards and reported a suite that scores nothing. The messages are
the predicate, which is why `REACH_GUARDS` is a message list.

The mode reuses the engine's `_sabotage_session`, `_apply_case` and `_run_tests`
rather than restating them. The leading underscores are deliberate: the engine's
own docstring says a second mode with its own hand-rolled cleanup reopens the
60th pass's killed-run bug, and its dirty-tree and red-baseline refusals are the
only thing standing between a scoring run and another agent's uncommitted work.
The default `score()` path is untouched, so the 33/33 above still means what it
said.

**The instrument is shown able to say "guard-fired" before it is believed.**
`guard_only()` is a pure function over failure lines and `self_test()` drives it
in both directions, including a line whose text is a guard message embedded in an
otherwise-ordinary assertion. `--guard-split` refuses to report if the self-test
fails, and emptying the marker list reddens it — measured, not assumed. A clean
audit and an audit that never ran print the same thing otherwise (215th, 219th).

## Why the sabotage anchors are data — the 270th pass, card `bb6e5601`

The sentence above used to say *emptying `REACH_GUARDS` reddens it*, and that is
an ambiguous instruction: the name occurs six times in this file, three of them
in this docstring. The anchor a pass would actually reach for, the list's opening
line, is unique here and does not appear in the prose — so this file was **sound
by luck**, and nothing recorded that it was.

`sabotage-schedulerjobs.py` was not. There the same sabotage was applied by
matching a string that occurs in the prose as well as the code; the prose came
first, the run measured a comment, and it printed green, which is
indistinguishable from a control that behaved. An anchor that matches nothing
reads as SKIPPED and is visible (261st); an anchor that matches the WRONG
occurrence is not.

So the anchors live in the self-sabotage table below the code rather than in this
paragraph, `self_test()` checks that each still occurs exactly once outside that
table and nowhere in this docstring, and `--prove-self-test` applies the set and
reports. Luck that nobody checks is the same instrument as no luck at all.

⚠️ **The colon rule this file's marker list relies on is NOT asserted structurally
here, and `sabotage-schedulerjobs.py`'s check does not port.** That check requires
every marker to end in `": "`; this list mixes shapes on purpose —
`"CreateCard made no request"` and `"want exactly 1: "` do not both end in a
colon — so importing it would redden a sound list. `bb6e5601` says to judge the
shape rule before copying it. Judged: it does not apply.
"""

import ast
import re
import sys
from collections import namedtuple
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import (  # noqa: E402
    REPO,
    Case,
    _apply_case,
    _run_tests,
    _sabotage_session,
    score,
)

TARGET = REPO / "internal/kanbanclient/client.go"
PACKAGES = ["./internal/kanbanclient/"]


# Messages these tests emit from a t.Fatal that only asserts the call under test
# was REACHED — it returned without an error, or the store recorded a request to
# look at. A row that fires only these has not scored the mechanism it names.
#
# Read off the two test files by hand, site by site, on 2026-08-16, and re-read
# against main's nine test files on 2026-09-26 when this scorer was ported
# (card ab673fe9): the branch's "CreateCard made no request" went with the test
# that printed it, and main's own guards were added. Every entry is a literal
# from the source, kept long enough to pick one site family: the trailing ": "
# on the operation names is load-bearing, because "ListCardLinks: " (the guard)
# and "ListCardLinks returned %v and no error for a 503" (an assertion) share
# their first thirteen characters.
REACH_GUARDS = [
    "CreateCard: ",
    "EnsureBoard: ",
    "ListCardLinks: ",
    "MoveCard: ",
    "ListCardPlacements: ",
    "PatchCardBody: ",
    "ListEntityTags: ",
    "ListCardsForEntity: ",
    "FindBoard: ",
    "HoldCard: ",
    "PutTicket: ",
    "AddCardLink: ",
    "AddEntityTag: ",
    "GetCardEffectiveDefaults: ",
    "unexpected error: ",
    "call returned an error on a scripted success: ",
    "stamp: ",
    "unset token: ",
    # recordingStore.only() and wireRecordingStore.onlyRequest(): a call that
    # made two requests, or none, is not the call under test.
    "want exactly 1: ",
    "expected exactly 1 upstream request, got ",
]

FAILURE_LINE = re.compile(r"_test\.go:\d+:")


def failure_lines(out):
    """The `file_test.go:NN: message` lines in a `go test` run, in order.

    Separate from guard_only() so the classification can be driven by synthetic
    lines in self_test() without a Go toolchain in the loop. A classifier that
    can only be exercised by running the suite it classifies is a classifier
    nobody can sabotage.
    """
    return [l.strip() for l in out.splitlines() if FAILURE_LINE.search(l)]


def marker_matches(marker, text):
    """Whether `marker` is what THIS scorer counts as naming `text`.

    The single-marker half of is_reach_guard(), split out so the classifier and
    check_markers_against_the_suite() cannot drift apart on what matching means.
    A scorer that matched by prefix instead would change this one function and
    nothing else — which is the whole reason the check below is portable, since
    the logging scorer (296th pass) really does match by prefix and this one
    cannot.

    Substring here, and it is not an accident: two of this list's markers name a
    site family from the MIDDLE of a message. "want exactly 1: " sits after an
    interpolated request count and could not be written as a prefix at all.
    """
    return marker in text


def is_reach_guard(text):
    """True when `text` carries any of this suite's reach-guard markers.

    Given a whole `file_test.go:NN: message` line by guard_only() and a bare
    format string by the checker. Substring matching is what makes that
    difference immaterial — the location prefix contains no marker — and stating
    it here is cheaper than discovering it: under a prefix matcher the two
    callers would need different inputs.
    """
    return any(marker_matches(marker, text) for marker in REACH_GUARDS)


def guard_only(lines):
    """True when every failure line is a reach guard, and there is at least one.

    Pure, and pure on purpose: this is the whole finding, so it has to be the
    thing the self-test drives directly. `all()` over an empty list is True, and
    a row with no failure lines at all is not a guard-fired row — it is a run
    whose output this scorer could not read, which is a different answer and
    must not be laundered into this one.
    """
    return bool(lines) and all(is_reach_guard(l) for l in lines)


# Messages printed from a non-Test function that JUDGE a value rather than guard
# a reach, with the reason each one judges something. The marker list must NOT
# carry these: a row reddened only by one of them has scored the mechanism it
# names, so it is assertion-fired, and reading it as a guard would launder a
# real finding into a footnote.
#
# Measured site by site on 2026-08-21 (298th pass) — all three are fixture
# handlers judging what the CLIENT put on the wire, which is the mechanism half
# of the cases they appear in.
#
# Checked in both directions, the same shape as the engine's
# `expected_unnoticed`: a declaration is a claim about code, so a message
# declared here that the suite has stopped printing is a defect, not a leftover.
HELPER_ASSERTIONS = {
    "%s %s: request body is not a JSON object: %v":
        "recordingStore judges what the client sent, and a client that posts a "
        "non-JSON body has failed the mechanism rather than the fixture",
    "%s %s: body is not a JSON object: %v":
        "the fake board store, same shape as above",
    "fake board store was asked for a route it does not serve: %s %s":
        "judges the route the client chose, which is the mechanism half of "
        "every EnsureBoard and CreateCard case",
    # Main's own helpers, judged 2026-09-26 when this scorer was ported.
    "request body is not a JSON object: %v":
        "wireRecordingStore.onlyRequestBody judges what the client sent, the "
        "same judgement as recordingStore's",
    "unexpected request %s %s":
        "effectiveDefaultsServer and moveCardAgainst judge the route and verb "
        "the client chose",
    "%s id=%q: request carries a query or fragment it was never given: %q":
        "assertSegments judges the path the client built from an id",
    "%s id=%q: %d path segments, want %d: %q":
        "assertSegments, the same judgement",
    "%s id=%q: segment %d %q does not decode: %v":
        "assertSegments, the same judgement",
    "%s id=%q: segment %d is %q (decoded %q), want %q — %s":
        "assertSegments, the same judgement",
}

# The directories whose .go files can print a failure line during THIS scorer's
# runs: the packages it tests, plus any helper package their test binaries
# compile against. One entry here, because PACKAGES is one package and its
# helpers are in its own test files.
#
# ⚠️ The limit, stated rather than left to be found: a helper package added
# later and not listed here is invisible to BOTH directions below — its guards
# read as assertions, and a marker naming only its messages reads as dead.
# Widening to the whole repo is not the answer and was measured: it puts 38
# unrelated helpers in front of the reverse direction. The 296th pass's audit
# met the mirror of this and reported live markers as dead by narrowing to
# PACKAGES alone, on a scorer whose guards print from a helper package.
SUITE_DIRS = ["internal/kanbanclient"]

GO_FAILURE_CALL = re.compile(r"\bt\.(?:Fatalf|Fatal|Errorf|Error)\(")
GO_FUNC_DECL = re.compile(r"^func\s+(?:\([^)]*\)\s*)?(?P<name>\w+)\s*\(")
GO_STRING = re.compile(r'"(?:[^"\\]|\\.)*"')

# A printf verb, so a marker can be asked whether it carries one. `%%` is a
# literal percent and is deliberately not a verb.
PRINTF_VERB = re.compile(r"%[-+ #0-9.*\[\]]*[a-zA-Z]")


def _go_unquote(literal):
    """The text a Go interpreted string literal stands for.

    Only the escapes these files actually use are handled, and an unknown one
    raises rather than being passed through: a silently mangled literal would be
    compared against a marker and reported as a mismatch nobody could explain.
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


def suite_files():
    """Every .go file in SUITE_DIRS, ordinary source as well as _test.go.

    Not narrowed to `_test.go`: a t.Fatalf can only appear where a *testing.T is
    in scope, so widening costs nothing and closes a hole the 296th pass's audit
    measured — the 269th pass's two guards print from an ordinary source file in
    a helper package, and a scan reading only _test.go calls both of them dead.

    Non-recursive, because a Go package is one directory. A helper package is a
    SUITE_DIRS entry of its own, named rather than swept up.
    """
    found = []
    for directory in SUITE_DIRS:
        found += sorted((REPO / directory).glob("*.go"))
    return found


def suite_failure_formats():
    """[(file, enclosing func, format string)] for every t.Fatal*/t.Error* call.

    Reads the FIRST argument, joining the `"..." + "..."` continuations Go wraps
    long messages in — reading only the first fragment would truncate a marker's
    subject mid-sentence and report a sound marker as naming nothing.

    Deliberately a text scan and not a Go parse: this file must run with no Go
    toolchain in the loop, which is what lets --prove-self-test drive it under
    sabotage.
    """
    found = []
    for path in suite_files():
        enclosing = "?"
        lines = path.read_text().split("\n")
        for index, line in enumerate(lines):
            declared = GO_FUNC_DECL.match(line)
            if declared:
                enclosing = declared.group("name")
            call = GO_FAILURE_CALL.search(line)
            if not call:
                continue
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


def fixed_segments(fmt):
    """The runs of literal text in a format string, split at every printf verb.

    A marker must END one of these. That single property carries both halves of
    what a marker has to be:

      * it lies inside a fixed run, so it cannot straddle a verb — "d requests"
        out of "got %d requests" is found by a text scan reading the format
        string and matches nothing the suite ever prints;
      * and it reaches the point where the message stops being fixed, so it
        cannot be a prefix of every later message opening the same way.

    The second half is the 269th pass's trailing-colon rule, and asking it this
    way is what lets it survive the two guards that falsify the colon spelling:
    the logging suite's guard has no colon anywhere, and `t.Fatal("no request
    body was captured")` interpolates nothing at all.
    """
    return PRINTF_VERB.split(fmt)


def check_markers_against_the_suite(formats=None, markers=None):
    """Problems with REACH_GUARDS, judged against the messages the suite prints.

    Empty means sound. Both arguments are injectable so self_test() can drive
    every branch on messages and lists that are not on disk — a check that can
    only be exercised by the tree it checks is a check nobody can sabotage, and
    the branches that matter most are the ones this suite is currently clean of.

    What this does NOT assert: that a marker names exactly ONE message. That is
    a property of the logging suite, which has a single guard. Here one guard
    helper is called from many sites and a marker naming the family is the
    point — demanding "exactly one" invented 8 findings in the 296th pass's
    audit, every one of which died when the flag was opened by hand.
    """
    problems = []
    formats = suite_failure_formats() if formats is None else formats
    markers = REACH_GUARDS if markers is None else markers
    printed = [fmt for _, _, fmt in formats]

    for marker in markers:
        verb = PRINTF_VERB.search(marker)
        if verb:
            problems.append(
                "marker %r carries the format verb %r, and a verb is not what "
                "prints — the marker matches no line at all, so every row it "
                "should have caught reports as assertion-fired"
                % (marker, verb.group(0)))
            continue
        named = [fmt for fmt in printed if marker_matches(marker, fmt)]
        if not named:
            problems.append(
                "marker %r matches no message this suite prints, so every row a "
                "guard of that wording would have explained reports as "
                "assertion-fired — the silent direction, and the one that makes "
                "a headline '0 guard-fired' partly an artefact of the list. "
                "Either the guard was reworded and the marker was not, or the "
                "marker's fixed text is interpolated in at run time (card "
                "050fca4c, which this check cannot see)" % marker)
            continue
        misplaced = [fmt for fmt in named
                     if not any(segment.endswith(marker)
                                for segment in fixed_segments(fmt))]
        if misplaced:
            problems.append(
                "marker %r is found in the format string %r but does not end "
                "where that message's fixed text does. Either it straddles a "
                "printf verb — in which case it matches the format string and "
                "never a printed line — or it stops short of the interpolation "
                "point, which makes it a prefix of every later message opening "
                "the same way. This is the 269th pass's trailing-colon "
                "discipline stated so that a guard with no colon and a guard "
                "with no interpolation at all both pass it, which the colon "
                "spelling rejects" % (marker, misplaced[0]))

    for name, enclosing, fmt in formats:
        if enclosing.startswith("Test"):
            continue
        if any(marker_matches(m, fmt) for m in markers) or fmt in HELPER_ASSERTIONS:
            continue
        problems.append(
            "%s: %s prints %r from outside a Test function, and neither a "
            "marker nor HELPER_ASSERTIONS accounts for it — a guard the list "
            "has not learned reports as an assertion, which understates the "
            "finding in silence. Add a marker, or declare it with the value it "
            "judges" % (name, enclosing, fmt))

    for declared in HELPER_ASSERTIONS:
        if declared not in printed:
            problems.append(
                "HELPER_ASSERTIONS declares %r, which this suite no longer "
                "prints — a declaration that outlives its subject is prose "
                "asserting something about code that is gone" % declared)
    return problems


SelfSabotage = namedtuple("SelfSabotage", "anchor replacement expect what")

# The anchors for the self-sabotages the docstring describes. They live here
# rather than in the prose because prose cannot hold an anchor: the docstring is
# the first thing in the file, so a quoted anchor is shadowed by its own
# documentation and a first-match replace edits the sentence instead of the code.
# That is card `bb6e5601`, measured on `sabotage-schedulerjobs.py`, where exactly
# that happened and the run printed green while sabotaging a comment.
#
# Each anchor is kept long enough to occur EXACTLY ONCE outside this table and
# NOWHERE in the module docstring. check_self_sabotage_anchors() asserts both,
# so an anchor that goes ambiguous — or that a later edit copies back up into the
# prose — reddens the run rather than sabotaging the wrong occurrence quietly.
#
# Each carries its replacement so `--prove-self-test` can apply the whole set. A
# procedure nobody has to hand-run is a procedure that cannot pick the wrong
# occurrence.
SELF_SABOTAGES = [
    SelfSabotage(
        "REACH_GUARDS = [",
        "REACH_GUARDS = []\n_MARKERS_THE_SABOTAGE_TOOK_AWAY = [",
        "RED",
        "empty the marker list: the two guard-only rows must fail",
    ),
    SelfSabotage(
        "def guard_only(lines):\n",
        "def guard_only(lines):\n    return True\n",
        "RED",
        "return a constant true: the assertion rows and the empty-input row must fail",
    ),
    # Four rows for check_markers_against_the_suite(), one per branch. It is
    # SKIPPED under --under-sabotage when asked of the tree, so what these
    # redden is the half self_test() drives on injected data — which is the half
    # that has to work, since this suite is currently clean under all four and a
    # branch no case can redden is a comment rather than a check.
    SelfSabotage(
        "        verb = PRINTF_VERB.search(marker)",
        "        verb = None",
        "RED",
        "stop noticing a format verb in a marker: a marker matching no printed "
        "line must still be rejected",
    ),
    SelfSabotage(
        "if not any(segment.endswith(marker)",
        "if False and any(segment.endswith(marker)",
        "RED",
        "accept a marker that does not end where the message's fixed text does: "
        "one that straddles a verb matches no printed line, one that stops short "
        "claims every later message opening the same way",
    ),
    SelfSabotage(
        '        if enclosing.startswith("Test"):',
        "        if True:",
        "RED",
        "stop reading helper-printed messages: a guard the list has not learned "
        "must not sit unnoticed",
    ),
    SelfSabotage(
        "    for declared in HELPER_ASSERTIONS:\n        if declared not in printed:",
        "    for declared in []:\n        if declared not in printed:",
        "RED",
        "stop checking the declarations: one that outlives the message it "
        "describes must be a defect, not a leftover",
    ),
    SelfSabotage(
        "# Messages these tests emit from a t.Fatal",
        "# The messages these tests emit from a t.Fatal",
        "GREEN",
        "CONTROL: reword the comment above the marker list; nothing may change",
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

    Read from disk rather than from `__doc__` so the table's text is visible too.
    The question these checks ask is what a hand-run string replace over the FILE
    would hit, and `__doc__` cannot answer it — it has already dropped everything
    the replace would have to compete with.
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

    Two properties:

      * exactly one occurrence outside the table, so a replace cannot pick a site
        nobody chose — the 261st pass's uniqueness rule;
      * no occurrence in the module docstring, which comes FIRST, so a first-match
        replace cannot edit the prose and print a control that behaved.

    Both hold today. Neither was checked before, and the second was already false
    in `sabotage-schedulerjobs.py` when this check was written.
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
    the classifier. Bounded to the first match, it is the defect this whole file
    was repaired for: the docstring's copy comes first.

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

    Status: 0 when each row reaches the verdict it claims. This is what makes the
    docstring's "emptying the marker list reddens it" a measurement rather than a
    story about a run somebody once did.

    It restores this file with `git checkout`, so it refuses to start on an
    uncommitted one: a restore-from-git harness over unsaved work is a delete
    (the 27th pass). The self-test runs in a fresh interpreter because the
    sabotage edits the module that would otherwise already be imported.

    ⚠️ **A sabotaged file that does not PARSE exits non-zero and reads as RED.**
    That is the 213th pass's build-failure-grades-as-a-control, and this mode hit
    it: an unbounded replace also edited the table's own copy of a single-line
    anchor, and a replacement carrying a newline then cut that string literal in
    half. The row printed RED, wanted RED, and had measured a SyntaxError. So the
    sabotage is applied to the ONE occurrence outside the docstring and the table,
    and the result is parsed before it is run — a row that will not compile is
    reported BROKEN, never RED.
    """
    import subprocess

    source_file = Path(__file__).resolve()
    dirty = subprocess.run(["git", "status", "--porcelain", "--", str(source_file)],
                           cwd=REPO, capture_output=True, text=True).stdout.strip()
    if dirty:
        print("REFUSED: %s has uncommitted changes, and this mode restores it with "
              "git checkout.\nCommit first, or the restore is a delete."
              % source_file.name)
        return 1

    def run_self_test():
        proc = subprocess.run(
            [sys.executable, str(source_file), "--self-test-only", "--under-sabotage"],
            cwd=REPO, capture_output=True, text=True)
        return proc.returncode, (proc.stdout + proc.stderr).strip()

    code, out = run_self_test()
    print("baseline: exit %d — %s\n" % (code, out.splitlines()[-1] if out else ""))
    if code != 0:
        print("REFUSED: the baseline is already red, which grades every row below "
              "as a broken control (213th, 230th).")
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
            # Loud on purpose. A file that will not parse exits non-zero, and a
            # non-zero exit is exactly what a reddened self-test looks like.
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
    # Counted, not written down. A tally in a footer is the thing that goes
    # stale first — sabotage-schedulerjobs.py's said "three" while its table
    # held six.
    reddening = len([s for s in SELF_SABOTAGES if s.expect == "RED"])
    print("all %d rows behaved: %d sabotages redden the self-test and %d control(s) "
          "do not" % (len(SELF_SABOTAGES), reddening, len(SELF_SABOTAGES) - reddening))
    return 0


def self_test():
    """Prove the classifier can return BOTH verdicts. Returns problems, empty is sound.

    Emptying the marker list must redden this — that is the sabotage that turns a
    silent instrument into a measured one, and it is the reason the guard-only
    cases come first. Its anchor is in the self-sabotage table above, not in this
    sentence, and `--prove-self-test` applies it.
    """
    problems = []

    def check(label, lines, want):
        got = guard_only(lines)
        if got != want:
            problems.append("%s: guard_only=%s, want %s" % (label, got, want))

    check("a single reach guard",
          ["client_test.go:83: CreateCard: kanban create card: 500 Internal Server Error"],
          True)
    check("two reach guards from different sites",
          ["client_test.go:83: CreateCard: boom",
           "client_store_contract_test.go:228: EnsureBoard: boom"],
          True)
    check("a real assertion",
          ["client_store_contract_test.go:571: MoveCard reported success for a 500"],
          False)
    check("one assertion alongside a guard — the mechanism WAS scored",
          ["client_test.go:83: CreateCard: boom",
           "client_store_contract_test.go:571: MoveCard reported success for a 500"],
          False)
    # The fixture trap in §B of the craft rules: a marker that is a substring of
    # ordinary English classifies an assertion as a guard. "MoveCard: " has the
    # colon; this line has the words and not the colon, and must read as an
    # assertion.
    check("an assertion that merely names a guarded operation",
          ["client_store_contract_test.go:544: MoveCard left the card in the column it started in"],
          False)
    check("no failure lines at all is not a guard verdict",
          [], False)

    # The marker list against the suite it describes, in both directions and on
    # data that is not on disk. guard_split() asks the on-disk question
    # separately; driving it here as well is what lets --prove-self-test redden
    # a broken list with no Go tree in the loop.
    #
    # `grounded` is built FROM the real list rather than transcribed, so it
    # cannot rot the way a hand-copied fixture does. Every marker is placed
    # after an interpolated value, which is where two of the real ones sit and
    # which a prefix matcher would reject — the fixture states the matcher.
    grounded = [("client_test.go", "TestX", "saw %d requests, " + m + "%v")
                for m in REACH_GUARDS]
    grounded += [("client_store_contract_test.go", "handler", declared)
                 for declared in HELPER_ASSERTIONS]
    found = check_markers_against_the_suite(grounded)
    if found:
        problems.append("check_markers_against_the_suite() rejected a list every "
                        "marker of which names a real message: %r" % (found,))
    hidden_guard = grounded + [("client_test.go", "mustCreate", "creating: %v")]
    if not check_markers_against_the_suite(hidden_guard):
        problems.append("check_markers_against_the_suite() accepted a helper guard "
                        "no marker covers and no declaration explains, which is "
                        "how the list stops describing the suite")
    # The verb branch is asked for its DIAGNOSIS, not for a verdict, and that is
    # the only way it can be reddened: over format strings, a marker carrying a
    # verb can never lie inside a fixed segment, so the straddle branch below
    # rejects it too and a check for "some problem" stays green with the verb
    # branch deleted. Measured — the row read GREEN until this became a check on
    # the message. What the branch buys is a reader being told the marker
    # carries `%v` rather than that it straddles something.
    verb_problems = check_markers_against_the_suite(grounded, markers=["MoveCard: %v"])
    if not any("carries the format verb" in problem for problem in verb_problems):
        problems.append("check_markers_against_the_suite() did not name the format "
                        "verb in a marker that carries one; it reported %r"
                        % (verb_problems,))
    if not check_markers_against_the_suite(grounded, markers=["EnsureBoard returned"]):
        problems.append("check_markers_against_the_suite() accepted a marker that "
                        "names none of the suite's messages")
    # Both halves of the fixed-run rule, on the two shapes that break it. The
    # declarations are carried into each fixture: without them the rot direction
    # fires on all three and the row reads as rejected whatever this branch does
    # — a fixture that cannot fail for the reason it names.
    declared_here = [("client_store_contract_test.go", "handler", declaration)
                     for declaration in HELPER_ASSERTIONS]
    straddles = [("client_test.go", "TestX", "saw %d requests")] + declared_here
    if not check_markers_against_the_suite(straddles, markers=["d requests"]):
        problems.append("check_markers_against_the_suite() accepted a marker that "
                        "straddles a printf verb, so it matches the format string "
                        "and never a printed line")
    stops_short = [("client_test.go", "TestX", "MoveCard: %v")] + declared_here
    if not check_markers_against_the_suite(stops_short, markers=["MoveCard"]):
        problems.append("check_markers_against_the_suite() accepted a marker that "
                        "stops short of the interpolation point, so it claims every "
                        "later message that opens the same way")
    # And the rot direction: a declaration outliving the message it describes.
    if not check_markers_against_the_suite(
            [("client_test.go", "TestX", "saw %d requests, " + m + "%v")
             for m in REACH_GUARDS]):
        problems.append("check_markers_against_the_suite() accepted a "
                        "HELPER_ASSERTIONS declaration this suite no longer prints")

    parsed = failure_lines(
        "--- FAIL: TestSomething (0.00s)\n"
        "    client_test.go:83: CreateCard: boom\n"
        "    not a failure line\n"
        "FAIL\n"
    )
    if parsed != ["client_test.go:83: CreateCard: boom"]:
        problems.append("failure_lines() read %r" % (parsed,))

    return problems


def guard_split():
    """Re-score every case, reading WHY each CAUGHT row went red.

    Status: 0 when the classifier is sound and no row is guard-fired only,
    1 otherwise. A guard-fired row is a finding, not a footnote — it is a case
    whose CAUGHT in the default table is a number for something nobody measured.
    """
    problems = self_test()
    # The sabotage procedure is an instrument too, and the one here whose failure
    # mode is a GREEN run — so it gates the report exactly as the classifier does.
    #
    # Skipped under `--under-sabotage`, which `--prove-self-test` passes to its
    # children. This check judges the file AT REST. Under a sabotage the file is
    # not the file at rest: every row edits an anchor site by definition, so
    # every row would report its own anchor missing, and the control could never
    # be green. The baseline run, which does check it, is the one that counts.
    if "--under-sabotage" not in sys.argv:
        problems.extend(check_self_sabotage_anchors())
        # The marker list is an instrument too, and its failure mode is also a
        # GREEN run: a marker that names nothing reads as "no guard fired". So
        # it gates the report exactly as the classifier and the anchors do.
        #
        # Skipped under `--under-sabotage` for the same reason as the anchors:
        # a self-sabotage that empties or reworks REACH_GUARDS would report its
        # own edit here, and the control could never be green. The baseline run
        # is the one that checks the list at rest.
        problems.extend(check_markers_against_the_suite())
    if problems:
        print("SELF-TEST FAILED — the classifier cannot be trusted to report anything:")
        for p in problems:
            print("  ⚠️  " + p)
        return 1
    print("self-test: guard_only() returns both verdicts for the right reasons\n")
    if "--self-test-only" in sys.argv:
        return 0

    targets = [TARGET]
    rows = []
    with _sabotage_session(targets, PACKAGES) as (rels, restore, original):
        for case in CASES:
            text = _apply_case(targets, original, case)
            for t in targets:
                if text[t] != original[t]:
                    t.write_text(text[t])
            verdict, out = _run_tests(PACKAGES)
            restore()
            lines = failure_lines(out)
            if verdict != "CAUGHT":
                why = ""
            elif not lines:
                why = "CAUGHT with no readable failure line — classify by hand"
            elif guard_only(lines):
                why = "guard only -- NOT coverage"
            else:
                why = "assertion-fired"
            rows.append((case, verdict, why, lines))
            print("%-14s %-22s %s" % (verdict, why, case.name))

    print("\n============ guard split ============")
    guarded = [c for c, v, w, _ in rows if w.startswith("guard only")]
    unreadable = [c for c, v, w, _ in rows if w.startswith("CAUGHT with no")]
    caught = [c for c, v, _, _ in rows if v == "CAUGHT"]
    print("%d CAUGHT rows, %d caught by a reach-guard only, %d unreadable"
          % (len(caught), len(guarded), len(unreadable)))
    for c in guarded:
        print("  ⚠️  guard-fired only, so this row scored nothing: %s" % c.name)
    for c in unreadable:
        print("  ⚠️  no failure line to read: %s" % c.name)
    if not guarded and not unreadable:
        print("  every CAUGHT row was reddened by an assertion about its own mechanism")
    return 1 if (guarded or unreadable) else 0


CASES = [
    # ---- controls ----
    Case(
        "CONTROL known-positive: a created card is never linked to anything",
        [("if err := c.AddCardLink(id, l.entityType, l.entityRef, l.label); err != nil {",
          "if err := func() error { _ = l; return nil }(); err != nil {")],
    ),
    Case(
        "CONTROL known-negative: reword the description a new board is created with",
        [('"description": "Auto-tracked agent dispatches (autoworker, scheduler agent jobs).",',
          '"description": "Cards written automatically by autoworker and by scheduler agent jobs.",')],
        expected_unnoticed="prose in a board record nothing asserts on; the harness must not call this a catch",
    ),

    # ---- the session link: the one path the two tests were written for ----
    # These read the spend chain backwards. CreateCard writes the link that says
    # which session did the work, and cmd/spend-ceiling-guard,
    # cmd/autoworker/spend.go and cmd/autoworker-reviewer all read it back to
    # decide what a task has cost. A drift here does not error — it returns zero
    # rows, which reads as zero dollars, which is a ceiling that never fires.
    Case(
        "the session link's entity type drifts from the one every spend reader filters on",
        [('{kanbanvocab.LinkTypeSession, a.SessionID, "autoworker"},',
          '{"bus_session", a.SessionID, "autoworker"},')],
    ),
    Case(
        "the session link points at an id nobody minted",
        [('{kanbanvocab.LinkTypeSession, a.SessionID, "autoworker"},',
          '{kanbanvocab.LinkTypeSession, a.SessionID + "-card", "autoworker"},')],
    ),
    Case(
        "the provider link is written as a session link too, so one card reports two sessions",
        [('{"autoworker_provider", a.Provider, ""},',
          '{kanbanvocab.LinkTypeSession, a.Provider, ""},')],
    ),
    Case(
        "the blank-ref skip is dropped, so every card with no session links to the session id \"\"",
        [('\t\tif l.entityRef == "" {\n\t\t\tcontinue\n\t\t}\n', "")],
    ),
    Case(
        "the link payload's entity_ref key drifts, so the store records a link to nothing",
        [('payload := map[string]any{"entity_type": entityType, "entity_ref": entityRef}',
          'payload := map[string]any{"entity_type": entityType, "ref": entityRef}')],
    ),
    Case(
        "links are read back from a route the store does not serve",
        [('resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/cards/%s/links", c.BaseURL, escapePathSegment(cardID)))',
          'resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/cards/%s/linkages", c.BaseURL, escapePathSegment(cardID)))')],
    ),

    # ---- card creation ----
    Case(
        "a card is posted to a route the store does not serve",
        [('url := fmt.Sprintf("%s/api/boards/%s/cards", c.BaseURL, escapePathSegment(c.BoardID))',
          'url := fmt.Sprintf("%s/api/boards/%s/items", c.BaseURL, escapePathSegment(c.BoardID))')],
    ),
    Case(
        "the new card's id is read from a key the store does not send",
        [('id, _ := created.Item["id"].(string)', 'id, _ := created.Item["card_id"].(string)')],
    ),
    Case(
        "a create response carrying no id is accepted, so the caller links against an empty card id",
        [('\tif id == "" {\n\t\treturn "", errors.New("create card: empty item id in response")\n\t}\n', "")],
    ),
    Case(
        "a refused card creation is reported as success",
        [('if resp.StatusCode != http.StatusCreated {\n\t\treturn "", statusErr("create card", resp)\n\t}',
          "_ = resp.StatusCode")],
    ),
    Case(
        "the card payload's column_id key drifts, so every card is created unplaced",
        [('"column_id": colID,', '"column": colID,')],
    ),
    Case(
        "the EnsureBoard-was-called guard is dropped, so a card is posted to the URL /api/boards//cards",
        [('\tif c.BoardID == "" {\n\t\treturn "", errors.New("kanbanclient: EnsureBoard not called")\n\t}\n', "")],
    ),
    Case(
        "the missing-column guard is dropped, so an unknown column name creates a card with a blank column",
        [('colID, ok := c.ColumnsByName[a.ColumnName]\n\tif !ok {\n\t\treturn "", fmt.Errorf("column %q missing on board", a.ColumnName)\n\t}',
          "colID := c.ColumnsByName[a.ColumnName]")],
    ),

    # ---- links, beyond the session one ----
    Case(
        "a refused link write is reported as success, so the card silently has no session",
        [('if resp.StatusCode != http.StatusCreated {\n\t\treturn statusErr("create card link", resp)\n\t}',
          "_ = resp.StatusCode")],
    ),
    Case(
        "the link's label is dropped, so nothing records which binary wrote the row",
        [('\tif label != "" {\n\t\tpayload["label"] = label\n\t}\n', "")],
    ),
    Case(
        "an unreadable link list is reported as an empty one, which reads as a card with no session",
        [('if resp.StatusCode != http.StatusOK {\n\t\treturn nil, statusErr("list card links", resp)\n\t}',
          "_ = resp.StatusCode")],
    ),

    # ---- board bootstrap ----
    # EnsureBoard is the only writer of auto_status, and auto_status is what
    # makes a column move rewrite the underlying noteboard item's status. A drift
    # here does not break the board; it silently changes what closing a card
    # DOES to the todo behind it.
    Case(
        "the Failed column is created with the Done column's auto_status, so an abandoned "
        "task's todo is marked done",
        [('case kanbanvocab.ColumnFailed:\n\t\t\ts := "archived"',
          'case kanbanvocab.ColumnFailed:\n\t\t\ts := "done"')],
    ),
    Case(
        "the Done column's auto_status drifts to a status noteboard does not accept",
        [('case kanbanvocab.ColumnDone:\n\t\t\ts := "done"',
          'case kanbanvocab.ColumnDone:\n\t\t\ts := "Done"')],
    ),
    Case(
        "EnsureBoard stops creating the columns it found missing, so it returns a half-built board",
        [("if _, ok := have[want]; ok {\n\t\t\tcontinue\n\t\t}",
          "if true {\n\t\t\tcontinue\n\t\t}")],
    ),
    Case(
        "EnsureBoard caches the columns it found rather than the ones it ensured",
        [("have[want] = col.ID", "_ = col")],
    ),
    Case(
        "a board is matched by position rather than by name, so every caller binds to whichever "
        "board the store listed first",
        [("if boards[i].Name == name {\n\t\t\treturn &boards[i], nil\n\t\t}",
          "_ = name\n\t\treturn &boards[i], nil")],
    ),
    Case(
        "a board that does not exist is created under a name of the store's choosing",
        [('body, _ := json.Marshal(map[string]any{\n\t\t"name":        name,',
          'body, _ := json.Marshal(map[string]any{\n\t\t"name":        "",\n\t\t"_name":       name,')],
    ),

    # ---- placements and moves ----
    Case(
        "a card is moved to a column with the position under a key the store does not read",
        [('"position":  position,', '"pos":       position,')],
    ),
    Case(
        "a refused move is reported as success, so the tick counts a card it never moved",
        [('if resp.StatusCode != http.StatusOK {\n\t\treturn MoveResult{}, statusErr("move card", resp)\n\t}',
          "_ = resp.StatusCode")],
    ),
    Case(
        "placements are read from a route the store does not serve",
        [('fmt.Sprintf("%s/api/cards/%s/placements", c.BaseURL, escapePathSegment(cardID))',
          'fmt.Sprintf("%s/api/cards/%s/boards", c.BaseURL, escapePathSegment(cardID))')],
    ),
    Case(
        "a card body is patched with POST, which the store routes somewhere else",
        [('req, _ := http.NewRequest("PATCH", fmt.Sprintf("%s/api/cards/%s", c.BaseURL, escapePathSegment(cardID)), bytes.NewReader(payload))',
          'req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/cards/%s", c.BaseURL, escapePathSegment(cardID)), bytes.NewReader(payload))')],
    ),
    Case(
        "a refused hold is reported as success, so work the user parked keeps being dispatched",
        [('if resp.StatusCode != http.StatusOK {\n\t\treturn statusErr("hold card", resp)\n\t}',
          "_ = resp.StatusCode")],
    ),

    # ---- entity tags and reverse lookups ----
    Case(
        "entity tags are read from a route the store does not serve, so every entity reads as untagged",
        [('resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/entities/%s/%s/tags",\n\t\tc.BaseURL, escapePathSegment(entityType), escapePathSegment(entityRef)))',
          'resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/entity/%s/%s/tags",\n\t\tc.BaseURL, escapePathSegment(entityType), escapePathSegment(entityRef)))')],
    ),
    Case(
        "a successful tag delete is reported as an error",
        [('if resp.StatusCode != http.StatusOK {\n\t\treturn statusErr("delete entity tag", resp)\n\t}',
          "if resp.StatusCode != http.StatusCreated {\n\t\treturn statusErr(\"delete entity tag\", resp)\n\t}")],
    ),
    Case(
        "the reverse card lookup reads a route the store does not serve, so no card is ever anybody's child",
        [('resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/entities/%s/%s/cards",',
          'resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/entities/%s/%s/links",')],
    ),

    # ---- transport ----
    Case(
        "the request timeout is dropped, so a wedged store hangs the whole tick instead of failing it",
        [("httpClient := &http.Client{Timeout: 10 * time.Second}",
          "httpClient := &http.Client{Timeout: 0 * time.Second}")],
    ),
    Case(
        "a base URL with a trailing slash stops being trimmed, so every request doubles the slash",
        [('BaseURL: strings.TrimRight(baseURL, "/"),', "BaseURL: baseURL,")],
    ),
    Case(
        "a store error loses the store's own explanation of it",
        [('return fmt.Errorf("kanban %s: %s — %s", op, resp.Status, strings.TrimSpace(string(raw)))',
          '_ = raw\n\treturn fmt.Errorf("kanban %s: %s", op, resp.Status)')],
    ),
]


if __name__ == "__main__":
    if "--prove-self-test" in sys.argv:
        sys.exit(prove_self_test())
    # `--self-test-only` is routed here as well as under `--guard-split`. It used
    # to be read only INSIDE guard_split(), so passing it on its own fell through
    # to a full scoring run — minutes of `go test` in answer to a request for a
    # sub-second check, and a 33/33 in answer to a question about the classifier.
    if "--guard-split" in sys.argv or "--self-test-only" in sys.argv:
        sys.exit(guard_split())
    sys.exit(score(TARGET, PACKAGES, CASES))
