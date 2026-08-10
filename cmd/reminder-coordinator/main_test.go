package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/noteboard/model"
)

// ---------------------------------------------------------------- fakes

type fakeNoteboard struct {
	items    []*model.Item
	created  []*model.CreateItemRequest
	patched  map[string]map[string]any
	schedule map[string]*model.Schedule
}

func newFakeNoteboard(items ...*model.Item) *fakeNoteboard {
	return &fakeNoteboard{
		items:    items,
		patched:  map[string]map[string]any{},
		schedule: map[string]*model.Schedule{},
	}
}

func (f *fakeNoteboard) ListOpen(string) ([]*model.Item, error) {
	open := []*model.Item{}
	for _, item := range f.items {
		if item.Status == "open" {
			open = append(open, item)
		}
	}
	return open, nil
}
func (f *fakeNoteboard) ListAllStatuses(string) ([]*model.Item, error) { return f.items, nil }
func (f *fakeNoteboard) ListScheduled(string) ([]*model.Item, error) {
	scheduled := []*model.Item{}
	for _, item := range f.items {
		if item.Schedule != nil {
			scheduled = append(scheduled, item)
		}
	}
	return scheduled, nil
}
func (f *fakeNoteboard) ListChildren(parentID string) ([]*model.Item, error) {
	children := []*model.Item{}
	for _, item := range f.items {
		if item.ParentID != nil && *item.ParentID == parentID {
			children = append(children, item)
		}
	}
	return children, nil
}
func (f *fakeNoteboard) FindByTag(itemType, tag string) (*model.Item, error) {
	for _, item := range f.items {
		if item.Type != itemType {
			continue
		}
		for _, t := range item.Tags {
			if t == tag {
				return item, nil
			}
		}
	}
	return nil, nil
}
func (f *fakeNoteboard) CreateItem(req *model.CreateItemRequest) (*model.Item, error) {
	f.created = append(f.created, req)
	created := &model.Item{
		ID: "created-" + req.Title, Type: req.Type, Title: req.Title,
		Tags: req.Tags, DueAt: req.DueAt, ParentID: req.ParentID, Status: "open",
	}
	f.items = append(f.items, created)
	return created, nil
}

// Patch MERGES, because a real noteboard PATCH merges the fields it is given
// into the row and leaves the rest alone. Replacing instead meant the last patch
// of a tick silently erased every earlier one: a full Run() writes the digest
// body and then stamps the sent-marker on the same note, so the body vanished
// from the record and a test asserting on it saw an empty string. That is a fake
// disagreeing with the service it stands in for, which reads as a coverage hole
// in the code under test.
func (f *fakeNoteboard) Patch(id string, fields map[string]any) error {
	merged := f.patched[id]
	if merged == nil {
		merged = map[string]any{}
		f.patched[id] = merged
	}
	for key, value := range fields {
		merged[key] = value
	}
	return nil
}
func (f *fakeNoteboard) SetSchedule(id string, schedule *model.Schedule, dryRun bool) error {
	if dryRun {
		return nil
	}
	f.schedule[id] = schedule
	for _, item := range f.items {
		if item.ID == id {
			item.Schedule = schedule
		}
	}
	return nil
}

type fakeCalendar struct {
	created, updated, deleted int
	events                    []CalendarEvent
	listErr                   error
}

func (f *fakeCalendar) ResolveDashToken() (string, error) { return "test-token", nil }
func (f *fakeCalendar) ListEvents(string, time.Time, time.Time) ([]CalendarEvent, error) {
	return f.events, f.listErr
}
func (f *fakeCalendar) CreateEvent(string, *model.Item) (string, error) {
	f.created++
	return "event-id", nil
}
func (f *fakeCalendar) UpdateEvent(string, string, *model.Item) error { f.updated++; return nil }
func (f *fakeCalendar) DeleteEvent(string, string) error              { f.deleted++; return nil }

type fakeHerald struct{ sessions []string }

func (f *fakeHerald) Relay(from, title, message string) error {
	f.sessions = append(f.sessions, message)
	return nil
}

func pacific(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}

func newTestCoordinator(t *testing.T, now time.Time, nb *fakeNoteboard) (*Coordinator, *fakeHerald, *fakeCalendar) {
	t.Helper()
	herald := &fakeHerald{}
	calendar := &fakeCalendar{}
	return &Coordinator{
		noteboard: nb,
		calendar:  calendar,
		herald:    herald,
		ScopeTag:  "personal",
		Location:  pacific(t),
		DigestAt:  dailyTime{Hour: 7, Minute: 30},
		Lookahead: 7 * 24 * time.Hour,
		Now:       now,
	}, herald, calendar
}

func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------- the safety property

// The single most dangerous thing in this program. The autoworker dispatches a
// Claude agent at any open todo that lacks the `personal` and `autoworker` tags.
// A materialized child that did not inherit its parent's tags would therefore be
// picked up as coding work — an agent would be handed "water the plants" and told
// to go do it.
func TestMaterializedChildInheritsTagsSoTheAutoworkerNeverPicksItUp(t *testing.T) {
	loc := pacific(t)
	anchor := time.Date(2026, 7, 21, 9, 0, 0, 0, loc)
	template := &model.Item{
		ID: "template", Type: "todo", Title: "Water the plants", Status: "open",
		Tags: []string{"personal", "home"},
		Schedule: &model.Schedule{
			DTStart: &anchor, TZID: "America/Los_Angeles",
			RRule: "FREQ=MONTHLY;BYDAY=3TU", Mode: model.ScheduleModeInstance,
		},
	}

	nb := newFakeNoteboard(template)
	// A tick just after the first occurrence.
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 21, 10, 0, 0, 0, loc), nb)

	if err := coordinator.materializeDueOccurrences(); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(nb.created) != 1 {
		t.Fatalf("created %d children, want 1", len(nb.created))
	}

	child := nb.created[0]
	hasPersonal := false
	for _, tag := range child.Tags {
		if tag == "personal" {
			hasPersonal = true
		}
	}
	if !hasPersonal {
		t.Fatalf("child tags = %v — WITHOUT `personal` the autoworker will dispatch an agent at this chore", child.Tags)
	}
	if len(child.Tags) != 2 || child.Tags[1] != "home" {
		t.Errorf("child tags = %v, want the parent's [personal home]", child.Tags)
	}

	// The child must NOT carry the rule, or it would start spawning children of
	// its own and the series would fan out exponentially.
	if child.Schedule != nil {
		t.Error("materialized child inherited the recurrence rule; it would spawn its own children")
	}
	if child.ParentID == nil || *child.ParentID != "template" {
		t.Errorf("child ParentID = %v, want the template", child.ParentID)
	}
	if child.DueAt == nil || !child.DueAt.Equal(anchor) {
		t.Errorf("child DueAt = %v, want the occurrence %s", child.DueAt, anchor)
	}
}

// The coordinator ticks every 15 minutes. Without idempotence a template whose
// occurrence has passed spawns a fresh child on EVERY tick — 96 copies of the
// same chore per day.
func TestMaterializationIsIdempotentAcrossTicks(t *testing.T) {
	loc := pacific(t)
	anchor := time.Date(2026, 7, 21, 9, 0, 0, 0, loc)
	template := &model.Item{
		ID: "template", Type: "todo", Title: "Water the plants", Status: "open",
		Tags: []string{"personal"},
		Schedule: &model.Schedule{
			DTStart: &anchor, TZID: "America/Los_Angeles",
			RRule: "FREQ=MONTHLY;BYDAY=3TU", Mode: model.ScheduleModeInstance,
		},
	}
	nb := newFakeNoteboard(template)

	// Twelve ticks across three hours, exactly as the scheduler would.
	for tick := 0; tick < 12; tick++ {
		now := time.Date(2026, 7, 21, 9, 15, 0, 0, loc).Add(time.Duration(tick) * 15 * time.Minute)
		coordinator, _, _ := newTestCoordinator(t, now, nb)
		if err := coordinator.materializeDueOccurrences(); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
	}

	if len(nb.created) != 1 {
		t.Fatalf("12 ticks produced %d children, want exactly 1 — the chore is being duplicated every tick", len(nb.created))
	}
	if nb.schedule["template"] == nil || nb.schedule["template"].LastMaterializedAt == nil {
		t.Fatal("last_materialized_at was never advanced; nothing stops the next tick re-creating it")
	}
}

// A template is a series, not a piece of work. Listing it in the digest would show
// "water the plants" as permanently due, next to the actual instance that is due.
func TestRecurringTemplateIsNotShownAsWork(t *testing.T) {
	loc := pacific(t)
	anchor := time.Date(2026, 7, 21, 9, 0, 0, 0, loc)
	template := &model.Item{
		ID: "template", Type: "todo", Title: "Water the plants", Status: "open",
		Tags: []string{"personal"}, DueAt: &anchor,
		Schedule: &model.Schedule{
			DTStart: &anchor, TZID: "America/Los_Angeles",
			RRule: "FREQ=MONTHLY;BYDAY=3TU", Mode: model.ScheduleModeInstance,
		},
	}
	occurrence := &model.Item{
		ID: "occurrence", Type: "todo", Title: "Water the plants", Status: "open",
		Tags: []string{"personal"}, DueAt: &anchor, ParentID: ptr("template"),
	}

	nb := newFakeNoteboard(template, occurrence)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 21, 10, 0, 0, 0, loc), nb)

	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}

	total := len(board.Overdue) + len(board.DueToday) + len(board.ThisWeek) + len(board.Undated)
	if total != 1 {
		t.Fatalf("board holds %d items, want 1 — the template is being listed as work alongside its own occurrence", total)
	}
	if len(board.DueToday) != 1 || board.DueToday[0].ID != "occurrence" {
		t.Fatalf("board should hold the occurrence, not the template; got %+v", board.DueToday)
	}
}

// ---------------------------------------------------------------- the original bug

// The bug this whole change exists to kill: one todo, two nudgers, two chat
// sessions. Here the same overdue item is BOTH in the daily digest and carrying
// its own nag rule — the exact shape of the car-title todo, which was listed by
// personal-todo-digest and separately nudged by car-title-nudge every weekday.
//
// It must produce exactly ONE herald session.
func TestAnOverdueItemWithItsOwnNagStillOpensOnlyOneSession(t *testing.T) {
	loc := pacific(t)
	due := time.Date(2026, 6, 30, 9, 0, 0, 0, loc)

	carTitle := &model.Item{
		ID: "car-title", Type: "todo", Title: "Call the tag agency", Status: "open",
		Tags: []string{"personal", "car-title"}, DueAt: &due, Priority: 3,
		Schedule: &model.Schedule{
			TZID:   "America/Los_Angeles",
			Remind: &model.Remind{Nag: "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;BYHOUR=10;BYMINUTE=0;BYSECOND=0"},
		},
	}
	digestNote := &model.Item{
		ID: "digest", Type: "note", Title: digestTitle, Tags: []string{"personal", digestTag},
	}

	nb := newFakeNoteboard(carTitle, digestNote)
	// A Monday at 10:30am: past the 7:30 digest hour AND past the 10am nag.
	now := time.Date(2026, 7, 13, 10, 30, 0, 0, loc)
	coordinator, herald, _ := newTestCoordinator(t, now, nb)

	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}
	if len(board.Overdue) != 1 {
		t.Fatalf("want the item overdue, got %d overdue", len(board.Overdue))
	}
	if len(board.NagFiring) != 1 {
		t.Fatalf("want its nag firing, got %d firing", len(board.NagFiring))
	}

	if err := coordinator.nudgeOnce(board); err != nil {
		t.Fatalf("nudgeOnce: %v", err)
	}

	// Both the digest AND the nag had something to say about the same todo. Under
	// the old design that was two jobs and two sessions.
	if len(herald.sessions) != 1 {
		t.Fatalf("opened %d herald sessions, want exactly 1 — this is the double-nudge bug", len(herald.sessions))
	}
}

// A nag fires at 10am. The coordinator ticks at 10:00, 10:15, 10:30... If the
// firing is not recorded, the user gets nagged on every one of them.
func TestANagFiresOnceNotOnEveryTick(t *testing.T) {
	loc := pacific(t)
	due := time.Date(2026, 6, 30, 9, 0, 0, 0, loc)
	item := &model.Item{
		ID: "nagged", Type: "todo", Title: "Call the tag agency", Status: "open",
		Tags: []string{"personal"}, DueAt: &due,
		Schedule: &model.Schedule{
			TZID:   "America/Los_Angeles",
			Remind: &model.Remind{Nag: "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;BYHOUR=10;BYMINUTE=0;BYSECOND=0"},
		},
	}
	digestNote := &model.Item{
		ID: "digest", Type: "note", Title: digestTitle, Tags: []string{"personal", digestTag},
		// Already sent today, so only the nag can speak.
		Links: []string{"digest-sent:2026-07-13"},
	}
	nb := newFakeNoteboard(item, digestNote)

	// Eight ticks through the two hours after the 10am nag.
	sessions := 0
	for tick := 0; tick < 8; tick++ {
		now := time.Date(2026, 7, 13, 10, 0, 0, 0, loc).Add(time.Duration(tick) * 15 * time.Minute)
		coordinator, herald, _ := newTestCoordinator(t, now, nb)
		board, err := coordinator.buildBoard()
		if err != nil {
			t.Fatalf("tick %d buildBoard: %v", tick, err)
		}
		if err := coordinator.nudgeOnce(board); err != nil {
			t.Fatalf("tick %d nudgeOnce: %v", tick, err)
		}
		sessions += len(herald.sessions)
	}

	if sessions != 1 {
		t.Fatalf("8 ticks after one 10am firing opened %d sessions, want 1 — last_nagged_at is not holding", sessions)
	}
}

// A nag is "bother me until it's done". Done means done.
func TestANagStopsWhenTheItemIsDone(t *testing.T) {
	loc := pacific(t)
	due := time.Date(2026, 6, 30, 9, 0, 0, 0, loc)
	item := &model.Item{
		ID: "finished", Type: "todo", Title: "Call the tag agency", Status: "done",
		Tags: []string{"personal"}, DueAt: &due,
		Schedule: &model.Schedule{
			TZID:   "America/Los_Angeles",
			Remind: &model.Remind{Nag: "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;BYHOUR=10"},
		},
	}
	nb := newFakeNoteboard(item)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 13, 10, 30, 0, 0, loc), nb)

	firing, err := coordinator.nagsFiringNow()
	if err != nil {
		t.Fatalf("nagsFiringNow: %v", err)
	}
	if len(firing) != 0 {
		t.Fatal("a completed item is still nagging")
	}
}

// The digest nudge goes out once a day, not on all 96 ticks after 7:30am.
func TestTheDailyDigestNudgeIsSentOncePerDay(t *testing.T) {
	loc := pacific(t)
	undated := &model.Item{
		ID: "todo", Type: "todo", Title: "something", Status: "open",
		Tags: []string{"personal"}, DueAt: ptr(time.Date(2026, 7, 13, 9, 0, 0, 0, loc)),
	}
	digestNote := &model.Item{
		ID: "digest", Type: "note", Title: digestTitle, Tags: []string{"personal", digestTag},
	}
	nb := newFakeNoteboard(undated, digestNote)

	sessions := 0
	for tick := 0; tick < 10; tick++ {
		now := time.Date(2026, 7, 13, 7, 30, 0, 0, loc).Add(time.Duration(tick) * 15 * time.Minute)
		coordinator, herald, _ := newTestCoordinator(t, now, nb)
		board, err := coordinator.buildBoard()
		if err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		if err := coordinator.nudgeOnce(board); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		sessions += len(herald.sessions)
		// The marker the real noteboard would now hold.
		if fields, ok := nb.patched["digest"]; ok {
			if links, ok := fields["links"].([]string); ok {
				digestNote.Links = links
			}
		}
	}

	if sessions != 1 {
		t.Fatalf("10 ticks after the digest hour sent %d nudges, want 1", sessions)
	}
}

// Before the digest hour, nothing is said. A reminder system that pings at 03:00
// gets muted, and then it reminds you of nothing at all.
func TestNothingIsSentBeforeTheDigestHour(t *testing.T) {
	loc := pacific(t)
	digestNote := &model.Item{
		ID: "digest", Type: "note", Title: digestTitle, Tags: []string{"personal", digestTag},
	}
	overdue := &model.Item{
		ID: "todo", Type: "todo", Title: "overdue thing", Status: "open",
		Tags: []string{"personal"}, DueAt: ptr(time.Date(2026, 7, 1, 9, 0, 0, 0, loc)),
	}
	nb := newFakeNoteboard(overdue, digestNote)

	coordinator, herald, _ := newTestCoordinator(t, time.Date(2026, 7, 13, 3, 0, 0, 0, loc), nb)
	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}
	if err := coordinator.nudgeOnce(board); err != nil {
		t.Fatalf("nudgeOnce: %v", err)
	}
	if len(herald.sessions) != 0 {
		t.Fatal("nudged at 3am, before the digest hour")
	}
}

// ---------------------------------------------------------------- rolling mode

func TestRollingItemAdvancesToItsNextOccurrenceWhenDone(t *testing.T) {
	loc := pacific(t)
	anchor := time.Date(2026, 7, 21, 9, 0, 0, 0, loc)
	item := &model.Item{
		ID: "rolling", Type: "todo", Title: "Renew registration", Status: "done",
		Tags: []string{"personal"}, DueAt: &anchor,
		Schedule: &model.Schedule{
			DTStart: &anchor, TZID: "America/Los_Angeles",
			RRule: "FREQ=MONTHLY;BYDAY=3TU", Mode: model.ScheduleModeRolling,
		},
	}
	nb := newFakeNoteboard(item)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 21, 10, 0, 0, 0, loc), nb)

	if err := coordinator.rollCompletedRollingItems(); err != nil {
		t.Fatalf("roll: %v", err)
	}

	fields, ok := nb.patched["rolling"]
	if !ok {
		t.Fatal("a completed rolling item was not advanced")
	}
	if fields["status"] != "open" {
		t.Errorf("status = %v, want reopened", fields["status"])
	}
	want := time.Date(2026, 8, 18, 9, 0, 0, 0, loc).Format(time.RFC3339)
	if fields["due_at"] != want {
		t.Errorf("due_at = %v, want the next 3rd Tuesday %s", fields["due_at"], want)
	}

	// Rolling mode is one row. It must not also spawn children.
	if len(nb.created) != 0 {
		t.Errorf("rolling item created %d children; rolling mode is a single row", len(nb.created))
	}
}

// A finite series has to be able to retire. Otherwise "renew this 3 times" nags
// forever.
func TestExhaustedRollingSeriesStaysDone(t *testing.T) {
	loc := pacific(t)
	anchor := time.Date(2026, 7, 21, 9, 0, 0, 0, loc)
	item := &model.Item{
		ID: "rolling", Type: "todo", Title: "Renew registration", Status: "done",
		Tags: []string{"personal"}, DueAt: &anchor,
		Schedule: &model.Schedule{
			DTStart: &anchor, TZID: "America/Los_Angeles",
			RRule: "FREQ=MONTHLY;BYDAY=3TU;COUNT=1", Mode: model.ScheduleModeRolling,
		},
	}
	nb := newFakeNoteboard(item)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 22, 10, 0, 0, 0, loc), nb)

	if err := coordinator.rollCompletedRollingItems(); err != nil {
		t.Fatalf("roll: %v", err)
	}
	if _, patched := nb.patched["rolling"]; patched {
		t.Fatal("an exhausted series was reopened; it should retire itself")
	}
}

// ---------------------------------------------------------------- calendar

// The calendar is opt-in. The user asked for it explicitly: a possibility, not
// the main route. A calendar carrying every todo stops working as a calendar.
func TestCalendarIsOptInPerItem(t *testing.T) {
	loc := pacific(t)
	soon := time.Date(2026, 7, 20, 9, 0, 0, 0, loc)

	optedIn := &model.Item{
		ID: "opted-in", Type: "todo", Title: "Dentist", Status: "open",
		Tags: []string{"personal"}, DueAt: &soon,
		Schedule: &model.Schedule{
			TZID:   "America/Los_Angeles",
			Remind: &model.Remind{Channels: []string{model.ChannelDigest, model.ChannelCalendar}},
		},
	}
	notOptedIn := &model.Item{
		ID: "digest-only", Type: "todo", Title: "Buy milk", Status: "open",
		Tags: []string{"personal"}, DueAt: &soon,
	}

	nb := newFakeNoteboard(optedIn, notOptedIn)
	coordinator, _, calendar := newTestCoordinator(t, time.Date(2026, 7, 13, 10, 0, 0, 0, loc), nb)

	if err := coordinator.syncCalendar(); err != nil {
		t.Fatalf("syncCalendar: %v", err)
	}
	if calendar.created != 1 {
		t.Fatalf("created %d events, want 1 — only the opted-in item should reach the calendar", calendar.created)
	}
	if _, patched := nb.patched["digest-only"]; patched {
		t.Error("an item that never asked for a calendar event was given one")
	}
}

// An item that had an event and no longer wants one must have it taken down, or
// the calendar accumulates events for todos that are done.
func TestCompletedItemHasItsCalendarEventRemoved(t *testing.T) {
	loc := pacific(t)
	soon := time.Date(2026, 7, 20, 9, 0, 0, 0, loc)
	done := &model.Item{
		ID: "done", Type: "todo", Title: "Dentist", Status: "done",
		Tags: []string{"personal"}, DueAt: &soon,
		Links: []string{"gcal:event-123", "gcalsync:" + soon.Format(time.RFC3339)},
		Schedule: &model.Schedule{
			TZID:   "America/Los_Angeles",
			Remind: &model.Remind{Channels: []string{model.ChannelCalendar}},
		},
	}
	nb := newFakeNoteboard(done)
	coordinator, _, calendar := newTestCoordinator(t, time.Date(2026, 7, 13, 10, 0, 0, 0, loc), nb)

	if err := coordinator.syncCalendar(); err != nil {
		t.Fatalf("syncCalendar: %v", err)
	}
	if calendar.deleted != 1 {
		t.Fatalf("deleted %d events, want 1 — a completed todo left its event on the calendar", calendar.deleted)
	}
	links, ok := nb.patched["done"]["links"].([]string)
	if !ok {
		t.Fatal("the gcal link was not cleared from the item")
	}
	for _, link := range links {
		if link == "gcal:event-123" {
			t.Fatal("the item still points at a deleted event")
		}
	}
}

// ---------------------------------------------------------------- rendering

func TestDigestSeparatesOverdueFromDueToday(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, loc)

	nb := newFakeNoteboard(
		&model.Item{ID: "a", Type: "todo", Title: "Overdue thing", Status: "open",
			Tags: []string{"personal", "car"}, Priority: 3,
			DueAt: ptr(time.Date(2026, 6, 30, 9, 0, 0, 0, loc))},
		&model.Item{ID: "b", Type: "todo", Title: "Today thing", Status: "open",
			Tags: []string{"personal"}, DueAt: ptr(time.Date(2026, 7, 13, 15, 0, 0, 0, loc))},
		&model.Item{ID: "c", Type: "todo", Title: "Someday thing", Status: "open",
			Tags: []string{"personal"}},
	)
	coordinator, _, _ := newTestCoordinator(t, now, nb)

	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}
	if len(board.Overdue) != 1 || len(board.DueToday) != 1 || len(board.Undated) != 1 {
		t.Fatalf("buckets = overdue %d / today %d / undated %d, want 1/1/1",
			len(board.Overdue), len(board.DueToday), len(board.Undated))
	}

	rendered := coordinator.renderDigest(board, nil, nil)
	for _, want := range []string{"Overdue", "Due today", "No due date", "❗Overdue thing", "`car`"} {
		if !contains(rendered, want) {
			t.Errorf("digest is missing %q:\n%s", want, rendered)
		}
	}
}

// The buckets must be TOTAL. This shipped broken: anything dated beyond the
// 7-day horizon landed in no bucket and vanished from the board — the real
// "Apply to jobs" todo, due exactly 7 days out, simply was not there. A board
// that omits a todo you still have is worse than no board, because it reads as
// "this is everything".
func TestNoOpenTodoCanFallThroughTheBuckets(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, loc)

	items := []*model.Item{
		{ID: "overdue", Type: "todo", Title: "overdue", Status: "open", Tags: []string{"personal"},
			DueAt: ptr(time.Date(2026, 6, 30, 9, 0, 0, 0, loc))},
		{ID: "today", Type: "todo", Title: "today", Status: "open", Tags: []string{"personal"},
			DueAt: ptr(time.Date(2026, 7, 13, 15, 0, 0, 0, loc))},
		{ID: "thisweek", Type: "todo", Title: "this week", Status: "open", Tags: []string{"personal"},
			DueAt: ptr(time.Date(2026, 7, 16, 9, 0, 0, 0, loc))},
		// Exactly on the 7-day horizon — the one that vanished.
		{ID: "horizon", Type: "todo", Title: "on the horizon", Status: "open", Tags: []string{"personal"},
			DueAt: ptr(time.Date(2026, 7, 20, 9, 0, 0, 0, loc))},
		// Far future.
		{ID: "faraway", Type: "todo", Title: "far away", Status: "open", Tags: []string{"personal"},
			DueAt: ptr(time.Date(2027, 3, 1, 9, 0, 0, 0, loc))},
		{ID: "undated", Type: "todo", Title: "undated", Status: "open", Tags: []string{"personal"}},
	}

	coordinator, _, _ := newTestCoordinator(t, now, newFakeNoteboard(items...))
	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}

	placed := map[string]bool{}
	for _, item := range board.All() {
		placed[item.ID] = true
	}
	for _, item := range items {
		if !placed[item.ID] {
			t.Errorf("open todo %q (due %v) landed in NO bucket — it is invisible on the board", item.Title, item.DueAt)
		}
	}
	if len(board.All()) != len(items) {
		t.Fatalf("board holds %d of %d open todos", len(board.All()), len(items))
	}

	// And it must actually be rendered, not merely bucketed.
	rendered := coordinator.renderDigest(board, nil, nil)
	for _, item := range items {
		if !contains(rendered, item.Title) {
			t.Errorf("todo %q is bucketed but missing from the rendered note:\n%s", item.Title, rendered)
		}
	}
}

// The whole reason calendar events are in the digest: seeing the day's shape and
// whether there is room for a long task. Events must be listed in start order and
// the day-load line must appear.
func TestDigestShowsTodaysScheduleInOrderWithDayLoad(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 7, 20, 8, 0, 0, 0, loc)
	nb := newFakeNoteboard(
		&model.Item{ID: "t", Type: "todo", Title: "A todo", Status: "open",
			Tags: []string{"personal"}, DueAt: ptr(time.Date(2026, 7, 20, 15, 0, 0, 0, loc))},
	)
	coordinator, _, _ := newTestCoordinator(t, now, nb)
	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}

	events := []CalendarEvent{
		{Summary: "Salsa", Start: time.Date(2026, 7, 20, 20, 0, 0, 0, loc), End: time.Date(2026, 7, 20, 21, 0, 0, 0, loc)},
		{Summary: "Ice Skating", Start: time.Date(2026, 7, 20, 6, 0, 0, 0, loc), End: time.Date(2026, 7, 20, 7, 0, 0, 0, loc)},
	}
	rendered := coordinator.renderDigest(board, events, nil)

	for _, want := range []string{"Today's schedule", "Salsa", "Ice Skating", "8pm", "booked", "free"} {
		if !contains(rendered, want) {
			t.Errorf("digest is missing %q:\n%s", want, rendered)
		}
	}
	// The 6am event must render before the 8pm one, not in input order.
	if strings.Index(rendered, "Ice Skating") > strings.Index(rendered, "Salsa") {
		t.Errorf("schedule is not ordered by start time:\n%s", rendered)
	}
	// Two 1-hour events in a 14-hour window leave a long open stretch — green.
	if !contains(rendered, "🟢") {
		t.Errorf("a nearly-empty day should read as wide open:\n%s", rendered)
	}
}

// A day wall-to-wall with meetings must warn that there is no long open stretch,
// even if the raw free hours look plentiful.
func TestDayLoadFlagsAFragmentedDay(t *testing.T) {
	loc := pacific(t)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 20, 8, 0, 0, 0, loc), newFakeNoteboard())
	// Hourly meetings every other hour from 9 to 9 — free hours exist but never
	// more than one at a stretch.
	events := []CalendarEvent{}
	for h := 9; h <= 21; h += 2 {
		events = append(events, CalendarEvent{
			Summary: "Meeting",
			Start:   time.Date(2026, 7, 20, h, 0, 0, 0, loc),
			End:     time.Date(2026, 7, 20, h+1, 30, 0, 0, loc),
		})
	}
	line := coordinator.dayLoad(events)
	if !strings.Contains(line, "🔴") {
		t.Errorf("a day with no >=90m gap should be flagged red, got: %s", line)
	}
}

// A calendar read failure must degrade to a visible note, never take down the
// digest — the reminders matter more than the schedule annotation.
func TestDigestSurvivesCalendarReadFailure(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 7, 20, 8, 0, 0, 0, loc)
	nb := newFakeNoteboard(
		&model.Item{ID: "t", Type: "todo", Title: "Still here", Status: "open",
			Tags: []string{"personal"}, DueAt: ptr(time.Date(2026, 7, 20, 15, 0, 0, 0, loc))},
	)
	coordinator, _, _ := newTestCoordinator(t, now, nb)
	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}

	rendered := coordinator.renderDigest(board, nil, fmt.Errorf("google timeout"))
	if !contains(rendered, "Couldn't load your calendar") {
		t.Errorf("a calendar failure should be surfaced in the digest:\n%s", rendered)
	}
	if !contains(rendered, "Still here") {
		t.Errorf("the todos must still render when the calendar read fails:\n%s", rendered)
	}
}

// ------------------------------------------------- what sabotage scoring found
//
// Everything below was written because a mutation of the mechanism it names came
// back UNNOTICED under scripts/sabotage-reminder-coordinator.py. Three of them
// sat behind a test whose NAME already claimed the mechanism; the assertion was
// being satisfied by something else in the fixture. Run the scorer, not the test
// names.

// The digest tag is the only thing tying this program to the note the user
// actually reads. Every other test seeds its fixture with the digestTag CONSTANT,
// so drifting that constant moves the fixture and the production code together
// and no assertion can ever separate them — the gate reproduced in the fixture.
// This test writes the wire literal instead, which is the only way the value is
// pinned at all.
func TestTheDigestNoteIsFoundByTheLiteralTagWrittenOnIt(t *testing.T) {
	loc := pacific(t)
	digestNote := &model.Item{
		ID: "digest", Type: "note", Title: "📆 Today — personal",
		Tags: []string{"personal", "reminder-digest"},
	}
	nb := newFakeNoteboard(digestNote)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 13, 9, 0, 0, 0, loc), nb)

	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}
	if err := coordinator.writeDigestNote(board, nil, nil); err != nil {
		t.Fatalf("writeDigestNote: %v", err)
	}
	if len(nb.created) != 0 {
		t.Fatalf("created %d new notes; the standing note tagged \"reminder-digest\" was not found, "+
			"so every tick would leave another duplicate behind", len(nb.created))
	}
	if _, patched := nb.patched["digest"]; !patched {
		t.Fatal("the standing note was never rewritten")
	}
}

// The comment on Run() insists a Google hiccup must never leave you with no
// digest at all — the reminders matter more than the schedule annotation.
// TestDigestSurvivesCalendarReadFailure calls renderDigest directly, so it pins
// the RENDERER and not that decision; turning the log-and-continue into a
// `return` scored UNNOTICED. This is also the only test that runs Run() end to
// end, which is why several other holes existed at all.
func TestARunSurvivesACalendarFailureAndStillWritesTheDigest(t *testing.T) {
	loc := pacific(t)
	digestNote := &model.Item{
		ID: "digest", Type: "note", Title: digestTitle, Tags: []string{"personal", digestTag},
	}
	todo := &model.Item{
		ID: "todo", Type: "todo", Title: "Still here", Status: "open",
		Tags: []string{"personal"}, DueAt: ptr(time.Date(2026, 7, 20, 15, 0, 0, 0, loc)),
	}
	nb := newFakeNoteboard(todo, digestNote)
	coordinator, _, calendar := newTestCoordinator(t, time.Date(2026, 7, 20, 8, 0, 0, 0, loc), nb)
	calendar.listErr = fmt.Errorf("google timeout")

	if err := coordinator.Run(); err != nil {
		t.Fatalf("a calendar read failure took the whole run down: %v", err)
	}

	fields, patched := nb.patched["digest"]
	if !patched {
		t.Fatal("the calendar failed and the digest note was never written; the user gets nothing")
	}
	body, _ := fields["body"].(string)
	if !contains(body, "Still here") {
		t.Errorf("the todos must still reach the note when the calendar read fails:\n%s", body)
	}
	if !contains(body, "Couldn't load your calendar") {
		t.Errorf("the failure must be visible in the note, not only in the log:\n%s", body)
	}
}

// materializeDueOccurrences carries TWO independent idempotence guards, and the
// source comment says so: last_materialized_at, and a scan of the template's
// existing children. TestMaterializationIsIdempotentAcrossTicks pins only the
// first — disabling the child scan scored UNNOTICED, because the stamp alone
// covers that fixture. The child scan exists precisely for the case the stamp
// cannot cover: a child created before the field existed, on a template whose
// stamp is therefore nil. That is this test, and it makes the scan a sole
// detector.
func TestAChildFromBeforeLastMaterializedAtExistedIsNotDuplicated(t *testing.T) {
	loc := pacific(t)
	anchor := time.Date(2026, 7, 21, 9, 0, 0, 0, loc)
	occurrence := time.Date(2026, 7, 21, 9, 0, 0, 0, loc)
	template := &model.Item{
		ID: "template", Type: "todo", Title: "Water the plants", Status: "open",
		Tags: []string{"personal"},
		Schedule: &model.Schedule{
			DTStart: &anchor, TZID: "America/Los_Angeles",
			RRule: "FREQ=MONTHLY;BYDAY=3TU", Mode: model.ScheduleModeInstance,
			// Deliberately nil: this template predates the field, which is the
			// whole situation the second guard was written for.
			LastMaterializedAt: nil,
		},
	}
	existingChild := &model.Item{
		ID: "child", Type: "todo", Title: "Water the plants", Status: "open",
		Tags: []string{"personal"}, ParentID: ptr("template"), DueAt: &occurrence,
	}
	nb := newFakeNoteboard(template, existingChild)

	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 21, 9, 15, 0, 0, loc), nb)
	if err := coordinator.materializeDueOccurrences(); err != nil {
		t.Fatalf("materializeDueOccurrences: %v", err)
	}

	if len(nb.created) != 0 {
		t.Fatalf("created %d duplicates of a chore that already had its child for that date; "+
			"with no last_materialized_at to lean on, the child scan is the only guard left", len(nb.created))
	}
}

// TestCalendarIsOptInPerItem looks like it pins the opt-in and does not: its
// not-opted-in fixture has NO schedule at all, so it is excluded by the
// `item.Schedule != nil` check earlier in the same chain. Deleting the opt-in
// check scored UNNOTICED. An item that HAS a schedule and asked only for the
// digest is the case that separates them.
func TestAnItemThatAskedOnlyForTheDigestGetsNoCalendarEvent(t *testing.T) {
	loc := pacific(t)
	soon := time.Date(2026, 7, 20, 9, 0, 0, 0, loc)
	digestOnly := &model.Item{
		ID: "digest-only", Type: "todo", Title: "Buy milk", Status: "open",
		Tags: []string{"personal"}, DueAt: &soon,
		Schedule: &model.Schedule{
			TZID:   "America/Los_Angeles",
			Remind: &model.Remind{Channels: []string{model.ChannelDigest}},
		},
	}
	nb := newFakeNoteboard(digestOnly)
	coordinator, _, calendar := newTestCoordinator(t, time.Date(2026, 7, 13, 10, 0, 0, 0, loc), nb)

	if err := coordinator.syncCalendar(); err != nil {
		t.Fatalf("syncCalendar: %v", err)
	}
	if calendar.created != 0 {
		t.Fatalf("created %d events for an item whose channels are digest-only; the calendar "+
			"stops being usable the moment every todo lands on it", calendar.created)
	}
	if _, patched := nb.patched["digest-only"]; patched {
		t.Error("an item that never asked for a calendar event had gcal links written to it")
	}
}

// An archived template stops producing. `done` deliberately does not — marking
// one occurrence done must not retire the series — so the two statuses have to be
// told apart, and nothing was telling them apart.
func TestAnArchivedTemplateStopsProducingOccurrences(t *testing.T) {
	loc := pacific(t)
	anchor := time.Date(2026, 7, 21, 9, 0, 0, 0, loc)
	schedule := func() *model.Schedule {
		return &model.Schedule{
			DTStart: &anchor, TZID: "America/Los_Angeles",
			RRule: "FREQ=MONTHLY;BYDAY=3TU", Mode: model.ScheduleModeInstance,
		}
	}

	archived := &model.Item{
		ID: "archived", Type: "todo", Title: "Retired chore", Status: "archived",
		Tags: []string{"personal"}, Schedule: schedule(),
	}
	nb := newFakeNoteboard(archived)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 21, 9, 15, 0, 0, loc), nb)
	if err := coordinator.materializeDueOccurrences(); err != nil {
		t.Fatalf("materializeDueOccurrences: %v", err)
	}
	if len(nb.created) != 0 {
		t.Fatalf("an archived template produced %d occurrences; archiving is how a series is "+
			"retired and it did nothing", len(nb.created))
	}

	// The known-negative half: `done` must NOT retire the series, or completing
	// this month's chore silently cancels every future one.
	done := &model.Item{
		ID: "done", Type: "todo", Title: "Live chore", Status: "done",
		Tags: []string{"personal"}, Schedule: schedule(),
	}
	nbDone := newFakeNoteboard(done)
	coordinatorDone, _, _ := newTestCoordinator(t, time.Date(2026, 7, 21, 9, 15, 0, 0, loc), nbDone)
	if err := coordinatorDone.materializeDueOccurrences(); err != nil {
		t.Fatalf("materializeDueOccurrences: %v", err)
	}
	if len(nbDone.created) != 1 {
		t.Fatalf("a template marked done produced %d occurrences, want 1 — a series must outlive "+
			"one completed occurrence", len(nbDone.created))
	}
}

// Rolling mode advances one row in place. Instance mode spawns children. Applying
// the roll to an instance-mode template would mutate the series itself, moving
// the anchor every time one occurrence was completed.
func TestARecurringTemplateIsNeverRolledInPlace(t *testing.T) {
	loc := pacific(t)
	anchor := time.Date(2026, 7, 21, 9, 0, 0, 0, loc)
	template := &model.Item{
		ID: "template", Type: "todo", Title: "Water the plants", Status: "done",
		Tags: []string{"personal"}, DueAt: &anchor,
		Schedule: &model.Schedule{
			DTStart: &anchor, TZID: "America/Los_Angeles",
			RRule: "FREQ=MONTHLY;BYDAY=3TU", Mode: model.ScheduleModeInstance,
		},
	}
	nb := newFakeNoteboard(template)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 22, 9, 0, 0, 0, loc), nb)

	if err := coordinator.rollCompletedRollingItems(); err != nil {
		t.Fatalf("rollCompletedRollingItems: %v", err)
	}
	if fields, patched := nb.patched["template"]; patched {
		t.Fatalf("an instance-mode template was rolled in place (%v); its occurrences are the "+
			"work, and moving the series is how you lose the anchor", fields)
	}
}

// markDailyDigestSent rewrites the note's links to carry exactly one day marker.
// It must drop the PREVIOUS day's marker and keep everything else — inverting
// that test drops every non-marker link on the note instead, which is silent
// data loss on the one note the user reads.
func TestMarkingTheDigestSentReplacesOnlyTheDayMarker(t *testing.T) {
	loc := pacific(t)
	digestNote := &model.Item{
		ID: "digest", Type: "note", Title: digestTitle, Tags: []string{"personal", digestTag},
		Links: []string{"digest-sent:2026-07-12", "https://example.com/keep-me"},
	}
	nb := newFakeNoteboard(digestNote)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 13, 8, 0, 0, 0, loc), nb)

	if err := coordinator.markDailyDigestSent(); err != nil {
		t.Fatalf("markDailyDigestSent: %v", err)
	}
	links, _ := nb.patched["digest"]["links"].([]string)

	want := map[string]bool{"digest-sent:2026-07-13": false, "https://example.com/keep-me": false}
	for _, link := range links {
		if _, ok := want[link]; !ok {
			t.Errorf("unexpected link left on the note: %q", link)
			continue
		}
		want[link] = true
	}
	for link, seen := range want {
		if !seen {
			t.Errorf("link %q was dropped from the digest note", link)
		}
	}
	if len(links) != 2 {
		t.Fatalf("note carries %d links (%v), want exactly today's marker and the unrelated one — "+
			"a growing log of every day the digest went out is not state, it is a leak", len(links), links)
	}
}

// Every fixture above anchors its template at DTStart == roughly `now`, so a rule
// cannot produce an occurrence before the tick that reads it and the backfill floor
// has nothing to hold back. Widening the floor to a year therefore scored UNNOTICED
// — not because the floor does nothing, but because no fixture had any history for
// it to refuse. A template anchored months ago, with no last_materialized_at, is the
// only shape that separates them: it is what a chore looks like the first tick after
// this field was added, or after the coordinator has been down for a while.
func TestALongIdleTemplateBackfillsOneDay(t *testing.T) {
	loc := pacific(t)
	anchor := time.Date(2026, 5, 1, 9, 0, 0, 0, loc)
	template := &model.Item{
		ID: "template", Type: "todo", Title: "Water the plants", Status: "open",
		Tags: []string{"personal"},
		Schedule: &model.Schedule{
			DTStart: &anchor, TZID: "America/Los_Angeles",
			RRule: "FREQ=DAILY", Mode: model.ScheduleModeInstance,
			// Nil is the whole point: with no stamp the floor is the only thing
			// standing between this tick and eighty-one days of missed chores.
			LastMaterializedAt: nil,
		},
	}
	nb := newFakeNoteboard(template)
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, loc)
	coordinator, _, _ := newTestCoordinator(t, now, nb)

	if err := coordinator.materializeDueOccurrences(); err != nil {
		t.Fatalf("materializeDueOccurrences: %v", err)
	}
	if len(nb.created) == 0 {
		t.Fatal("nothing was materialized at all, so this fixture cannot say anything about the floor")
	}

	// The floor is `now - 24h`. Stated as the earliest due date allowed rather than
	// as a count, because the count also depends on Lookahead and would move for a
	// reason that has nothing to do with backfill.
	floor := now.Add(-24 * time.Hour)
	for _, child := range nb.created {
		if child.DueAt == nil {
			t.Fatalf("materialized %q with no due date", child.Title)
		}
		if child.DueAt.Before(floor) {
			t.Errorf("materialized an occurrence due %s, before the %s floor — a long-idle tick "+
				"is dumping months of missed chores into the digest at once",
				child.DueAt.Format(time.RFC3339), floor.Format(time.RFC3339))
		}
	}

	// Eight: the daily occurrences at 9am inside [now-24h, now+7d], which is
	// 21 July through 28 July inclusive. Written as a literal so that widening the
	// window fails here instead of quietly moving the expectation with it.
	if len(nb.created) != 8 {
		t.Errorf("materialized %d occurrences, want 8 — the window is one day back plus the lookahead",
			len(nb.created))
	}
}

// The roll anchor is `max(now, item.DueAt)`, and both halves of that max are load
// bearing. TestRollingItemAdvancesToItsNextOccurrenceWhenDone cannot see either:
// its item is due 9am, the tick is 10am the same day, and the rule is monthly — so
// anchoring at `now` and anchoring at the due date both land on the same next
// occurrence a month out. Inverting the comparison scored UNNOTICED against it.
// A daily rule is what makes the two anchors disagree.
//
// This half: an OVERDUE item. Anchoring at its due date instead of now walks the
// series to an occurrence that has already passed, so completing the chore reopens
// it in the past and it is overdue again the instant it comes back.
func TestARollingItemNeverRollsBackwardsIntoThePast(t *testing.T) {
	loc := pacific(t)
	dtstart := time.Date(2026, 7, 1, 9, 0, 0, 0, loc)
	overdue := time.Date(2026, 7, 19, 9, 0, 0, 0, loc)
	item := &model.Item{
		ID: "rolling", Type: "todo", Title: "Renew registration", Status: "done",
		Tags: []string{"personal"}, DueAt: &overdue,
		Schedule: &model.Schedule{
			DTStart: &dtstart, TZID: "America/Los_Angeles",
			RRule: "FREQ=DAILY", Mode: model.ScheduleModeRolling,
		},
	}
	nb := newFakeNoteboard(item)
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, loc)
	coordinator, _, _ := newTestCoordinator(t, now, nb)

	if err := coordinator.rollCompletedRollingItems(); err != nil {
		t.Fatalf("rollCompletedRollingItems: %v", err)
	}
	fields, patched := nb.patched["rolling"]
	if !patched {
		t.Fatal("a completed rolling item was not advanced at all")
	}

	rolled, err := time.ParseInLocation(time.RFC3339, fields["due_at"].(string), loc)
	if err != nil {
		t.Fatalf("due_at is not a timestamp: %v", err)
	}
	if !rolled.After(now) {
		t.Errorf("rolled to %s, which is not after the %s tick — a two-day-overdue chore was "+
			"reopened already overdue, and it will be reopened in the past on every tick after this one",
			rolled.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	want := time.Date(2026, 7, 22, 9, 0, 0, 0, loc)
	if !rolled.Equal(want) {
		t.Errorf("rolled to %s, want the next occurrence after the tick, %s",
			rolled.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// The other half of the same max, and it fails in the opposite direction: an item
// completed EARLY. Anchoring at `now` rather than at its own due date rolls it to an
// occurrence that is still before the one just finished, so the series goes round the
// same date twice and the early completion is thrown away.
func TestARollingItemCompletedEarlyAdvancesPastTheOccurrenceItFinished(t *testing.T) {
	loc := pacific(t)
	dtstart := time.Date(2026, 7, 1, 9, 0, 0, 0, loc)
	future := time.Date(2026, 7, 25, 9, 0, 0, 0, loc)
	item := &model.Item{
		ID: "rolling", Type: "todo", Title: "Renew registration", Status: "done",
		Tags: []string{"personal"}, DueAt: &future,
		Schedule: &model.Schedule{
			DTStart: &dtstart, TZID: "America/Los_Angeles",
			RRule: "FREQ=DAILY", Mode: model.ScheduleModeRolling,
		},
	}
	nb := newFakeNoteboard(item)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 21, 10, 0, 0, 0, loc), nb)

	if err := coordinator.rollCompletedRollingItems(); err != nil {
		t.Fatalf("rollCompletedRollingItems: %v", err)
	}
	fields, patched := nb.patched["rolling"]
	if !patched {
		t.Fatal("a completed rolling item was not advanced at all")
	}

	rolled, err := time.ParseInLocation(time.RFC3339, fields["due_at"].(string), loc)
	if err != nil {
		t.Fatalf("due_at is not a timestamp: %v", err)
	}
	if !rolled.After(future) {
		t.Errorf("rolled to %s, which is not after the %s occurrence that was just completed — "+
			"finishing a chore early hands you back a date you have already dealt with",
			rolled.Format(time.RFC3339), future.Format(time.RFC3339))
	}
	want := time.Date(2026, 7, 26, 9, 0, 0, 0, loc)
	if !rolled.Equal(want) {
		t.Errorf("rolled to %s, want the occurrence after the completed one, %s",
			rolled.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// syncCalendar's update arm is guarded on the synced due date having MOVED. No
// fixture above ever reaches that arm: the opt-in tests have no event yet (the
// create arm) and the teardown test is not open (the remove arm), so inverting the
// guard scored UNNOTICED. It needs an item already in the steady state — an event,
// a gcalsync stamp, and a due date that agrees with it.
//
// This half is the quiet one. Inverted, the coordinator rewrites an event that has
// not moved on every 15-minute tick: 96 Google Calendar writes a day per item,
// costing nothing visible and losing no reminder, which is exactly why it would sit
// unfixed.
func TestAnUnmovedCalendarEventIsNotRewrittenOnEveryTick(t *testing.T) {
	loc := pacific(t)
	due := time.Date(2026, 7, 20, 9, 0, 0, 0, loc)
	settled := &model.Item{
		ID: "settled", Type: "todo", Title: "Dentist", Status: "open",
		Tags: []string{"personal"}, DueAt: &due,
		Links: []string{"gcal:event-123", "gcalsync:" + due.Format(time.RFC3339)},
		Schedule: &model.Schedule{
			TZID:   "America/Los_Angeles",
			Remind: &model.Remind{Channels: []string{model.ChannelCalendar}},
		},
	}
	nb := newFakeNoteboard(settled)
	coordinator, _, calendar := newTestCoordinator(t, time.Date(2026, 7, 13, 10, 0, 0, 0, loc), nb)

	if err := coordinator.syncCalendar(); err != nil {
		t.Fatalf("syncCalendar: %v", err)
	}
	if calendar.updated != 0 {
		t.Errorf("rewrote %d events for an item whose due date has not moved; the tick runs every "+
			"15 minutes, so this is 96 writes a day for an event nobody changed", calendar.updated)
	}
	if calendar.created != 0 || calendar.deleted != 0 {
		t.Errorf("created %d and deleted %d events for a settled item; it should be left alone entirely",
			calendar.created, calendar.deleted)
	}
	if _, patched := nb.patched["settled"]; patched {
		t.Error("a settled item's links were rewritten, so noteboard is being written to on every tick as well")
	}
}

// The other side of the same guard, and the one that loses information rather than
// spending it: a due date that moved must reach the calendar, or the event stays at
// the old time and the reminder arrives for a slot that no longer exists.
func TestAMovedDueDatePushesTheCalendarEventToTheNewTime(t *testing.T) {
	loc := pacific(t)
	was := time.Date(2026, 7, 20, 9, 0, 0, 0, loc)
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, loc)
	moved := &model.Item{
		ID: "moved", Type: "todo", Title: "Dentist", Status: "open",
		Tags: []string{"personal"}, DueAt: &now,
		Links: []string{"gcal:event-123", "gcalsync:" + was.Format(time.RFC3339)},
		Schedule: &model.Schedule{
			TZID:   "America/Los_Angeles",
			Remind: &model.Remind{Channels: []string{model.ChannelCalendar}},
		},
	}
	nb := newFakeNoteboard(moved)
	coordinator, _, calendar := newTestCoordinator(t, time.Date(2026, 7, 13, 10, 0, 0, 0, loc), nb)

	if err := coordinator.syncCalendar(); err != nil {
		t.Fatalf("syncCalendar: %v", err)
	}
	if calendar.updated != 1 {
		t.Fatalf("updated %d events, want 1 — the appointment moved and the calendar still says the old time",
			calendar.updated)
	}
	if calendar.created != 0 {
		t.Errorf("created %d events; the item already had one and it should have been moved, not duplicated",
			calendar.created)
	}

	// The stamp has to move too, or the next tick reads the same stale value and
	// updates again forever.
	links, ok := nb.patched["moved"]["links"].([]string)
	if !ok {
		t.Fatal("the event was updated but the item's gcalsync stamp was never rewritten")
	}
	wantStamp := "gcalsync:" + now.Format(time.RFC3339)
	found := false
	for _, link := range links {
		if link == wantStamp {
			found = true
		}
		if link == "gcalsync:"+was.Format(time.RFC3339) {
			t.Errorf("the old gcalsync stamp %q is still on the item, so it will be updated again next tick", link)
		}
	}
	if !found {
		t.Errorf("links = %v, want the new stamp %q", links, wantStamp)
	}
}

// renderSchedule does not trust its input to be today's events only — it re-filters
// against the day boundary, because the section is titled "Today's schedule" and an
// event from tomorrow under that heading is a meeting you will believe you have in
// the next few hours. No fixture ever handed it an event outside today, so pushing
// the boundary out by a day scored UNNOTICED.
//
// Both sides of the boundary are here on purpose. An assertion that only tomorrow's
// event is absent is equally satisfied by a filter that drops everything.
func TestTomorrowsEventsAreNotFoldedIntoTodaysSchedule(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 7, 20, 8, 0, 0, 0, loc)
	coordinator, _, _ := newTestCoordinator(t, now, newFakeNoteboard())
	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}

	events := []CalendarEvent{
		{
			Summary: "Late tonight",
			Start:   time.Date(2026, 7, 20, 23, 30, 0, 0, loc),
			End:     time.Date(2026, 7, 21, 0, 30, 0, 0, loc),
		},
		{
			Summary: "Tomorrow morning",
			Start:   time.Date(2026, 7, 21, 0, 15, 0, 0, loc),
			End:     time.Date(2026, 7, 21, 1, 15, 0, 0, loc),
		},
	}
	rendered := coordinator.renderDigest(board, events, nil)

	if contains(rendered, "Tomorrow morning") {
		t.Errorf("an event starting tomorrow is listed under Today's schedule:\n%s", rendered)
	}
	if !contains(rendered, "Late tonight") {
		t.Errorf("an event starting tonight at 11:30 is missing from Today's schedule:\n%s", rendered)
	}
	if !contains(rendered, "Today's schedule (1)") {
		t.Errorf("the schedule count should read 1, the single event that starts today:\n%s", rendered)
	}
}

// The day-load line is read for its NUMBERS, not only for its colour: "~1.5h
// booked, ~12.5h free" is the sentence that answers "is there room today?".
// Every fixture before this one used whole-hour events and asserted only the
// icon, so rounding a duration to the nearest quarter-DAY instead of the nearest
// half hour scored UNNOTICED — the line still said "booked", still said "free",
// and still went green.
func TestDayLoadReportsItsHoursToTheNearestHalfHour(t *testing.T) {
	loc := pacific(t)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 20, 8, 0, 0, 0, loc), newFakeNoteboard())

	// 90 minutes booked inside a 14-hour window (8am–10pm): 1.5h booked, 12.5h
	// free, and the tail from 10:30 to 10pm is the biggest open stretch at 11.5h.
	// All three are half-hours on purpose — a whole-hour fixture cannot tell
	// half-hour rounding from any coarser rounding.
	line := coordinator.dayLoad([]CalendarEvent{{
		Summary: "Standup and a long review",
		Start:   time.Date(2026, 7, 20, 9, 0, 0, 0, loc),
		End:     time.Date(2026, 7, 20, 10, 30, 0, 0, loc),
	}})
	for _, want := range []string{"~1.5h booked", "~12.5h free", "~11.5h"} {
		if !strings.Contains(line, want) {
			t.Errorf("day-load line is missing %q:\n%s", want, line)
		}
	}

	// Under an hour renders as minutes. "0h booked" on a day with a real meeting
	// on it reads as a free morning.
	short := coordinator.dayLoad([]CalendarEvent{{
		Summary: "Quick call",
		Start:   time.Date(2026, 7, 20, 9, 0, 0, 0, loc),
		End:     time.Date(2026, 7, 20, 9, 45, 0, 0, loc),
	}})
	if !strings.Contains(short, "~45m booked") {
		t.Errorf("a 45-minute meeting should read as minutes, not as a rounded hour:\n%s", short)
	}
}

// Each bucket is sorted by due date before it is rendered, so a section reads in
// the order you will actually meet it. noteboard hands items back newest-created
// first, which is not that order and is not any order the reader can use.
//
// No fixture had ever put two dated items in one bucket, so reversing the
// comparison scored UNNOTICED: with one item per bucket, every sort is correct.
func TestABucketIsRenderedInDueDateOrderNotInTheOrderNoteboardReturned(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, loc)

	// Handed over latest-due first, which is the wrong order and is also the
	// order a real noteboard listing arrives in.
	nb := newFakeNoteboard(
		&model.Item{ID: "friday", Type: "todo", Title: "Friday thing", Status: "open",
			Tags: []string{"personal"}, DueAt: ptr(time.Date(2026, 7, 17, 9, 0, 0, 0, loc))},
		&model.Item{ID: "wednesday", Type: "todo", Title: "Wednesday thing", Status: "open",
			Tags: []string{"personal"}, DueAt: ptr(time.Date(2026, 7, 15, 9, 0, 0, 0, loc))},
		&model.Item{ID: "tuesday", Type: "todo", Title: "Tuesday thing", Status: "open",
			Tags: []string{"personal"}, DueAt: ptr(time.Date(2026, 7, 14, 9, 0, 0, 0, loc))},
	)
	coordinator, _, _ := newTestCoordinator(t, now, nb)

	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}
	if len(board.ThisWeek) != 3 {
		t.Fatalf("this-week bucket holds %d items, want all 3", len(board.ThisWeek))
	}
	wantOrder := []string{"Tuesday thing", "Wednesday thing", "Friday thing"}
	for i, want := range wantOrder {
		if board.ThisWeek[i].Title != want {
			t.Fatalf("bucket order = %q at %d, want %q", board.ThisWeek[i].Title, i, want)
		}
	}

	// And the sort has to survive into the note, which is the only place the
	// user ever sees it.
	rendered := coordinator.renderDigest(board, nil, nil)
	previous := -1
	for _, title := range wantOrder {
		at := strings.Index(rendered, title)
		if at < 0 {
			t.Fatalf("rendered digest is missing %q:\n%s", title, rendered)
		}
		if at < previous {
			t.Errorf("the digest lists %q out of due order:\n%s", title, rendered)
		}
		previous = at
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
