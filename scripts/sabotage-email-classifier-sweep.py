#!/usr/bin/env python3
"""Score cmd/email-classifier's re-score sweep by breaking it on purpose.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-email-classifier-sweep.py [--diffs] [--crosstable]

Why the sweep gets a scorer of its own rather than rows in
sabotage-email-classifier.py: that plan declares its seam as CONFIGURATION —
the flag set, the environment wiring, the registration payload and the one
guard — and says in as many words that the tick's own logic is not sabotaged
there. sweep.go is tick logic. Folding these rows in would quietly widen a
declared scope, and the declaration is the thing that makes that plan's silence
readable.

Why the sweep was worth scoring at all, and this is a measurement rather than a
judgement: on 2026-08-15 an unconditional `panic()` at rescoreBoard's first line
left `go test ./...` GREEN across the whole repository. Nothing executed it. It
runs every fifteen minutes and PATCHes the board.

The mechanism this file exists for is -dry-run, and it has two halves that fail
independently:

  - a dry run that WRITES is a switch that does not switch;
  - a dry run that writes nothing and COUNTS nothing prints "0 repriced, 0
    retagged", which is exactly what a board with nothing to do prints. That is
    the answer somebody runs -dry-run to disprove.

Both are cases below, because a suite that only asserted "no HTTP happened"
would catch the first and sail past the second.

Case-writing rules inherited from the email-classifier, autoworker and
reminder-coordinator plans:

  - Prefer a DRIFTED VALUE to a deletion. It orphans no variable, needs no
    second edit, and is the likelier real-world regression.
  - Read every applied diff against its label (`--diffs`). A row prints the
    name you gave it, not the edit you made.
  - An UNNOTICED row is not automatically a coverage hole. It can be a
    behavioural no-op, or a fake in the test file that swallows the value the
    mutation moved.

Score when filed by the 216th nightly pass: 19/19 real mechanisms caught, both
controls behaved, exit 0.

One row was UNNOTICED on the first run and is worth recording rather than
smoothing over: `card.Tags = newTags` could be neutered without reddening
anything. The suite asserted the urgency FIELD was carried back onto the card
and not the tag LIST, and the two are written separately — so a card whose two
halves disagreed would have reached the merge pass and the digest, both of which
read the tags. The test was widened; the case was not.

⚠️ `--crosstable` has NOT been taken for this plan, and no `unreddened`
declaration is made below, because an empty one would read as "measured and
found none" rather than "not measured". The next pass on this file owns that.
Most columns are expected to be silent: five of the six test files in this
package cover code no case here touches.

⚠️ Region this plan does NOT cover, declared rather than left to be
rediscovered: scoreCard and everything under it (stakesOf, bucketForDue,
priorityMatrix, mootWhenPassed) are NOT sabotaged here — priority_test.go pins
that arithmetic directly and sabotage-email-classifier.py owns the flags. Nor
is sessionGate's resolve path, which session_test.go covers. This plan is
rescoreBoard's own control flow: what it skips, what it writes, what it counts,
and what it does when a write fails.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

TARGETS = [REPO / "cmd/email-classifier/sweep.go"]
PACKAGES = ["./cmd/email-classifier/"]


CASES = [
    # ---- controls ----
    Case(
        "CONTROL known-positive: the sweep reports an empty summary whatever it did",
        [("\t\t\tresult.Repriced++\n\t\t}\n\t}\n\n\treturn result\n}",
          "\t\t\tresult.Repriced++\n\t\t}\n\t}\n\n\treturn sweepResult{}\n}")],
    ),
    Case(
        "CONTROL known-negative: reword the dry-run log line a human reads",
        [('log.Printf("[email] DRY-RUN sweep %q: %s", card.Title, change)',
          'log.Printf("[email] dry run would change %q: %s", card.Title, change)')],
        expected_unnoticed="log prose for whoever is watching a -dry-run tick. No test reads the "
                           "log and nothing downstream parses it. This control is what proves the "
                           "rows below redden on the BEHAVIOUR and not on the source lines carrying "
                           "it. Falsified by a test that captures log output.",
    ),

    # ---- -dry-run: the switch whose whole job is to not write ----
    Case(
        "the dry-run gate inverts: a dry run writes the board and a real tick writes nothing",
        [("\t\tif dryRun {\n\t\t\t// The urgency clause", "\t\tif !dryRun {\n\t\t\t// The urgency clause")],
    ),
    Case(
        "a dry run reports nothing: the summary is what a board with no work to do prints",
        [("\t\t\tif reprice {\n\t\t\t\tresult.Repriced++\n\t\t\t}\n\t\t\tif retag {\n\t\t\t\tresult.Retagged++\n\t\t\t}\n\t\t\tcontinue",
          "\t\t\tcontinue")],
    ),
    Case(
        "a dry run counts a reprice as a retag: the two axes trade places in the summary only",
        [("\t\t\tif reprice {\n\t\t\t\tresult.Repriced++\n\t\t\t}",
          "\t\t\tif reprice {\n\t\t\t\tresult.Retagged++\n\t\t\t}")],
    ),

    # ---- the two deliberate exclusions ----
    Case(
        "the completed-column skip reads the wrong column: settled cards are rescored and No action is frozen",
        [("if card.ColumnName == columnActionCompleted {", "if card.ColumnName == columnNoAction {")],
    ),
    Case(
        "the completed-column skip is dead: a deadline passing re-scores what a human already settled",
        [("if card.ColumnName == columnActionCompleted {", "if false {")],
    ),
    Case(
        "the agent gate inverts: the sweep retags exactly the cards a live session is editing",
        [("}\n\t\tif owned, session := gate.agentOwns(card); owned {",
          "}\n\t\tif owned, session := gate.agentOwns(card); !owned {")],
    ),
    Case(
        "the agent gate is dead: a card an agent holds is retagged underneath it",
        [("}\n\t\tif owned, session := gate.agentOwns(card); owned {",
          "}\n\t\tif _, session := gate.agentOwns(card); false {")],
    ),

    # ---- what counts as a change worth writing ----
    Case(
        "a card must move on BOTH axes to be written: every priority-only card stops being scored",
        [("if !retag && !reprice {", "if !retag || !reprice {")],
    ),
    Case(
        "the no-deadline guard goes: a card with no due date has the model's urgency tag blanked",
        [('retag := score.Urgency != "" && score.Urgency != card.Urgency',
          "retag := score.Urgency != card.Urgency")],
    ),
    Case(
        "only upward moves are written: a lapsed concert keeps the score it had when it mattered",
        [("reprice := score.Priority != card.Priority", "reprice := score.Priority > card.Priority")],
    ),

    # ---- what is actually sent to kanban-store ----
    Case(
        "the retag PATCH names a field kanban-store does not have: the write 200s and changes nothing",
        [('kc.PatchCard(card.ID, map[string]any{"tags": newTags})',
          'kc.PatchCard(card.ID, map[string]any{"tag": newTags})')],
    ),
    Case(
        "the retag writes the tags it read: every sweep PATCHes the board back to what it already said",
        [("newTags := replaceTag(card.Tags, tagPrefixUrgent, score.Urgency)",
          "newTags := replaceTag(card.Tags, tagPrefixUrgent, card.Urgency)")],
    ),
    Case(
        "the reprice writes the old score: the board is PATCHed every tick and never moves",
        [('kc.PatchCard(card.ID, map[string]any{"priority": score.Priority})',
          'kc.PatchCard(card.ID, map[string]any{"priority": card.Priority})')],
    ),

    # ---- the in-memory board the rest of the tick reads ----
    Case(
        "the written score is not carried back onto the card: later steps score against a stale board",
        [("\t\t\tcard.Priority = score.Priority", "\t\t\tcard.Priority = card.Priority")],
    ),
    Case(
        "the urgency is carried back but the tags are not: the two halves of one card disagree",
        [("\t\t\tcard.Tags = newTags", "\t\t\tcard.Tags = card.Tags")],
    ),

    # ---- the failure path: one bad write must not abandon the board ----
    Case(
        "a failed retag falls through to the reprice: the card is half-written and reported whole",
        [('log.Printf("[email] sweep: retag %q failed: %v", card.Title, err)\n\t\t\t\tcontinue',
          'log.Printf("[email] sweep: retag %q failed: %v", card.Title, err)')],
    ),
    Case(
        "a failed write abandons the whole board: the next tick finds the same failure and nothing recovers",
        [('log.Printf("[email] sweep: retag %q failed: %v", card.Title, err)\n\t\t\t\tcontinue',
          'log.Printf("[email] sweep: retag %q failed: %v", card.Title, err)\n\t\t\t\tbreak')],
    ),
    Case(
        "a failed reprice is counted as a success: the summary reports a write that did not land",
        [('log.Printf("[email] sweep: reprice %q failed: %v", card.Title, err)\n\t\t\t\tcontinue',
          'log.Printf("[email] sweep: reprice %q failed: %v", card.Title, err)')],
    ),

    # ---- the one number that says the sweep saw the whole board ----
    Case(
        "Scanned counts only the cards that produced work: an excluded board prints as an empty one",
        [("\t\tresult.Scanned++\n\n\t\tif card.ColumnName == columnActionCompleted {",
          "\t\tif card.ColumnName == columnActionCompleted {")],
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
