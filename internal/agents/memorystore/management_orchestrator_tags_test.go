package memorystore

import (
	"fmt"
	"testing"
)

// seedOrchestratorMemories writes one memory per named orchestrator and returns
// the store. Each memory carries a tag naming its owner and a tag naming itself:
// without the second, two memories of the same orchestrator hold identical tag
// lists, and a lookup that hands every memory the first one's tags reads as
// correct.
func seedOrchestratorMemories(t *testing.T, orchestrators ...string) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir() + "/memory.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	for i, orch := range orchestrators {
		id := fmt.Sprintf("mem-%s-%d", orch, i)
		m := Memory{
			ID:           id,
			Content:      fmt.Sprintf("memory %d owned by %s", i, orch),
			Importance:   0.5,
			Source:       "test",
			Orchestrator: orch,
			Tags:         []string{"code", "owner-" + orch, "id-" + id},
		}
		if err := s.Save(m); err != nil {
			t.Fatalf("save memory for %s: %v", orch, err)
		}
	}
	return s
}

// A tag lookup that cannot run must say so. Answering with empty tag lists and a
// nil error reports a broken store as a page of untagged memories, and the
// caller has no way to tell the two apart.
//
// searchInternal was repaired for exactly this and ListByOrchestrator was left
// behind, so the assertion is on the error being RETURNED, not merely on the
// call failing somewhere.
func TestListByOrchestratorReturnsATagLookupFailureRatherThanUntaggedMemories(t *testing.T) {
	s := seedOrchestratorMemories(t, "inber", "inber", "openclaw")

	if _, err := s.db.Exec("DROP TABLE memory_tags"); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListByOrchestrator("inber", 10, 0)
	if err == nil {
		t.Fatalf("list reported success with no memory_tags table, returning %d memories", len(got))
	}
	if got != nil {
		t.Fatalf("list returned %d memories alongside its error; a partial page invites the caller to use it", len(got))
	}
}

// The failure has to survive the page being empty of tags for an innocent
// reason too: a memory with no tags is not evidence the lookup worked.
func TestListByOrchestratorReportsATagFailureEvenWhenNoMemoryHasTags(t *testing.T) {
	s, err := NewStore(t.TempDir() + "/memory.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.Save(Memory{
		ID: "mem-untagged", Content: "no tags at all", Importance: 0.5,
		Source: "test", Orchestrator: "inber",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DROP TABLE memory_tags"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ListByOrchestrator("inber", 10, 0); err == nil {
		t.Fatal("list reported success with no memory_tags table; an untagged page is not proof the lookup ran")
	}
}

// The repair must not cost the tags themselves: each memory keeps its own,
// scoped to the orchestrator asked for.
func TestListByOrchestratorGivesEachMemoryItsOwnTags(t *testing.T) {
	s := seedOrchestratorMemories(t, "inber", "openclaw", "inber")

	got, err := s.ListByOrchestrator("inber", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d memories for inber, want 2", len(got))
	}
	for _, m := range got {
		if m.Orchestrator != "inber" {
			t.Fatalf("memory %s belongs to %q, not the orchestrator asked for", m.ID, m.Orchestrator)
		}
		var ownTag bool
		for _, tag := range m.Tags {
			if tag == "id-"+m.ID {
				ownTag = true
			}
			if tag == "owner-openclaw" {
				t.Fatalf("memory %s picked up another orchestrator's tag: %v", m.ID, m.Tags)
			}
		}
		if !ownTag {
			t.Fatalf("memory %s came back with tags %v; it was saved carrying id-%s, so these are somebody else's", m.ID, m.Tags, m.ID)
		}
	}
}

// A memory that genuinely has no tags gets an empty slice, not nil. The
// distinction is visible to every JSON caller: nil marshals to null and an empty
// slice to [], and the two branch differently in a client.
func TestListByOrchestratorGivesAnUntaggedMemoryAnEmptySliceNotNil(t *testing.T) {
	s := seedOrchestratorMemories(t, "inber")

	if err := s.Save(Memory{
		ID: "mem-untagged", Content: "no tags at all", Importance: 0.5,
		Source: "test", Orchestrator: "inber",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListByOrchestrator("inber", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, m := range got {
		if m.ID != "mem-untagged" {
			continue
		}
		seen = true
		if m.Tags == nil {
			t.Fatal("an untagged memory came back with a nil tag list; it marshals to null, not []")
		}
		if len(m.Tags) != 0 {
			t.Fatalf("an untagged memory came back with tags %v", m.Tags)
		}
	}
	if !seen {
		t.Fatal("the untagged memory did not come back at all")
	}
}

// The batched lookup replaced one query per memory. Pin that it still holds at a
// page size past the chunk boundary the batching exists for — the previous
// per-memory loop could not fail this way, and a future edit that rebuilds a
// single IN list would.
func TestListByOrchestratorTagsSurviveAPageLargerThanOneChunk(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds more than one tag-lookup chunk")
	}
	const n = tagLookupChunkSize + 7
	s, err := NewStore(t.TempDir() + "/memory.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	for i := 0; i < n; i++ {
		if err := s.Save(Memory{
			ID: fmt.Sprintf("mem-%06d", i), Content: fmt.Sprintf("memory number %d", i),
			Importance: 0.5, Source: "test", Orchestrator: "inber",
			Tags: []string{fmt.Sprintf("batch-%d", i%7)},
		}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	got, err := s.ListByOrchestrator("inber", n, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("got %d memories, want %d", len(got), n)
	}
	for _, m := range got {
		if len(m.Tags) != 1 {
			t.Fatalf("memory %s came back with %d tags, want 1: %v", m.ID, len(m.Tags), m.Tags)
		}
	}
}
