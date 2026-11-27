package spend

import (
	"strings"
	"testing"
)

// The bucket date used to be cut out of starting_at with a fixed byte slice,
// b.StartingAt[:10]. These tests drive dailyFromBuckets — the loop that
// actually consumes the value — rather than bucketDate alone, because the
// helper was never the thing that was broken; the call site was.
//
// A short starting_at makes the old spelling PANIC, so the short-input cases
// below fail by crashing the test binary, not by returning a wrong string.
// That is the point of the card these came from: this site is ASCII by
// construction and can never split a rune, so a reviewer hunting corruption
// passes straight over the only defect it has.

func bucket(startingAt string, results ...rawUsageResult) rawUsageBucket {
	return rawUsageBucket{StartingAt: startingAt, Results: results}
}

func sonnet(apiKeyID string, outputTokens int64) rawUsageResult {
	return rawUsageResult{APIKeyID: apiKeyID, Model: "claude-sonnet-4-5", OutputTokens: outputTokens}
}

// KNOWN-NEGATIVE CONTROL. A well-formed UTC bucket must keep working, and this
// case passes against the unfixed byte-slice too. Without it the suite cannot
// tell "rejects a malformed date" from "rejects everything".
func TestDailyFromBuckets_WellFormedUTCBucketKeepsItsDay(t *testing.T) {
	daily, err := dailyFromBuckets([]rawUsageBucket{
		bucket("2026-08-01T00:00:00Z", sonnet("apikey_a", 1_000_000)),
	}, 1700000000, nil)
	if err != nil {
		t.Fatalf("well-formed bucket rejected: %v", err)
	}
	if len(daily) != 1 {
		t.Fatalf("want 1 row, got %d", len(daily))
	}
	if daily[0].Date != "2026-08-01" {
		t.Errorf("Date = %q, want 2026-08-01", daily[0].Date)
	}
	if daily[0].Provider != "anthropic" || daily[0].APIKeyID != "apikey_a" {
		t.Errorf("row = %+v, want provider anthropic / key apikey_a", daily[0])
	}
	if daily[0].FetchedAt != 1700000000 {
		t.Errorf("FetchedAt = %d, want the value passed in", daily[0].FetchedAt)
	}
	if daily[0].TotalUSD != 15 {
		t.Errorf("TotalUSD = %v, want 15 (1M sonnet output tokens at $15/MTok)", daily[0].TotalUSD)
	}
}

// An omitted starting_at decodes to "", which is where the byte slice panics.
// Go's zero value gives no hint the field was ever missing, so nothing upstream
// of here can catch it first.
func TestDailyFromBuckets_MissingStartingAtIsRefusedNotPanicked(t *testing.T) {
	_, err := dailyFromBuckets([]rawUsageBucket{
		bucket("", sonnet("apikey_a", 1_000_000)),
	}, 1700000000, nil)
	if err == nil {
		t.Fatal("empty starting_at accepted; want an error")
	}
	if !strings.Contains(err.Error(), "starting_at") {
		t.Errorf("error %q does not name the offending field", err)
	}
}

// Ten bytes is the exact boundary the old cut assumed. Anything shorter
// panicked; these are the values that would have taken the refresh handler
// down with "slice bounds out of range".
func TestDailyFromBuckets_ShortStartingAtIsRefusedNotPanicked(t *testing.T) {
	for _, short := range []string{"2", "2026", "2026-08", "2026-08-0"} {
		if _, err := dailyFromBuckets([]rawUsageBucket{
			bucket(short, sonnet("apikey_a", 1_000_000)),
		}, 1700000000, nil); err == nil {
			t.Errorf("starting_at %q accepted; want an error", short)
		}
	}
}

// The silent half of the defect, and the half no length check would have
// caught. These are all at least ten bytes, so the old cut did not panic — it
// returned a ten-byte prefix that is not a date and filed real dollars under
// it. Date is half the primary key of a spend row, so the bad key then matched
// itself on every later fetch and looked stable.
func TestDailyFromBuckets_LongButUnparseableStartingAtIsRefused(t *testing.T) {
	for _, bad := range []string{
		"not-a-timestamp-at-all",
		"01/08/2026T00:00:00Z",
		"2026-13-45T00:00:00Z",
		"          ",
	} {
		got, err := dailyFromBuckets([]rawUsageBucket{
			bucket(bad, sonnet("apikey_a", 1_000_000)),
		}, 1700000000, nil)
		if err == nil {
			t.Errorf("starting_at %q accepted, produced Date %q; want an error", bad, got[0].Date)
		}
	}
}

// DailySpend.Date is documented as YYYY-MM-DD UTC and the fetch window is built
// from UTC midnights, but the byte cut took whatever day the string happened to
// be written in. An offset timestamp late in the day therefore landed on the
// wrong row entirely.
func TestDailyFromBuckets_OffsetTimestampIsNormalizedToItsUTCDay(t *testing.T) {
	daily, err := dailyFromBuckets([]rawUsageBucket{
		bucket("2026-08-01T23:30:00-08:00", sonnet("apikey_a", 1_000_000)),
	}, 1700000000, nil)
	if err != nil {
		t.Fatalf("offset timestamp rejected: %v", err)
	}
	if daily[0].Date != "2026-08-02" {
		t.Errorf("Date = %q, want 2026-08-02 (the UTC day); the byte cut gave 2026-08-01", daily[0].Date)
	}
}

// One bucket, two keys, one row each — and a key with no results gets no row
// at all rather than a zero-dollar one.
func TestDailyFromBuckets_PartitionsOneBucketPerAPIKey(t *testing.T) {
	daily, err := dailyFromBuckets([]rawUsageBucket{
		bucket("2026-08-01T00:00:00Z",
			sonnet("apikey_a", 1_000_000),
			sonnet("apikey_b", 2_000_000),
			sonnet("apikey_a", 1_000_000),
		),
		bucket("2026-08-02T00:00:00Z"),
	}, 1700000000, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daily) != 2 {
		t.Fatalf("want 2 rows (two keys on day one, none on day two), got %d: %+v", len(daily), daily)
	}
	byKey := map[string]float64{}
	for _, d := range daily {
		if d.Date != "2026-08-01" {
			t.Errorf("row %+v on the wrong day", d)
		}
		byKey[d.APIKeyID] += d.TotalUSD
	}
	if byKey["apikey_a"] != 30 {
		t.Errorf("apikey_a = %v, want 30 (two results summed into one row)", byKey["apikey_a"])
	}
	if byKey["apikey_b"] != 30 {
		t.Errorf("apikey_b = %v, want 30", byKey["apikey_b"])
	}
}

// A refusal has to abort the whole fetch, not drop the one bad bucket and
// persist the rest. A partial Daily written into the store is indistinguishable
// from a period of genuinely zero spend.
func TestDailyFromBuckets_OneBadBucketDiscardsTheWholeBatch(t *testing.T) {
	daily, err := dailyFromBuckets([]rawUsageBucket{
		bucket("2026-08-01T00:00:00Z", sonnet("apikey_a", 1_000_000)),
		bucket("", sonnet("apikey_a", 1_000_000)),
		bucket("2026-08-03T00:00:00Z", sonnet("apikey_a", 1_000_000)),
	}, 1700000000, nil)
	if err == nil {
		t.Fatal("batch containing a malformed bucket accepted; want an error")
	}
	if daily != nil {
		t.Errorf("rows returned alongside the error: %+v", daily)
	}
}
