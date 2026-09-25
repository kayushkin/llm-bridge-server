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

Scope is runAsk, publishInbound, resolveInstance and buildPrompt. The display
name and the --from guard have their own tests and no cases here yet.

Measured 2026-09-25 on main: plain score 43/43, both controls behaved, exit 0.
--crosstable exits 1, deliberately: 40 of 70 tests are reddened by no case,
and they are exactly the tests in display_name_test.go and agent_name_test.go.
Every test in relay_test.go (and TestBuildPrompt_Attribution…) is reddened by
at least one case. The 40 are NOT declared `unreddened`: that argument is for a
guard no single mutation can isolate, and these are simply functions this list
has no cases for yet. Declaring them would turn a gap into a green run. Adding
those cases is its own card.
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

]


if __name__ == "__main__":
    sys.exit(score(TARGET, PACKAGES, CASES))
