package main

import (
	"strings"
	"testing"
)

// These tests pin what the guard decides, where degraded_run_test.go pins what
// it reports. Ported from a44e030a ("Give the spend ceiling and the review loop
// their first tests"), which wrote them against an enforceSpendCeilings that
// main never took; main extracted run(config) instead, so they drive that and
// the fleet fixture beside it.
//
// None of them reproduces the guard's filters. A fixture that copies a filter
// agrees with it when it drifts and stays green, which is how a drift of the
// link filter to "sess" once scored UNNOTICED: no session links reads as no
// sessions, no sessions reads as $0, and a $5 ceiling never fires.

// TestATaskOverItsCeilingIsHeld is the happy path. The hold reason is what a
// person reads on the parked card, and the only record of why the work stopped,
// so it has to carry both amounts.
func TestATaskOverItsCeilingIsHeld(t *testing.T) {
	f := &fleet{
		items: []noteboardItem{ceilingOwner("task-1", 5)},
		links: map[string][]string{"task-1": {"autoworker-nightly-1"}},
		cost:  map[string]float64{"autoworker-nightly-1": 7.5},
	}
	if got := run(f.start(t)); got != 0 {
		t.Errorf("a tick that held what it meant to exited %d, want 0", got)
	}
	reason, ok := f.holdReasons["task-1"]
	if !ok {
		t.Fatalf("task-1 spent $7.50 against a $5.00 ceiling and was NOT held; holds=%v", f.holdCalls)
	}
	for _, want := range []string{"7.50", "5.00"} {
		if !strings.Contains(reason, want) {
			t.Errorf("hold reason %q does not name %s", reason, want)
		}
	}
}

// TestTheCeilingHoldsWhenItIsReached is the boundary. The package doc says a
// task is held when spend has REACHED its ceiling, so spending exactly the pot
// holds. Every other test here sits far from the line, so `<` turned into `<=`
// would pass them all.
func TestTheCeilingHoldsWhenItIsReached(t *testing.T) {
	for name, tc := range map[string]struct {
		cost     float64
		wantHeld bool
	}{
		"a cent under the ceiling": {4.99, false},
		"exactly at the ceiling":   {5.00, true},
		"a cent over":              {5.01, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fleet{
				items: []noteboardItem{ceilingOwner("task-1", 5)},
				links: map[string][]string{"task-1": {"s1"}},
				cost:  map[string]float64{"s1": tc.cost},
			}
			run(f.start(t))
			if held := len(f.holdCalls) == 1; held != tc.wantHeld {
				t.Errorf("$%.2f against a $5.00 ceiling: held=%v, want %v", tc.cost, held, tc.wantHeld)
			}
		})
	}
}

// TestTheCeilingIsPricedAgainstTheSubtreeNotTheRowItSitsOn. The autoworker's
// prompt tells a worker to split a large todo into children, so a ceiling
// priced per row is walked around by exactly the split we ask for. Neither row
// breaches alone here; only the total does. And the OWNER is what gets held:
// holding the child that tripped it would leave the parent and its siblings
// spending against an empty pot.
func TestTheCeilingIsPricedAgainstTheSubtreeNotTheRowItSitsOn(t *testing.T) {
	parent := "parent"
	f := &fleet{
		items: []noteboardItem{
			ceilingOwner("parent", 5),
			{ID: "child", ParentID: &parent},
		},
		links: map[string][]string{"parent": {"s-parent"}, "child": {"s-child"}},
		cost:  map[string]float64{"s-parent": 2, "s-child": 4},
	}
	run(f.start(t))
	if len(f.holdCalls) != 1 || f.holdCalls[0] != "parent" {
		t.Fatalf("holds=%v, want [parent]: $2 on the parent plus $4 on its child is $6 against a $5 ceiling", f.holdCalls)
	}
}

// TestAHeldMemberOfATaskTreeStillCountsTowardTheCeiling. A child parked on its
// own is still money spent. The guard skips a held OWNER, and it must not
// widen that skip to a held descendant, or the ancestor's ceiling quietly rises
// by whatever the parked child cost.
func TestAHeldMemberOfATaskTreeStillCountsTowardTheCeiling(t *testing.T) {
	parent, heldAt := "parent", "2026-08-08T00:00:00Z"
	f := &fleet{
		items: []noteboardItem{
			ceilingOwner("parent", 5),
			{ID: "child", ParentID: &parent, HeldAt: &heldAt},
		},
		links: map[string][]string{"parent": {"s-parent"}, "child": {"s-child"}},
		cost:  map[string]float64{"s-parent": 1, "s-child": 4.5},
	}
	run(f.start(t))
	if len(f.holdCalls) != 1 || f.holdCalls[0] != "parent" {
		t.Fatalf("holds=%v, want [parent]: the parked child's $4.50 still counts", f.holdCalls)
	}
}

// TestOnlySessionLinksAreBilled. A card links to predictions, notes and pull
// requests through the same route as sessions. The rows below are written as
// wire literals, not built from a constant, because a renamed constant does not
// rename the rows already in kanban-store.
func TestOnlySessionLinksAreBilled(t *testing.T) {
	f := &fleet{
		items: []noteboardItem{ceilingOwner("task-1", 5)},
		links: map[string][]string{"task-1": {"s-real"}},
		// The decoy's ref is a costed session id, so counting it would breach.
		otherLinks: map[string][]map[string]any{
			"task-1": {{"entity_type": "prediction", "entity_ref": "s-decoy"}},
		},
		cost: map[string]float64{"s-real": 1, "s-decoy": 100},
	}
	run(f.start(t))
	if len(f.holdCalls) != 0 {
		t.Errorf("a $1 task was held against its $5 ceiling: a non-session link was billed; holds=%v", f.holdCalls)
	}
}

// TestATaskWithNoCeilingIsNeverPriced. Most of the board has no ceiling, and
// pricing it would ask kanban-store for links on every item on every tick. The
// links route is broken here so that pricing would also paint the run red.
func TestATaskWithNoCeilingIsNeverPriced(t *testing.T) {
	f := &fleet{
		items:       []noteboardItem{{ID: "task-1"}},
		linksStatus: 500,
	}
	if got := run(f.start(t)); got != 0 {
		t.Errorf("a board with no ceiling on it exited %d, want 0", got)
	}
	if len(f.linksAskedOf) != 0 {
		t.Errorf("priced an uncapped task (asked kanban-store for the links of %v)", f.linksAskedOf)
	}
}
