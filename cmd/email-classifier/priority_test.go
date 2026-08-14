package main

import (
	"testing"
	"time"
)

func mustParseDue(t *testing.T, raw string) *time.Time {
	t.Helper()
	due, err := parseDueDate(raw, time.UTC)
	if err != nil {
		t.Fatalf("parseDueDate(%q): %v", raw, err)
	}
	return &due
}

// TestActionGateBeatsCategory pins the asymmetry that makes this board readable.
// Every one of these carries a heavyweight category and owes nothing.
func TestActionGateBeatsCategory(t *testing.T) {
	gated := []struct{ category, action string }{
		{"housing", "none"}, // "Zillow rental listings — Reno area"
		{"finance", "file"}, // "Payment & purchase receipts"
		{"health", "read"},  // a health newsletter is still a newsletter
		{"government-legal", "none"},
	}
	for _, c := range gated {
		if got := stakesOf(c.category, c.action); got != stakesNone {
			t.Errorf("stakesOf(%q, %q) = %v, want none: a card owing nothing must not be lifted by its category",
				c.category, c.action, got)
		}
	}
}

// TestUnknownActionFallsThroughRatherThanGating guards the classifier-misbehaved
// case. Scoring an unrecognized value to zero would bury the card that most
// needs looking at.
func TestUnknownActionFallsThroughRatherThanGating(t *testing.T) {
	if got := stakesOf("finance", "wat"); got != stakesSevere {
		t.Errorf("stakesOf(finance, wat) = %v, want severe from the category", got)
	}
	if got := stakesOf("promotions", "wat"); got != stakesNone {
		t.Errorf("stakesOf(promotions, wat) = %v, want none", got)
	}
}

// TestExpiryRoutesByStakes is the distinction the old single urgency axis lost:
// one of these is over, the other is getting worse.
func TestExpiryRoutesByStakes(t *testing.T) {
	now := time.Date(2026, 8, 13, 22, 0, 0, 0, time.UTC)
	yesterday := mustParseDue(t, "2026-08-12")

	rent := scoreCard("housing", "pay", "high", yesterday, now)
	if rent.Priority != maxPriority {
		t.Errorf("overdue rent scored %d, want %d: a missed payment is not history", rent.Priority, maxPriority)
	}

	concert := scoreCard("social-events", "schedule", "critical", yesterday, now)
	if concert.Priority >= rent.Priority {
		t.Errorf("lapsed concert scored %d, rent %d: they must not rank alike", concert.Priority, rent.Priority)
	}
	if concert.Priority == 0 {
		t.Error("lapsed concert scored 0; it should sink but stay visible until archived by hand")
	}
}

// TestDeadlineDrivesUrgencyOnceKnown reproduces the live drift that motivated
// storing a date at all: on Aug 13 an Aug 14 deadline must read critical, no
// matter that the model called it "normal" when it filed the mail on Aug 11.
func TestDeadlineDrivesUrgencyOnceKnown(t *testing.T) {
	now := time.Date(2026, 8, 13, 22, 0, 0, 0, time.UTC)
	got := scoreCard("social-events", "schedule", "normal", mustParseDue(t, "2026-08-14"), now)
	if got.Urgency != "critical" {
		t.Errorf("urgency = %q, want critical: Aug 14 is inside 24h of Aug 13 22:00", got.Urgency)
	}
}

// TestMissingDueDateLeavesStoredUrgencyAlone protects the 75 cards that predate
// due_at. The sweep may rank them; it may not overwrite the model's reading with
// a derived one, least of all into a column noteboard cannot roll back.
func TestMissingDueDateLeavesStoredUrgencyAlone(t *testing.T) {
	now := time.Date(2026, 8, 13, 22, 0, 0, 0, time.UTC)
	got := scoreCard("finance", "pay", "critical", nil, now)
	if got.Urgency != "" {
		t.Errorf("urgency = %q, want empty: with no due date there is nothing better to write", got.Urgency)
	}
	if got.Priority == 0 {
		t.Error("a severe card with no due date scored 0; it should still rank on stakes")
	}
}

// TestBareDateIsDueAtEndOfDay stops a whole class of cards expiring a day early.
func TestBareDateIsDueAtEndOfDay(t *testing.T) {
	due := mustParseDue(t, "2026-08-14")
	morning := time.Date(2026, 8, 14, 0, 1, 0, 0, time.UTC)
	if bucketForDue(*due, morning) == deadlineOverdue {
		t.Error("a bill due Aug 14 is not overdue at 00:01 on Aug 14")
	}
	nextDay := time.Date(2026, 8, 15, 0, 1, 0, 0, time.UTC)
	if bucketForDue(*due, nextDay) != deadlineOverdue {
		t.Error("it is overdue the following day")
	}
}

func TestParseDueDateRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "  ", "next tuesday", "08/14/2026", "2026-13-45"} {
		if _, err := parseDueDate(raw, time.UTC); err == nil {
			t.Errorf("parseDueDate(%q) succeeded; a dropped deadline must be loud", raw)
		}
	}
	if _, err := parseDueDate("2026-08-14T18:00:00Z", time.UTC); err != nil {
		t.Errorf("RFC3339 rejected: %v", err)
	}
}

// TestPriorityRisesAsDeadlineApproaches is the monotonicity the whole scheme
// rests on: for fixed stakes, less time left never scores lower.
func TestPriorityRisesAsDeadlineApproaches(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	for _, s := range []stakes{stakesMinor, stakesReal, stakesSevere} {
		previous := -1
		for _, bucket := range []deadlineBucket{deadlineNone, deadlineFar, deadlineWeek, deadlineSoon, deadlineToday} {
			got := priorityMatrix[s][bucket]
			if got < previous {
				t.Errorf("stakes %v: priority fell to %d at bucket %d", s, got, bucket)
			}
			previous = got
		}
	}
	_ = now
}

// TestBucketBoundaries pins the thresholds against the vocabulary the model is
// told to use, so the two scoring paths cannot drift apart.
func TestBucketBoundaries(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		offset time.Duration
		want   deadlineBucket
	}{
		{-time.Hour, deadlineOverdue},
		{12 * time.Hour, deadlineToday},
		{2 * 24 * time.Hour, deadlineSoon},
		{10 * 24 * time.Hour, deadlineWeek},
		{40 * 24 * time.Hour, deadlineFar},
	}
	for _, c := range cases {
		if got := bucketForDue(now.Add(c.offset), now); got != c.want {
			t.Errorf("offset %v: bucket %d, want %d", c.offset, got, c.want)
		}
	}
}

// TestRealBoardCardsRankSensibly is the end-to-end sanity check, using cards
// actually on the Email board on 2026-08-13 with the urgency each carried.
func TestRealBoardCardsRankSensibly(t *testing.T) {
	now := time.Date(2026, 8, 13, 22, 0, 0, 0, time.UTC)
	type card struct {
		title            string
		category, action string
		urgency          string
		due              *time.Time
	}
	cards := []card{
		{"Pay August rent to Real Property Management", "housing", "pay", "high", nil},
		{"Domain renewal - expires in 24 hours", "dev-infra", "pay", "critical", mustParseDue(t, "2026-08-12")},
		{"NNBA Dog Days of Summer performance Aug 14", "social-events", "schedule", "normal", mustParseDue(t, "2026-08-14")},
		{"Blue Öyster Cult free concert at GSR tonight", "social-events", "schedule", "critical", mustParseDue(t, "2026-08-10")},
		{"Confirm Nevada DMV appointment Sept 5 8am", "government-legal", "schedule", "critical", mustParseDue(t, "2026-09-05")},
		{"Meetup event digests", "social-events", "none", "low", nil},
		{"Uber promotions and features", "promotions", "none", "low", nil},
	}
	got := map[string]int{}
	for _, c := range cards {
		got[c.title] = scoreCard(c.category, c.action, c.urgency, c.due, now).Priority
	}

	// The two buckets are noise and must sit at the floor.
	for _, title := range []string{"Meetup event digests", "Uber promotions and features"} {
		if got[title] != 0 {
			t.Errorf("%q scored %d, want 0", title, got[title])
		}
	}
	// The overdue domain renewal outranks the DMV appointment three weeks out —
	// the inversion the frozen-urgency scheme produced, now corrected.
	if got["Domain renewal - expires in 24 hours"] <= got["Confirm Nevada DMV appointment Sept 5 8am"] {
		t.Errorf("overdue renewal (%d) must outrank a Sept 5 appointment (%d)",
			got["Domain renewal - expires in 24 hours"], got["Confirm Nevada DMV appointment Sept 5 8am"])
	}
	// Tomorrow's performance outranks the show that already happened.
	if got["NNBA Dog Days of Summer performance Aug 14"] <= got["Blue Öyster Cult free concert at GSR tonight"] {
		t.Errorf("tomorrow's performance (%d) must outrank a lapsed concert (%d)",
			got["NNBA Dog Days of Summer performance Aug 14"], got["Blue Öyster Cult free concert at GSR tonight"])
	}
}
