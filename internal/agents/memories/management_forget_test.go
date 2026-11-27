package memories

import (
	"testing"
)

// Forget is the destructive half of the prefix contract Get settled: the id it
// is handed may be an abbreviation, so it has to resolve a row, and it has to
// resolve exactly one. These tests pin that a prefix cannot reach a memory the
// caller did not name, and that the empty string names nothing at all.

func newForgetTestStore(t *testing.T) *Store {
	t.Helper()
	store := openTestStore(t)
	t.Cleanup(func() { store.Close() })
	return store
}

func storedImportance(t *testing.T, s *Store, id string) float64 {
	t.Helper()
	var importance float64
	if err := s.db.QueryRow("SELECT importance FROM memories WHERE id = $1", id).Scan(&importance); err != nil {
		t.Fatalf("read importance for %q: %v", id, err)
	}
	return importance
}

func saveForgetFixture(t *testing.T, s *Store, id, content string) {
	t.Helper()
	if err := s.Save(Memory{ID: id, Content: content, Importance: 0.6}); err != nil {
		t.Fatalf("Save %q: %v", id, err)
	}
}

// A memory whose id merely starts with the one the caller named is a different
// memory. inber writes "conversation-summary:<session-id>:<suffix>", so
// forgetting the base id used to take every part written under it.
func TestForgetLeavesRowsTheIDOnlyPrefixes(t *testing.T) {
	store := newForgetTestStore(t)

	const named = "conversation-summary:br_1785582021693620208"
	const sibling = named + ":part2"
	saveForgetFixture(t, store, named, "the summary the caller asked to forget")
	saveForgetFixture(t, store, sibling, "a later part of the same conversation")

	if err := store.Forget(named); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	if got := storedImportance(t, store, named); got != 0 {
		t.Errorf("named memory importance = %v, want 0 (it should be forgotten)", got)
	}
	if got := storedImportance(t, store, sibling); got == 0 {
		t.Error("a memory the id only prefixes was forgotten too")
	}
}

// The abbreviation is the reason a prefix is accepted at all, so it still has
// to work: a pointer naming the first eight characters must forget that row.
func TestForgetResolvesAnAbbreviatedID(t *testing.T) {
	store := newForgetTestStore(t)

	const id = "11112222-3333-4444-5555-666677778888"
	saveForgetFixture(t, store, id, "a large block lifted out of a message")

	if err := store.Forget(id[:8]); err != nil {
		t.Fatalf("Forget by prefix: %v", err)
	}
	if got := storedImportance(t, store, id); got != 0 {
		t.Errorf("importance = %v, want 0 — the abbreviated id did not resolve", got)
	}
}

// The empty string is a prefix of every id. Reached through memory_forget with
// a missing argument it used to soft-delete the entire store and report success.
func TestForgetRefusesAnEmptyID(t *testing.T) {
	store := newForgetTestStore(t)

	saveForgetFixture(t, store, "keep-me", "a memory nobody asked to forget")
	saveForgetFixture(t, store, "keep-me-too", "another one")

	if err := store.Forget(""); err == nil {
		t.Error("Forget(\"\") returned no error — an empty id names no memory")
	}

	for _, id := range []string{"keep-me", "keep-me-too"} {
		if got := storedImportance(t, store, id); got == 0 {
			t.Errorf("%s was forgotten by an empty id", id)
		}
	}
}

// LIKE reads `_` as a wildcard and bridge session ids carry one, so an
// unescaped pattern lets one id reach a row that differs from it.
//
// The abbreviated id is the case that exercises this. Handed the id in full,
// the exact-match ordering picks the right row whether or not the pattern was
// escaped, and the sabotage that removes the escaping comes back green.
func TestForgetEscapesWildcardsInTheID(t *testing.T) {
	store := newForgetTestStore(t)

	const named = "summary:br_1785"
	const other = "summary:brX1785"
	// Written first, so an unescaped pattern reaches it first.
	saveForgetFixture(t, store, other, "a different session entirely")
	saveForgetFixture(t, store, named, "the one the caller named")

	// A prefix, the way a recall pointer names a memory — no exact match to
	// fall back on.
	if err := store.Forget("summary:br_17"); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	if got := storedImportance(t, store, named); got != 0 {
		t.Errorf("named memory importance = %v, want 0", got)
	}
	if got := storedImportance(t, store, other); got == 0 {
		t.Error("the underscore matched another id — the LIKE pattern is unescaped")
	}
}

// An exact id beats a row that merely starts with it, so the caller who names a
// memory in full forgets that memory and not a longer one.
//
// The derived row is written first on purpose, so insertion order cannot make
// this pass by accident. It still passes with the ORDER BY removed: SQLite
// scans the id primary-key index, so a prefix arrives before its extensions
// whatever order they were written in. What this pins is the behavior, not
// that one clause — if a later plan change stops delivering it, this fails.
func TestForgetPrefersAnExactID(t *testing.T) {
	store := newForgetTestStore(t)

	saveForgetFixture(t, store, "abc123-derived", "something derived from it")
	saveForgetFixture(t, store, "abc123", "the memory named in full")

	if err := store.Forget("abc123"); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	if got := storedImportance(t, store, "abc123"); got != 0 {
		t.Errorf("exact id importance = %v, want 0", got)
	}
	if got := storedImportance(t, store, "abc123-derived"); got == 0 {
		t.Error("the derived memory was forgotten instead of the one named in full")
	}
}

// Forgetting something that is not there is still an error, which is what the
// HTTP layer turns into a 404 and the tool into "error: memory not found".
func TestForgetReportsAMissingMemory(t *testing.T) {
	store := newForgetTestStore(t)
	saveForgetFixture(t, store, "present", "a memory")

	if err := store.Forget("absent"); err == nil {
		t.Error("Forget of an unknown id returned no error")
	}
}
