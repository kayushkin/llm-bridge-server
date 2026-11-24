package db

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kayushkin/noteboard/model"
)

func pacific(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}

func TestScheduleRoundTripsThroughTheStore(t *testing.T) {
	store := newTestStore(t)
	loc := pacific(t)
	anchor := time.Date(2026, 7, 21, 9, 0, 0, 0, loc)

	created, err := store.CreateItem(&model.CreateItemRequest{
		Type:  model.TypeTodo,
		Title: "Water the plants",
		Tags:  []string{"personal", "home"},
		Schedule: &model.Schedule{
			DTStart: &anchor,
			TZID:    "America/Los_Angeles",
			RRule:   "FREQ=MONTHLY;BYDAY=3TU",
			Mode:    model.ScheduleModeInstance,
			Remind:  &model.Remind{Lead: []string{"-P1D"}, Channels: []string{model.ChannelDigest, model.ChannelCalendar}},
		},
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	got, err := store.GetItem(created.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Schedule == nil {
		t.Fatal("schedule did not survive the round trip")
	}
	if got.Schedule.RRule != "FREQ=MONTHLY;BYDAY=3TU" {
		t.Errorf("rrule = %q, want the stored rule", got.Schedule.RRule)
	}
	if got.Schedule.TZID != "America/Los_Angeles" {
		t.Errorf("tzid = %q; without it the rule cannot survive a DST transition", got.Schedule.TZID)
	}
	if got.Schedule.DTStart == nil || !got.Schedule.DTStart.Equal(anchor) {
		t.Errorf("dtstart = %v, want %s", got.Schedule.DTStart, anchor)
	}
	if !got.Schedule.Remind.DeliversOn(model.ChannelCalendar) {
		t.Error("channels did not survive the round trip")
	}

	// And the rule is still expandable after a trip through SQLite — a schedule
	// that stores but no longer parses is the failure this whole field exists to
	// prevent.
	occurrences, err := got.Schedule.DueOccurrences(got.DueAt,
		time.Date(2026, 7, 1, 0, 0, 0, 0, loc),
		time.Date(2026, 9, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("DueOccurrences after round trip: %v", err)
	}
	if len(occurrences) != 2 {
		t.Fatalf("got %d occurrences after round trip, want 2", len(occurrences))
	}
}

// An unscheduled item must store SQL NULL, not "null" or "{}" — the partial index
// and the coordinator's scan are both built on `schedule IS NOT NULL`.
func TestUnscheduledItemStoresNull(t *testing.T) {
	store := newTestStore(t)
	created, err := store.CreateItem(&model.CreateItemRequest{Type: model.TypeTodo, Title: "no schedule"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	var schedule any
	if err := store.db.QueryRow("SELECT schedule FROM items WHERE id = ?", created.ID).Scan(&schedule); err != nil {
		t.Fatalf("scan schedule: %v", err)
	}
	if schedule != nil {
		t.Fatalf("unscheduled item stored %v, want SQL NULL — it would show up in the coordinator's scan", schedule)
	}

	scheduled, err := store.ListScheduledItems()
	if err != nil {
		t.Fatalf("ListScheduledItems: %v", err)
	}
	if len(scheduled) != 0 {
		t.Fatalf("ListScheduledItems returned %d items; none have a schedule", len(scheduled))
	}
}

func TestListScheduledItemsFindsOnlyScheduledOnes(t *testing.T) {
	store := newTestStore(t)
	loc := pacific(t)
	anchor := time.Date(2026, 7, 21, 9, 0, 0, 0, loc)

	for i := 0; i < 3; i++ {
		if _, err := store.CreateItem(&model.CreateItemRequest{Type: model.TypeTodo, Title: "plain todo"}); err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
	}
	want, err := store.CreateItem(&model.CreateItemRequest{
		Type:     model.TypeTodo,
		Title:    "recurring",
		Schedule: &model.Schedule{DTStart: &anchor, TZID: "America/Los_Angeles", RRule: "FREQ=WEEKLY"},
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	got, err := store.ListScheduledItems()
	if err != nil {
		t.Fatalf("ListScheduledItems: %v", err)
	}
	if len(got) != 1 || got[0].ID != want.ID {
		t.Fatalf("ListScheduledItems = %d items, want exactly the one scheduled item", len(got))
	}
}

// `"schedule": null` is how a recurrence is taken off an item. Without presence
// tracking this was indistinguishable from "don't touch it", and a rule could
// never be removed.
func TestScheduleCanBeCleared(t *testing.T) {
	store := newTestStore(t)
	loc := pacific(t)
	anchor := time.Date(2026, 7, 21, 9, 0, 0, 0, loc)

	created, err := store.CreateItem(&model.CreateItemRequest{
		Type:     model.TypeTodo,
		Title:    "recurring",
		Schedule: &model.Schedule{DTStart: &anchor, TZID: "America/Los_Angeles", RRule: "FREQ=WEEKLY"},
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	var req model.UpdateItemRequest
	if err := json.Unmarshal([]byte(`{"schedule": null}`), &req); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if !req.HasSchedule {
		t.Fatal(`"schedule": null did not register as present; the rule could never be removed`)
	}

	updated, err := store.UpdateItem(created.ID, &req)
	if err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	if updated.Schedule != nil {
		t.Fatal("schedule survived an explicit null")
	}

	scheduled, err := store.ListScheduledItems()
	if err != nil {
		t.Fatalf("ListScheduledItems: %v", err)
	}
	if len(scheduled) != 0 {
		t.Fatal("cleared schedule still shows up in the coordinator's scan")
	}
}

// A PATCH that leaves a schedule in place while clearing the due date it anchors
// on strands the rule: it has nothing to count from, so it expands to nothing and
// silently reminds no one. Neither half of the patch looks wrong on its own.
func TestClearingDueDateThatARuleAnchorsOnIsRejected(t *testing.T) {
	store := newTestStore(t)
	loc := pacific(t)
	due := time.Date(2026, 6, 30, 9, 0, 0, 0, loc)

	created, err := store.CreateItem(&model.CreateItemRequest{
		Type:  model.TypeTodo,
		Title: "Call the tag agency",
		DueAt: &due,
		// Nag only, no dtstart: the rule anchors on the item's due date.
		Schedule: &model.Schedule{
			TZID:   "America/Los_Angeles",
			Remind: &model.Remind{Nag: "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR"},
		},
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	var req model.UpdateItemRequest
	if err := json.Unmarshal([]byte(`{"due_at": null}`), &req); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if _, err := store.UpdateItem(created.ID, &req); err == nil {
		t.Fatal("clearing the due date a nag anchors on was accepted; the nag would silently stop firing")
	}

	// The item is untouched — a rejected patch must not half-apply.
	after, err := store.GetItem(created.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if after.DueAt == nil || !after.DueAt.Equal(due) {
		t.Fatalf("due date = %v after a rejected patch, want it untouched at %s", after.DueAt, due)
	}
}

// due_at must be clearable when nothing depends on it — a rolling series that has
// run out has to be able to put the item down.
func TestDueDateCanBeClearedWhenNoRuleAnchorsOnIt(t *testing.T) {
	store := newTestStore(t)
	loc := pacific(t)
	due := time.Date(2026, 6, 30, 9, 0, 0, 0, loc)

	created, err := store.CreateItem(&model.CreateItemRequest{Type: model.TypeTodo, Title: "one-shot", DueAt: &due})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	var req model.UpdateItemRequest
	if err := json.Unmarshal([]byte(`{"due_at": null}`), &req); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	updated, err := store.UpdateItem(created.ID, &req)
	if err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	if updated.DueAt != nil {
		t.Fatalf("due_at = %v after an explicit null, want cleared", updated.DueAt)
	}
}

// A schedule that stores but no longer parses would make the coordinator expand
// it to nothing and remind nobody, forever — invisible by construction. It has to
// fail loudly at the read, not degrade to "no schedule".
func TestUnreadableScheduleFailsLoudly(t *testing.T) {
	store := newTestStore(t)
	created, err := store.CreateItem(&model.CreateItemRequest{Type: model.TypeTodo, Title: "corrupt"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := store.db.Exec("UPDATE items SET schedule = ? WHERE id = ?", "{not json", created.ID); err != nil {
		t.Fatalf("corrupt the row: %v", err)
	}

	if _, err := store.GetItem(created.ID); err == nil {
		t.Fatal("an unreadable schedule was silently swallowed; the item would just quietly stop reminding")
	}
}
