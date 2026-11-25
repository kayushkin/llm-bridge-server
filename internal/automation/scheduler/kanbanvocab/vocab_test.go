package kanbanvocab

import (
	"strings"
	"testing"
)

// TestWireValues pins every string in this package to its literal.
//
// These are not local names. Each one is either written by one binary and read
// back by another, or written into kanban-store's database and read back on a
// later tick. Changing one is a wire-format change that silently desynchronises
// two binaries or orphans existing rows, and it produces no error and no empty
// result that looks wrong. So the literals are spelled out here a second time
// on purpose: if you change a constant, this test fails, and you have to decide
// deliberately rather than by accident.
//
// The values below were taken from the four binaries before they were unified,
// so this test also records that the unification changed nothing.
func TestWireValues(t *testing.T) {
	for _, c := range []struct{ name, got, want string }{
		{"EntityTypeNote", EntityTypeNote, "note"},
		{"EntityTypeSession", EntityTypeSession, "session"},
		{"LinkTypeSession", LinkTypeSession, "session"},
		{"LinkTypeParentCard", LinkTypeParentCard, "parent_card"},

		{"ColumnInProgress", ColumnInProgress, "In Progress"},
		{"ColumnDone", ColumnDone, "Done"},
		{"ColumnFailed", ColumnFailed, "Failed"},

		{"BoardTaskCompletionLoop", BoardTaskCompletionLoop, "Task completion loop"},
		{"ColumnGoals", ColumnGoals, "Goals"},
		{"ColumnBacklog", ColumnBacklog, "Backlog"},
		{"TagTrigger", TagTrigger, "task-completion-loop"},
		{"TagSubtask", TagSubtask, "task-completion-loop-subtask"},
		{"TagScoped", TagScoped, "scoped"},
		{"TagGroupPrefix", TagGroupPrefix, "group:"},

		{"BoardAgentRuns", BoardAgentRuns, "Agent runs"},
		{"BoardAutoworkerQueue", BoardAutoworkerQueue, "Autoworker queue"},
		{"ColumnQueued", ColumnQueued, "Queued"},
		{"ColumnBlocked", ColumnBlocked, "Blocked"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q — this is a WIRE FORMAT change, not a rename", c.name, c.got, c.want)
		}
	}
}

// TestTriggerTagIsNotAPrefixOfSubtaskTagByAccident guards a real hazard: the
// trigger tag is a literal prefix of the sub-task tag, so any code that reaches
// for strings.HasPrefix instead of equality would treat every sub-task card as
// a fresh parent to decompose, and the scoper would loop.
func TestTriggerTagIsNotAPrefixOfSubtaskTagByAccident(t *testing.T) {
	if !strings.HasPrefix(TagSubtask, TagTrigger) {
		t.Fatalf("precondition changed: %q is no longer prefixed by %q; the hazard this test guards may have moved", TagSubtask, TagTrigger)
	}
	if TagSubtask == TagTrigger {
		t.Fatal("TagSubtask and TagTrigger are equal — the scoper cannot tell a parent from its own output")
	}
}

// TestColumnSetsAreNotMerged is the invariant the package doc argues for.
//
// Column names are looked up per board, so sharing a NAME across boards is
// safe. Sharing a SET is not: "Done" on Task completion loop and "Done" on
// Agent runs are two different rows with two different ids. If someone
// "simplifies" the two functions below into one shared list, the boards stop
// having distinct shapes and the next EnsureBoard call creates the wrong
// columns on whichever board it is pointed at.
func TestColumnSetsAreNotMerged(t *testing.T) {
	loop := TaskCompletionLoopColumns()
	runs := AgentRunsColumns()

	if sameStrings(loop, runs) {
		t.Fatal("the two boards' column sets are now identical — they describe different boards and must not be merged")
	}
	if !containsAll(loop, ColumnGoals, ColumnBacklog) {
		t.Errorf("Task completion loop lost a column it owns: %v", loop)
	}
	if containsAny(loop, ColumnQueued, ColumnBlocked) {
		t.Errorf("Task completion loop gained an Agent runs column: %v", loop)
	}
	if !containsAll(runs, ColumnQueued, ColumnBlocked) {
		t.Errorf("Agent runs lost a column it owns: %v", runs)
	}
	if containsAny(runs, ColumnGoals, ColumnBacklog) {
		t.Errorf("Agent runs gained a Task completion loop column: %v", runs)
	}
	// The lifecycle triple is the part that IS shared, and one binary
	// (kanban-curator) depends on it being present on every board.
	for _, c := range []string{ColumnInProgress, ColumnDone, ColumnFailed} {
		if !containsAll(loop, c) {
			t.Errorf("Task completion loop is missing lifecycle column %q — kanban-curator runs against this board", c)
		}
		if !containsAll(runs, c) {
			t.Errorf("Agent runs is missing lifecycle column %q — kanban-curator runs against this board", c)
		}
	}
}

// TestAutoworkerQueueSetIsSeparateFromAgentRuns records that the two sets
// coincide by value today and must not be collapsed into one.
//
// cmd/autoworker used to pass a single shared slice to EnsureBoard for both
// boards. That is not equivalent to calling two functions that happen to return
// equal values: the shared slice makes a future change to either board silently
// change the other, and because EnsureBoard only ever adds columns, the mistake
// surfaces as a stray column rather than an error.
func TestAutoworkerQueueSetIsSeparateFromAgentRuns(t *testing.T) {
	queue := AutoworkerQueueColumns()
	runs := AgentRunsColumns()

	if !sameStrings(queue, runs) {
		t.Logf("the two sets have diverged (%v vs %v) — that is allowed, and is exactly why they are separate functions", queue, runs)
	}
	// Whatever the values, mutating one must never reach the other.
	queue[0] = "clobbered"
	if AgentRunsColumns()[0] == "clobbered" {
		t.Fatal("AutoworkerQueueColumns and AgentRunsColumns share backing state — the shared-slice hazard is back")
	}
	if AutoworkerQueueColumns()[0] == "clobbered" {
		t.Fatal("AutoworkerQueueColumns returns shared backing state")
	}
}

// TestNeedsApprovalIsNotInTheDeclaredSet pins a known gap so it cannot be
// closed by accident. The live Autoworker queue board has a "Needs approval"
// column that no code here creates; adding it to the set below is a real
// behavior change (EnsureBoard would start creating it) and wants a decision,
// not a drive-by edit.
func TestNeedsApprovalIsNotInTheDeclaredSet(t *testing.T) {
	if containsAny(AutoworkerQueueColumns(), "Needs approval") {
		t.Error(`"Needs approval" was added to AutoworkerQueueColumns — that changes what EnsureBoard creates on a fresh board. If this is intended, delete this test and say so in the commit.`)
	}
}

// TestColumnSetsReturnFreshSlices makes sure a caller cannot mutate the
// vocabulary for every other caller in the process.
func TestColumnSetsReturnFreshSlices(t *testing.T) {
	a := TaskCompletionLoopColumns()
	a[0] = "clobbered"
	if TaskCompletionLoopColumns()[0] != ColumnGoals {
		t.Error("TaskCompletionLoopColumns returns shared backing state; a caller mutated the vocabulary")
	}
	b := AgentRunsColumns()
	b[0] = "clobbered"
	if AgentRunsColumns()[0] != ColumnQueued {
		t.Error("AgentRunsColumns returns shared backing state; a caller mutated the vocabulary")
	}
}

// TestGroupTagRoundTrip is the scoper -> kanban-store -> dispatcher path for the
// grouping tag, which is the single most drift-sensitive string in the package.
// The construction below mirrors kanban-scoper and the parse mirrors
// kanban-dispatcher's contextGroupKey (less its namespacing and its
// substitution for a card with no group tag).
func TestGroupTagRoundTrip(t *testing.T) {
	parse := func(tags []string) string {
		for _, tag := range tags {
			if strings.HasPrefix(tag, TagGroupPrefix) {
				return strings.TrimPrefix(tag, TagGroupPrefix)
			}
		}
		return ""
	}

	for _, key := range []string{"build", "docs", "a-b-c", "with space", "group:nested", ":", "1"} {
		tags := []string{TagSubtask, TagGroupPrefix + key}
		if got := parse(tags); got != key {
			t.Errorf("group key %q did not survive the round trip: got %q", key, got)
		}
	}

	// A card with no group tag must parse as "" — the dispatcher treats that
	// as ungrouped, and it must not be confused with a group literally named
	// after the prefix.
	if got := parse([]string{TagSubtask}); got != "" {
		t.Errorf("a card with no group tag parsed as %q, want empty", got)
	}
	// The bare prefix means an empty key, not a missing tag. Both parse to "",
	// which is why the dispatcher must not use the key alone as an identity.
	if got := parse([]string{TagGroupPrefix}); got != "" {
		t.Errorf("bare prefix parsed as %q, want empty", got)
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsAll(hay []string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for _, h := range hay {
			if h == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func containsAny(hay []string, needles ...string) bool {
	for _, n := range needles {
		for _, h := range hay {
			if h == n {
				return true
			}
		}
	}
	return false
}
