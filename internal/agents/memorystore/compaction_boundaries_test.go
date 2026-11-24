package memorystore

import (
	"fmt"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Store.Compact selects what to destroy with four numeric cuts written into one
// WHERE clause:
//
//	WHERE created_at < ? AND access_count < ? AND importance < 0.7 AND importance > 0
//
// Compaction merges content and then soft-deletes the originals, so each cut is
// the line between a memory that survives and a memory that does not. All four
// were exercised on every row of the existing suite and pinned by none of it.
//
// Measured before these tests, with scripts/sabotage-compaction.py (18 rows,
// whole package per row, both controls behaving): 4 CAUGHT / 18. The age cut
// was caught in one direction only — the old suite's rows sit 10 days behind a
// 7-day cutoff, so widening the window kept them eligible and nothing failed.
// The access cut, the importance ceiling and the soft-delete floor could each be
// REMOVED ENTIRELY and the package stayed green.
//
// Reaching an axis is not straddling it. Every test below therefore saves two
// pairs that differ on one cut and agree on everything else: one pair just
// inside, one pair just outside. A cut that loosens admits the outside pair and
// yields a second compaction group; a cut that tightens drops the inside pair
// and yields none. Both directions fail an assertion.
//
// Pairs, not singletons, because Compact skips a group of fewer than two
// members — a lone row on the wrong side of a cut would be skipped for the size
// and the cut would go on being untested.
//
// ⚠️ Do not read a fixture back with Get before calling Compact. Get bumps
// access_count and multiplies importance by 1.01 (crud.go), which moves the very
// values these tests place next to a boundary. Assert on what Compact returns.

// saveCompactionCandidate writes one memory positioned on the cuts Compact
// selects with. Every argument is a coordinate on one of those axes.
func saveCompactionCandidate(t *testing.T, store *Store, id, tag string, age time.Duration, importance float64, accessCount int) {
	t.Helper()
	if err := store.Save(Memory{
		ID:          id,
		Content:     "candidate " + id,
		Tags:        []string{tag},
		Importance:  importance,
		AccessCount: accessCount,
		Source:      "agent",
		CreatedAt:   time.Now().Add(-age),
	}); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
}

// saveCompactionPair writes two memories that share a tag, so they form one
// group and clear Compact's len(members) < 2 skip.
func saveCompactionPair(t *testing.T, store *Store, tag string, age time.Duration, importance float64, accessCount int) []string {
	t.Helper()
	ids := []string{tag + "-a", tag + "-b"}
	for _, id := range ids {
		saveCompactionCandidate(t, store, id, tag, age, importance, accessCount)
	}
	return ids
}

// assertCompactedExactly checks that Compact merged one group and that the group
// holds precisely the memories named. Asserting the count alone would pass for a
// group that swallowed the wrong side of a cut.
func assertCompactedExactly(t *testing.T, results []CompactionResult, wantIDs []string) {
	t.Helper()
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 compaction group, got %d: %s", len(results), describeResults(results))
	}
	got := append([]string(nil), results[0].OriginalIDs...)
	sort.Strings(got)
	want := append([]string(nil), wantIDs...)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("compacted the wrong memories: got %v, want %v", got, want)
	}
}

func describeResults(results []CompactionResult) string {
	if len(results) == 0 {
		return "nothing was compacted"
	}
	var out []string
	for _, r := range results {
		out = append(out, fmt.Sprint(r.OriginalIDs))
	}
	return fmt.Sprint(out)
}

// Cut: created_at < now-minAge.
//
// The two pairs sit one hour either side of a 24-hour cutoff, so any move of the
// window in either direction crosses one of them. Sabotage caught by this test:
// halving the window, doubling it, and removing the age cut entirely.
//
// ⚠️ `<` to `<=` is NOT caught, and cannot be: it differs only for a memory whose
// created_at equals the cutoff to the second, and the cutoff is computed from
// time.Now() inside the call. That is an equivalent mutation for any fixture,
// not a gap — the same shape the 247th pass recorded for `st.Size() > window`.
func TestCompactLeavesAMemoryYoungerThanMinAgeAlone(t *testing.T) {
	store := newTestStore(t)

	const minAge = 24 * time.Hour
	past := saveCompactionPair(t, store, "aged-past-the-cutoff", minAge+time.Hour, 0.3, 1)
	saveCompactionPair(t, store, "aged-inside-the-cutoff", minAge-time.Hour, 0.3, 1)

	results, err := store.Compact(minAge, 3)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	assertCompactedExactly(t, results, past)
}

// Cut: access_count < minCount.
//
// minCount is a strict upper bound, so a memory read exactly minCount times is
// still in use and must survive. The pairs sit at minCount-1 and minCount.
// Sabotage caught: `<` to `<=`, minCount+-1, and removing the access cut.
func TestCompactLeavesAMemoryReadMinCountTimesAlone(t *testing.T) {
	store := newTestStore(t)

	const minCount = 3
	below := saveCompactionPair(t, store, "accessed-below-the-cut", 10*24*time.Hour, 0.3, minCount-1)
	saveCompactionPair(t, store, "accessed-at-the-cut", 10*24*time.Hour, 0.3, minCount)

	results, err := store.Compact(7*24*time.Hour, minCount)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	assertCompactedExactly(t, results, below)
}

// Cut: importance < 0.7.
//
// 0.7 is hardcoded in the query and named nowhere else, so it is the kind of
// literal that moves in a refactor without anyone deciding to move it. The
// pairs sit at 0.69 and exactly 0.7. Sabotage caught: the ceiling raised,
// lowered, turned into `<=`, and removed.
func TestCompactLeavesAMemoryAtTheImportanceCeilingAlone(t *testing.T) {
	store := newTestStore(t)

	const ceiling = 0.7
	below := saveCompactionPair(t, store, "importance-below-the-ceiling", 10*24*time.Hour, ceiling-0.01, 1)
	saveCompactionPair(t, store, "importance-at-the-ceiling", 10*24*time.Hour, ceiling, 1)

	results, err := store.Compact(7*24*time.Hour, 3)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	assertCompactedExactly(t, results, below)
}

// Cut: importance > 0 — the soft-delete floor, and the sharpest of the four.
//
// Compact marks the originals it destroys by writing importance = 0
// (compaction.go, "Soft-delete originals"). That same 0 is what this cut
// excludes, so the floor is what stops compaction from picking up its own
// leftovers, merging them a second time and stacking "[Compacted from N
// memories]" headers on content whose originals are already gone.
//
// Nothing pinned it: relaxing the floor to `>= 0` left the package green.
//
// The deleted pair is written with a direct UPDATE because Save coerces an
// importance of 0 to 0.5 (crud.go, "Set defaults") — the same statement Compact
// itself uses, so the fixture is the state production actually leaves behind.
// The live pair sits just above the floor at 0.05, so raising the floor is
// caught as well as lowering it.
func TestCompactDoesNotReviveTheMemoriesItAlreadySoftDeleted(t *testing.T) {
	store := newTestStore(t)

	live := saveCompactionPair(t, store, "live-just-above-the-floor", 10*24*time.Hour, 0.05, 1)
	deleted := saveCompactionPair(t, store, "already-soft-deleted", 10*24*time.Hour, 0.3, 1)
	for _, id := range deleted {
		if _, err := store.db.Exec("UPDATE memories SET importance = 0 WHERE id = ?", id); err != nil {
			t.Fatalf("soft-delete %s: %v", id, err)
		}
	}

	results, err := store.Compact(7*24*time.Hour, 3)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	assertCompactedExactly(t, results, live)
}
