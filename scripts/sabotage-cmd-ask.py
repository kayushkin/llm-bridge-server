#!/usr/bin/env python3
"""Score cmd/ask's tests by breaking the binary on purpose.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-cmd-ask.py [--diffs|--crosstable]

Ported 2026-09-25 from test/first-tests-for-ask (338ec17, 4a6aef1, c11a09c,
3f441d4) onto main, whose ask had since gained the --session reuse path and
moved display-name shaping into its own functions. Dropped: the truncate and
envOr cases (main has neither function; display_name_test.go pins the display
name). Added: the reuse path's cases.

Scope is runAsk, publishInbound, resolveInstance and buildPrompt, and since
2026-09-26 the display name (displayNameFromQuestion, displayNameFromTitle,
resolveDisplayName and the shaping under them) and the --from guard
(validateAgentName).

Measured 2026-09-25 on main 26178ce, before the display-name cases: plain score
43/43, both controls behaved; --crosstable exited 1 with 40 of 70 tests reddened
by no case — exactly the tests in display_name_test.go and agent_name_test.go.

Measured 2026-09-26 after adding 33 cases for those functions: plain score
76/76, both controls behaved. --crosstable left one test silent,
TestWhitespaceSweep_CoversTheWholeClass, which calls nothing in main.go and is
declared in UNREDDENED with that reason. Every other test is reddened by at
least one case. Re-run the mode rather than trusting this paragraph; it is a
record of a measurement, not the measurement.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

TARGET = REPO / "cmd/ask/main.go"
PACKAGES = ["./cmd/ask/"]

DRY_RUN_REPORT = ('fmt.Fprintf(out, "instance: %s\\nfrom:     %s\\nsession:  %s<ns>\\ndisplay:  %s\\n\\n--- prompt ---\\n%s\\n",')
PUBLISH = "if err := pub.Publish(messages.BridgeInboundSubject, in); err != nil {"


CASES = [
    # ---- controls ----
    Case(
        "CONTROL known-positive: the run never publishes anything",
        [(PUBLISH, "if err := func() error { _ = in; return nil }(); err != nil {")],
    ),
    Case(
        "CONTROL known-negative: reword the dry-run report's field labels",
        [(DRY_RUN_REPORT,
          'fmt.Fprintf(out, "host: %s\\nsender: %s\\nid: %s<ns>\\ntitle: %s\\n\\n--- prompt ---\\n%s\\n",')],
        expected_unnoticed="cosmetic labels in a human-facing report; the tests assert on the "
                           "values it carries, not the words around them",
    ),

    # ---- wire values another program reads back ----
    Case(
        "the bus subject drifts from the one the adapter subscribes to",
        [(PUBLISH, 'if err := pub.Publish("bridge.in", in); err != nil {')],
    ),
    Case("the session Channel drifts", [('heraldChannel = "herald"', 'heraldChannel = "heralds"')]),
    Case("the session Purpose drifts", [('heraldPurpose = "herald"', 'heraldPurpose = "relay"')]),
    Case("the session Origin drifts", [('askOrigin = "ask"', 'askOrigin = "asker"')]),
    Case(
        "the orchestrator name drifts",
        [('orchestratorClaudeCode = "claude-code"', 'orchestratorClaudeCode = "claude_code"')],
    ),
    Case(
        "the session Type stops being the one isUnattendedSession accepts",
        [("Type:      msg.SessionTypeHerald,", "Type:      msg.SessionTypeAutonomous,")],
    ),
    Case(
        "the harness drifts to one the instance does not run",
        [("Harness:     msg.HarnessClaudeCode,\n\t\t\tInstanceID:  instanceID,",
          "Harness:     msg.HarnessCodex,\n\t\t\tInstanceID:  instanceID,")],
    ),
    Case(
        "the session-id prefix collides with the curator's idle-reaper scope",
        [('heraldSessionPrefix = "herald-"', 'heraldSessionPrefix = "autoworker-"')],
    ),
    Case("the default --from tag drifts", [('defaultFromTag = "herald"', 'defaultFromTag = "unknown"')]),
    Case("AutoStart is dropped, so the relay turn never begins", [("AutoStart: true,", "AutoStart: false,")]),
    Case(
        "a new session carries someone else's clock",
        [("Timestamp:    cfg.now,\n\t\tCreate:", "Timestamp:    time.Time{},\n\t\tCreate:")],
    ),

    # ---- refusals ----
    Case(
        "an unreadable question is accepted and a session is spawned for it",
        [("if !hasReadableText(cfg.question) {", "if false {")],
    ),
    Case(
        "the --from guard is skipped",
        [('if err := validateAgentName(cfg.from); err != nil {\n\t\treturn "", err\n\t}', "")],
    ),
    Case(
        "an unresolvable instance no longer stops the run",
        [('if err != nil {\n\t\treturn "", fmt.Errorf("resolve instance: %w", err)\n\t}', "_ = err")],
    ),
    Case(
        "a failed connection is swallowed and the run continues",
        [('if err != nil {\n\t\treturn fmt.Errorf("nats connect: %w", err)\n\t}', "_ = err")],
    ),
    Case(
        "a refused publish is reported as success",
        [(PUBLISH + '\n\t\treturn fmt.Errorf("publish: %w", err)\n\t}',
          "_ = pub.Publish(messages.BridgeInboundSubject, in)")],
    ),
    Case(
        "a refused publish still hands back a new session's id",
        [('if err != nil {\n\t\treturn "", err\n\t}\n\treturn sessionID, nil', "return sessionID, err")],
    ),
    Case("the connection is never closed", [("defer pub.Close()", "")]),

    # ---- the dry run ----
    Case(
        "-dry-run publishes anyway",
        [("instanceID, cfg.from, heraldSessionPrefix, displayName, prompt)\n\t\treturn \"\", nil",
          "instanceID, cfg.from, heraldSessionPrefix, displayName, prompt)")],
    ),
    Case(
        # The edit has to land BEFORE the dry-run early return. Placed after it,
        # the mutation is unreachable on a dry run and scores UNNOTICED for a
        # reason that has nothing to do with the tests.
        "-dry-run opens a connection it never uses",
        [("\tif cfg.dryRun {\n\t\t" + DRY_RUN_REPORT,
          "\t_, _ = connect()\n\tif cfg.dryRun {\n\t\t" + DRY_RUN_REPORT)],
    ),

    # ---- the display name, as runAsk uses it ----
    Case(
        "--title is ignored and the question is always used",
        [("resolveDisplayName(cfg.title, cfg.question)", 'resolveDisplayName("", cfg.question)')],
    ),

    # ---- --session reuse ----
    Case(
        "reuse accepts -instance and ignores it",
        [('if cfg.instanceName != "" {\n\t\t\treturn', 'if false {\n\t\t\treturn')],
    ),
    Case(
        "reuse accepts -title and ignores it",
        [('if cfg.title != "" {\n\t\t\treturn', 'if false {\n\t\t\treturn')],
    ),
    Case(
        "a reuse dry run publishes anyway",
        [('cfg.reuseSession, cfg.from, prompt)\n\t\t\treturn "", nil', "cfg.reuseSession, cfg.from, prompt)")],
    ),
    Case(
        "reuse looks up an instance it has no use for",
        [("\t\terr := publishInbound(connect, messages.BridgeInbound{\n\t\t\tBusSessionID: cfg.reuseSession,",
          "\t\t_, _ = resolveInstance(cfg.bridgeURL, \"\")\n\t\terr := publishInbound(connect, messages.BridgeInbound{\n\t\t\tBusSessionID: cfg.reuseSession,")],
    ),
    Case(
        "reuse delivers to a fresh id instead of the named session",
        [("BusSessionID: cfg.reuseSession,", "BusSessionID: heraldSessionPrefix,")],
    ),
    Case(
        "reuse reports no session id, so the caller cannot reuse it again",
        [("return cfg.reuseSession, nil", 'return "", nil')],
    ),
    Case(
        "a refused reuse publish still hands back the session id",
        [('if err != nil {\n\t\t\treturn "", err\n\t\t}\n\t\treturn cfg.reuseSession, nil',
          "return cfg.reuseSession, err")],
    ),

    # ---- resolveInstance ----
    #
    # The three cases below were added after --crosstable found this list
    # scoring 39/39 with three resolveInstance tests reddened by nothing. Every
    # case here aimed at a REFUSAL — the ambiguous name, the ambiguous default,
    # the three gates — because each was drafted from a card worried about the
    # wrong instance being picked. The paths where resolveInstance is meant to
    # SUCCEED, and the one where it is meant to report a dead server, had no
    # case at all. A case list inherits the worry of the card that drafted it.
    Case(
        "a unique --instance name resolves to the wrong row's id",
        [("\t\tcase 1:\n\t\t\treturn matches[0], nil",
          "\t\tcase 1:\n\t\t\treturn instances[0].ID, nil")],
    ),
    Case(
        # The dangerous half of this is not the missing error. It is where
        # control goes instead: out of the switch, past the `if name != ""`
        # block, and into the no-name search — so a typo'd --instance silently
        # routes the question to whatever local claude_code instance is up.
        "an --instance name no row carries falls through to the local default",
        [('\t\tcase 0:\n\t\t\treturn "", fmt.Errorf("no instance named %q", name)',
          "\t\tcase 0:\n\t\t\t// fall through to the local default")],
    ),
    Case(
        "a bridge that cannot be reached is reported as success with no instance",
        [('\tresp, err := http.Get(url)\n\tif err != nil {\n\t\treturn "", err\n\t}',
          '\tresp, err := http.Get(url)\n\tif err != nil {\n\t\treturn "", nil\n\t}')],
    ),
    Case(
        "an ambiguous --instance name silently picks the first match",
        [('\t\tdefault:\n\t\t\treturn "", fmt.Errorf("%d instances named %q — name must be unique", len(matches), name)',
          "\t\tdefault:\n\t\t\treturn matches[0], nil")],
    ),
    Case(
        "two local claude_code instances silently pick the first",
        [('\tdefault:\n\t\treturn "", fmt.Errorf("%d local claude_code instances — pass --instance <name> to disambiguate", len(matches))',
          "\tdefault:\n\t\treturn matches[0], nil")],
    ),
    Case(
        "the harness-type gate is dropped, so a codex instance can host a herald turn",
        [("if i.HarnessType != string(msg.HarnessClaudeCode) || !i.Enabled {", "if !i.Enabled {")],
    ),
    Case(
        "the enabled gate is dropped, so a disabled instance is chosen",
        [("if i.HarnessType != string(msg.HarnessClaudeCode) || !i.Enabled {",
          "if i.HarnessType != string(msg.HarnessClaudeCode) {")],
    ),
    Case(
        "the local-transport gate is dropped, so the question routes to another machine",
        [('if i.Machine == nil || i.Machine.Transport != "local" {\n\t\t\tcontinue\n\t\t}', "")],
    ),
    Case(
        "a non-OK status is accepted as an empty instance list",
        [('return "", fmt.Errorf("GET /instances: HTTP %s — %s", resp.Status, strings.TrimSpace(string(body)))',
          'return "", fmt.Errorf("GET /instances failed")')],
    ),
    Case(
        "an undecodable body is treated as no instances",
        [('return "", fmt.Errorf("decode instances: %w", err)', "instances = nil")],
    ),

    # ---- the prompt ----
    Case(
        "the prompt stops telling the agent not to answer its own question",
        [("Do not answer it yourself or take any action yet.", "Answer it as best you can.")],
    ),
    Case(
        "the --on-reply instruction is dropped from the prompt",
        [("b.WriteString(strings.TrimSpace(onReply))", "")],
    ),
    Case(
        "a whitespace-only --on-reply is treated as a real instruction",
        [('if strings.TrimSpace(onReply) != "" {', 'if onReply != "" {')],
    ),
    Case(
        "the question itself is left out of the prompt",
        [("b.WriteString(question)", "")],
    ),
    Case(
        "the sender attribution is dropped from the prompt",
        [("`+\"`%s`\"+` to the user, then wait.", "` + \"`someone`\" + ` to the user, then wait."),
         # Second edit: dropping the %s verb orphans the argument, and go test
         # runs vet's printf check, so without this the case reports a compile
         # error instead of a score.
         ("`, from)", "`)")],
    ),

    # ---- the display name derived from a question ----
    Case(
        "the display-name bound drifts off sixty characters",
        [("const displayNameRuneLimit = 60", "const displayNameRuneLimit = 61")],
    ),
    Case(
        "the bound grows past any question a caller writes, so no fixture straddles it",
        [("const displayNameRuneLimit = 60", "const displayNameRuneLimit = 1000")],
    ),
    Case(
        # The constant stays 60 here, so only a test that measures the name can
        # tell. Cutting at a huge limit rather than deleting the call keeps the
        # textcut import in use.
        "a derived name is never cut",
        [("return textcut.TruncateToRuneLimit(shapeIntoOneLineOfText(question), displayNameRuneLimit)",
          "return textcut.TruncateToRuneLimit(shapeIntoOneLineOfText(question), 1<<30)")],
    ),
    Case(
        # The byte-count defect this test was written for lives in textcut, which
        # is not a TARGET here. At this call site the nearest break is a cut that
        # spends one character less than the bound, so a question of exactly 60
        # characters comes back shortened.
        "a derived name is cut one character short of the bound",
        [("return textcut.TruncateToRuneLimit(shapeIntoOneLineOfText(question), displayNameRuneLimit)",
          "return textcut.TruncateToRuneLimit(shapeIntoOneLineOfText(question), displayNameRuneLimit-1)")],
    ),
    Case(
        "the cut runs before the shaping, so whitespace spends the reading budget",
        [("return textcut.TruncateToRuneLimit(shapeIntoOneLineOfText(question), displayNameRuneLimit)",
          "return shapeIntoOneLineOfText(textcut.TruncateToRuneLimit(question, displayNameRuneLimit))")],
    ),
    Case(
        "a derived name is cut but never shaped",
        [("return textcut.TruncateToRuneLimit(shapeIntoOneLineOfText(question), displayNameRuneLimit)",
          "return textcut.TruncateToRuneLimit(question, displayNameRuneLimit)")],
    ),

    # ---- the display name stated with --title ----
    Case(
        "a --title is stored exactly as typed",
        [("\treturn shapeIntoOneLineOfText(title)", "\treturn title")],
    ),
    Case(
        "a --title is cut at the derived name's bound",
        [("\treturn shapeIntoOneLineOfText(title)",
          "\treturn textcut.TruncateToRuneLimit(shapeIntoOneLineOfText(title), displayNameRuneLimit)")],
    ),

    # ---- which name wins ----
    Case(
        "the choice is made on the raw title, so a title of only control characters wins",
        [('if shaped := displayNameFromTitle(title); shaped != "" {',
          'if shaped := displayNameFromTitle(title); title != "" {')],
    ),
    Case(
        "a stated title loses to the question",
        [("\t\treturn shaped, false", "\t\treturn displayNameFromQuestion(question), false")],
    ),
    Case(
        "a good title is reported as holding no readable text",
        [("\t\treturn shaped, false", "\t\treturn shaped, true")],
    ),
    Case(
        "a title that shaped away to nothing is replaced without saying so",
        [('return displayNameFromQuestion(question), title != ""',
          "return displayNameFromQuestion(question), false")],
    ),
    Case(
        "no title at all is reported as a rejected title",
        [('return displayNameFromQuestion(question), title != ""',
          "return displayNameFromQuestion(question), true")],
    ),

    # ---- the shaping every name gets ----
    Case(
        # The guard main used before non-text removal existed. unicode.IsSpace is
        # false for every rune spaceOutNonText removes, so "\x00\x00" passes it.
        "the blank guard goes back to TrimSpace and disagrees with the shaping",
        [('return shapeIntoOneLineOfText(s) != ""', 'return strings.TrimSpace(s) != ""')],
    ),
    Case(
        "the collapse runs before the removal, so a removed character widens a gap",
        [("return collapseWhitespaceRuns(spaceOutNonText(s))",
          "return spaceOutNonText(collapseWhitespaceRuns(s))")],
    ),
    Case(
        "the shaping forgets to remove non-text characters",
        [("return collapseWhitespaceRuns(spaceOutNonText(s))", "return collapseWhitespaceRuns(s)")],
    ),
    Case(
        "a removed character is dropped, welding the words either side into one",
        [("\t\tif isNonText(r) {\n\t\t\treturn ' '", "\t\tif isNonText(r) {\n\t\t\treturn -1")],
    ),
    Case(
        "the bidi controls stop counting as non-text, so an override reorders the row",
        [("return (unicode.IsControl(r) && !unicode.IsSpace(r)) || unicode.Is(unicode.Bidi_Control, r)",
          "return unicode.IsControl(r) && !unicode.IsSpace(r)")],
    ),
    Case(
        "the rule takes all of category Cf, so the join controls are torn out of text",
        [("return (unicode.IsControl(r) && !unicode.IsSpace(r)) || unicode.Is(unicode.Bidi_Control, r)",
          "return (unicode.IsControl(r) && !unicode.IsSpace(r)) || unicode.Is(unicode.Cf, r)")],
    ),
    Case(
        "the whitespace controls are counted as non-text too",
        [("return (unicode.IsControl(r) && !unicode.IsSpace(r)) || unicode.Is(unicode.Bidi_Control, r)",
          "return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)")],
    ),
    Case(
        # What the code did before it split on unicode.IsSpace.
        "only \\n is put on one line",
        [('return strings.Join(strings.Fields(s), " ")',
          'return strings.TrimSpace(strings.ReplaceAll(s, "\\n", " "))')],
    ),
    Case(
        "each whitespace character becomes a space, but a run is not collapsed",
        [('return strings.Join(strings.Fields(s), " ")',
          "return strings.TrimSpace(strings.Map(func(r rune) rune {\n"
          "\t\tif unicode.IsSpace(r) {\n\t\t\treturn ' '\n\t\t}\n\t\treturn r\n\t}, s))")],
    ),
    Case(
        "the words are joined with no separator",
        [('return strings.Join(strings.Fields(s), " ")', 'return strings.Join(strings.Fields(s), "")')],
    ),
    Case(
        "leading whitespace survives as one space",
        [('return strings.Join(strings.Fields(s), " ")',
          'if s != "" && unicode.IsSpace([]rune(s)[0]) {\n'
          '\t\treturn " " + strings.Join(strings.Fields(s), " ")\n\t}\n'
          '\treturn strings.Join(strings.Fields(s), " ")')],
    ),
    Case(
        "a repeated word is dropped",
        [('return strings.Join(strings.Fields(s), " ")',
          "seen := map[string]bool{}\n\tvar words []string\n"
          "\tfor _, w := range strings.Fields(s) {\n"
          "\t\tif !seen[w] {\n\t\t\tseen[w] = true\n\t\t\twords = append(words, w)\n\t\t}\n\t}\n"
          '\treturn strings.Join(words, " ")')],
    ),

    # ---- the --from guard ----
    Case(
        "the no-readable-text refusal is skipped, so an empty --from is accepted",
        [("\tif !hasReadableText(from) {\n\t\treturn fmt.Errorf(\"--from %q holds",
          "\tif false {\n\t\treturn fmt.Errorf(\"--from %q holds")],
    ),
    Case(
        "a --from that is not one line of text is accepted",
        [("if shaped := shapeIntoOneLineOfText(from); shaped != from {",
          "if shaped := shapeIntoOneLineOfText(from); false {")],
    ),
    Case(
        "a --from padded with spaces is accepted, because the guard compares after trimming",
        [("if shaped := shapeIntoOneLineOfText(from); shaped != from {",
          "if shaped := shapeIntoOneLineOfText(from); shaped != strings.TrimSpace(from) {")],
    ),
    Case(
        "every --from is refused, including the names real callers pass",
        [("from, shaped)\n\t}\n\treturn nil", 'from, shaped)\n\t}\n\treturn fmt.Errorf("--from %q refused", from)')],
    ),
    Case(
        "the rewrite refusal stops naming the --from flag",
        [('return fmt.Errorf("--from %q is not one line of text', 'return fmt.Errorf("agent name %q is not one line of text')],
    ),
    Case(
        "the rewrite refusal names the refused value where the rewrite belongs",
        [("from, shaped)\n\t}\n\treturn nil", "from, from)\n\t}\n\treturn nil")],
    ),
    Case(
        "the rewrite refusal writes the refused value raw to the terminal",
        [('return fmt.Errorf("--from %q is not one line of text', 'return fmt.Errorf("--from %s is not one line of text')],
    ),
    Case(
        "the no-readable-text refusal writes the refused value raw to the terminal",
        [('return fmt.Errorf("--from %q holds no readable text', 'return fmt.Errorf("--from %s holds no readable text')],
    ),
]


# A test in this column asserts on something no edit to main.go can change.
UNREDDENED = {
    "TestWhitespaceSweep_CoversTheWholeClass":
        "checks the test's own sweep of Go's unicode tables and calls nothing in main.go; "
        "it guards the fixture every whitespace test uses, so only a change to Go can redden it",
}


if __name__ == "__main__":
    sys.exit(score(TARGET, PACKAGES, CASES, UNREDDENED))
