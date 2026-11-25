#!/usr/bin/env python3
"""Score cmd/scheduler's tests by breaking the daemon on purpose.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per boot
decision the suite is meant to pin.

    python3 scripts/sabotage-cmd-scheduler.py [--diffs|--crosstable]

Ported 2026-09-25 from test/the-three-separating-cases (22f40695, da0da720,
ef5262cf, 16da9855) onto main, whose daemon had since moved its configuration
into internal/config (servicesettings), begun publishing the runs startup
reconciliation closes, and decided that a bus that is down stops the boot.
Dropped with the code they broke: the four default-drift cases, the four
environment-key cases, the token-fallback case and both envOr cases.
cmd/scheduler has no configFromEnv or envOr any more, and internal/config's
own tests pin those values. Dropped as main's decision rather than a defect:
"the bus handler is sent to dial a bus this process could not reach" (that is
what 6be744c chose) and "a box with no NATS no longer boots" (it does not boot
on main either; the handler stops it one step later). Added: the mirror of the
first (the handler handed an empty URL when this process could not connect),
and publishInterruptedRuns's cases.

Measured 2026-09-25 on the ported tree: score 36/36, both controls behaved.
--crosstable 37 cases x 22 tests, 0 reddened by no case (its first run found
TestPublishInterruptedRunsSaysNothingWhenNothingWasStranded silent; the
phantom-finish case closes it). --guard-split 37 CAUGHT, 0 guard-fired only,
2 runtime-fired (nil-pointer panics: the swallowed bus-handler build and the
unguarded Close), 0 unreadable; --self-test-only passes.
"""

import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import (  # noqa: E402
    REPO, Case, _apply_case, _run_tests, _sabotage_session, score)

TARGET = REPO / "cmd/scheduler/main.go"
PACKAGES = ["./cmd/scheduler/"]

CONNECT = "\tconn, err := d.connectNATS(cfg.NATSURL, natsClientName)"
CONNECT_WARNING = "\t\td.logf(\"warning: NATS connect failed: %v\", err)\n\t\tconn = nil"
BUS_HANDLER = "busHandler, err := d.newBusHandler(cfg.NATSURL, store, scheduler, logger)"


CASES = [
    # ---- controls ----
    Case(
        "CONTROL known-positive: the run returns before it boots anything",
        [(CONNECT,
          "\tif cfg.Port != 0 {\n\t\treturn fmt.Errorf(\"sabotage\")\n\t}\n" + CONNECT)],
    ),
    Case(
        "CONTROL known-negative: reword the agent-executor operator log line",
        [('d.logf("agent executor configured (NATS)")',
          'd.logf("agent jobs will run over the bus")')],
        expected_unnoticed="an operator log line no test reads; the suite asserts that the "
                           "executor was BUILT, not what was said about it",
    ),

    # ---- values another program reads ----
    Case(
        "the name this process announces on the bus drifts",
        [('const natsClientName = "scheduler"', 'const natsClientName = "scheduler-daemon"')],
    ),
    Case(
        "the token never reaches the API, so every job mutation is unauthenticated",
        [("srv := api.New(store, scheduler, cfg.Token, settings)",
          'srv := api.New(store, scheduler, "", settings)')],
    ),
    Case(
        "the listen address loses its colon, so the port is read as a host",
        [('addr := fmt.Sprintf(":%d", cfg.Port)', 'addr := fmt.Sprintf("%d", cfg.Port)')],
    ),
    # Every closer still fires, so the post-shutdown tail is untouched and
    # TestRunScheduler_ServesTheConfiguredPortAndShutsDownCleanly stays green;
    # only TestRunScheduler_ShutdownClosesTheBusHandlerAndTheConnectionOnce reads
    # the line. The structured logger's "shutting_down" record is deliberately
    # left in place — the mutation is the operator-facing half going quiet, not
    # the shutdown itself.
    Case(
        "the shutdown is not reported to the operator log",
        [("\t\td.logf(\"shutting down\")\n", "")],
    ),

    # ---- a failed bus connection ----
    Case(
        "a connection handed back alongside an error is used anyway",
        [(CONNECT_WARNING, "\t\td.logf(\"warning: NATS connect failed: %v\", err)")],
    ),
    # The failed connect is only a warning at this site, so the operator log line
    # is the whole signal that it happened here rather than at the bus handler.
    # Only the failed-connection test asserts the line;
    # TestRunScheduler_AConnectionReturnedWithAnErrorIsNotUsed walks the same
    # branch and does not, which is the containment this separates.
    Case(
        "a failed bus connection is not reported to the operator log",
        [(CONNECT_WARNING, "\t\tconn = nil")],
    ),
    Case(
        "an agent executor is wired with no bus behind it",
        [("\tif conn != nil {\n\t\tscheduler.SetAgentExecutor(d.newAgentExecutor())",
          "\tif true {\n\t\tscheduler.SetAgentExecutor(d.newAgentExecutor())")],
    ),
    Case(
        "no agent executor is ever wired, so every agent job is a no-op",
        [("\t\tscheduler.SetAgentExecutor(d.newAgentExecutor())\n", "")],
    ),
    # The branch this was ported from made the opposite choice: hand the handler
    # an empty URL when this process could not connect, so a NATS-less box booted
    # with no bus. Main's 6be744c chose to stop the boot instead, and this is the
    # edit that would silently undo that.
    Case(
        "the bus handler is handed an empty URL when this process could not connect, "
        "so a box whose bus is down boots on with no bus commands",
        [("\t" + BUS_HANDLER,
          "\tnatsURLForBus := cfg.NATSURL\n\tif conn == nil {\n\t\tnatsURLForBus = \"\"\n\t}\n"
          "\tbusHandler, err := d.newBusHandler(natsURLForBus, store, scheduler, logger)")],
    ),
    Case(
        "the bus handler never gets a URL, so a connected box still has no bus commands",
        [(BUS_HANDLER, 'busHandler, err := d.newBusHandler("", store, scheduler, logger)')],
    ),

    # ---- what runs before the first tick ----
    Case(
        "stranded runs are never reconciled, so they read as live duplicates forever",
        [("interruptedRuns, err := store.ReconcileOrphanedRuns()",
          "interruptedRuns, err := []db.InterruptedRun(nil), error(nil)")],
    ),
    Case(
        "a failed reconciliation is swallowed and the boot continues",
        [('return fmt.Errorf("reconcile orphaned runs: %w", err)', "interruptedRuns = nil")],
    ),
    Case(
        "the reconciliation is reported on a boot that reconciled nothing",
        [("\tif len(interruptedRuns) > 0 {", "\tif true {")],
    ),
    Case(
        "the runs reconciliation closed are never published, so a killed run "
        "exists only as a row in SQLite",
        [("\tpublishInterruptedRuns(logger, interruptedRuns)\n", "")],
    ),

    # ---- publishInterruptedRuns ----
    Case(
        "a reconciled run is published as a success, so a killed run files at info",
        [("r.RunID, runstatus.Interrupted,", "r.RunID, runstatus.Success,")],
    ),
    Case(
        "a reconciled run is published with no output",
        [("\t\t\tr.Output, r.FinishedAt.Sub(r.StartedAt))", "\t\t\t\"\", r.FinishedAt.Sub(r.StartedAt))")],
    ),
    Case(
        "a reconciled run is published with no duration",
        [("\t\t\tr.Output, r.FinishedAt.Sub(r.StartedAt))", "\t\t\tr.Output, 0)")],
    ),
    # Aimed at TestPublishInterruptedRunsSaysNothingWhenNothingWasStranded, which
    # the first cross-table of this port found reddened by no case: every other
    # publishInterruptedRuns case drops or bends a finish, none invents one.
    Case(
        "a boot with nothing stranded publishes a finish anyway, so every clean "
        "restart files an error",
        [("\tfor _, r := range runs {",
          "\tif len(runs) == 0 {\n\t\tlogger.LogJobFinish(0, \"\", \"\", 0, runstatus.Interrupted, \"\", 0)\n\t}\n"
          "\tfor _, r := range runs {")],
    ),
    Case(
        "only the first reconciled run is published",
        [("\t\t\tr.Output, r.FinishedAt.Sub(r.StartedAt))\n\t}",
          "\t\t\tr.Output, r.FinishedAt.Sub(r.StartedAt))\n\t\tbreak\n\t}")],
    ),

    # ---- failures that must stop a boot ----
    Case(
        "a database that will not open is swallowed and the boot reports success",
        [('return fmt.Errorf("db: %w", err)', "return nil")],
    ),
    Case(
        "the database error drops the reason it would not open",
        [('return fmt.Errorf("db: %w", err)', 'return fmt.Errorf("db: could not open")')],
    ),
    Case(
        "a bus handler that will not build is swallowed",
        [('return fmt.Errorf("bus: %w", err)', "busHandler = nil")],
    ),
    Case(
        "a scheduler that will not start is swallowed, so no job ever runs",
        [('return fmt.Errorf("cron: %w", err)', "_ = err")],
    ),
    Case(
        "a serving failure is reported as an ordinary shutdown",
        [('return fmt.Errorf("http: %w", err)', "return nil")],
    ),
    Case(
        "the serving error drops the reason the port could not be bound",
        [('return fmt.Errorf("http: %w", err)', 'return fmt.Errorf("http: server stopped")')],
    ),

    # ---- failures that must NOT stop a boot ----
    Case(
        "a bus handler that cannot subscribe takes the whole daemon down with it",
        [('d.logf("warning: bus handler failed to start: %v", err)',
          'return fmt.Errorf("bus start: %w", err)')],
    ),
    Case(
        "the bus handler is never started, so no bus command is ever answered",
        [("\tif err := busHandler.Start(); err != nil {", "\tif err := error(nil); err != nil {")],
    ),

    # ---- shutdown ----
    Case(
        "an ordinary shutdown is reported as a failure",
        [('logger.LogSchedulerLifecycle("shutting_down")\n\t\treturn nil',
          'logger.LogSchedulerLifecycle("shutting_down")\n\t\treturn fmt.Errorf("interrupted")')],
    ),
    Case(
        "the bus handler is left open at shutdown",
        [("\t\tbusHandler.Close()\n", "")],
    ),
    Case(
        "the NATS connection is left open at shutdown",
        [("\t\tif conn != nil {\n\t\t\tconn.Close()\n\t\t}\n", "")],
    ),
    Case(
        "shutdown closes a connection that was never opened",
        [("\t\tif conn != nil {\n\t\t\tconn.Close()\n\t\t}", "\t\tconn.Close()")],
    ),
    Case(
        "the run stops waiting for a signal and returns as soon as it has booted",
        [("\tcase <-d.signals:", "\tdefault:")],
    ),

    # ---- tests only the known-positive control would otherwise redden ----
    #
    # `--crosstable` asks "is this test reddened by NO case". A test reddened
    # only by the known-positive control — the edit that stops the binary
    # booting at all — passes that check while nothing aimed at any mechanism
    # touches it. These cases are one per such test (ef5262cf, 16da9855).
    Case(
        # Aimed at TestRunScheduler_ConnectsToTheBusExactlyOnce. The daemon's
        # connect is one line and nothing else calls it, so the only edit that
        # opens a second connection is one that adds a second call — here on the
        # logger path, which is where a real one would go: newLogger closes over
        # the client connectNATS assigns, so "the logger has no client" invites
        # exactly this repair.
        #
        # ⚠️ It reddens TestRunScheduler_AnnouncesItselfOnTheBusByName too, and
        # that is not avoidable: the harness appends to connectNames on every
        # connect, so the name test's `len(names) != 1` tracks the connect count
        # exactly. No mutation separates them, so no case can.
        "the daemon opens a second bus connection on the logger path, doubling "
        "every subscription the bus handler makes",
        [("\tlogger := d.newLogger()\n",
          "\tif _, err := d.connectNATS(cfg.NATSURL, natsClientName); err != nil {\n"
          "\t\td.logf(\"warning: NATS reconnect failed: %v\", err)\n"
          "\t}\n"
          "\tlogger := d.newLogger()\n")],
    ),
    Case(
        # Aimed at TestRunScheduler_TheBoundHandlerIsTheJobAPI. Neutralised
        # rather than deleted — `srv` still takes its logger and `http` is still
        # imported, so this scores a suite rather than a build.
        "a bare mux is bound instead of the job API, so the port answers and "
        "every route 404s",
        [("served <- d.listenAndServe(addr, srv)",
          "served <- d.listenAndServe(addr, http.NewServeMux())")],
    ),
    Case(
        # Aimed at TestRunScheduler_TheLoggerIsBuiltAfterTheBusConnection.
        # productionDeps's newLogger closes over a *natsbus.Client that only
        # connectNATS assigns. Build the logger first and it is bound to a nil
        # client; Logger.publish returns silently on one, so the daemon's whole
        # lifecycle log leaves the bus with no error raised anywhere.
        "the logger is built before the bus connection, so it closes over a nil "
        "client and every lifecycle line is dropped in silence",
        [(CONNECT + "\n\tif err != nil {\n" + CONNECT_WARNING + "\n\t}\n\n\tlogger := d.newLogger()\n",
          "\tlogger := d.newLogger()\n\n" + CONNECT + "\n\tif err != nil {\n" + CONNECT_WARNING + "\n\t}\n")],
    ),

    # ---- the wiring line with no local effect ----
    Case(
        # The bound API can run a job, and the run result then arrives at the
        # handler this binary wired. A seam with no local effect is still
        # observable at the other end of it.
        "the cron loop is left with no publisher, so no run result reaches the bus",
        [("\tscheduler.SetBusHandler(busHandler)\n", "")],
    ),
]


# ─────────────────────────── the guard/assertion split ───────────────────────
#
# Ported with the case list from 5392fd68 and a9d898fa
# (test/the-cmd-scheduler-scorer-tells-a-guard-from-an-assertion). Re-keyed to
# the renamed failed-connection test and to storeBrokenBy.
#
# Card 3b6eba2a: every CAUGHT row above is ambiguous until something can say
# whether an ASSERTION went red or a setup REACH GUARD tripped first. A row
# caught by a guard alone scored nothing — the mutation broke the setup, the
# test never reached the property it is named for, and the default table counts
# it as coverage.
#
# ⭐ THIS SUITE NEEDED A SECOND CHANNEL, AND THAT IS THIS FILE'S FINDING.
#
# The four scorers split before this one (254th, 269th, 281st, 296th passes)
# classify a failure by its MESSAGE: a marker is the fixed head of the guard's
# format string. That works when the guard prints fixed text. **Fifteen of this
# suite's eighty-two failure calls are a bare `t.Fatal(err)`** — the message is
# whatever the error says, composed at runtime out of the code under test and
# the standard library. There is no fixed text to key on, at any length, and no
# marker list can ever name them.
#
# That is card cc1ac87f's population measured on one suite: 6.5% of the fleet's
# failure calls have no discriminating fixed text, and here it is 18%. Its
# conclusion is that the call site is the only discriminator left, reached
# through a DECLARATION CHANNEL checked in both directions. So:
#
#   channel 1  REACH_GUARD_MESSAGES  — guards that print fixed text, matched on
#                                      the message, `startswith` (the 281st's
#                                      property 1: an assertion that merely ENDS
#                                      in a guard's wording is an assertion).
#   channel 2  GUARD_CALL_SITES      — guards with no fixed text, matched on the
#                                      LINE `go test` reports, resolved from the
#                                      source at check time.
#
# ⚠️ A declaration carries no line NUMBERS. It names the enclosing function and
# the exact source of the call, and the numbers are resolved by reading the
# suite. A table of line numbers rots on the next edit to main_test.go and rots
# SILENTLY, in the direction of "no guard fired" — which is the reading that
# ends the enquiry.
#
# ⭐ AND `t.Helper()` MOVES THE LINE — measured, not reasoned about.
#
# All four helpers here call `t.Helper()`, so the line `go test` prints for a
# guard inside one is the CALLER's, not the guard's. Probed with a real
# mutation (~/.nightly-311/probe_lines.py, the no-NATS case):
#
#     main_test.go:477: the run never reached the HTTP server
#     main_test.go:774: the run never reached the HTTP server
#
# The `t.Fatal` is at line 256. Neither 477 nor 774 holds a failure call at all.
# So channel 2 cannot be "the reported line is a declared guard call": for a
# helper guard the reported line is an ordinary call to the helper. The two
# channels divide on exactly that boundary, and it is not the boundary a reader
# would guess:
#
#   * a guard inside a `t.Helper()` helper reports a line that names nothing,
#     and must be classified by its MESSAGE — so its message must be fixed;
#   * a guard written inline in the Test function reports its OWN line, and can
#     be classified by its call site — so it need not print anything fixed.
#
# `storeBrokenBy` is both at once: it is a `t.Helper()` helper AND its three
# guards are bare `t.Fatal(err)`. Neither channel can reach it. It is declared
# in UNREACHABLE_BY_EITHER_CHANNEL with the argument for why no case here can
# fire it, and check_markers_against_the_suite() re-derives that argument from
# the source rather than trusting this comment.

# ⭐⭐ THE THIRD CHANNEL, AND IT IS A CORRECTION TO THE CARD FAMILY'S PREMISE.
#
# GUARD_IS_THE_PROPERTY_NOTE. Every scorer split before this one treats
# "guard-fired" as a synonym for "this row scored nothing". That holds while a
# guard is a VALUE CHECK inside a test double — `expected exactly one published
# entry`, `CreateCard: %v` — because no test's property is ever "the double
# behaved".
#
# It does not hold for a SYNCHRONISATION helper. `waitListening` and
# `runToFailure` each encode a real claim about the daemon: that it reached the
# serving seam, and that it gave up. Most tests need that claim to have happened
# before they can ask their own question, and there the helper is an ordinary
# reach guard. But four tests in this suite are named for exactly that claim —
# `..._AFailedBusConnectionCosts…`, `..._IsOnlyAWarning`,
# `..._StopsTheBoot` twice — and in those the guard firing IS the mechanism
# being caught.
#
# Measured, not reasoned: the first --guard-split run over this case list
# reported 4 of 37 CAUGHT rows guard-fired, and reading all four showed the
# classifier wrong on every one. The four rows are real coverage.
#
#   > Guard-ness belongs to the (helper, TEST) pair, not to the helper. A fatal
#   > helper that waits for a state transition is a reach guard in the tests
#   > that need the transition to have happened, and it is the ASSERTION in the
#   > tests whose property IS the transition.
#
# ⚠️ The direction this errs in matters. A classifier blind to this reports real
# coverage as scoring nothing — it manufactures findings, and each costs a
# reader a manual check that ends in "no, that one was fine". That is the
# opposite of the failure mode the earlier scorers guard against (a marker that
# names nothing understates), so a suite can carry both at once.

# Guards that print fixed text. Each is a constant string — no interpolation
# anywhere — so the marker is the whole message and the "where does the fixed
# text stop" discipline the 296th pass restated has nothing to bite on here.
REACH_GUARD_MESSAGES = [
    # harness.waitListening — the boot never reached the HTTP seam, so nothing
    # the test goes on to read was ever produced.
    "the run never reached the HTTP server",
    # harness.runToFailure — the boot did not stop where it should have, so the
    # error the test is about never arrived.
    "the run kept booting past a failure that should have stopped it",
    # harness.shutdown — the run never returned, so every after-shutdown
    # assertion is about a daemon still running.
    "the run did not return after a shutdown signal",
]

# Guards with NO fixed text, declared by call site. Key: the enclosing function.
# Value: (exact source of the call, why every occurrence of it in that function
# is a reach guard rather than an assertion).
#
# Declared per FUNCTION rather than per line because these functions guard a
# setup sequence with the same call repeated — six of them in one test — and a
# per-line table would carry six numbers that all rot together.
GUARD_CALL_SITES = {
    "TestRunScheduler_TheBoundHandlerIsTheJobAPI": (
        "t.Fatal(err)",
        "builds the request the test then sends; asserts nothing about the daemon",
    ),
    "TestRunScheduler_ClosesOutRunsStrandedByAPreviousProcess": (
        "t.Fatal(err)",
        "opens the store, writes the stranded run and reads it back; the property "
        "is the run's STATUS after a boot, asserted at the t.Errorf below",
    ),
    "TestRunScheduler_ASilentBootReportsNoReconciliation": (
        "t.Fatal(err)",
        "opens an empty store; the property is what the operator log says",
    ),
    "TestRunScheduler_AConnectionReturnedWithAnErrorIsNotUsed": (
        "t.Fatal(err)",
        "delivers the shutdown; the property is that the connection was not used",
    ),
    "TestRunScheduler_RunResultsReachTheBusHandler": (
        "t.Fatal(err)",
        "creates the job and builds the request; the property is that the bus "
        "handler was told about the run",
    ),
}

# ⭐ Guards that ARE the property, per test. Measured, and the finding of this
# file — see the header note above GUARD_IS_THE_PROPERTY_NOTE.
#
# Key: the test. Value: {marker: why the guard firing IS this test's mechanism}.
# A failure line matching one of these markers INSIDE one of these tests is an
# assertion, not a reach guard, and the row it belongs to is real coverage.
#
# Both directions are checked: the test must exist and must really call the
# helper that prints the marker, and the marker must really be a declared guard
# marker (otherwise this table would be a way to launder any message at all).
GUARD_IS_THE_PROPERTY = {
    "TestRunScheduler_AFailedBusConnectionCostsTheAgentExecutorAndStillHandsTheBusHandlerTheAddress": {
        "the run never reached the HTTP server":
            "a failed connect is only a warning at the connect site, and with a "
            "handler that builds the boot goes on; reaching the HTTP seam is "
            "what observes that it did, so the wait is the assertion",
    },
    "TestRunScheduler_ABusHandlerThatWillNotSubscribeIsOnlyAWarning": {
        "the run never reached the HTTP server":
            "'only a warning' means the boot continued; the wait is what "
            "observes that it did",
    },
    "TestRunScheduler_AFailedReconciliationStopsTheBoot": {
        "the run kept booting past a failure that should have stopped it":
            "the property is that the boot STOPS, and this guard fires exactly "
            "when it did not",
    },
    "TestRunScheduler_ASchedulerThatWillNotStartStopsTheBoot": {
        "the run kept booting past a failure that should have stopped it":
            "the property is that the boot STOPS, and this guard fires exactly "
            "when it did not",
    },
}


# Failure calls no channel can classify, with the argument that none of them can
# fire under this case list. A declaration here is a claim about REACHABILITY,
# and check_markers_against_the_suite() checks the half that is mechanically
# checkable — that the function really is a t.Helper() helper whose failure
# calls really do carry no fixed text. The reachability half is the argument
# below and is the reason this list is short.
UNREACHABLE_BY_EITHER_CHANNEL = {
    "newHarness": (
        "t.Fatal(err)",
        "a t.Helper() helper whose one guard is a bare t.Fatal(err) on building "
        "the settings registry from a fixed one-entry map. Added by the port, "
        "because main's api.New takes the registry. It fails only if "
        "internal/config refuses SCHEDULER_DB, and every case in this file edits "
        "cmd/scheduler/main.go, which does not declare it.",
    ),
    "storeBrokenBy": (
        "t.Fatal(err)",
        "a t.Helper() helper whose three guards are bare t.Fatal(err): the line "
        "is relocated to the caller, so the call-site channel cannot see it, and "
        "there is no fixed text for the message channel. It is unreachable by "
        "this case list rather than merely unclassified — all three guards fail "
        "only if sql.Open, db.NewWithDB or the schema edit fails against a fresh "
        "t.TempDir() database, and every case in this file edits cmd/scheduler/"
        "main.go, which none of those three touch. A case list that reached into "
        "internal/db could fire them, and then this row would have to become a "
        "real answer instead of an argument.",
    ),
}


# `go test` reports a failure as `file.go:NN: message`. Split rather than
# searched: the marker is judged against the MESSAGE, and a line-wide substring
# test would read an assertion that quotes a guard's wording as the guard (the
# 281st pass's property 1). The file part is NOT narrowed to `_test\.go` — the
# 269th pass's correction, since a helper that forgets `t.Helper()` reports
# against its own file.
FAILURE_LINE = re.compile(r"^\s*(?P<file>[\w./-]+\.go):(?P<line>\d+):\s(?P<message>.*)$")

# A printf verb, so a marker can be checked for carrying one. `%%` is a literal
# percent and is deliberately not a verb.
PRINTF_VERB = re.compile(r"%[-+ #0-9.*\[\]]*[a-zA-Z]")

# `\bt\.` and not `t\.`: `fmt.Errorf` ends in `t.Errorf` and a boundary-less
# pattern reads all five of main.go's error returns as test failure calls. Met
# while writing this file, and it is the 269th pass's wrong-occurrence trap in
# its cheapest form.
GO_FAILURE_CALL = re.compile(r"\bt\.(?:Fatalf|Fatal|Errorf|Error)\(")
GO_FUNC_DECL = re.compile(r"^func\s+(?:\([^)]*\)\s*)?(?P<name>\w+)\s*\(")
GO_STRING = re.compile(r'"(?:[^"\\]|\\.)*"')

# Every .go file in the package, not just the _test.go ones: a `t.Fatalf` can
# only appear where a *testing.T is in scope, so widening costs nothing and
# closes the hole the 269th pass's own guards fell through.
SUITE_FILES = sorted((REPO / "cmd" / "scheduler").glob("*.go"))

# Names the runtime prints when it killed the run rather than a test reporting a
# failure. Such a row prints no `file.go:NN:` line at all, so a classifier can
# only call it unreadable unless it is told to look. The 281st pass met one for
# real (os.Exit under a -h flag) and named it a third verdict rather than
# laundering it into either neighbor.
#
# ⚠️ Declared, not assumed: the --guard-split run prints the count either way.
# It is carried because this suite CAN produce one — the daemon under test runs
# in a goroutine, so a nil map or a closed channel in the boot path panics the
# test binary with no failure line anywhere.
RUNTIME_KILLS = [
    "panic: unexpected call to os.Exit(",
    "panic: ",
    "fatal error: ",
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


def failure_sites(out):
    """[(file, line, message)] for every `file.go:NN: message` in a `go test` run.

    Separate from guard_only() so the classification can be driven by synthetic
    rows in self_test() without a Go toolchain in the loop. A classifier that can
    only be exercised by running the suite it classifies is a classifier nobody
    can sabotage.
    """
    found = []
    for line in out.splitlines():
        match = FAILURE_LINE.match(line)
        if match:
            found.append((match.group("file"),
                          int(match.group("line")),
                          match.group("message").strip()))
    return found


def _source_lines(name):
    for path in SUITE_FILES:
        if path.name == name:
            return path.read_text().splitlines()
    return []


def enclosing_function(file, line):
    """The Go function `file`:`line` falls in, or "" when it is outside one.

    Read from the source rather than from the failure text, because the whole
    point of channel 2 is that the failure text says nothing.
    """
    lines = _source_lines(file)
    if not 1 <= line <= len(lines):
        return ""
    name = ""
    for candidate in lines[:line]:
        match = GO_FUNC_DECL.match(candidate)
        if match:
            name = match.group("name")
    return name


def is_reach_guard(file, line, message):
    """True when this failure line is one of this suite's setup guards.

    Two channels, in order. The message channel is tried first because it is the
    one that survives `t.Helper()` relocating the line: a helper's guard reports
    a caller line that holds no failure call, and asking the call-site channel
    about it would answer "not a guard" for a guard.

    `startswith`, not `in`: an assertion that merely ends in a guard's wording is
    an assertion (the 281st pass's property 1).
    """
    function = enclosing_function(file, line)
    for marker, _ in sorted(GUARD_IS_THE_PROPERTY.get(function, {}).items()):
        if message.startswith(marker):
            # Load-bearing here: this test's mechanism IS the transition the
            # helper waits for, so the guard is the assertion.
            return False
    if any(message.startswith(marker) for marker in REACH_GUARD_MESSAGES):
        return True
    if function not in GUARD_CALL_SITES:
        return False
    # The declaration names the call it covers; a reported line inside a declared
    # function that holds some OTHER failure call is that other call's row, and
    # this suite has five such functions. Without this the declaration would
    # swallow every assertion in the same test — which is the direction that
    # hides a finding.
    call, _ = GUARD_CALL_SITES[function]
    lines = _source_lines(file)
    return 1 <= line <= len(lines) and lines[line - 1].strip() == call


def guard_only(sites):
    """True when every failure line is a reach guard, and there is at least one.

    Pure, and pure on purpose: this is the whole finding, so it has to be the
    thing the self-test drives directly. `all()` over an empty list is True, and
    a row with no failure lines at all is not a guard-fired row — it is a run
    whose output this scorer could not read, which is a different answer and
    must not be laundered into this one.
    """
    return bool(sites) and all(is_reach_guard(*site) for site in sites)


# ─── grounding both channels in the suite, rather than in the comments above ──


def _go_unquote(literal):
    """The text a Go interpreted string literal stands for.

    Only the escapes this suite actually uses are handled, and an unknown one
    raises rather than being passed through: a silently mangled literal would be
    compared against a marker and reported as a mismatch nobody could explain.
    """
    body = literal[1:-1]
    out = []
    index = 0
    while index < len(body):
        char = body[index]
        if char != "\\":
            out.append(char)
            index += 1
            continue
        escape = body[index + 1]
        if escape not in '"\\nt':
            raise ValueError("unhandled Go escape \\%s in %s" % (escape, literal))
        out.append({'"': '"', "\\": "\\", "n": "\n", "t": "\t"}[escape])
        index += 2
    return "".join(out)


def suite_failure_calls():
    """[(file, line, function, call source, first string literal or None)].

    Every t.Fatal*/t.Error* call in the package. The literal is None when the
    call carries no string at all — the population channel 2 exists for.
    """
    calls = []
    for path in SUITE_FILES:
        function = ""
        for number, line in enumerate(path.read_text().splitlines(), 1):
            declaration = GO_FUNC_DECL.match(line)
            if declaration:
                function = declaration.group("name")
            if not GO_FAILURE_CALL.search(line):
                continue
            literal = GO_STRING.search(line)
            calls.append((path.name, number, function, line.strip(),
                          _go_unquote(literal.group(0)) if literal else None))
    return calls


def _function_body(name):
    """The source of the named Go function, or "" when the suite has no such one."""
    for path in SUITE_FILES:
        lines = path.read_text().splitlines()
        start = None
        for number, line in enumerate(lines):
            match = GO_FUNC_DECL.match(line)
            if match and match.group("name") == name:
                start = number
            elif match and start is not None:
                return "\n".join(lines[start:number])
        if start is not None:
            return "\n".join(lines[start:])
    return ""


def _helper_printing(marker, calls):
    """The function whose failure call prints `marker`, or "" if none does."""
    for _, _, function, _, literal in calls:
        if literal is not None and literal.startswith(marker):
            return function
    return ""


def check_markers_against_the_suite(calls=None):
    """Both directions, for both channels. Returns a list of problems.

    The three scorers before the 296th pass carried their markers as prose plus
    a list and nothing checked the list against the code it claimed to describe.
    Two ways that rots, both silent and both pushing the count the same way —
    toward "no guard fired":

      * a marker that names nothing (a reworded guard, a copied format verb)
        matches no line, so a guard-fired row reports as assertion-fired;
      * a guard added later that no marker covers does the same.

    ⚠️ This is a screen, not a verdict. A marker can be right about the PRINTED
    line and absent from the source when the guard interpolates before its fixed
    text — cmd/email-classifier guards with `t.Fatalf("parsing %s: %v", max, err)`
    and the 281st pass's markers appear in no source file. This suite has no
    guard of that shape (all three message markers are whole constant strings),
    so the check is exact here rather than approximate; carrying it to a suite
    that does means teaching it to expand the interpolation first.
    """
    if calls is None:
        calls = suite_failure_calls()
    problems = []
    printed = [literal for _, _, _, _, literal in calls if literal is not None]

    # ── channel 1, forward: every message marker names exactly one message,
    #    is that message's fixed head, and carries no format verb.
    for marker in REACH_GUARD_MESSAGES:
        if PRINTF_VERB.search(marker):
            problems.append(
                "message marker carries a format verb, so it can match no printed "
                "line and its guard would report as an assertion: %r" % marker)
            continue
        prefixed = [literal for literal in printed if literal.startswith(marker)]
        if not prefixed:
            problems.append(
                "message marker is the head of no message this suite prints, so it "
                "names nothing: %r" % marker)
        elif len(prefixed) > 1:
            problems.append(
                "message marker is the head of %d printed messages, so it cannot "
                "pick a guard out of them: %r -> %r"
                % (len(prefixed), marker, prefixed))
        elif prefixed[0] != marker:
            # Not an error in general — a marker is allowed to be a proper head —
            # but every guard in THIS suite prints a constant, so a marker that is
            # only part of one means the guard grew an interpolation and the
            # comment above has stopped being true.
            problems.append(
                "message marker is a proper prefix of %r; every guard in this suite "
                "prints a constant, so this one has changed shape and the two "
                "channels no longer divide where the file says they do" % prefixed[0])

    # ── channel 2, forward: every declared call site exists, in the function it
    #    is declared under, and really carries no fixed text.
    for function, (call, _) in sorted(GUARD_CALL_SITES.items()):
        matching = [c for c in calls if c[2] == function and c[3] == call]
        if not matching:
            problems.append(
                "declared guard call site names nothing: %s does not contain %r"
                % (function, call))
            continue
        with_text = [c for c in matching if c[4] is not None]
        if with_text:
            problems.append(
                "declared guard call site in %s carries fixed text (%r), so it "
                "belongs in the message channel where property 1 still applies"
                % (function, with_text[0][4]))

    # ── channel 2, reverse: every failure call with no fixed text is declared,
    #    as a guard or as unreachable. This is the direction that rots, and it is
    #    the one the earlier scorers' prose lists never had.
    declared = set(GUARD_CALL_SITES) | set(UNREACHABLE_BY_EITHER_CHANNEL)
    for file, number, function, call, literal in calls:
        if literal is not None or function in declared:
            continue
        problems.append(
            "%s:%d in %s prints no fixed text and is declared nowhere, so this "
            "classifier can only guess at it: %s" % (file, number, function, call))

    # ── the unreachable list, checked for the half that is mechanical: the
    #    function must really be a helper (no *testing.T of its own to report
    #    against — it takes one), and its calls must really carry no fixed text.
    for function, (call, _) in sorted(UNREACHABLE_BY_EITHER_CHANNEL.items()):
        matching = [c for c in calls if c[2] == function]
        if not matching:
            problems.append(
                "%s is declared unreachable but contains no failure call at all; "
                "the argument for it has outlived the code" % function)
            continue
        if any(c[4] is not None for c in matching):
            problems.append(
                "%s is declared to have no fixed text but prints %r, so the "
                "message channel could reach it after all"
                % (function, next(c[4] for c in matching if c[4] is not None)))
        if any(c[3] != call for c in matching):
            problems.append(
                "%s has grown a failure call the declaration does not name: %r"
                % (function, next(c[3] for c in matching if c[3] != call)))
        # The whole function, not a window above the call: a fixed window
        # misses t.Helper() once a helper grows past it.
        if "t.Helper()" not in _function_body(function):
            problems.append(
                "%s is declared unreachable because t.Helper() relocates its line; "
                "it no longer calls t.Helper(), so the call-site channel can see "
                "it and it should be declared as a guard instead" % function)

    # ── the load-bearing table, both directions. Without this it is a way to
    #    launder any message at all into an assertion, which is the one edit
    #    that could hide a real guard-fired row.
    for test, markers in sorted(GUARD_IS_THE_PROPERTY.items()):
        if not any(c[2] == test for c in calls):
            problems.append(
                "%s is declared to have a load-bearing guard but contains no "
                "failure call, so it is not a test in this suite any more" % test)
            continue
        body = _function_body(test)
        for marker in sorted(markers):
            if marker not in REACH_GUARD_MESSAGES:
                problems.append(
                    "%s declares %r load-bearing, but that is not a guard marker; "
                    "this table may only reclassify a declared guard"
                    % (test, marker))
                continue
            helper = _helper_printing(marker, calls)
            if not helper:
                problems.append(
                    "%s declares %r load-bearing, but no function in this suite "
                    "prints it" % (test, marker))
            elif not re.search(r"\b%s\(" % re.escape(helper), body):
                problems.append(
                    "%s declares the guard in %s() load-bearing but never calls "
                    "it, so the declaration describes a test that no longer "
                    "exists in this shape" % (test, helper))

    # ── the two channels must not overlap. A function that is BOTH declared a
    #    call-site guard and printing a declared message would be classified by
    #    whichever channel ran first, and the answer would depend on this file's
    #    order rather than on the suite.
    both = set(GUARD_CALL_SITES) & set(UNREACHABLE_BY_EITHER_CHANNEL)
    if both:
        problems.append(
            "declared in both channels, so the verdict depends on this file's "
            "order rather than on the suite: %s" % ", ".join(sorted(both)))
    return problems


def _a_line_inside(name):
    """A line number inside the named function, resolved from the source.

    Resolved rather than written down: a self-test carrying line numbers stops
    testing the classifier and starts testing the table it was written beside.
    """
    for path in SUITE_FILES:
        lines = path.read_text().splitlines()
        start = None
        for number, line in enumerate(lines, 1):
            match = GO_FUNC_DECL.match(line)
            if match and match.group("name") == name:
                start = number
            elif match and start is not None:
                return start + 1
        if start is not None:
            return start + 1
    raise AssertionError("no function named %s in the suite" % name)


def self_test():
    """Drive both channels in BOTH directions on rows taken from this suite.

    Every check asserts a verdict AND its opposite: a one-sided check passes
    against a classifier that returns True unconditionally, which is exactly the
    degenerate answer ("nothing was really scored") that would end the enquiry.

    Rows are (file, line, message) the way `go test` prints them, so the line
    numbers are resolved against the real source and a check cannot pass by
    agreeing with a table this file also wrote.
    """
    problems = []

    def check(label, got, want):
        if got != want:
            problems.append("%s: got %r, want %r" % (label, got, want))

    # ⛔ Each declaration table is asserted NON-EMPTY before anything iterates it.
    # Found by sabotaging this file rather than by reading it: emptying
    # GUARD_IS_THE_PROPERTY printed GREEN, because every check that would have
    # noticed is a `for` over the table itself and an empty table skips them all.
    # A skip-shortcut is an instrument whose failure mode is silence — no wrong
    # answer, just no answer, reported as coverage (95th pass). The direction is
    # the bad one: with that table gone, four rows of real coverage report as
    # having scored nothing, and nothing says so.
    #
    # Each count below is a measured property of THIS suite, so a rewrite that
    # genuinely removes the population fails here loudly and gets re-read, which
    # is the tolerable direction.
    if not REACH_GUARD_MESSAGES:
        problems.append(
            "no message markers: this suite's three t.Helper() guards print "
            "fixed text and are the ONLY channel that can classify them, since "
            "t.Helper() relocates their line to a caller that holds no call")
    if not GUARD_CALL_SITES:
        problems.append(
            "no declared guard call sites: 15 of this suite's failure calls are "
            "a bare t.Fatal(err) with no fixed text, and the message channel "
            "cannot reach one of them")
    if not GUARD_IS_THE_PROPERTY:
        problems.append(
            "no load-bearing guards declared: four tests here are named for the "
            "very transition their fatal helper waits for, and with this table "
            "empty all four report as scoring nothing when they are real "
            "coverage — see GUARD_IS_THE_PROPERTY_NOTE")

    calls = suite_failure_calls()
    by_function = {}
    for file, number, function, call, literal in calls:
        by_function.setdefault(function, []).append((file, number, call, literal))

    def a_guard_line(function):
        call, _ = GUARD_CALL_SITES[function]
        for file, number, source, _ in by_function[function]:
            if source == call:
                return (file, number, "some error the setup produced")
        raise AssertionError("no declared guard call in %s" % function)

    def an_assertion_line(function):
        call, _ = GUARD_CALL_SITES[function]
        for file, number, source, literal in by_function[function]:
            if source != call and literal is not None:
                return (file, number, literal)
        raise AssertionError("no assertion in %s" % function)

    # ── channel 1, both ways.
    # Lines are RESOLVED from the source, never written down: channel 3 makes the
    # verdict depend on which test the line falls in, so a hard-coded number
    # would stop testing the classifier and start testing this file's memory of
    # the suite's layout. Both tests below are deliberately ones with no
    # GUARD_IS_THE_PROPERTY entry.
    needs_it = _a_line_inside("TestRunScheduler_TheBoundHandlerIsTheJobAPI")
    also_needs_it = _a_line_inside("TestRunScheduler_ConnectsToTheBusExactlyOnce")
    check("a helper guard's message is a guard",
          is_reach_guard("main_test.go", needs_it,
                         "the run never reached the HTTP server"),
          True)
    check("an ordinary assertion is not a guard",
          is_reach_guard("main_test.go", needs_it,
                         'listening on ":8092", want ":9999"'),
          False)
    # Property 1, the 281st pass's: ENDING in a guard's wording is not being one.
    check("an assertion that ends in a guard's wording is an assertion",
          is_reach_guard("main_test.go", needs_it,
                         "after the shutdown, the run never reached the HTTP server"),
          False)
    # Channel 1 ignores WHERE the line came from among tests that merely need the
    # transition — t.Helper() relocates it, so the message must be believed from
    # any such line.
    check("a helper guard is a guard from any test that only needs it",
          is_reach_guard("main_test.go", also_needs_it,
                         "the run never reached the HTTP server"),
          True)

    # ── channel 2, both ways, on real lines of the real suite.
    for function in sorted(GUARD_CALL_SITES):
        check("a declared bare guard in %s is a guard" % function,
              is_reach_guard(*a_guard_line(function)), True)
        check("an assertion in the same function is not a guard" % (),
              is_reach_guard(*an_assertion_line(function)), False)

    # A bare t.Fatal(err) in a function that declares no guard call site must NOT
    # be swallowed. Without this the reverse-direction check above would be the
    # only thing standing between an undeclared guard and a silent misread.
    undeclared = [
        (file, number) for file, number, function, _, literal in calls
        if literal is None and function not in GUARD_CALL_SITES
        and function not in UNREACHABLE_BY_EITHER_CHANNEL]
    for file, number in undeclared:
        check("an undeclared bare failure at %s:%d is not a guard" % (file, number),
              is_reach_guard(file, number, "some error"), False)

    # A line inside a declared function that is NOT the declared call is an
    # assertion. This is the check that stops a declaration swallowing its whole
    # test, which is the direction that hides a finding.
    check("a line in a declared function holding no failure call is not a guard",
          is_reach_guard("main_test.go", 523, "anything at all"), False)
    check("a line outside the suite is not a guard",
          is_reach_guard("nowhere.go", 1, "anything at all"), False)

    # ── the load-bearing channel, both ways, on the real lines of the real
    #    suite. The same message must answer differently in two different tests,
    #    which is the whole claim, and a table-driven check that only ever asked
    #    one of them would pass against a classifier that ignored the test.
    for test, markers in sorted(GUARD_IS_THE_PROPERTY.items()):
        marker = sorted(markers)[0]
        line = _a_line_inside(test)
        check("a load-bearing guard in %s is NOT a reach guard" % test,
              is_reach_guard("main_test.go", line, marker), False)
    # ...and the same marker in a test that merely needs the transition IS one.
    check("the same message in a test that only needs the transition is a guard",
          is_reach_guard("main_test.go",
                         _a_line_inside("TestRunScheduler_TheBoundHandlerIsTheJobAPI"),
                         "the run never reached the HTTP server"),
          True)
    # A message that is NOT a declared guard is not laundered by the table
    # either, in a test that has an entry.
    check("an ordinary assertion inside a load-bearing test is still not a guard",
          is_reach_guard("main_test.go",
                         _a_line_inside("TestRunScheduler_AFailedBusConnectionCostsTheAgentExecutorAndStillHandsTheBusHandlerTheAddress"),
                         "built 3 agent executors with no bus, want 0"),
          False)

    # ── guard_only, and the empty case it must NOT launder.
    guard = ("main_test.go", needs_it, "the run never reached the HTTP server")
    assertion = ("main_test.go", needs_it, 'listening on ":8092", want ":9999"')
    check("all guards is guard-only", guard_only([guard, guard]), True)
    check("one assertion among guards is not guard-only",
          guard_only([guard, assertion]), False)
    check("all assertions is not guard-only", guard_only([assertion]), False)
    check("no failure lines at all is NOT guard-only", guard_only([]), False)

    # ── the reader of `go test` output, both ways.
    check("a failure line is read",
          failure_sites("    main_test.go:477: the run never reached the HTTP server"),
          [("main_test.go", 477, "the run never reached the HTTP server")])
    check("an ordinary log line is not read as a failure",
          failure_sites("    scheduler: listening on :9999"), [])
    check("a PASS line is not read as a failure",
          failure_sites("ok  \tgithub.com/slava/scheduler/cmd/scheduler\t0.17s"), [])

    # ── the runtime channel, both ways.
    check("a panic is a runtime kill",
          runtime_kill("panic: send on closed channel") != "", True)
    check("an ordinary failure is not a runtime kill",
          runtime_kill("    main_test.go:431: listening on X, want Y"), "")

    # ── the boundary that cost this file a false start: fmt.Errorf is not a test
    #    failure call. Asserted here rather than left to the `\b`, because the
    #    regex is one character away from reading main.go's five error returns as
    #    guards and the wrong answer would look exactly like a clean suite.
    check("fmt.Errorf is not a test failure call",
          bool(GO_FAILURE_CALL.search('\t\treturn fmt.Errorf("db: %w", err)')), False)
    check("t.Fatalf is a test failure call",
          bool(GO_FAILURE_CALL.search('\t\tt.Fatalf("bound %d addresses", n)')), True)
    return problems


def guard_split():
    """Re-score every case, reading WHY each CAUGHT row went red.

    Status: 0 when both self-tests pass and no row is guard-fired only or
    unreadable, 1 otherwise. A guard-fired row is a finding, not a footnote — it
    is a case whose CAUGHT in the default table is a number for something nobody
    measured.
    """
    problems = self_test()
    problems.extend(check_markers_against_the_suite())
    if problems:
        print("SELF-TEST FAILED — the classifier cannot be trusted to report anything:")
        for problem in problems:
            print("  ⚠️  " + problem)
        return 1
    print("self-test: all three channels return both verdicts for the right "
          "reasons; every message marker names exactly one message this suite "
          "prints, every failure call with no fixed text is declared, and every "
          "load-bearing guard is really called by the test that claims it\n")
    if "--self-test-only" in sys.argv:
        return 0

    targets = [TARGET]
    rows = []
    with _sabotage_session(targets, PACKAGES) as (rels, restore, original):
        for case in CASES:
            text = _apply_case(targets, original, case)
            for target in targets:
                if text[target] != original[target]:
                    target.write_text(text[target])
            verdict, out = _run_tests(PACKAGES)
            restore()
            sites = failure_sites(out)
            killed = runtime_kill(out)
            if verdict != "CAUGHT":
                why = ""
            elif sites and guard_only(sites):
                why = "guard only -- NOT coverage"
            elif sites:
                why = "assertion-fired"
            elif killed:
                why = "runtime-fired"
            else:
                why = "CAUGHT with no readable failure line"
            rows.append((case, verdict, why, killed, sites))
            print("%-11s %-32s %s" % (verdict, why, case.name))

    print("\n============ guard split ============")
    guarded = [r for r in rows if r[2].startswith("guard only")]
    runtime = [r for r in rows if r[2] == "runtime-fired"]
    unreadable = [r for r in rows if r[2].startswith("CAUGHT with no")]
    caught = [r for r in rows if r[1] == "CAUGHT"]
    print("%d CAUGHT rows, %d caught by a reach-guard only, %d caught by the Go "
          "test runtime, %d unreadable"
          % (len(caught), len(guarded), len(runtime), len(unreadable)))
    for case, _, _, _, sites in guarded:
        print("  ⚠️  guard-fired only, so this row scored nothing: %s" % case.name)
        for file, number, message in sites:
            print("      %s:%d: %s" % (file, number, message))
    # Printed, never swallowed. A verdict that does not fail the run still has to
    # be visible, or "0 guard-fired" reads as "every row was an assertion".
    for case, _, _, killed, _ in runtime:
        print("  ·   real coverage, but no assertion ran — the Go test runtime "
              "caught it:\n      %s\n      %s" % (case.name, killed))
    for case, _, _, _, _ in unreadable:
        print("  ⚠️  no readable failure line, classify by hand: %s" % case.name)
    return 1 if guarded or unreadable else 0


if __name__ == "__main__":
    if "--guard-split" in sys.argv or "--self-test-only" in sys.argv:
        sys.exit(guard_split())
    sys.exit(score(TARGET, PACKAGES, CASES))
