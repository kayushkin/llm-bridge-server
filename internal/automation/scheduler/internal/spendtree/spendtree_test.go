package spendtree

import "testing"

func usd(v float64) *float64 { return &v }

func noSessions(string) ([]string, error) { return nil, nil }

// The escape hatch this package exists to close: a $5 ceiling on a parent must
// not become $5 PER CHILD. Five children under a "$5" cap would be $25.
func TestChildrenInheritTheAncestorCeiling(t *testing.T) {
	tree := NewTree([]Item{
		{ID: "parent", AutoHoldAtUSD: usd(5)},
		{ID: "child", ParentID: "parent"},
		{ID: "grandchild", ParentID: "child"},
	})

	for _, id := range []string{"parent", "child", "grandchild"} {
		ceiling, owner, ok := tree.EffectiveCeiling(id)
		if !ok {
			t.Fatalf("%s: no ceiling — it escaped its ancestor's cap", id)
		}
		if ceiling != 5 {
			t.Errorf("%s: ceiling = %v, want 5", id, ceiling)
		}
		// The OWNER is what gets held on a breach. Holding the grandchild would
		// leave the parent and its siblings spending against an empty pot.
		if owner != "parent" {
			t.Errorf("%s: ceiling owner = %q, want \"parent\"", id, owner)
		}
	}
}

// A child's own ceiling is tighter than the pot it sits in, and wins for itself.
func TestNearestAncestorWins(t *testing.T) {
	tree := NewTree([]Item{
		{ID: "parent", AutoHoldAtUSD: usd(20)},
		{ID: "child", ParentID: "parent", AutoHoldAtUSD: usd(3)},
	})
	ceiling, owner, _ := tree.EffectiveCeiling("child")
	if ceiling != 3 || owner != "child" {
		t.Fatalf("child's own ceiling should win: got %v owned by %q", ceiling, owner)
	}
}

func TestNoCeilingAnywhereMeansNoCeiling(t *testing.T) {
	tree := NewTree([]Item{
		{ID: "parent"},
		{ID: "child", ParentID: "parent"},
	})
	if _, _, ok := tree.EffectiveCeiling("child"); ok {
		t.Fatal("reported a ceiling where the whole tree has none")
	}
}

// Spend rolls UP: what the parent "cost" includes everything its children spent,
// or the children are a laundering route around the cap.
func TestSpendRollsUpFromDescendants(t *testing.T) {
	tree := NewTree([]Item{
		{ID: "parent", AutoHoldAtUSD: usd(5)},
		{ID: "child", ParentID: "parent"},
		{ID: "grandchild", ParentID: "child"},
	})
	sessions := map[string][]string{
		"parent":     {"s1"},
		"child":      {"s2"},
		"grandchild": {"s3"},
	}
	cost := map[string]float64{"s1": 1.00, "s2": 2.00, "s3": 3.00}

	got, err := tree.SubtreeSpendUSD("parent", func(id string) ([]string, error) {
		return sessions[id], nil
	}, cost)
	if err != nil {
		t.Fatalf("SubtreeSpendUSD: %v", err)
	}
	if got != 6.00 {
		t.Fatalf("subtree spend = %v, want 6.00 (1+2+3) — descendants' spend was not counted", got)
	}
}

// One session that worked a parent and then its child is one bill, not two.
func TestASessionIsCountedOnce(t *testing.T) {
	tree := NewTree([]Item{
		{ID: "parent", AutoHoldAtUSD: usd(5)},
		{ID: "child", ParentID: "parent"},
	})
	sessions := map[string][]string{"parent": {"s1"}, "child": {"s1"}}
	got, _ := tree.SubtreeSpendUSD("parent", func(id string) ([]string, error) {
		return sessions[id], nil
	}, map[string]float64{"s1": 4.00})
	if got != 4.00 {
		t.Fatalf("shared session double-counted: got %v, want 4.00", got)
	}
}

// A parent_id cycle must not hang the walk.
func TestCyclesTerminate(t *testing.T) {
	tree := NewTree([]Item{
		{ID: "a", ParentID: "b", AutoHoldAtUSD: usd(5)},
		{ID: "b", ParentID: "a"},
	})
	if _, _, ok := tree.EffectiveCeiling("b"); !ok {
		t.Error("cycle walk lost the ceiling")
	}
	if got := len(tree.Subtree("a")); got != 2 {
		t.Errorf("Subtree over a cycle returned %d members, want 2", got)
	}
	if _, err := tree.SubtreeSpendUSD("a", noSessions, nil); err != nil {
		t.Errorf("SubtreeSpendUSD over a cycle: %v", err)
	}
}
