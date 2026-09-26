#!/usr/bin/env python3
"""Score cmd/kanban-dispatcher's degradation tests by breaking the code they pin.

Ported from 20d9d87's scorer and re-aimed at main's dispatcher, which rebuilt
the tick around runOptions and backlogDispatcher after that branch forked.

A green suite is a claim about a suite, not about the code. This plants one
defect at a time and asks whether anything goes red.

Three outcomes, not two. A mutation that does not COMPILE exits non-zero exactly
as a red suite does, so every arm is judged on the output as well: `build
failed`, `undefined:` and friends are ABORTED, and an ABORTED arm is a hole in
the scorer, not a win. Silencing a Record turns it into `_ = fmt.Sprintf(` and
dropping a name uses `%.0s`, so both keep every argument used and still build.

Both directions are armed. Job 16 logs `scoped-parents=0` and `dispatched=0` on
nearly every tick, so an arm that turns a deliberate skip into a shortfall must
be CAUGHT, or the job paints red every five minutes and its exit code stops
meaning anything. The sharpest is the 404: a sibling session that no longer
EXISTS is not one the tick could not read.

The repository is resolved from __file__ and printed, so running this from one
checkout can never score another. Files are restored from bytes captured in
this process, never with `git checkout`, so uncommitted work scores as itself.
"""
import pathlib
import re
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
MAIN = REPO / "cmd" / "kanban-dispatcher" / "main.go"
PKG = "./cmd/kanban-dispatcher/"


def silence(message_start):
    """An arm that turns the one Record whose format starts so into a no-op."""
    return ('degraded.Record("' + message_start, '_ = fmt.Sprintf("' + message_start)


def record_after(anchor, reason, recorder="degraded"):
    """An arm that records a shortfall right after anchor — over-recording."""
    indent = re.match(r"\t*", anchor.split("\n")[-2] if anchor.endswith("\n") else anchor).group(0)
    return (anchor, anchor + indent + recorder + '.Record("' + reason + '")\n')


# name -> (old, new, expectation)
#   "caught"   = at least one test must go red
#   "survives" = the suite must stay green (a control)
ARMS = [
    # ------------------------------------------------ each Record silenced
    ("drop-the-unreadable-scoped-tag-record",
     *silence("goal %s (%q): its scoped tag could not be read"), "caught"),
    ("drop-the-unlistable-children-record",
     *silence("goal %s: its children could not be listed"), "caught"),
    ("drop-the-unreadable-sibling-links-record",
     *silence("goal %s group %q: sibling card %s could not be read"), "caught"),
    ("drop-the-unreadable-session-state-record",
     *silence("goal %s group %q: the state of sibling session"), "caught"),
    ("drop-the-unlistable-siblings-record",
     *silence("goal %s group %q: its sibling cards could not be listed"), "caught"),
    ("drop-the-closure-children-record",
     *silence("goal %s (%s): its children could not be listed for the closure pass"), "caught"),
    ("drop-the-unreadable-placement-record",
     *silence("goal %s (%s): child %s is %s but its placement could not be read"), "caught"),
    ("drop-the-refused-closure-move-record",
     *silence("goal %s (%s): child %s is %s but the board refused to move it"), "caught"),
    ("drop-the-rollup-children-record",
     *silence("goal %s (%s): its children could not be listed for the rollup"), "caught"),
    ("drop-the-refused-rollup-note-record",
     *silence("goal %s (%s): the board refused its rollup note"), "caught"),
    ("drop-the-refused-rollup-move-record",
     *silence("goal %s (%s): all %d of its children are settled"), "caught"),
    ("drop-the-undispatched-card-record",
     *silence("card %s %q was not dispatched"), "caught"),

    # ------------------------------------------------ the verdict
    ("the-run-always-succeeds",
     "\tif degraded.HasShortfall() {", "\tif false && degraded.HasShortfall() {", "caught"),
    ("only-the-first-shortfall-is-kept",
     "reasons := degraded.Reasons()", "reasons := degraded.Reasons()[:1]", "caught"),

    # ------------------------------------------------ a report that names nothing
    ("the-scoped-tag-report-does-not-name-the-goal",
     '"goal %s (%q): its scoped tag', '"goal %.0s(%q): its scoped tag', "caught"),
    ("the-unreadable-sibling-report-does-not-name-the-sibling",
     "sibling card %s could not be read", "sibling card %.0scould not be read", "caught"),
    ("the-unreadable-session-state-report-does-not-name-the-session",
     "the state of sibling session %s could not be read", "the state of sibling session %.0scould not be read", "caught"),
    ("the-unreadable-placement-report-does-not-name-the-child",
     "child %s is %s but its placement", "child %.0sis %s but its placement", "caught"),
    ("the-refused-closure-move-report-does-not-name-the-child",
     "child %s is %s but the board refused", "child %.0sis %s but the board refused", "caught"),
    ("the-undispatched-report-does-not-name-the-card",
     '"card %s %q was not dispatched', '"card %.0s%.0q was not dispatched', "caught"),

    # ------------------------------------------------ nothing to do is not a shortfall
    ("a-gone-session-is-a-shortfall",
     "\t\tif errors.Is(err, errSessionGone) {", "\t\tif false && errors.Is(err, errSessionGone) {", "caught"),
    ("a-busy-session-is-a-shortfall",
     *record_after('\t\tif session.State != "idle" {\n', "busy sibling"), "caught"),
    ("an-unsettled-child-is-a-shortfall",
     *record_after("\t\t\t\t// Still being worked on.\n", "unsettled child"), "caught"),
    ("a-child-not-on-this-board-is-a-shortfall",
     *record_after("nothing to move\", p.title(), shortID(e.CardID), status)\n\t\t\t\t", "not on this board"), "caught"),
    ("the-per-tick-cap-is-a-shortfall",
     *record_after("\t\t\tif dispatched >= d.maxDispatch {\n", "cap", recorder="d.degraded"), "caught"),
    ("a-goal-with-no-children-is-a-shortfall",
     "\t\tif total == 0 || settled < total {\n",
     "\t\tif total == 0 {\n\t\t\tdegraded.Record(\"no children\")\n\t\t}\n\t\tif total == 0 || settled < total {\n", "caught"),

    # ------------------------------------------------ behaviour the records sit on
    ("a-failed-children-read-reads-as-no-children",
     '\t\treturn 0, 0, "", err', '\t\treturn 0, 0, "", nil', "caught"),
    ("a-refused-rollup-note-stops-the-move",
     "p.cardID, p.title(), err, total)\n\t\t}", "p.cardID, p.title(), err, total)\n\t\t\tcontinue\n\t\t}", "caught"),
    ("the-closure-pass-writes-on-a-dry-run",
     "\t\t\tif options.DryRun {", "\t\t\tif false && options.DryRun {", "caught"),
    ("the-rollup-writes-on-a-dry-run",
     "children settled\", p.title(), settled, total)\n\t\tif options.DryRun {",
     "children settled\", p.title(), settled, total)\n\t\tif false && options.DryRun {", "caught"),
    ("the-placement-read-drops-the-service-token",
     "placements, err := kc.ListCardPlacements(cardID)",
     'placements, err := kanbanclient.New(kc.BaseURL, "").ListCardPlacements(cardID)', "caught"),

    # ================================================================
    # Ported from the branch scorer sabotage-kanban-dispatcher.py
    # (51c1fa69, 45860cdb, f119a711 on test/the-mint-site-truncates-a-long-
    # parent-id) and re-aimed at main. Rows whose mechanism main no longer
    # has — the board found by name, an absent board as a quiet tick, envOr,
    # the SCHEDULER_URL default, the -register path through main() — were
    # dropped rather than kept as ABORTED.
    # ================================================================

    # ------------------------------------------------ wire values another program wrote
    ("the-goals-column-name-drifts",
     "if cv.Column.Name == columnGoals {", 'if cv.Column.Name == "goals" {', "caught"),
    ("the-backlog-column-name-drifts",
     "if cv.Column.Name == columnBacklog {", 'if cv.Column.Name == "To do" {', "caught"),
    ("the-in-progress-column-a-dispatched-card-moves-to-drifts",
     "inProgressColumnID: colByName[columnInProgress],", 'inProgressColumnID: colByName["In progress"],', "caught"),
    ("the-done-column-the-closure-pass-moves-to-drifts",
     "targetCol, targetName = colByName[columnDone], columnDone", 'targetCol, targetName = colByName["done"], columnDone', "caught"),
    ("the-failed-column-an-archived-child-moves-to-drifts",
     "targetCol, targetName = colByName[columnFailed], columnFailed", 'targetCol, targetName = colByName["failed"], columnFailed', "caught"),
    ("the-scoped-tag-drifts",
     "if !anyTagEquals(etags, tagScoped) {", 'if !anyTagEquals(etags, "is-scoped") {', "caught"),
    ("the-scoped-gate-stops-applying",
     "if !anyTagEquals(etags, tagScoped) {", "if false && !anyTagEquals(etags, tagScoped) {", "caught"),
    ("the-group-tag-prefix-drifts",
     "if strings.HasPrefix(t, tagGroupPrefix) {\n\t\t\treturn strings.TrimPrefix(t, tagGroupPrefix)",
     'if strings.HasPrefix(t, "grp:") {\n\t\t\treturn strings.TrimPrefix(t, "grp:")', "caught"),
    ("the-parent-card-link-type-drifts",
     "entries, err := kc.ListCardsForEntity(linkTypeParent, parentID)\n\tif err != nil {\n\t\treturn nil, err",
     'entries, err := kc.ListCardsForEntity("parent", parentID)\n\tif err != nil {\n\t\treturn nil, err', "caught"),
    ("the-session-link-is-written-with-an-entity-type-nothing-reads",
     'd.kanban.AddCardLink(childID, linkTypeSession, sessionID, "tcl-dispatcher")',
     'd.kanban.AddCardLink(childID, "bus_session", sessionID, "tcl-dispatcher")', "caught"),
    ("the-session-link-points-at-another-id-than-the-one-published",
     'd.kanban.AddCardLink(childID, linkTypeSession, sessionID, "tcl-dispatcher")',
     'd.kanban.AddCardLink(childID, linkTypeSession, sessionID+"-card", "tcl-dispatcher")', "caught"),
    ("the-session-link-read-in-the-revive-decision-drifts",
     "if l.EntityType == linkTypeSession {", 'if l.EntityType == "bus_session" {', "caught"),

    # ------------------------------------------------ gates that WIDEN: they bill
    ("a-session-that-is-not-idle-is-revived",
     'if session.State != "idle" {', 'if session.State == "" {', "caught"),
    ("the-revive-window-widens",
     "if now.Sub(updatedAt) > reviveWindow {", "if now.Sub(updatedAt) > 100*reviveWindow {", "caught"),
    # Returns rather than `if false`: with the check off, time.Parse("") fails
    # and the unreadable-age check below skips the session anyway, so that
    # mutation changes nothing and scored MISSED for a reason no test can fix.
    ("a-session-with-no-updated-at-is-revived",
     "reports no updated_at — not reviving on an unknown age\", candidateID, sid)\n\t\t\tcontinue",
     "reports no updated_at — not reviving on an unknown age\", candidateID, sid)\n\t\t\treturn sid", "caught"),
    ("a-session-with-an-unparseable-updated-at-is-revived",
     "has an unreadable updated_at %q: %v — not reviving\", candidateID, sid, session.UpdatedAt, err)\n\t\t\tcontinue",
     "has an unreadable updated_at %q: %v — not reviving\", candidateID, sid, session.UpdatedAt, err)\n\t\t\treturn sid", "caught"),
    ("the-per-group-dispatch-cap-is-dropped",
     "for _, g := range groups {\n\t\t\tif dispatched >= d.maxDispatch {\n\t\t\t\tbreak\n\t\t\t}",
     "for _, g := range groups {", "caught"),
    ("a-dry-run-publishes-and-writes",
     "if d.dryRun {\n\t\t\t\tdispatched++\n\t\t\t\tcontinue\n\t\t\t}", "", "caught"),
    ("a-card-already-in-its-target-column-is-moved-again",
     "if placement == targetCol {\n\t\t\t\tcontinue\n\t\t\t}", "", "caught"),
    ("a-card-with-no-placement-on-this-board-is-moved",
     'if placement == "" {', 'if false && placement == "" {', "caught"),
    ("a-child-neither-done-nor-archived-is-closed",
     "\t\t\t\t// Still being worked on.\n\t\t\t\tcontinue\n",
     "\t\t\t\ttargetCol, targetName = colByName[columnDone], columnDone\n", "caught"),
    ("a-parent-with-no-children-is-rolled-up",
     "if total == 0 || settled < total {", "if settled < total {", "caught"),
    ("a-parent-is-rolled-up-while-a-child-is-open",
     "if total == 0 || settled < total {", "if total == 0 {", "caught"),
    ("a-failed-publish-still-links-and-moves",
     'notDispatched(fmt.Errorf("nats publish: %w", err))\n\t\t\t\tcontinue',
     'notDispatched(fmt.Errorf("nats publish: %w", err))', "caught"),
    ("a-failed-session-link-still-moves",
     "linking it to the card failed: %w\", sessionID, err))\n\t\t\t\tcontinue",
     "linking it to the card failed: %w\", sessionID, err))", "caught"),

    # ------------------------------------------------ gates that NARROW: they go quiet
    ("an-idle-sibling-is-never-reused",
     "if sessionID == \"\" {\n\t\t\t\tsessionID = mintSessionID(p.cardID, g.key)\n\t\t\t}",
     "sessionID = mintSessionID(p.cardID, g.key)", "caught"),
    ("a-minted-session-id-stops-naming-its-group",
     'return fmt.Sprintf("tcl-%s-%s-%d", shortID(parentID), safeGroup, now)',
     'return fmt.Sprintf("tcl-%s-%.0s%d", shortID(parentID), safeGroup, now)', "caught"),
    ("an-ungrouped-card-mints-an-empty-segment",
     'if safeGroup == "" {\n\t\tsafeGroup = "default"\n\t}', "", "caught"),
    ("shortid-stops-shortening",
     "textcut.PrefixWithinByteBudgetOnRuneBoundary(id, 8)",
     "textcut.PrefixWithinByteBudgetOnRuneBoundary(id, len(id))", "caught"),
    ("the-mint-site-takes-a-raw-byte-cut",
     'return fmt.Sprintf("tcl-%s-%s-%d", shortID(parentID), safeGroup, now)',
     'return fmt.Sprintf("tcl-%s-%s-%d", parentID[:8], safeGroup, now)', "caught"),
    ("the-mint-site-stops-shortening",
     'return fmt.Sprintf("tcl-%s-%s-%d", shortID(parentID), safeGroup, now)',
     'return fmt.Sprintf("tcl-%s-%s-%d", parentID, safeGroup, now)', "caught"),
    ("the-loop-mints-from-the-childs-id",
     "sessionID = mintSessionID(p.cardID, g.key)", "sessionID = mintSessionID(childID, g.key)", "caught"),
    ("the-closure-log-takes-a-raw-byte-cut",
     'log.Printf("[%s] closure: child %s status=%s → %s", p.title(), shortID(e.CardID), status, targetName)',
     'log.Printf("[%s] closure: child %s status=%s → %s", p.title(), e.CardID[:8], status, targetName)', "caught"),

    # ------------------------------------------------ a tick that cannot start
    ("a-board-missing-a-lifecycle-column-is-a-clean-tick",
     'return fmt.Errorf("board %q missing required column %q", board.Name, want)', "return nil", "caught"),
    ("an-unreadable-board-view-is-a-clean-tick",
     'return fmt.Errorf("board view: %w", err)', "return nil", "caught"),
    ("an-unreadable-board-is-a-clean-tick",
     'return fmt.Errorf("get board %s: %w", options.BoardID, err)', "return nil", "caught"),
    ("a-board-that-is-gone-is-a-clean-tick",
     "if errors.Is(err, kanbanclient.ErrBoardNotFound) {\n\t\treturn fmt.Errorf(",
     "if errors.Is(err, kanbanclient.ErrBoardNotFound) {\n\t\treturn nil\n\t\t_ = fmt.Errorf(", "caught"),
    ("the-rollup-note-overwrites-the-goals-body",
     "kc.PatchCardBody(p.cardID, body+suffix)", "kc.PatchCardBody(p.cardID, suffix+body[:0])", "caught"),
    ("currentplacement-answers-for-the-first-board-listed",
     "\t\tif p.BoardID == boardID {\n\t\t\treturn p.ColumnID, nil",
     "\t\tif boardID != \"\" {\n\t\t\treturn p.ColumnID, nil", "caught"),

    # ------------------------------------------------ the message this binary publishes
    ("auto-start-is-dropped",
     "AutoStart:   true,", "AutoStart:   false,", "caught"),
    ("the-session-purpose-drifts",
     'Purpose:     "dispatcher",', 'Purpose:     "kanban-dispatcher",', "caught"),
    ("the-session-origin-drifts",
     'Origin:      "scheduler",', 'Origin:      "kanban-dispatcher",', "caught"),
    ("the-session-type-drifts",
     "Type:        msg.SessionTypeAutonomous,", "Type:        msg.SessionTypeSystem,", "caught"),
    ("the-agent-label-drifts",
     'Agent:        "tcl-worker",', 'Agent:        "worker",', "caught"),
    ("the-orchestrator-label-drifts",
     'Orchestrator: "task-completion-loop",', 'Orchestrator: "tcl",', "caught"),
    ("the-channel-label-drifts",
     'Channel:      "task-completion-loop",', 'Channel:      "tcl",', "caught"),
    ("the-envelope-is-stamped-in-local-time",
     "Timestamp:    time.Now().UTC(),", "Timestamp:    time.Now(),", "caught"),
    ("a-revived-session-asks-for-a-second-create",
     "if fresh {\n\t\tin.Create = &msg.CreateSessionRequest{", "if true {\n\t\tin.Create = &msg.CreateSessionRequest{", "caught"),

    # ------------------------------------------------ the prompt, which is the sub-task
    ("the-worker-is-told-to-close-its-parent",
     "header, childID, title, body, p.title(), p.cardID, group, childID)",
     "header, childID, title, body, p.title(), p.cardID, group, p.cardID)", "caught"),
    ("the-prompt-drops-the-cards-body",
     'body, _ := child.Item["body"].(string)\n\tif body == "" {',
     'body, _ := child.Item["no-such-field"].(string)\n\tif body == "" {', "caught"),
    ("a-bodyless-card-leaves-a-blank",
     'body = "(no body — see title)"', 'body = ""', "caught"),
    ("a-revived-worker-is-prompted-as-fresh",
     'header = "Picking up another sub-task in the same group — you already have context from a sibling task in this session."',
     'header = "You are picking up a sub-task in the **task-completion-loop** workflow."', "caught"),
    ("an-untitled-parent-is-an-empty-string",
     "func (p scopedParent) title() string {\n\tt, _ := p.item[\"title\"].(string)\n\tif t == \"\" {",
     "func (p scopedParent) title() string {\n\tt, _ := p.item[\"title\"].(string)\n\tif false {", "caught"),
    ("an-untitled-child-is-an-empty-string",
     "func childTitle(c kanbanclient.CardView) string {\n\tt, _ := c.Item[\"title\"].(string)\n\tif t == \"\" {",
     "func childTitle(c kanbanclient.CardView) string {\n\tt, _ := c.Item[\"title\"].(string)\n\tif false {", "caught"),

    # ------------------------------------------------ reading tags
    ("a-tag-that-is-not-a-string-hides-the-tags-after-it",
     "\t\tif s, ok := x.(string); ok {\n\t\t\tout = append(out, s)\n\t\t}",
     "\t\ts, ok := x.(string)\n\t\tif !ok {\n\t\t\tbreak\n\t\t}\n\t\tout = append(out, s)", "caught"),
    ("the-scoped-tag-is-matched-by-prefix",
     "\t\tif t.Tag == want {", "\t\tif strings.HasPrefix(t.Tag, want) {", "caught"),

    # ------------------------------------------------ the rollup's arithmetic
    ("an-archived-child-is-counted-as-open",
     'case "archived":\n\t\t\tarchived++', 'case "archived":\n\t\t\tother++', "caught"),
    ("the-rollup-note-reports-failed-children-as-done",
     'summary := fmt.Sprintf("children: done=%d failed=%d open=%d", done, archived, other)',
     'summary := fmt.Sprintf("children: done=%d failed=%d open=%d", done+archived, 0, other)', "caught"),

    # ------------------------------------------------ reading a sibling session
    ("a-bridge-answering-500-reads-as-a-session-not-idle",
     "if resp.StatusCode != http.StatusOK {\n\t\tbody, _ := io.ReadAll(resp.Body)\n\t\treturn bridgeSession{}, ",
     "if false {\n\t\tbody, _ := io.ReadAll(resp.Body)\n\t\treturn bridgeSession{}, ", "caught"),

    # ------------------------------------------------ groupSiblings
    ("sibling-search-stops-filtering-by-parent",
     "\t\t\tif !parentChildren[c.Placement.CardID] {\n\t\t\t\tcontinue\n\t\t\t}\n\t\t\tif c.Item == nil {",
     "\t\t\tif false && !parentChildren[c.Placement.CardID] {\n\t\t\t\tcontinue\n\t\t\t}\n\t\t\tif c.Item == nil {", "caught"),
    ("sibling-search-stops-filtering-by-group",
     "\t\t\tif groupKey(c.Item) != key {\n\t\t\t\tcontinue\n\t\t\t}", "\t\t\t_ = key", "caught"),

    # ------------------------------------------------ -register
    ("the-registered-job-name-drifts",
     '"name":        "kanban-dispatcher-tick",', '"name":        "kanban-dispatcher",', "caught"),
    ("the-registered-cadence-drifts",
     '"schedule":    "*/5 * * * *",', '"schedule":    "*/30 * * * *",', "caught"),
    ("the-registered-command-names-another-binary",
     'bin := filepath.Join(home, "bin", "kanban-dispatcher") + " -board-id " + boardID',
     'bin := filepath.Join(home, "bin", "kanban-curator") + " -board-id " + boardID', "caught"),
    ("the-registered-command-drops-the-board",
     'bin := filepath.Join(home, "bin", "kanban-dispatcher") + " -board-id " + boardID',
     'bin := filepath.Join(home, "bin", "kanban-dispatcher") + boardID[:0]', "caught"),
    ("the-registered-job-is-a-type-the-scheduler-cannot-run",
     '"type":        "shell",', '"type":        "agent",', "caught"),
    ("an-existing-tick-is-not-recognised",
     'if j.Name == "kanban-dispatcher-tick" {', 'if j.Name == "kanban-dispatcher-tick-hourly" {', "caught"),
    ("the-job-name-check-matches-by-prefix",
     'if j.Name == "kanban-dispatcher-tick" {', 'if strings.HasPrefix(j.Name, "kanban-dispatcher") {', "caught"),
    ("a-refused-registration-is-reported-as-success",
     "if resp.StatusCode >= 300 {", "if false && resp.StatusCode >= 300 {", "caught"),
    ("a-job-list-answered-500-reads-as-no-tick",
     "if resp.StatusCode != http.StatusOK {\n\t\treturn fmt.Errorf(\"list jobs: scheduler returned",
     "if false && resp.StatusCode != http.StatusOK {\n\t\treturn fmt.Errorf(\"list jobs: scheduler returned", "caught"),
    ("a-job-list-that-is-not-json-reads-as-no-tick",
     "if err := json.Unmarshal(body, &jobs); err != nil {", "if err := json.Unmarshal(body, &jobs); false && err != nil {", "caught"),
    ("the-scheduler-is-read-at-a-path-it-does-not-serve",
     'resp, err := http.Get(schedURL + "/api/jobs")', 'resp, err := http.Get(schedURL + "/api/job")', "caught"),

    # ------------------------------------------------ boundary values
    # Ported from 081b8ac6. Every arm above moves a direction, renames a wire
    # string or deletes a mechanism; none moves a NUMBER by one unit. Fixtures
    # written in terms of the constant (`-2*reviveWindow`) sit on the right
    # side of it whatever its value, so each value needs a fixture that quotes
    # the number itself. The compiled-in ports 081b8ac6 also moved now live in
    # internal/config, outside this file's one target. shortID's `<= 7` is not
    # armed: a budget of 7 differs from 8 only at length 8, where the guard and the
    # cut return the same string, so no input can tell them apart.
    ("the-revive-window-grows-by-a-minute",
     "reviveWindow       = time.Hour\n", "reviveWindow       = time.Hour + time.Minute\n", "caught"),
    ("the-revive-window-shrinks-by-a-minute",
     "reviveWindow       = time.Hour\n", "reviveWindow       = time.Hour - time.Minute\n", "caught"),
    ("the-per-tick-dispatch-cap-grows-by-one",
     "maxDispatchPerTick = 3\n", "maxDispatchPerTick = 4\n", "caught"),
    ("the-per-tick-dispatch-cap-shrinks-by-one",
     "maxDispatchPerTick = 3\n", "maxDispatchPerTick = 2\n", "caught"),
    ("the-register-success-threshold-moves-up-one-status",
     "if resp.StatusCode >= 300 {", "if resp.StatusCode >= 301 {", "caught"),
    ("the-register-success-threshold-moves-down-one-status",
     "if resp.StatusCode >= 300 {", "if resp.StatusCode >= 299 {", "caught"),
    ("shortid-keeps-one-more-byte-whole",
     "textcut.PrefixWithinByteBudgetOnRuneBoundary(id, 8)",
     "textcut.PrefixWithinByteBudgetOnRuneBoundary(id, 9)", "caught"),
    ("a-dispatched-card-lands-one-row-down-in-progress",
     "d.kanban.MoveCard(childID, d.board.ID, d.inProgressColumnID, 0)",
     "d.kanban.MoveCard(childID, d.board.ID, d.inProgressColumnID, 1)", "caught"),
    ("a-closed-child-lands-one-row-down-its-column",
     "kc.MoveCard(e.CardID, board.ID, targetCol, 0)", "kc.MoveCard(e.CardID, board.ID, targetCol, 1)", "caught"),
    ("a-rolled-up-parent-lands-one-row-down-done",
     "kc.MoveCard(p.cardID, board.ID, colByName[columnDone], 0)",
     "kc.MoveCard(p.cardID, board.ID, colByName[columnDone], 1)", "caught"),
    ("the-noteboard-port-in-the-prompt-drifts-by-one",
     "PATCH http://localhost:8191/api/items/%s", "PATCH http://localhost:8192/api/items/%s", "caught"),

    ("shortid-cuts-mid-rune",
     "textcut.PrefixWithinByteBudgetOnRuneBoundary(id, 8)",
     "func() string { _ = textcut.Ellipsis; if len(id) <= 8 { return id }; return id[:8] }()", "caught"),
    ("the-title-key-drifts",
     't, _ := c.Item["title"].(string)', 't, _ := c.Item["name"].(string)', "caught"),
    ("a-titled-child-is-named-by-its-id",
     "\tif t == \"\" {\n\t\tt = c.Placement.CardID", "\tif true {\n\t\tt = c.Placement.CardID", "caught"),

    # ------------------------------------------------ controls
    ("control-reword-the-rollup-progress-line",
     '"[%s] rollup: %d/%d children settled"', '"[%s] rollup ready: %d of %d children settled"', "survives"),
    ("control-reword-the-not-on-this-board-line",
     "is not placed on this board — nothing to move", "has no placement on this board, so there is nothing to move", "survives"),
]

ABORT_MARKERS = ("build failed", "undefined:", "imported and not used",
                 "declared and not used", "syntax error", "cannot use",
                 "[build failed]", "no required module", "not enough arguments",
                 "too many arguments")


def run_tests():
    p = subprocess.run(["go", "test", "-count=1", PKG], cwd=REPO,
                       capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def main():
    if "--list-sabotages" in sys.argv:
        for name, _, _, _ in ARMS:
            print(name)
        return 0

    print(f"scoring repository: {REPO}")
    head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=REPO,
                          capture_output=True, text=True).stdout.strip()
    dirty = subprocess.run(["git", "status", "--porcelain", "--", str(MAIN)],
                           cwd=REPO, capture_output=True, text=True).stdout.strip()
    print(f"scoring commit:     {head}{' (main.go DIRTY -- you are scoring uncommitted bytes)' if dirty else ''}")

    original = MAIN.read_text()

    code, out = run_tests()
    if code != 0:
        print("BASELINE IS RED -- fix that before scoring any arm:\n" + out)
        return 2
    print("baseline: GREEN\n")

    only = None
    if "--sabotage" in sys.argv:
        only = sys.argv[sys.argv.index("--sabotage") + 1]

    caught = missed = aborted = survived = wrong = 0
    for name, old, new, expect in ARMS:
        if only and name != only:
            continue
        if old not in original:
            print(f"  ABORTED  {name}  -- anchor not found in main.go; the arm points at code that moved")
            aborted += 1
            continue
        if original.count(old) != 1:
            print(f"  ABORTED  {name}  -- anchor matches {original.count(old)} times; the edit would be one nobody chose")
            aborted += 1
            continue
        MAIN.write_text(original.replace(old, new, 1))
        try:
            code, out = run_tests()
        finally:
            MAIN.write_text(original)

        if any(m in out for m in ABORT_MARKERS):
            print(f"  ABORTED  {name}  -- the mutation did not compile, so nothing was tested")
            aborted += 1
            continue

        red = code != 0
        names = sorted(set(re.findall(r"--- FAIL: (\w+)", out)))
        if expect == "caught":
            if red:
                print(f"  CAUGHT   {name}  by {', '.join(names) or 'the package'}")
                caught += 1
            else:
                print(f"  MISSED   {name}  -- no test noticed")
                missed += 1
        else:
            if red:
                print(f"  WRONG    {name}  -- a control went red ({', '.join(names)}): the suite is pinning the wording, not the behaviour")
                wrong += 1
            else:
                print(f"  SURVIVES {name}  (control, as intended)")
                survived += 1

    print(f"\nCAUGHT {caught} / MISSED {missed} / ABORTED {aborted}"
          f"  |  controls SURVIVED {survived} / WRONG {wrong}")
    return 0 if (missed == 0 and aborted == 0 and wrong == 0) else 1


if __name__ == "__main__":
    sys.exit(main())
