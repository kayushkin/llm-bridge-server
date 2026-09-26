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
//  1. materialize  due occurrences of recurring templates into real todos
//  2. roll         completed rolling-mode items forward to their next occurrence
//  3. digest       rewrite the standing "Today" note — the always-available board
//  4. calendar     mirror opted-in items to Google Calendar (secondary, opt-in)
//  5. nudge        at most ONE herald session, and only when something new fired
//
// The digest note is the primary surface and is rewritten every tick, so it is
// always current and always in one place. Chat is the exception channel, not the
// record. The calendar is opt-in per item (remind.channels), because a calendar
// full of todos stops being a calendar.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
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

	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/kayushkin/noteboard/model"
	"github.com/slava/scheduler/internal/boundedtext"
	schedulerconfig "github.com/slava/scheduler/internal/config"
	"github.com/slava/scheduler/internal/degradations"
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

// config is every value reminder-coordinator reads off its command line.
type config struct {
	noteboardURL string
	dashURL      string
	askPath      string
	scopeTag     string
	digestHour   int
	digestMinute int
	workStart    string
	workEnd      string
	workDays     string
	timezone     string
	lookahead    time.Duration
	dryRun       bool

	// Not flags: where `systemctl --user` finds this user's manager when it
	// reads dash-server's token.
	runtimeDirectory  string
	sessionBusAddress string
}

// errSettingsRefused marks a parseFlags error that came from the environment:
// the settings registry refused a value. main prints it and exits 1, where the
// flag package has already printed its own errors and exits 2.
var errSettingsRefused = errors.New("settings refused")

// parseFlags builds the flag set and parses args into a config.
//
// The flag set lives here rather than in main() so that a test can reach the
// defaults. Registered inside main() they were unreachable — nothing in a test
// can call main() — and the suite builds its own Coordinator field by field, so
// every one of these nine values was set by the tests and none of them was the
// value that ships. The digest hour is the one that matters most: this binary is
// the only thing on the box that reminds the user of anything, and a wrong hour
// is silent. It does not fail, it just nudges at a time nobody is reading.
//
// The flag set is a fresh FlagSet, not the package-level flag.CommandLine: a
// test calling this twice would panic on the redefined flag, and a mistake there
// would look like a broken fixture rather than the seam being unusable.
// ContinueOnError is what makes it callable at all — ExitOnError would take the
// test binary down with it on a bad argument instead of returning the error.
//
// The environment is an argument, not the process's own, so that a test states
// which variables were set without setting them: the defaults below are read
// at flag-registration time, so a test that exported NOTEBOARD_URL to observe
// it would be racing every other test in the package for one process-wide
// variable. It is read through the binary's settings registry
// (internal/config), which refuses a misspelled REMINDER_ variable. The -ask
// default is ~/bin/ask, from the home directory the registry is built in.
func parseFlags(args []string, environment servicesettings.Environment) (config, error) {
	settings, err := schedulerconfig.ReminderCoordinator.NewSettingsRegistry(environment)
	if err != nil {
		return config{}, fmt.Errorf("%w: %v", errSettingsRefused, err)
	}
	fs := flag.NewFlagSet("reminder-coordinator", flag.ContinueOnError)
	var c config
	fs.StringVar(&c.noteboardURL, "noteboard-url", settings.String(schedulerconfig.SettingNoteboardURL), "noteboard base URL — the source of truth for todos and their rules.")
	fs.StringVar(&c.dashURL, "dash-url", settings.String(schedulerconfig.SettingDashURL), "dash base URL, for the authenticated Google Calendar proxy.")
	fs.StringVar(&c.askPath, "ask", settings.String(schedulerconfig.SettingAskPath), "path to the herald `ask` binary.")
	fs.StringVar(&c.scopeTag, "tag", "personal", "only items carrying this tag are coordinated. Keeps the ~40 bot/coding todos out of the digest.")
	fs.IntVar(&c.digestHour, "digest-hour", 7, "local hour to send the daily digest nudge.")
	fs.IntVar(&c.digestMinute, "digest-minute", 30, "local minute to send the daily digest nudge.")
	fs.StringVar(&c.workStart, "work-start", "08:00", "local time the working day begins. Hours inside it are not open time, so they are never offered as room for a todo.")
	fs.StringVar(&c.workEnd, "work-end", "16:30", "local time the working day ends.")
	fs.StringVar(&c.workDays, "work-days", "Mon,Tue,Wed,Thu,Fri", "days the working hours apply to. A day not named here is free all the way from 8am.")
	fs.StringVar(&c.timezone, "tz", settings.String(schedulerconfig.SettingTimezone), "IANA zone the daily digest hour is interpreted in.")
	fs.DurationVar(&c.lookahead, "lookahead", 7*24*time.Hour, "how far ahead to materialize occurrences of a recurring template.")
	fs.BoolVar(&c.dryRun, "dry-run", false, "report what would change; mutate nothing, send nothing.")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	// reject reports a configuration the flag set parsed but the program cannot
	// use. It prints where fs.Parse prints its own errors, because main exits 2
	// on anything this returns: a validation failure that only set the exit code
	// would kill the one job on this box that reminds the user of anything, and
	// say nothing about why.
	reject := func(format string, arguments ...any) (config, error) {
		err := fmt.Errorf(format, arguments...)
		fmt.Fprintln(fs.Output(), err)
		fs.Usage()
		return config{}, err
	}

	workStart, err := parseDailyTime(c.workStart)
	if err != nil {
		return reject("-work-start: %w", err)
	}
	workEnd, err := parseDailyTime(c.workEnd)
	if err != nil {
		return reject("-work-end: %w", err)
	}
	// A working day that ends before it starts would measure as no work at all,
	// handing back the whole day as open time — the wrong answer, arrived at
	// quietly.
	if !workEnd.OnDay(referenceDay).After(workStart.OnDay(referenceDay)) {
		return reject("-work-end %s is not after -work-start %s", c.workEnd, c.workStart)
	}
	if _, err := parseWorkDays(c.workDays); err != nil {
		return reject("-work-days: %w", err)
	}
	c.runtimeDirectory = settings.String(schedulerconfig.SettingRuntimeDirectory)
	c.sessionBusAddress = settings.String(schedulerconfig.SettingSessionBusAddress)
	return c, nil
}

// weekdaysByName is the set -work-days is read against. A table rather than a
// chain of comparisons so an unknown day is one lookup that fails, and so the
// error can list what was actually accepted.
var weekdaysByName = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// parseWorkDays reads "Mon,Tue,Wed,Thu,Fri" into the set of days the working
// hours apply to.
//
// An empty day is refused rather than skipped. A blank value, a trailing comma
// and a doubled one all produce one, and skipping them would read as "no work
// days" and hand every day back as wide open — the same silent wrong answer a
// backwards working day gives, which is why that is refused too. There is no
// separate check for an empty RESULT: strings.Split never yields an empty slice,
// so every path here either records a day or returns an error.
func parseWorkDays(value string) (map[time.Weekday]bool, error) {
	days := map[time.Weekday]bool{}
	for _, name := range strings.Split(value, ",") {
		trimmed := strings.ToLower(strings.TrimSpace(name))
		if trimmed == "" {
			return nil, fmt.Errorf("%q has an empty day in it", value)
		}
		weekday, known := weekdaysByName[trimmed]
		if !known {
			return nil, fmt.Errorf("%q is not a day: use Mon, Tue, Wed, Thu, Fri, Sat or Sun", name)
		}
		days[weekday] = true
	}
	return days, nil
}

// referenceDay is an arbitrary fixed day used only to compare two daily times to
// each other. Any day works — both sides land on the same one — and a constant
// keeps the comparison off the wall clock, where it would depend on which day
// the process happened to start.
var referenceDay = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// parseDailyTime reads "8:00" or "16:30" into the hour and minute it names.
// Rejecting a malformed value here rather than defaulting it is the point: a
// working day silently reset to midnight would report the whole day as open.
func parseDailyTime(value string) (dailyTime, error) {
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return dailyTime{}, fmt.Errorf("%q is not a 24-hour HH:MM time", value)
	}
	return dailyTime{Hour: parsed.Hour(), Minute: parsed.Minute()}, nil
}

// newCoordinator wires a parsed config onto the Coordinator that runs the tick.
//
// This is lifted out of main() for the same reason the flag set is, and it is
// the half of main() with something to get wrong. Nine flags reach eight fields
// and a two-field struct; the pairs that read alike — digest hour against digest
// minute, noteboard URL against dash URL — would swap without a compile error
// and without a test noticing. Trimming the trailing slash off both base URLs
// happens here too, and that was unreachable as well: every URL the suite builds
// a client with is already clean.
func newCoordinator(c config, location *time.Location, now time.Time) *Coordinator {
	return &Coordinator{
		noteboard: &NoteboardClient{BaseURL: strings.TrimRight(c.noteboardURL, "/")},
		calendar:  &GoogleCalendarMirror{DashURL: strings.TrimRight(c.dashURL, "/"), RuntimeDirectory: c.runtimeDirectory, SessionBusAddress: c.sessionBusAddress},
		herald:    &Herald{AskPath: c.askPath},
		// The subject matches the prefix this command's own error lines already
		// carry, so a reader of scheduler run history meets one voice rather than
		// two. Constructed here rather than left nil for a caller to supply: every
		// coordinator has to be able to record, and a nil recorder would turn the
		// first shortfall into a panic on the one path this package exists to make
		// survivable.
		Degraded:  degradations.New("reminder-coordinator"),
		ScopeTag:  c.scopeTag,
		Location:  location,
		DigestAt:  dailyTime{Hour: c.digestHour, Minute: c.digestMinute},
		WorkStart: mustParseDailyTime(c.workStart),
		WorkEnd:   mustParseDailyTime(c.workEnd),
		WorkDays:  mustParseWorkDays(c.workDays),
		Lookahead: c.lookahead,
		DryRun:    c.dryRun,
		Now:       now.In(location),
	}
}

// mustParseDailyTime is safe only because newCoordinator runs after parseFlags,
// which has already rejected anything unparseable. A panic here means those two
// were reordered, which is worth a crash rather than a silent midnight.
// mustParseWorkDays is safe for the same reason as mustParseDailyTime: parseFlags
// has already rejected anything unreadable.
func mustParseWorkDays(value string) map[time.Weekday]bool {
	parsed, err := parseWorkDays(value)
	if err != nil {
		panic("newCoordinator reached unvalidated work days: " + err.Error())
	}
	return parsed
}

func mustParseDailyTime(value string) dailyTime {
	parsed, err := parseDailyTime(value)
	if err != nil {
		panic("newCoordinator reached an unvalidated daily time: " + err.Error())
	}
	return parsed
}

func main() {
	os.Exit(run())
}

// run is the whole command. It returns the process exit status rather than
// calling os.Exit itself, so that every path out settles the run's verdict
// through the same return and a new one cannot forget to. Copied in shape from
// cmd/kanban-classifier and cmd/email-classifier, which is where this verdict was
// worked out; see internal/degradations for why a tick that fell short must not
// exit 0.
func run() int {
	cfg, err := parseFlags(os.Args[1:], servicesettings.ProcessEnvironment())
	if err != nil {
		if errors.Is(err, errSettingsRefused) {
			log.Fatal(err)
		}
		// parseFlags has already printed the error and the usage message — both
		// for what fs.Parse rejects and for what it validates afterwards. These
		// are the two exit codes flag.ExitOnError would have used, kept identical
		// so lifting the flag set out of main() changed nothing a caller sees.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	location, err := time.LoadLocation(cfg.timezone)
	if err != nil {
		log.Fatalf("reminder-coordinator: bad -tz %q: %v", cfg.timezone, err)
	}

	coordinator := newCoordinator(cfg, location, time.Now())

	if err := coordinator.Run(); err != nil {
		// Loud. A reminder system that fails quietly is indistinguishable from one
		// with nothing to say, and you find out weeks later by missing something.
		log.Fatalf("reminder-coordinator: %v", err)
	}

	// A tick that reminded nobody because it had nothing to say exits 0; a tick
	// that fell short of part of its job exits non-zero, and the report names
	// which part. Telling those two apart in scheduler run history is the whole
	// reason this return exists.
	return coordinator.Degraded.ExitCode()
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
	// Relay delivers message to the user and returns the bus session id it
	// landed in. With reuseSessionID empty it opens a new herald session;
	// otherwise it adds a turn to that existing session (and title, a
	// create-time name, must be empty).
	Relay(from, title, message, reuseSessionID string) (string, error)
}

type Coordinator struct {
	noteboard noteboardAPI
	calendar  calendarMirror
	herald    heraldRelay

	// Degraded collects the ways one tick fell short of its job, so the shortfall
	// reaches scheduler run history rather than living only in the log body. It is
	// a field rather than a package global because the failure sites are methods
	// on this type and the tests build one coordinator per case.
	Degraded *degradations.Recorder

	ScopeTag string
	Location *time.Location
	DigestAt dailyTime
	// WorkStart and WorkEnd bound the hours that are spoken for every day. They
	// are measured as busy, so the free time the digest reports is time the user
	// could actually spend on a todo.
	WorkStart dailyTime
	WorkEnd   dailyTime
	// WorkDays is which days those hours apply to. A Saturday is free from 8am,
	// and a digest that told the user otherwise would be hiding the one day the
	// overdue pile is actually reachable.
	WorkDays  map[time.Weekday]bool
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
		// Carrying on is right; exiting 0 afterwards is not. The digest and the
		// nudge both go out without the day's shape, which is the half of them the
		// user reads to decide whether an overdue todo is reachable today. Measured
		// on the live job before this line existed: 12 of the last 100 ticks of job
		// 32 logged exactly the failure above — auth-store held no google
		// credential for two hours on 2026-08-28 — and all 100 runs were recorded
		// `success`, so the outage reached nobody.
		c.Degraded.Record("today's calendar could not be read, so the digest and the nudge went out without the day's schedule: %v", calErr)
	}

	if err := c.writeDigestNote(board, events, calErr); err != nil {
		return fmt.Errorf("write digest note: %w", err)
	}
	if err := c.syncCalendar(); err != nil {
		return fmt.Errorf("sync calendar: %w", err)
	}
	if err := c.nudgeOnce(board, events, calErr); err != nil {
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

	renderSection(&out, digestStyle, headingOverdue, board.Overdue, c.labelWithLateness)
	renderSection(&out, digestStyle, headingDueToday, board.DueToday, c.labelTitleOnly)
	renderSection(&out, digestStyle, headingThisWeek, board.ThisWeek, c.labelWithDueDate)
	renderSection(&out, digestStyle, "🔭 Later", board.Later, c.labelWithDueDate)
	renderSection(&out, digestStyle, "💤 No due date", board.Undated, c.labelTitleOnly)

	return out.String()
}

// The headings the digest note and the herald nudge share. They are constants
// because the two surfaces must name the same bucket the same way: a nudge that
// says "Late" against a note that says "Overdue" reads as two different boards.
const (
	headingOverdue  = "⏰ Overdue"
	headingDueToday = "📅 Due today"
	headingThisWeek = "🗓 Later this week"
	headingSchedule = "🗓 Today's schedule"
)

// sectionStyle is how a bucket is dressed for one surface. The digest note is a
// markdown document and the nudge is a chat message, so they differ in heading
// and bullet — and in nothing else. Everything that says what an ITEM is lives
// in the label functions below, which both surfaces call.
type sectionStyle struct {
	// HeadingFormat takes the heading and the bucket's TOTAL size, which stays
	// the true total even when Limit hides some of it.
	HeadingFormat string
	Bullet        string
	// Limit caps how many items are listed; 0 lists every one. Only the nudge
	// sets it — see nudgeSectionLimit.
	Limit int
}

var (
	digestStyle = sectionStyle{HeadingFormat: "\n## %s (%d)\n\n", Bullet: "- "}
	nudgeStyle  = sectionStyle{HeadingFormat: "\n%s (%d)\n", Bullet: "  ", Limit: nudgeSectionLimit}
	// The day's schedule is NOT capped, where the todo buckets are. A backlog
	// grows without limit and a day cannot: it is bounded by being one day. The
	// cap also truncated in the worst possible place — chronological order puts
	// the evening last, so a day with six meetings hid the 8pm event, which is
	// the one that answers whether there is room tonight.
	nudgeScheduleStyle = sectionStyle{HeadingFormat: "\n%s (%d)\n", Bullet: "  "}
)

// nudgeSectionLimit caps how many items of one bucket the chat nudge lists. The
// note is the record and must stay total; a chat message with fifteen overdue
// items in it is one nobody reads to the end. Five is enough to make the nudge
// worth reading on its own and short enough that it still points at the note.
const nudgeSectionLimit = 5

// renderSection writes one bucket of the board under a heading. Both the digest
// note and the herald nudge render through here over the same label functions,
// so the two cannot disagree about what an item says — the nudge is a shorter
// view of the board, never a second copy of it.
//
// A capped section always says how many it dropped. A silent truncation reads as
// "this is everything", which is the same lie a non-total bucket tells.
func renderSection[T any](out *bytes.Buffer, style sectionStyle, heading string, items []T, label func(T) string) {
	if len(items) == 0 {
		return
	}
	shown := items
	if style.Limit > 0 && len(shown) > style.Limit {
		shown = shown[:style.Limit]
	}
	fmt.Fprintf(out, style.HeadingFormat, heading, len(items))
	for _, item := range shown {
		fmt.Fprintf(out, "%s%s\n", style.Bullet, label(item))
	}
	if dropped := len(items) - len(shown); dropped > 0 {
		fmt.Fprintf(out, "%s…and %d more in the note\n", style.Bullet, dropped)
	}
}

// labelWithLateness renders an overdue item: its due date AND how long it has
// been sitting there. The date alone makes every overdue item look alike — "due
// Mon 20 Jul" and "due Sat 15 Aug" read as equally late until you work out
// today's date yourself, which is the arithmetic the reminder exists to spare
// you.
func (c *Coordinator) labelWithLateness(item *model.Item) string {
	return fmt.Sprintf("%s%s — due %s (%dd late)%s", priorityMark(item), item.Title,
		item.DueAt.In(c.Location).Format("Mon 2 Jan"), c.daysLate(item), areaTags(item, c.ScopeTag))
}

// labelWithDueDate renders a future-dated item: when it lands, no lateness.
func (c *Coordinator) labelWithDueDate(item *model.Item) string {
	return fmt.Sprintf("%s%s — %s%s", priorityMark(item), item.Title,
		item.DueAt.In(c.Location).Format("Mon 2 Jan"), areaTags(item, c.ScopeTag))
}

// labelTitleOnly renders an item whose date says nothing worth printing: due
// today (the heading already said so) or never dated at all.
func (c *Coordinator) labelTitleOnly(item *model.Item) string {
	return fmt.Sprintf("%s%s%s", priorityMark(item), item.Title, areaTags(item, c.ScopeTag))
}

// daysLate counts WHOLE CALENDAR DAYS from an item's due date to today, which is
// how a person counts them — an item due 9am yesterday is "1d late" at 8am today,
// not "0d". Both ends are midnight in the user's zone, so the subtraction is
// rounded to whole days: across a DST boundary the gap is 23 or 25 hours and
// truncating it would drop or add a day twice a year.
func (c *Coordinator) daysLate(item *model.Item) int {
	due := dateOf(item.DueAt.In(c.Location))
	return int(math.Round(dateOf(c.Now.In(c.Location)).Sub(due).Hours() / 24))
}

// splitTodaysEvents keeps only what actually falls on today and separates the
// timed events, in start order, from the all-day markers. Both the digest
// section and the one-line nudge read the day through here, so they can never
// count a different number of events for the same day.
func (c *Coordinator) splitTodaysEvents(events []CalendarEvent) (timed, allDay []CalendarEvent) {
	dayStart := dateOf(c.Now.In(c.Location))
	dayEnd := dayStart.AddDate(0, 0, 1)

	// All-day events are bounded by the same window, in civil dates rather than
	// instants. They used to be exempt, which read as "an all-day event is always
	// today" — but a trip and a holiday are as capable of being next month as a
	// meeting is, and the heading counts them.
	today := civilDate(dayStart)

	timed = []CalendarEvent{}
	allDay = []CalendarEvent{}
	for _, event := range events {
		if event.AllDay {
			if !civilDate(event.Start).After(today) && today.Before(allDayEnd(event)) {
				allDay = append(allDay, event)
			}
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
	return timed, allDay
}

// renderSchedule folds today's calendar into the digest: the events in order and
// one line reading how much of the day is actually left. That last line is the
// point — it is what lets you tell, without opening a calendar, whether there is
// room today for a long task.
func (c *Coordinator) renderSchedule(out *bytes.Buffer, events []CalendarEvent, calErr error) {
	if calErr != nil {
		out.WriteString("\n## " + headingSchedule + "\n\n⚠️ Couldn't load your calendar this tick.\n")
		return
	}

	timed, allDay := c.splitTodaysEvents(events)
	if len(timed) == 0 && len(allDay) == 0 {
		out.WriteString("\n## " + headingSchedule + "\n\nNothing on the calendar — the day is yours.\n")
		fmt.Fprintf(out, "\n%s\n", c.dayLoad(nil))
		return
	}

	renderSection(out, digestStyle, headingSchedule, c.orderedForDisplay(allDay, timed), c.eventLabel)
	fmt.Fprintf(out, "\n%s\n", c.dayLoad(timed))
}

// orderedForDisplay is the one order both surfaces list a day in: the all-day
// markers, which own no hour, above the timed events that do.
func (c *Coordinator) orderedForDisplay(allDay, timed []CalendarEvent) []CalendarEvent {
	ordered := make([]CalendarEvent, 0, len(allDay)+len(timed))
	ordered = append(ordered, allDay...)
	return append(ordered, timed...)
}

// eventLabel renders one calendar event as a line. Shared by the digest section
// and the nudge for the same reason the todo labels are: two renderings of one
// event are two things to keep in step.
func (c *Coordinator) eventLabel(event CalendarEvent) string {
	if event.AllDay {
		return "all day — " + event.Summary
	}
	start := clockTime(event.Start.In(c.Location))
	if event.End.IsZero() {
		return fmt.Sprintf("%s — %s", start, event.Summary)
	}
	return fmt.Sprintf("%s–%s — %s", start, clockTime(event.End.In(c.Location)), event.Summary)
}

// dayShape is how much of the day is actually available, measured once and
// rendered two ways — the digest's sentence and the nudge's line — because the
// MEASUREMENT is what must not differ between them; how many words each spends
// on it may.
type dayShape struct {
	// Free and BiggestOpen both EXCLUDE the working day. An 8-hour workday spent
	// in the "free" column is the reason this exists: a digest that offered a
	// Tuesday morning as room for a long todo was offering time that was already
	// gone, which makes the whole line worth ignoring.
	Free, BiggestOpen time.Duration
	Icon, Verdict     string
}

// measureDay reads the day against a waking window (8am–10pm), treating the
// working day as busy alongside the calendar's own events, and finds the biggest
// open stretch — the number that actually answers "can I start something long
// today?". A day can be only 4 hours booked and still have no 3-hour gap.
//
// Work is measured as just another busy block rather than by trimming the window
// down to the evening. That is what makes a meeting at 10am cost nothing (it
// merges into the work block it sits inside) while a meeting from 4pm to 5pm
// costs only the half hour of it that lands after work.
func (c *Coordinator) measureDay(events []CalendarEvent) dayShape {
	dayStart := dateOf(c.Now.In(c.Location))
	windowStart := dayStart.Add(8 * time.Hour)
	windowEnd := dayStart.Add(22 * time.Hour)
	window := windowEnd.Sub(windowStart)

	type interval struct{ start, end time.Time }
	clip := func(start, end time.Time) (interval, bool) {
		start, end = maxTime(start, windowStart), minTime(end, windowEnd)
		return interval{start, end}, start.Before(end)
	}

	busy := []interval{}
	if c.isWorkDay(dayStart) {
		if work, ok := clip(c.WorkStart.OnDay(dayStart), c.WorkEnd.OnDay(dayStart)); ok {
			busy = append(busy, work)
		}
	}
	for _, event := range events {
		if event.End.IsZero() {
			continue
		}
		if clipped, ok := clip(event.Start.In(c.Location), event.End.In(c.Location)); ok {
			busy = append(busy, clipped)
		}
	}
	if len(busy) == 0 {
		return dayShape{
			Free: window, BiggestOpen: window,
			Icon: "🟢", Verdict: "room for a long task",
		}
	}
	sort.Slice(busy, func(i, j int) bool { return busy[i].start.Before(busy[j].start) })

	merged := []interval{busy[0]}
	for _, v := range busy[1:] {
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

	icon, verdict := "🟢", "room for a long task"
	switch {
	case biggest < 90*time.Minute:
		icon, verdict = "🔴", "no long open stretch — probably not the day for a big todo"
	case biggest < 3*time.Hour:
		icon, verdict = "🟡", "some room, but the biggest open block is modest"
	}
	return dayShape{Free: window - booked, BiggestOpen: biggest, Icon: icon, Verdict: verdict}
}

// isWorkDay reports whether the working hours apply to a given day. Saturday and
// Sunday are not working days by default, so the whole window is open on them.
func (c *Coordinator) isWorkDay(day time.Time) bool {
	return c.WorkDays[day.In(c.Location).Weekday()]
}

// workHours renders the working day as the digest names it: "8am–4:30pm".
func (c *Coordinator) workHours() string {
	day := dateOf(c.Now.In(c.Location))
	return fmt.Sprintf("%s–%s", clockTime(c.WorkStart.OnDay(day)), clockTime(c.WorkEnd.OnDay(day)))
}

// dayLoad is the digest note's reading of the day, spelled out, since the note is
// what you open when you are deciding what to do with the day. It says the
// working day out loud rather than quietly subtracting it: a free figure that
// dropped eight hours without explaining why reads as a bug.
func (c *Coordinator) dayLoad(events []CalendarEvent) string {
	shape := c.measureDay(events)
	// On a day off the line must NOT say "Work 8am–4:30pm". It would be the one
	// sentence in the digest that is simply untrue, and it would hide the fact
	// that a Saturday is the day the overdue pile is most reachable.
	if !c.isWorkDay(c.Now) {
		return fmt.Sprintf("%s No work today; ~%s free (8am–10pm), biggest open stretch ~%s — %s.",
			shape.Icon, roundDuration(shape.Free), roundDuration(shape.BiggestOpen), shape.Verdict)
	}
	return fmt.Sprintf("%s Work %s; ~%s free outside it (to 10pm), biggest open stretch ~%s — %s.",
		shape.Icon, c.workHours(), roundDuration(shape.Free), roundDuration(shape.BiggestOpen), shape.Verdict)
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
// two sessions a day, and every fire minted a fresh one which then piled up in
// the dash "Needs you" list. Here there is one decision point: gather
// everything that fired, say it once, and record that it was said. And since
// 2026-08-31 saying it twice in one day reuses one session: the first relay's
// id is recorded on the digest note, and a later firing (a nag whose rule puts
// it after the digest hour) is delivered into that session via `ask -session`
// rather than opening a second one under the same title.
//
// The message NAMES the work rather than counting it. It used to say only "4
// overdue, 1 due today" and point at the digest note, on the reasoning that
// restating the note in chat leaves two things to keep in sync. The counts were
// too thin to act on — you cannot tell from "4 overdue" whether to stop what you
// are doing — and the sync worry is answered by construction now: both surfaces
// render through renderSection over the same label functions off the same Board,
// so there is one description of an item, not two. What the nudge does
// differently is stop early (nudgeSectionLimit) and skip the buckets that are
// not asking for anything today. The note remains the full record, and the
// message still points at it.
func (c *Coordinator) nudgeOnce(board *Board, events []CalendarEvent, calErr error) error {
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
		renderSection(&message, nudgeStyle, headingOverdue, board.Overdue, c.labelWithLateness)
		renderSection(&message, nudgeStyle, headingDueToday, board.DueToday, c.labelTitleOnly)
		renderSection(&message, nudgeStyle, headingThisWeek, board.ThisWeek, c.labelWithDueDate)
		if len(board.Overdue)+len(board.DueToday)+len(board.ThisWeek) == 0 {
			// Say so out loud. A digest that renders no sections and no line about
			// it reads as a message that failed to load, not as a clear board.
			message.WriteString("\nNothing due today, and nothing late. 🎉\n")
		}
		c.renderNudgeSchedule(&message, events, calErr)
	}
	if len(board.NagFiring) > 0 {
		message.WriteString("\n🔔 Still not done:\n")
		for _, item := range board.NagFiring {
			fmt.Fprintf(&message, "  • %s\n", item.Title)
		}
	}
	message.WriteString("\nFull board: the \"" + digestTitle + "\" note in dash /notes.\n")
	message.WriteString("\nWant to go through any of these?")

	// One herald session per day, not per firing. The first relay of the day
	// opens the session and its id is recorded on the digest note; every later
	// relay that day — typically a nag firing after the morning digest — lands
	// as a new turn in that same session instead of a second "Needs you" entry
	// with the same title. (2026-08-31: a Monday-morning nag 90 minutes after
	// the digest produced exactly that pair.)
	reuseSessionID, err := c.todaysHeraldSessionID()
	if err != nil {
		return err
	}

	log.Printf("%snudge: digest=%v nags=%d reuse=%s", dryPrefix(c.DryRun), digestDue, len(board.NagFiring), reuseSessionID)
	if c.DryRun {
		fmt.Println(message.String())
		return nil
	}

	title := ""
	if reuseSessionID == "" {
		title = "Personal todos — " + c.Now.In(c.Location).Format("Jan 2")
	}
	sessionID, err := c.herald.Relay("personal-reminders", title, message.String(), reuseSessionID)
	if err != nil {
		return err
	}

	// Record what was said, so the next tick 15 minutes from now stays quiet.
	// Written AFTER a successful relay: a nag marked delivered by a send that
	// failed is a reminder silently lost, which is the failure mode that matters.
	// A relay into today's existing session with no digest has nothing new to
	// record — re-patching an unchanged marker would mint a needless revision.
	if sessionID != reuseSessionID || digestDue {
		if err := c.recordRelayMarkers(sessionID, digestDue); err != nil {
			return err
		}
	}
	for _, item := range board.NagFiring {
		stamped := *item.Schedule
		naggedAt := c.Now
		stamped.LastNaggedAt = &naggedAt
		if err := c.noteboard.SetSchedule(item.ID, &stamped, false); err != nil {
			return fmt.Errorf("stamp last_nagged_at on %s: %w", item.ID, err)
		}
	}
	return nil
}

// renderNudgeSchedule puts today's calendar in the nudge: the events themselves,
// then the same day-load line the note carries. The events are there because
// "7 events" does not tell you whether the day is yours after 5pm or broken into
// twenty-minute pieces, and the todos above are only actionable against a day
// you can see.
func (c *Coordinator) renderNudgeSchedule(out *bytes.Buffer, events []CalendarEvent, calErr error) {
	if calErr != nil {
		// Say the day is unknown rather than dropping the section. A silently
		// absent schedule is indistinguishable from an empty calendar.
		out.WriteString("\n🗓 Today: couldn't load your calendar.\n")
		return
	}

	timed, allDay := c.splitTodaysEvents(events)
	if len(timed) == 0 && len(allDay) == 0 {
		out.WriteString("\n🗓 Today: nothing on the calendar.\n")
		fmt.Fprintf(out, "%s\n", c.dayLoad(nil))
		return
	}

	renderSection(out, nudgeScheduleStyle, headingSchedule, c.orderedForDisplay(allDay, timed), c.eventLabel)
	fmt.Fprintf(out, "\n%s\n", c.dayLoad(timed))
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

// recordRelayMarkers rewrites the digest note's marker links after a relay:
// today's herald-session marker always, plus today's digest-sent marker when
// the digest just went out. Both markers live on the note for the same reason —
// the coordinator keeps no state of its own — and they are written in ONE
// read-and-patch: as two functions each doing its own read-modify-write, the
// second write was built from a read that predated the first, which clobbers
// the first marker the moment the reads stop interleaving kindly.
//
// Only today's marker of each kind is kept: the note carries one line of state
// per kind, not a growing log of every day the digest ever went out.
func (c *Coordinator) recordRelayMarkers(sessionID string, digestSent bool) error {
	note, err := c.noteboard.FindByTag(model.TypeNote, digestTag)
	if err != nil || note == nil {
		return err
	}
	today := c.Now.In(c.Location)
	links := []string{heraldSessionMarker(today, sessionID)}
	if digestSent {
		links = append(links, digestSentMarker(today))
	}
	for _, link := range note.Links {
		if strings.HasPrefix(link, "herald-session:") {
			continue
		}
		if digestSent && strings.HasPrefix(link, "digest-sent:") {
			// Replaced above. On a nag-only relay (digestSent false) the branch
			// is skipped, so a digest-sent marker from earlier today survives.
			continue
		}
		links = append(links, link)
	}
	return c.noteboard.Patch(note.ID, map[string]any{"links": links})
}

func digestSentMarker(day time.Time) string {
	return "digest-sent:" + day.Format("2006-01-02")
}

func heraldSessionMarker(day time.Time, sessionID string) string {
	return "herald-session:" + day.Format("2006-01-02") + ":" + sessionID
}

// todaysHeraldSessionID returns the bus session id of a herald session already
// opened today, or "" when today has not spoken yet (including before the
// digest note first exists).
func (c *Coordinator) todaysHeraldSessionID() (string, error) {
	note, err := c.noteboard.FindByTag(model.TypeNote, digestTag)
	if err != nil || note == nil {
		return "", err
	}
	prefix := "herald-session:" + c.Now.In(c.Location).Format("2006-01-02") + ":"
	for _, link := range note.Links {
		if strings.HasPrefix(link, prefix) {
			return strings.TrimPrefix(link, prefix), nil
		}
	}
	return "", nil
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

// civilDate reduces an instant to the calendar date it names, anchored in UTC so
// that two civil dates compare to each other and never to a wall clock. All-day
// calendar events carry no timezone — comparing their UTC-midnight bounds against
// a local midnight would land them a day out for any calendar east of UTC.
func civilDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// allDayEnd is an all-day event's EXCLUSIVE end date, matching Google's own
// convention. An event with no end.date at all is one day long.
func allDayEnd(event CalendarEvent) time.Time {
	if event.End.IsZero() {
		return civilDate(event.Start).AddDate(0, 0, 1)
	}
	return civilDate(event.End)
}

// sortByDue orders items soonest-due first and puts undated items last.
//
// Two undated items must compare EQUAL, not each less than the other. The
// earlier `return items[j].DueAt == nil` said true in both directions for a
// pair of undated items, which is not a strict weak ordering: SliceStable can
// only keep the input order of elements it is told are equal, so the stability
// it was chosen for was defeated for exactly the group that has nothing else to
// order it. The digest note is rewritten every fifteen minutes, so undated
// items changed places between runs with nothing changing underneath them.
func sortByDue(items []*model.Item) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].DueAt == nil || items[j].DueAt == nil {
			return items[i].DueAt != nil && items[j].DueAt == nil
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

// Relay carries the message to the user through `ask` and returns the bus
// session id `ask` prints. With reuseSessionID empty it opens a new herald
// session; otherwise it passes -session so the message lands as a new turn in
// that existing session. Either way the message ends in a question, so
// llm-bridge-server's turn-end heuristic parks the session as awaiting_user,
// which is what puts it in the dash "Needs you" list rather than letting it
// scroll away.
func (h *Herald) Relay(from, title, message, reuseSessionID string) (string, error) {
	arguments := []string{"-from", from}
	if reuseSessionID != "" {
		// -title names a session at creation; ask rejects it alongside -session.
		arguments = append(arguments, "-session", reuseSessionID)
	} else {
		arguments = append(arguments, "-title", title)
	}
	arguments = append(arguments, message)
	command := exec.Command(h.AskPath, arguments...)
	// Output, not CombinedOutput: stdout must be exactly the session id, and a
	// stray log line on stderr must not corrupt it. On failure the ExitError
	// carries the captured stderr.
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return "", fmt.Errorf("ask: %w — %s", err, strings.TrimSpace(string(exitError.Stderr)))
		}
		return "", fmt.Errorf("ask: %w", err)
	}
	sessionID := strings.TrimSpace(string(output))
	if sessionID == "" {
		return "", fmt.Errorf("ask exited 0 but printed no session id")
	}
	return sessionID, nil
}

// ------------------------------------------------------------------ noteboard

type NoteboardClient struct{ BaseURL string }

func (n *NoteboardClient) ListOpen(tag string) ([]*model.Item, error) {
	return n.list("/api/items?" + url.Values{
		"type": {"todo"}, "status": {"open"}, "tag": {tag}, "limit": {"500"},
	}.Encode())
}

// ListAllStatuses covers the calendar teardown path: an item that was completed
// or archived still has an event to take down.
func (n *NoteboardClient) ListAllStatuses(tag string) ([]*model.Item, error) {
	all := []*model.Item{}
	for _, status := range []string{"open", "done", "archived"} {
		items, err := n.list("/api/items?" + url.Values{
			"type": {"todo"}, "status": {status}, "tag": {tag}, "limit": {"500"},
		}.Encode())
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
	return n.list("/api/items?" + url.Values{
		"parent_id": {parentID}, "limit": {"500"},
	}.Encode())
}

func (n *NoteboardClient) FindByTag(itemType, tag string) (*model.Item, error) {
	items, err := n.list("/api/items?" + url.Values{
		"type": {itemType}, "tag": {tag}, "limit": {"10"},
	}.Encode())
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

// itemPath addresses one noteboard item, keeping the id inside a single path
// segment however it is spelled. The id is not ours: noteboard assigns it and the
// coordinator reads it back off a list answer, so nothing here constrains its
// shape. An id carrying "/" opens further segments and re-points the request at
// another noteboard route; "?" or "#" ends the path early, so the call addresses a
// DIFFERENT item and reports success.
//
// This one is a WRITE, which is what separates it from the read sites. A
// re-pointed PATCH does not return the wrong answer — it edits the wrong row, on a
// service with no auth at all, and answers 200. `~/CLAUDE.md` records that an
// UPDATE in noteboard is the one operation that can still destroy data, because
// its revision snapshot covers 8 of 20 columns and not `rank`, `due_at` or
// `schedule` — the very fields SetSchedule writes through here.
//
// PathEscape, not QueryEscape, for the same reason calendarEventPath gives: this
// is a path position, and QueryEscape writes a space as "+", a literal plus in a
// path segment. The query-string sites a few lines up correctly use url.Values
// instead, because PathEscape does not escape "&".
func itemPath(id string) string {
	return "/api/items/" + url.PathEscape(id)
}

func (n *NoteboardClient) Patch(id string, fields map[string]any) error {
	return n.do("PATCH", itemPath(id), fields, nil)
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

type GoogleCalendarMirror struct {
	DashURL string
	// RuntimeDirectory and SessionBusAddress are handed to `systemctl --user`
	// so it finds this user's manager from a job's environment.
	RuntimeDirectory  string
	SessionBusAddress string
}

// ResolveDashToken reads the token out of the running dash-server unit rather
// than keeping a copy. A rotated token is picked up on the next tick, and there
// is no second place for it to go stale.
func (g *GoogleCalendarMirror) ResolveDashToken() (string, error) {
	command := exec.Command("systemctl", "--user", "show", "dash-server.service", "-p", "Environment")
	// The child inherits this process's environment (command.Environ), with the
	// two variables it needs set to the values in force.
	command.Env = append(command.Environ(),
		"XDG_RUNTIME_DIR="+g.RuntimeDirectory,
		"DBUS_SESSION_BUS_ADDRESS="+g.SessionBusAddress,
	)
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("read dash-server.service environment: %w", err)
	}
	return dashTokenFromSystemdEnvironmentProperty(string(output))
}

// dashTokenFromSystemdEnvironmentProperty pulls DASH_TOKEN out of what
// `systemctl show -p Environment` prints. That is one line per property, and the
// whole space-separated variable list is the VALUE of a single `Environment=`
// property:
//
//	Environment=DASH_PORT=8101 DASH_TOKEN=abc HOME=/home/…
//
// So the first field is `Environment=DASH_PORT=8101`, not `DASH_PORT=8101`.
// Splitting on whitespace and asking each field for a `DASH_TOKEN=` prefix
// cannot see the token when it is the FIRST variable in the unit — that one
// field is `Environment=DASH_TOKEN=abc`. It works on this host only because
// DASH_PORT happens to be declared first, which nothing guarantees: reorder the
// unit file and every calendar read and write stops, reporting a unit that
// "declares no DASH_TOKEN" while the unit plainly declares one. Strip the
// property name first, then split.
func dashTokenFromSystemdEnvironmentProperty(output string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		value, isEnvironment := strings.CutPrefix(strings.TrimSpace(line), "Environment=")
		if !isEnvironment {
			continue
		}
		for _, field := range strings.Fields(value) {
			if token, found := strings.CutPrefix(field, "DASH_TOKEN="); found {
				return token, nil
			}
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
	// The proxy answers 200 with a populated `error` field when it could not read
	// one of the user's calendars, and still returns the events that did arrive
	// (dash 05fc2b6; before it, the multi-calendar branch this client takes
	// dropped the error). So a set error means the list is PARTIAL, not empty.
	// Treat it as loud either way: a partial day printed as the whole day is a
	// false statement in the note the user reads before getting up.
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
				// Summary, dateTime and the parser's own message are all
				// unbounded: they arrive from Google via the dash proxy, and
				// time.Parse quotes its whole input back twice. This error
				// reaches the tick's log, which scheduler run history keeps.
				// %w becomes %v because a bounded string is no longer the
				// error — nothing unwraps this, and the alternative is to keep
				// the whole 15 KB so that errors.Is could work.
				return nil, fmt.Errorf("event %q start %q: %v",
					boundedtext.Value(e.Summary), boundedtext.Value(e.Start.DateTime), boundedtext.ErrorMessage(err))
			}
			event.Start = start
			if e.End.DateTime != "" {
				end, err := time.Parse(time.RFC3339, e.End.DateTime)
				if err != nil {
					return nil, fmt.Errorf("event %q end %q: %v",
						boundedtext.Value(e.Summary), boundedtext.Value(e.End.DateTime), boundedtext.ErrorMessage(err))
				}
				event.End = end
			}
		case e.Start.Date != "":
			// An all-day event is a range of civil dates, not an instant, so both
			// bounds parse to UTC midnight and are only ever compared against other
			// civil dates (see civilDate). Google's end.date is EXCLUSIVE: a
			// single-day event on the 8th comes back as 08-08 -> 08-09.
			event.AllDay = true
			day, err := time.Parse("2006-01-02", e.Start.Date)
			if err != nil {
				return nil, fmt.Errorf("event %q date %q: %v",
					boundedtext.Value(e.Summary), boundedtext.Value(e.Start.Date), boundedtext.ErrorMessage(err))
			}
			event.Start = day
			if e.End.Date != "" {
				end, err := time.Parse("2006-01-02", e.End.Date)
				if err != nil {
					return nil, fmt.Errorf("event %q end date %q: %v",
						boundedtext.Value(e.Summary), boundedtext.Value(e.End.Date), boundedtext.ErrorMessage(err))
				}
				event.End = end
			}
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

// calendarEventPath addresses one event, keeping the id inside a single path
// segment however it is spelled. The id is not ours: splitCalendarLinks trims it
// off an entry in a noteboard item's `links` array, and noteboard constrains
// nothing in that array. An id carrying "/" opens further segments and re-points
// the request at another dash route; "?" or "#" ends the path early, so the call
// addresses a DIFFERENT event and reports success. Both requests carry the
// DASH_TOKEN, so a re-pointed path is an authenticated call the coordinator never
// meant to make.
//
// PathEscape, not QueryEscape: this is a path segment, and QueryEscape would write
// a space as "+", which is a literal plus in a path. The converse also holds —
// PathEscape does not escape "&", which is why the query-string sites a few
// hundred lines up use url.Values instead.
func calendarEventPath(eventID string) string {
	return "/api/calendar/google/" + url.PathEscape(eventID)
}

func (g *GoogleCalendarMirror) UpdateEvent(token, eventID string, item *model.Item) error {
	return g.do(token, "PUT", calendarEventPath(eventID), eventPayload(item), nil)
}

func (g *GoogleCalendarMirror) DeleteEvent(token, eventID string) error {
	err := g.do(token, "DELETE", calendarEventPath(eventID), nil, nil)
	// Google leaves a `cancelled` tombstone, and re-deleting one returns 410; an
	// event Google has never heard of returns 404. Either way the state we wanted
	// is the state we have, so both are success.
	//
	// Read off the status CODE, never the message text. The message carries the
	// request path and the path carries the event id — see calendarHTTPError.
	var httpErr *calendarHTTPError
	if errors.As(err, &httpErr) &&
		(httpErr.StatusCode == http.StatusGone || httpErr.StatusCode == http.StatusNotFound) {
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

// calendarHTTPError is a non-2xx answer from the dash calendar proxy, carrying the
// status as a NUMBER as well as in its message.
//
// The number is the point. The only caller that classifies one is DeleteEvent,
// which treats "the event is already gone" as success — and it used to decide
// that by looking for "410" or "404" as a SUBSTRING of the formatted message.
// That message also carries the request path, and the path carries the caller's
// event id. A Google event id is base32hex, so "6f8k404m2p1q9r" is an ordinary
// id and not a contrived one: any failure at all on that event — a 500 from
// Google, a 401 from a rotated DASH_TOKEN, a 503 from the proxy — matched, and
// the mirror reported the delete as done while the event stayed on the calendar.
type calendarHTTPError struct {
	Method     string
	Path       string
	Status     string
	StatusCode int
	Body       string
}

func (e *calendarHTTPError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %s — %s", e.Method, e.Path, e.Status, e.Body)
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
		return &calendarHTTPError{
			Method:     method,
			Path:       path,
			Status:     response.Status,
			StatusCode: response.StatusCode,
			Body:       strings.TrimSpace(string(raw)),
		}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
