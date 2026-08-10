#!/usr/bin/env python3
"""Score cmd/reminder-coordinator's tests by breaking the binary on purpose.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-reminder-coordinator.py [--diffs]

Why this binary was scored before the other unscored ones: it is the only thing
on this box that reminds the user of anything. It materializes recurring todos,
rewrites the standing "📆 Today — personal" note, mirrors to Google Calendar and
sends the single herald session per tick. Its failure mode is silence, and
silence is indistinguishable from a quiet day, so nothing downstream notices.

Case-writing rules learned the expensive way, and re-earned here:

  - Prefer a DRIFTED COMPARISON to a deletion. It orphans no variable, needs no
    second edit, and is the likelier real-world regression. `&& false` is the
    next-best thing when a whole guard has to go: the identifiers stay
    referenced, so the case reports a score instead of a compile error.
  - Read every applied diff against its label (`--diffs`). A row prints the name
    you gave it, not the edit you made.
  - An UNNOTICED row is not automatically a coverage hole. It can be a
    behavioural no-op, or a fake in the test file that swallows the value the
    mutation moved. Both appear below, named, with expected_unnoticed set.

Score: 35/35, exit 0. Cross-table re-taken 2026-08-08 after the four below were
closed (`--crosstable`): every one of the 30 tests is reddened by at least one
case, so this binary has no silent column and needs no `unreddened` declaration.
Do not re-derive it; re-run the mode instead.

That column was not empty when it was first measured. Five tests were reddened
by nothing, and all five turned out to carry a failure DIRECTION the case list
did not express rather than to pin nothing:

    ...StillOpensOnlyOneSession      two reasons to nudge opening two sessions
    ExhaustedRollingSeriesStaysDone  a COUNT= series reopened instead of retiring
    CalendarIsOptInPerItem           an item with no schedule treated as opted in
    DayLoadFlagsAFragmentedDay       the 90-minute fragmentation threshold itself
    DigestSurvivesCalendarReadFailure  a failed load rendering as an EMPTY calendar

The reason they clustered like that is worth keeping: every case above them was
drafted from a card worried about SILENCE — a reminder that fails to arrive — so
every case broke the binary in that direction. The five tests sit on the other
side, where the coordinator does too much rather than too little. The five cases
that close them are the ones naming a doubled session, a series that will not
end, an unasked-for calendar event, and a calendar failure that reads as a free
day.

The four cases that scored UNNOTICED after that — backfill floor, rolling anchor,
calendar update guard, day boundary — clustered too, and along a different axis.
EVERY ONE of them compares two instants, and no fixture had ever made those two
instants disagree:

    floor      now-24h vs the template's anchor   DTStart was always ~now
    anchor     now vs the item's own due date     both on one morning, monthly rule
    guard      the gcalsync stamp vs due_at       equal, or one of them absent
    boundary   an event's start vs midnight       every event mid-afternoon

A fixture written to ask "does this mechanism run?" naturally puts every timestamp
it needs on the same day, and a mechanism whose whole job is to measure a DISTANCE
between two timestamps is then unobservable while appearing fully exercised. All
four are now pinned by fixtures that spread their instants apart on purpose —
a template anchored 81 days back, an item two days overdue and another completed
four days early, a stamp that disagrees with its due date, an event at 00:15
tomorrow. When a case scores UNNOTICED, ask what two values the mechanism compares
and check whether the fixture ever made them differ.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

TARGET = REPO / "cmd/reminder-coordinator/main.go"
PACKAGES = ["./cmd/reminder-coordinator/"]


CASES = [
    # ---- controls ----
    Case(
        "CONTROL known-positive: a materialized child inherits none of its parent's tags",
        [("tags := append([]string(nil), template.Tags...)", "tags := []string{}")],
    ),
    Case(
        "CONTROL known-negative: reword the materialize log line",
        [('log.Printf("%smaterialize: %q due %s", dryPrefix(c.DryRun), template.Title,',
          'log.Printf("%sMATERIALIZE %q at %s", dryPrefix(c.DryRun), template.Title,')],
        expected_unnoticed="log wording for a human reading journalctl; no test reads it, and "
                           "nothing downstream parses it",
    ),

    # ---- materialization: the safety properties ----
    Case(
        "the child inherits the parent's RRULE, so occurrences start spawning occurrences",
        [("\t\tParentID: &template.ID,", "\t\tParentID: &template.ID,\n\t\tSchedule: template.Schedule,")],
    ),
    Case(
        "the duplicate-child guard stops firing, so every tick re-materializes the same occurrence",
        [("if hasChildDueAt(existing, occurrence) {", "if false && hasChildDueAt(existing, occurrence) {")],
    ),
    Case(
        "hasChildDueAt compares the wrong way, so an existing child never matches",
        [("if child.DueAt != nil && child.DueAt.Equal(occurrence) {",
          "if child.DueAt != nil && child.DueAt.After(occurrence) {")],
    ),
    Case(
        "last_materialized_at never advances, so the second guard is the only one left",
        [("if latest != nil && (template.Schedule.LastMaterializedAt == nil || latest.After(*template.Schedule.LastMaterializedAt)) {",
          "if false && (template.Schedule.LastMaterializedAt == nil || latest.After(*template.Schedule.LastMaterializedAt)) {")],
    ),
    Case(
        "an archived template keeps producing occurrences",
        [('if template.Status == "archived" {', 'if template.Status == "deleted" {')],
    ),
    Case(
        "the backfill floor widens to a year, so a long-idle tick dumps months of chores at once",
        [("\t\tfrom := c.Now.Add(-24 * time.Hour)\n\t\tif last := template.Schedule.LastMaterializedAt; last != nil {",
          "\t\tfrom := c.Now.AddDate(-1, 0, 0)\n\t\tif last := template.Schedule.LastMaterializedAt; last != nil {")],
    ),

    # ---- board bucketing: an item that lands in no bucket vanishes ----
    Case(
        "a recurring template is listed as work alongside its own occurrence",
        [("if isTemplate(item) {\n\t\t\tcontinue", "if false && isTemplate(item) {\n\t\t\tcontinue")],
    ),
    Case(
        "isTemplate inverts, so rolling items are hidden and templates are shown",
        [("item.Schedule.EffectiveMode() == model.ScheduleModeInstance\n}",
          "item.Schedule.EffectiveMode() != model.ScheduleModeInstance\n}")],
    ),
    Case(
        "the Later bucket drops its items, so anything past this week vanishes from the board",
        [("\t\t\tboard.Later = append(board.Later, item)", "\t\t\t_ = item")],
    ),
    Case(
        "undated items are dropped instead of bucketed",
        [("board.Undated = append(board.Undated, item)\n\t\t\tcontinue", "_ = item\n\t\t\tcontinue")],
    ),
    Case(
        "the overdue boundary drifts a day, so today's work is reported as overdue",
        [("case due.Before(todayDate):", "case due.Before(todayDate.AddDate(0, 0, 1)):")],
    ),

    # ---- nags: fire until done, once per firing ----
    Case(
        "the last-nagged floor drifts back a year, so a nag re-fires on every 15-minute tick",
        [("from = last.Add(time.Second)\n\t\t}\n\n\t\toccurrences, err := item.Schedule.NagOccurrences(",
          "from = last.Add(-365 * 24 * time.Hour)\n\t\t}\n\n\t\toccurrences, err := item.Schedule.NagOccurrences(")],
    ),
    Case(
        "a nag keeps firing after the item is done",
        [('if item.Status != "open" {', 'if item.Status == "archived" {')],
    ),
    Case(
        "last_nagged_at is never written back, so the nag re-fires next tick",
        [("if err := c.noteboard.SetSchedule(item.ID, &stamped, false); err != nil {",
          "if err := func() error { _ = stamped; return nil }(); err != nil {")],
    ),
    Case(
        "the quiet-tick gate opens, so a session is sent every 15 minutes with nothing to say",
        [("if !digestDue && len(board.NagFiring) == 0 {", "if false && len(board.NagFiring) == 0 {")],
    ),
    Case(
        "the nags get their own session again, so a tick that has both a digest and a nag "
        "opens two — the double-nudge bug this binary was built to end",
        [('\tif len(board.NagFiring) > 0 {\n\t\tmessage.WriteString("\\n🔔 Still not done:\\n")',
          '\tif len(board.NagFiring) > 0 {\n\t\t_ = c.herald.Relay("personal-reminders", "Still not done", "nag")\n'
          '\t\tmessage.WriteString("\\n🔔 Still not done:\\n")')],
    ),

    # ---- the daily digest nudge: at most one per day, never before the hour ----
    Case(
        "the digest hour is ignored by twelve hours, so the nudge lands in the middle of the night",
        [("if now.Before(c.DigestAt.OnDay(now)) {", "if now.Before(c.DigestAt.OnDay(now).Add(-12 * time.Hour)) {")],
    ),
    Case(
        "the already-sent marker never matches, so the digest re-sends every tick after the hour",
        [("if link == todayMarker {", 'if link == todayMarker+"-never" {')],
    ),
    Case(
        "the sent marker loses its day, so one send suppresses the whole month",
        [('return "digest-sent:" + day.Format("2006-01-02")', 'return "digest-sent:" + day.Format("2006-01")')],
    ),
    Case(
        "marking the digest sent keeps the old markers and drops every other link",
        [('if !strings.HasPrefix(link, "digest-sent:") {', 'if strings.HasPrefix(link, "digest-sent:") {')],
    ),

    # ---- rolling items ----
    Case(
        "a completed rolling item is rolled but left done, so it never comes back",
        [('"status": "open",', '"status": "done",')],
    ),
    Case(
        "instance-mode items are rolled too, so a template is mutated instead of spawning a child",
        [('if item.Schedule.EffectiveMode() != model.ScheduleModeRolling || item.Status != "done" {',
          'if item.Status != "done" {')],
    ),
    Case(
        "the rolling anchor drifts, so the next occurrence is computed from the wrong instant",
        [("if item.DueAt != nil && item.DueAt.After(after) {", "if item.DueAt != nil && item.DueAt.Before(after) {")],
    ),
    Case(
        "an exhausted rolling series is reopened instead of retiring, so a COUNT= rule never ends",
        [('\t\tif next == nil {\n\t\t\tlog.Printf("rolling series exhausted, leaving done: %q", item.Title)\n\t\t\tcontinue\n\t\t}',
          '\t\tif next == nil {\n\t\t\tlog.Printf("rolling series exhausted, leaving done: %q", item.Title)\n\t\t\tnext = &after\n\t\t}')],
    ),

    # ---- calendar mirror: opt-in, idempotent, tears itself down ----
    Case(
        "the calendar opt-in is ignored, so every personal todo lands on the calendar",
        [("item.Schedule.Remind.DeliversOn(model.ChannelCalendar) &&",
          "(item.Schedule != nil || item.Schedule.Remind.DeliversOn(model.ChannelCalendar)) &&")],
    ),
    Case(
        "an item with NO schedule at all counts as opted in, so a plain todo lands on the calendar",
        [("item.Schedule != nil &&\n\t\t\titem.Schedule.Remind.DeliversOn(model.ChannelCalendar) &&",
          "(item.Schedule == nil || item.Schedule.Remind.DeliversOn(model.ChannelCalendar)) &&")],
    ),
    Case(
        "a completed item keeps its calendar event instead of having it torn down",
        [('wantsEvent := item.Status == "open" &&', 'wantsEvent := item.Status != "archived" &&')],
    ),
    Case(
        "the update guard inverts, so every tick rewrites an event that has not moved",
        [("case wantsEvent && syncedDueAt != item.DueAt.Format(time.RFC3339):",
          "case wantsEvent && syncedDueAt == item.DueAt.Format(time.RFC3339):")],
    ),
    Case(
        "the gcal link prefix drifts, orphaning every event personal-todo-calsync created",
        [('linkGoogleCalendarEvent       = "gcal:"', 'linkGoogleCalendarEvent       = "gcalevent:"')],
    ),

    # ---- the digest note itself ----
    Case(
        "a calendar read failure takes the whole digest down with it",
        [('\tevents, calErr := c.todaysCalendar()\n\tif calErr != nil {\n\t\tlog.Printf("reminder-coordinator: calendar read failed, digest will note it: %v", calErr)\n\t}',
          '\tevents, calErr := c.todaysCalendar()\n\tif calErr != nil {\n\t\treturn fmt.Errorf("calendar: %w", calErr)\n\t}')],
    ),
    Case(
        "the digest note is found by a tag nothing wrote, so each tick creates a second note",
        [('digestTag   = "reminder-digest"', 'digestTag   = "reminder-digests"')],
    ),
    Case(
        "today's schedule swallows tomorrow's events",
        [("if !start.Before(dayEnd) {", "if !start.Before(dayEnd.AddDate(0, 0, 1)) {")],
    ),
    Case(
        "the waking-day window shrinks, so day-load over-reports how booked the day is",
        [("windowEnd := dayStart.Add(22 * time.Hour)", "windowEnd := dayStart.Add(18 * time.Hour)")],
    ),
    Case(
        "the fragmented-day threshold drops, so a day chopped into half-hours reads as workable",
        [("case biggest < 90*time.Minute:", "case biggest < 30*time.Minute:")],
    ),
    Case(
        "a calendar read failure renders as an EMPTY calendar, so a failed load reads as a free day",
        [('out.WriteString("\\n## 🗓 Today\'s schedule\\n\\n⚠️ Couldn\'t load your calendar this tick.\\n")',
          'out.WriteString("\\n## 🗓 Today\'s schedule\\n\\nNothing on the calendar — the day is yours.\\n")')],
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGET, PACKAGES, CASES))
