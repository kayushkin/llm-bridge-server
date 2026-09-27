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

Re-aimed 2026-09-25 (card 7e51e288): main moved configuration to
llm-bridge/servicesettings and the model call to llm-bridge-server's oneshot
route, so 11 rows and the known-negative control named code that was gone and
the run aborted on its second row. Nine rows now edit main's equivalent — the
defaults in internal/config, which setting feeds which field in main.go — and
three are deleted with their mechanism (the auth-store key resolve and envOr).
Score after: 26/26 real mechanisms caught, both controls behaved, exit 0.

Same night, C3b (card d1b06dd2) ported the rows the scorer branches wrote
between 2026-08-08 and 2026-08-14 — the per-tick cap, -register, the bridge
readers, the digest and schema, the boundary values and the quiet side — each
re-aimed at main. The "ONE target" and "region not covered" paragraphs above
describe the 199th pass's list; the sections below them now cover the tick.
Score after: 86/87 real mechanisms caught, the one miss declared (budgetShare's
unreachable guard), both controls behaved, exit 0. `--crosstable` has still not
been taken on main.

Score now (2026-09-26, main after cd3920a, measured by the C3 close-out, card
2fab79e5): 80/81, the same one miss declared, both controls behaved, exit 0,
0 aborted. The list is smaller than 86/87 on purpose: C3h (5fc9368) moved the
seven registration-mechanism rows to scripts/sabotage-schedulerjobs.py with
the code, and the close-out added one row (the bridge's own session id is kept).

That 29 is worth reading against the card that asked for this work, which
counted 12 flags and 8 environment reads. THE FLAGS WERE RIGHT AND THE
ENVIRONMENT READS WERE NOT — there are six envOr call sites over six distinct
variables, plus one os.UserHomeDir, so seven. The 8 came from `grep -c 'envOr('`
which returns 7 because it counts envOr's own declaration, plus the UserHomeDir.
This is the second consecutive binary where the flag count was exact and the
number derived from grepping a helper's name was one too high; the 198th pass
predicted this card would break the same way, and it did.

`--crosstable` was first taken on main on 2026-09-27 (bff5fc4, card 68161ab4):
58 of 159 tests were reddened by no case, because every case then edited
configuration or the parts of the tick C3b had ported. The section THE TICK'S
OWN LOGIC below adds 55 rows aimed at those 58 — selectCandidates and the
per-session gates, the signature helpers, the recorded shortfalls, the bridge
readers, the model call and prompt, the digest builder, both card write paths,
run() and main(). Score after (6cea0fc): 137/138, the same one miss declared,
both controls behaved, exit 0; `--crosstable` 140 cases x 159 tests, 0 tests
reddened by no case, exit 0. No `unreddened` declaration is needed.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

# The defaults and the variable names this binary starts with live in
# internal/config since configuration moved to llm-bridge/servicesettings, so a
# row about a shipped value has to edit them there. PACKAGES stays the one
# binary: the question is whether ITS tests notice, not internal/config's.
TARGETS = [
    REPO / "cmd/kanban-classifier/main.go",
    REPO / "internal/config/jobs.go",
    REPO / "internal/config/settings.go",
    # The digest's cuts are all textcut.TruncateToByteBudgetOnRuneBoundary, so
    # the row that cuts from the wrong end has to edit it there.
    REPO / "internal/textcut/textcut.go",
]
PACKAGES = ["./cmd/kanban-classifier/"]


CASES = [
    # ---- controls ----
    Case(
        "CONTROL known-positive: the board this binary writes every card to is renamed",
        [('defaultBoardName     = kanbanvocab.BoardAgentRuns', 'defaultBoardName     = "Agent run"')],
    ),
    Case(
        "CONTROL known-negative: reword the -board usage string",
        [('"Kanban board name to write cards to."', '"Name of the kanban board this binary files every session card onto."')],
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
        "the model's environment variable is renamed: a host's KANBAN_CLASSIFIER_MODEL override "
        "is refused as a misspelling and the job stops",
        [('modelDefinition("KANBAN_CLASSIFIER_MODEL", DefaultKanbanClassifierModel)',
          'modelDefinition("KANBAN_CLASSIFIER_MODEL_ID", DefaultKanbanClassifierModel)')],
    ),
    Case(
        "the default model drifts to a frontier model: same work, many times the bill, nothing fails",
        [('DefaultKanbanClassifierModel     = "claude-haiku-4-5-20251001"',
          'DefaultKanbanClassifierModel     = "claude-opus-4-1"')],
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
        [('DefaultKanbanStoreURL    = "http://localhost:8305"', 'DefaultKanbanStoreURL    = "http://localhost:8191"')],
    ),
    Case(
        "-bridge-url points at kanban-store: no session is ever listed, so no tick classifies anything",
        [('DefaultBridgeURL    = "http://localhost:8160"', 'DefaultBridgeURL    = "http://localhost:8305"')],
    ),
    # Two rows deleted on 2026-09-25 with their mechanism: -auth-store-url and
    # -auth-token existed to resolve an Anthropic key for a direct API call, and
    # bd8c36a replaced that call with llm-bridge-server's oneshot route.
    Case(
        "SCHEDULER_URL's default drifts to the kanban-store port: -register POSTs the job to the wrong service",
        [('DefaultSchedulerURL      = "http://localhost:8092"', 'DefaultSchedulerURL      = "http://localhost:8305"')],
    ),

    # ---- the wiring: which setting feeds which field ----
    #
    # Nothing downstream would reject a swap. Every setting is read with
    # settings.String and lands in a string field, so the compiler cannot tell
    # the kanban-store URL from the bridge URL or the model id.
    Case(
        "-kanban-url reads the bridge URL setting: both are base URLs and the compiler cannot tell them apart",
        [('settings.String(schedulerconfig.SettingKanbanStoreURL), "kanban-store base URL"',
          'settings.String(schedulerconfig.SettingBridgeURL), "kanban-store base URL"')],
    ),
    Case(
        "-register's scheduler URL is read from the kanban-store setting: the job is POSTed to kanban-store",
        [('c.SchedulerURL = settings.String(schedulerconfig.SettingSchedulerURL)',
          'c.SchedulerURL = settings.String(schedulerconfig.SettingKanbanStoreURL)')],
    ),
    Case(
        "the model id is read from the kanban-store URL setting: every classify call names a URL as its model",
        [('fs.StringVar(&c.Model, "model", settings.String(schedulerconfig.SettingModel)',
          'fs.StringVar(&c.Model, "model", settings.String(schedulerconfig.SettingKanbanStoreURL)')],
    ),
    # envOr's presence-versus-emptiness row was deleted on 2026-09-25: envOr is
    # gone, and an empty variable is now treated as unset inside
    # llm-bridge/servicesettings, another repo this scorer cannot edit.
    # TestAnEmptyEnvironmentVariableIsTreatedAsUnset still pins it from here.

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
        [('Command:     cfg.BinaryPath,', 'Command:     cfg.SchedulerURL,')],
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

    # ---- the per-tick spend cap (classifyTick) ----
    #
    # Ported 2026-09-25 from 99db69ad and 00a10531. The flag rows above pin the
    # cap's VALUE; these pin the loop that enforces it. It bounds a whole tick,
    # and selectCandidates returns everything updated in the last seven days, so
    # with it gone one tick classifies every session on the box instead of ten,
    # ninety-six times a day. It was unscoreable until it left run() for
    # classifyTick.
    Case(
        "GATE: the per-tick cap stops firing, so one tick bills for every candidate on the box",
        [("\t\tif classified >= maxClassifications {", "\t\tif classified >= maxClassifications && false {")],
    ),
    Case(
        "GATE: the per-tick cap is off by one, so every tick buys one classification it was not budgeted",
        [("\t\tif classified >= maxClassifications {", "\t\tif classified > maxClassifications {")],
    ),
    Case(
        "GATE: the per-tick cap fires one early, so the tick under-spends and defers work it had budget for",
        [("\t\tif classified >= maxClassifications {", "\t\tif classified+1 >= maxClassifications {")],
    ),
    Case(
        "a session a gate SKIPPED consumes the tick's budget, so a run of unchanged sessions eats the whole tick",
        [("\t\tif classifyOneSession(sess) {\n\t\t\tclassified++\n\t\t}",
          "\t\tclassifyOneSession(sess)\n\t\tclassified++")],
    ),
    Case(
        "the tick spends its budget on the TAIL of the candidate list, undoing the newest-first sort",
        [("\tfor i, sess := range candidates {",
          "\tfor i := range candidates {\n\t\tsess := candidates[len(candidates)-1-i]")],
    ),
    # Two edits: counting from `classified` orphans the loop index, and a
    # `declared and not used` compile error scores nothing at all.
    Case(
        "the deferral count is measured from the spend, inflating it by every session the gates skipped",
        [("\tfor i, sess := range candidates {", "\tfor _, sess := range candidates {"),
         ("len(candidates)-i", "len(candidates)-classified")],
    ),
    # The over-eager direction. The rows above break the cap by letting spend
    # through; this one stops the binary dead, and a classifier that classifies
    # nothing reads in journalctl exactly like a box with nothing to classify.
    Case(
        "GATE: the per-tick cap fires unconditionally, so the classifier never runs at all",
        [("\t\tif classified >= maxClassifications {", "\t\tif classified >= maxClassifications || true {")],
    ),

    # ---- what -register reads back, and the columns that decide whether the binary runs ----
    #
    # Ported 2026-09-25 from 348e0589. That commit's rows for the job's name,
    # cadence and command are left out: the rows above already edit the
    # constants they come from. Its envOr rows went with envOr.
    #
    # The registration mechanism itself (the duplicate check, the list-read
    # guards, the refusal boundary, the routes and the job type) is shared
    # with six other binaries since 2026-09-26 and scored once, in
    # scripts/sabotage-schedulerjobs.py. The rows here pin this binary's own
    # literals.
    Case(
        "a board missing a lifecycle column is accepted, so a classification has nowhere to go",
        [('if kc.ColumnsByName[n] == "" {', "if false {")],
    ),
    Case(
        "the missing-column error does not say which column, on a four-column check",
        [('return fmt.Errorf("column %q missing on board %s", n, kc.BoardID)',
          'return fmt.Errorf("board %s is missing a required column", kc.BoardID)')],
    ),

    # ---- the bridge client: three reads that check a status, then decode ----
    #
    # Ported from 348e0589 onto main's readers. Delete any status check below
    # and the call still fails, one step later, on the decode, so a test
    # asserting only `err != nil` is green on all three. getSession's route is
    # built in internal/bridgesession, which session_url_test.go pins.
    Case(
        "a bridge answering 500 with a valid session body is classified anyway",
        [('if resp.StatusCode != http.StatusOK {\n\t\tbody, _ := io.ReadAll(resp.Body)\n\t\treturn s, fmt.Errorf("get session %s: %s — %s", id, resp.Status, string(body))\n\t}',
          "")],
    ),
    Case(
        "a session list answered 500 is read as a box with no sessions on it",
        [('if resp.StatusCode != http.StatusOK {\n\t\tbody, _ := io.ReadAll(resp.Body)\n\t\treturn page, fmt.Errorf("list sessions: %s — %s", resp.Status, string(body))\n\t}',
          "")],
    ),
    Case(
        "an unreadable transcript is read as a session with no messages, and paid for",
        [('if resp.StatusCode != http.StatusOK {\n\t\tbody, _ := io.ReadAll(resp.Body)\n\t\treturn sessionPage{}, fmt.Errorf("get messages: %s — %s", resp.Status, string(body))\n\t}',
          "")],
    ),
    Case(
        "a session answered without an id leaves every later write keyed on the empty string",
        [('if s.SessionID == "" {\n\t\ts.SessionID = id\n\t}', "")],
    ),
    # The other direction of the same fallback. The case above deletes it and so
    # only asks the EMPTY direction; nothing asked what happens when the bridge
    # DOES supply an id. Making the assignment unconditional throws the bridge's
    # canonical id away on every read, and the classifier then keys its
    # signature, card link and do-not-track tag on whatever alias the caller
    # passed. Reddens TestASessionAnsweredWithAnIDKeepsTheBridgesOwn and no other
    # test. Found uncommitted in the scheduler-wt-pass148 worktree (2026-08-31)
    # and ported by the C3 close-out, card 2fab79e5.
    Case(
        "an id the bridge supplied is overwritten by the one it was asked for, so an alias becomes the key",
        [('if s.SessionID == "" {\n\t\ts.SessionID = id\n\t}', "s.SessionID = id")],
    ),
    Case(
        "the session list is asked for at a route the bridge does not serve",
        [('base := strings.TrimRight(bridgeURL, "/") + "/sessions/summary?limit=100"',
          'base := strings.TrimRight(bridgeURL, "/") + "/session/summary?limit=100"')],
    ),
    Case(
        "the transcript is asked for at a route the bridge does not serve",
        [('bridgesession.SubresourceURL(bridgeURL, id, "messages")', 'bridgesession.SubresourceURL(bridgeURL, id, "history")')],
    ),
    Case(
        "a base URL with a trailing slash doubles the separator in the session list's path",
        [('base := strings.TrimRight(bridgeURL, "/") + "/sessions/summary?limit=100"',
          'base := bridgeURL + "/sessions/summary?limit=100"')],
    ),

    # ---- the digest block, the schema, and the two tag helpers beside them ----
    #
    # Ported 2026-09-25 from d9ba6aa5 (test/classifier-digest-block-is-scored).
    # These functions are executed by the suite on its way to other assertions
    # and were observed by none of them. The digest is the prompt this binary
    # pays for; tagSet and applyDoNotTrack decide whether a session is ever
    # classified again. That commit's tool-name row is not ported: main sends
    # the schema to the oneshot route, and there is no tool to name.
    Case(
        "tagSet records every tag as ABSENT, so all three per-session gates read a session as never seen",
        [("\t\tout[t.Tag] = true", "\t\tout[t.Tag] = false")],
    ),
    Case(
        "tagSet returns an empty set, so a session's whole history — gates and signature alike — is invisible",
        [("\tfor _, t := range tags {\n\t\tout[t.Tag] = true\n\t}\n\treturn out",
          "\tfor _, t := range tags {\n\t\t_ = t\n\t}\n\treturn out")],
    ),
    Case(
        "an untrackable session is tagged final-classified instead of do-not-track, so it is never re-evaluated",
        [("if err := kc.AddEntityTag(cardLinkSession, sess.SessionID, tagDoNotTrack); err != nil {",
          "if err := kc.AddEntityTag(cardLinkSession, sess.SessionID, tagFinalClassified); err != nil {")],
    ),
    Case(
        "budgetShare hands back the WHOLE budget instead of its share, so one section can claim all of it",
        [("\treturn int(float64(total) * fraction)", "\treturn total")],
    ),
    Case(
        "budgetShare ignores the total and always reserves the default budget's share",
        [("\treturn int(float64(total) * fraction)",
          "\treturn int(float64(digestCharBudget) * fraction)")],
    ),
    Case(
        "budgetShare's non-positive guard stops firing",
        [("\tif total <= 0 {\n\t\treturn 0\n\t}", "\tif total <= 0 && false {\n\t\treturn 0\n\t}")],
        expected_unnoticed="redundant with the line below it, and unreachable besides. At total=0 both "
                           "paths return 0. Only total<0 differs, and no call site can pass one: "
                           "buildDigest replaces a non-positive charBudget with digestCharBudget and "
                           "clamps `remaining` to 0 before calling budgetShare. Falsified the day a "
                           "third caller passes an unclamped difference — then this becomes a real case.",
    ),
    Case(
        "the digest stops naming the session's bridge id, so the model cannot tell which session it is reading",
        [('\tfmt.Fprintf(&b, "bridge_id: %s\\n", sess.SessionID)\n', "")],
    ),
    Case(
        "the meta section reports the session's purpose where its state belongs, so a finished session reads as running",
        [('\tfmt.Fprintf(&b, "state: %s\\n", sess.State)',
          '\tfmt.Fprintf(&b, "state: %s\\n", sess.Purpose)')],
    ),
    Case(
        "messages_shown always reads zero, so every session looks like it has said nothing",
        [('\tfmt.Fprintf(&b, "messages_shown: %d\\n\\n", msgCount)',
          '\tfmt.Fprintf(&b, "messages_shown: %d\\n\\n", 0)')],
    ),
    Case(
        "the meta section loses its trailing blank line, so it runs into the section after it",
        [('\tfmt.Fprintf(&b, "messages_shown: %d\\n\\n", msgCount)',
          '\tfmt.Fprintf(&b, "messages_shown: %d\\n", msgCount)')],
    ),
    Case(
        "the USER PROMPTS section is filled with assistant turns, so the model reads its own replies as the ask",
        [('\t\tif m.Role == "user" && strings.TrimSpace(m.Content) != "" {',
          '\t\tif m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {')],
    ),
    Case(
        "an empty user message is rendered as a prompt, so the section fills with blanks the budget still pays for",
        [('\t\tif m.Role == "user" && strings.TrimSpace(m.Content) != "" {',
          '\t\tif m.Role == "user" {')],
    ),
    Case(
        "a user prompt is no longer bounded, so one pasted log can swallow the whole section",
        [("textcut.TruncateToByteBudgetOnRuneBoundary(m.Content, 1500)", "m.Content")],
    ),
    Case(
        "a session with no user prompts renders an EMPTY section instead of saying (none)",
        [('\tif len(entries) == 0 {\n\t\treturn header + "(none)\\n\\n"\n\t}\n'
          '\treturn renderSectionKeepingNewest(header, entries, budget, "earlier prompt(s) omitted")',
          '\tif len(entries) == 0 {\n\t\treturn ""\n\t}\n'
          '\treturn renderSectionKeepingNewest(header, entries, budget, "earlier prompt(s) omitted")')],
    ),
    # Both of these need the loop variable to stay referenced — `n += 0` and
    # `n++` orphan `e` and score `compile error`, which is not a score.
    Case(
        "totalLen reports zero, so card bodies are never shed and EXISTING CARDS overruns its reserve",
        [("\t\tn += len(e)", "\t\tn += 0 * len(e)")],
    ),
    Case(
        "totalLen counts entries rather than characters, so the shed decision is made on the wrong scale",
        [("\tfor _, e := range entries {\n\t\tn += len(e)\n\t}", "\tfor range entries {\n\t\tn++\n\t}")],
    ),
    Case(
        "the schema stops REQUIRING trackable, so a reply that omits it is read as untrackable and tags the session off",
        [('\t\t"required": []string{"trackable"},', '\t\t"required": []string{},')],
    ),
    Case(
        "existing_cards stops demanding EVERY card back, so the model may answer on a subset and the rest stay in the column they were filed in",
        [('"description": "One entry per pre-existing card on this session. Required: include every card you were given."',
          '"description": "One entry per pre-existing card on this session."')],
    ),
    Case(
        "a new task's state may be any string the model likes, so a state the applier cannot map reaches the board",
        [('\t\t\t\t\t\t"title":   map[string]any{"type": "string", "maxLength": 80},\n'
          '\t\t\t\t\t\t"state":   map[string]any{"type": "string", "enum": []string{"in_progress", "blocked", "done", "abandoned"}},',
          '\t\t\t\t\t\t"title":   map[string]any{"type": "string", "maxLength": 80},\n'
          '\t\t\t\t\t\t"state":   map[string]any{"type": "string"},')],
    ),
    Case(
        "an existing card's state may be any string the model likes, so a card is moved nowhere it was asked to go",
        [('\t\t\t\t\t\t"card_id": map[string]any{"type": "string"},\n'
          '\t\t\t\t\t\t"state":   map[string]any{"type": "string", "enum": []string{"in_progress", "blocked", "done", "abandoned"}},',
          '\t\t\t\t\t\t"card_id": map[string]any{"type": "string"},\n'
          '\t\t\t\t\t\t"state":   map[string]any{"type": "string"},')],
    ),
    Case(
        "a new task's title loses its length bound in the schema the model is shown",
        [('"title":   map[string]any{"type": "string", "maxLength": 80},',
          '"title":   map[string]any{"type": "string"},')],
    ),

    # ---- digest boundary VALUES ----
    #
    # Ported 2026-09-25 from c6e9e67f. Every digest row above moves a DIRECTION
    # or deletes a mechanism; none moves a boundary to a different NUMBER, and
    # measured 2026-08-14 all seven below were UNNOTICED. The cause was
    # homogeneous fixtures: a cut that never fires or always fires hides its
    # value. digest_boundaries_test.go straddles each bound with two rows one
    # byte apart.
    Case(
        "the user-prompt bound loosens to 1900, so a prompt overruns the reserve the section was sized for",
        [("textcut.TruncateToByteBudgetOnRuneBoundary(m.Content, 1500)", "textcut.TruncateToByteBudgetOnRuneBoundary(m.Content, 1900)")],
    ),
    Case(
        "the user-prompt bound tightens to 400, so the verbatim record of what was asked is cut to a quarter",
        [("textcut.TruncateToByteBudgetOnRuneBoundary(m.Content, 1500)", "textcut.TruncateToByteBudgetOnRuneBoundary(m.Content, 400)")],
    ),
    Case(
        "the assistant-turn bound loosens to 900, so ten turns can crowd out the prompts they answer",
        [("textcut.TruncateToByteBudgetOnRuneBoundary(m.Content, 600)", "textcut.TruncateToByteBudgetOnRuneBoundary(m.Content, 900)")],
    ),
    Case(
        "the existing-card body bound tightens to 200, halving how much of a card the model sees "
        "before it is asked to rewrite that card's body",
        [("textcut.TruncateToByteBudgetOnRuneBoundary(body, 400)", "textcut.TruncateToByteBudgetOnRuneBoundary(body, 200)")],
    ),
    Case(
        "the digest renders card titles cut at 40 while the schema and the applier still use 80, "
        "so two different cards can render as one string",
        [("textcut.TruncateToByteBudgetOnRuneBoundary(title, 80)", "textcut.TruncateToByteBudgetOnRuneBoundary(title, 40)")],
    ),
    Case(
        "the assistant-turn cap slips to 11, so every classified session bills for one turn more than the cap says",
        [("if len(withText) > 10 {\n\t\twithText = withText[len(withText)-10:]",
          "if len(withText) > 11 {\n\t\twithText = withText[len(withText)-11:]")],
    ),
    Case(
        "the tool-call cap slips to 16 in the one section that is handed no budget at all",
        [("if len(tools) > 15 {\n\t\ttools = tools[len(tools)-15:]",
          "if len(tools) > 16 {\n\t\ttools = tools[len(tools)-16:]")],
    ),
    # The boundary tests assert on a marked LAST CHARACTER, not on a length. A
    # cut that keeps the wrong end of the string produces a result of exactly
    # the right length, so a length-only assertion reports green.
    Case(
        "the cut keeps the END of an over-long value instead of its beginning, "
        "which no assertion on length alone can see",
        [("\treturn s[:cut] + Ellipsis", "\treturn s[len(s)-cut:] + Ellipsis")],
    ),

    # ---- THE QUIET SIDE ----
    #
    # Ported 2026-09-25 from 3aa6a3fc, which found that a case list scoring
    # 72/72 left 13 of 88 tests reddened by no case, and all 13 pinned the
    # quiet direction: the empty input, the zero, the boundary that should be
    # KEPT, the tick that should cost nothing. A case list inherits the worry
    # of the card that drafted it, so its blind side is the far side of that
    # worry. A classifier that classifies nothing logs `classified=0`, which is
    # also what a box with nothing to do logs.
    #
    # Four of its rows are not ported. The signature lost its minute stamp, so
    # the seconds row has nothing to edit. priorSigTag's and priorMsgCount's
    # no-signature rows would be equivalent mutants on main: a bare prefix and
    # an empty signature both parse as "no result, no count", and callers read
    # an event count only when `known` is true. The truncation guard moved into
    # internal/oneshot, which this scorer does not target.
    Case(
        "the lookback narrows to five days, so sessions still being worked on stop being candidates",
        [("candidateLookback         = 7 * 24 * time.Hour",
          "candidateLookback         = 5 * 24 * time.Hour")],
    ),
    Case(
        "the do-not-track delta turns inclusive, so a session that grew by exactly the threshold is still skipped",
        [("} else if eventCount-priorN < doNotTrackReevalEvents {",
          "} else if eventCount-priorN <= doNotTrackReevalEvents {")],
    ),
    Case(
        "the once-per-response gate skips any session whose signature names a response, not just this one",
        [("\treturn ok && recorded == finishedResponseID", "\treturn ok && (recorded == finishedResponseID || true)")],
    ),
    Case(
        "the once-per-response gate also skips the never-signed, so no session is ever classified a first time",
        [("\treturn ok && recorded == finishedResponseID", "\treturn !ok || recorded == finishedResponseID")],
    ),
    Case(
        "any session with a state at all is retired as final-classified, including one still running",
        [("if sess.State == terminalStateCompleted || sess.State == terminalStateError {",
          'if sess.State != "" {')],
    ),
    Case(
        "an empty card list renders no (none), so a session with no cards reads like a digest missing its cards section",
        [('if len(existingCards) == 0 {\n\t\treturn header + "(none)\\n\\n"\n\t}',
          'if len(existingCards) == 0 {\n\t\treturn header + "\\n"\n\t}')],
    ),
    Case(
        "a session with no tool calls renders no (none), so it reads like a digest missing its tool section",
        [('fmt.Fprintf(&b, "(none)\\n")', 'fmt.Fprintf(&b, "\\n")')],
    ),
    Case(
        "fitKeepingNewest stops one short, so it sheds the oldest entry and announces an omission on a digest that fitted",
        [("for i := len(entries) - 1; i >= 0; i-- {",
          "for i := len(entries) - 1; i >= 1; i-- {")],
    ),
    Case(
        "the tick's spend counter starts at one, so every tick reports a classification it never made",
        [("classifyOneSession func(session) bool) int {\n\tclassified := 0",
          "classifyOneSession func(session) bool) int {\n\tclassified := 1")],
    ),
    # ---- THE TICK'S OWN LOGIC ----
    #
    # Added 2026-09-27 (card 68161ab4). The first --crosstable taken on main
    # (bff5fc4) found 58 of 159 tests reddened by no case: the per-session gates,
    # the signature helpers, the card apply path, the bridge readers, the model
    # call and part of the digest had no row since C3b re-aimed the list. Each
    # row below was aimed at one of those 58 and checked against main's code,
    # not copied from the 08-31 branch: most of that branch's needles named code
    # that has since moved (the Anthropic call, auth-store, registration).

    # ---- the card apply path: dedupe, columns, links and the provenance trailer ----
    Case(
        'the last-resort dedupe stops firing, so a task already on the board is filed again',
        [('\t\tif filed[normalizeTitle(nt.Title)] {\n\t\t\tlog.Printf(',
          '\t\tif false && filed[normalizeTitle(nt.Title)] {\n\t\t\tlog.Printf(')],
    ),
    Case(
        'a task filed this tick is not remembered, so one report naming it twice files it twice',
        [('\t\tfiled[normalizeTitle(nt.Title)] = true\n\n\t\tcolName := stateToColumn(nt.State)',
          '\t\t_ = filed\n\n\t\tcolName := stateToColumn(nt.State)')],
    ),
    Case(
        'the source link is written with no value, so a classifier card cannot be told from a hand-filed one',
        [('kc.AddCardLink(cardID, cardLinkSource, cardSourceClassifier, "")',
          'kc.AddCardLink(cardID, cardLinkSource, "", "")')],
    ),
    Case(
        'the id-keyed session link carries the display name, so nothing can join a created card to its session',
        [('kc.AddCardLink(cardID, cardLinkSession, sess.SessionID, "classifier")',
          'kc.AddCardLink(cardID, cardLinkSession, sess.DisplayName, "classifier")')],
    ),
    Case(
        'an unrecognised state lands in Done instead of defaulting to In Progress',
        [('\tcase "abandoned":\n\t\treturn kanbanvocab.ColumnFailed\n\tdefault:\n\t\treturn kanbanvocab.ColumnInProgress',
          '\tcase "abandoned":\n\t\treturn kanbanvocab.ColumnFailed\n\tdefault:\n\t\treturn kanbanvocab.ColumnDone')],
    ),
    Case(
        'normalizeTitle drops digits, so two titles differing only by a number dedupe into one',
        [("case r >= 'a' && r <= 'z', r >= '0' && r <= '9':",
          "case r >= 'a' && r <= 'z':")],
    ),
    Case(
        'normalizeTitle stops folding case, so Fix X and fix x are two different tasks',
        [('for _, r := range strings.ToLower(strings.TrimSpace(title)) {',
          'for _, r := range strings.TrimSpace(title) {')],
    ),
    Case(
        'a card whose column is missing is moved anyway instead of being skipped',
        [('\t\tif !ok {\n\t\t\tlog.Printf("[%s] column %q missing for existing card update; skipping"',
          '\t\tif !ok && false {\n\t\t\tlog.Printf("[%s] column %q missing for existing card update; skipping"')],
    ),
    Case(
        'a card whose move failed has its body rewritten anyway, so the body and the column disagree',
        [('degraded.Record("session %s: card %s not moved to %q", sess.SessionID, ec.CardID, colName)\n\t\t\tcontinue',
          'degraded.Record("session %s: card %s not moved to %q", sess.SessionID, ec.CardID, colName)')],
    ),
    Case(
        'a card that could not be moved is logged and not recorded, so the run reports success',
        [('degraded.Record("session %s: card %s not moved to %q", sess.SessionID, ec.CardID, colName)\n',
          '\n')],
    ),
    Case(
        "an updated card body drops the model's summary and keeps only the trailer",
        [('\t\tbody := ec.Summary\n\t\tif ec.Blocker',
          '\t\tbody := ""\n\t\tif ec.Blocker')],
    ),
    Case(
        "an update carries the summary twice, so the body is no longer this tick's summary and trailer alone",
        [('body += "\\n\\n" + provenanceTrailer("updated by", sess.SessionID, ec.State) + "\\n"',
          'body += "\\n\\n" + provenanceTrailer("updated by", sess.SessionID, ec.State) + "\\n" + ec.Summary')],
    ),
    Case(
        'the update trailer names the display name where the session id belongs',
        [('provenanceTrailer("updated by", sess.SessionID, ec.State)',
          'provenanceTrailer("updated by", sess.DisplayName, ec.State)')],
    ),
    Case(
        'the create trailer names the display name where the session id belongs',
        [('provenanceTrailer("created by", sess.SessionID, nt.State)',
          'provenanceTrailer("created by", sess.DisplayName, nt.State)')],
    ),
    Case(
        'the provenance trailer drops its state field',
        [('return fmt.Sprintf("_%s classifier @ %s · session=%s · state=%s_",\n\t\taction, time.Now().UTC().Format(time.RFC3339), sessionID, state)',
          'return fmt.Sprintf("_%s classifier @ %s · session=%s_",\n\t\taction, time.Now().UTC().Format(time.RFC3339), sessionID)')],
    ),
    Case(
        'an update reads the card back before replacing it',
        [('\t\tbody := ec.Summary\n\t\tif ec.Blocker',
          '\t\t_, _ = kc.ListCardsForEntity(cardLinkSession, sess.SessionID)\n\t\tbody := ec.Summary\n\t\tif ec.Blocker')],
    ),

    # ---- signatures and the per-session gates ----
    Case(
        'newerSig prefers the signature with FEWER events, so the stale one wins',
        [('\tif countA != countB {\n\t\treturn countA > countB',
          '\tif countA != countB {\n\t\treturn countA < countB')],
    ),
    Case(
        'newerSig ranks a signature with no event count above one that has a count',
        [('\tif knownA != knownB {\n\t\treturn knownA',
          '\tif knownA != knownB {\n\t\treturn knownB')],
    ),
    Case(
        'equal event counts no longer break the tie, so the pick depends on map order',
        [('\t\treturn countA > countB\n\t}\n\treturn a > b',
          '\t\treturn countA > countB\n\t}\n\treturn true')],
    ),
    Case(
        'priorSigTag counts every tag as a signature, so a session with none reports one',
        [('\t\tif !strings.HasPrefix(tag, tagCheckSigPrefix) {\n\t\t\tcontinue\n\t\t}\n\t\tfound++',
          '\t\tfound++')],
    ),
    Case(
        'a signature delete that fails is logged and not recorded',
        [('\t\t\t\tdegraded.Record("session %s: superseded check signature not deleted, it now carries more than one", sess.SessionID)\n',
          '')],
    ),
    Case(
        'a signature that did not land is logged and not recorded',
        [('\t\tdegraded.Record("session %s: check signature not written, it will be re-classified and re-billed next tick", sess.SessionID)\n',
          '')],
    ),
    Case(
        'a session mid-turn is classified anyway, so a half-finished answer is billed',
        [('\treturn state.IsActive() || (state.IsBlockedOnUser() && state != msg.SessionAwaitingUser)',
          '\treturn state.IsActive()')],
    ),
    Case(
        "the lookback measures the row's updated_at only, so a backfilled row reads as a live conversation",
        [('\treturn !newestEntryAt.IsZero() && newestEntryAt.Before(cutoff)',
          '\treturn false')],
    ),
    Case(
        'the interactive filter keeps every session type',
        [('\t\tif msg.SessionType(sess.Type) != msg.SessionTypeInteractive {\n\t\t\tskippedByType[sess.Type]++',
          '\t\tif false {\n\t\t\tskippedByType[sess.Type]++')],
    ),
    Case(
        'the dropped sessions are all counted under no type',
        [('\t\t\tskippedByType[sess.Type]++',
          '\t\t\tskippedByType[""]++')],
    ),
    Case(
        'an event count with trailing garbage is read as a number',
        [('\t\t\tn, err := strconv.Atoi(value)\n\t\t\treturn n, err == nil',
          "\t\t\tn, err := strconv.Atoi(strings.TrimRightFunc(value, func(r rune) bool { return r < '0' || r > '9' }))\n\t\t\treturn n, err == nil")],
    ),
    Case(
        'the processSession mid-turn gate stops firing, so a session part-way through a turn is billed',
        [('\tif turnInFlight(msg.SessionState(sess.State)) {',
          '\tif false && turnInFlight(msg.SessionState(sess.State)) {')],
    ),
    Case(
        'the once-per-response gate reads the whole signature as the response id, so a later event count reads as a new response',
        [('\t\tif value, found := strings.CutPrefix(field, "result="); found && value != "" {\n\t\t\treturn value, true',
          '\t\tif value, found := strings.CutPrefix(field, "result="); found && value != "" {\n\t\t\treturn value + strings.TrimPrefix(sig, tagCheckSigPrefix+"result="+value), true')],
    ),

    # ---- shortfalls a tick must record ----
    Case(
        'an unreadable session page is logged and not recorded',
        [('\t\tdegraded.Record("session %s not classified: reading its messages failed", sess.SessionID)\n',
          '')],
    ),
    Case(
        'a board whose tags cannot be read is logged and not recorded',
        [('\t\tdegraded.Record("session %s not classified: list tags failed", sess.SessionID)\n',
          '')],
    ),

    # ---- the bridge readers ----
    Case(
        'the newest finished response is the FIRST result on the page, not the last',
        [('\t\t\tif e.Role == "assistant" && e.Kind == "result" && e.Text != "" {\n\t\t\t\tnewest = id',
          '\t\t\tif e.Role == "assistant" && e.Kind == "result" && e.Text != "" && newest == "" {\n\t\t\t\tnewest = id')],
    ),
    Case(
        'a result with no text counts as a finished response',
        [('\t\t\tif e.Role == "assistant" && e.Kind == "result" && e.Text != "" {\n\t\t\t\tnewest = id',
          '\t\t\tif e.Role == "assistant" && e.Kind == "result" {\n\t\t\t\tnewest = id')],
    ),
    Case(
        'newestMessageTime keeps the oldest timestamp',
        [('\t\tif at.After(newest) {\n\t\t\tnewest = at',
          '\t\tif newest.IsZero() {\n\t\t\tnewest = at')],
    ),
    Case(
        'an unparseable entry timestamp is skipped instead of refused',
        [('\t\t\treturn time.Time{}, fmt.Errorf("entry timestamp %q: %w", m.Timestamp, err)',
          '\t\t\tcontinue')],
    ),
    Case(
        'digestMessages keeps the hidden duplicate copy of a prompt',
        [('\t\t\tif !ok || e.Duplicate {\n\t\t\t\tcontinue\n\t\t\t}\n\t\t\tswitch {',
          '\t\t\tif !ok {\n\t\t\t\tcontinue\n\t\t\t}\n\t\t\tswitch {')],
    ),
    Case(
        'digestMessages drops tool calls',
        [('\t\t\tcase e.Kind == "tool_call" && e.ToolName != "":',
          '\t\t\tcase false:')],
    ),
    Case(
        'getSession fills in the asked-for id when the bridge answers with no session',
        [('\tif s.SessionID == "" {\n\t\ts.SessionID = id\n\t}\n\treturn s, nil',
          '\tif s.SessionID == "" {\n\t\ts.SessionID = id\n\t}\n\ts.SessionID = id\n\treturn s, nil')],
    ),
    Case(
        'getSession accepts a non-200 answer',
        [('\t\treturn s, fmt.Errorf("get session %s: %s — %s", id, resp.Status, string(body))',
          '\t\t_ = body')],
    ),
    Case(
        'the single-session read builds its URL by hand, so an id is not escaped and an empty one is sent',
        [('\tsessionURL, err := bridgesession.URL(bridgeURL, id)',
          '\tsessionURL, err := strings.TrimRight(bridgeURL, "/")+"/sessions/"+id, error(nil)')],
    ),
    Case(
        'the messages read builds its URL by hand, so an id that names no session is sent anyway',
        [('\tmessagesURL, err := bridgesession.SubresourceURL(bridgeURL, id, "messages")',
          '\tmessagesURL, err := strings.TrimRight(bridgeURL, "/")+"/sessions/"+id+"/messages", error(nil)')],
    ),

    # ---- the model call and what the model is told ----
    Case(
        'the system prompt is sent as the user message and the digest as the system prompt',
        [('\t}, systemPrompt, digest, reportSchema(), &rep); err != nil {',
          '\t}, digest, systemPrompt, reportSchema(), &rep); err != nil {')],
    ),
    Case(
        'the report schema sent is empty',
        [('\t}, systemPrompt, digest, reportSchema(), &rep); err != nil {',
          '\t}, systemPrompt, digest, map[string]any{}, &rep); err != nil {')],
    ),
    Case(
        'max_tokens is sent as something other than the constant the truncation warning quotes',
        [('\t\tMaxTokens:  maxOutputTokens,',
          '\t\tMaxTokens:  maxOutputTokens / 2,')],
    ),
    Case(
        'a truncated report is no longer recognised as one, so it reads as an ordinary failure',
        [('var errTruncatedReport = oneshot.ErrTruncated',
          'var errTruncatedReport = errors.New("report truncated")')],
    ),
    Case(
        'a failed model call returns an error that names no HTTP status',
        [('\t}, systemPrompt, digest, reportSchema(), &rep); err != nil {\n\t\treturn nil, err',
          '\t}, systemPrompt, digest, reportSchema(), &rep); err != nil {\n\t\treturn nil, errors.New("classify failed")')],
    ),
    Case(
        'the prompt stops promising the model the card bodies',
        [('- Any cards already linked to this session, with their current state and body',
          '- Any cards already linked to this session, with their current state')],
    ),
    Case(
        'summary stops being required on an existing-card record, so a model can decline to replace a body',
        [('"required": []string{"card_id", "state", "summary"},',
          '"required": []string{"card_id", "state"},')],
    ),
    Case(
        'the existing-card record grows a prior-body field',
        [('\tBlocker string `json:"blocker,omitempty"`\n}\n\ntype NewRec struct {',
          '\tBlocker string `json:"blocker,omitempty"`\n\tBody    string `json:"body,omitempty"`\n}\n\ntype NewRec struct {')],
    ),
    Case(
        'classify refuses a complete report that carries no existing cards, as if emptiness meant truncation',
        [('\t\treturn nil, err\n\t}\n\treturn &rep, nil\n}',
          '\t\treturn nil, err\n\t}\n\tif len(rep.ExistingCards) == 0 {\n\t\treturn nil, errTruncatedReport\n\t}\n\treturn &rep, nil\n}')],
    ),

    # ---- the digest builder ----
    Case(
        'fitKeepingNewest drops the newest entry when it alone exceeds the budget',
        [('\t\tif costOfKeeping > budget && i != len(entries)-1 {',
          '\t\tif costOfKeeping > budget {')],
    ),
    Case(
        'the existing-cards section goes back to the END of the digest, where it falls off',
        [('\treturn meta + cards + prompts + turns + tools',
          '\treturn meta + prompts + turns + tools + cards')],
    ),
    Case(
        'the tool section keeps the first fifteen calls instead of the last fifteen',
        [('\t\ttools = tools[len(tools)-15:]',
          '\t\ttools = tools[:15]')],
    ),
    Case(
        'a session with no assistant text renders an empty section',
        [('return header + "(no assistant text content)\\n\\n"',
          'return header + "\\n"')],
    ),

    # ---- run, main and the settings refusal ----
    Case(
        'the single-session path exits 0 whatever it recorded',
        [('\t\tprocessSession(ctx, cfg, kc, sess, degraded)\n\t\treturn degraded.ExitCode()',
          '\t\tprocessSession(ctx, cfg, kc, sess, degraded)\n\t\treturn 0')],
    ),
    Case(
        "main discards run's status",
        [('\tos.Exit(run())',
          '\trun()')],
    ),
    Case(
        'a refused settings registry is no longer marked as a refusal',
        [('return config{}, fmt.Errorf("%w: %v", errSettingsRefused, err)',
          'return config{}, fmt.Errorf("settings: %v", err)')],
    ),

]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
