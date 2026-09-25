#!/usr/bin/env python3
"""Score cmd/kanban-curator's tests by breaking the binary on purpose.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-kanban-curator.py [--diffs|--crosstable]

scripts/score-curator-degradations.py is the other scorer for this binary. It
arms the six `degraded.Record` sites and the exit code; this file arms what the
curator DECIDES — which column a card goes to, which cards it leaves alone, the
wire values it shares with kanban-store and llm-bridge-server — and -register.

Ported 2026-09-25 from test/first-tests-for-curator-and-dispatcher (6767efd1,
c2e7390b, 8464fff3, 86b6dc8f, a3b1e8b1, e4e46f95) onto main, whose curator had
since been rewritten: boards are pinned by id, the autoworker gates read the
session's `purpose` instead of an id prefix, and a board that cannot be read
is fatal instead of an error returned from curateBoard. Dropped, because main
has no such code: the board-name drift, envOr, the board LIST read, "a board
that does not exist yet", and the -board flag. e4e46f95's exit-status fix is in
the shared engine already.

Measured 2026-09-25 on main: before the ported tests 26/50; with them 50/50,
both controls behaved, exit 0. --crosstable: 8 of 43 tests are reddened by no
case here, and they are the four shortfall tests score-curator-degradations.py
arms (links unreadable, no noteboard item, empty board, running session) and
the four in session_url_test.go, which pin bridgesession.URL's call site. They
are NOT declared `unreddened`: those are tests another scorer or no scorer
covers yet, not guards no single mutation can isolate.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

TARGET = REPO / "cmd/kanban-curator/main.go"
PACKAGES = ["./cmd/kanban-curator/"]


CASES = [
    # ---- controls ----
    Case(
        "CONTROL known-positive: every tick is a dry run",
        [("if cfg.dryRun {\n\t\t\tcontinue\n\t\t}", "if true || cfg.dryRun {\n\t\t\tcontinue\n\t\t}")],
    ),
    Case(
        "CONTROL known-negative: reword a log line",
        [('log.Printf("[%s] move card: %v", title, err)',
          'log.Printf("[%s] moving the card failed: %v", title, err)')],
        expected_unnoticed="a log line nothing asserts on; the harness must not call this a catch",
    ),

    # ---- wire values another program wrote ----
    Case(
        "the In Progress column name drifts",
        [('columnInProgress   = "In Progress"', 'columnInProgress   = "In progress"')],
    ),
    Case(
        "the Done column name drifts",
        [('columnDone         = "Done"', 'columnDone         = "done"')],
    ),
    Case(
        "the Failed column name drifts",
        [('columnFailed       = "Failed"', 'columnFailed       = "Error"')],
    ),
    Case(
        "the card-link entity type drifts",
        [('linkTypeSession    = "session"', 'linkTypeSession    = "bus_session"')],
    ),
    Case(
        "the entity type the review tag hangs on drifts",
        [('entityTypeSession = "session"', 'entityTypeSession = "sessions"')],
    ),
    Case(
        "the review-settled tag drifts from what the reviewer writes",
        [('tagReviewSettled  = "review-settled"', 'tagReviewSettled  = "review_settled"')],
    ),
    Case(
        "the purpose llm-bridge-server stamps on an autoworker session drifts",
        [('purposeAutoworker = "autoworker"', 'purposeAutoworker = "auto-worker"')],
    ),
    Case(
        "the session-state literal for a finished session drifts",
        [('case state == "completed":', 'case state == "complete":')],
    ),
    Case(
        "an aborted session is no longer read as failed",
        [('case state == "error", state == "aborted", state == "disconnected":',
          'case state == "error", state == "abort", state == "disconnected":')],
    ),
    Case(
        "a disconnected session is no longer read as failed",
        [('case state == "error", state == "aborted", state == "disconnected":',
          'case state == "error", state == "aborted", state == "disconnect":')],
    ),
    Case(
        "an autoworker session parked on a permission prompt is never reaped",
        [('case state == "idle", state == "awaiting_permission", state == "awaiting_user":',
          'case state == "idle", state == "awaiting-permission", state == "awaiting_user":')],
    ),
    Case(
        "an autoworker session parked on a question to the user is never reaped",
        [('case state == "idle", state == "awaiting_permission", state == "awaiting_user":',
          'case state == "idle", state == "awaiting_permission", state == "awaiting-user":')],
    ),

    # ---- decisions ----
    Case(
        "Done and Failed are swapped",
        [("\t\t\ttarget, targetName = doneCol, columnDone\n\t\tcase state == \"error\"",
          "\t\t\ttarget, targetName = failedCol, columnFailed\n\t\tcase state == \"error\""),
         # Second edit: without it doneCol is orphaned and the case reports a
         # compile error instead of a score.
         ("var inProgress []kanbanclient.CardView",
          "_ = doneCol\n\tvar inProgress []kanbanclient.CardView")],
    ),
    Case(
        "a failed session closes to Done instead of Failed",
        [('case state == "error", state == "aborted", state == "disconnected":\n\t\t\ttarget, targetName = failedCol, columnFailed',
          'case state == "error", state == "aborted", state == "disconnected":\n\t\t\ttarget, targetName = doneCol, columnDone')],
    ),
    Case(
        "the reviewer handshake is dropped: autoworker cards close immediately",
        [("if createdByAutoworker(sess) && !staleCompleted(ended) {", "if false {")],
    ),
    Case(
        "the reviewer handshake applies to every session, not only autoworker's",
        [("if createdByAutoworker(sess) && !staleCompleted(ended) {", "if !staleCompleted(ended) {")],
    ),
    Case(
        "the review grace window never expires, so a down reviewer wedges the card",
        [("return time.Since(t) > reviewGrace", "_ = t\n\treturn false")],
    ),
    Case(
        "reviewSettled answers true when the tag store is down",
        [("\t\treturn false, err\n", "\t\treturn true, err\n")],
    ),
    Case(
        "any tag on the session counts as the reviewer's sign-off",
        [("if t.Tag == tagReviewSettled {", "if t.Tag != \"\" {")],
    ),
    Case(
        "the origin gate on reaping is dropped: idle sessions are reaped on any board",
        [("if !createdByAutoworker(sess) {\n\t\t\t\tskipped++\n\t\t\t\tcontinue\n\t\t\t}", "")],
    ),
    Case(
        "the stale window is dropped: a waiter is reaped the moment it pauses",
        [("if !staleSince(ended, cfg.staleAfter) {\n\t\t\t\tcontinue\n\t\t\t}", "")],
    ),
    Case(
        "a running session is closed too",
        [("default:\n\t\t\tcontinue // running / unknown — leave alone",
          "default:\n\t\t\ttarget, targetName = doneCol, columnDone")],
    ),
    Case(
        "a vanished session is skipped instead of reaped, wedging the card",
        [('\t\t\ttarget, targetName = failedCol, columnFailed\n\t\t\treapNote = "reaped: bridge session not found (archived/pruned)"',
          "\t\t\tcontinue")],
    ),
    Case(
        "a card with no session link is looked up anyway",
        [('if sessionID == "" {', 'if false && sessionID == "" {')],
    ),
    Case(
        "-dry-run writes anyway",
        [("if cfg.dryRun {\n\t\t\tcontinue\n\t\t}", "")],
    ),
    Case(
        "the closing stamp overwrites the card body instead of appending",
        [("if err := kc.PatchCardBody(c.Placement.CardID, body+suffix); err != nil {",
          "_ = body\n\t\t\t\tif err := kc.PatchCardBody(c.Placement.CardID, suffix); err != nil {")],
    ),
    Case(
        "a reaped card's note does not say why it was reaped",
        [("time.Now().UTC().Format(time.RFC3339), reapNote)",
          'time.Now().UTC().Format(time.RFC3339), "")')],
    ),
    Case(
        "a closed card's note does not say what state the session ended in",
        [("time.Now().UTC().Format(time.RFC3339), state, ended)",
          'time.Now().UTC().Format(time.RFC3339), "", ended)')],
    ),
    Case(
        "a card is moved to the column it is already in",
        [("moveResult, err := kc.MoveCard(c.Placement.CardID, board.ID, target, 0)",
          "_ = target\n\t\tmoveResult, err := kc.MoveCard(c.Placement.CardID, board.ID, colByName[columnInProgress], 0)")],
    ),

    # ---- the reads the tick makes ----
    Case(
        "a bridge that answers 500 is read as a session that no longer exists",
        [('return bridgeSession{}, false, fmt.Errorf("HTTP %s — %s", resp.Status, strings.TrimSpace(string(body)))',
          'return bridgeSession{}, true, fmt.Errorf("HTTP %s — %s", resp.Status, strings.TrimSpace(string(body)))')],
    ),
    Case(
        "a bridge 404 is read as an error instead of a vanished session",
        [('return bridgeSession{}, true, fmt.Errorf("not found")',
          'return bridgeSession{}, false, fmt.Errorf("not found")')],
    ),
    Case(
        "a session with no purpose is accepted as somebody else's",
        [('if s.Purpose == "" {', 'if false && s.Purpose == "" {')],
    ),
    Case(
        "createdByAutoworker goes back to matching the session id",
        [("return s.Purpose == purposeAutoworker",
          'return strings.HasPrefix(s.SessionID, "autoworker-")')],
    ),
    Case(
        "a waiter with no timestamp at all is treated as stale, and reaped",
        [('func staleSince(updatedAt string, d time.Duration) bool {\n\tif updatedAt == "" {\n\t\treturn false',
          'func staleSince(updatedAt string, d time.Duration) bool {\n\tif updatedAt == "" {\n\t\treturn true')],
    ),
    Case(
        "a waiter whose timestamp does not parse is treated as stale, and reaped",
        [("return false\n\t}\n\treturn time.Since(t) > d",
          "return true\n\t}\n\treturn time.Since(t) > d")],
    ),
    Case(
        "staleSince has its comparison backwards: fresh waiters are reaped, old ones kept",
        [("return time.Since(t) > d", "return time.Since(t) < d")],
    ),
    Case(
        "a completed session with no timestamp skips the reviewer",
        [('func staleCompleted(updatedAt string) bool {\n\tif updatedAt == "" {\n\t\treturn false',
          'func staleCompleted(updatedAt string) bool {\n\tif updatedAt == "" {\n\t\treturn true')],
    ),
    Case(
        "a completed session whose timestamp does not parse skips the reviewer",
        [("return false\n\t}\n\treturn time.Since(t) > reviewGrace",
          "return true\n\t}\n\treturn time.Since(t) > reviewGrace")],
    ),
    Case(
        "the review grace window is some other length",
        [("reviewGrace = 30 * time.Minute", "reviewGrace = 3 * time.Hour")],
    ),
    Case(
        "a board that no longer exists is not refused",
        [("return nil, fmt.Errorf(\"board %s does not exist — refusing to create a replacement, since a second board is exactly the fork pinning the id prevents\", boardID)",
          "return &kanbanclient.Board{ID: boardID}, nil")],
    ),

    # ---- -register ----
    Case(
        "the Agent runs board's job loses its original name",
        [('return "kanban-curator-tick"', 'return "kanban-curator-agent-runs-tick"')],
    ),
    Case(
        "-job-name is read and then ignored, so a second curator registers under the first's name",
        [('if jobName == "" {', "if true {")],
    ),
    Case(
        "the registered command loses -board-id, so the job cannot run at all",
        [('command := bin + " -board-id " + board.ID', "command := bin")],
    ),
    Case(
        "-register is no longer idempotent: a re-run registers a second job beside the first",
        [("if j.Name == jobName {", "if j.Name == jobName && false {")],
    ),
    Case(
        "the registered cron job runs on a different cadence than the curator is designed for",
        [('"schedule":    "*/5 * * * *",', '"schedule":    "*/5 * * * 1",')],
    ),
    Case(
        "the registered job is an agent job, not a shell job",
        [('"type":        "shell",', '"type":        "agent",')],
    ),
    Case(
        "a scheduler that refuses the job is reported as a successful registration",
        [('return fmt.Errorf("scheduler returned %d: %s", resp.StatusCode, string(body))',
          "return nil")],
    ),
    Case(
        "a job list the scheduler could not serve is read as no jobs, so a re-run registers a second curator",
        [("if resp.StatusCode != http.StatusOK {\n\t\treturn fmt.Errorf(\"list jobs: scheduler returned",
          "if false {\n\t\treturn fmt.Errorf(\"list jobs: scheduler returned")],
    ),
    Case(
        "a job list that does not decode is read as no jobs, so a re-run registers a second curator",
        [("if err := json.Unmarshal(body, &jobs); err != nil {\n\t\treturn fmt.Errorf(\"list jobs: decode job list: %w\", err)",
          "if err := json.Unmarshal(body, &jobs); err != nil && false {\n\t\treturn fmt.Errorf(\"list jobs: decode job list: %w\", err)")],
    ),
    Case(
        "the job-name slug drops digits, so two boards can collapse onto one job",
        [("case r >= 'a' && r <= 'z', r >= '0' && r <= '9':", "case r >= 'a' && r <= 'z':")],
    ),
    Case(
        "the job-name slug keeps its leading and trailing dashes",
        [('return strings.Trim(b.String(), "-")', "return b.String()")],
    ),
    Case(
        "upper-case letters in a board name are dropped from the job name",
        [("for _, r := range strings.ToLower(s) {", "for _, r := range s {")],
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGET, PACKAGES, CASES))
