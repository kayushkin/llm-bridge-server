package memorystore

import (
	"fmt"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// These three tests pin compactionGroupKeyForMemory. Compaction concatenates
// content and then soft-deletes the originals, so a grouping key that is not an
// identity destroys data rather than merely mis-filing it. Each test below was
// verified by removing the one mechanism it names and watching it — and only it
// — go red; the red sets are recorded next to each test so a later reader can
// tell a real check from one that passes for an unrelated reason.

func saveOldMemory(t *testing.T, store *Store, id, content string, tags []string) {
	t.Helper()
	if err := store.Save(Memory{
		ID:          id,
		Content:     content,
		Tags:        tags,
		Importance:  0.3,
		AccessCount: 1,
		Source:      "agent",
		CreatedAt:   time.Now().Add(-10 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
}

// Mechanism: the len(tags) == 0 branch returning a per-memory key.
// Sabotage (return the literal "untagged" instead) reddens this test only.
func TestCompactDoesNotMergeUntaggedMemoriesWithEachOther(t *testing.T) {
	store := newTestStore(t)

	// Three memories with nothing in common. The tagger gives source "agent"
	// no tags at all, so this is the ordinary case, not an exotic one.
	unrelated := []string{
		"the front door key is under the mat",
		"postgres connection pool size is 20",
		"her birthday is in March",
	}
	for i, content := range unrelated {
		saveOldMemory(t, store, fmt.Sprintf("untagged-%d", i), content, nil)
	}

	// Precondition: these really are untagged. If the tagger ever starts
	// tagging them this test would pass for the wrong reason.
	for i := range unrelated {
		m, err := store.Get(fmt.Sprintf("untagged-%d", i))
		if err != nil {
			t.Fatalf("get untagged-%d: %v", i, err)
		}
		if len(m.Tags) != 0 {
			t.Fatalf("precondition failed: untagged-%d has tags %v, so this test no longer exercises the untagged path", i, m.Tags)
		}
	}

	results, err := store.Compact(7*24*time.Hour, 3)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("untagged memories were merged into %d group(s); first merged %d unrelated memories %v",
			len(results), results[0].Count, results[0].OriginalIDs)
	}
}

// Mechanism: sort.Strings on the tag slice.
// Sabotage (drop the sort) reddens this test only — without it the two
// memories key on "zebra" and "alpha", become groups of one, and nothing
// compacts.
func TestCompactGroupsMemoriesWithTheSameTagsRegardlessOfTagOrder(t *testing.T) {
	store := newTestStore(t)

	// Identical tag SETS, different stored order. Tag() ranges a Go map, so
	// the order really is arbitrary in production — this reproduces it.
	saveOldMemory(t, store, "mem-a", "content a", []string{"zebra", "alpha"})
	saveOldMemory(t, store, "mem-b", "content b", []string{"alpha", "zebra"})

	results, err := store.Compact(7*24*time.Hour, 3)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected the two same-tagged memories to form 1 group, got %d", len(results))
	}
	if results[0].Count != 2 {
		t.Errorf("expected 2 memories in the group, got %d", results[0].Count)
	}
}

// Mechanism: the "tag:" / "memory-id:" key prefixes.
// Sabotage (return the bare tag and the bare memory id) reddens this test only.
// This is not hypothetical: the tagger emits bare filenames as tags, and memory
// ids are caller-supplied strings, so a tag colliding with some other memory's
// id is an ordinary collision rather than a contrived one.
func TestCompactKeepsTagKeysAndMemoryIDKeysInSeparateNamespaces(t *testing.T) {
	store := newTestStore(t)

	// An untagged memory whose id happens to equal another memory's tag.
	saveOldMemory(t, store, "code", "an untagged memory that happens to be called code", nil)
	saveOldMemory(t, store, "mem-tagged-1", "a genuinely code-tagged memory", []string{"code"})
	saveOldMemory(t, store, "mem-tagged-2", "another genuinely code-tagged memory", []string{"code"})

	results, err := store.Compact(7*24*time.Hour, 3)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 group (the two code-tagged memories), got %d", len(results))
	}
	for _, id := range results[0].OriginalIDs {
		if id == "code" {
			t.Errorf("the untagged memory %q was merged into the %q tag group because its id collided with the tag name", id, "code")
		}
	}
	if results[0].Count != 2 {
		t.Errorf("expected 2 memories in the code group, got %d (%v)", results[0].Count, results[0].OriginalIDs)
	}
}
