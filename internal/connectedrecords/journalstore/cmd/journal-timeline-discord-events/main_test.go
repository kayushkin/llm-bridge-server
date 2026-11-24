package main

import "testing"

func TestOwnedItemsAreKnownByTheirOpening(t *testing.T) {
	for body, want := range map[string]bool{
		"Went to [Bowling](ref:discord_event_000001)":       true,
		"Both went to [Bowling](ref:discord_event_000001)":  true,
		"Signed up for [Bowling](ref:discord_event_000001)": true,
		"A long table of the group eating wings":            false,
	} {
		if got := owned(timelineItem{Body: body}); got != want {
			t.Errorf("owned(%q) = %v", body, got)
		}
	}
}

func TestAnItemHoldsADateWithinADayAndAHalf(t *testing.T) {
	item := timelineItem{OccurredAt: 1000000}
	if !sameDate(item, 1000000+30*3600) || sameDate(item, 1000000-7*86400) {
		t.Error("sameDate window is wrong")
	}
	if !cites(timelineItem{Sources: []string{"message_000001", "discord_event_000002"}}, "discord_event_000002") {
		t.Error("cites missed a source")
	}
}
