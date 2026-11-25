package main

import (
	"fmt"
	"io"
	"os"
	"slices"
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

type fakeHerald struct {
	sessions []string // the message of each relay, in order
	reused   []string // the reuseSessionID passed alongside each ("" = opened new)
}

func (f *fakeHerald) Relay(from, title, message, reuseSessionID string) (string, error) {
	f.sessions = append(f.sessions, message)
	f.reused = append(f.reused, reuseSessionID)
	if reuseSessionID != "" {
		return reuseSessionID, nil
	}
	return fmt.Sprintf("herald-test-%d", len(f.sessions)), nil
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
	// Built from the SHIPPED defaults rather than field by field. Every value the
	// suite restated here was a value it could no longer tell apart from the one
	// the binary runs with, and the working day proved it: added as a flag with a
	// default of 8am–4:30pm, it stayed 00:00–00:00 in every test, so the suite
	// measured a day with no work in it while the binary measured a real one.
	config, err := parseFlags(nil, noEnvironment)
	if err != nil {
		t.Fatalf("parseFlags with no arguments should yield the shipped defaults: %v", err)
	}
	coordinator := newCoordinator(config, pacific(t), now)
	coordinator.noteboard = nb
	coordinator.calendar = calendar
	coordinator.herald = herald
	return coordinator, herald, calendar
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

	if err := coordinator.nudgeOnce(board, nil, nil); err != nil {
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
		if err := coordinator.nudgeOnce(board, nil, nil); err != nil {
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
		if err := coordinator.nudgeOnce(board, nil, nil); err != nil {
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
	if err := coordinator.nudgeOnce(board, nil, nil); err != nil {
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

	// Work runs 8am–4:30pm and Salsa takes 8–9pm, so 4.5 of the 14-hour window is
	// free. Ice Skating at 6am is before the window opens and costs none of it.
	for _, want := range []string{
		"Today's schedule", "Salsa", "Ice Skating", "8pm", "Work 8am–4:30pm", "~4.5h free",
	} {
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

// recordRelayMarkers rewrites the note's links to carry exactly one day marker
// of each kind. It must drop the PREVIOUS day's markers and keep everything
// else — inverting that test drops every non-marker link on the note instead,
// which is silent data loss on the one note the user reads.
func TestMarkingTheDigestSentReplacesOnlyTheDayMarkers(t *testing.T) {
	loc := pacific(t)
	digestNote := &model.Item{
		ID: "digest", Type: "note", Title: digestTitle, Tags: []string{"personal", digestTag},
		Links: []string{"digest-sent:2026-07-12", "herald-session:2026-07-12:herald-yesterday", "https://example.com/keep-me"},
	}
	nb := newFakeNoteboard(digestNote)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 13, 8, 0, 0, 0, loc), nb)

	if err := coordinator.recordRelayMarkers("herald-today", true); err != nil {
		t.Fatalf("recordRelayMarkers: %v", err)
	}
	links, _ := nb.patched["digest"]["links"].([]string)

	want := map[string]bool{
		"digest-sent:2026-07-13":                 false,
		"herald-session:2026-07-13:herald-today": false,
		"https://example.com/keep-me":            false,
	}
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
	if len(links) != 3 {
		t.Fatalf("note carries %d links (%v), want exactly today's two markers and the unrelated one — "+
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

// The twin of the materializer's floor, one function away and spelled the same.
// An item that has never nagged has no last_nagged_at to expand from, so the nag
// window falls back to the last day — otherwise the first tick after the nag is
// added replays every firing since the anchor and opens a session for a weekday
// rule that has been "firing" unheard for months.
//
// The weekend is what makes this visible: a weekday nag has a 72-hour gap across
// it, so on a Sunday the most recent firing is well outside the floor and a
// correct coordinator stays quiet.
//
// Ported from b92b34f1.
func TestAnItemThatHasNeverNaggedFiresOnlyForARecentOccurrence(t *testing.T) {
	loc := pacific(t)
	due := time.Date(2026, 6, 30, 9, 0, 0, 0, loc)
	item := &model.Item{
		ID: "never-nagged", Type: "todo", Title: "Call the tag agency", Status: "open",
		Tags: []string{"personal"}, DueAt: &due,
		Schedule: &model.Schedule{
			TZID:   "America/Los_Angeles",
			Remind: &model.Remind{Nag: "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;BYHOUR=10;BYMINUTE=0;BYSECOND=0"},
		},
	}
	nb := newFakeNoteboard(item)

	// Sunday noon. The last weekday firing was Friday at 10am, 50 hours back.
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 12, 12, 0, 0, 0, loc), nb)
	firing, err := coordinator.nagsFiringNow()
	if err != nil {
		t.Fatalf("nagsFiringNow: %v", err)
	}
	if len(firing) != 0 {
		t.Fatalf("a never-nagged item fired on a Sunday for a Friday occurrence — the floor is "+
			"not holding, so the first tick replays the item's whole nag history (%d firing)", len(firing))
	}

	// And the floor is a floor, not a mute: the same item on Monday morning, still
	// never nagged, must speak.
	coordinator, _, _ = newTestCoordinator(t, time.Date(2026, 7, 13, 10, 30, 0, 0, loc), nb)
	firing, err = coordinator.nagsFiringNow()
	if err != nil {
		t.Fatalf("nagsFiringNow: %v", err)
	}
	if len(firing) != 1 {
		t.Fatalf("%d items firing 30 minutes after the 10am nag, want 1 — the floor has "+
			"stopped the nag working at all", len(firing))
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

// ListEvents used to read start.date for an all-day event and drop end.date, so
// every all-day event arrived one day long whatever its real span, and the
// schedule's window had nothing to test a multi-day trip against.
//
// Google's end.date is exclusive, and this asserts it is carried through as sent
// rather than adjusted in the adapter — the window arithmetic that reads it lives
// in splitTodaysEvents, and two places converting one convention drift apart.
//
// Ported from 3644d4c9.
func TestListEventsCarriesAnAllDayEventsExclusiveEndDate(t *testing.T) {
	mirror := calendarServing(t, `{"events":[
		{"summary":"Trip","start":{"date":"2026-07-18"},"end":{"date":"2026-07-25"}},
		{"summary":"Standup","start":{"dateTime":"2026-07-20T09:00:00-07:00"},"end":{"dateTime":"2026-07-20T09:15:00-07:00"}}
	]}`)
	events, err := mirror.ListEvents("test-token", time.Now(), time.Now())
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 — the fixture is not reaching the parser", len(events))
	}

	trip := events[0]
	if !trip.AllDay {
		t.Errorf("an event with start.date did not come back as all-day")
	}
	if want := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC); !trip.Start.Equal(want) {
		t.Errorf("trip start = %s, want %s", trip.Start, want)
	}
	if want := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC); !trip.End.Equal(want) {
		t.Errorf("trip end = %s, want %s — end.date dropped, so a week-long trip reads as one day", trip.End, want)
	}
	if events[1].AllDay {
		t.Errorf("an event with start.dateTime came back as all-day")
	}
}

// allDayEvent builds a fixture the way ListEvents does: an all-day event is a
// range of civil dates parsed to UTC midnight, and its end is EXCLUSIVE. The
// dates are literals rather than derived from the coordinator's clock, so the
// fixture cannot agree with the filter by construction.
func allDayEvent(summary, start, end string) CalendarEvent {
	event := CalendarEvent{Summary: summary, AllDay: true}
	parsed, err := time.Parse("2006-01-02", start)
	if err != nil {
		panic(err)
	}
	event.Start = parsed
	if end != "" {
		parsed, err = time.Parse("2006-01-02", end)
		if err != nil {
			panic(err)
		}
		event.End = parsed
	}
	return event
}

// The sibling of TestTomorrowsEventsAreNotFoldedIntoTodaysSchedule, for the other
// half of the same window. splitTodaysEvents used to `continue` on event.AllDay
// before any date check ran, which read as "an all-day event is always today" —
// but a holiday and a trip are as capable of being next month as a meeting is,
// and the heading counts them either way.
//
// Ported from 3644d4c9 and 9f910416. "Conference tomorrow" is the near side of
// the start bound: without it, moving the bound by a day changed no test.
func TestTodaysScheduleBoundsAllDayEventsToToday(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 7, 20, 8, 0, 0, 0, loc)
	coordinator, _, _ := newTestCoordinator(t, now, newFakeNoteboard())
	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}

	events := []CalendarEvent{
		allDayEvent("Bank holiday", "2026-07-20", "2026-07-21"),          // today, single day
		allDayEvent("Trip through the week", "2026-07-18", "2026-07-25"), // began Saturday, still on
		allDayEvent("Conference tomorrow", "2026-07-21", "2026-07-22"),   // the near side of the bound
		allDayEvent("Conference next month", "2026-08-20", "2026-08-21"),
		allDayEvent("Holiday last year", "2025-01-15", "2025-01-16"),
		// Ends the instant today begins. Google's end.date is exclusive, so this
		// one's last real day was the 19th — it is over.
		allDayEvent("Trip that ended yesterday", "2026-07-10", "2026-07-20"),
	}
	rendered := coordinator.renderDigest(board, events, nil)

	for _, elsewhere := range []string{"Conference tomorrow", "Conference next month", "Holiday last year", "Trip that ended yesterday"} {
		if contains(rendered, elsewhere) {
			t.Errorf("all-day event %q is listed under today's schedule:\n%s", elsewhere, rendered)
		}
	}
	for _, today := range []string{"Bank holiday", "Trip through the week"} {
		if !contains(rendered, today) {
			t.Errorf("all-day event %q covers today and is missing from the schedule:\n%s", today, rendered)
		}
	}
	// The heading counts all-day events alongside timed ones, so a filter that
	// dropped the strays from the list but still counted them would read as a
	// busier day.
	if !contains(rendered, "Today's schedule (2)") {
		t.Errorf("schedule heading does not count exactly today's two all-day events:\n%s", rendered)
	}
}

// civilDate anchors both sides of the all-day comparison in UTC, and this is the
// test that earns that word. An all-day event carries no zone, so its bounds
// parse to UTC midnight; the coordinator's day starts at midnight where the user
// is. Compare the two as instants and the day boundary lands wherever the offset
// puts it.
//
// Pacific hides this: it is behind UTC, so the right and wrong comparisons agree
// there. East of UTC they do not, and a trip that ended last night stays on
// today's schedule.
//
// Ported from 9f910416.
func TestAllDayWindowDoesNotDriftInAZoneEastOfUTC(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	coordinator, _, _ := newTestCoordinator(t, time.Time{}, newFakeNoteboard())
	coordinator.Location = tokyo
	coordinator.Now = time.Date(2026, 7, 20, 8, 0, 0, 0, tokyo)
	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}

	rendered := coordinator.renderDigest(board, []CalendarEvent{
		// Exclusive end of the 20th: its last real day was the 19th, so in Tokyo
		// it ended last night.
		allDayEvent("Trip that ended last night", "2026-07-10", "2026-07-20"),
		allDayEvent("Bank holiday", "2026-07-20", "2026-07-21"),
	}, nil)

	if contains(rendered, "Trip that ended last night") {
		t.Errorf("a trip that ended last night is on today's schedule in Tokyo — the all-day "+
			"bound is comparing a UTC midnight against a local one:\n%s", rendered)
	}
	if !contains(rendered, "Bank holiday") {
		t.Errorf("today's all-day event is missing from the schedule in Tokyo:\n%s", rendered)
	}
}

// An all-day event with no end.date is one day long. Reading a missing end as
// zero would put the event's end at the start of time and drop it from every
// window.
//
// Ported from 3644d4c9.
func TestAllDayEventWithNoEndDateCoversItsOwnDay(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 7, 20, 8, 0, 0, 0, loc)
	coordinator, _, _ := newTestCoordinator(t, now, newFakeNoteboard())
	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}

	rendered := coordinator.renderDigest(board, []CalendarEvent{
		allDayEvent("Endless holiday", "2026-07-20", ""),
		allDayEvent("Yesterday, endless", "2026-07-19", ""),
	}, nil)

	if !contains(rendered, "Endless holiday") {
		t.Errorf("an all-day event today with no end.date was dropped:\n%s", rendered)
	}
	if contains(rendered, "Yesterday, endless") {
		t.Errorf("an all-day event from yesterday with no end.date is listed under today:\n%s", rendered)
	}
}

// The day-load line is read for its NUMBERS, not only for its color: "~1.5h
// booked, ~12.5h free" is the sentence that answers "is there room today?".
// Every fixture before this one used whole-hour events and asserted only the
// icon, so rounding a duration to the nearest quarter-DAY instead of the nearest
// half hour scored UNNOTICED — the line still said "booked", still said "free",
// and still went green.
func TestDayLoadReportsItsHoursToTheNearestHalfHour(t *testing.T) {
	loc := pacific(t)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 20, 8, 0, 0, 0, loc), newFakeNoteboard())

	// The work block (8am–4:30pm) leaves 5.5 hours of the 14-hour window. A
	// 90-minute evening event takes 1.5 of them, leaving 4h free and a 3.5h
	// stretch from 6:30pm to 10pm. All three are half-hours on purpose — a
	// whole-hour fixture cannot tell half-hour rounding from any coarser rounding.
	line := coordinator.dayLoad([]CalendarEvent{{
		Summary: "A long review",
		Start:   time.Date(2026, 7, 20, 17, 0, 0, 0, loc),
		End:     time.Date(2026, 7, 20, 18, 30, 0, 0, loc),
	}})
	for _, want := range []string{"~4h free", "~3.5h"} {
		if !strings.Contains(line, want) {
			t.Errorf("day-load line is missing %q:\n%s", want, line)
		}
	}

	// Under an hour renders as minutes. "0h free" on an evening that still has
	// three quarters of an hour in it reads as no evening at all.
	short := coordinator.dayLoad([]CalendarEvent{{
		Summary: "Evening booked solid",
		Start:   time.Date(2026, 7, 20, 16, 30, 0, 0, loc),
		End:     time.Date(2026, 7, 20, 21, 15, 0, 0, loc),
	}})
	if !strings.Contains(short, "~45m free") {
		t.Errorf("45 minutes left should read as minutes, not as a rounded hour:\n%s", short)
	}

	// Ninety minutes is past the hour, so it reads in hours. Under a cut at two
	// hours it would read "90m" in a line that otherwise talks in hours.
	ninety := coordinator.dayLoad([]CalendarEvent{{
		Summary: "Evening mostly booked",
		Start:   time.Date(2026, 7, 20, 16, 30, 0, 0, loc),
		End:     time.Date(2026, 7, 20, 20, 30, 0, 0, loc),
	}})
	if !strings.Contains(ninety, "~1.5h free") {
		t.Errorf("90 minutes left should read as 1.5h:\n%s", ninety)
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

// The day-load verdict has three bands, and until now only one of them was ever
// asserted. TestDayLoadFlagsAFragmentedDay builds a day whose biggest open
// stretch is 60 minutes and checks it reads red — true at any threshold above
// 60, so the 90 was pinned to a band and not to a value. Raising it to 120
// minutes left the whole suite green, and the 3-hour amber/green cut was not
// exercised by any fixture at all.
//
// Each pair below differs by ONE minute and lands on opposite sides of a cut, so
// a threshold that moves in either direction reddens a row. A single event that
// runs to the end of the waking day makes the biggest open stretch exactly the
// morning before it, which is what lets the fixture name a duration to the
// minute instead of hoping one falls out of the merge.
func TestTheDayLoadBandsStraddleTheirThresholds(t *testing.T) {
	loc := pacific(t)
	// A Saturday: on a work day the working hours are charged as busy and
	// cover the morning this fixture measures.
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 25, 8, 0, 0, 0, loc), newFakeNoteboard())

	for _, row := range []struct {
		name          string
		startHour     int
		startMinute   int
		biggestMinute int
		wantIcon      string
	}{
		{"one minute under the fragmented cut", 9, 29, 89, "🔴"},
		{"exactly on the fragmented cut", 9, 30, 90, "🟡"},
		{"one minute under the roomy cut", 10, 59, 179, "🟡"},
		{"exactly on the roomy cut", 11, 0, 180, "🟢"},
	} {
		t.Run(row.name, func(t *testing.T) {
			// Ends at the close of the waking day, so the only open stretch is
			// the morning: start - 8am.
			line := coordinator.dayLoad([]CalendarEvent{{
				Summary: "All afternoon",
				Start:   time.Date(2026, 7, 25, row.startHour, row.startMinute, 0, 0, loc),
				End:     time.Date(2026, 7, 25, 22, 0, 0, 0, loc),
			}})
			if !strings.HasPrefix(line, row.wantIcon) {
				t.Errorf("a biggest open stretch of %d minutes should read %s, got:\n%s",
					row.biggestMinute, row.wantIcon, line)
			}
		})
	}
}

// The priority mark fires at rank 3 and up. Every fixture that had ever carried a
// priority used 3 or left it at 0, so the cut could move to 2 — flagging every p2
// todo on the board — with the suite green. The two items here differ by one rank
// and must render differently.
func TestThePriorityMarkStraddlesItsRank(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, loc)
	due := time.Date(2026, 7, 13, 15, 0, 0, 0, loc)

	nb := newFakeNoteboard(
		&model.Item{ID: "p2", Type: "todo", Title: "Rank two todo", Status: "open",
			Tags: []string{"personal"}, Priority: 2, DueAt: &due},
		&model.Item{ID: "p3", Type: "todo", Title: "Rank three todo", Status: "open",
			Tags: []string{"personal"}, Priority: 3, DueAt: &due},
	)
	coordinator, _, _ := newTestCoordinator(t, now, nb)

	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}
	rendered := coordinator.renderDigest(board, nil, nil)

	if !contains(rendered, "❗Rank three todo") {
		t.Errorf("a rank-3 todo should carry the priority mark:\n%s", rendered)
	}
	if contains(rendered, "❗Rank two todo") {
		t.Errorf("a rank-2 todo must NOT carry the priority mark — if everything is "+
			"marked, nothing is:\n%s", rendered)
	}
}

// This Week ends before the seventh day; the seventh day itself is Later.
//
// TestNoOpenTodoCanFallThroughTheBuckets already carries an item due exactly on
// that horizon, and it is the item whose disappearance this whole bucket switch
// was repaired for — but it asserts only that the item lands in SOME bucket.
// Under that assertion the horizon can slide a day in either direction and the
// item simply changes sections, silently. These two differ by one day and name
// the bucket each must land in.
func TestTheThisWeekHorizonStraddlesTheSeventhDay(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, loc)

	nb := newFakeNoteboard(
		&model.Item{ID: "sixth", Type: "todo", Title: "Sixth day out", Status: "open",
			Tags: []string{"personal"}, DueAt: ptr(time.Date(2026, 7, 19, 9, 0, 0, 0, loc))},
		&model.Item{ID: "seventh", Type: "todo", Title: "Seventh day out", Status: "open",
			Tags: []string{"personal"}, DueAt: ptr(time.Date(2026, 7, 20, 9, 0, 0, 0, loc))},
	)
	coordinator, _, _ := newTestCoordinator(t, now, nb)

	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}

	bucketOf := func(id string) string {
		for _, row := range []struct {
			name  string
			items []*model.Item
		}{
			{"Overdue", board.Overdue}, {"DueToday", board.DueToday},
			{"ThisWeek", board.ThisWeek}, {"Later", board.Later}, {"Undated", board.Undated},
		} {
			for _, item := range row.items {
				if item.ID == id {
					return row.name
				}
			}
		}
		return "NONE"
	}

	if got := bucketOf("sixth"); got != "ThisWeek" {
		t.Errorf("an item due six days out belongs in This Week, got %s", got)
	}
	if got := bucketOf("seventh"); got != "Later" {
		t.Errorf("an item due seven days out is past the horizon and belongs in Later, got %s", got)
	}
}

// A mirrored calendar event is half an hour long. Nothing asserted its length,
// so the constant could be any number: fakeCalendar is handed the *model.Item
// and builds no payload, and the concrete mirror that calls eventPayload is
// declared unobservable by this suite. eventPayload is a free function in this
// package, so the value itself is reachable even though its call site is not —
// that half belongs to branch test/concrete-clients-speak-the-real-protocol.
//
// The assertion is on the gap between start and end rather than on the end
// alone: an end stamp of the right shape computed from the wrong start reads
// correct, and the duration is the thing the constant names.
func TestAMirroredCalendarEventIsHalfAnHourLong(t *testing.T) {
	loc := pacific(t)
	due := time.Date(2026, 7, 20, 15, 0, 0, 0, loc)
	payload := eventPayload(&model.Item{ID: "x", Type: "todo", Title: "Dentist", DueAt: &due})

	start, ok := payload["start"].(map[string]string)
	if !ok {
		t.Fatalf("start is not a {dateTime} map: %#v", payload["start"])
	}
	end, ok := payload["end"].(map[string]string)
	if !ok {
		t.Fatalf("end is not a {dateTime} map: %#v", payload["end"])
	}

	startAt, err := time.Parse(time.RFC3339, start["dateTime"])
	if err != nil {
		t.Fatalf("start dateTime %q: %v", start["dateTime"], err)
	}
	endAt, err := time.Parse(time.RFC3339, end["dateTime"])
	if err != nil {
		t.Fatalf("end dateTime %q: %v", end["dateTime"], err)
	}

	if !startAt.Equal(due) {
		t.Errorf("event starts at %s, want the item's due time %s", startAt, due)
	}
	if got := endAt.Sub(startAt); got != 30*time.Minute {
		t.Errorf("a mirrored event runs %s, want 30m", got)
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

// ---------------------------------------------------------------- the nudge names the work

// newNudgeFixture builds a board whose digest is due: the note exists, carries no
// marker for today, and the clock is past the digest hour.
func newNudgeFixture(t *testing.T, now time.Time, items ...*model.Item) (*Coordinator, *fakeHerald, *fakeCalendar) {
	t.Helper()
	note := &model.Item{ID: "digest", Type: "note", Title: digestTitle, Tags: []string{"personal", digestTag}}
	return newTestCoordinator(t, now, newFakeNoteboard(append(items, note)...))
}

// The nudge used to say "4 overdue, 1 due today" and nothing else. A count is not
// something you can act on: it cannot tell you whether the overdue pile is one
// forgotten errand or the tax return, so the only move it leaves is to go open
// the note — which makes the message a doorbell, not a reminder.
func TestTheNudgeNamesEachItemAndHowLateItIs(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, loc)

	coordinator, herald, _ := newNudgeFixture(t, now,
		&model.Item{
			ID: "late", Type: "todo", Title: "Apply to jobs", Status: "open", Priority: 3,
			Tags: []string{"personal", "career"}, DueAt: ptr(time.Date(2026, 7, 20, 9, 0, 0, 0, loc)),
		},
		&model.Item{
			ID: "today", Type: "todo", Title: "Check the parent portal", Status: "open",
			Tags: []string{"personal", "money"}, DueAt: ptr(time.Date(2026, 8, 20, 9, 0, 0, 0, loc)),
		},
		&model.Item{
			ID: "week", Type: "todo", Title: "Read Exiles", Status: "open",
			Tags: []string{"personal", "reading"}, DueAt: ptr(time.Date(2026, 8, 22, 9, 0, 0, 0, loc)),
		},
		// Undated work stays OUT of the nudge. Twenty-five shopping-list items are
		// a real part of this board and would bury the three things due.
		&model.Item{ID: "someday", Type: "todo", Title: "Buy green tea", Status: "open", Tags: []string{"personal"}},
	)

	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}
	if err := coordinator.nudgeOnce(board, nil, nil); err != nil {
		t.Fatalf("nudgeOnce: %v", err)
	}
	if len(herald.sessions) != 1 {
		t.Fatalf("opened %d herald sessions, want 1", len(herald.sessions))
	}
	message := herald.sessions[0]

	for _, want := range []string{
		"Apply to jobs",           // named, not counted
		"(31d late)",              // and how long it has been sitting there
		"❗",                       // priority 3 still marked
		"`career`",                // which part of life it belongs to
		"Check the parent portal", // due today
		"Read Exiles",             // later this week
		"⏰ Overdue (1)",           // the bucket totals survive
		"📅 Due today (1)",
		"🗓 Later this week (1)",
		digestTitle, // and it still points at the full record
	} {
		if !strings.Contains(message, want) {
			t.Errorf("nudge is missing %q:\n%s", want, message)
		}
	}
	if strings.Contains(message, "Buy green tea") {
		t.Errorf("undated work reached the nudge; only what is due belongs there:\n%s", message)
	}
}

// A cap that hides items silently is worse than no cap: the message reads as the
// whole board while being a fifth of it. This is the same failure the Later
// bucket exists to prevent in the note.
func TestTheNudgeCapsALongBucketAndSaysHowManyItHid(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, loc)

	items := []*model.Item{}
	for i := 0; i < nudgeSectionLimit+3; i++ {
		items = append(items, &model.Item{
			ID: fmt.Sprintf("late-%d", i), Type: "todo", Status: "open", Tags: []string{"personal"},
			Title: fmt.Sprintf("Overdue thing %d", i),
			DueAt: ptr(time.Date(2026, 8, 1, 9, 0, 0, 0, loc).Add(time.Duration(i) * time.Hour)),
		})
	}
	coordinator, herald, _ := newNudgeFixture(t, now, items...)

	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}
	if err := coordinator.nudgeOnce(board, nil, nil); err != nil {
		t.Fatalf("nudgeOnce: %v", err)
	}
	message := herald.sessions[0]

	if listed := strings.Count(message, "Overdue thing "); listed != nudgeSectionLimit {
		t.Errorf("listed %d items, want the cap of %d:\n%s", listed, nudgeSectionLimit, message)
	}
	// The heading keeps the TRUE total, and the dropped items are counted out loud.
	if !strings.Contains(message, fmt.Sprintf("⏰ Overdue (%d)", nudgeSectionLimit+3)) {
		t.Errorf("heading should carry the full bucket size, not the capped one:\n%s", message)
	}
	if !strings.Contains(message, "…and 3 more") {
		t.Errorf("capped section hid 3 items without saying so:\n%s", message)
	}

	// The note is the record and is never capped.
	rendered := coordinator.renderDigest(board, nil, nil)
	if listed := strings.Count(rendered, "Overdue thing "); listed != nudgeSectionLimit+3 {
		t.Errorf("the note listed %d of %d items — the record must stay total:\n%s",
			listed, nudgeSectionLimit+3, rendered)
	}
}

// The old design justified its terseness by saying a copy of the note in chat is
// a second thing to keep in sync. That was a real risk, and this is the answer to
// it: both surfaces render an item through the SAME label function, so a change
// to how an item reads lands on both or on neither.
func TestTheNudgeAndTheNoteDescribeAnItemIdentically(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, loc)
	item := &model.Item{
		ID: "late", Type: "todo", Title: "Follow up with the seller", Status: "open", Priority: 3,
		Tags: []string{"personal", "car-title", "phone"}, DueAt: ptr(time.Date(2026, 8, 1, 12, 0, 0, 0, loc)),
	}
	coordinator, herald, _ := newNudgeFixture(t, now, item)

	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}
	if err := coordinator.nudgeOnce(board, nil, nil); err != nil {
		t.Fatalf("nudgeOnce: %v", err)
	}

	label := coordinator.labelWithLateness(item)
	if !strings.Contains(herald.sessions[0], label) {
		t.Errorf("nudge does not carry the shared label %q:\n%s", label, herald.sessions[0])
	}
	if !strings.Contains(coordinator.renderDigest(board, nil, nil), label) {
		t.Errorf("note does not carry the shared label %q", label)
	}
}

// The todos say what to do; the calendar says whether there is room to do it. A
// nudge listing four overdue items on a day with no free hour is telling you to
// act on something you cannot act on.
func TestTheNudgeReadsTheDayAndSaysSoWhenTheCalendarFails(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, loc)
	todo := &model.Item{
		ID: "late", Type: "todo", Title: "Laundry", Status: "open", Tags: []string{"personal"},
		DueAt: ptr(time.Date(2026, 8, 15, 10, 0, 0, 0, loc)),
	}
	events := []CalendarEvent{
		{Summary: "Standup", Start: time.Date(2026, 8, 20, 10, 0, 0, 0, loc), End: time.Date(2026, 8, 20, 10, 30, 0, 0, loc)},
		{Summary: "Long review", Start: time.Date(2026, 8, 20, 12, 0, 0, 0, loc), End: time.Date(2026, 8, 20, 20, 0, 0, 0, loc)},
	}

	coordinator, herald, _ := newNudgeFixture(t, now, todo)
	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}
	if err := coordinator.nudgeOnce(board, events, nil); err != nil {
		t.Fatalf("nudgeOnce: %v", err)
	}
	message := herald.sessions[0]
	// The nudge LISTS the day, it does not summarize it: "2 events" cannot tell
	// you whether the evening is yours. Standup sits inside work hours and costs
	// nothing; the review runs to 8pm and eats all but the last two hours, so 2h
	// is free and 8pm–10pm is the biggest stretch. The free figure is asserted
	// because the line carries two durations that are interchangeable at a
	// glance: printing booked where it says free makes the fullest day read as
	// the emptiest.
	for _, want := range []string{
		"🗓 Today's schedule (2)", "10am–10:30am — Standup", "12pm–8pm — Long review",
		"Work 8am–4:30pm", "~2h free", "biggest open stretch ~2h", "🟡",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("schedule line is missing %q:\n%s", want, message)
		}
	}

	// A calendar read that failed must SAY the day is unknown. Dropping the line
	// makes a failed read look exactly like a free day.
	coordinator, herald, _ = newNudgeFixture(t, now, todo)
	board, err = coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}
	if err := coordinator.nudgeOnce(board, nil, fmt.Errorf("google timeout")); err != nil {
		t.Fatalf("nudgeOnce: %v", err)
	}
	if !strings.Contains(herald.sessions[0], "couldn't load your calendar") {
		t.Errorf("a failed calendar read was silently dropped from the nudge:\n%s", herald.sessions[0])
	}
}

// With the counts gone, a clear board renders no sections at all. Sending the
// header and then nothing reads as a message that failed to build.
func TestTheNudgeSaysTheBoardIsClearRatherThanSayingNothing(t *testing.T) {
	loc := pacific(t)
	coordinator, herald, _ := newNudgeFixture(t, time.Date(2026, 8, 20, 9, 0, 0, 0, loc),
		&model.Item{ID: "someday", Type: "todo", Title: "Buy green tea", Status: "open", Tags: []string{"personal"}})

	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}
	if err := coordinator.nudgeOnce(board, nil, nil); err != nil {
		t.Fatalf("nudgeOnce: %v", err)
	}
	if !strings.Contains(herald.sessions[0], "Nothing due today") {
		t.Errorf("a clear board produced a message with no statement in it:\n%s", herald.sessions[0])
	}
}

// Lateness is counted in calendar days, the way a person counts them. Across the
// spring-forward boundary five calendar days are 119 hours, so dividing by 24 and
// truncating reports "4d late" — the one day of the year the number is quietly
// wrong, and no fixture built on whole days would catch it.
func TestLatenessIsCountedInCalendarDaysAcrossADaylightSavingBoundary(t *testing.T) {
	loc := pacific(t)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 3, 10, 8, 0, 0, 0, loc), newFakeNoteboard())

	// Due Thu 5 Mar (PST), now Tue 10 Mar (PDT); DST began Sun 8 Mar.
	late := &model.Item{
		ID: "late", Type: "todo", Title: "thing", Status: "open", Tags: []string{"personal"},
		DueAt: ptr(time.Date(2026, 3, 5, 9, 0, 0, 0, loc)),
	}
	if got := coordinator.daysLate(late); got != 5 {
		t.Errorf("daysLate = %d, want 5 — an hour of daylight saving moved the count", got)
	}

	// Due yesterday at 9am, read at 8am today: a person calls that one day late,
	// not zero. Truncating the 23-hour gap would say 0.
	yesterday := &model.Item{
		ID: "yesterday", Type: "todo", Title: "thing", Status: "open", Tags: []string{"personal"},
		DueAt: ptr(time.Date(2026, 3, 9, 9, 0, 0, 0, loc)),
	}
	if got := coordinator.daysLate(yesterday); got != 1 {
		t.Errorf("daysLate = %d, want 1 for something due yesterday", got)
	}
}

// ---------------------------------------------------------------- the working day is not open time

// The reason the day-load line existed at all was to answer "is there room today
// for a big todo?", and for as long as the window was a bare 8am–10pm it answered
// wrong every weekday: eight of those hours are spoken for. A digest offering a
// Tuesday morning as free is offering time that is already gone.
func TestWorkHoursAreNotCountedAsOpenTime(t *testing.T) {
	loc := pacific(t)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 20, 8, 0, 0, 0, loc), newFakeNoteboard())

	// Nothing on the calendar at all. The window is 14 hours; work takes 8.5 of
	// them, so 5.5 are left and the whole of it is one stretch after 4:30pm.
	shape := coordinator.measureDay(nil)
	if shape.Free != 5*time.Hour+30*time.Minute {
		t.Errorf("free = %s, want 5h30m — the working day is being offered as open time", shape.Free)
	}
	if shape.BiggestOpen != 5*time.Hour+30*time.Minute {
		t.Errorf("biggest open stretch = %s, want 5h30m", shape.BiggestOpen)
	}

	line := coordinator.dayLoad(nil)
	// The line SAYS the working day rather than quietly subtracting it. A free
	// figure that dropped eight hours without explaining why reads as a bug.
	if !strings.Contains(line, "Work 8am–4:30pm") {
		t.Errorf("day-load line does not say where the missing hours went:\n%s", line)
	}
}

// Work is measured as a busy block, not by trimming the window down to the
// evening, and this is the difference: a meeting inside work hours is already
// paid for. Counting it again would report a normal working Tuesday as busier
// than a day off with the same meeting on it.
func TestAMeetingInsideWorkHoursCostsNoExtraFreeTime(t *testing.T) {
	loc := pacific(t)
	coordinator, _, _ := newTestCoordinator(t, time.Date(2026, 7, 20, 8, 0, 0, 0, loc), newFakeNoteboard())

	free := coordinator.measureDay([]CalendarEvent{{
		Summary: "Standup",
		Start:   time.Date(2026, 7, 20, 10, 0, 0, 0, loc),
		End:     time.Date(2026, 7, 20, 10, 30, 0, 0, loc),
	}}).Free
	if free != 5*time.Hour+30*time.Minute {
		t.Errorf("free = %s, want 5h30m — a meeting already inside work hours was charged twice", free)
	}

	// A meeting that straddles the end of the working day costs only the part
	// that lands after it.
	straddling := coordinator.measureDay([]CalendarEvent{{
		Summary: "Runs late",
		Start:   time.Date(2026, 7, 20, 16, 0, 0, 0, loc),
		End:     time.Date(2026, 7, 20, 17, 0, 0, 0, loc),
	}}).Free
	if straddling != 5*time.Hour {
		t.Errorf("free = %s, want 5h — only the half hour after 4:30pm should have been charged", straddling)
	}
}

// An empty calendar still has a working day in it. Rendering "nothing on the
// calendar" and stopping would say the day is wide open when 8.5 hours of it are
// not.
func TestAnEmptyCalendarStillReportsTheWorkingDay(t *testing.T) {
	loc := pacific(t)
	coordinator, herald, _ := newNudgeFixture(t, time.Date(2026, 7, 20, 9, 0, 0, 0, loc),
		&model.Item{ID: "late", Type: "todo", Title: "Laundry", Status: "open", Tags: []string{"personal"},
			DueAt: ptr(time.Date(2026, 7, 15, 10, 0, 0, 0, loc))})

	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}
	if err := coordinator.nudgeOnce(board, nil, nil); err != nil {
		t.Fatalf("nudgeOnce: %v", err)
	}
	message := herald.sessions[0]
	if !strings.Contains(message, "nothing on the calendar") {
		t.Errorf("nudge should say the calendar is empty:\n%s", message)
	}
	if !strings.Contains(message, "Work 8am–4:30pm") || !strings.Contains(message, "~5.5h free") {
		t.Errorf("an empty calendar still has a working day in it:\n%s", message)
	}
}

// A working day that cannot be parsed, or that ends before it begins, measures as
// no work at all — which hands back the whole day as open time. That is the wrong
// answer arrived at silently, so it is refused at startup instead.
func TestAnUnusableWorkingDayIsRefusedAtStartup(t *testing.T) {
	for _, arguments := range [][]string{
		{"-work-start", "half-eight"},
		{"-work-end", "25:00"},
		{"-work-start", "16:30", "-work-end", "08:00"},
		{"-work-start", "09:00", "-work-end", "09:00"},
	} {
		if _, err := parseFlags(arguments, noEnvironment); err == nil {
			t.Errorf("%v was accepted; an unusable working day makes the whole day read as free", arguments)
		}
	}

	// And the shipped default is the working day the user actually keeps.
	config, err := parseFlags(nil, noEnvironment)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if config.workStart != "08:00" || config.workEnd != "16:30" {
		t.Errorf("shipped working day is %s–%s, want 08:00–16:30", config.workStart, config.workEnd)
	}
}

// The todo buckets are capped and the day is not, because a backlog grows
// without limit and a day cannot. The cap also truncated in the worst place:
// chronological order puts the evening last, so a busy day hid the 8pm event —
// the one that says whether there is room tonight.
func TestTheNudgeShowsTheWholeDayIncludingTheEveningOnABusyDay(t *testing.T) {
	loc := pacific(t)
	now := time.Date(2026, 7, 20, 9, 0, 0, 0, loc)
	coordinator, herald, _ := newNudgeFixture(t, now,
		&model.Item{ID: "late", Type: "todo", Title: "Laundry", Status: "open", Tags: []string{"personal"},
			DueAt: ptr(time.Date(2026, 7, 15, 10, 0, 0, 0, loc))})

	events := []CalendarEvent{}
	for hour := 9; hour < 9+nudgeSectionLimit+1; hour++ {
		events = append(events, CalendarEvent{
			Summary: fmt.Sprintf("Meeting %d", hour),
			Start:   time.Date(2026, 7, 20, hour, 0, 0, 0, loc),
			End:     time.Date(2026, 7, 20, hour, 30, 0, 0, loc),
		})
	}
	events = append(events, CalendarEvent{
		Summary: "Salsa",
		Start:   time.Date(2026, 7, 20, 20, 0, 0, 0, loc),
		End:     time.Date(2026, 7, 20, 21, 0, 0, 0, loc),
	})

	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("buildBoard: %v", err)
	}
	if err := coordinator.nudgeOnce(board, events, nil); err != nil {
		t.Fatalf("nudgeOnce: %v", err)
	}
	message := herald.sessions[0]
	if !strings.Contains(message, "Salsa") {
		t.Errorf("the evening event was truncated away — it is the one the day-load line is about:\n%s", message)
	}
	if strings.Contains(message, "more in the note") {
		t.Errorf("the day's schedule should not be capped:\n%s", message)
	}
}

// main exits 2 on anything parseFlags returns. A rejection that only set the
// exit code would stop the one job on this box that reminds the user of
// anything and say nothing about why — a silent death in the job history.
func TestAnUnusableWorkingDayIsReportedAndNotJustRefused(t *testing.T) {
	realStderr := os.Stderr
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	os.Stderr = write
	_, parseErr := parseFlags([]string{"-work-end", "08:00"}, noEnvironment)
	write.Close()
	os.Stderr = realStderr

	reported, err := io.ReadAll(read)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if parseErr == nil {
		t.Fatal("a working day ending at its start was accepted")
	}
	if !strings.Contains(string(reported), "-work-end 08:00 is not after -work-start 08:00") {
		t.Errorf("the refusal was never printed, so the operator sees only exit 2:\n%s", reported)
	}
}

// ---------------------------------------------------------------- the weekend is not a working day

// The working hours are Monday to Friday. Charging them on a Saturday would hide
// the one day the overdue pile is actually reachable — and the line would say
// "Work 8am–4:30pm" on a day with no work in it, which is simply untrue.
func TestTheWeekendCarriesNoWorkingDay(t *testing.T) {
	loc := pacific(t)
	// 2026-07-18 is a Saturday; 2026-07-20 is the Monday after it.
	saturday, _, _ := newTestCoordinator(t, time.Date(2026, 7, 18, 9, 0, 0, 0, loc), newFakeNoteboard())
	monday, _, _ := newTestCoordinator(t, time.Date(2026, 7, 20, 9, 0, 0, 0, loc), newFakeNoteboard())

	if free := saturday.measureDay(nil).Free; free != 14*time.Hour {
		t.Errorf("Saturday free = %s, want the whole 14-hour window", free)
	}
	if free := monday.measureDay(nil).Free; free != 5*time.Hour+30*time.Minute {
		t.Errorf("Monday free = %s, want 5h30m — the working day stopped being charged", free)
	}

	weekendLine := saturday.dayLoad(nil)
	if strings.Contains(weekendLine, "Work 8am") {
		t.Errorf("the Saturday line claims a working day:\n%s", weekendLine)
	}
	if !strings.Contains(weekendLine, "No work today") || !strings.Contains(weekendLine, "~14h free") {
		t.Errorf("the Saturday line should say the day is entirely free:\n%s", weekendLine)
	}
	if !strings.Contains(monday.dayLoad(nil), "Work 8am–4:30pm") {
		t.Errorf("the Monday line dropped the working day:\n%s", monday.dayLoad(nil))
	}
}

// A day list that reads as nothing would hand every day back as wide open — the
// same silent wrong answer a backwards working day gives.
func TestAnUnusableWorkDayListIsRefusedAtStartup(t *testing.T) {
	for _, value := range []string{"", "Mon,Funday", "Mon,,Fri", "Mon,Tue,"} {
		if _, err := parseFlags([]string{"-work-days", value}, noEnvironment); err == nil {
			t.Errorf("-work-days %q was accepted", value)
		}
	}

	// The shipped list is Monday to Friday, and nothing else.
	config, err := parseFlags(nil, noEnvironment)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	days, err := parseWorkDays(config.workDays)
	if err != nil {
		t.Fatalf("the shipped work days do not parse: %v", err)
	}
	for _, weekday := range []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday} {
		if !days[weekday] {
			t.Errorf("%s is not a working day in the shipped default", weekday)
		}
	}
	for _, weekend := range []time.Weekday{time.Saturday, time.Sunday} {
		if days[weekend] {
			t.Errorf("%s ships as a working day", weekend)
		}
	}
}

// 2026-08-31, a Monday: the 7:30 digest opened a session titled "Personal todos
// — Aug 31", and a weekly nag with BYHOUR=9 opened a second one 90 minutes
// later with the SAME title but almost none of the content — two "Needs you"
// entries that read as a duplicate. The fix: the first relay of the day records
// its session id on the digest note, and every later relay that day is
// delivered into that session as a new turn.
func TestALaterFiringTheSameDayReusesTheMorningSession(t *testing.T) {
	loc := pacific(t)
	overdue := time.Date(2026, 8, 24, 9, 0, 0, 0, loc)
	nagged := &model.Item{
		ID: "apply", Type: "todo", Title: "Apply to jobs", Status: "open",
		Tags: []string{"personal", "career"}, DueAt: &overdue,
		Schedule: &model.Schedule{
			TZID:   "America/Los_Angeles",
			Remind: &model.Remind{Nag: "FREQ=WEEKLY;BYDAY=MO;BYHOUR=9;BYMINUTE=0;BYSECOND=0"},
		},
	}

	// 7:30: the digest opens a fresh session and records its id on the note.
	morning := time.Date(2026, 8, 31, 7, 30, 0, 0, loc)
	digestNote := &model.Item{
		ID: "digest", Type: "note", Title: digestTitle, Tags: []string{"personal", digestTag},
	}
	nb := newFakeNoteboard(nagged, digestNote)
	coordinator, herald, _ := newTestCoordinator(t, morning, nb)
	board, err := coordinator.buildBoard()
	if err != nil {
		t.Fatalf("morning buildBoard: %v", err)
	}
	if err := coordinator.nudgeOnce(board, nil, nil); err != nil {
		t.Fatalf("morning nudgeOnce: %v", err)
	}
	if len(herald.reused) != 1 || herald.reused[0] != "" {
		t.Fatalf("the day's first relay should open a new session, got reused=%q", herald.reused)
	}
	patchedLinks, ok := nb.patched["digest"]["links"].([]string)
	if !ok {
		t.Fatalf("the digest note was not patched with links after the first relay: %#v", nb.patched["digest"])
	}
	wantMarker := "herald-session:2026-08-31:herald-test-1"
	if !slices.Contains(patchedLinks, wantMarker) {
		t.Fatalf("links after the first relay = %q, want them to carry %q", patchedLinks, wantMarker)
	}

	// 9:00, same day: the nag fires. The note now carries both markers (the fake
	// Patch does not write back to the item, so state them as the real store
	// would hold them). The relay must land in the morning's session — and must
	// not re-patch an unchanged marker, which would mint a needless revision.
	nine := time.Date(2026, 8, 31, 9, 0, 0, 0, loc)
	digestNote.Links = []string{"digest-sent:2026-08-31", "herald-session:2026-08-31:herald-morning"}
	coordinator, herald, _ = newTestCoordinator(t, nine, nb)
	delete(nb.patched, "digest")
	board, err = coordinator.buildBoard()
	if err != nil {
		t.Fatalf("9am buildBoard: %v", err)
	}
	if len(board.NagFiring) != 1 {
		t.Fatalf("want the 9am nag firing, got %d", len(board.NagFiring))
	}
	if err := coordinator.nudgeOnce(board, nil, nil); err != nil {
		t.Fatalf("9am nudgeOnce: %v", err)
	}
	if len(herald.reused) != 1 || herald.reused[0] != "herald-morning" {
		t.Fatalf("the 9am nag should reuse the morning session, got reused=%q", herald.reused)
	}
	if _, patched := nb.patched["digest"]; patched {
		t.Errorf("reusing a session re-patched the note: %#v", nb.patched["digest"])
	}

	// Next Monday: yesterday's marker is stale, so the digest opens a new
	// session again rather than resurrecting last week's.
	nextMonday := time.Date(2026, 9, 7, 7, 30, 0, 0, loc)
	digestNote.Links = []string{"digest-sent:2026-08-31", "herald-session:2026-08-31:herald-morning"}
	nagged.Schedule.LastNaggedAt = &nine
	coordinator, herald, _ = newTestCoordinator(t, nextMonday, nb)
	board, err = coordinator.buildBoard()
	if err != nil {
		t.Fatalf("next-Monday buildBoard: %v", err)
	}
	if err := coordinator.nudgeOnce(board, nil, nil); err != nil {
		t.Fatalf("next-Monday nudgeOnce: %v", err)
	}
	if len(herald.reused) != 1 || herald.reused[0] != "" {
		t.Fatalf("a marker from a past day must not be reused, got reused=%q", herald.reused)
	}
}
