package memorystore

import (
	"fmt"
	"testing"
	"time"
)

// seedTaggedMemories writes n memories, each carrying two tags, and returns the
// store. Content is unique per memory so an embedding is too.
func seedTaggedMemories(t *testing.T, n int) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir() + "/memory.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	for i := 0; i < n; i++ {
		m := Memory{
			ID:         fmt.Sprintf("mem-%06d", i),
			Content:    fmt.Sprintf("memory number %d about authentication and handlers", i),
			Importance: 0.5,
			Source:     "test",
			Tags:       []string{"code", fmt.Sprintf("batch-%d", i%7)},
		}
		if err := s.Save(m); err != nil {
			t.Fatalf("remember %d: %v", i, err)
		}
	}
	return s
}

// A candidate set larger than SQLite's bound-parameter ceiling used to come back
// with every tag list empty, because the tag lookup bound one parameter per
// candidate and its error was discarded. Both recall paths are pinned here.
//
// The size has to clear the measured ceiling or the test proves nothing: 32,766
// candidates answered fine under the old code and 32,767 did not, so anything
// below that passes either way.
func TestTagsSurviveACandidateSetLargerThanSQLitesParameterLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 32,767 memories")
	}
	const n = 32767 // one past the last candidate count the old single IN clause survived
	s := seedTaggedMemories(t, n)

	got, err := s.Search("authentication", 10)
	if err != nil {
		t.Fatalf("search over %d memories: %v", n, err)
	}
	if len(got) == 0 {
		t.Fatal("search returned nothing")
	}
	for _, m := range got {
		if len(m.Tags) == 0 {
			t.Fatalf("search result %s came back with no tags; every seeded memory has two", m.ID)
		}
	}

	built, _, err := s.BuildContext(BuildContextRequest{
		Tags:              []string{"code"},
		TokenBudget:       2000,
		IncludeAlwaysLoad: true,
	})
	if err != nil {
		t.Fatalf("build context over %d memories: %v", n, err)
	}
	if len(built) == 0 {
		t.Fatal("build context returned nothing")
	}
	for _, m := range built {
		if len(m.Tags) == 0 {
			t.Fatalf("context memory %s came back with no tags; every seeded memory has two", m.ID)
		}
	}
}

// A tag lookup that cannot run must say so rather than hand back untagged
// memories, which rank as if they matched nothing.
func TestATagLookupFailureIsReturnedNotSwallowed(t *testing.T) {
	s := seedTaggedMemories(t, 5)

	if _, err := s.db.Exec("DROP TABLE memory_tags"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Search("authentication", 10); err == nil {
		t.Fatal("search reported success with no memory_tags table")
	}
	if _, _, err := s.BuildContext(BuildContextRequest{TokenBudget: 2000, IncludeAlwaysLoad: true}); err == nil {
		t.Fatal("build context reported success with no memory_tags table")
	}
}

// Tags reach the caller at all, at a size where the chunking splits.
func TestEveryCandidateKeepsItsOwnTagsAcrossChunkBoundaries(t *testing.T) {
	const n = tagLookupChunkSize*2 + 13
	s := seedTaggedMemories(t, n)

	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("mem-%06d", i)
	}
	tagsByMemory, err := s.loadTagsForMemories(ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(tagsByMemory) != n {
		t.Fatalf("got tags for %d memories, want %d", len(tagsByMemory), n)
	}
	for i, id := range ids {
		want := fmt.Sprintf("batch-%d", i%7)
		var found bool
		for _, tag := range tagsByMemory[id] {
			if tag == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s carries %v, missing its own %s", id, tagsByMemory[id], want)
		}
	}
}

// BuildContext ranks on importance, tags and recency. It must not pay to read
// and decode the embedding vector, and the name of its scan function says the
// field comes back empty so nobody quietly ranks on it.
func TestBuildContextDoesNotCarryEmbeddings(t *testing.T) {
	s := seedTaggedMemories(t, 20)

	built, _, err := s.BuildContext(BuildContextRequest{TokenBudget: 32000, IncludeAlwaysLoad: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(built) == 0 {
		t.Fatal("build context returned nothing")
	}
	for _, m := range built {
		if len(m.Embedding) != 0 {
			t.Fatalf("%s came back with a %d-element embedding; BuildContext does not rank on it", m.ID, len(m.Embedding))
		}
	}

	// Search selects the whole row and so still returns the stored embedding,
	// which is the difference from BuildContext this test exists to pin.
	//
	// ⚠️ It no longer returns it because anything RANKS on it. Search moved to
	// BM25 over an FTS5 index (noteboard todo
	// 0c31b497-0418-4577-b156-6699918ef5a8), so the embedding column is now
	// written by Save and read by nothing. That makes this half of the test a
	// statement about the response shape, not about scoring, and the column is
	// a deletion nobody has scoped yet — it is in the Memory struct that
	// agent-store and inber also carry, so removing it is a three-repo change.
	got, err := s.Search("authentication", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("search returned nothing")
	}
	for _, m := range got {
		if len(m.Embedding) == 0 {
			t.Fatalf("%s came back without an embedding; Search returns the whole row, unlike BuildContext", m.ID)
		}
	}
}

// The sort that replaced the quadratic one must still put the highest score
// first, and must still return exactly `limit` results.
func TestSearchStillRanksHighestScoreFirst(t *testing.T) {
	s, err := NewStore(t.TempDir() + "/memory.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Same content, different importance: score is similarity × importance ×
	// recency, so importance alone decides the order.
	for i, importance := range []float64{0.1, 0.9, 0.5, 0.3, 0.7} {
		if err := s.Save(Memory{
			ID:           fmt.Sprintf("mem-%d", i),
			Content:      "authentication handler for the session gateway",
			Importance:   importance,
			Source:       "test",
			LastAccessed: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Search("authentication handler", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}
	want := []float64{0.9, 0.7, 0.5}
	for i, m := range got {
		if m.Importance != want[i] {
			t.Fatalf("result %d has importance %v, want %v (order: %v)", i, m.Importance, want[i], got)
		}
	}
}
