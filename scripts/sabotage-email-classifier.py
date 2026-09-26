#!/usr/bin/env python3
"""Score cmd/email-classifier's tests by breaking the binary on purpose.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-email-classifier.py [--diffs] [--crosstable]
    python3 scripts/sabotage-email-classifier.py --guard-split
    python3 scripts/sabotage-email-classifier.py --self-test-only
    python3 scripts/sabotage-email-classifier.py --prove-self-test

`--guard-split` re-scores every case and reads WHY each CAUGHT row went red: an
assertion judging the mutated value, or a `t.Fatalf` reach guard tripping on a
setup step that never got as far as judging anything. That is card `3b6eba2a`
and it is the third scorer in this repo to get the split, after `kanbanclient`
(254th pass) and `schedulerjobs` (269th).

⚠️ **This is deliberately a hand-rolled marker list, not a fix to the shared
engine.** `scripts/sabotage.py` is untouched: card `55d41f34` owns the engine
port, the marker list is per-suite by nature, and the 254th's finding — that the
zero does not generalise, so the mode has to be run per scorer — is the reason
the split is worth hand-rolling a third time rather than waiting for the port.

## Two properties of THIS suite that the previous two scorers' matcher gets wrong

Both were measured against real failure lines from these four test files, not
imagined, and each one silently inflates the score in the same direction — an
assertion read as a guard, which turns a row that WAS scored into a row reported
as scoring nothing.

1. **A marker must anchor at the start of the MESSAGE, not match anywhere in the
   line.** The 254th and 269th both ask `any(marker in line)`. `text_test.go`
   guards a decode with `t.Fatalf("decode: %v", err)` and then asserts on the
   decoded shape with `t.Fatalf("filings did not decode: %+v", ...)`. The second
   line CONTAINS the first's marker. A substring test calls a real assertion a
   guard; a prefix test on the message after `file.go:NN: ` does not.

2. **The trailing-colon rule is not expressible as a prefix when the guard
   interpolates a value BEFORE its colon.** `mustParseDue` guards with
   `parseDueDate(%q): %v` while two assertions in the same file read
   `parseDueDate(%q) succeeded; ...` and `parseDueDate(%q) lost the time of
   day: %s`. No fixed prefix separates them — the raw date sits in the middle,
   and the colon that distinguishes a guard from prose lands AFTER it. So the
   marker has to be a pattern rather than a prefix, and `INTERPOLATING_GUARDS`
   below carries exactly the ones that need it. The 269th's colon discipline is
   right; a prefix is just too weak a shape to state it in here.

The colon discipline itself is asserted structurally over both lists in
`self_test()`, not through a fabricated failure line, for the 269th's reason: no
line this suite actually prints can redden a dropped colon, and a discipline no
case can redden is a comment rather than a rule.

## A THIRD verdict, which this suite produced and the previous two did not

`kanbanclient` and `schedulerjobs` both split CAUGHT rows two ways, into an
assertion firing and a reach guard tripping. One row here is neither:

    the flag set goes back to ExitOnError  ->  panic: unexpected call to
                                               os.Exit(0) during test

`-h` under `ExitOnError` calls `os.Exit(0)`, and Go's testing package panics
rather than let a test binary exit green half way through a run. The row is
CAUGHT, it is caught by `TestHelpIsDistinguishableFromABadArgument` — the test
named for exactly that mechanism — and **no assertion in the file ever runs**:
the process dies at the `-h` call on line 385, three lines above the first
`t.Fatal`. So there is no `file.go:NN:` line to read, and a message-based
classifier can only report "unreadable".

Reported as `runtime-fired`, and deliberately not folded into either neighbour.
Calling it guard-fired would understate coverage that is real and correctly
aimed; calling it assertion-fired would name a line that did not execute. It
does not fail the run, but it is printed with the panic that produced it — a
verdict nobody sees is a verdict that turns "0 guard-fired" into "every row was
an assertion".

Score under the split, taken 2026-08-17 by the 281st pass:
**30 rows, 29 CAUGHT, 0 guard-fired only, 1 runtime-fired, 0 unreadable**, and
the known-negative control still UNNOTICED.

Re-aimed 2026-09-26 (card `d55e0a3a`). e6bcf96 moved the flag defaults and the
environment wiring into the settings registry in internal/config, and 14
needles stopped matching, the known-positive control among them, so the scorer
had aborted on its first row since then. Eleven rows now aim at where each value
lives; three are deleted with a line saying why (-account, -auth-store-url,
envOr). Re-aiming found a real hole: TestATickWithNoBudgetIsRefused... passed
no board id, so validate refused the missing board and the -max guard could be
deleted with the test green. The test now names a board.

Score after the re-aim: **25/25 real mechanisms CAUGHT, both controls behaved,
exit 0.** Under the split: 26 CAUGHT rows, 0 guard-fired only, 0 runtime-fired,
**1 unreadable**, so `--guard-split` exits 1. The unreadable row is ExitOnError:
TestTheFlagsTheBoardReplacedAreGone now passes a removed flag before the `-h`
test runs, so the binary exits 2 on that bad argument and prints no failure line
and no panic. The 281st's runtime-fired verdict depended on `-h` being the first
bad input the binary met.

Fixed 2026-09-26 (card `fbb0f344`). Every test that hands parseFlags a bad
argument now calls it through parseFlagsInChildProcess, which runs it in a
re-executed copy of the test binary. An os.Exit under ExitOnError ends the child
only, and the parent reports it as a `flag_defaults_test.go:NN:` assertion in
the test that passed the argument. Under the split: **26 CAUGHT rows, 0
guard-fired only, 0 runtime-fired, 0 unreadable, exit 0.** The row no longer
depends on which bad-argument test runs first. RUNTIME_KILLS stays: any other
panic out of the code under test still needs it.

Why this binary is worth a scorer: it has the largest flag set of the 21 cmd
mains on this box (seventeen), it reads the environment in ten places, and its
loudest failures are not its worst ones. FIVE of the seventeen switches turn a
tick into a no-op — four early exits (-register, -merge, -backfill-due,
-dump-digest) and -dry-run, which classifies at full model cost and writes
nothing. Any of them shipping ON leaves a binary that runs every 15 minutes,
reads the board, logs a full pass and exits 0, which from outside is a quiet
mailbox. The sibling binaries scored before this one had one such switch each.

ONE target, and that is deliberate. The seam the 198th pass opened is
configuration: the flag set, the environment wiring, the registration payload
and the one guard. The tick's own logic lives in classify.go, apply.go,
priority.go, merge.go and text.go, each with its own tests, and none of it is
sabotaged here.

Case-writing rules inherited from the reminder-coordinator and autoworker plans:

  - Prefer a DRIFTED VALUE to a deletion. It orphans no variable, needs no
    second edit, and is the likelier real-world regression.
  - Read every applied diff against its label (`--diffs`). A row prints the name
    you gave it, not the edit you made.
  - An UNNOTICED row is not automatically a coverage hole. It can be a
    behavioural no-op, or a fake in the test file that swallows the value the
    mutation moved.

Score when filed by the 198th pass: 28/28 real mechanisms caught, both controls
behaved, exit 0.

That 28 is worth reading against the card that asked for this work, which
counted 17 flags and 12 environment reads. The flags were right and the
environment reads were not — there are ten call sites over nine distinct
variables, and one of the ten was a duplicate read of EMAIL_BOARD_ID inside
registerSchedulerJob that the seam removed. The rows the card could not see are
the registration payload (four) and the guard, which are configuration this
binary writes once per host and never reads back.

⚠️ `--crosstable` has NOT been taken for this binary, and no `unreddened`
declaration is made below, because an empty one would read as "measured and
found none" rather than "not measured". The next pass on this binary owns that.
It will be a large table: three of the four test files in this package cover
code no case here touches, so most of its columns are expected to be silent
against THIS plan and that is not a finding.

⚠️ Region this plan does NOT cover, declared rather than left to be
rediscovered: every case below edits configuration. collectUnfiled's page walk,
routeByAffinity, buildDigest, classifyAndApply's halving retry, the merge pass
and the priority scorer are NOT sabotaged here. That is the seam the 198th pass
opened and nothing more; it is not a statement that the rest is covered.
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

# The flag defaults and the environment wiring moved out of main.go into the
# settings registry at e6bcf96, so the two internal/config files that declare
# them are targets too. PACKAGES stays this binary's own: the rows ask whether
# ITS tests notice a drifted value, not whether internal/config's do.
TARGETS = [
    REPO / "cmd/email-classifier/main.go",
    REPO / "internal/config/jobs.go",
    REPO / "internal/config/settings.go",
]
PACKAGES = ["./cmd/email-classifier/"]


CASES = [
    # ---- controls ----
    Case(
        "CONTROL known-positive: a run with no board id is let through",
        [('if !c.ListVocabularies && c.BoardID == "" {', 'if false && c.BoardID == "" {')],
    ),
    Case(
        "CONTROL known-negative: reword the -mail-token usage string",
        [('"mailstack bearer token"', '"Bearer token presented to mailstack on every request."')],
        expected_unnoticed="flag usage text for a human reading -h. No test reads it, and nothing "
                           "downstream parses it. This control is what proves the rows below redden "
                           "on the VALUES that ship and not on the source lines carrying them. "
                           "Falsified by a test that asserts on fs.PrintDefaults() output.",
    ),

    # ---- the five switches that turn a tick into a no-op ----
    #
    # This family is why the binary was worth scoring. Each one leaves a process
    # that exits 0 having filed nothing, and nothing on this box distinguishes
    # that from a week with no mail.
    Case(
        "-register ships ON: every tick registers a cron job and exits before reading the board",
        [('fs.BoolVar(&c.RegisterJob, "register", false,', 'fs.BoolVar(&c.RegisterJob, "register", true,')],
    ),
    Case(
        "-dry-run ships ON: every tick pays the full model bill and writes nothing",
        [('fs.BoolVar(&c.DryRun, "dry-run", false,', 'fs.BoolVar(&c.DryRun, "dry-run", true,')],
    ),
    Case(
        "-dump-digest ships ON: the digest goes to stdout and the model is never called",
        [('fs.BoolVar(&c.DumpDigest, "dump-digest", false,', 'fs.BoolVar(&c.DumpDigest, "dump-digest", true,')],
    ),
    Case(
        "-merge ships ON: every tick runs the merge pass and returns, filing no new mail",
        [('fs.BoolVar(&c.MergePass, "merge", false,', 'fs.BoolVar(&c.MergePass, "merge", true,')],
    ),
    Case(
        "-backfill-due ships ON: the one-off deadline recovery runs every 15 minutes instead",
        [('fs.BoolVar(&c.BackfillDue, "backfill-due", false,', 'fs.BoolVar(&c.BackfillDue, "backfill-due", true,')],
    ),

    # ---- the two values that decide what a tick costs ----
    Case(
        "-model drifts to the cheap model the sibling classifiers use: quality falls, nothing fails",
        [('"model", settings.String(schedulerconfig.SettingModel)', '"model", "claude-haiku-4-5"')],
    ),
    Case(
        "DefaultEmailClassifierModel itself drifts to a more expensive model: same work, several times the bill",
        [('DefaultEmailClassifierModel = "claude-sonnet-4-6"', 'DefaultEmailClassifierModel = "claude-opus-4-1"')],
    ),
    Case(
        "-max drifts 50 -> 5: the backlog drains ten times slower and no tick ever says so",
        [('fs.IntVar(&c.MaxEmails, "max", maxEmailsPerTick,', 'fs.IntVar(&c.MaxEmails, "max", 5,')],
    ),
    Case(
        "maxEmailsPerTick itself drifts 50 -> 200: one tick's provider fetches and model spend quadruple",
        [("maxEmailsPerTick = 50", "maxEmailsPerTick = 200")],
    ),

    # ---- the remaining flag defaults ----
    Case(
        "-backfill ships ON: every tick walks the whole archive instead of stopping at the caught-up page",
        [('fs.BoolVar(&c.Backfill, "backfill", false,', 'fs.BoolVar(&c.Backfill, "backfill", true,')],
    ),
    # "-account ships pinned to one mailbox" was deleted 2026-09-26: the flag is
    # gone and the board's classifier.mail_account_ids says which accounts a run
    # reads. TestTheFlagsTheBoardReplacedAreGone pins that it stays gone.
    Case(
        "-board-id ships with a value: every host pins the same board, which exists on none of them",
        [('"board-id", settings.String(schedulerconfig.SettingEmailBoardID)', '"board-id", "email-board"')],
    ),
    Case(
        "-kanban-url points at noteboard: the board read that every other step depends on",
        [('DefaultKanbanStoreURL    = "http://localhost:8305"', 'DefaultKanbanStoreURL    = "http://localhost:8191"')],
    ),
    Case(
        "-mail-url points at kanban-store: no message is ever fetched",
        [('DefaultMailstackURL      = "http://localhost:8195"', 'DefaultMailstackURL      = "http://localhost:8305"')],
    ),
    Case(
        "-bridge-url points at auth-store: the agent-ownership gate cannot resolve a session",
        [('DefaultBridgeURL    = "http://localhost:8160"', 'DefaultBridgeURL    = "http://localhost:8303"')],
    ),
    # "-auth-store-url points at llm-bridge" was deleted 2026-09-26: the binary
    # no longer resolves a provider key itself. Model calls go through
    # llm-bridge-server, so there is no auth-store address left to drift.

    # ---- the wiring: nine variables, seven of them interchangeable strings ----
    Case(
        "the mailstack token is read from the kanban service token: mailstack answers every fetch 401",
        [('mailToken := settings.String(schedulerconfig.SettingMailstackToken)',
          'mailToken := settings.String(schedulerconfig.SettingKanbanStoreServiceToken)')],
    ),
    Case(
        "MAILSTACK_URL is read into the kanban field: the two endpoints trade places",
        [('"kanban-url", settings.String(schedulerconfig.SettingKanbanStoreURL)',
          '"kanban-url", settings.String(schedulerconfig.SettingMailstackURL)')],
    ),
    Case(
        "the board id is read from the wrong variable: a pinned board silently unpins",
        # Both edits, or the registry refuses the old name as a misspelling under
        # the owned prefix and the row fires only the parse guard. Renamed with
        # its prefix, EMAIL_BOARD_ID is simply ignored, which is the silent case.
        [('Key: SettingEmailBoardID, EnvironmentVariable: "EMAIL_BOARD_ID"', 'Key: SettingEmailBoardID, EnvironmentVariable: "BOARD_ID"'),
         ('OwnedEnvironmentVariablePrefixes: []string{"EMAIL_CLASSIFIER_", "EMAIL_BOARD_ID"}',
          'OwnedEnvironmentVariablePrefixes: []string{"EMAIL_CLASSIFIER_", "BOARD_ID"}')],
    ),
    # "envOr prefers its fallback over the environment" was deleted 2026-09-26:
    # envOr is gone, and the lookup that replaced it lives in llm-bridge's
    # servicesettings, outside this repo. TestAnUnsetVariableFallsBackAndASetOneDoesNot
    # still pins the behaviour from this side.
    Case(
        "SCHEDULER_URL falls back to the kanban port: -register posts the job to the wrong service",
        [('DefaultSchedulerURL      = "http://localhost:8092"', 'DefaultSchedulerURL      = "http://localhost:8305"')],
    ),
    Case(
        "the registered command loses its bin segment: the job scheduler creates cannot exec",
        [('filepath.Join(homeDirectory, "bin", "email-classifier")',
          'filepath.Join(homeDirectory, "email-classifier")')],
    ),

    # ---- the registration payload, written once per host and never re-read ----
    Case(
        "the job registers with no timeout: the scheduler's 5-minute default kills every long tick mid-write",
        [("schedulerJobTimeoutSeconds = 900", "schedulerJobTimeoutSeconds = 0")],
    ),
    Case(
        "the cadence drifts */15 -> */5: three times the provider round trips and three times the model spend",
        [('schedulerJobSchedule       = "*/15 * * * *"', 'schedulerJobSchedule       = "*/5 * * * *"')],
    ),
    Case(
        "the job name drifts: -register no longer recognises its own job and registers a second one",
        [('schedulerJobName           = "email-classifier-tick"', 'schedulerJobName           = "email-classifier"')],
    ),
    Case(
        "registeredCommand drops the pinned board id: the job resolves by name and a rename forks the board",
        [('	return binaryPath + " -board-id " + boardID', "	return binaryPath")],
    ),

    # ---- the one guard, and the property that makes the seam usable ----
    Case(
        "the empty-budget guard is dead: -max=0 files nothing and still reports success",
        [("if c.MaxEmails < 1 {", "if false {")],
    ),
    Case(
        "the flag set goes back to ExitOnError: a bad argument takes the test binary down with it",
        [('flag.NewFlagSet("email-classifier", flag.ContinueOnError)',
          'flag.NewFlagSet("email-classifier", flag.ExitOnError)')],
    ),
]


# ---------------------------------------------------------------------------
# The guard/assertion split. Card `3b6eba2a`.
# ---------------------------------------------------------------------------
#
# Every marker below is a MESSAGE PREFIX for a `t.Fatalf` that guards a setup
# step: a parse, a decode, a fixture the test needs before it can judge
# anything. When one of these is the only failure line a case produced, the
# suite never reached the assertion that names the mechanism, so the CAUGHT in
# the default table is a number for something nobody measured.
#
# Each keeps its trailing ": " on purpose. It is the whole distinction between
# the guard, which stops describing and starts printing an error, and an
# assertion phrased in the same words — and this suite has that collision twice
# over (see the module docstring).
REACH_GUARDS = [
    # flag_defaults_test.go — nine sites parse an empty command line purely to
    # get at cfg. A parse that fails says the flag set is broken, not that the
    # default under test drifted.
    "parsing an empty command line: ",
    # The same guard where the test passes arguments it names in prose instead.
    "parsing: ",
    # TestATickWithNoBudgetIsRefusedRatherThanReportingSuccess, both iterations.
    # Written out rather than truncated to "parsing " — that shorter form is a
    # prefix of the two above and of any later `parsing X` guard, which is how a
    # marker list stops being a list of what actually prints.
    "parsing -max=0: ",
    "parsing -max=-1: ",
    # TestParsingTwiceDoesNotPanic. Both parses are the fixture; the property is
    # that neither panics, and an error from either is the guard, not the finding.
    "first parse: ",
    "second parse: ",
    # priority_test.go's zoned setup, guarding the offset assertion below it.
    "zoned parse: ",
    # text_test.go decodes the model's report before judging its shape.
    "decode: ",
]

# Guards whose message interpolates a value BEFORE the colon, so no fixed prefix
# can carry the colon discipline. Anchored at the start of the message for the
# same reason the plain markers are, and each one keeps a literal "): " so it
# cannot match the two assertions in the same file that open with the same call.
INTERPOLATING_GUARDS = [
    # mustParseDue and TestNaiveDatetimeIsReadInLocalZone.
    # Guard:     parseDueDate("2026-08-14T19:30:00"): parsing time ...
    # Assertion: parseDueDate("2026-08-14T19:30:00") lost the time of day: ...
    # Assertion: parseDueDate("garbage") succeeded; a dropped deadline must be loud
    re.compile(r'parseDueDate\([^)]*\): '),
]

# Lines that mean the Go testing RUNTIME killed the run rather than a test
# reporting a failure. A row caught this way prints no `file.go:NN:` line at all,
# so a message-based classifier can only say "unreadable" — and this suite
# produces one for real, which is why the third verdict exists rather than being
# hypothesised.
#
# It is a THIRD answer and not a synonym for either of the other two. The row is
# genuinely caught, and caught by the test named for the mechanism, so calling it
# guard-fired would understate real coverage. But no assertion in the file ran,
# so calling it assertion-fired is a lie about which line did the work. Naming it
# is the only honest option; laundering it into either neighbour is not.
RUNTIME_KILLS = [
    # `-h` under ExitOnError calls os.Exit(0), and Go's testing package panics
    # on that rather than letting a test binary exit green half way through.
    "panic: unexpected call to os.Exit(",
    # Any other panic out of the code under test. Also real coverage, also
    # nothing to do with an assertion.
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
# substring test reads "filings did not decode: {...}" as the "decode: " guard.
#
# The file part is NOT narrowed to `_test\.go` — that is the 269th pass's
# correction, and it applies here too. A helper that forgets `t.Helper()` reports
# against its own file, and a row whose only visible lines were guards would then
# read as guard-fired where an assertion fired.
FAILURE_LINE = re.compile(r"^\s*[\w./-]+\.go:\d+:\s(?P<message>.*)$")


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

    startswith, not `in`: see property 1 in the module docstring. An assertion
    that merely ends in a guard's wording is an assertion.
    """
    if any(message.startswith(marker) for marker in REACH_GUARDS):
        return True
    return any(pattern.match(message) for pattern in INTERPOLATING_GUARDS)


def guard_only(messages):
    """True when every failure message is a reach guard, and there is at least one.

    Pure, and pure on purpose: this is the whole finding, so it has to be the
    thing the self-test drives directly. `all()` over an empty list is True, and
    a row with no failure lines at all is not a guard-fired row — it is a run
    whose output this scorer could not read, which is a different answer and must
    not be laundered into this one. This suite produces that case for real: the
    ExitOnError row takes the test binary down with os.Exit(2) and prints no
    failure line at all.
    """
    return bool(messages) and all(is_reach_guard(m) for m in messages)


SelfSabotage = namedtuple("SelfSabotage", "anchor replacement expect what")

# The anchors for the self-sabotages, kept here rather than in the prose because
# prose cannot hold an anchor: the docstring is the first thing in the file, so a
# quoted anchor is shadowed by its own documentation and a first-match replace
# edits the sentence instead of the code. That is card `bb6e5601`, filed by the
# 269th pass after exactly that produced a green run that had measured a comment.
#
# Each anchor is kept long enough to occur EXACTLY ONCE outside this table and
# NOWHERE in the module docstring. check_self_sabotage_anchors() asserts both.
SELF_SABOTAGES = [
    SelfSabotage(
        "REACH_GUARDS = [\n    # flag_defaults_test.go",
        "REACH_GUARDS = []\n_MARKERS_THE_SABOTAGE_TOOK_AWAY = [\n"
        "    # flag_defaults_test.go",
        "RED",
        "empty the plain marker list: every guard-only row must fail",
    ),
    SelfSabotage(
        "INTERPOLATING_GUARDS = [\n    # mustParseDue",
        "INTERPOLATING_GUARDS = []\n_PATTERNS_THE_SABOTAGE_TOOK_AWAY = [\n"
        "    # mustParseDue",
        "RED",
        "empty the interpolating pattern list: the parseDueDate guard row must fail",
    ),
    SelfSabotage(
        "    if any(message.startswith(marker) for marker in REACH_GUARDS):\n",
        "    if any(marker in message for marker in REACH_GUARDS):\n",
        "RED",
        "match anywhere in the message instead of at its start: the "
        "'filings did not decode' assertion must stop being an assertion",
    ),
    SelfSabotage(
        r"    re.compile(r'parseDueDate\([^)]*\): ')," + "\n",
        r"    re.compile(r'parseDueDate\([^)]*\)')," + "\n",
        "RED",
        "drop the colon from the interpolating pattern: the two parseDueDate "
        "assertions must stop being assertions",
    ),
    SelfSabotage(
        '    "first parse: ",\n',
        '    "first parse",\n',
        "RED",
        "drop a trailing colon-and-space: the marker-shape check must fail",
    ),
    SelfSabotage(
        "RUNTIME_KILLS = [\n    # `-h` under ExitOnError",
        "RUNTIME_KILLS = []\n_KILLS_THE_SABOTAGE_TOOK_AWAY = [\n"
        "    # `-h` under ExitOnError",
        "RED",
        "empty the runtime-kill list: the os.Exit-during-test row must stop being "
        "recognised, which would report real coverage as unreadable",
    ),
    SelfSabotage(
        "def guard_only(messages):\n",
        "def guard_only(messages):\n    return True\n",
        "RED",
        "return a constant true: the assertion rows and the empty-input row must fail",
    ),
    SelfSabotage(
        "# text_test.go decodes the model's report before judging its shape.",
        "# text_test.go decodes the model report before judging its shape.",
        "GREEN",
        "CONTROL: reword a comment inside the marker list; nothing may change",
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

    Two properties, the second of which is card `bb6e5601`:

      * exactly one occurrence outside the table, so a replace cannot pick a site
        nobody chose — the 261st pass's uniqueness rule;
      * no occurrence in the module docstring, which comes FIRST, so a first-match
        replace cannot edit the prose and print a control that behaved.
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


def self_test():
    """Prove the classifier returns BOTH verdicts, for the right reasons.

    Returns problems; empty means sound. Every failure line below is real output
    from this package's four test files, taken from their `t.Fatalf` and
    `t.Errorf` format strings rather than invented — a self-test written against
    lines the suite does not print measures the author's idea of the format.
    """
    problems = []

    def check(label, messages, want):
        got = guard_only(messages)
        if got != want:
            problems.append("%s: guard_only=%s, want %s" % (label, got, want))

    check("a single parse guard",
          ["parsing an empty command line: flag provided but not defined: -board-id"],
          True)
    check("two parse guards from different tests",
          ["parsing an empty command line: bad flag",
           "first parse: bad flag"],
          True)
    check("a real assertion on a drifted default",
          ['-max default: got 5, want 50'],
          False)
    check("one assertion alongside a guard — the mechanism WAS scored",
          ["parsing an empty command line: bad flag",
           '-max default: got 5, want 50'],
          False)
    check("no failure lines at all is not a guard verdict", [], False)

    # Property 1, the reason markers anchor at the start of the message. Both
    # lines below come from text_test.go and the second CONTAINS the first's
    # marker; a `marker in message` test calls the assertion a guard.
    check("the decode guard itself", ["decode: unexpected end of JSON input"], True)
    check("an assertion that ends in the decode guard's wording",
          ["filings did not decode: []main.filing(nil)"],
          False)

    # Property 2, the reason INTERPOLATING_GUARDS exists. The guard interpolates
    # the raw date before its colon; the two assertions open with the same call.
    check("the parseDueDate guard, value interpolated before the colon",
          ['parseDueDate("2026-08-14T19:30:00"): parsing time "": cannot parse'],
          True)
    check("an assertion opening with the same call, no colon after the paren",
          ['parseDueDate("2026-08-14") lost the time of day: 2026-08-14 00:00:00 +0000 UTC'],
          False)
    check("the other assertion opening with the same call",
          ['parseDueDate("not a date") succeeded; a dropped deadline must be loud'],
          False)

    # An assertion whose subject is the configuration a guard also parses. This
    # is the §B shape — ordinary English naming a guarded operation — and it is
    # what the trailing ": " on "parsing: " is for.
    check("an assertion about the shipped configuration, not a parse guard",
          ["the configuration this binary ships with does not validate: -max must be positive"],
          False)

    # The colon discipline, asserted structurally rather than through a fixture.
    # No failure line this suite prints can redden a dropped colon — the markers
    # would still match every line they match today — so the rule is stated
    # directly. A discipline nothing can redden is a comment, not a rule.
    for marker in REACH_GUARDS:
        if not marker.endswith(": "):
            problems.append("marker %r has lost its trailing ': ', so it now matches "
                            "any assertion phrased in the same words" % marker)
    for pattern in INTERPOLATING_GUARDS:
        if not pattern.pattern.endswith(r"\): "):
            problems.append("pattern %r does not end at a '): ', so it matches the "
                            "assertions that open with the same call"
                            % pattern.pattern)

    # The third verdict, driven on real output from the ExitOnError row rather
    # than on a fabricated panic — the row that forced this verdict to exist.
    killed = runtime_kill(
        "--- FAIL: TestHelpIsDistinguishableFromABadArgument (0.00s)\n"
        "panic: unexpected call to os.Exit(0) during test [recovered, repanicked]\n"
        "\ngoroutine 20 [running]:\n"
    )
    if not killed.startswith("panic: unexpected call to os.Exit("):
        problems.append("runtime_kill() read %r from a run the testing package "
                        "killed for calling os.Exit during a test" % killed)
    # And the other direction, which is the one that would quietly turn a
    # guard-fired row into a runtime-fired one: an ordinary reported failure is
    # not a runtime kill, whatever it says.
    if runtime_kill("--- FAIL: TestTheDefaultConfigurationValidates (0.01s)\n"
                    "    flag_defaults_test.go:313: parsing an empty command line: bad flag\n"
                    "FAIL\n"):
        problems.append("runtime_kill() called an ordinary reported failure a "
                        "runtime kill, which would launder a guard-fired row")

    # And the reader itself: the location half must be stripped, non-failure
    # lines dropped, and a helper's own .go file read rather than skipped.
    parsed = failure_messages(
        "--- FAIL: TestTheDefaultConfigurationValidates (0.01s)\n"
        "    flag_defaults_test.go:313: parsing an empty command line: bad flag\n"
        "    priority.go:88: a helper that forgot t.Helper()\n"
        "    not a failure line\n"
        "FAIL\tgithub.com/slava/scheduler/cmd/email-classifier\t0.02s\n"
    )
    if parsed != ["parsing an empty command line: bad flag",
                  "a helper that forgot t.Helper()"]:
        problems.append("failure_messages() read %r" % (parsed,))

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

    Status: 0 when each row reaches the verdict it claims. This is what makes the
    classifier's soundness a measurement rather than a story about a run somebody
    once did — and the 269th's third correction is why it exists at all: a
    self-test proves the FUNCTION, and only this proves the MODE.

    It restores this file with `git checkout`, so it refuses to start on an
    uncommitted one: a restore-from-git harness over unsaved work is a delete.

    ⚠️ A sabotaged file that does not PARSE exits non-zero and reads as RED, which
    is a SyntaxError grading itself as a caught mutation. Every row is parsed
    before it is run and reported BROKEN, never RED, when it will not compile.
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
            [sys.executable, str(source_file), "--self-test-only", "--under-sabotage"],
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

    Status: 0 when the classifier is sound and no row is guard-fired only,
    1 otherwise. A guard-fired row is a finding, not a footnote — it is a case
    whose CAUGHT in the default table is a number for something nobody measured.
    """
    problems = self_test()
    # The sabotage procedure is an instrument too, and the one here whose failure
    # mode is a GREEN run — so it gates the report exactly as the classifier does.
    #
    # Skipped under `--under-sabotage`, which `--prove-self-test` passes to its
    # children: this check judges the file AT REST, and under a sabotage every row
    # would report its own anchor missing, so the control could never be green.
    if "--under-sabotage" not in sys.argv:
        problems.extend(check_self_sabotage_anchors())
    if problems:
        print("SELF-TEST FAILED — the classifier cannot be trusted to report anything:")
        for p in problems:
            print("  ⚠️  " + p)
        return 1
    print("self-test: guard_only() returns both verdicts for the right reasons\n")
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
    # Printed, never swallowed. A verdict that does not fail the run still has to
    # be visible, or "0 guard-fired" reads as "every row was an assertion".
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
