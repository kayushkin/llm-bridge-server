#!/usr/bin/env python3
"""Sabotage cases for the rune-safe display-name work.

The defect is one mechanism spread over five call sites and a shared helper.
The engine in sabotage.py scores one file at a time, so this drives it once per
target and adds the scores up.

Scoring the helper alone would not be enough. Its own tests can be perfect
while a call site still byte-cuts, and that is exactly the state this repo was
in: sessions.go:106 and renamer.go:68 were already rune-safe while three
neighbours cut bytes. So every call site gets a case that puts the plain byte
cut back, and it has to go red.

Two call sites are KNOWN GAPs and say so. Both discovery loops need real
harness discovery — they run harness binaries and read sessions off disk — so
no unit test reaches them. They are listed rather than omitted: a file that
dropped them would score higher and measure less.

Run with --diffs at least once and read each applied edit against its label. A
row prints the name it was given, not the edit it made.

⚠️ The internal/server cases run under `-run DisplayName|AutoRename` rather
than the whole package. The full server suite takes 128s, so eleven cases
against it is half an hour, and the narrow set is the honest scope anyway:
CAUGHT then means "caught by the tests that claim this mechanism" rather than
"caught by something, somewhere". It is the stronger reading, not the weaker
one.

It does make one claim cheaper than it should be. A KNOWN GAP asserts that
NOTHING catches the mutation, and under a filter that only proves the filtered
tests miss it. Both gap cases were therefore also run against the unfiltered
package once, and both were UNNOTICED there too.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import Case, REPO, counts_as_coverage, problems, score  # noqa: E402

# See the note in the docstring: scoped to the tests that claim these
# mechanisms, because the unfiltered package takes 128s per case.
SERVER_PKG = ["-run", "DisplayName|AutoRename", "./internal/server/"]

# The fixture guards in the suites above, by message. A guard fires when a
# mutation stops the test input reaching the code under test; `go test` exits
# non-zero for that exactly as it does for a real assertion, so without these
# the engine counts the test falling over as coverage. See
# sabotage.classify_caught().
#
# All four sites in scope share one sentence — internal/textutil/runelimit_test.go:60,
# internal/server/displayname_runes_test.go:57 and :79, and
# cmd/bridge-agent/displayname_test.go:46 — so one marker covers them. The repo's
# other guards (manager_derivation_test.go, interrupt_resume_live_test.go,
# tool_provision_test.go) are in packages no target here runs.
GUARD_MARKERS = (
    "the test no longer reaches the defect",
)

HELPER = REPO / "internal" / "textutil" / "runelimit.go"
SESSIONS = REPO / "internal" / "server" / "sessions.go"
RENAMER = REPO / "internal" / "server" / "renamer.go"
SERVER = REPO / "internal" / "server" / "server.go"
AGENT = REPO / "cmd" / "bridge-agent" / "main.go"

RUNE_WALK = """	runeCount := 0
	for byteOffset := range text {
		runeCount++
		if runeCount > maxRunes {
			return text[:byteOffset]
		}
	}
	return text"""

BYTE_CUT = """	if len(text) > maxRunes {
		return text[:maxRunes]
	}
	return text"""

TEXTUTIL_IMPORT = '\t"github.com/kayushkin/llm-bridge-server/internal/textutil"\n'

TARGETS = [
    (HELPER, ["./internal/textutil/"], [
        # The defect itself, put back.
        Case("helper byte-cuts instead of counting runes", [(RUNE_WALK, BYTE_CUT)]),

        # The budget. Drifted comparisons, so nothing is orphaned and the case
        # scores instead of reporting a compile error.
        Case("cut lands one rune late", [("if runeCount > maxRunes {", "if runeCount > maxRunes+1 {")]),
        Case("cut lands one rune early", [("if runeCount > maxRunes {", "if runeCount >= maxRunes {")]),

        # "Valid UTF-8 within budget" is satisfied by giving up entirely. This
        # is the case the exact-value assertions exist for.
        #
        # Written as `byteOffset*0` rather than `""`. The plain empty string is
        # a deletion of byteOffset's only use, so it orphans the variable and
        # the case reports `compile error` instead of a score -- which hides
        # whether any test would have caught the behaviour. Multiplying by zero
        # is the same value with every identifier still live.
        Case("helper trims to nothing", [("return text[:byteOffset]", "return text[:byteOffset*0]")]),

        # Replacing the whole walk removes byteOffset and runeCount together,
        # so nothing is orphaned. Unused parameters are legal in Go.
        Case("CONTROL known-positive: the helper returns a fixed string",
             [(RUNE_WALK, '\treturn "SABOTAGE"')]),
        Case("CONTROL known-negative: <= 0 rewritten as < 1, identical for ints",
             [("if maxRunes <= 0 {", "if maxRunes < 1 {")],
             expected_unnoticed="a behavioural no-op; it must NOT be caught"),
    ]),

    (SESSIONS, SERVER_PKG, [
        Case("message title byte-cuts again", [(
            '''	if truncated := textutil.TruncateToRuneLimit(text, maxRunes); truncated != text {
		return truncated + "…"
	}''',
            '''	if len(text) > maxRunes {
		return text[:maxRunes] + "…"
	}''')]),
        Case("message title loses its ellipsis",
             [('return truncated + "…"', "return truncated")]),
        Case("discovered title byte-cuts again", [(
            "	return textutil.TruncateToRuneLimit(displayName, maxDiscoveredDisplayNameRunes)",
            """	if len(displayName) > maxDiscoveredDisplayNameRunes {
		displayName = displayName[:maxDiscoveredDisplayNameRunes]
	}
	return displayName""")]),
        Case("discovered title prefers the project over the prompt", [(
            """	displayName := prompt
	if displayName == "" {
		displayName = project
	}""",
            """	displayName := prompt
	if displayName != "" {
		displayName = project
	}""")]),

        # KNOWN GAP. handleDiscoverSessions runs harness binaries to find
        # sessions on disk; nothing in the unit suite reaches the loop.
        Case("handleDiscoverSessions' call site byte-cuts again", [(
            "	for _, ds := range sessions {\n		displayName := displayNameForDiscoveredSession(ds.Prompt, ds.Project)",
            """	for _, ds := range sessions {
		displayName := ds.Prompt
		if displayName == "" {
			displayName = ds.Project
		}
		if len(displayName) > 100 {
			displayName = displayName[:100]
		}""")],
             expected_unnoticed="KNOWN GAP: the loop needs real harness discovery to run"),
    ]),

    (RENAMER, SERVER_PKG, [
        # Drifted limit: keeps every identifier live, so no second edit.
        Case("auto-rename's cap stops applying", [(
            "name = textutil.TruncateToRuneLimit(name, maxAutoRenameRunes)",
            "name = textutil.TruncateToRuneLimit(name, maxAutoRenameRunes*10)")]),
        # A genuine deletion: this is the file's only use of textutil, so the
        # import goes with it or the case reports a compile error instead of a
        # score.
        Case("auto-rename byte-cuts the title again", [
            ("	name = textutil.TruncateToRuneLimit(name, maxAutoRenameRunes)",
             """	if len(name) > maxAutoRenameRunes {
		name = name[:maxAutoRenameRunes]
	}"""),
            (TEXTUTIL_IMPORT, ""),
        ]),
    ]),

    (SERVER, SERVER_PKG, [
        # KNOWN GAP, same reason as handleDiscoverSessions.
        Case("AutoDiscover's call site byte-cuts again", [(
            "		for _, ds := range sessions {\n			displayName := displayNameForDiscoveredSession(ds.Prompt, ds.Project)",
            """		for _, ds := range sessions {
			displayName := ds.Prompt
			if displayName == "" {
				displayName = ds.Project
			}
			if len(displayName) > 100 {
				displayName = displayName[:100]
			}""")],
             expected_unnoticed="KNOWN GAP: AutoDiscover needs real harness discovery to run"),
    ]),

    (AGENT, ["./cmd/bridge-agent/"], [
        Case("delegate name appends its ellipsis on the wrong branch",
             [("truncated != name", "truncated == name")]),
        Case("delegate name byte-cuts again", [
            ('''	if truncated := textutil.TruncateToRuneLimit(name, maxRunes); truncated != name {
		name = truncated + "…"
	}''',
             '''	if len(name) > maxRunes {
		name = name[:maxRunes] + "…"
	}'''),
            (TEXTUTIL_IMPORT, ""),
        ]),
    ]),
]


def main():
    caught = real = gaps = 0
    found = []
    for target, packages, cases in TARGETS:
        print("\n########## %s ##########" % target.relative_to(REPO))
        results = score(target, packages, cases, GUARD_MARKERS)
        found += problems(results)
        for case, verdict, _, _ in results:
            if case.name.startswith("CONTROL"):
                continue
            if case.expected_unnoticed:
                gaps += 1
                continue
            real += 1
            # counts_as_coverage, not `verdict == "CAUGHT"`: a row that went red
            # because a fixture guard fired is not a mechanism this suite pins,
            # and adding it in here is the inflation the split exists to stop.
            if counts_as_coverage(verdict):
                caught += 1

    print("\n================ total ================")
    print("%d/%d real mechanisms caught across %d files" % (caught, real, len(TARGETS)))
    print("%d KNOWN GAP cases, reported rather than omitted" % gaps)
    if found:
        print("%d problem(s) across the tables above" % len(found))

    # The status has to carry the finding. This printed its totals and returned,
    # so the process exited 0 with every mechanism unpinned and both controls
    # misbehaving — and anything running it from a guard read that as success.
    #
    # The engine is asked what went wrong rather than inferring it from the
    # counters above: `caught < real` would miss a known-negative control that
    # WAS caught, which means the suite is red for a reason unrelated to
    # behaviour and every CAUGHT in the table is suspect. That run scores a
    # perfect caught == real and is worth nothing.
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
