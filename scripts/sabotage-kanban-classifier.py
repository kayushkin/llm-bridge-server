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

# The defaults and the variable names this binary starts with live in
# internal/config since configuration moved to llm-bridge/servicesettings, so a
# row about a shipped value has to edit them there. PACKAGES stays the one
# binary: the question is whether ITS tests notice, not internal/config's.
TARGETS = [
    REPO / "cmd/kanban-classifier/main.go",
    REPO / "internal/config/jobs.go",
    REPO / "internal/config/settings.go",
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
    Case(
        "the registered job is a type the scheduler cannot run",
        [('"type":        "shell",', '"type":        "agent",')],
    ),
    Case(
        "an existing tick is not recognised, so -register stacks a second one on every deploy",
        [("if j.Name == schedulerJobName {", 'if j.Name == schedulerJobName+"-hourly" {')],
    ),
    Case(
        "the job-name check matches by prefix, so a neighbouring job proves the tick is registered",
        [("if j.Name == schedulerJobName {", 'if strings.HasPrefix(j.Name, "kanban-classifier") {')],
    ),
    Case(
        "a scheduler that refuses the job is reported as a successful registration",
        [('if resp.StatusCode >= 300 {\n\t\treturn fmt.Errorf("scheduler returned %d: %s", resp.StatusCode, string(body))\n\t}',
          "")],
    ),
    # The finding 348e0589 was filed for. An unreadable job list left `jobs`
    # empty, which is byte-identical to "no classifier tick on this box" — and
    # the next statement registers one.
    Case(
        "a job list answered 500 is read as a box with no classifier tick on it",
        [('if resp.StatusCode != http.StatusOK {\n\t\treturn fmt.Errorf("list jobs: scheduler returned %d: %s",\n\t\t\tresp.StatusCode, strings.TrimSpace(string(body)))\n\t}',
          "")],
    ),
    Case(
        "a job list that is not a JSON array is read as a box with no classifier tick on it",
        [('if err := json.Unmarshal(body, &jobs); err != nil {\n\t\treturn fmt.Errorf("list jobs: %w", err)\n\t}',
          "_ = json.Unmarshal(body, &jobs)")],
    ),
    Case(
        "the scheduler is reached at a path it does not serve",
        [('resp, err := http.Get(schedURL + "/api/jobs")', 'resp, err := http.Get(schedURL + "/api/job")')],
    ),
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
]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
