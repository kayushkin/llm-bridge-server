#!/usr/bin/env python3
"""Score cmd/kanban-classifier's tests by breaking the binary on purpose.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-kanban-classifier.py [--diffs] [--crosstable]

Why this binary is worth a scorer: it is the last of the four cmd mains that
had test files but no config seam, it registers twelve flags, and it decides
what the box spends classifying every bridge session every fifteen minutes.

ONE target, and that is deliberate. The seam the 199th pass opened is
configuration: the flag set, the environment wiring, the registration payload
and the two guards. The tick's own logic — candidate selection, the digest
builder, the classify call and the card writes — lives in the same file but is
covered by classify_test.go, digest_test.go and provenance_test.go and is NOT
sabotaged here.

Case-writing rules inherited from the reminder-coordinator, autoworker and
email-classifier plans:

  - Prefer a DRIFTED VALUE to a deletion. It orphans no variable, needs no
    second edit, and is the likelier real-world regression.
  - Read every applied diff against its label (`--diffs`). A row prints the name
    you gave it, not the edit you made.
  - An UNNOTICED row is not automatically a coverage hole. It can be a
    behavioural no-op, or a fake in the test file that swallows the value the
    mutation moved.
  - Verify every needle is unique WITHIN its target file before running. The
    engine refuses a needle found in more than one target file, but with a
    single target that check never fires, and `_apply_case` then applies
    `.replace(find, replace, 1)` to the first occurrence — an edit nobody chose.
    All 31 needles below were checked unique in main.go by a separate script.

Score when filed by the 199th pass: 29/29 real mechanisms caught, both controls
behaved, exit 0. 31 cases in the list: 29 real plus the two controls.

That 29 is worth reading against the card that asked for this work, which
counted 12 flags and 8 environment reads. THE FLAGS WERE RIGHT AND THE
ENVIRONMENT READS WERE NOT — there are six envOr call sites over six distinct
variables, plus one os.UserHomeDir, so seven. The 8 came from `grep -c 'envOr('`
which returns 7 because it counts envOr's own declaration, plus the UserHomeDir.
This is the second consecutive binary where the flag count was exact and the
number derived from grepping a helper's name was one too high; the 198th pass
predicted this card would break the same way, and it did.

⚠️ `--crosstable` has NOT been taken for this binary, and no `unreddened`
declaration is made below, because an empty one would read as "measured and
found none" rather than "not measured". The next pass on this binary owns that.
Expect it to be a large table with mostly silent columns: three of the four test
files in this package cover code no case here touches, and that is not a
finding.

⚠️ Region this plan does NOT cover, declared rather than left to be
rediscovered: every case below edits configuration. selectCandidates' lookback
and sort, processSession's dedupe-signature short-circuit, the do-not-track
re-evaluation delta, buildDigest's section budgets, classify's truncated-report
rejection and both card write paths are NOT sabotaged here.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

TARGETS = [REPO / "cmd/kanban-classifier/main.go"]
PACKAGES = ["./cmd/kanban-classifier/"]


CASES = [
    # ---- controls ----
    Case(
        "CONTROL known-positive: the board this binary writes every card to is renamed",
        [('defaultBoardName     = "Agent runs"', 'defaultBoardName     = "Agent run"')],
    ),
    Case(
        "CONTROL known-negative: reword the -auth-token usage string",
        [('"auth-store bearer token"', '"Bearer token presented to auth-store when resolving the Anthropic key."')],
        expected_unnoticed="flag usage text for a human reading -h. No test reads it and nothing "
                           "downstream parses it. This control is what proves the rows below redden "
                           "on the VALUES that ship and not on the source lines carrying them. "
                           "Falsified by a test that asserts on fs.PrintDefaults() output.",
    ),

    # ---- the three switches that turn a tick into a no-op ----
    #
    # This family is the reason the binary was worth scoring. Each leaves a
    # process that walks the session list, logs a full pass and exits 0 having
    # written no card, and nothing on this box distinguishes that from a week
    # with no active sessions.
    Case(
        "-register ships ON: every tick registers a cron job and exits before reading a session",
        [('fs.BoolVar(&c.RegisterJob, "register", false,', 'fs.BoolVar(&c.RegisterJob, "register", true,')],
    ),
    Case(
        "-dry-run ships ON: every tick pays the full model bill and writes no card",
        [('fs.BoolVar(&c.DryRun, "dry-run", false,', 'fs.BoolVar(&c.DryRun, "dry-run", true,')],
    ),
    Case(
        "-dump-digest ships ON: every digest goes to stdout, no session is classified, "
        "and the run history fills with plausible output",
        [('fs.BoolVar(&c.DumpDigest, "dump-digest", false,', 'fs.BoolVar(&c.DumpDigest, "dump-digest", true,')],
    ),

    # ---- the values that decide what a tick costs ----
    Case(
        "-model drifts to a frontier model: the cheap-model curator quietly bills like a premium one",
        [('fs.StringVar(&c.Model, "model", envOr(lookupEnvironment, "KANBAN_CLASSIFIER_MODEL", defaultModel)',
          'fs.StringVar(&c.Model, "model", envOr(lookupEnvironment, "KANBAN_CLASSIFIER_MODEL", "claude-opus-4-1")')],
    ),
    Case(
        "defaultModel itself drifts to a frontier model: same work, many times the bill, nothing fails",
        [('defaultModel              = "claude-haiku-4-5-20251001"', 'defaultModel              = "claude-opus-4-1"')],
    ),
    Case(
        "-max drifts 10 -> 2: the session backlog drains five times slower and every tick reports success",
        [('fs.IntVar(&c.MaxClassify, "max", maxClassifyPerTick,', 'fs.IntVar(&c.MaxClassify, "max", 2,')],
    ),
    Case(
        "maxClassifyPerTick itself drifts 10 -> 100: one tick's model spend goes up tenfold",
        [("maxClassifyPerTick   = 10", "maxClassifyPerTick   = 100")],
    ),
    Case(
        "-digest-budget drifts 16000 -> 64000: every prompt quadruples in size at four times the cost",
        [('fs.IntVar(&c.DigestBudget, "digest-budget", digestCharBudget,',
          'fs.IntVar(&c.DigestBudget, "digest-budget", 64000,')],
    ),
    Case(
        "digestCharBudget itself drifts 16000 -> 4000: digests silently lose the context the model needs",
        [("digestCharBudget = 16000", "digestCharBudget = 4000")],
    ),

    # ---- the remaining flag defaults ----
    Case(
        "-board drifts off the constant: cards go to a board that exists on no host, "
        "and the bootstrap path creates a second one",
        [('fs.StringVar(&c.BoardName, "board", defaultBoardName,', 'fs.StringVar(&c.BoardName, "board", "Agent sessions",')],
    ),
    Case(
        "-session ships pinned to one id: every tick classifies that one session and ignores the sweep",
        [('fs.StringVar(&c.OneSession, "session", "",', 'fs.StringVar(&c.OneSession, "session", "sess-1",')],
    ),
    Case(
        "-kanban-url points at noteboard: every card read and write goes to the wrong service",
        [('"KANBAN_STORE_URL", "http://localhost:8305"', '"KANBAN_STORE_URL", "http://localhost:8191"')],
    ),
    Case(
        "-bridge-url points at kanban-store: no session is ever listed, so no tick classifies anything",
        [('"LLM_BRIDGE_URL", "http://localhost:8160"', '"LLM_BRIDGE_URL", "http://localhost:8305"')],
    ),
    Case(
        "-auth-store-url points at the legacy apiauth port: the Anthropic key resolve fails on every tick",
        [('"AUTH_STORE_URL", "http://localhost:8303"', '"AUTH_STORE_URL", "http://localhost:8190"')],
    ),
    Case(
        "-auth-token ships as a real-looking token instead of the placeholder: "
        "a host that never set one now sends a wrong credential rather than an obvious one",
        [('"AUTH_STORE_TOKEN", "changeme"', '"AUTH_STORE_TOKEN", "sk-auth-store"')],
    ),
    Case(
        "SCHEDULER_URL's default drifts to the kanban-store port: -register POSTs the job to the wrong service",
        [('"SCHEDULER_URL", "http://localhost:8092"', '"SCHEDULER_URL", "http://localhost:8305"')],
    ),

    # ---- the environment wiring: six variables, five of them interchangeable strings ----
    #
    # Nothing downstream would reject a swap. kanbanclient.New takes the kanban
    # URL positionally, resolveAnthropicKey takes AuthStoreURL and AuthToken off
    # the same struct, and listSessions takes the bridge URL — all strings.
    Case(
        "KANBAN_STORE_URL and LLM_BRIDGE_URL swap: both are base URLs and the compiler cannot tell them apart",
        [('fs.StringVar(&c.KanbanURL, "kanban-url", envOr(lookupEnvironment, "KANBAN_STORE_URL"',
          'fs.StringVar(&c.KanbanURL, "kanban-url", envOr(lookupEnvironment, "LLM_BRIDGE_URL"')],
    ),
    Case(
        "the auth-store URL is read from AUTH_STORE_TOKEN: the key resolve targets a token string as a host",
        [('fs.StringVar(&c.AuthStoreURL, "auth-store-url", envOr(lookupEnvironment, "AUTH_STORE_URL"',
          'fs.StringVar(&c.AuthStoreURL, "auth-store-url", envOr(lookupEnvironment, "AUTH_STORE_TOKEN"')],
    ),
    Case(
        "the model id is read from KANBAN_STORE_URL: every classify call names a URL as its model",
        [('fs.StringVar(&c.Model, "model", envOr(lookupEnvironment, "KANBAN_CLASSIFIER_MODEL"',
          'fs.StringVar(&c.Model, "model", envOr(lookupEnvironment, "KANBAN_STORE_URL"')],
    ),
    Case(
        "envOr tests for PRESENCE instead of emptiness: an exported-but-empty variable "
        "now beats the built-in default and every URL becomes the empty string",
        [("	if v := lookupEnvironment(key); v != \"\" {", "	if _, v := true, lookupEnvironment(key); true {")],
    ),

    # ---- the derived values, which no flag registers and nothing else would catch ----
    Case(
        "ExplicitSession ships true: the per-session dedupe gates that ration the cron sweep "
        "are bypassed on every session of every tick",
        [("	c.ExplicitSession = c.OneSession != \"\"", "	c.ExplicitSession = true")],
    ),
    Case(
        "the binary path -register writes drifts: the registered job execs a file that is not there",
        [('c.BinaryPath = filepath.Join(homeDirectory, "bin", "kanban-classifier")',
          'c.BinaryPath = filepath.Join(homeDirectory, "bin", "kanban-classifier-tick")')],
    ),

    # ---- the registration payload, written once per host and uncorrectable through the API ----
    Case(
        "timeout_seconds goes back to 0: the job takes the scheduler's 5-minute default "
        "and a tick making one model call per session is killed mid-write",
        [("schedulerJobTimeoutSeconds = 900", "schedulerJobTimeoutSeconds = 0")],
    ),
    Case(
        "the cadence drifts */15 -> */5: three times the session sweeps and three times the model spend",
        [('schedulerJobSchedule = "*/15 * * * *"', 'schedulerJobSchedule = "*/5 * * * *"')],
    ),
    Case(
        "the job name drifts: -register stops recognising its own job, registers a second one, "
        "and the box classifies every session twice",
        [('schedulerJobName     = "kanban-classifier-tick"', 'schedulerJobName     = "kanban-classifier"')],
    ),
    Case(
        "the registered command drops the binary path: the job runs the scheduler URL as a shell command",
        [('"command":     cfg.BinaryPath,', '"command":     cfg.SchedulerURL,')],
    ),

    # ---- the two guards, and the property that makes the seam usable ----
    Case(
        "the empty-budget guard is dead: -max=0 breaks the loop before the first session and reports success",
        [("if c.MaxClassify < 1 {", "if false {")],
    ),
    Case(
        "the digest-budget guard is dead: -digest-budget=0 is silently replaced by the full-size "
        "default and the operator's intent is discarded at full cost",
        [("if c.DigestBudget < 1 {", "if false {")],
    ),
    Case(
        "the flag set goes back to ExitOnError: a bad argument takes the test binary down with it",
        [('flag.NewFlagSet("kanban-classifier", flag.ContinueOnError)',
          'flag.NewFlagSet("kanban-classifier", flag.ExitOnError)')],
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
