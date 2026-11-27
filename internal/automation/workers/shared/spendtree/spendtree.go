// Package spendtree answers two questions about a task's money, and answers them
// over the whole TREE of work rather than a single item.
//
// Both the autoworker and spend-ceiling-guard need the same two answers, and if
// they ever disagreed the gate would enforce one number while the dispatcher
// budgeted for another. So they share this.
//
// Why a tree at all: the autoworker's own prompt instructs a worker that a
// too-large todo must be SPLIT into child todos, and kanban-scoper decomposes a
// goal into sub-cards. If a $5 ceiling applied only to the item it was written
// on, the very decomposition we ask for would walk around it — five children at
// $5 each is $25 against a "$5" cap. So:
//
//   - A ceiling covers its whole subtree. It is a shared pot, not an allowance
//     handed to each child.
//   - Spend rolls UP: what a task cost is what it and all its descendants cost.
//   - A child with no ceiling of its own inherits the nearest ancestor that has
//     one.
//
// The parent edge is noteboard's parent_id, which is the canonical one:
// noteboard itself inherits holds down it. (kanban-store also has a `parent_card`
// card LINK, which kanban-dispatcher uses for closure roll-up; kanban-scoper now
// writes both, so the two agree.)
package spendtree

import "fmt"

// Item is the slice of a noteboard item this package needs. Deliberately narrow:
// the callers hand us whatever they already fetched.
type Item struct {
	ID            string
	ParentID      string
	AutoHoldAtUSD *float64
}

// Tree indexes items by id and by parent so ancestors and descendants are both
// cheap to walk.
type Tree struct {
	byID     map[string]Item
	children map[string][]string
}

func NewTree(items []Item) *Tree {
	t := &Tree{
		byID:     make(map[string]Item, len(items)),
		children: make(map[string][]string),
	}
	for _, it := range items {
		t.byID[it.ID] = it
		if it.ParentID != "" {
			t.children[it.ParentID] = append(t.children[it.ParentID], it.ID)
		}
	}
	return t
}

// EffectiveCeiling returns the ceiling governing this item and the id of the item
// that owns it — which may be an ancestor. Holding on a breach must hold the
// OWNER, not the descendant that happened to trip it: holding the child would
// leave the parent and its siblings free to keep spending against a pot that is
// already empty.
//
// Returns ok=false when no ancestor-or-self sets a ceiling.
func (t *Tree) EffectiveCeiling(id string) (ceiling float64, ownerID string, ok bool) {
	seen := map[string]bool{}
	for cur := id; cur != ""; {
		if seen[cur] {
			break // parent_id cycle; stop rather than spin
		}
		seen[cur] = true

		it, exists := t.byID[cur]
		if !exists {
			break
		}
		if it.AutoHoldAtUSD != nil {
			return *it.AutoHoldAtUSD, cur, true
		}
		cur = it.ParentID
	}
	return 0, "", false
}

// Subtree returns the item and every descendant of it. This is the set whose
// costs are summed against the ceiling.
func (t *Tree) Subtree(id string) []string {
	var out []string
	seen := map[string]bool{}
	stack := []string{id}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue // cycle-safe
		}
		seen[cur] = true
		out = append(out, cur)
		stack = append(stack, t.children[cur]...)
	}
	return out
}

// SubtreeSpendUSD totals what an item and all its descendants have cost.
//
// sessionsFor maps an item id to the bridge session ids that worked it (the
// card's session links); costOf maps a session id to its cost. A session with no
// cost recorded contributes nothing — it has not billed a turn yet.
//
// A session is counted once even if two items in the subtree link to it, which
// they can: a session that works a parent and then its child is one bill, not two.
func (t *Tree) SubtreeSpendUSD(id string, sessionsFor func(itemID string) ([]string, error), costOf map[string]float64) (float64, error) {
	counted := map[string]bool{}
	var total float64
	for _, member := range t.Subtree(id) {
		sessions, err := sessionsFor(member)
		if err != nil {
			return 0, fmt.Errorf("sessions for %s: %w", member, err)
		}
		for _, sid := range sessions {
			if counted[sid] {
				continue
			}
			counted[sid] = true
			total += costOf[sid]
		}
	}
	return total, nil
}
