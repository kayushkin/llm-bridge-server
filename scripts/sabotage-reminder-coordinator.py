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

Score: 86/92, exit 0 — 86 real mechanisms caught and six cases carrying an
`expected_unnoticed`. Was 69/75 until the nudge stopped being three counts and
started naming the work, then 78/84; the fifteen cases at the end of the list are
those two passes' mechanisms. One of them (booked hours printed where the line
says free) was UNNOTICED on its first run and is pinned now. Cross-table taken 2026-08-10 (`--crosstable`): every one
of the 32 tests then present is reddened by at least one case, so this binary has
no silent column and needs no `unreddened` declaration. Do not re-derive it;
re-run the mode instead. ⚠️ The 196th pass added 15 tests and did NOT re-take the
cross-table, and the two nudge passes added 12 more, so 27 columns are unmeasured —
re-run `--crosstable` before claiming this file has none.

The jump from 50/57 came from the 196th pass lifting `parseFlags` and
`newCoordinator` out of `main()`. Two rows that had sat under `expected_unnoticed`
since this plan was written — the digest hour default and envOr's guard — each
named that seam as the thing that would falsify them, and both are now real rows
that go red. The card asking for the seam counted NINE flags; the seam made
nineteen rows scorable, because the flags were only half of what `main()` hid.
The other half is the wiring, where every same-typed pair (digest hour against
digest minute, noteboard URL against dash URL) swaps without a compile error.

## The source side, closed 2026-08-10 — the 37 functions this plan never named

`healthcheck/scripts/sabotage-scorer-unmutated-function-scan.py` asks the twin of
the cross-table's question: not which TESTS no case reddens, but which FUNCTIONS
no case breaks. It found this plan the worst on the box — **15 of 52 functions
named, 37 silent**, the largest absolute region of any per-binary scorer. A 35/35
and a file two-thirds unnamed are the same number, which is the whole point.

The 37 split cleanly in two, and only one half was a coverage hole:

  - **Nineteen were real mechanisms** the plan simply had not reached: the digest
    hour's zone, the totality of Board.All, the note write-back guard, dateOf,
    the clock and duration renderers, the two clipping helpers under day-load,
    the priority mark, the area tags, the bucket sort, the calendar read's own
    error path, and both halves of the gcalsync link scheme. Seventeen were
    CAUGHT on the first run — the suite already watched them and the instrument
    was blind to that, which is the flattering direction this scan exists to
    catch. **Two were genuine holes** and are now pinned by new fixtures:
    `roundDuration` (every prior day-load fixture used whole-hour events, so
    rounding to the nearest quarter-DAY still read as "booked … free" and still
    went green) and `sortByDue` (no fixture had ever put two dated items in ONE
    bucket, so with one item per bucket every sort is correct).
  - **Eighteen cannot be observed by this suite at all**, and are declared rather
    than deleted. `main` is flag parsing; `envOr` is reached only from it and
    from ResolveDashToken; `dryPrefix` only ever reaches a log line. The other
    fifteen are the CONCRETE clients — NoteboardClient, GoogleCalendarMirror,
    Herald — which the coordinator holds behind interfaces and the suite replaces
    with fakes wholesale. Those three are declared as **one case each covering
    the whole client**, not one case per method: the claim is about the client,
    and fifteen identical declarations would read as fifteen measurements.

    ⚠️ **Two of those eighteen expired on 2026-08-15 and this is what that looks
    like.** `main` and `envOr` were declared unobservable because nothing in a
    test can call `main()` — true as written, and it stopped being true the
    moment the flag set moved out of `main()` into `parseFlags`. The declarations
    did not have to be guessed at: each one had named its own falsifier in
    advance ("falsified by a test that execs the built binary, or one that calls
    main"), and the engine's both-directions check would have failed the run if
    they had been left in place. **A declaration is a dated claim about the
    suite, not a property of the function** — when the suite changes shape,
    re-read them before assuming they still hold.

`Board.HasActionable` is the odd one out and is worth naming separately: it has
**no caller anywhere** — not in main.go, not in the suite. No mutation of dead
code can be observed by anything, so it is declared, and the declaration says
what falsifies it. It wants wiring or deleting; that is a decision, not this
plan's job.

⚠️ The three client declarations are the ones most likely to expire. They say
"nothing constructs the concrete client in a test", and branch
`test/concrete-clients-speak-the-real-protocol` exists to make that false. The
engine checks a declaration in BOTH directions, so a case that starts being
CAUGHT will say so — do not pre-emptively soften the wording to survive it.

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
        [('out.WriteString("\\n## " + headingSchedule + "\\n\\n⚠️ Couldn\'t load your calendar this tick.\\n")',
          'out.WriteString("\\n## " + headingSchedule + "\\n\\nNothing on the calendar — the day is yours.\\n")')],
    ),

    # ---- the digest hour, the board's totality, and the note write-back ----
    Case(
        "the digest hour is read in UTC instead of the day's own zone, so it lands "
        "seven hours early — at 00:30 Pacific rather than 07:30",
        [("return time.Date(day.Year(), day.Month(), day.Day(), d.Hour, d.Minute, 0, 0, day.Location())",
          "return time.Date(day.Year(), day.Month(), day.Day(), d.Hour, d.Minute, 0, 0, time.UTC)")],
    ),
    Case(
        "Board.All forgets the undated bucket, so a board of nothing but undated "
        "todos renders as \"Nothing open. 🎉\"",
        [("all = append(all, b.Undated...)", "_ = b.Undated")],
    ),
    Case(
        "the digest note is only rewritten when nothing changed, so every real "
        "change to the board is dropped and the note freezes",
        [("if existing.Body == body {", "if existing.Body != body {")],
    ),
    Case(
        "the Overdue heading is rendered from the due-today bucket, so today's work "
        "reads as late and the genuinely overdue items are never shown",
        [("renderSection(&out, digestStyle, headingOverdue, board.Overdue, c.labelWithLateness)",
          "renderSection(&out, digestStyle, headingOverdue, board.DueToday, c.labelWithLateness)")],
    ),
    Case(
        "dateOf rolls the day forward, so \"today\" is tomorrow everywhere at once — "
        "the overdue boundary, the schedule window and the day-load window",
        [("return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())",
          "return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())")],
    ),

    # ---- how the day is rendered and measured ----
    Case(
        "wall-clock times render on a 24-hour clock, so the digest reads 20:00 "
        "where every other line of it says 8pm",
        [('return t.Format("3pm")', 'return t.Format("15:04")')],
    ),
    Case(
        "durations round to the nearest quarter-day instead of the nearest half "
        "hour, so a 90-minute meeting reports as 6h booked",
        [("half := math.Round(d.Hours()*2) / 2", "half := math.Round(d.Hours()*4) / 2")],
    ),
    Case(
        "an event that starts before the waking day is no longer clipped to it, so "
        "a 6am gym class is counted as two hours of the day already booked",
        [("if a.After(b) {", "if a.Before(b) {")],
    ),
    Case(
        "an event that runs past the waking day is no longer clipped to it, so an "
        "evening that ends at 11:30pm eats an hour and a half of day-load that is "
        "not in the window at all",
        [("if a.Before(b) {\n\t\treturn a\n\t}\n\treturn b\n}\n\nfunc priorityMark",
          "if a.After(b) {\n\t\treturn a\n\t}\n\treturn b\n}\n\nfunc priorityMark")],
    ),
    Case(
        "the priority mark never fires, so an important overdue todo reads exactly "
        "like the rest of the list",
        [("if item.Priority >= 3 {", "if item.Priority >= 300 {")],
    ),
    Case(
        "the scope tag is printed on every line, so the one tag every item shares "
        "drowns the sub-tags that say what KIND of personal it is",
        [("if tag != scopeTag && tag != digestTag {", "if tag != digestTag {")],
    ),
    Case(
        "a bucket is rendered in noteboard's order rather than by due date, so the "
        "furthest-away item leads the section",
        [("return items[i].DueAt.Before(*items[j].DueAt)", "return items[i].DueAt.After(*items[j].DueAt)")],
    ),

    # ---- the calendar read and the link scheme ----
    Case(
        "today's calendar read swallows its own failure and reports an empty day, "
        "so a Google outage is indistinguishable from a free afternoon",
        [("return c.calendar.ListEvents(token, dayStart, dayEnd)", "events, _ := c.calendar.ListEvents(token, dayStart, dayEnd)\n\treturn events, nil")],
    ),
    Case(
        "the gcalsync stamp is read with its prefix still attached, so it never "
        "equals the due date and every tick rewrites an event that has not moved",
        [("syncedDueAt = strings.TrimPrefix(link, linkGoogleCalendarSyncedDueAt)", "syncedDueAt = link")],
    ),
    Case(
        "the gcalsync stamp is written without the due date it is a stamp OF, so "
        "the next tick reads it as absent and updates the event again forever",
        [("links = append(links, linkGoogleCalendarSyncedDueAt+dueAt.Format(time.RFC3339))",
          "links = append(links, linkGoogleCalendarSyncedDueAt)")],
    ),

    # ---- the command line: the defaults that ship, and the wiring under them ----
    #
    # Every row in this section was unscorable until parseFlags and newCoordinator
    # were lifted out of main(). Two of them used to sit in the declared section
    # below, each naming this seam as its own falsifier; the rest were never
    # enumerated at all, because the card that asked for the seam counted FLAGS
    # and the flags are only half of what main() was hiding. The other half is the
    # wiring: nine values reaching eight fields and a two-field struct, where
    # every same-typed pair swaps without a compile error.
    Case(
        "the default digest hour moves to 3am",
        [('fs.IntVar(&c.digestHour, "digest-hour", 7,', 'fs.IntVar(&c.digestHour, "digest-hour", 3,')],
    ),
    Case(
        "the default digest minute moves to the top of the hour",
        [('fs.IntVar(&c.digestMinute, "digest-minute", 30,', 'fs.IntVar(&c.digestMinute, "digest-minute", 0,')],
    ),
    Case(
        "the default noteboard port moves off 8191, so the source of truth is unreachable",
        [('"noteboard-url", envOr(lookupEnvironment, "NOTEBOARD_URL", "http://localhost:8191")',
          '"noteboard-url", envOr(lookupEnvironment, "NOTEBOARD_URL", "http://localhost:8192")')],
    ),
    Case(
        "the default dash port moves off 8101, so the calendar mirror talks to nothing",
        [('"dash-url", envOr(lookupEnvironment, "DASH_URL", "http://localhost:8101")',
          '"dash-url", envOr(lookupEnvironment, "DASH_URL", "http://localhost:8100")')],
    ),
    Case(
        "the default scope tag empties, so every open todo on the box is pulled into the personal digest",
        [('fs.StringVar(&c.scopeTag, "tag", "personal",', 'fs.StringVar(&c.scopeTag, "tag", "",')],
    ),
    Case(
        "the default timezone stops naming a loadable zone, so the binary log.Fatalf's on startup "
        "and reminds the user of nothing at all",
        [('"tz", envOr(lookupEnvironment, "REMINDER_TZ", "America/Los_Angeles")',
          '"tz", envOr(lookupEnvironment, "REMINDER_TZ", "America/Los_Angles")')],
    ),
    Case(
        "the default lookahead shrinks to a day, so a weekly template materializes nothing until "
        "the morning it is already due",
        [('fs.DurationVar(&c.lookahead, "lookahead", 7*24*time.Hour,',
          'fs.DurationVar(&c.lookahead, "lookahead", 24*time.Hour,')],
    ),
    Case(
        "dry-run defaults ON — the quietest failure available: every tick discovers the work, "
        "logs what it would do, writes nothing and reports success",
        [('fs.BoolVar(&c.dryRun, "dry-run", false,', 'fs.BoolVar(&c.dryRun, "dry-run", true,')],
    ),
    Case(
        "the herald binary's default path drops the bin/ directory, so the one nudge per tick "
        "fails to launch while the digest note still looks healthy",
        [('"ASK_BIN", filepath.Join(homeDirectory, "bin", "ask")',
          '"ASK_BIN", filepath.Join(homeDirectory, "ask")')],
    ),
    Case(
        "envOr prefers its fallback over the environment",
        [('if v := lookupEnvironment(key); v != "" {', 'if v := lookupEnvironment(key); v == "" {')],
    ),
    Case(
        "the digest hour and minute swap on the way to the coordinator, so 7:30 becomes 30:07",
        [("DigestAt:  dailyTime{Hour: c.digestHour, Minute: c.digestMinute},",
          "DigestAt:  dailyTime{Hour: c.digestMinute, Minute: c.digestHour},")],
    ),
    Case(
        "the two base URLs swap, so the coordinator asks noteboard for calendar events and dash for todos",
        [('noteboard: &NoteboardClient{BaseURL: strings.TrimRight(c.noteboardURL, "/")},\n\t\tcalendar:  &GoogleCalendarMirror{DashURL: strings.TrimRight(c.dashURL, "/")},',
          'noteboard: &NoteboardClient{BaseURL: strings.TrimRight(c.dashURL, "/")},\n\t\tcalendar:  &GoogleCalendarMirror{DashURL: strings.TrimRight(c.noteboardURL, "/")},')],
    ),
    Case(
        "the noteboard base URL keeps its trailing slash, so every request path doubles its first slash",
        [('&NoteboardClient{BaseURL: strings.TrimRight(c.noteboardURL, "/")}',
          '&NoteboardClient{BaseURL: c.noteboardURL}')],
    ),
    Case(
        "the dash base URL keeps its trailing slash",
        [('&GoogleCalendarMirror{DashURL: strings.TrimRight(c.dashURL, "/")}',
          '&GoogleCalendarMirror{DashURL: c.dashURL}')],
    ),
    Case(
        "the coordinator's clock is left in UTC instead of the digest zone, so the day rolls over "
        "at 5pm Pacific and every boundary in the tick moves with it",
        [("Now:       now.In(location),", "Now:       now,")],
    ),
    Case(
        "-dry-run never reaches the coordinator, so the flag that exists to mutate nothing mutates everything",
        [("DryRun:    c.dryRun,", "DryRun:    false,")],
    ),
    Case(
        "the lookahead never reaches the coordinator",
        [("Lookahead: c.lookahead,", "Lookahead: 0,")],
    ),
    Case(
        "the scope tag never reaches the coordinator",
        [("ScopeTag:  c.scopeTag,", 'ScopeTag:  "",')],
    ),
    Case(
        "the flag set goes back to ExitOnError, so a bad argument takes the calling process down "
        "instead of returning — which is what made this seam unusable from a test in the first place",
        [('flag.NewFlagSet("reminder-coordinator", flag.ContinueOnError)',
          'flag.NewFlagSet("reminder-coordinator", flag.ExitOnError)')],
    ),
    Case(
        "CONTROL known-negative: reword the -digest-hour usage string",
        [('"digest-hour", 7, "local hour to send the daily digest nudge."',
          '"digest-hour", 7, "hour of the day the digest nudge goes out."')],
        expected_unnoticed="the usage string reaches -h output and nothing else. This is the control "
                           "for the whole section above: it proves those rows redden on the VALUES "
                           "that ship and not on the source lines that carry them, which is the way "
                           "a defaults test most often lies.",
    ),

    # ---- declared unnoticeable: nothing in this suite executes these ----
    Case(
        "Board.HasActionable inverts",
        [("return len(b.Overdue) > 0 || len(b.DueToday) > 0", "return len(b.Overdue) > 0 && len(b.DueToday) > 0")],
        expected_unnoticed="HasActionable has NO caller — not in main.go, not in the suite. It is dead "
                           "code, so no mutation of it can be observed by anything. Falsified the "
                           "moment something calls it; `grep -rn HasActionable` finding a second site "
                           "is the check.",
    ),
    Case(
        "main maps -h to the failure exit code",
        [("if errors.Is(err, flag.ErrHelp) {\n\t\t\tos.Exit(0)", "if errors.Is(err, flag.ErrHelp) {\n\t\t\tos.Exit(2)")],
        expected_unnoticed="what main() still holds after the seam: the mapping from parseFlags's "
                           "error to an exit code, and its choice of os.Getenv and os.UserHomeDir as "
                           "the real arguments. Nothing in a test can call main, so this is the "
                           "residual the seam did NOT close — the tests pin that parseFlags RETURNS "
                           "flag.ErrHelp, not what main does with it. Falsified only by a test that "
                           "execs the built binary and reads its exit status.",
    ),
    Case(
        "the dry-run log prefix is reworded",
        [('return "[dry-run] "', 'return "[DRY RUN] "')],
        expected_unnoticed="the string only ever reaches log.Printf and the dry-run fmt.Println. No "
                           "test reads the log and nothing downstream parses it — the same standing "
                           "reason as the known-negative control. Falsified by a test that captures "
                           "log output, or by anything that greps journalctl for the prefix.",
    ),
    Case(
        "the whole Herald process launch is scrambled — the -from and -title flags swap",
        [('command := exec.Command(h.AskPath, "-from", from, "-title", title, message)',
          'command := exec.Command(h.AskPath, "-title", from, "-from", title, message)')],
        expected_unnoticed="the suite injects fakeHerald through the heraldRelay interface; the "
                           "concrete Herald never runs, so it cannot spawn `ask` wrongly in a test. "
                           "Falsified by a test that points AskPath at a script and reads its argv.",
    ),
    Case(
        "the ENTIRE concrete NoteboardClient is broken at once — every list path, "
        "every write verb, and the request builder under them",
        [
            ('"/api/items?type=todo&status=open&tag=%s&limit=500"', '"/api/items?type=todo&status=open&tag=%s&limit=5"'),
            ('for _, status := range []string{"open", "done", "archived"} {', 'for _, status := range []string{"open"} {'),
            ("\t\tif item.Schedule != nil {\n\t\t\tscheduled = append(scheduled, item)",
             "\t\tif item.Schedule == nil {\n\t\t\tscheduled = append(scheduled, item)"),
            ('return n.list("/api/items?parent_id=" + parentID + "&limit=500")',
             'return n.list("/api/items?parent_id=" + parentID + "&limit=1")'),
            ("return items[0], nil", "return items[len(items)-1], nil"),
            ('if err := n.do("POST", "/api/items", req, &created); err != nil {',
             'if err := n.do("PUT", "/api/items", req, &created); err != nil {'),
            ('return n.do("PATCH", "/api/items/"+id, fields, nil)', 'return n.do("POST", "/api/items/"+id, fields, nil)'),
            ('return n.Patch(id, map[string]any{"schedule": schedule})', 'return n.Patch(id, map[string]any{"schedules": schedule})'),
            ('if err := n.do("GET", path, nil, &items); err != nil {', 'if err := n.do("HEAD", path, nil, &items); err != nil {'),
            ("request, err := http.NewRequest(method, n.BaseURL+path, payload)",
             'request, err := http.NewRequest(method, n.BaseURL+path+"&broken=1", payload)'),
        ],
        expected_unnoticed="every one of these ten methods reaches the coordinator through the "
                           "noteboardAPI interface, and the suite passes fakeNoteboard. The concrete "
                           "client is never constructed in a test, so nothing here can go red — "
                           "including the SetSchedule key `schedules`, which noteboard would decode "
                           "non-strictly and answer 200 to. This is deliberately ONE case rather than "
                           "ten: the claim is about the whole client, and ten identical declarations "
                           "would read as ten separate measurements. Falsified by any test that builds "
                           "a NoteboardClient against an httptest server — which is exactly what card "
                           "d80db401 and branch test/concrete-clients-speak-the-real-protocol are for.",
    ),
    Case(
        "the ENTIRE concrete GoogleCalendarMirror is broken at once — token resolution, "
        "the list decoder, both write verbs, the payload and the authorization header",
        [
            ('if token, found := strings.CutPrefix(field, "DASH_TOKEN="); found {',
             'if token, found := strings.CutPrefix(field, "DASH_TOKENS="); found {'),
            ('if resp.Error != "" {', 'if resp.Error == "" {'),
            ('if created.ID == "" {', 'if created.ID != "" {'),
            ('return g.do(token, "PUT", "/api/calendar/google/"+eventID, eventPayload(item), nil)',
             'return g.do(token, "POST", "/api/calendar/google/"+eventID, eventPayload(item), nil)'),
            ('strings.Contains(err.Error(), "410")', 'strings.Contains(err.Error(), "400")'),
            ('"summary": "📋 " + item.Title,', '"summary": item.Title,'),
            ('request.Header.Set("Authorization", "Bearer "+token)', 'request.Header.Set("Authorization", "Bearer"+token)'),
        ],
        expected_unnoticed="same shape as the NoteboardClient case: the coordinator holds a "
                           "calendarMirror interface and the suite passes fakeCalendar, whose "
                           "ResolveDashToken returns a constant and whose ListEvents ignores its "
                           "window. The concrete mirror never runs, so a missing Bearer space and a "
                           "410 that is no longer treated as already-deleted are both invisible. "
                           "Falsified by a test that drives GoogleCalendarMirror at an httptest "
                           "server standing in for the dash proxy.",
    ),

    # ---- what the herald nudge says, and what it is allowed to leave out ----
    # The nudge used to be three counts. These pin the four things that make it
    # worth reading instead: it names the items, it dates them, it says what it
    # hid, and it says whether the day has room to act.
    Case(
        "the nudge lists the overdue bucket without its lateness, so 31 days late "
        "and 5 days late read alike",
        [("renderSection(&message, nudgeStyle, headingOverdue, board.Overdue, c.labelWithLateness)",
          "renderSection(&message, nudgeStyle, headingOverdue, board.Overdue, c.labelWithDueDate)")],
    ),
    Case(
        "a capped section hides the overflow silently, so a message showing 5 of 15 "
        "overdue items reads as the whole board",
        [('fmt.Fprintf(out, "%s…and %d more in the note\\n", style.Bullet, dropped)',
          "_ = dropped")],
    ),
    Case(
        "the cap goes away, so a bad week puts the entire overdue pile in a chat message",
        [("Bullet: \"  \", Limit: nudgeSectionLimit", "Bullet: \"  \", Limit: 0")],
    ),
    Case(
        "a section heading counts what it SHOWED rather than what the bucket holds, "
        "so a capped nudge says \"Overdue (5)\" of fifteen",
        [("fmt.Fprintf(out, style.HeadingFormat, heading, len(items))",
          "fmt.Fprintf(out, style.HeadingFormat, heading, len(shown))")],
    ),
    Case(
        "the note is capped like the nudge, so the record stops being total",
        [('digestStyle = sectionStyle{HeadingFormat: "\\n## %s (%d)\\n\\n", Bullet: "- "}',
          'digestStyle = sectionStyle{HeadingFormat: "\\n## %s (%d)\\n\\n", Bullet: "- ", Limit: nudgeSectionLimit}')],
    ),
    Case(
        "lateness truncates instead of rounding, so five calendar days across the "
        "spring-forward boundary are 119 hours and report as four",
        [("return int(math.Round(dateOf(c.Now.In(c.Location)).Sub(due).Hours() / 24))",
          "return int(dateOf(c.Now.In(c.Location)).Sub(due).Hours() / 24)")],
    ),
    Case(
        "a failed calendar read drops the nudge's schedule line, so a failure to load "
        "the day is indistinguishable from an empty one",
        [('out.WriteString("\\n🗓 Today: couldn\'t load your calendar.\\n")',
          'out.WriteString("\\n🗓 Today: nothing on the calendar.\\n")')],
    ),
    Case(
        "the day-load line reports the day's BOOKED hours where it says free, so the "
        "fuller the day looks the emptier it reads",
        [("return dayShape{Free: window - booked, BiggestOpen: biggest, Icon: icon, Verdict: verdict}",
          "return dayShape{Free: booked, BiggestOpen: biggest, Icon: icon, Verdict: verdict}")],
    ),

    # ---- the working day is not open time ----
    Case(
        "the working day stops being measured as busy, so eight hours the user is at "
        "work are offered back as room for a big todo",
        [("if work, ok := clip(c.WorkStart.OnDay(dayStart), c.WorkEnd.OnDay(dayStart)); ok {",
          "if work, ok := clip(c.WorkStart.OnDay(dayStart), c.WorkStart.OnDay(dayStart)); ok {")],
    ),
    Case(
        "the working day is measured but never named, so the digest quietly drops "
        "eight hours and the free figure reads as a bug",
        [('"%s Work %s; ~%s free outside it (to 10pm), biggest open stretch ~%s — %s."',
          '"%s%.0s ~%s free, biggest open stretch ~%s — %s."')],
    ),
    Case(
        "a working day that ends before it starts is accepted, measuring as no work "
        "at all and handing the whole day back as open time",
        [("if !workEnd.OnDay(referenceDay).After(workStart.OnDay(referenceDay)) {",
          "if false && !workEnd.OnDay(referenceDay).After(workStart.OnDay(referenceDay)) {")],
    ),
    Case(
        "an unparseable working day defaults to midnight instead of refusing, so the "
        "whole day reads as free",
        [("return dailyTime{}, fmt.Errorf(\"%q is not a 24-hour HH:MM time\", value)",
          "return dailyTime{}, nil")],
    ),
    Case(
        "a rejected working day is refused but never printed, so the job dies with "
        "exit 2 and no reason in its run history",
        [("fmt.Fprintln(fs.Output(), err)", "_ = err")],
    ),
    Case(
        "the working hours are charged on every day, so a Saturday reports 5.5 free "
        "hours and hides the one day the overdue pile is reachable",
        [("if c.isWorkDay(dayStart) {", "if true || c.isWorkDay(dayStart) {")],
    ),
    Case(
        "the day-load line claims a working day on a day off, the one sentence in "
        "the digest that is simply untrue",
        [("if !c.isWorkDay(c.Now) {", "if false && !c.isWorkDay(c.Now) {")],
    ),
    Case(
        "the weekend joins the shipped working week",
        [('fs.StringVar(&c.workDays, "work-days", "Mon,Tue,Wed,Thu,Fri",',
          'fs.StringVar(&c.workDays, "work-days", "Mon,Tue,Wed,Thu,Fri,Sat,Sun",')],
    ),
    Case(
        "an empty work-days list is read as no work days instead of refused, so "
        "every day reads as wide open",
        [('return nil, fmt.Errorf("no days given; the working hours would apply to nothing")',
          "return days, nil")],
    ),
    Case(
        "an unknown day name is skipped instead of refused, so a typo silently "
        "shortens the working week",
        [('return nil, fmt.Errorf("%q is not a day: use Mon, Tue, Wed, Thu, Fri, Sat or Sun", name)',
          "continue")],
    ),
    Case(
        "the shipped working day drifts off the hours the user actually keeps",
        [('fs.StringVar(&c.workEnd, "work-end", "16:30",', 'fs.StringVar(&c.workEnd, "work-end", "23:30",')],
    ),
    Case(
        "the nudge caps the day's schedule like a todo bucket, so a busy day hides "
        "the evening — the part the day-load line is about",
        [('nudgeScheduleStyle = sectionStyle{HeadingFormat: "\\n%s (%d)\\n", Bullet: "  "}',
          'nudgeScheduleStyle = sectionStyle{HeadingFormat: "\\n%s (%d)\\n", Bullet: "  ", Limit: nudgeSectionLimit}')],
    ),
    Case(
        "an event already inside work hours is charged again, so a normal working "
        "Tuesday reads as busier than a day off with the same meeting on it",
        [("busy = append(busy, clipped)", "busy = append(busy, clipped, clipped)"),
         ("if !v.start.After(last.end) {", "if false && !v.start.After(last.end) {")],
    ),
    Case(
        "a clear board renders no sections and says nothing about it, so the digest "
        "arrives looking like a message that failed to build",
        [("if len(board.Overdue)+len(board.DueToday)+len(board.ThisWeek) == 0 {",
          "if len(board.Overdue)+len(board.DueToday)+len(board.ThisWeek) < 0 {")],
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGET, PACKAGES, CASES))
