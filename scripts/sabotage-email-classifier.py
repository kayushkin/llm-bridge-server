#!/usr/bin/env python3
"""Score cmd/email-classifier's tests by breaking the binary on purpose.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-email-classifier.py [--diffs] [--crosstable]

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

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

TARGETS = [REPO / "cmd/email-classifier/main.go"]
PACKAGES = ["./cmd/email-classifier/"]


CASES = [
    # ---- controls ----
    Case(
        "CONTROL known-positive: the board this binary looks for is renamed",
        [('defaultBoardName = "Email"', 'defaultBoardName = "Emails"')],
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
        [('fs.StringVar(&c.Model, "model", envOr(lookupEnvironment, "EMAIL_CLASSIFIER_MODEL", defaultModel)',
          'fs.StringVar(&c.Model, "model", envOr(lookupEnvironment, "EMAIL_CLASSIFIER_MODEL", "claude-haiku-4-5")')],
    ),
    Case(
        "defaultModel itself drifts to a more expensive model: same work, several times the bill",
        [('defaultModel = "claude-sonnet-4-6"', 'defaultModel = "claude-opus-4-1"')],
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
    Case(
        "-account ships pinned to one mailbox: every other account is silently skipped forever",
        [('fs.StringVar(&c.OnlyAccount, "account", "",', 'fs.StringVar(&c.OnlyAccount, "account", "gmail",')],
    ),
    Case(
        "-board-id ships with a value: every host pins the same board, which exists on none of them",
        [('fs.StringVar(&c.BoardID, "board-id", envOr(lookupEnvironment, "EMAIL_BOARD_ID", "")',
          'fs.StringVar(&c.BoardID, "board-id", envOr(lookupEnvironment, "EMAIL_BOARD_ID", "email-board")')],
    ),
    Case(
        "-kanban-url points at noteboard: the board read that every other step depends on",
        [('"KANBAN_STORE_URL", "http://localhost:8305"', '"KANBAN_STORE_URL", "http://localhost:8191"')],
    ),
    Case(
        "-mail-url points at kanban-store: no message is ever fetched",
        [('"MAILSTACK_URL", "http://localhost:8195"', '"MAILSTACK_URL", "http://localhost:8305"')],
    ),
    Case(
        "-bridge-url points at auth-store: the agent-ownership gate cannot resolve a session",
        [('"LLM_BRIDGE_URL", "http://localhost:8160"', '"LLM_BRIDGE_URL", "http://localhost:8303"')],
    ),
    Case(
        "-auth-store-url points at llm-bridge: the Anthropic key resolve that every model call needs",
        [('"AUTH_STORE_URL", "http://localhost:8303"', '"AUTH_STORE_URL", "http://localhost:8160"')],
    ),

    # ---- the wiring: nine variables, seven of them interchangeable strings ----
    Case(
        "the two bearer tokens are swapped: both default to changeme, so on this host it is invisible",
        [('fs.StringVar(&c.MailToken, "mail-token", envOr(lookupEnvironment, "MAILSTACK_TOKEN", "changeme")',
          'fs.StringVar(&c.MailToken, "mail-token", envOr(lookupEnvironment, "AUTH_STORE_TOKEN", "changeme")')],
    ),
    Case(
        "MAILSTACK_URL is read into the kanban field: the two endpoints trade places",
        [('fs.StringVar(&c.KanbanURL, "kanban-url", envOr(lookupEnvironment, "KANBAN_STORE_URL", "http://localhost:8305")',
          'fs.StringVar(&c.KanbanURL, "kanban-url", envOr(lookupEnvironment, "MAILSTACK_URL", "http://localhost:8305")')],
    ),
    Case(
        "the board id is read from the wrong variable: a pinned board silently unpins",
        [('envOr(lookupEnvironment, "EMAIL_BOARD_ID", "")', 'envOr(lookupEnvironment, "EMAIL_BOARD", "")')],
    ),
    Case(
        "envOr prefers its fallback over the environment: every endpoint pins to localhost",
        [('if v := lookupEnvironment(key); v != "" {', 'if v := lookupEnvironment(key); v == "" {')],
    ),
    Case(
        "SCHEDULER_URL falls back to the kanban port: -register posts the job to the wrong service",
        [('"SCHEDULER_URL", "http://localhost:8092"', '"SCHEDULER_URL", "http://localhost:8305"')],
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


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
