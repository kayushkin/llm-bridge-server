package memorystore

import (
	"testing"

)

// Get accepts a prefix of a memory's id because that is what the recall
// pointers written into a conversation carry — inber's stasher names the first
// eight characters of a UUID. These tests pin that the rest of Get honors the
// row it resolved rather than the prefix it was handed, and that a prefix can
// never reach a row the caller did not name.

func newGetTestStore(t *testing.T) *Store {
	t.Helper()
	store := openTestStore(t)
	t.Cleanup(func() { store.Close() })
	return store
}

func storedAccessCount(t *testing.T, s *Store, id string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow("SELECT access_count FROM memories WHERE id = $1", id).Scan(&count); err != nil {
		t.Fatalf("read access_count for %q: %v", id, err)
	}
	return count
}

func TestGetByPrefixReturnsTheRowsTags(t *testing.T) {
	store := newGetTestStore(t)

	const id = "11112222-3333-4444-5555-666677778888"
	if err := store.Save(Memory{
		ID:      id,
		Content: "a large block lifted out of a message",
		Tags:    []string{"stashed", "large-input", "code"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The pointer conversation.StashLargeContent leaves in the transcript.
	got, err := store.Get(id[:8])
	if err != nil {
		t.Fatalf("Get by prefix: %v", err)
	}
	if got.ID != id {
		t.Fatalf("Get by prefix resolved %q, want %q", got.ID, id)
	}
	if len(got.Tags) != 3 {
		t.Errorf("Get by prefix returned %d tags %v, want the 3 the memory was saved with — "+
			"a tag query keyed on the prefix matches no row and the memory reads as untagged", len(got.Tags), got.Tags)
	}
}

func TestGetByPrefixRecordsTheAccessItReports(t *testing.T) {
	store := newGetTestStore(t)

	const id = "aaaabbbb-cccc-dddd-eeee-ffff00001111"
	if err := store.Save(Memory{ID: id, Content: "recalled by pointer"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Get(id[:8])
	if err != nil {
		t.Fatalf("Get by prefix: %v", err)
	}
	if got.AccessCount != 1 {
		t.Fatalf("returned AccessCount = %d, want 1", got.AccessCount)
	}
	if stored := storedAccessCount(t, store, id); stored != 1 {
		t.Errorf("stored access_count = %d after a prefix Get, want 1 — "+
			"the returned memory claimed an access the store never recorded", stored)
	}
}

func TestGetPrefersTheExactIDOverALongerOneSharingIt(t *testing.T) {
	store := newGetTestStore(t)

	// A compaction archive and any id built by extending it. Both satisfy the
	// prefix branch; only one is the memory the caller asked for.
	const exact = "conversation-summary:sess-1:abcd1234"
	const longer = exact + ":follow-up"
	if err := store.Save(Memory{ID: longer, Content: "the wrong one"}); err != nil {
		t.Fatalf("Save longer: %v", err)
	}
	if err := store.Save(Memory{ID: exact, Content: "the one asked for"}); err != nil {
		t.Fatalf("Save exact: %v", err)
	}

	got, err := store.Get(exact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != exact {
		t.Errorf("Get(%q) resolved %q — an exact id must beat a row that merely starts with it", exact, got.ID)
	}
}

func TestGetDoesNotReadTheCallersIDAsALikePattern(t *testing.T) {
	store := newGetTestStore(t)

	// inber ids are "conversation-summary:<session-id>:<suffix>", and a session
	// id can carry an underscore. Unescaped, `_` matches any single character,
	// so asking for one memory answers with another.
	const asked = "conversation-summary:sess_1:abcd1234"
	const other = "conversation-summary:sessX1:abcd1234"
	if err := store.Save(Memory{ID: other, Content: "a different session's transcript"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if got, err := store.Get(asked); err == nil {
		t.Errorf("Get(%q) answered with %q; the store holds no memory with that id", asked, got.ID)
	}

	// The same shape with `%`, which would otherwise match everything.
	if got, err := store.Get("%"); err == nil {
		t.Errorf("Get(%q) answered with %q; `%%` is an id, not a wildcard", "%", got.ID)
	}
}

func TestLikePrefixPatternEscapesMetacharacters(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"abc", `abc%`},
		{"a_b", `a\_b%`},
		{"a%b", `a\%b%`},
		{`a\b`, `a\\b%`},
		{"", "%"},
	} {
		if got := likePrefixPattern(c.in); got != c.want {
			t.Errorf("likePrefixPattern(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
