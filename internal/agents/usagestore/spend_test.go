package usagestore

import (
	"testing"
	"time"
)

// The spend tables are the money half of this store: what each API key cost,
// what credit was bought, and therefore what is left. Measured on `main` with a
// panic() at function entry, every function in this file except migrateSpend
// left `go test ./...` green — nothing executed them at all.

// day renders a unix second as the YYYY-MM-DD UTC key spend_daily is stored on,
// so a test can say "three days ago" without duplicating the format string the
// production code uses.
func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

func daysAgo(n int) time.Time { return time.Now().UTC().AddDate(0, 0, -n) }

func saveDaily(t *testing.T, s *Store, provider, keyID string, when time.Time, usd float64) {
	t.Helper()
	err := s.SaveDailySpend(DailySpend{
		Provider: provider, APIKeyID: keyID, Date: day(when), TotalUSD: usd,
	})
	if err != nil {
		t.Fatalf("SaveDailySpend(%s/%s/%s): %v", provider, keyID, day(when), err)
	}
}

func saveKey(t *testing.T, s *Store, m KeyMeta) {
	t.Helper()
	if err := s.SaveKeyMeta(m); err != nil {
		t.Fatalf("SaveKeyMeta(%s/%s): %v", m.Provider, m.APIKeyID, err)
	}
}

// ---- containsAny ----

func TestContainsAnyReportsWhichOfSeveralSubstringsIsPresent(t *testing.T) {
	if !containsAny("CREATE TABLE spend_keys (total_usd_30d REAL)", "total_usd_24h", "total_usd_30d") {
		t.Fatal("the second substring is present and containsAny said no")
	}
	if !containsAny("total_usd_24h", "total_usd_24h") {
		t.Fatal("the only substring is present and containsAny said no")
	}
	if containsAny("CREATE TABLE spend_keys (fetched_at INTEGER)", "total_usd_24h", "total_usd_30d") {
		t.Fatal("neither substring is present and containsAny said yes")
	}
}

func TestContainsAnyWithNoSubstringsIsFalse(t *testing.T) {
	// The empty case decides what migrateSpend does with a schema it was given
	// nothing to look for: it must not read as a match and drop a live table.
	if containsAny("anything at all") {
		t.Fatal("containsAny with no substrings must be false, not vacuously true")
	}
}

// ---- migrateSpend ----

func TestMigrateSpendDropsTheSupersededTotalsLayout(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.db.Exec(`DROP TABLE spend_keys`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	// The layout migrateSpend exists to remove: per-window totals frozen onto
	// the key row instead of computed from spend_daily.
	_, err := s.db.Exec(`
		CREATE TABLE spend_keys (
			provider TEXT NOT NULL, api_key_id TEXT NOT NULL,
			total_usd_24h REAL, total_usd_7d REAL, total_usd_30d REAL,
			fetched_at INTEGER NOT NULL,
			PRIMARY KEY (provider, api_key_id));
		INSERT INTO spend_keys VALUES ('anthropic', 'key_old', 1, 2, 3, 100);`)
	if err != nil {
		t.Fatalf("create old layout: %v", err)
	}

	if err := s.migrateSpend(); err != nil {
		t.Fatalf("migrateSpend: %v", err)
	}

	// The old row is gone with the old table, and the new layout accepts a write.
	saveKey(t, s, KeyMeta{Provider: "anthropic", APIKeyID: "key_new", APIKeyName: "new", FetchedAt: 7})
	metas, err := s.ListKeyMeta("anthropic")
	if err != nil {
		t.Fatalf("ListKeyMeta: %v", err)
	}
	if len(metas) != 1 || metas[0].APIKeyID != "key_new" {
		t.Fatalf("want only the new key after the drop, got %+v", metas)
	}
}

func TestMigrateSpendKeepsTheCurrentLayoutAndItsRows(t *testing.T) {
	s := newTestStore(t)
	saveKey(t, s, KeyMeta{Provider: "anthropic", APIKeyID: "key_a", APIKeyName: "a", FetchedAt: 5})

	// Re-running the migration on an up-to-date schema must not be destructive:
	// Open() calls it on every start, so a drop here would wipe spend history
	// on each restart.
	if err := s.migrateSpend(); err != nil {
		t.Fatalf("migrateSpend: %v", err)
	}

	metas, err := s.ListKeyMeta("anthropic")
	if err != nil {
		t.Fatalf("ListKeyMeta: %v", err)
	}
	if len(metas) != 1 || metas[0].APIKeyID != "key_a" {
		t.Fatalf("re-running the migration lost the row: %+v", metas)
	}
}

// ---- SaveKeyMeta / ListKeyMeta / LatestKeyRaw ----

func TestSaveKeyMetaRequiresBothHalvesOfItsPrimaryKey(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveKeyMeta(KeyMeta{APIKeyID: "key_a", FetchedAt: 1}); err == nil {
		t.Fatal("a key meta with no provider was accepted")
	}
	if err := s.SaveKeyMeta(KeyMeta{Provider: "anthropic", FetchedAt: 1}); err == nil {
		t.Fatal("a key meta with no api_key_id was accepted")
	}
	metas, err := s.ListKeyMeta("anthropic")
	if err != nil {
		t.Fatalf("ListKeyMeta: %v", err)
	}
	if len(metas) != 0 {
		t.Fatalf("a rejected write still stored something: %+v", metas)
	}
}

func TestSaveKeyMetaStampsFetchedAtWhenTheCallerLeavesItZero(t *testing.T) {
	s := newTestStore(t)
	before := time.Now().Unix()
	saveKey(t, s, KeyMeta{Provider: "anthropic", APIKeyID: "key_a", APIKeyName: "a"})

	metas, err := s.ListKeyMeta("anthropic")
	if err != nil {
		t.Fatalf("ListKeyMeta: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("want 1 key, got %d", len(metas))
	}
	if metas[0].FetchedAt < before || metas[0].FetchedAt > time.Now().Unix()+1 {
		t.Fatalf("fetched_at %d is not the current time (>= %d)", metas[0].FetchedAt, before)
	}
}

func TestSaveKeyMetaKeepsTheFetchedAtItWasGiven(t *testing.T) {
	s := newTestStore(t)
	saveKey(t, s, KeyMeta{Provider: "anthropic", APIKeyID: "key_a", APIKeyName: "a", FetchedAt: 1234})

	metas, _ := s.ListKeyMeta("anthropic")
	if len(metas) != 1 || metas[0].FetchedAt != 1234 {
		t.Fatalf("want the supplied fetched_at 1234, got %+v", metas)
	}
}

func TestSaveKeyMetaUpsertsOneRowPerKeyRatherThanAppending(t *testing.T) {
	s := newTestStore(t)
	saveKey(t, s, KeyMeta{
		Provider: "anthropic", APIKeyID: "key_a", APIKeyName: "old name",
		APIKeyHint: "sk-...aaa", APIKeyStatus: "active", RawJSON: `{"v":1}`, FetchedAt: 100,
	})
	saveKey(t, s, KeyMeta{
		Provider: "anthropic", APIKeyID: "key_a", APIKeyName: "new name",
		APIKeyHint: "sk-...bbb", APIKeyStatus: "archived", RawJSON: `{"v":2}`, FetchedAt: 200,
	})

	metas, err := s.ListKeyMeta("anthropic")
	if err != nil {
		t.Fatalf("ListKeyMeta: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("a refresh of the same key made %d rows, want 1", len(metas))
	}
	got := metas[0]
	if got.APIKeyName != "new name" || got.APIKeyHint != "sk-...bbb" ||
		got.APIKeyStatus != "archived" || got.FetchedAt != 200 {
		t.Fatalf("the refresh did not overwrite every field: %+v", got)
	}
	raw, err := s.LatestKeyRaw("anthropic", "key_a")
	if err != nil {
		t.Fatalf("LatestKeyRaw: %v", err)
	}
	if raw != `{"v":2}` {
		t.Fatalf("raw_json still holds the superseded snapshot: %q", raw)
	}
}

func TestListKeyMetaPutsActiveKeysAboveArchivedOnesAndSortsByName(t *testing.T) {
	s := newTestStore(t)
	saveKey(t, s, KeyMeta{Provider: "anthropic", APIKeyID: "k1", APIKeyName: "zulu", APIKeyStatus: "active", FetchedAt: 1})
	saveKey(t, s, KeyMeta{Provider: "anthropic", APIKeyID: "k2", APIKeyName: "alpha", APIKeyStatus: "archived", FetchedAt: 2})
	saveKey(t, s, KeyMeta{Provider: "anthropic", APIKeyID: "k3", APIKeyName: "bravo", APIKeyStatus: "active", FetchedAt: 3})

	metas, err := s.ListKeyMeta("anthropic")
	if err != nil {
		t.Fatalf("ListKeyMeta: %v", err)
	}
	var order []string
	for _, m := range metas {
		order = append(order, m.APIKeyName)
	}
	// Active before archived is the first sort term; name is the tie-break, so
	// "zulu" (active) outranks "alpha" (archived) and loses to "bravo".
	want := []string{"bravo", "zulu", "alpha"}
	if len(order) != len(want) {
		t.Fatalf("want %d keys, got %v", len(want), order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order is %v, want %v", order, want)
		}
	}
}

func TestListKeyMetaKeepsProvidersApart(t *testing.T) {
	s := newTestStore(t)
	saveKey(t, s, KeyMeta{Provider: "anthropic", APIKeyID: "k1", APIKeyName: "a", FetchedAt: 1})
	saveKey(t, s, KeyMeta{Provider: "openai", APIKeyID: "k2", APIKeyName: "b", FetchedAt: 2})

	metas, _ := s.ListKeyMeta("anthropic")
	if len(metas) != 1 || metas[0].APIKeyID != "k1" {
		t.Fatalf("anthropic's list carries another provider's key: %+v", metas)
	}
	if metas[0].Provider != "anthropic" {
		t.Fatalf("the row does not report the provider it was stored under: %+v", metas[0])
	}
}

func TestListKeyMetaOfAnUnknownProviderIsAnEmptySliceNotNil(t *testing.T) {
	// The HTTP layer ranges over this and json-encodes the result; a nil slice
	// renders as `null` and the UI's `.map` on it throws.
	s := newTestStore(t)
	metas, err := s.ListKeyMeta("nobody")
	if err != nil {
		t.Fatalf("ListKeyMeta: %v", err)
	}
	if metas == nil {
		t.Fatal("want an empty slice, got nil")
	}
	if len(metas) != 0 {
		t.Fatalf("want no keys, got %+v", metas)
	}
}

func TestListKeyMetaLeavesTheRawSnapshotBehind(t *testing.T) {
	// raw_json is a whole API response per key. It is served by its own route
	// on demand, and listing it for every key would put all of them in one
	// response body.
	s := newTestStore(t)
	saveKey(t, s, KeyMeta{Provider: "anthropic", APIKeyID: "k1", APIKeyName: "a", RawJSON: `{"big":"blob"}`, FetchedAt: 1})

	metas, _ := s.ListKeyMeta("anthropic")
	if len(metas) != 1 {
		t.Fatalf("want 1 key, got %d", len(metas))
	}
	if metas[0].RawJSON != "" {
		t.Fatalf("the list carries the raw snapshot: %q", metas[0].RawJSON)
	}
}

func TestLatestKeyRawIsEmptyAndUnexceptionalForAKeyNeverSeen(t *testing.T) {
	s := newTestStore(t)
	raw, err := s.LatestKeyRaw("anthropic", "missing")
	if err != nil {
		t.Fatalf("an unknown key is not an error condition: %v", err)
	}
	if raw != "" {
		t.Fatalf("want empty, got %q", raw)
	}
}

func TestLatestKeyRawTreatsAStoredNullAsEmpty(t *testing.T) {
	// raw_json is nullable, so a key saved before snapshots were kept scans
	// into a NULL. That must read as "no snapshot", not fail the request.
	s := newTestStore(t)
	_, err := s.db.Exec(`INSERT INTO spend_keys (provider, api_key_id, fetched_at) VALUES ('anthropic', 'k1', 1)`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	raw, err := s.LatestKeyRaw("anthropic", "k1")
	if err != nil {
		t.Fatalf("LatestKeyRaw: %v", err)
	}
	if raw != "" {
		t.Fatalf("want empty for a NULL snapshot, got %q", raw)
	}
}

func TestLatestKeyRawKeepsProvidersApart(t *testing.T) {
	s := newTestStore(t)
	saveKey(t, s, KeyMeta{Provider: "anthropic", APIKeyID: "shared", RawJSON: `{"who":"anthropic"}`, FetchedAt: 1})
	saveKey(t, s, KeyMeta{Provider: "openai", APIKeyID: "shared", RawJSON: `{"who":"openai"}`, FetchedAt: 1})

	raw, _ := s.LatestKeyRaw("openai", "shared")
	if raw != `{"who":"openai"}` {
		t.Fatalf("asked openai and got %q", raw)
	}
}

// ---- SaveDailySpend ----

func TestSaveDailySpendReplacesTheDayTotalRatherThanAccumulating(t *testing.T) {
	// Every refresh re-reads the same days from the provider. If the write
	// added instead of replacing, one day's cost would grow on every poll.
	s := newTestStore(t)
	when := daysAgo(1)
	saveDaily(t, s, "anthropic", "k1", when, 3)
	saveDaily(t, s, "anthropic", "k1", when, 5)

	total, err := s.SumSpendSince("anthropic", "k1", daysAgo(2).Unix())
	if err != nil {
		t.Fatalf("SumSpendSince: %v", err)
	}
	if total != 5 {
		t.Fatalf("want the latest figure 5, got %v", total)
	}
}

func TestSaveDailySpendKeepsSeparateDaysSeparate(t *testing.T) {
	s := newTestStore(t)
	saveDaily(t, s, "anthropic", "k1", daysAgo(1), 3)
	saveDaily(t, s, "anthropic", "k1", daysAgo(2), 5)

	total, _ := s.SumSpendSince("anthropic", "k1", daysAgo(3).Unix())
	if total != 8 {
		t.Fatalf("want 3+5=8 across two days, got %v", total)
	}
}

func TestSaveDailySpendStampsFetchedAtWhenTheCallerLeavesItZero(t *testing.T) {
	s := newTestStore(t)
	before := time.Now().Unix()
	saveDaily(t, s, "anthropic", "k1", daysAgo(1), 3)

	var fetchedAt int64
	err := s.db.QueryRow(`SELECT fetched_at FROM spend_daily WHERE api_key_id = 'k1'`).Scan(&fetchedAt)
	if err != nil {
		t.Fatalf("read fetched_at: %v", err)
	}
	if fetchedAt < before || fetchedAt > time.Now().Unix()+1 {
		t.Fatalf("fetched_at %d is not the current time (>= %d)", fetchedAt, before)
	}
}

func TestSaveDailySpendKeepsTheFetchedAtItWasGiven(t *testing.T) {
	s := newTestStore(t)
	err := s.SaveDailySpend(DailySpend{
		Provider: "anthropic", APIKeyID: "k1", Date: day(daysAgo(1)), TotalUSD: 3, FetchedAt: 4321,
	})
	if err != nil {
		t.Fatalf("SaveDailySpend: %v", err)
	}
	var fetchedAt int64
	s.db.QueryRow(`SELECT fetched_at FROM spend_daily WHERE api_key_id = 'k1'`).Scan(&fetchedAt)
	if fetchedAt != 4321 {
		t.Fatalf("want the supplied fetched_at 4321, got %d", fetchedAt)
	}
}

// ---- SumSpendSince / PerKeyTotals: the window ----

func TestSumSpendSinceIncludesTheDayTheWindowOpensOn(t *testing.T) {
	// The cutoff is converted to a UTC date and compared with >=, so spend
	// recorded ON the boundary day is inside the window. A > here would drop a
	// whole day of cost from every figure on the page.
	s := newTestStore(t)
	boundary := daysAgo(7)
	saveDaily(t, s, "anthropic", "k1", boundary, 10)

	total, err := s.SumSpendSince("anthropic", "", boundary.Unix())
	if err != nil {
		t.Fatalf("SumSpendSince: %v", err)
	}
	if total != 10 {
		t.Fatalf("the boundary day was excluded: got %v, want 10", total)
	}
}

func TestSumSpendSinceExcludesTheDayBeforeTheWindow(t *testing.T) {
	s := newTestStore(t)
	saveDaily(t, s, "anthropic", "k1", daysAgo(8), 10)
	saveDaily(t, s, "anthropic", "k1", daysAgo(6), 4)

	total, _ := s.SumSpendSince("anthropic", "", daysAgo(7).Unix())
	if total != 4 {
		t.Fatalf("want only the 4 inside the window, got %v", total)
	}
}

func TestSumSpendSinceWithNoKeyCoversEveryKeyOfThatProvider(t *testing.T) {
	s := newTestStore(t)
	saveDaily(t, s, "anthropic", "k1", daysAgo(1), 3)
	saveDaily(t, s, "anthropic", "k2", daysAgo(1), 4)

	total, _ := s.SumSpendSince("anthropic", "", daysAgo(2).Unix())
	if total != 7 {
		t.Fatalf("want 3+4=7 across both keys, got %v", total)
	}
}

func TestSumSpendSinceWithAKeyCoversOnlyThatKey(t *testing.T) {
	s := newTestStore(t)
	saveDaily(t, s, "anthropic", "k1", daysAgo(1), 3)
	saveDaily(t, s, "anthropic", "k2", daysAgo(1), 4)

	total, _ := s.SumSpendSince("anthropic", "k2", daysAgo(2).Unix())
	if total != 4 {
		t.Fatalf("want only k2's 4, got %v", total)
	}
}

func TestSumSpendSinceKeepsProvidersApart(t *testing.T) {
	s := newTestStore(t)
	saveDaily(t, s, "anthropic", "k1", daysAgo(1), 3)
	saveDaily(t, s, "openai", "k1", daysAgo(1), 100)

	total, _ := s.SumSpendSince("anthropic", "", daysAgo(2).Unix())
	if total != 3 {
		t.Fatalf("anthropic's total absorbed openai's spend: got %v, want 3", total)
	}
}

func TestSumSpendSinceIsZeroWhenNothingWasSpent(t *testing.T) {
	// SUM over no rows is NULL in SQLite. Without the COALESCE the scan fails
	// and a provider with no spend yet reports an error instead of $0.
	s := newTestStore(t)
	total, err := s.SumSpendSince("anthropic", "", daysAgo(30).Unix())
	if err != nil {
		t.Fatalf("no spend is not an error condition: %v", err)
	}
	if total != 0 {
		t.Fatalf("want 0, got %v", total)
	}
}

func TestPerKeyTotalsReportsOneEntryPerKey(t *testing.T) {
	s := newTestStore(t)
	saveDaily(t, s, "anthropic", "k1", daysAgo(1), 3)
	saveDaily(t, s, "anthropic", "k1", daysAgo(2), 2)
	saveDaily(t, s, "anthropic", "k2", daysAgo(1), 7)

	totals, err := s.PerKeyTotals("anthropic", daysAgo(3).Unix())
	if err != nil {
		t.Fatalf("PerKeyTotals: %v", err)
	}
	if len(totals) != 2 {
		t.Fatalf("want 2 keys, got %+v", totals)
	}
	if totals["k1"] != 5 {
		t.Fatalf("k1 should sum both its days to 5, got %v", totals["k1"])
	}
	if totals["k2"] != 7 {
		t.Fatalf("k2 should be 7, got %v", totals["k2"])
	}
}

func TestPerKeyTotalsAppliesTheSameWindowEdgeAsTheProviderSum(t *testing.T) {
	// The account header and the key rows beneath it are computed by different
	// queries. If their cutoffs disagreed, the rows would not add up to the
	// header and neither figure would be wrong on its own.
	s := newTestStore(t)
	boundary := daysAgo(7)
	saveDaily(t, s, "anthropic", "k1", boundary, 10)
	saveDaily(t, s, "anthropic", "k1", daysAgo(8), 99)

	totals, _ := s.PerKeyTotals("anthropic", boundary.Unix())
	sum, _ := s.SumSpendSince("anthropic", "", boundary.Unix())
	if totals["k1"] != 10 {
		t.Fatalf("per-key window disagrees: got %v, want 10", totals["k1"])
	}
	if totals["k1"] != sum {
		t.Fatalf("per-key %v and provider sum %v disagree on the same window", totals["k1"], sum)
	}
}

func TestPerKeyTotalsKeepsProvidersApart(t *testing.T) {
	s := newTestStore(t)
	saveDaily(t, s, "anthropic", "shared", daysAgo(1), 3)
	saveDaily(t, s, "openai", "shared", daysAgo(1), 100)

	totals, _ := s.PerKeyTotals("openai", daysAgo(2).Unix())
	if totals["shared"] != 100 {
		t.Fatalf("want openai's 100 for the shared key id, got %v", totals["shared"])
	}
}

func TestPerKeyTotalsOfAProviderWithNoSpendIsAnEmptyMapNotNil(t *testing.T) {
	// assembleProvider indexes this map for every key it lists. A nil map reads
	// fine in Go, so this is about the caller never having to check.
	s := newTestStore(t)
	totals, err := s.PerKeyTotals("anthropic", daysAgo(30).Unix())
	if err != nil {
		t.Fatalf("PerKeyTotals: %v", err)
	}
	if totals == nil {
		t.Fatal("want an empty map, got nil")
	}
	if len(totals) != 0 {
		t.Fatalf("want no entries, got %+v", totals)
	}
}

// ---- top-ups ----

func TestAddTopupRefusesEveryIncompleteDeposit(t *testing.T) {
	s := newTestStore(t)
	cases := []struct {
		name  string
		topup Topup
	}{
		{"no provider", Topup{AmountUSD: 10, OccurredAt: 100}},
		{"zero amount", Topup{Provider: "anthropic", OccurredAt: 100}},
		{"negative amount", Topup{Provider: "anthropic", AmountUSD: -10, OccurredAt: 100}},
		{"no occurred_at", Topup{Provider: "anthropic", AmountUSD: 10}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.AddTopup(c.topup); err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
		})
	}
	topups, _ := s.ListTopups("anthropic")
	if len(topups) != 0 {
		t.Fatalf("a rejected deposit still reached the table: %+v", topups)
	}
}

func TestAddTopupStampsCreatedAtWhileKeepingTheSuppliedOccurredAt(t *testing.T) {
	// The two timestamps mean different things: occurred_at is when the credit
	// was bought and anchors the balance window; created_at is when the row was
	// typed in. Defaulting the wrong one moves the baseline.
	s := newTestStore(t)
	before := time.Now().Unix()
	if _, err := s.AddTopup(Topup{Provider: "anthropic", AmountUSD: 25, OccurredAt: 1000}); err != nil {
		t.Fatalf("AddTopup: %v", err)
	}

	topups, _ := s.ListTopups("anthropic")
	if len(topups) != 1 {
		t.Fatalf("want 1 top-up, got %d", len(topups))
	}
	if topups[0].OccurredAt != 1000 {
		t.Fatalf("occurred_at was overwritten: %d", topups[0].OccurredAt)
	}
	if topups[0].CreatedAt < before || topups[0].CreatedAt > time.Now().Unix()+1 {
		t.Fatalf("created_at %d is not the current time (>= %d)", topups[0].CreatedAt, before)
	}
}

func TestAddTopupKeepsTheCreatedAtItWasGiven(t *testing.T) {
	s := newTestStore(t)
	_, err := s.AddTopup(Topup{Provider: "anthropic", AmountUSD: 25, OccurredAt: 1000, CreatedAt: 555})
	if err != nil {
		t.Fatalf("AddTopup: %v", err)
	}
	topups, _ := s.ListTopups("anthropic")
	if len(topups) != 1 || topups[0].CreatedAt != 555 {
		t.Fatalf("want the supplied created_at 555, got %+v", topups)
	}
}

func TestAddTopupReturnsTheIdOfTheRowItWrote(t *testing.T) {
	s := newTestStore(t)
	first, err := s.AddTopup(Topup{Provider: "anthropic", AmountUSD: 10, OccurredAt: 1000, Note: "first"})
	if err != nil {
		t.Fatalf("AddTopup: %v", err)
	}
	second, err := s.AddTopup(Topup{Provider: "anthropic", AmountUSD: 20, OccurredAt: 2000, Note: "second"})
	if err != nil {
		t.Fatalf("AddTopup: %v", err)
	}
	if first == second {
		t.Fatalf("two deposits share id %d — DeleteTopup could not tell them apart", first)
	}

	topups, _ := s.ListTopups("anthropic")
	byID := map[int64]Topup{}
	for _, tp := range topups {
		byID[tp.ID] = tp
	}
	if byID[first].Note != "first" || byID[second].Note != "second" {
		t.Fatalf("the returned ids do not name the rows written: %+v", topups)
	}
}

func TestAddTopupKeepsTheNote(t *testing.T) {
	s := newTestStore(t)
	s.AddTopup(Topup{Provider: "anthropic", AmountUSD: 10, OccurredAt: 1000, Note: "invoice 42"})
	topups, _ := s.ListTopups("anthropic")
	if len(topups) != 1 || topups[0].Note != "invoice 42" {
		t.Fatalf("the note did not survive the write: %+v", topups)
	}
}

func TestListTopupsPutsTheEarliestDepositFirst(t *testing.T) {
	// assembleProvider reads element zero as the balance baseline, so this
	// ordering is not cosmetic: reverse it and "remaining" is computed from the
	// most recent top-up and every earlier dollar disappears.
	s := newTestStore(t)
	s.AddTopup(Topup{Provider: "anthropic", AmountUSD: 10, OccurredAt: 3000, Note: "third"})
	s.AddTopup(Topup{Provider: "anthropic", AmountUSD: 20, OccurredAt: 1000, Note: "first"})
	s.AddTopup(Topup{Provider: "anthropic", AmountUSD: 30, OccurredAt: 2000, Note: "second"})

	topups, err := s.ListTopups("anthropic")
	if err != nil {
		t.Fatalf("ListTopups: %v", err)
	}
	var order []string
	for _, tp := range topups {
		order = append(order, tp.Note)
	}
	want := []string{"first", "second", "third"}
	if len(order) != len(want) {
		t.Fatalf("want %d top-ups, got %v", len(want), order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order is %v, want %v", order, want)
		}
	}
}

func TestListTopupsBreaksATieOnTheSameSecondByInsertionOrder(t *testing.T) {
	// Two deposits recorded for the same day carry the same occurred_at, so the
	// id decides. Without the second sort term the pair would come back in
	// whatever order SQLite chose and the baseline row could change between
	// requests.
	s := newTestStore(t)
	s.AddTopup(Topup{Provider: "anthropic", AmountUSD: 10, OccurredAt: 1000, Note: "earlier row"})
	s.AddTopup(Topup{Provider: "anthropic", AmountUSD: 20, OccurredAt: 1000, Note: "later row"})

	topups, _ := s.ListTopups("anthropic")
	if len(topups) != 2 {
		t.Fatalf("want 2 top-ups, got %d", len(topups))
	}
	if topups[0].Note != "earlier row" || topups[1].Note != "later row" {
		t.Fatalf("tie broken the wrong way: %+v", topups)
	}
}

func TestListTopupsKeepsProvidersApart(t *testing.T) {
	s := newTestStore(t)
	s.AddTopup(Topup{Provider: "anthropic", AmountUSD: 10, OccurredAt: 1000})
	s.AddTopup(Topup{Provider: "openai", AmountUSD: 999, OccurredAt: 1000})

	topups, _ := s.ListTopups("anthropic")
	if len(topups) != 1 || topups[0].AmountUSD != 10 {
		t.Fatalf("anthropic's credit absorbed another provider's: %+v", topups)
	}
	if topups[0].Provider != "anthropic" {
		t.Fatalf("the row does not report the provider it was stored under: %+v", topups[0])
	}
}

func TestListTopupsOfAProviderWithNoCreditIsAnEmptySliceNotNil(t *testing.T) {
	s := newTestStore(t)
	topups, err := s.ListTopups("anthropic")
	if err != nil {
		t.Fatalf("ListTopups: %v", err)
	}
	if topups == nil {
		t.Fatal("want an empty slice, got nil")
	}
	if len(topups) != 0 {
		t.Fatalf("want no top-ups, got %+v", topups)
	}
}

func TestDeleteTopupRemovesOnlyTheRowNamed(t *testing.T) {
	s := newTestStore(t)
	doomed, _ := s.AddTopup(Topup{Provider: "anthropic", AmountUSD: 10, OccurredAt: 1000, Note: "doomed"})
	s.AddTopup(Topup{Provider: "anthropic", AmountUSD: 20, OccurredAt: 2000, Note: "keeper"})

	if err := s.DeleteTopup(doomed); err != nil {
		t.Fatalf("DeleteTopup: %v", err)
	}

	topups, _ := s.ListTopups("anthropic")
	if len(topups) != 1 || topups[0].Note != "keeper" {
		t.Fatalf("want only the keeper left, got %+v", topups)
	}
}

func TestDeleteTopupOfAnUnknownIdIsNotAnError(t *testing.T) {
	// The handler turns any error here into a 500. A double-click on Delete
	// must not read as a server fault.
	s := newTestStore(t)
	if err := s.DeleteTopup(99999); err != nil {
		t.Fatalf("deleting a row that is not there: %v", err)
	}
}
