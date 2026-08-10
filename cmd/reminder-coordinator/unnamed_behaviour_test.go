package main

// Behaviour in this scorer's target that no test in this package named.
//
// Three functions here were executed by the digest tests and asserted by none
// of them. Each could have its whole body replaced by a zero-value return with
// the package still reporting ok:
//
//	sortByDue     the order every reminder surface is rendered in
//	dryPrefix     the marker that says a run did not touch anything
//	roundDuration the age each overdue item is reported with

import (
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/noteboard/model"
)

func itemDue(title string, due *time.Time) *model.Item {
	return &model.Item{Title: title, DueAt: due}
}

func at(day int) *time.Time {
	t := time.Date(2026, 8, day, 9, 0, 0, 0, time.UTC)
	return &t
}

// The "📆 Today — personal" note is the user's primary surface and it is
// rendered straight out of this order. Emptying sortByDue leaves the items in
// whatever order noteboard happened to return them, which reads as a sorted
// list that is quietly wrong rather than as an obvious failure.
func TestSortByDue_SoonestFirstAndUndatedLast(t *testing.T) {
	items := []*model.Item{
		itemDue("no due date", nil),
		itemDue("thursday", at(13)),
		itemDue("monday", at(10)),
		itemDue("also no due date", nil),
		itemDue("wednesday", at(12)),
	}
	sortByDue(items)

	var got []string
	for _, item := range items {
		got = append(got, item.Title)
	}
	want := []string{"monday", "wednesday", "thursday", "no due date", "also no due date"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("sortByDue order = %v, want %v", got, want)
	}
}

// Two undated items must keep the order they arrived in — the sort is stable so
// that a list the caller already ordered does not get shuffled underneath it.
func TestSortByDue_IsStableAmongUndatedItems(t *testing.T) {
	items := []*model.Item{itemDue("first", nil), itemDue("second", nil), itemDue("third", nil)}
	sortByDue(items)
	for index, want := range []string{"first", "second", "third"} {
		if items[index].Title != want {
			t.Errorf("position %d = %q, want %q — undated items must not be reordered", index, items[index].Title, want)
		}
	}
}

// dryPrefix is the only thing distinguishing a rehearsal's log from a run that
// really mirrored items to Google Calendar and really opened a herald session.
// If it returns the empty string on a dry run, the two are indistinguishable
// after the fact — and this coordinator is the single thing on the box allowed
// to interrupt the user.
func TestDryPrefix_MarksARehearsalAndOnlyARehearsal(t *testing.T) {
	if got := dryPrefix(true); got != "[dry-run] " {
		t.Errorf("dryPrefix(true) = %q, want %q — an unmarked rehearsal reads as a real run", got, "[dry-run] ")
	}
	if got := dryPrefix(false); got != "" {
		t.Errorf("dryPrefix(false) = %q, want empty — a real run must not claim to be a rehearsal", got)
	}
}

// roundDuration reports how overdue something is. Its three branches are three
// different scales and the boundaries between them are where it reads wrong.
func TestRoundDuration_EachScaleAndItsBoundary(t *testing.T) {
	for _, testCase := range []struct {
		name string
		in   time.Duration
		want string
	}{
		{"zero", 0, "0h"},
		{"negative is not a negative age", -3 * time.Hour, "0h"},
		{"under an hour reports minutes", 42 * time.Minute, "42m"},
		{"minutes round to the nearest", 90 * time.Second, "2m"},
		{"exactly an hour crosses to the hour scale", time.Hour, "1h"},
		{"a whole number of hours has no decimal", 3 * time.Hour, "3h"},
		{"a half hour past shows one decimal", 90 * time.Minute, "1.5h"},
		{"hours round to the nearest half", 100 * time.Minute, "1.5h"},
		{"just under the half rounds down", 105*time.Minute - time.Second, "1.5h"},
		{"days are still reported in hours", 50 * time.Hour, "50h"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := roundDuration(testCase.in); got != testCase.want {
				t.Errorf("roundDuration(%v) = %q, want %q", testCase.in, got, testCase.want)
			}
		})
	}
}
