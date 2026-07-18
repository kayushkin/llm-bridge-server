// reminder-coordinator is the ONLY thing that reminds the user of anything.
//
// It replaces five scheduler jobs that each surfaced personal todos while
// knowing nothing about each other:
//
//	personal-todo-calsync  (*/15)      mirror dated todos to Google Calendar
//	personal-todo-digest   (7:30am)    herald nudge listing overdue + due today
//	car-title-nudge        (weekdays)  herald nudge for ONE todo
//	weekly-jobs-nudge      (Mon)       herald nudge + a web search
//	ai-club-weekly-nudge   (Sun)       herald nudge
//
// That design double-nudged by construction. The car-title todo was overdue, so
// the daily digest listed it AND its dedicated weekday job nudged it — two herald
// sessions for one task, every weekday, because no job could see what another had
// already said. Cadence lived in cron lines and hardcoded shell scripts rather
// than on the todo, so there was nowhere for that knowledge to live.
//
// Now cadence lives on the item (noteboard's `schedule` field, RFC 5545), and
// exactly one process reads it. Nothing CAN double-nudge, because there is only
// one nudger.
//
// Each tick:
//
//	1. materialize  due occurrences of recurring templates into real todos
//	2. roll         completed rolling-mode items forward to their next occurrence
//	3. digest       rewrite the standing "Today" note — the always-available board
//	4. calendar     mirror opted-in items to Google Calendar (secondary, opt-in)
//	5. nudge        at most ONE herald session, and only when something new fired
//
// The digest note is the primary surface and is rewritten every tick, so it is
// always current and always in one place. Chat is the exception channel, not the
// record. The calendar is opt-in per item (remind.channels), because a calendar
// full of todos stops being a calendar.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/noteboard/model"
)

const (
	// digestTag marks the standing digest note. It is found by tag rather than by
	// a remembered id so the coordinator has no state of its own to lose: delete
	// the note and the next tick rebuilds it.
	digestTag   = "reminder-digest"
	digestTitle = "📆 Today — personal"

	// linkGoogleCalendarEvent and linkGoogleCalendarSyncedDueAt keep the calendar
	// mirror idempotent, and are the same link scheme personal-todo-calsync used —
	// so any event it created is still recognised and adopted rather than
	// abandoned and duplicated.
	linkGoogleCalendarEvent       = "gcal:"
	linkGoogleCalendarSyncedDueAt = "gcalsync:"

	calendarEventMinutes = 30
)

func main() {
	var (
		noteboardURL = flag.String("noteboard-url", envOr("NOTEBOARD_URL", "http://localhost:8191"), "noteboard base URL — the source of truth for todos and their rules.")
		dashURL      = flag.String("dash-url", envOr("DASH_URL", "http://localhost:8101"), "dash base URL, for the authenticated Google Calendar proxy.")
		askPath      = flag.String("ask", envOr("ASK_BIN", os.ExpandEnv("$HOME/bin/ask")), "path to the herald `ask` binary.")
		scopeTag     = flag.String("tag", "personal", "only items carrying this tag are coordinated. Keeps the ~40 bot/coding todos out of the digest.")
		digestHour   = flag.Int("digest-hour", 7, "local hour to send the daily digest nudge.")
		digestMinute = flag.Int("digest-minute", 30, "local minute to send the daily digest nudge.")
		timezone     = flag.String("tz", envOr("REMINDER_TZ", "America/Los_Angeles"), "IANA zone the daily digest hour is interpreted in.")
		lookahead    = flag.Duration("lookahead", 7*24*time.Hour, "how far ahead to materialize occurrences of a recurring template.")
		dryRun       = flag.Bool("dry-run", false, "report what would change; mutate nothing, send nothing.")
	)
	flag.Parse()

	location, err := time.LoadLocation(*timezone)
	if err != nil {
		log.Fatalf("reminder-coordinator: bad -tz %q: %v", *timezone, err)
	}

	coordinator := &Coordinator{
		noteboard: &NoteboardClient{BaseURL: strings.TrimRight(*noteboardURL, "/")},
		calendar:  &GoogleCalendarMirror{DashURL: strings.TrimRight(*dashURL, "/")},
		herald:    &Herald{AskPath: *askPath},
		ScopeTag:  *scopeTag,
		Location:  location,
		DigestAt:  dailyTime{Hour: *digestHour, Minute: *digestMinute},
		Lookahead: *lookahead,
		DryRun:    *dryRun,
		Now:       time.Now().In(location),
	}

	if err := coordinator.Run(); err != nil {
		// Loud. A reminder system that fails quietly is indistinguishable from one
		// with nothing to say, and you find out weeks later by missing something.
		log.Fatalf("reminder-coordinator: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type dailyTime struct{ Hour, Minute int }

// OnDay is the instant this daily time falls on a given day, in that day's zone.
func (d dailyTime) OnDay(day time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), d.Hour, d.Minute, 0, 0, day.Location())
}

// The collaborators are interfaces so the coordinator's decisions can be tested
// without a noteboard, a dash, or a live herald session. The decisions are the
// whole point: whether a child inherits its parent's tags, whether a nag fires
// twice, whether two sessions can open at once. Those are exactly the things that
// are invisible in production until they have already gone wrong.
type noteboardAPI interface {
	ListOpen(tag string) ([]*model.Item, error)
	ListAllStatuses(tag string) ([]*model.Item, error)
	ListScheduled(tag string) ([]*model.Item, error)
	ListChildren(parentID string) ([]*model.Item, error)
	FindByTag(itemType, tag string) (*model.Item, error)
	CreateItem(req *model.CreateItemRequest) (*model.Item, error)
	Patch(id string, fields map[string]any) error
	SetSchedule(id string, schedule *model.Schedule, dryRun bool) error
}

type calendarMirror interface {
	ResolveDashToken() (string, error)
	ListEvents(token string, timeMin, timeMax time.Time) ([]CalendarEvent, error)
	CreateEvent(token string, item *model.Item) (string, error)
	UpdateEvent(token, eventID string, item *model.Item) error
	DeleteEvent(token, eventID string) error
}

type heraldRelay interface {
	Relay(from, title, message string) error
}

type Coordinator struct {
	noteboard noteboardAPI
	calendar  calendarMirror
	herald    heraldRelay

	ScopeTag  string
	Location  *time.Location
	DigestAt  dailyTime
	Lookahead time.Duration
	DryRun    bool
	Now       time.Time
}

func (c *Coordinator) Run() error {
	if err := c.materializeDueOccurrences(); err != nil {
		return fmt.Errorf("materialize: %w", err)
	}
	if err := c.rollCompletedRollingItems(); err != nil {
		return fmt.Errorf("roll: %w", err)
	}

	board, err := c.buildBoard()
	if err != nil {
		return fmt.Errorf("build board: %w", err)
	}

	// Today's calendar is a secondary READ folded into the digest so you can see
	// whether the day has room for a long task. A failure here is logged loudly and
	// noted in the digest, but must NOT take down the primary board: the reminders
	// matter more than the schedule annotation, and a Google hiccup should never
	// leave you with no digest at all.
	events, calErr := c.todaysCalendar()
	if calErr != nil {
		log.Printf("reminder-coordinator: calendar read failed, digest will note it: %v", calErr)
	}

	if err := c.writeDigestNote(board, events, calErr); err != nil {
		return fmt.Errorf("write digest note: %w", err)
	}
	if err := c.syncCalendar(); err != nil {
		return fmt.Errorf("sync calendar: %w", err)
	}
	if err := c.nudgeOnce(board); err != nil {
		return fmt.Errorf("nudge: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- materialize

// materializeDueOccurrences turns each occurrence of an instance-mode template
// into a real, ordinary todo.
//
// This is what keeps every consumer downstream dumb. The digest, the calendar
// mirror and the nudge all just read open todos; none of them knows what an RRULE
// is. Recurrence is resolved here, once.
//
// Idempotence matters more than it looks: this runs every 15 minutes, so a
// template whose occurrence has passed would spawn a duplicate child on every
// single tick — 96 copies of "water the plants" a day. Two independent guards:
// schedule.last_materialized_at (advanced past every occurrence acted on), and a
// scan of the template's existing children for one already carrying that due
// date, which also covers a child created before this field existed.
func (c *Coordinator) materializeDueOccurrences() error {
	templates, err := c.noteboard.ListScheduled(c.ScopeTag)
	if err != nil {
		return err
	}

	horizon := c.Now.Add(c.Lookahead)
	for _, template := range templates {
		if template.Schedule == nil || !template.Schedule.HasDueRecurrence() {
			continue
		}
		if template.Schedule.EffectiveMode() != model.ScheduleModeInstance {
			continue
		}
		// An archived or deleted template stops producing. `done` does NOT stop it:
		// a template is not the work, and marking one occurrence done must not
		// retire the series.
		if template.Status == "archived" {
			continue
		}

		// Start from the last occurrence acted on, so a long-idle coordinator does
		// not backfill months of missed chores in one go — but never before the
		// template's own anchor.
		from := c.Now.Add(-24 * time.Hour)
		if last := template.Schedule.LastMaterializedAt; last != nil {
			from = last.Add(time.Second)
		}

		occurrences, err := template.Schedule.DueOccurrences(template.DueAt, from, horizon)
		if err != nil {
			return fmt.Errorf("template %s (%q): %w", template.ID, template.Title, err)
		}
		if len(occurrences) == 0 {
			continue
		}

		existing, err := c.noteboard.ListChildren(template.ID)
		if err != nil {
			return fmt.Errorf("children of %s: %w", template.ID, err)
		}

		latest := template.Schedule.LastMaterializedAt
		for _, occurrence := range occurrences {
			if hasChildDueAt(existing, occurrence) {
				continue
			}
			if err := c.materializeOccurrence(template, occurrence); err != nil {
				return err
			}
			if latest == nil || occurrence.After(*latest) {
				stamp := occurrence
				latest = &stamp
			}
		}

		if latest != nil && (template.Schedule.LastMaterializedAt == nil || latest.After(*template.Schedule.LastMaterializedAt)) {
			advanced := *template.Schedule
			advanced.LastMaterializedAt = latest
			if err := c.noteboard.SetSchedule(template.ID, &advanced, c.DryRun); err != nil {
				return fmt.Errorf("advance last_materialized_at on %s: %w", template.ID, err)
			}
		}
	}
	return nil
}

func (c *Coordinator) materializeOccurrence(template *model.Item, occurrence time.Time) error {
	log.Printf("%smaterialize: %q due %s", dryPrefix(c.DryRun), template.Title, occurrence.Format(time.RFC3339))
	if c.DryRun {
		return nil
	}

	// Tags are INHERITED, and that is a safety property, not a nicety. The
	// autoworker picks up any open todo that lacks the `personal` and `autoworker`
	// tags and dispatches an agent at it. An untagged child of "water the plants"
	// would therefore land in the coding queue and get handed to a Claude session.
	// The child must carry everything that made the parent personal.
	tags := append([]string(nil), template.Tags...)

	occurrenceDue := occurrence
	child := &model.CreateItemRequest{
		Type:     model.TypeTodo,
		Title:    template.Title,
		Body:     &template.Body,
		Tags:     tags,
		Priority: &template.Priority,
		DueAt:    &occurrenceDue,
		ParentID: &template.ID,
		// Deliberately NO schedule: the child is one occurrence, not a series. A
		// child that inherited the rule would itself start spawning children.
	}
	if _, err := c.noteboard.CreateItem(child); err != nil {
		return fmt.Errorf("create occurrence of %s: %w", template.ID, err)
	}
	return nil
}

func hasChildDueAt(children []*model.Item, occurrence time.Time) bool {
	for _, child := range children {
		if child.DueAt != nil && child.DueAt.Equal(occurrence) {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ roll

// rollCompletedRollingItems advances a rolling-mode item to its next occurrence
// once it is done: one row, no children, no completion history. Right for a task
// where last year's instance is of no interest ("renew the registration").
//
// When the series runs out — an RRULE with UNTIL or COUNT eventually does — the
// item is left done. That is how a rolling item retires itself rather than
// reopening forever.
func (c *Coordinator) rollCompletedRollingItems() error {
	items, err := c.noteboard.ListScheduled(c.ScopeTag)
	if err != nil {
		return err
	}

	for _, item := range items {
		if item.Schedule == nil || !item.Schedule.HasDueRecurrence() {
			continue
		}
		if item.Schedule.EffectiveMode() != model.ScheduleModeRolling || item.Status != "done" {
			continue
		}

		after := c.Now
		if item.DueAt != nil && item.DueAt.After(after) {
			after = *item.DueAt
		}
		next, err := item.Schedule.NextDueOccurrence(item.DueAt, after)
		if err != nil {
			return fmt.Errorf("item %s (%q): %w", item.ID, item.Title, err)
		}
		if next == nil {
			log.Printf("rolling series exhausted, leaving done: %q", item.Title)
			continue
		}

		log.Printf("%sroll: %q -> %s", dryPrefix(c.DryRun), item.Title, next.Format(time.RFC3339))
		if c.DryRun {
			continue
		}
		if err := c.noteboard.Patch(item.ID, map[string]any{
			"status": "open",
			"due_at": next.Format(time.RFC3339),
		}); err != nil {
			return fmt.Errorf("roll %s: %w", item.ID, err)
		}
	}
	return nil
}

// ------------------------------------------------------------------ board

// Board is what is actionable right now, bucketed. Everything downstream — the
// note, the nudge — renders from this one structure, so the note and the nudge
// can never disagree about what is due.
type Board struct {
	Overdue  []*model.Item
	DueToday []*model.Item
	ThisWeek []*model.Item
	// Later catches everything dated beyond this week. It exists because the
	// buckets must be TOTAL: an open todo that lands in no bucket vanishes from
	// the board entirely, and a board that omits a todo you still have is worse
	// than no board — it reads as "this is everything" while being a lie. That
	// bug shipped and was caught only because a real item happened to sit exactly
	// one day past the horizon.
	Later     []*model.Item
	Undated   []*model.Item
	NagFiring []*model.Item // unfinished items whose nag rule fired since it last did
}

// All returns every item on the board. Used to assert the buckets are total.
func (b *Board) All() []*model.Item {
	all := []*model.Item{}
	all = append(all, b.Overdue...)
	all = append(all, b.DueToday...)
	all = append(all, b.ThisWeek...)
	all = append(all, b.Later...)
	all = append(all, b.Undated...)
	return all
}

func (b *Board) HasActionable() bool { return len(b.Overdue) > 0 || len(b.DueToday) > 0 }

func (c *Coordinator) buildBoard() (*Board, error) {
	items, err := c.noteboard.ListOpen(c.ScopeTag)
	if err != nil {
		return nil, err
	}

	today := c.Now.In(c.Location)
	todayDate := dateOf(today)
	weekEnd := todayDate.AddDate(0, 0, 7)

	board := &Board{}
	for _, item := range items {
		// A recurring TEMPLATE is not work — its occurrences are. Listing the
		// template would show "water the plants" as permanently due alongside this
		// week's actual instance.
		if isTemplate(item) {
			continue
		}

		if item.DueAt == nil {
			board.Undated = append(board.Undated, item)
			continue
		}
		due := dateOf(item.DueAt.In(c.Location))
		switch {
		case due.Before(todayDate):
			board.Overdue = append(board.Overdue, item)
		case due.Equal(todayDate):
			board.DueToday = append(board.DueToday, item)
		case due.Before(weekEnd):
			board.ThisWeek = append(board.ThisWeek, item)
		default:
			// Every remaining dated item. No item may fall through this switch.
			board.Later = append(board.Later, item)
		}
	}

	sortByDue(board.Overdue)
	sortByDue(board.DueToday)
	sortByDue(board.ThisWeek)
	sortByDue(board.Later)

	nagging, err := c.nagsFiringNow()
	if err != nil {
		return nil, err
	}
	board.NagFiring = nagging
	return board, nil
}

// isTemplate reports whether an item is a recurring series rather than a piece of
// work. Rolling-mode items ARE the work (they have no children), so only
// instance-mode series with a due recurrence are templates.
func isTemplate(item *model.Item) bool {
	return item.Schedule != nil &&
		item.Schedule.HasDueRecurrence() &&
		item.Schedule.EffectiveMode() == model.ScheduleModeInstance
}

// nagsFiringNow returns the unfinished items whose nag rule has fired since the
// last time it fired for them — the "keep bothering me until it's done" set.
//
// last_nagged_at is what stops a */15 tick from re-nagging 96 times a day about
// the same 10am firing.
func (c *Coordinator) nagsFiringNow() ([]*model.Item, error) {
	items, err := c.noteboard.ListScheduled(c.ScopeTag)
	if err != nil {
		return nil, err
	}

	firing := []*model.Item{}
	for _, item := range items {
		if item.Schedule == nil || item.Schedule.Remind == nil || item.Schedule.Remind.Nag == "" {
			continue
		}
		// A nag is "until it's done". Done means done.
		if item.Status != "open" {
			continue
		}
		if !item.Schedule.Remind.DeliversOn(model.ChannelHerald) && !item.Schedule.Remind.DeliversOn(model.ChannelDigest) {
			continue
		}

		// Only look back as far as the last nag. Without a floor, a nag rule with no
		// last_nagged_at would expand its whole history and fire for every weekday
		// since its anchor.
		from := c.Now.Add(-24 * time.Hour)
		if last := item.Schedule.LastNaggedAt; last != nil {
			from = last.Add(time.Second)
		}

		occurrences, err := item.Schedule.NagOccurrences(item.DueAt, from, c.Now)
		if err != nil {
			return nil, fmt.Errorf("nag rule on %s (%q): %w", item.ID, item.Title, err)
		}
		if len(occurrences) == 0 {
			continue
		}
		firing = append(firing, item)
	}
	return firing, nil
}

// ------------------------------------------------------------------ digest note

// writeDigestNote rewrites the standing "Today" note in place.
//
// This is the primary surface: one note, always current, always in the same
// place, readable at dash /notes whenever you want it — rather than a chat
// session that scrolls away and has to be re-sent to be seen again.
//
// It is overwritten rather than appended. The todos are the durable record; a log
// of past digests is a log of what was already true.
func (c *Coordinator) writeDigestNote(board *Board, events []CalendarEvent, calErr error) error {
	body := c.renderDigest(board, events, calErr)

	existing, err := c.noteboard.FindByTag(model.TypeNote, digestTag)
	if err != nil {
		return err
	}

	if existing == nil {
		log.Printf("%sdigest note: creating", dryPrefix(c.DryRun))
		if c.DryRun {
			// The rendered board IS the output of a dry run. Reporting only that a
			// note "would be written", without showing what it would say, hides the
			// one thing worth checking.
			fmt.Println(body)
			return nil
		}
		_, err := c.noteboard.CreateItem(&model.CreateItemRequest{
			Type:  model.TypeNote,
			Title: digestTitle,
			Body:  &body,
			Tags:  []string{c.ScopeTag, digestTag},
		})
		return err
	}

	if existing.Body == body {
		return nil // nothing changed; don't churn a revision
	}
	log.Printf("%sdigest note: updating", dryPrefix(c.DryRun))
	if c.DryRun {
		fmt.Println(body)
		return nil
	}
	return c.noteboard.Patch(existing.ID, map[string]any{"body": body})
}

func (c *Coordinator) renderDigest(board *Board, events []CalendarEvent, calErr error) string {
	var out bytes.Buffer
	now := c.Now.In(c.Location)

	fmt.Fprintf(&out, "_Rebuilt %s by reminder-coordinator. Edits here are overwritten — change the todos._\n",
		now.Format("Mon 2 Jan, 15:04 MST"))

	// Today's schedule leads the digest: the day's shape frames whether its todos
	// are even reachable today. Rendered before the empty-board check on purpose —
	// a full calendar day with no open todos is still worth seeing.
	c.renderSchedule(&out, events, calErr)

	if len(board.All()) == 0 {
		out.WriteString("\nNothing open. 🎉\n")
		return out.String()
	}

	section := func(heading string, items []*model.Item, label func(*model.Item) string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&out, "\n## %s (%d)\n\n", heading, len(items))
		for _, item := range items {
			fmt.Fprintf(&out, "- %s\n", label(item))
		}
	}

	section("⏰ Overdue", board.Overdue, func(item *model.Item) string {
		return fmt.Sprintf("%s%s — due %s%s", priorityMark(item), item.Title,
			item.DueAt.In(c.Location).Format("Mon 2 Jan"), areaTags(item, c.ScopeTag))
	})
	section("📅 Due today", board.DueToday, func(item *model.Item) string {
		return fmt.Sprintf("%s%s%s", priorityMark(item), item.Title, areaTags(item, c.ScopeTag))
	})
	section("🗓 Later this week", board.ThisWeek, func(item *model.Item) string {
		return fmt.Sprintf("%s%s — %s%s", priorityMark(item), item.Title,
			item.DueAt.In(c.Location).Format("Mon 2 Jan"), areaTags(item, c.ScopeTag))
	})
	section("🔭 Later", board.Later, func(item *model.Item) string {
		return fmt.Sprintf("%s%s — %s%s", priorityMark(item), item.Title,
			item.DueAt.In(c.Location).Format("Mon 2 Jan"), areaTags(item, c.ScopeTag))
	})
	section("💤 No due date", board.Undated, func(item *model.Item) string {
		return fmt.Sprintf("%s%s%s", priorityMark(item), item.Title, areaTags(item, c.ScopeTag))
	})

	return out.String()
}

// renderSchedule folds today's calendar into the digest: the timed events in
// order, any all-day markers, and one line reading how booked the day is. That
// last line is the point — it is what lets you tell, without opening a calendar,
// whether there is room today for a long task.
func (c *Coordinator) renderSchedule(out *bytes.Buffer, events []CalendarEvent, calErr error) {
	if calErr != nil {
		out.WriteString("\n## 🗓 Today's schedule\n\n⚠️ Couldn't load your calendar this tick.\n")
		return
	}

	dayStart := dateOf(c.Now.In(c.Location))
	dayEnd := dayStart.AddDate(0, 0, 1)

	timed := []CalendarEvent{}
	allDay := []CalendarEvent{}
	for _, event := range events {
		if event.AllDay {
			allDay = append(allDay, event)
			continue
		}
		start := event.Start.In(c.Location)
		if !start.Before(dayEnd) {
			continue // starts tomorrow or later
		}
		if !event.End.IsZero() && !event.End.In(c.Location).After(dayStart) {
			continue // ended before today began
		}
		timed = append(timed, event)
	}
	sort.SliceStable(timed, func(i, j int) bool { return timed[i].Start.Before(timed[j].Start) })

	if len(timed) == 0 && len(allDay) == 0 {
		out.WriteString("\n## 🗓 Today's schedule\n\nNothing on the calendar — the day is yours.\n")
		return
	}

	fmt.Fprintf(out, "\n## 🗓 Today's schedule (%d)\n\n", len(timed)+len(allDay))
	for _, event := range allDay {
		fmt.Fprintf(out, "- all day — %s\n", event.Summary)
	}
	for _, event := range timed {
		start := event.Start.In(c.Location)
		if event.End.IsZero() {
			fmt.Fprintf(out, "- %s — %s\n", clockTime(start), event.Summary)
			continue
		}
		fmt.Fprintf(out, "- %s–%s — %s\n", clockTime(start), clockTime(event.End.In(c.Location)), event.Summary)
	}
	fmt.Fprintf(out, "\n%s\n", c.dayLoad(timed))
}

// dayLoad reads how much of a waking day (8am–10pm) today's timed events consume,
// merging overlaps so a double-booked hour is not counted twice, and reports the
// biggest open stretch — the number that actually answers "can I start something
// long today?". A day can be only 4 hours "booked" and still have no 3-hour gap.
func (c *Coordinator) dayLoad(events []CalendarEvent) string {
	dayStart := dateOf(c.Now.In(c.Location))
	windowStart := dayStart.Add(8 * time.Hour)
	windowEnd := dayStart.Add(22 * time.Hour)
	window := windowEnd.Sub(windowStart)

	type interval struct{ start, end time.Time }
	clipped := []interval{}
	for _, event := range events {
		if event.End.IsZero() {
			continue
		}
		start := maxTime(event.Start.In(c.Location), windowStart)
		end := minTime(event.End.In(c.Location), windowEnd)
		if start.Before(end) {
			clipped = append(clipped, interval{start, end})
		}
	}
	if len(clipped) == 0 {
		return "🟢 Wide open (8am–10pm) — plenty of room for a big task."
	}
	sort.Slice(clipped, func(i, j int) bool { return clipped[i].start.Before(clipped[j].start) })

	merged := []interval{clipped[0]}
	for _, v := range clipped[1:] {
		last := &merged[len(merged)-1]
		if !v.start.After(last.end) {
			if v.end.After(last.end) {
				last.end = v.end
			}
			continue
		}
		merged = append(merged, v)
	}

	var booked time.Duration
	for _, m := range merged {
		booked += m.end.Sub(m.start)
	}
	biggest := merged[0].start.Sub(windowStart)
	for i := 1; i < len(merged); i++ {
		if gap := merged[i].start.Sub(merged[i-1].end); gap > biggest {
			biggest = gap
		}
	}
	if tail := windowEnd.Sub(merged[len(merged)-1].end); tail > biggest {
		biggest = tail
	}
	free := window - booked

	icon, verdict := "🟢", "room for a long task"
	switch {
	case biggest < 90*time.Minute:
		icon, verdict = "🔴", "no long open stretch — probably not the day for a big todo"
	case biggest < 3*time.Hour:
		icon, verdict = "🟡", "some room, but the biggest open block is modest"
	}
	return fmt.Sprintf("%s ~%s booked, ~%s free (8am–10pm); biggest open stretch ~%s — %s.",
		icon, roundDuration(booked), roundDuration(free), roundDuration(biggest), verdict)
}

// clockTime renders a wall-clock time compactly: "6pm", "6:30pm".
func clockTime(t time.Time) string {
	if t.Minute() == 0 {
		return t.Format("3pm")
	}
	return t.Format("3:04pm")
}

// roundDuration renders a duration to the nearest half hour ("5h", "1.5h"), or
// minutes under an hour ("45m").
func roundDuration(d time.Duration) string {
	if d <= 0 {
		return "0h"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(math.Round(d.Minutes())))
	}
	half := math.Round(d.Hours()*2) / 2
	if half == math.Trunc(half) {
		return fmt.Sprintf("%dh", int(half))
	}
	return fmt.Sprintf("%.1fh", half)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func priorityMark(item *model.Item) string {
	if item.Priority >= 3 {
		return "❗"
	}
	return ""
}

// areaTags renders the sub-tags that say what KIND of personal this is (car,
// home, health), dropping the scope tag itself — every item here has it, so
// printing it on every line says nothing.
func areaTags(item *model.Item, scopeTag string) string {
	areas := []string{}
	for _, tag := range item.Tags {
		if tag != scopeTag && tag != digestTag {
			areas = append(areas, tag)
		}
	}
	if len(areas) == 0 {
		return ""
	}
	return "  `" + strings.Join(areas, " ") + "`"
}

// ------------------------------------------------------------------ nudge

// nudgeOnce sends AT MOST ONE herald session per tick, and only when something
// actually fired.
//
// This is the fix. The old design let any of five jobs open a session whenever
// its own cron line came up, so an overdue todo with a dedicated nudge job got
// two sessions a day, and every fire minted a fresh one (herald `ask` has no
// session reuse) which then piled up in the dash "Needs you" list. Here there is
// one decision point: gather everything that fired, say it once, and record that
// it was said.
//
// The message is a pointer to the digest note, not a copy of it. The note is the
// record; restating it in chat is how you end up with two things to keep in sync.
func (c *Coordinator) nudgeOnce(board *Board) error {
	digestDue, err := c.dailyDigestIsDue()
	if err != nil {
		return err
	}
	if !digestDue && len(board.NagFiring) == 0 {
		return nil
	}

	var message bytes.Buffer
	if digestDue {
		fmt.Fprintf(&message, "📋 Personal todos — %s\n", c.Now.In(c.Location).Format("Mon 2 Jan"))
		fmt.Fprintf(&message, "%d overdue, %d due today, %d later this week.\n",
			len(board.Overdue), len(board.DueToday), len(board.ThisWeek))
	}
	if len(board.NagFiring) > 0 {
		message.WriteString("\n🔔 Still not done:\n")
		for _, item := range board.NagFiring {
			fmt.Fprintf(&message, "  • %s\n", item.Title)
		}
	}
	message.WriteString("\nFull board: the \"" + digestTitle + "\" note in dash /notes.\n")
	message.WriteString("\nWant to go through any of these?")

	log.Printf("%snudge: digest=%v nags=%d", dryPrefix(c.DryRun), digestDue, len(board.NagFiring))
	if c.DryRun {
		fmt.Println(message.String())
		return nil
	}

	if err := c.herald.Relay("personal-reminders",
		"Personal todos — "+c.Now.In(c.Location).Format("Jan 2"), message.String()); err != nil {
		return err
	}

	// Record what was said, so the next tick 15 minutes from now stays quiet.
	// Written AFTER a successful relay: a nag marked delivered by a send that
	// failed is a reminder silently lost, which is the failure mode that matters.
	for _, item := range board.NagFiring {
		stamped := *item.Schedule
		naggedAt := c.Now
		stamped.LastNaggedAt = &naggedAt
		if err := c.noteboard.SetSchedule(item.ID, &stamped, false); err != nil {
			return fmt.Errorf("stamp last_nagged_at on %s: %w", item.ID, err)
		}
	}
	if digestDue {
		return c.markDailyDigestSent()
	}
	return nil
}

// dailyDigestIsDue reports whether the digest hour has passed today and today's
// digest has not been sent yet. The marker lives on the digest note's links, so
// the coordinator keeps no state of its own.
func (c *Coordinator) dailyDigestIsDue() (bool, error) {
	now := c.Now.In(c.Location)
	if now.Before(c.DigestAt.OnDay(now)) {
		return false, nil
	}

	note, err := c.noteboard.FindByTag(model.TypeNote, digestTag)
	if err != nil {
		return false, err
	}
	if note == nil {
		return false, nil // first tick creates the note; nudge from the next one
	}

	todayMarker := digestSentMarker(now)
	for _, link := range note.Links {
		if link == todayMarker {
			return false, nil
		}
	}
	return true, nil
}

func (c *Coordinator) markDailyDigestSent() error {
	note, err := c.noteboard.FindByTag(model.TypeNote, digestTag)
	if err != nil || note == nil {
		return err
	}
	// Keep only today's marker: the note carries one line of state, not a growing
	// log of every day the digest ever went out.
	links := []string{digestSentMarker(c.Now.In(c.Location))}
	for _, link := range note.Links {
		if !strings.HasPrefix(link, "digest-sent:") {
			links = append(links, link)
		}
	}
	return c.noteboard.Patch(note.ID, map[string]any{"links": links})
}

func digestSentMarker(day time.Time) string {
	return "digest-sent:" + day.Format("2006-01-02")
}

// ------------------------------------------------------------------ calendar

// todaysCalendar reads today's events for the schedule section of the digest. It
// resolves its own token: syncCalendar resolves one too, but a second resolve per
// tick is cheap, and coupling the read to the mirror's token would be a false
// economy the first time one path is refactored.
func (c *Coordinator) todaysCalendar() ([]CalendarEvent, error) {
	token, err := c.calendar.ResolveDashToken()
	if err != nil {
		return nil, err
	}
	dayStart := dateOf(c.Now.In(c.Location))
	dayEnd := dayStart.AddDate(0, 0, 1)
	return c.calendar.ListEvents(token, dayStart, dayEnd)
}

// syncCalendar keeps a Google Calendar event in step with each item that asked
// for one, and tears down events for items that no longer want one.
//
// Opt-in, via remind.channels. The calendar is a notification surface, not the
// record — a calendar with every todo on it stops being usable as a calendar. An
// item says it wants one; otherwise it lives in the digest.
//
// The link scheme is the one personal-todo-calsync used (gcal: / gcalsync:), so
// any event that script created is recognised and kept in step rather than
// orphaned on the calendar with a fresh duplicate beside it.
func (c *Coordinator) syncCalendar() error {
	token, err := c.calendar.ResolveDashToken()
	if err != nil {
		return err
	}

	// Every scoped item in any status: an item that was done or un-opted-in still
	// has an event to take down.
	items, err := c.noteboard.ListAllStatuses(c.ScopeTag)
	if err != nil {
		return err
	}

	for _, item := range items {
		eventID, syncedDueAt, otherLinks := splitCalendarLinks(item.Links)

		wantsEvent := item.Status == "open" &&
			item.DueAt != nil &&
			!item.DueAt.Before(c.Now) &&
			item.Schedule != nil &&
			item.Schedule.Remind.DeliversOn(model.ChannelCalendar) &&
			!isTemplate(item) // the series has no single time to put on a calendar; its occurrences do

		switch {
		case wantsEvent && eventID == "":
			log.Printf("%scalendar create: %q", dryPrefix(c.DryRun), item.Title)
			if c.DryRun {
				continue
			}
			newID, err := c.calendar.CreateEvent(token, item)
			if err != nil {
				return err
			}
			if err := c.noteboard.Patch(item.ID, map[string]any{
				"links": withCalendarLinks(otherLinks, newID, item.DueAt),
			}); err != nil {
				return err
			}

		case wantsEvent && syncedDueAt != item.DueAt.Format(time.RFC3339):
			log.Printf("%scalendar update: %q", dryPrefix(c.DryRun), item.Title)
			if c.DryRun {
				continue
			}
			if err := c.calendar.UpdateEvent(token, eventID, item); err != nil {
				return err
			}
			if err := c.noteboard.Patch(item.ID, map[string]any{
				"links": withCalendarLinks(otherLinks, eventID, item.DueAt),
			}); err != nil {
				return err
			}

		case !wantsEvent && eventID != "":
			log.Printf("%scalendar remove: %q", dryPrefix(c.DryRun), item.Title)
			if c.DryRun {
				continue
			}
			if err := c.calendar.DeleteEvent(token, eventID); err != nil {
				return err
			}
			if err := c.noteboard.Patch(item.ID, map[string]any{"links": otherLinks}); err != nil {
				return err
			}
		}
	}
	return nil
}

func splitCalendarLinks(links []string) (eventID, syncedDueAt string, others []string) {
	others = []string{}
	for _, link := range links {
		switch {
		case strings.HasPrefix(link, linkGoogleCalendarEvent):
			eventID = strings.TrimPrefix(link, linkGoogleCalendarEvent)
		case strings.HasPrefix(link, linkGoogleCalendarSyncedDueAt):
			syncedDueAt = strings.TrimPrefix(link, linkGoogleCalendarSyncedDueAt)
		default:
			others = append(others, link)
		}
	}
	return eventID, syncedDueAt, others
}

func withCalendarLinks(others []string, eventID string, dueAt *time.Time) []string {
	links := append([]string(nil), others...)
	links = append(links, linkGoogleCalendarEvent+eventID)
	if dueAt != nil {
		links = append(links, linkGoogleCalendarSyncedDueAt+dueAt.Format(time.RFC3339))
	}
	return links
}

// ------------------------------------------------------------------ helpers

func dateOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func sortByDue(items []*model.Item) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].DueAt == nil || items[j].DueAt == nil {
			return items[j].DueAt == nil
		}
		return items[i].DueAt.Before(*items[j].DueAt)
	})
}

func dryPrefix(dryRun bool) string {
	if dryRun {
		return "[dry-run] "
	}
	return ""
}

// ------------------------------------------------------------------ herald

type Herald struct{ AskPath string }

// Relay opens a single herald session carrying the message. It ends in a question
// so llm-bridge-server's turn-end heuristic parks it as awaiting_user, which is
// what puts it in the dash "Needs you" list rather than letting it scroll away.
func (h *Herald) Relay(from, title, message string) error {
	command := exec.Command(h.AskPath, "-from", from, "-title", title, message)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ask: %w — %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// ------------------------------------------------------------------ noteboard

type NoteboardClient struct{ BaseURL string }

func (n *NoteboardClient) ListOpen(tag string) ([]*model.Item, error) {
	return n.list(fmt.Sprintf("/api/items?type=todo&status=open&tag=%s&limit=500", tag))
}

// ListAllStatuses covers the calendar teardown path: an item that was completed
// or archived still has an event to take down.
func (n *NoteboardClient) ListAllStatuses(tag string) ([]*model.Item, error) {
	all := []*model.Item{}
	for _, status := range []string{"open", "done", "archived"} {
		items, err := n.list(fmt.Sprintf("/api/items?type=todo&status=%s&tag=%s&limit=500", status, tag))
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
	}
	return all, nil
}

// ListScheduled returns scoped items in any status that carry a rule. Status is
// not filtered: a `done` rolling item is exactly what the roll step looks for,
// and a template keeps producing regardless of its own status.
func (n *NoteboardClient) ListScheduled(tag string) ([]*model.Item, error) {
	items, err := n.ListAllStatuses(tag)
	if err != nil {
		return nil, err
	}
	scheduled := []*model.Item{}
	for _, item := range items {
		if item.Schedule != nil {
			scheduled = append(scheduled, item)
		}
	}
	return scheduled, nil
}

func (n *NoteboardClient) ListChildren(parentID string) ([]*model.Item, error) {
	return n.list("/api/items?parent_id=" + parentID + "&limit=500")
}

func (n *NoteboardClient) FindByTag(itemType, tag string) (*model.Item, error) {
	items, err := n.list(fmt.Sprintf("/api/items?type=%s&tag=%s&limit=10", itemType, tag))
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	return items[0], nil
}

func (n *NoteboardClient) CreateItem(req *model.CreateItemRequest) (*model.Item, error) {
	var created model.Item
	if err := n.do("POST", "/api/items", req, &created); err != nil {
		return nil, err
	}
	return &created, nil
}

func (n *NoteboardClient) Patch(id string, fields map[string]any) error {
	return n.do("PATCH", "/api/items/"+id, fields, nil)
}

// SetSchedule writes back the coordinator's bookkeeping (last_materialized_at,
// last_nagged_at) without disturbing the rest of the rule.
func (n *NoteboardClient) SetSchedule(id string, schedule *model.Schedule, dryRun bool) error {
	if dryRun {
		return nil
	}
	return n.Patch(id, map[string]any{"schedule": schedule})
}

func (n *NoteboardClient) list(path string) ([]*model.Item, error) {
	var items []*model.Item
	if err := n.do("GET", path, nil, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (n *NoteboardClient) do(method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(encoded)
	}

	request, err := http.NewRequest(method, n.BaseURL+path, payload)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %s — %s", method, path, response.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// ------------------------------------------------------------------ calendar client

type GoogleCalendarMirror struct{ DashURL string }

// ResolveDashToken reads the token out of the running dash-server unit rather
// than keeping a copy. A rotated token is picked up on the next tick, and there
// is no second place for it to go stale.
func (g *GoogleCalendarMirror) ResolveDashToken() (string, error) {
	command := exec.Command("systemctl", "--user", "show", "dash-server.service", "-p", "Environment")
	command.Env = append(os.Environ(),
		"XDG_RUNTIME_DIR="+envOr("XDG_RUNTIME_DIR", fmt.Sprintf("/run/user/%d", os.Getuid())),
		"DBUS_SESSION_BUS_ADDRESS="+envOr("DBUS_SESSION_BUS_ADDRESS", fmt.Sprintf("unix:path=/run/user/%d/bus", os.Getuid())),
	)
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("read dash-server.service environment: %w", err)
	}
	for _, field := range strings.Fields(string(output)) {
		if token, found := strings.CutPrefix(field, "DASH_TOKEN="); found {
			return token, nil
		}
	}
	// Loud: without this, every calendar call would 401 and the mirror would look
	// like it simply had nothing to do.
	return "", fmt.Errorf("dash-server.service declares no DASH_TOKEN — refusing to guess")
}

// CalendarEvent is one occurrence on the user's Google Calendar, already expanded
// from any recurrence by the API (singleEvents=true). Timed events carry Start and
// End; all-day events (holidays, trips) set AllDay and are shown but never counted
// as booked time.
type CalendarEvent struct {
	Summary      string
	CalendarName string
	Start        time.Time
	End          time.Time
	AllDay       bool
}

// ListEvents returns events between timeMin and timeMax across every calendar the
// user has, via the same authenticated dash proxy the mirror writes through. Work
// and personal events come back together on purpose: for judging how booked a day
// is, a work meeting blocks the day exactly like a dance class does.
func (g *GoogleCalendarMirror) ListEvents(token string, timeMin, timeMax time.Time) ([]CalendarEvent, error) {
	path := fmt.Sprintf("/api/calendar/google?timeMin=%s&timeMax=%s",
		url.QueryEscape(timeMin.Format(time.RFC3339)),
		url.QueryEscape(timeMax.Format(time.RFC3339)))

	var resp struct {
		Events []struct {
			Summary      string `json:"summary"`
			CalendarName string `json:"calendarName"`
			Start        struct {
				DateTime string `json:"dateTime"`
				Date     string `json:"date"`
			} `json:"start"`
			End struct {
				DateTime string `json:"dateTime"`
				Date     string `json:"date"`
			} `json:"end"`
		} `json:"events"`
		Error string `json:"error"`
	}
	if err := g.do(token, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	// The proxy answers 200 with an {error, events:[]} body when a single calendar
	// fails; treat that as loud, not as "no events today".
	if resp.Error != "" {
		return nil, fmt.Errorf("calendar list: %s", resp.Error)
	}

	events := make([]CalendarEvent, 0, len(resp.Events))
	for _, e := range resp.Events {
		event := CalendarEvent{Summary: e.Summary, CalendarName: e.CalendarName}
		switch {
		case e.Start.DateTime != "":
			start, err := time.Parse(time.RFC3339, e.Start.DateTime)
			if err != nil {
				return nil, fmt.Errorf("event %q start %q: %w", e.Summary, e.Start.DateTime, err)
			}
			event.Start = start
			if e.End.DateTime != "" {
				end, err := time.Parse(time.RFC3339, e.End.DateTime)
				if err != nil {
					return nil, fmt.Errorf("event %q end %q: %w", e.Summary, e.End.DateTime, err)
				}
				event.End = end
			}
		case e.Start.Date != "":
			event.AllDay = true
			day, err := time.Parse("2006-01-02", e.Start.Date)
			if err != nil {
				return nil, fmt.Errorf("event %q date %q: %w", e.Summary, e.Start.Date, err)
			}
			event.Start = day
		default:
			continue // an event with no start is nothing we can place
		}
		events = append(events, event)
	}
	return events, nil
}

func (g *GoogleCalendarMirror) CreateEvent(token string, item *model.Item) (string, error) {
	payload := eventPayload(item)
	payload["description"] = fmt.Sprintf("Personal todo from noteboard (id %s).", item.ID)

	var created struct {
		ID string `json:"id"`
	}
	if err := g.do(token, "POST", "/api/calendar/google", payload, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", fmt.Errorf("calendar create for %q returned no event id", item.Title)
	}
	return created.ID, nil
}

func (g *GoogleCalendarMirror) UpdateEvent(token, eventID string, item *model.Item) error {
	return g.do(token, "PUT", "/api/calendar/google/"+eventID, eventPayload(item), nil)
}

func (g *GoogleCalendarMirror) DeleteEvent(token, eventID string) error {
	err := g.do(token, "DELETE", "/api/calendar/google/"+eventID, nil, nil)
	// Google leaves a `cancelled` tombstone, and re-deleting one returns 410. An
	// event that is already gone is the state we wanted.
	if err != nil && (strings.Contains(err.Error(), "410") || strings.Contains(err.Error(), "404")) {
		return nil
	}
	return err
}

func eventPayload(item *model.Item) map[string]any {
	start := item.DueAt
	end := start.Add(calendarEventMinutes * time.Minute)
	return map[string]any{
		"summary": "📋 " + item.Title,
		"start":   map[string]string{"dateTime": start.Format(time.RFC3339)},
		"end":     map[string]string{"dateTime": end.Format(time.RFC3339)},
	}
}

func (g *GoogleCalendarMirror) do(token, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(encoded)
	}

	request, err := http.NewRequest(method, g.DashURL+path, payload)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %s — %s", method, path, response.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
