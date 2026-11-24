package memorystore

import "testing"

// TestVolatilePrefixMatchesTheBareprefix pins the boundary the hand-rolled
// prefix tests got wrong.
//
// isVolatileMemory decides which memories partitionStableFirst pushes to the
// end, i.e. which ones a caller may keep out of its cached system prefix. It
// used to ask `len(m.ID) > 8 && m.ID[:8] == "fileref:"`, which is HasPrefix with
// a strict inequality where it needed a loose one: an id that is exactly the
// bare prefix has length 8, fails `> 8`, and came back stable.
//
// inber implements the same predicate over the same ids with strings.HasPrefix
// (engine/turn_prompt.go, isVolatileMemoryID), so these ids were the one input
// on which the two copies of this rule returned different answers — one would
// cache the memory in the system prefix, the other would treat it as volatile.
func TestVolatilePrefixMatchesTheBarePrefix(t *testing.T) {
	volatile := []string{
		// The bare prefixes — the regression.
		"fileref:",
		"recent:",
		"file:",
		// The ordinary shapes, which always worked and must keep working.
		"fileref:builder.go",
		"recent:cmd/main.go",
		"file:/etc/hosts",
	}
	for _, id := range volatile {
		if !isVolatileMemory(Memory{ID: id}) {
			t.Errorf("id %q should be volatile", id)
		}
	}

	stable := []string{
		"",
		"identity",
		"memory-instructions",
		"tool-registry",
		// One character short of each prefix, so a fix that over-matched by
		// dropping the comparison entirely would be caught here.
		"fileref",
		"recent",
		"file",
		// A prefix that only appears later in the id is not a prefix.
		"note-about-fileref:things",
	}
	for _, id := range stable {
		if isVolatileMemory(Memory{ID: id}) {
			t.Errorf("id %q should be stable", id)
		}
	}
}

// TestVolatileRecentTagStillCounts guards the branch below the prefixes, which
// the prefix change did not touch. It is the half of this predicate that inber's
// copy does NOT implement — see the open todo on the two rules disagreeing about
// the `recent` tag; this test states the current answer, it does not settle that.
func TestVolatileRecentTagStillCounts(t *testing.T) {
	if !isVolatileMemory(Memory{ID: "some-uuid", Tags: []string{"recent"}}) {
		t.Error(`a memory tagged "recent" should be volatile`)
	}
	if isVolatileMemory(Memory{ID: "some-uuid", Tags: []string{"recently", "recent-ish"}}) {
		t.Error("only the exact tag counts")
	}
}
