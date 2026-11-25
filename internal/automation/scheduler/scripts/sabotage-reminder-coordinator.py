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
    behavioral no-op, or a fake in the test file that swallows the value the
    mutation moved. Both appear below, named, with expected_unnoticed set.

Score: 132/135, exit 0 on 2026-10-05 — see the last dated section. Before
that: 122/132, exit 0 on 2026-09-26, after the undated-item sort fix and four
rows for sortByDue, dryPrefix and roundDuration's hour boundary (card
`526ea345`); the dry-run prefix row stopped being a declaration then. Before
that: 117/128, exit 0 on 2026-09-25, after six rows for the all-day window
and the never-nagged floor (a05c967). Before them: 111/122 — see the dated
section at the end of this docstring. Before that: 91/97, exit 0 — 91 real mechanisms caught and six cases carrying an
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

## A score and a census both ask about MECHANISMS, never about NUMBERS

Added 2026-08-15 (card `23be5012`, note `bd881397`). At 50/57 with both controls
behaving and zero silent functions, **eight of eleven boundary VALUES in this
binary could be moved to a different number with the whole suite green.** Five
were real holes: the two day-load verdict thresholds, the priority mark's rank,
the This Week horizon, and the mirrored event's length. Three were flag defaults,
declared unnoticed when this was written; on main parseFlags lets the suite read
them, and all three rows are caught.

The four rows this list already had that touch a literal are all FAR moves —
22h to 18h, 90m to 30m, `*2` to `*4`, `>=3` to `>=300`. A far move proves the
mechanism runs; it pins the boundary to a band. `TestDayLoadFlagsAFragmentedDay`
built a day whose biggest gap was 60 minutes and asserted red, which is true at
any threshold above 60 — so the 90 was held to "somewhere over an hour". The
adjacent-value rows at the bottom of CASES are the fix, and the fixtures behind
them straddle each cut by one minute, one rank or one day.

Worst of them, because the fixture already existed: `TestNoOpenTodoCanFallThrough
TheBuckets` carries an item due EXACTLY on the seven-day horizon — the very item
whose disappearance the bucket switch was repaired for — and asserts only that it
lands in *some* bucket. The horizon could slide a day either way and that item
just changed sections. **A fixture placed on a boundary is not a boundary test
until something asserts which side it fell.**

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
    from ResolveDashToken; `dryPrefix` only ever reaches a log line (expired
    2026-09-26: `unnamed_behavior_test.go` now asserts it directly). The other
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

---- 2026-09-25: thirteen dead rows re-aimed or deleted, two bundles split ----

Main moved this binary's defaults into internal/config's settings registry,
moved the -h exit code into run(), and rewrote the Herald argv, the relay
markers and the client paths, so 13 rows named code that no longer existed and
the engine aborted the whole file (card 2d1112ca). Twelve are re-aimed at main's
code; envOr's row is deleted, because the environment-versus-default choice now
lives in llm-bridge's servicesettings, outside the target.

The two "ENTIRE concrete client" rows went CAUGHT once re-aimed, because main's
noteboard_protocol_test.go and calendar_protocol_test.go now drive both clients
at httptest servers. That made their declarations stale and told us nothing per
edit, so each is split into one row per edit: 17 rows, 10 CAUGHT, and 7
UNNOTICED. The seven are real holes on main. The parked branch
test/concrete-clients-speak-the-real-protocol pins all seven in
client_protocol_test.go, so each carries a declaration that names that branch.
Porting it should turn those declarations STALE.

---- 2026-10-05: two rows re-aimed at the pager, three added, the port is off ----

8fcbdda replaced the fixed limit=500 reads with listEveryPage, so the ListOpen
and ListChildren rows named code that no longer existed and the file aborted
again. Both now pin what 8fcbdda fixed, a read that stops after one full page,
and three new rows pin the pager's bounds: the offset advances, a full page is
not the end, and the page bound is an error rather than a partial list. All
five are CAUGHT.

The parked branch was never ported. The scheduler C3 close-out (card 2fab79e5)
tagged it superseded/test/concrete-clients-speak-the-real-protocol on
2026-09-26, ported only its DASH_TOKEN fix (cd3920a), and judged the rest of
client_protocol_test.go covered by main's protocol tests. That judgment was
wrong for six of the seven rows: measured today, only the DASH_TOKENS row is
CAUGHT, and ListScheduled's filter, FindByTag's first match, SetSchedule's
key, UpdateEvent's verb, the 📋 prefix and the Bearer header were still
UNNOTICED (126/135). Six new tests in noteboard_protocol_test.go and
calendar_protocol_test.go now catch them, one each, and their declarations
are gone: 132/135. The three left are declared above: HasActionable, -h's
exit code and the Herald argv.
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
    # Two floors in this block. This row mutates the default taken when an item
    # has never nagged (ported from b92b34f1); the next one mutates `last != nil`.
    Case(
        "the never-nagged floor widens to a year, so a first nag replays every firing since the anchor",
        [("\t\tfrom := c.Now.Add(-24 * time.Hour)\n\t\tif last := item.Schedule.LastNaggedAt; last != nil {",
          "\t\tfrom := c.Now.AddDate(-1, 0, 0)\n\t\tif last := item.Schedule.LastNaggedAt; last != nil {")],
    ),
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
          '\tif len(board.NagFiring) > 0 {\n\t\t_, _ = c.herald.Relay("personal-reminders", "Still not done", "nag", "")\n'
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
        # Re-aimed 2026-09-25: the markers are rewritten in one pass now
        # (recordRelayMarkers), so the drift is the digest-sent branch inverting.
        [('if digestSent && strings.HasPrefix(link, "digest-sent:") {',
          'if digestSent && !strings.HasPrefix(link, "digest-sent:") {')],
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
        # Re-aimed 2026-09-25: main added a comment and a Degraded.Record under the
        # log line, so the needle is the log line alone.
        [('\t\tlog.Printf("reminder-coordinator: calendar read failed, digest will note it: %v", calErr)\n',
          '\t\treturn fmt.Errorf("calendar: %w", calErr)\n')],
    ),
    Case(
        "the digest note is found by a tag nothing wrote, so each tick creates a second note",
        [('digestTag   = "reminder-digest"', 'digestTag   = "reminder-digests"')],
    ),
    Case(
        "today's schedule swallows tomorrow's events",
        [("if !start.Before(dayEnd) {", "if !start.Before(dayEnd.AddDate(0, 0, 1)) {")],
    ),

    # ---- all-day calendar events: a civil-date window, EXCLUSIVE at the end ----
    # Ported from 20a3178b with the all-day window (3644d4c9). One case per test,
    # so the cross-table attributes them.
    Case(
        "all-day events are not bounded to today, so next month's conference is on today's schedule",
        [("if !civilDate(event.Start).After(today) && today.Before(allDayEnd(event)) {",
          "if !civilDate(event.Start).After(today.AddDate(0, 0, 1)) && today.Before(allDayEnd(event)) {")],
    ),
    Case(
        "the all-day end bound is read as INCLUSIVE, so a trip that ended last night is still on today",
        [("if !civilDate(event.Start).After(today) && today.Before(allDayEnd(event)) {",
          "if !civilDate(event.Start).After(today) && !today.After(allDayEnd(event)) {")],
    ),
    Case(
        "an all-day event with no end.date covers nothing, so it drops out of its own day",
        [("\t\treturn civilDate(event.Start).AddDate(0, 0, 1)",
          "\t\treturn civilDate(event.Start)")],
    ),
    # Mutating civilDate itself to time.Local would score whatever zone the box
    # is in. The call site carries the same claim and is deterministic: dayStart
    # is a midnight in c.Location, which every test sets.
    Case(
        "the all-day window compares a LOCAL midnight against UTC civil dates, so the bound drifts east of UTC",
        [("\ttoday := civilDate(dayStart)", "\ttoday := dayStart")],
    ),
    Case(
        "ListEvents converts Google's exclusive end.date to an inclusive one, so the two halves disagree by a day",
        [("\t\t\t\tevent.End = end\n\t\t\t}\n\t\tdefault:",
          "\t\t\t\tevent.End = end.AddDate(0, 0, -1)\n\t\t\t}\n\t\tdefault:")],
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
        # Re-aimed 2026-09-25: the default moved into internal/config's settings
        # registry, so the drift that can happen HERE is the flag not reading it.
        [('"noteboard-url", settings.String(schedulerconfig.SettingNoteboardURL),',
          '"noteboard-url", "http://localhost:8192",')],
    ),
    Case(
        "the default dash port moves off 8101, so the calendar mirror talks to nothing",
        # Re-aimed 2026-09-25: the default moved into internal/config's settings
        # registry, so the drift that can happen HERE is the flag not reading it.
        [('"dash-url", settings.String(schedulerconfig.SettingDashURL),',
          '"dash-url", "http://localhost:8100",')],
    ),
    Case(
        "the default scope tag empties, so every open todo on the box is pulled into the personal digest",
        [('fs.StringVar(&c.scopeTag, "tag", "personal",', 'fs.StringVar(&c.scopeTag, "tag", "",')],
    ),
    Case(
        "the default timezone stops naming a loadable zone, so the binary log.Fatalf's on startup "
        "and reminds the user of nothing at all",
        # Re-aimed 2026-09-25: the default moved into internal/config's settings
        # registry, so the drift that can happen HERE is the flag not reading it.
        [('"tz", settings.String(schedulerconfig.SettingTimezone),',
          '"tz", "America/Los_Angles",')],
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
        # Re-aimed 2026-09-25: the default moved into internal/config's settings
        # registry, so the drift that can happen HERE is the flag not reading it.
        [('"ask", settings.String(schedulerconfig.SettingAskPath),',
          '"ask", strings.TrimSuffix(settings.String(schedulerconfig.SettingAskPath), "bin/ask")+"ask",')],
    ),
    # Deleted 2026-09-25: "envOr prefers its fallback over the environment".
    # envOr is gone; which of the environment and the default wins is decided
    # by llm-bridge's servicesettings, outside the target.
    Case(
        "the digest hour and minute swap on the way to the coordinator, so 7:30 becomes 30:07",
        [("DigestAt:  dailyTime{Hour: c.digestHour, Minute: c.digestMinute},",
          "DigestAt:  dailyTime{Hour: c.digestMinute, Minute: c.digestHour},")],
    ),
    Case(
        "the two base URLs swap, so the coordinator asks noteboard for calendar events and dash for todos",
        # Re-aimed 2026-09-25: the mirror literal gained two fields, so the swap is
        # two edits, one per client.
        [('&NoteboardClient{BaseURL: strings.TrimRight(c.noteboardURL, "/")}',
          '&NoteboardClient{BaseURL: strings.TrimRight(c.dashURL, "/")}'),
         ('&GoogleCalendarMirror{DashURL: strings.TrimRight(c.dashURL, "/"),',
          '&GoogleCalendarMirror{DashURL: strings.TrimRight(c.noteboardURL, "/"),')],
    ),
    Case(
        "the noteboard base URL keeps its trailing slash, so every request path doubles its first slash",
        [('&NoteboardClient{BaseURL: strings.TrimRight(c.noteboardURL, "/")}',
          '&NoteboardClient{BaseURL: c.noteboardURL}')],
    ),
    Case(
        "the dash base URL keeps its trailing slash",
        [('&GoogleCalendarMirror{DashURL: strings.TrimRight(c.dashURL, "/"),',
          '&GoogleCalendarMirror{DashURL: c.dashURL,')],
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
        # Re-aimed 2026-09-25: main moved the exit-code mapping into run().
        [("if errors.Is(err, flag.ErrHelp) {\n\t\t\treturn 0", "if errors.Is(err, flag.ErrHelp) {\n\t\t\treturn 2")],
        expected_unnoticed="what main() still holds after the seam: the mapping from parseFlags's "
                           "error to an exit code, and its choice of os.Getenv and os.UserHomeDir as "
                           "the real arguments. Nothing in a test can call main, so this is the "
                           "residual the seam did NOT close — the tests pin that parseFlags RETURNS "
                           "flag.ErrHelp, not what main does with it. Falsified only by a test that "
                           "execs the built binary and reads its exit status.",
    ),
    Case(
        "the dry-run log prefix is reworded",
        # Was declared unnoticed ("no test reads the log"). The declaration
        # expired 2026-09-26 when TestDryPrefix_MarksARehearsalAndOnlyARehearsal
        # (unnamed_behavior_test.go, card `526ea345`) began asserting the
        # prefix itself; the scorer reported it STALE and it is a real row now.
        [('return "[dry-run] "', 'return "[DRY RUN] "')],
    ),
    Case(
        "the whole Herald process launch is scrambled — the -from and -title flags swap",
        # Re-aimed 2026-09-25: Relay builds its argv in steps now (-session or
        # -title), so the swap is two edits.
        [('arguments := []string{"-from", from}', 'arguments := []string{"-title", from}'),
         ('arguments = append(arguments, "-title", title)', 'arguments = append(arguments, "-from", title)')],
        expected_unnoticed="the suite injects fakeHerald through the heraldRelay interface; the "
                           "concrete Herald never runs, so it cannot spawn `ask` wrongly in a test. "
                           "Falsified by a test that points AskPath at a script and reads its argv.",
    ),
    # Split 2026-09-25: this was ONE case declared unnoticed because the concrete
    # noteboard client never ran in a test. Main's noteboard_protocol_test.go drives it at an
    # httptest server now, the bundle went CAUGHT and its declaration went stale,
    # and a bundle that goes red says nothing about each of its edits, so each
    # edit is its own row.
    # Re-aimed 2026-10-05: 8fcbdda replaced the three fixed limit=500 reads with
    # listEveryPage, so the old needle was gone and the file aborted here. The
    # defect worth pinning now is the one 8fcbdda fixed: a read that stops at
    # one full page and calls it the whole board.
    Case(
        'NoteboardClient: ListOpen reads one page and stops, so a board past 500 open todos is silently cut',
        [('return n.listEveryPage(url.Values{"type": {"todo"}, "status": {"open"}, "tag": {tag}})',
          'return n.list("/api/items?" + url.Values{"type": {"todo"}, "status": {"open"}, "tag": {tag}, "limit": {"500"}}.Encode())')],
    ),
    Case(
        'NoteboardClient: ListAllStatuses reads only open items, so done and archived ones vanish',
        [('for _, status := range []string{"open", "done", "archived"} {',
          'for _, status := range []string{"open"} {')],
    ),
    Case(
        'NoteboardClient: ListScheduled keeps the items WITHOUT a schedule',
        [('\t\tif item.Schedule != nil {\n\t\t\tscheduled = append(scheduled, item)',
          '\t\tif item.Schedule == nil {\n\t\t\tscheduled = append(scheduled, item)')],
    ),
    # Re-aimed 2026-10-05, as ListOpen above.
    Case(
        "NoteboardClient: ListChildren reads one page and stops, so a template's 501st occurrence is invisible",
        [('return n.listEveryPage(url.Values{"parent_id": {parentID}})',
          'return n.list("/api/items?" + url.Values{"parent_id": {parentID}, "limit": {"500"}}.Encode())')],
    ),
    # Added 2026-10-05: listEveryPage is the pager 8fcbdda put under ListOpen,
    # ListAllStatuses and ListChildren, and these are its three bounds.
    Case(
        'NoteboardClient: the pager never advances its offset, so every page re-reads the first',
        [('pageQuery.Set("offset", strconv.Itoa(page*noteboardPageSize))',
          'pageQuery.Set("offset", strconv.Itoa(0*noteboardPageSize))')],
    ),
    Case(
        'NoteboardClient: the pager stops on a full page instead of a short one, so it reads one page and returns',
        [('if len(batch) < noteboardPageSize {',
          'if len(batch) <= noteboardPageSize {')],
    ),
    Case(
        'NoteboardClient: the pager hands back a partial list as the whole board once it hits the page bound',
        [('if len(batch) < noteboardPageSize {',
          'if len(batch) < noteboardPageSize || page == noteboardMaxPages-1 {')],
    ),
    Case(
        'NoteboardClient: FindByTag returns the last match instead of the first',
        [('return items[0], nil',
          'return items[len(items)-1], nil')],
    ),
    Case(
        'NoteboardClient: CreateItem sends PUT, not POST',
        [('if err := n.do("POST", "/api/items", req, &created); err != nil {',
          'if err := n.do("PUT", "/api/items", req, &created); err != nil {')],
    ),
    Case(
        'NoteboardClient: Patch sends POST, not PATCH',
        [('return n.do("PATCH", itemPath(id), fields, nil)',
          'return n.do("POST", itemPath(id), fields, nil)')],
    ),
    Case(
        'NoteboardClient: SetSchedule writes the key `schedules`, which noteboard ignores and answers 200 to',
        [('return n.Patch(id, map[string]any{"schedule": schedule})',
          'return n.Patch(id, map[string]any{"schedules": schedule})')],
    ),
    Case(
        'NoteboardClient: list sends HEAD, not GET',
        [('if err := n.do("GET", path, nil, &items); err != nil {',
          'if err := n.do("HEAD", path, nil, &items); err != nil {')],
    ),
    Case(
        'NoteboardClient: the request builder appends a stray query parameter to every path',
        [('request, err := http.NewRequest(method, n.BaseURL+path, payload)',
          'request, err := http.NewRequest(method, n.BaseURL+path+"&broken=1", payload)')],
    ),
    # Split 2026-09-25: this was ONE case declared unnoticed because the concrete
    # calendar client never ran in a test. Main's calendar_protocol_test.go drives it at an
    # httptest server now, the bundle went CAUGHT and its declaration went stale,
    # and a bundle that goes red says nothing about each of its edits, so each
    # edit is its own row.
    Case(
        'GoogleCalendarMirror: token resolution looks for DASH_TOKENS=, so it never finds the token',
        # Caught since cd3920a ported dash_token_test.go; its declaration went
        # STALE on 2026-10-05 and is gone.
        [('if token, found := strings.CutPrefix(field, "DASH_TOKEN="); found {',
          'if token, found := strings.CutPrefix(field, "DASH_TOKENS="); found {')],
    ),
    Case(
        'GoogleCalendarMirror: the list decoder treats an error response as success and success as an error',
        [('if resp.Error != "" {',
          'if resp.Error == "" {')],
    ),
    Case(
        'GoogleCalendarMirror: CreateEvent refuses every event that came back WITH an id',
        [('if created.ID == "" {',
          'if created.ID != "" {')],
    ),
    Case(
        'GoogleCalendarMirror: UpdateEvent sends POST, not PUT',
        [('return g.do(token, "PUT", calendarEventPath(eventID), eventPayload(item), nil)',
          'return g.do(token, "POST", calendarEventPath(eventID), eventPayload(item), nil)')],
    ),
    Case(
        'GoogleCalendarMirror: DeleteEvent stops treating 410 Gone as already deleted',
        [('httpErr.StatusCode == http.StatusGone ||',
          'httpErr.StatusCode == http.StatusBadRequest ||')],
    ),
    Case(
        "GoogleCalendarMirror: the event payload loses its 📋 prefix, so mirrored todos read like the user's own events",
        [('"summary": "📋 " + item.Title,',
          '"summary": item.Title,')],
    ),
    Case(
        'GoogleCalendarMirror: the authorization header loses the space after Bearer',
        [('request.Header.Set("Authorization", "Bearer "+token)',
          'request.Header.Set("Authorization", "Bearer"+token)')],
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
        "an empty day in the work-days list is skipped instead of refused, so a "
        "blank value or a trailing comma reads as no work days and every day "
        "reads as wide open",
        [('return nil, fmt.Errorf("%q has an empty day in it", value)', "continue")],
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

    # ---- boundary VALUES: the number, not the mechanism (card 23be5012) ----
    #
    # Everything above this line moves a DIRECTION or deletes a MECHANISM. Four
    # rows touch a literal — windowEnd 22h->18h, the fragmented cut 90m->30m,
    # roundDuration's *2->*4, priorityMark's >=3->>=300 — and every one of them
    # is a FAR move, which pins the boundary to a band and not to a value. The
    # last of those does not even leave the mechanism running: nothing has
    # priority 300, so it is a deletion wearing a number.
    #
    # Probed 2026-08-15 with one adjacent-value drift per literal, against this
    # list at 50/57 with both controls behaving: 3 of 11 CAUGHT. The rows below
    # are that probe, kept. Five were real holes and are now pinned by
    # straddling fixtures in main_test.go; each pair of fixtures differs by one
    # minute, one rank or one day, so a threshold that moves in EITHER direction
    # reddens a row.
    Case(
        "VALUE the waking day starts an hour later, so an 8am event stops counting",
        [("windowStart := dayStart.Add(8 * time.Hour)", "windowStart := dayStart.Add(9 * time.Hour)")],
    ),
    Case(
        "VALUE the waking day ends an hour earlier — the adjacent move, where the "
        "22h->18h case above only proves the window is somewhere below 10pm",
        [("windowEnd := dayStart.Add(22 * time.Hour)", "windowEnd := dayStart.Add(21 * time.Hour)")],
    ),
    Case(
        "VALUE the red/amber fragmented-day cut rises 90m->120m, so a day whose "
        "biggest gap is under two hours is still called workable",
        [("case biggest < 90*time.Minute:", "case biggest < 120*time.Minute:")],
    ),
    Case(
        "VALUE the amber/green cut rises 3h->4h — no fixture exercised this band "
        "at all until the straddle went in",
        [("case biggest < 3*time.Hour:", "case biggest < 4*time.Hour:")],
    ),
    Case(
        "VALUE the priority mark drops a rank (>=3 -> >=2), so every p2 todo is "
        "flagged and the mark stops distinguishing anything",
        [("if item.Priority >= 3 {", "if item.Priority >= 2 {")],
    ),
    Case(
        "VALUE the This Week horizon widens a day (7 -> 8), quietly moving the "
        "seventh-day item out of Later and into This Week",
        [("weekEnd := todayDate.AddDate(0, 0, 7)", "weekEnd := todayDate.AddDate(0, 0, 8)")],
    ),
    Case(
        "VALUE a mirrored calendar event runs 45 minutes instead of 30, blocking "
        "time on the real calendar that the todo never asked for",
        [("calendarEventMinutes = 30", "calendarEventMinutes = 45")],
    ),
    Case(
        "VALUE durations render as minutes up to two hours, so 90 free "
        "minutes report as \"90m\" in a line that talks in hours",
        [("\tif d < time.Hour {\n\t\treturn fmt.Sprintf(\"%dm\"",
          "\tif d < 2*time.Hour {\n\t\treturn fmt.Sprintf(\"%dm\"")],
    ),
    Case(
        "VALUE the digest hour default drifts 7 -> 8",
        [('fs.IntVar(&c.digestHour, "digest-hour", 7,', 'fs.IntVar(&c.digestHour, "digest-hour", 8,')],
    ),
    Case(
        "VALUE the digest minute default drifts 30 -> 31",
        [('fs.IntVar(&c.digestMinute, "digest-minute", 30,', 'fs.IntVar(&c.digestMinute, "digest-minute", 31,')],
    ),
    Case(
        "VALUE the materialization lookahead default doubles 7d -> 14d",
        [('fs.DurationVar(&c.lookahead, "lookahead", 7*24*time.Hour,',
          'fs.DurationVar(&c.lookahead, "lookahead", 14*24*time.Hour,')],
    ),
    # ---- the three helpers every reminder surface is rendered through ----
    # Written 2026-08-30 on test/the-coordinator-silent-column-is-closed and
    # ported 2026-09-26 (card `526ea345`), closing the silent column
    # `--crosstable` found there: all four tests in unnamed_behavior_test.go
    # were reddened by NO case. This list is drafted from worries about the digest
    # DECIDING WRONGLY — what is due, what nags, what reaches the calendar — and
    # has no vocabulary for the helpers that decide how the answer is ORDERED,
    # MARKED and MEASURED. Card `6e66450b` item 1.
    #
    # One case per test, aimed at a mechanism only that test holds: a single
    # mutation to sortByDue would redden both of its tests and leave neither
    # separately pinned (card `2920becb`).
    Case(
        "undated items sort to the FRONT, so the digest opens on items with no date",
        [("return items[i].DueAt != nil && items[j].DueAt == nil",
          "return items[i].DueAt == nil && items[j].DueAt != nil")],
    ),
    Case(
        "the sort invents an order for undated items instead of leaving them alone",
        [("""\t\tif items[i].DueAt == nil || items[j].DueAt == nil {
\t\t\treturn items[i].DueAt != nil && items[j].DueAt == nil
\t\t}""",
          """\t\tif items[i].DueAt == nil || items[j].DueAt == nil {
\t\t\tif items[i].DueAt == nil && items[j].DueAt == nil {
\t\t\t\treturn items[i].Title > items[j].Title
\t\t\t}
\t\t\treturn items[i].DueAt != nil && items[j].DueAt == nil
\t\t}""")],
    ),
    Case(
        "a real run is marked as a rehearsal and a rehearsal is not",
        [('''func dryPrefix(dryRun bool) string {
\tif dryRun {''',
          '''func dryPrefix(dryRun bool) string {
\tif !dryRun {''')],
    ),
    Case(
        "the hour boundary is inclusive, so exactly one hour overdue reports as 60m",
        [("\tif d < time.Hour {", "\tif d <= time.Hour {")],
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGET, PACKAGES, CASES))
