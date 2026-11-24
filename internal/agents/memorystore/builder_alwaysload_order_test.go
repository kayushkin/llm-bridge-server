package memorystore

import (
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// alwaysLoadRow is one row of the head under test. It carries the three inputs
// calculateScore reads — importance, tags and lastAccessed — because the point
// of these tests is that none of them may reach the head's order.
type alwaysLoadRow struct {
	id           string
	tags         []string
	importance   float64
	tokens       int
	lastAccessed time.Time
}

// hostAlwaysLoadRows is the always-load set inber actually carries, measured on
// ~/.inber/memory.db and ~/repos/inber/.inber/memory.db (both stores hold these
// three ids, these tags, and these importances).
//
// The two 0.9s are the reason this is worth a test rather than an argument:
// memory-instructions and tool-registry are TIED on importance, so the head is
// one tag match, one updateAccess bump or one recency-bucket crossing away from
// reordering. Nothing about the head itself would have changed.
func hostAlwaysLoadRows(now time.Time) []alwaysLoadRow {
	return []alwaysLoadRow{
		{id: "identity", tags: []string{"identity", "always-load"}, importance: 1.0, tokens: 2211, lastAccessed: now},
		{id: "memory-instructions", tags: []string{"instructions", "memory", "always-load"}, importance: 0.9, tokens: 207, lastAccessed: now},
		{id: "tool-registry", tags: []string{"tools", "capabilities", "system"}, importance: 0.9, tokens: 722, lastAccessed: now},
	}
}

func storeWithAlwaysLoad(t *testing.T, name string, rows []alwaysLoadRow) *Store {
	t.Helper()

	s, err := NewStore(filepath.Join(t.TempDir(), name+".db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	for _, r := range rows {
		m := Memory{
			ID:           r.id,
			Content:      "content of " + r.id,
			Tags:         r.tags,
			Importance:   r.importance,
			Tokens:       r.tokens,
			AlwaysLoad:   true,
			LastAccessed: r.lastAccessed,
			CreatedAt:    r.lastAccessed,
			Source:       "test",
		}
		if err := s.Save(m); err != nil {
			t.Fatalf("save %s: %v", m.ID, err)
		}
	}
	return s
}

// headOf returns the ids of the leading always-load run of a BuildContext
// result. It stops at the first non-always-load memory rather than filtering,
// so a test that expected a contiguous head cannot pass on a scattered one.
func headOf(t *testing.T, memories []Memory) []string {
	t.Helper()
	var out []string
	for _, m := range memories {
		if !m.AlwaysLoad {
			break
		}
		out = append(out, m.ID)
	}
	return out
}

func equalIDs(a, b []string) bool {
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

// TestAlwaysLoadHeadIgnoresTheCurrentMessagesTags is the sharpest of the three
// score inputs, because it changes on every single turn.
//
// inber calls AutoTag on the text of the message being answered and hands the
// result to BuildContext as req.Tags (engine/turn_prompt.go). calculateScore
// adds 0.3 for each candidate tag in that set. So on a turn whose message
// mentions the agent's role, "identity" is in the set; on the next turn it is
// not. If score decided the head's order, the front of the cached system prefix
// would be a function of what the user just typed.
func TestAlwaysLoadHeadIgnoresTheCurrentMessagesTags(t *testing.T) {
	now := time.Now()
	s := storeWithAlwaysLoad(t, "tags", hostAlwaysLoadRows(now))

	// Every tag any of the three rows carries, one query each, plus the
	// no-match case. A head that moved for any of these moved for a reason
	// that has nothing to do with the head.
	queries := [][]string{
		nil,
		{"identity"},
		{"instructions"},
		{"memory"},
		{"tools"},
		{"capabilities"},
		{"system"},
		{"always-load"},
		{"tools", "capabilities", "system"},
	}

	var want []string
	for i, tags := range queries {
		got, _, err := s.BuildContext(BuildContextRequest{
			Tags:              tags,
			TokenBudget:       32000,
			IncludeAlwaysLoad: true,
		})
		if err != nil {
			t.Fatalf("BuildContext(tags=%v): %v", tags, err)
		}
		head := headOf(t, got)
		if len(head) != 3 {
			t.Fatalf("tags=%v: expected all 3 always-load memories in a contiguous head, got %v", tags, head)
		}
		if i == 0 {
			want = head
			continue
		}
		if !equalIDs(head, want) {
			t.Fatalf("the current message's tags reordered the cached head\n  tags=%v\n  got  %v\n  want %v", tags, head, want)
		}
	}

	// State the order, so a change that keeps the head merely self-consistent
	// while moving it still has to be deliberate.
	expected := []string{"identity", "memory-instructions", "tool-registry"}
	if !equalIDs(want, expected) {
		t.Fatalf("head order changed: got %v, want %v", want, expected)
	}
}

// TestAlwaysLoadHeadIgnoresImportanceAndRecency covers the two inputs that move
// on their own, with no help from the user.
//
// updateAccess multiplies importance by 1.01 on every read and DecayImportance
// multiplies it by 0.99 daily, so the two rows tied at 0.9 on this host do not
// stay tied. calculateScore's recency bonus is a step function of wall-clock
// (+0.2 under a day, +0.1 under a week, 0 beyond), so a row also moves as its
// last_accessed ages past either boundary.
//
// Both stores below hold the same three ids. Only the score inputs differ, and
// they differ in the direction that would invert the head if score decided it:
// tool-registry is made the highest-scoring row and identity the lowest.
func TestAlwaysLoadHeadIgnoresImportanceAndRecency(t *testing.T) {
	now := time.Now()

	flipped := hostAlwaysLoadRows(now)
	for i := range flipped {
		switch flipped[i].id {
		case "identity":
			flipped[i].importance = 0.5                      // decayed
			flipped[i].lastAccessed = now.AddDate(0, 0, -30) // no recency bonus
		case "memory-instructions":
			flipped[i].importance = 0.7
			flipped[i].lastAccessed = now.AddDate(0, 0, -3) // +0.1
		case "tool-registry":
			flipped[i].importance = 1.0
			flipped[i].lastAccessed = now // +0.2
		}
	}

	base := storeWithAlwaysLoad(t, "base", hostAlwaysLoadRows(now))
	moved := storeWithAlwaysLoad(t, "moved", flipped)

	req := BuildContextRequest{TokenBudget: 32000, IncludeAlwaysLoad: true}

	a, tokensA, err := base.BuildContext(req)
	if err != nil {
		t.Fatalf("BuildContext(base): %v", err)
	}
	b, tokensB, err := moved.BuildContext(req)
	if err != nil {
		t.Fatalf("BuildContext(moved): %v", err)
	}

	headA, headB := headOf(t, a), headOf(t, b)
	if len(headA) != 3 || len(headB) != 3 {
		t.Fatalf("expected a 3-row head from both stores, got %v and %v", headA, headB)
	}
	if !equalIDs(headA, headB) {
		t.Fatalf("importance and recency reordered the cached head\n  base  %v\n  moved %v", headA, headB)
	}
	if tokensA != tokensB {
		t.Fatalf("head order changed the token total: %d vs %d", tokensA, tokensB)
	}
}

// TestAlwaysLoadHeadSurvivesImportanceConvergingOnTheCap is the route this head
// actually reaches, as opposed to the hand-set importances above.
//
// updateAccess raises importance by MIN(1.0, importance * 1.01) — a ceiling, not
// an open climb — so importance does not merely drift, it CONVERGES. This host's
// three rows sit at 1.0 / 0.9 / 0.9 with access_count 0, and Get is the only
// caller of updateAccess, so eleven Gets on either 0.9 row take it to the cap
// (0.9 * 1.01^11 = 1.004, clamped to 1.0) and the importance distinction between
// all three disappears for good.
//
// That is worse than a drift, because of what the old comparator fell through
// to next: token count, smallest first. At the cap the head would have gone from
//
//	identity(2211), memory-instructions(207), tool-registry(722)
//
// to
//
//	memory-instructions(207), tool-registry(722), identity(2211)
//
// — identity from the front of the cached prefix to the back, permanently, with
// nothing about the memories themselves having changed. Ordering by id makes the
// cap unreachable as a cause.
func TestAlwaysLoadHeadSurvivesImportanceConvergingOnTheCap(t *testing.T) {
	now := time.Now()

	converged := hostAlwaysLoadRows(now)
	for i := range converged {
		converged[i].importance = 1.0 // every row has reached the ceiling
	}

	before := storeWithAlwaysLoad(t, "pre-cap", hostAlwaysLoadRows(now))
	after := storeWithAlwaysLoad(t, "at-cap", converged)

	req := BuildContextRequest{TokenBudget: 32000, IncludeAlwaysLoad: true}

	a, _, err := before.BuildContext(req)
	if err != nil {
		t.Fatalf("BuildContext(pre-cap): %v", err)
	}
	b, _, err := after.BuildContext(req)
	if err != nil {
		t.Fatalf("BuildContext(at-cap): %v", err)
	}

	headBefore, headAfter := headOf(t, a), headOf(t, b)
	if !equalIDs(headBefore, headAfter) {
		t.Fatalf("importance reaching the 1.0 cap reordered the cached head\n  before %v\n  after  %v", headBefore, headAfter)
	}
	if headAfter[0] != "identity" {
		t.Fatalf("identity left the front of the cached prefix: head is %v", headAfter)
	}
}

// TestOrdinaryMemoriesAreStillRankedByScore is the other half of the change
// above, and it was missing: nothing in this package distinguished "ordered by
// score" from "ordered by id" for ordinary memories, because every existing
// ordering test builds a set that scores identically, where the two are the same
// answer.
//
// That gap is not academic now. The change above puts an id comparison inside
// this comparator for the first time, one line above the score comparison, so
// the cheapest wrong edit here — dropping the AlwaysLoad condition and letting
// the id rule decide every pair — silently replaces relevance ranking with
// alphabetical order for the entire store. Verified against the suite as it
// stood: that edit passed everything.
//
// The ids below sort in the OPPOSITE order to the scores, so the two hypotheses
// cannot both be satisfied.
func TestOrdinaryMemoriesAreStillRankedByScore(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "ranked.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer s.Close()

	now := time.Now()
	rows := []struct {
		id         string
		importance float64
	}{
		{"a-least-important", 0.5},
		{"b-middling", 0.7},
		{"c-most-important", 0.9},
	}
	for _, r := range rows {
		if err := s.Save(Memory{
			ID:           r.id,
			Content:      "content of " + r.id,
			Importance:   r.importance,
			Tokens:       100,
			LastAccessed: now,
			CreatedAt:    now,
			Source:       "test",
		}); err != nil {
			t.Fatalf("save %s: %v", r.id, err)
		}
	}

	got, _, err := s.BuildContext(BuildContextRequest{TokenBudget: 32000, IncludeAlwaysLoad: true})
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	want := []string{"c-most-important", "b-middling", "a-least-important"}
	if !equalIDs(idsOf(got), want) {
		t.Fatalf("ordinary memories are not ranked by score\n  got  %v\n  want %v (ascending id order would be the reverse)", idsOf(got), want)
	}

	// The tag bonus is the other thing score carries, and it has to outrank the
	// importance the ids were built to contradict.
	got, _, err = s.BuildContext(BuildContextRequest{
		Tags:              []string{"boost"},
		TokenBudget:       32000,
		IncludeAlwaysLoad: true,
	})
	if err != nil {
		t.Fatalf("BuildContext(tagged): %v", err)
	}
	if !equalIDs(idsOf(got), want) {
		t.Fatalf("untagged baseline moved: %v", idsOf(got))
	}

	if err := s.Save(Memory{
		ID:           "a-least-important",
		Content:      "content of a-least-important",
		Tags:         []string{"boost"},
		Importance:   0.5,
		Tokens:       100,
		LastAccessed: now,
		CreatedAt:    now,
		Source:       "test",
	}); err != nil {
		t.Fatalf("re-save with tag: %v", err)
	}

	got, _, err = s.BuildContext(BuildContextRequest{
		Tags:              []string{"boost"},
		TokenBudget:       32000,
		IncludeAlwaysLoad: true,
	})
	if err != nil {
		t.Fatalf("BuildContext(tagged): %v", err)
	}
	// 0.5 + 0.3 = 0.8, which clears b-middling's 0.7 and not c's 0.9.
	wantTagged := []string{"c-most-important", "a-least-important", "b-middling"}
	if !equalIDs(idsOf(got), wantTagged) {
		t.Fatalf("a matching tag did not lift the memory it matched\n  got  %v\n  want %v", idsOf(got), wantTagged)
	}
}

// TestAlwaysLoadHeadOrderCannotChangeMembership is the claim that makes ordering
// the head by id safe rather than merely stable: score is not being overruled on
// a decision it was making, because it never made one here.
//
// The budget cut appends an always-load memory whether or not it fits
// (builder.go, the m.AlwaysLoad branch), so every always-load candidate reaches
// the result and the running total after the head is the sum of the head
// whatever order it went in. This test asserts that at a budget far below the
// head's own size, which is where an ordinary memory would have been dropped.
func TestAlwaysLoadHeadOrderCannotChangeMembership(t *testing.T) {
	now := time.Now()
	rows := hostAlwaysLoadRows(now)

	total := 0
	for _, r := range rows {
		total += r.tokens
	}

	s := storeWithAlwaysLoad(t, "membership", rows)

	// One tenth of what the head alone needs.
	got, tokens, err := s.BuildContext(BuildContextRequest{
		TokenBudget:       total / 10,
		IncludeAlwaysLoad: true,
	})
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	head := headOf(t, got)
	if len(head) != len(rows) {
		t.Fatalf("a budget below the head dropped part of it: got %v, want all %d", head, len(rows))
	}
	if tokens != total {
		t.Fatalf("token total %d, want %d — the head is appended over budget, so it is the sum of the head", tokens, total)
	}
}
