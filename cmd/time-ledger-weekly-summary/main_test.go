package main

import (
	"testing"
	"time"

	timeledgerstore "github.com/kayushkin/time-ledger-store"
)

func TestWeekWindowCountsSevenLocalDays(t *testing.T) {
	zone, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	from, to, err := weekWindow("2026-11-09", zone, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Unix(from, 0).In(zone).Format("2006-01-02 15:04"); got != "2026-11-02 00:00" {
		t.Errorf("from = %s", got)
	}
	if got := time.Unix(to, 0).In(zone).Format("2006-01-02 15:04"); got != "2026-11-09 00:00" {
		t.Errorf("to = %s", got)
	}
	// 2026-11-01 is the end of daylight time, so a week across it is 169 hours.
	from, to, err = weekWindow("2026-11-03", zone, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if hours := (to - from) / 3600; hours != 169 {
		t.Errorf("week across the clock change is %d hours, want 169", hours)
	}
	now := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC) // 2026-11-12 18:00 in Reno
	_, to, err = weekWindow("", zone, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Unix(to, 0).In(zone).Format("2006-01-02"); got != "2026-11-12" {
		t.Errorf("empty -week-ending ends the week at %s, want the local today 2026-11-12", got)
	}
	if _, _, err := weekWindow("11/09/2026", zone, now); err == nil {
		t.Error("a malformed -week-ending was accepted")
	}
}

func TestBuildWeekPayloadSumsLiveMoneyAndLeavesFreeEntriesWithout(t *testing.T) {
	zone := time.UTC
	removed := int64(5)
	cents := func(value int64) *int64 { return &value }
	entries := []timeledgerstore.TimeEntry{
		{ID: "time_entry_000001", Title: "dinner", StartsAt: 100, EndsAt: 200, Outcome: "done", OutcomeReviewID: "time_entry_review_000001",
			Kinds: []timeledgerstore.KindAssignment{{Name: "social"}, {Name: "meal"}},
			Links: []timeledgerstore.Link{
				{LinkType: "transactions", Ref: "transaction_000001", AmountCents: cents(2500)},
				{LinkType: "transactions", Ref: "transaction_000002", AmountCents: cents(900), RemovedAt: &removed},
				{LinkType: "visits", Ref: "visit_000001"},
			}},
		{ID: "time_entry_000002", Title: "walk", StartsAt: 300, EndsAt: 400, Outcome: "unknown"},
	}
	reviews := map[string]timeledgerstore.Review{"time_entry_000001": {Reason: "a charge there", ReviewedBy: "time-ledger-review (model)"}}
	payload := buildWeekPayload(0, 7*86400, zone, entries, reviews)

	if total := payload["amount_cents_total"].(int64); total != 2500 {
		t.Errorf("total = %d, want 2500 (the removed link left out)", total)
	}
	if count := payload["entries_with_money"].(int); count != 1 {
		t.Errorf("entries_with_money = %d", count)
	}
	shown := payload["entries"].([]map[string]any)
	if len(shown) != 2 {
		t.Fatalf("%d entries shown", len(shown))
	}
	first, second := shown[0], shown[1]
	if counts := first["records_linked"].(map[string]int); counts["transactions"] != 1 || counts["visits"] != 1 {
		t.Errorf("records_linked = %v", counts)
	}
	if kinds := first["kinds"].([]string); len(kinds) != 2 || kinds[0] != "meal" {
		t.Errorf("kinds = %v, want sorted", kinds)
	}
	if first["outcome_reason"] != "a charge there" {
		t.Errorf("outcome_reason = %v", first["outcome_reason"])
	}
	if _, has := second["amount_cents"]; has {
		t.Error("an entry with no money link carries amount_cents")
	}
	if _, has := second["outcome_reason"]; has {
		t.Error("an entry with no review carries outcome_reason")
	}
}
