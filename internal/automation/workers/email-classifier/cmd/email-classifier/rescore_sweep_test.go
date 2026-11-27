package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/kanbanclient"
)

// ===================== what these tests are here to pin =====================
//
// rescoreBoard runs unattended every fifteen minutes and PATCHes the board it
// reads. Before this file, no test executed it at all: an unconditional panic()
// at its first line left the whole repo's suite green. That is the measurement
// that made it worth writing tests for, not a guess about risk.
//
// The mechanism that matters most here is -dry-run. It is one of the five
// switches that turn a tick into a no-op, and the only one whose job is to keep
// the writes from happening while everything else runs at full cost. Two halves
// of it can each break on their own and neither is visible from outside:
//
//   - a dry run that WRITES is a switch that does not switch;
//   - a dry run that writes nothing but COUNTS nothing either reports a clean
//     "0 repriced, 0 retagged" pass, which reads exactly like a board that had
//     nothing to do. That is the answer a person runs -dry-run to disprove.
//
// So every dry-run assertion below checks both halves. The rest of the file
// pins the two deliberate exclusions (a completed card, a card an agent holds),
// the no-deadline case that must leave the model's tag alone, and the failure
// path — where one card's failed write skips the rest of THAT card and must not
// abandon the board.

// patchRecord is one write the sweep sent to kanban-store.
type patchRecord struct {
	CardID string
	Fields map[string]any
}

// fakeKanban stands in for kanban-store. It records every PATCH and can be told
// to fail the ones carrying a named field, which is how the failure paths below
// are reached without a live service.
type fakeKanban struct {
	mu      sync.Mutex
	patches []patchRecord
	// failField fails any PATCH whose body carries this key. Empty accepts all.
	failField string
	server    *httptest.Server
}

func newFakeKanban(t *testing.T) *fakeKanban {
	t.Helper()
	f := &fakeKanban{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("fake kanban-store got a %s %s; the sweep must only PATCH", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var fields map[string]any
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			t.Errorf("undecodable PATCH body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// /api/cards/{id}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		cardID := parts[len(parts)-1]

		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failField != "" {
			if _, carries := fields[f.failField]; carries {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		f.patches = append(f.patches, patchRecord{CardID: cardID, Fields: fields})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeKanban) client() *kanbanclient.Client {
	return kanbanclient.New(f.server.URL, "")
}

func (f *fakeKanban) recorded() []patchRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]patchRecord, len(f.patches))
	copy(out, f.patches)
	return out
}

// patchesFor returns the accepted writes to one card, in the order they arrived.
func (f *fakeKanban) patchesFor(cardID string) []patchRecord {
	var out []patchRecord
	for _, p := range f.recorded() {
		if p.CardID == cardID {
			out = append(out, p)
		}
	}
	return out
}

// idleGate is a gate that owns nothing and cannot make an HTTP call: no card in
// these tests carries a session link unless the test adds one, so the resolve
// path is never entered. Passing a nil gate would also work — agentOwns guards
// for it — but it would skip the ownership check rather than exercise it.
func idleGate() *sessionGate {
	return &sessionGate{ongoing: map[string]bool{}}
}

// gateHolding is a gate that reports the named session as still working. The
// verdict is seeded into the per-tick cache, so agentOwns answers from memory
// and never reaches the bridge.
func gateHolding(sessionID string) *sessionGate {
	return &sessionGate{ongoing: map[string]bool{sessionID: true}}
}

// overdueRentCard is the board's worst case and the one every assertion below
// is built on: severe stakes (cat:housing + action:pay), a deadline an hour in
// the past, and a stored score from before it lapsed. Scoring it moves BOTH
// axes — priority 2 -> 5 and urgency normal -> expired — so a test can tell a
// sweep that wrote one field from a sweep that wrote both.
func overdueRentCard(id string, now time.Time) *cardState {
	due := now.Add(-time.Hour)
	return &cardState{
		ID:         id,
		Title:      "Pay August rent",
		ColumnName: columnActionNeeded,
		Tags:       []string{"email", "cat:housing", "urgency:normal"},
		Category:   "housing",
		Action:     "pay",
		Urgency:    "normal",
		DueAt:      &due,
		Priority:   2,
	}
}

func boardOf(cards ...*cardState) *boardState {
	st := &boardState{Cards: map[string]*cardState{}}
	for _, c := range cards {
		st.Cards[c.ID] = c
	}
	return st
}

// TestDryRunWritesNothingAndStillReportsWhatItWouldHaveDone pins both halves of
// -dry-run at once. A run that writes is a switch that does not switch; a run
// that reports nothing is indistinguishable from a board with nothing to do,
// which is the exact question a person runs -dry-run to answer.
func TestDryRunWritesNothingAndStillReportsWhatItWouldHaveDone(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	kanban := newFakeKanban(t)
	card := overdueRentCard("card-rent", now)

	result := rescoreBoard(kanban.client(), boardOf(card), idleGate(), now, true)

	if got := kanban.recorded(); len(got) != 0 {
		t.Fatalf("dry run sent %d PATCH(es) to kanban-store, want 0: %+v", len(got), got)
	}
	if result.Repriced != 1 {
		t.Errorf("dry run reported Repriced=%d, want 1: a dry run that counts nothing reads as a board with nothing to do", result.Repriced)
	}
	if result.Retagged != 1 {
		t.Errorf("dry run reported Retagged=%d, want 1", result.Retagged)
	}
	if result.Scanned != 1 {
		t.Errorf("Scanned=%d, want 1", result.Scanned)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped=%d, want 0: a dry run withholds the write, it does not skip the card", result.Skipped)
	}
	// The in-memory card must not be advanced either. A dry run that mutates
	// the board state hands a wrong "before" value to everything later in the
	// tick that reads the same struct.
	if card.Priority != 2 || card.Urgency != "normal" {
		t.Errorf("dry run mutated the in-memory card to priority=%d urgency=%q, want 2/normal",
			card.Priority, card.Urgency)
	}
}

// TestARealSweepWritesTagsAndPriorityAsSeparatePatches pins the write path the
// dry run suppresses: what is sent, with which values, and that the two fields
// travel as two PATCHes rather than one merged body.
func TestARealSweepWritesTagsAndPriorityAsSeparatePatches(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	kanban := newFakeKanban(t)
	card := overdueRentCard("card-rent", now)

	result := rescoreBoard(kanban.client(), boardOf(card), idleGate(), now, false)

	writes := kanban.patchesFor("card-rent")
	if len(writes) != 2 {
		t.Fatalf("sweep sent %d PATCH(es), want 2 (tags, then priority): %+v", len(writes), writes)
	}
	tags, ok := writes[0].Fields["tags"].([]any)
	if !ok {
		t.Fatalf("first PATCH carried %v, want a tags array", writes[0].Fields)
	}
	var gotTags []string
	for _, tag := range tags {
		gotTags = append(gotTags, tag.(string))
	}
	if !equalStringSlices(gotTags, []string{"email", "cat:housing", "urgency:expired"}) {
		t.Errorf("wrote tags %v, want the urgency value replaced in place and every other tag kept", gotTags)
	}
	if got := writes[1].Fields["priority"]; got != float64(5) {
		t.Errorf("wrote priority %v, want 5: severe stakes past their deadline are the top of the scale", got)
	}
	if result.Retagged != 1 || result.Repriced != 1 {
		t.Errorf("result = %+v, want one retag and one reprice", result)
	}
	// The struct the rest of the tick reads must carry what was written, or the
	// next step scores against a value the board no longer holds.
	if card.Priority != 5 || card.Urgency != "expired" {
		t.Errorf("in-memory card left at priority=%d urgency=%q, want 5/expired", card.Priority, card.Urgency)
	}
	// The tag list is carried back separately from the urgency field, and the
	// merge pass and the digest both read the tags rather than the field. A
	// card whose two halves disagree is one the rest of the tick reads wrong.
	if !equalStringSlices(card.Tags, []string{"email", "cat:housing", "urgency:expired"}) {
		t.Errorf("in-memory tags %v, want the value that was just written to the board", card.Tags)
	}
}

// TestACardInActionCompletedIsNeverRescored pins the first deliberate exclusion.
// A deadline passing does not un-complete anything a person has already settled.
func TestACardInActionCompletedIsNeverRescored(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	kanban := newFakeKanban(t)
	card := overdueRentCard("card-settled", now)
	card.ColumnName = columnActionCompleted

	result := rescoreBoard(kanban.client(), boardOf(card), idleGate(), now, false)

	if got := kanban.recorded(); len(got) != 0 {
		t.Fatalf("a completed card was written to: %+v", got)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped=%d, want 1", result.Skipped)
	}
	if result.Repriced != 0 || result.Retagged != 0 {
		t.Errorf("result = %+v, want nothing counted as changed", result)
	}
}

// TestACardAnAgentIsWorkingIsNeverRescored pins the second exclusion. Retagging
// underneath a live session fights whatever that session is doing, and the card
// is scored again on the next tick anyway once the session ends.
func TestACardAnAgentIsWorkingIsNeverRescored(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	kanban := newFakeKanban(t)
	card := overdueRentCard("card-held", now)
	card.AllLinks = []kanbanclient.CardLink{
		{CardID: card.ID, EntityType: linkTypeSession, EntityRef: "sess-1"},
	}

	result := rescoreBoard(kanban.client(), boardOf(card), gateHolding("sess-1"), now, false)

	if got := kanban.recorded(); len(got) != 0 {
		t.Fatalf("a card held by a live session was written to: %+v", got)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped=%d, want 1", result.Skipped)
	}
	if result.Repriced != 0 || result.Retagged != 0 {
		t.Errorf("result = %+v, want nothing counted as changed", result)
	}
}

// TestACardWhoseScoreHasNotMovedIsWrittenAndCountedNowhere pins the difference
// between the two reasons a card produces no write. Skipped means the sweep
// declined to score the card at all; a card that scored the same as it already
// held is scanned, scored, and simply has nothing to say. Folding the second
// into Skipped would make the run summary report an excluded board.
func TestACardWhoseScoreHasNotMovedIsWrittenAndCountedNowhere(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	kanban := newFakeKanban(t)
	// A promotion owes nothing whatever its deadline says, so it is already at
	// the floor and stays there.
	card := &cardState{
		ID:         "card-promo",
		Title:      "20% off everything",
		ColumnName: columnNoAction,
		Tags:       []string{"email", "cat:promotions", "urgency:low"},
		Category:   "promotions",
		Action:     "read",
		Urgency:    "low",
		Priority:   0,
	}

	result := rescoreBoard(kanban.client(), boardOf(card), idleGate(), now, false)

	if got := kanban.recorded(); len(got) != 0 {
		t.Fatalf("an unchanged card was written to: %+v", got)
	}
	if result.Scanned != 1 {
		t.Errorf("Scanned=%d, want 1: the card was read and scored", result.Scanned)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped=%d, want 0: nothing declined to score this card", result.Skipped)
	}
	if result.Repriced != 0 || result.Retagged != 0 {
		t.Errorf("result = %+v, want nothing counted as changed", result)
	}
}

// TestACardWithNoDeadlineIsRepricedButKeepsTheModelsUrgencyTag pins the
// asymmetry that makes this sweep safe to run over the cards written before
// due_at existed. Without a date there is nothing better to replace the tag
// with, and noteboard's revision snapshots do not cover due_at, so overwriting
// it would be unrecoverable. The priority still moves, because stakes alone are
// enough to derive that.
func TestACardWithNoDeadlineIsRepricedButKeepsTheModelsUrgencyTag(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	kanban := newFakeKanban(t)
	card := &cardState{
		ID:         "card-undated",
		Title:      "Verify your account",
		ColumnName: columnActionNeeded,
		Tags:       []string{"email", "cat:personal", "urgency:critical"},
		Category:   "personal",
		Action:     "verify",
		Urgency:    "critical",
		DueAt:      nil,
		Priority:   1,
	}

	result := rescoreBoard(kanban.client(), boardOf(card), idleGate(), now, false)

	writes := kanban.patchesFor("card-undated")
	if len(writes) != 1 {
		t.Fatalf("sent %d PATCH(es), want exactly 1 (priority only): %+v", len(writes), writes)
	}
	if _, wroteTags := writes[0].Fields["tags"]; wroteTags {
		t.Errorf("the sweep rewrote the urgency tag of a card with no deadline: %+v", writes[0].Fields)
	}
	if got := writes[0].Fields["priority"]; got != float64(5) {
		t.Errorf("wrote priority %v, want 5 derived from severe stakes and the stored urgency", got)
	}
	if result.Retagged != 0 {
		t.Errorf("Retagged=%d, want 0", result.Retagged)
	}
	if result.Repriced != 1 {
		t.Errorf("Repriced=%d, want 1", result.Repriced)
	}
	if card.Urgency != "critical" {
		t.Errorf("in-memory urgency became %q, want the model's own value kept", card.Urgency)
	}
}

// TestAFailedRetagAbandonsThatCardsRepriceAndNotTheBoard pins the recovery
// shape. One card failing to write must not abandon the rest of the board: the
// next tick would find the same wrong scores and the same failure, and nothing
// would ever recover. The failed card's OWN reprice is given up for this tick,
// which is why nothing is counted for it at all.
func TestAFailedRetagAbandonsThatCardsRepriceAndNotTheBoard(t *testing.T) {
	// rescoreBoard ranges over a map, so on any one run the failing card may
	// come last, and then a loop that stops at the first failure writes the
	// healthy card anyway. One run caught a `break` about 4 times in 5
	// (measured 2026-09-25). Repeating the scenario makes an order that hides
	// the defect on every attempt too unlikely to happen.
	for attempt := 0; attempt < 32; attempt++ {
		now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
		kanban := newFakeKanban(t)
		kanban.failField = "tags"

		failing := overdueRentCard("card-rent", now)
		// A second card that needs only a reprice, so it is written through a
		// field the fake is not failing and proves the loop carried on.
		healthy := &cardState{
			ID:         "card-undated",
			Title:      "Verify your account",
			ColumnName: columnActionNeeded,
			Tags:       []string{"email", "cat:personal", "urgency:critical"},
			Category:   "personal",
			Action:     "verify",
			Urgency:    "critical",
			Priority:   1,
		}

		result := rescoreBoard(kanban.client(), boardOf(failing, healthy), idleGate(), now, false)

		if got := kanban.patchesFor("card-rent"); len(got) != 0 {
			t.Errorf("attempt %d: the failing card recorded %d accepted write(s), want 0: %+v", attempt, len(got), got)
		}
		if got := kanban.patchesFor("card-undated"); len(got) != 1 {
			t.Fatalf("attempt %d: the healthy card got %d write(s), want 1: one card's failure must not abandon the board", attempt, len(got))
		}
		if result.Retagged != 0 {
			t.Errorf("attempt %d: Retagged=%d, want 0: a write that failed is not a retag", attempt, result.Retagged)
		}
		if result.Repriced != 1 {
			t.Errorf("attempt %d: Repriced=%d, want 1 — the healthy card only. The failing card's reprice is given up with its retag", attempt, result.Repriced)
		}
		if result.Scanned != 2 {
			t.Errorf("attempt %d: Scanned=%d, want 2", attempt, result.Scanned)
		}
		if failing.Priority != 2 || failing.Urgency != "normal" {
			t.Errorf("attempt %d: the failing card was advanced in memory to priority=%d urgency=%q despite writing nothing",
				attempt, failing.Priority, failing.Urgency)
		}
	}
}

// TestAFailedRepriceLeavesTheRetagThatAlreadyLanded pins the other half of the
// failure path. The two fields are written separately, so a reprice that fails
// after a retag succeeded must leave the retag counted — the tag really is on
// the board, and reporting otherwise would send the next reader looking for a
// write that did happen.
func TestAFailedRepriceLeavesTheRetagThatAlreadyLanded(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	kanban := newFakeKanban(t)
	kanban.failField = "priority"
	card := overdueRentCard("card-rent", now)

	result := rescoreBoard(kanban.client(), boardOf(card), idleGate(), now, false)

	writes := kanban.patchesFor("card-rent")
	if len(writes) != 1 {
		t.Fatalf("recorded %d accepted write(s), want 1 (the tags PATCH): %+v", len(writes), writes)
	}
	if _, ok := writes[0].Fields["tags"]; !ok {
		t.Errorf("the accepted write was %+v, want the tags PATCH", writes[0].Fields)
	}
	if result.Retagged != 1 {
		t.Errorf("Retagged=%d, want 1: that write landed", result.Retagged)
	}
	if result.Repriced != 0 {
		t.Errorf("Repriced=%d, want 0: that write failed", result.Repriced)
	}
	if card.Urgency != "expired" {
		t.Errorf("in-memory urgency %q, want expired to match what the board now holds", card.Urgency)
	}
	if card.Priority != 2 {
		t.Errorf("in-memory priority %d, want the old value kept — the write did not land", card.Priority)
	}
}

// TestALapsedConcertIsWrittenDOWNAndNotOnlyUp pins the direction the sweep was
// written for. The bug that produced it was a concert "tonight" still reading
// critical two days later: its deadline passing must SINK it, not raise it. A
// sweep that only ever wrote scores upward would fix nothing here and no other
// test in this file would notice, because every other card it uses escalates.
func TestALapsedConcertIsWrittenDOWNAndNotOnlyUp(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	kanban := newFakeKanban(t)
	due := now.Add(-time.Hour)
	card := &cardState{
		ID:         "card-concert",
		Title:      "Blue Öyster Cult free concert at GSR tonight",
		ColumnName: columnActionNeeded,
		Tags:       []string{"email", "cat:social-events", "urgency:critical"},
		Category:   "social-events",
		Action:     "schedule",
		Urgency:    "critical",
		DueAt:      &due,
		Priority:   4,
	}

	result := rescoreBoard(kanban.client(), boardOf(card), idleGate(), now, false)

	writes := kanban.patchesFor("card-concert")
	if len(writes) != 2 {
		t.Fatalf("sent %d PATCH(es), want 2: %+v", len(writes), writes)
	}
	if got := writes[1].Fields["priority"]; got != float64(1) {
		t.Errorf("wrote priority %v, want 1: a lapsed concert is over, and sinking it is the correct answer", got)
	}
	if result.Repriced != 1 {
		t.Errorf("Repriced=%d, want 1", result.Repriced)
	}
	if card.Priority != 1 {
		t.Errorf("in-memory priority %d, want 1", card.Priority)
	}
}

// TestTheSweepScansEveryCardWhateverItDoesWithThem pins Scanned as a count of
// the board rather than a count of the cards that produced work. It is the only
// number in the summary that answers "did the sweep see the whole board", so a
// version of it that counted only the interesting cards would make an excluded
// board and an empty one print the same line.
func TestTheSweepScansEveryCardWhateverItDoesWithThem(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	kanban := newFakeKanban(t)

	changed := overdueRentCard("card-rent", now)
	completed := overdueRentCard("card-settled", now)
	completed.ColumnName = columnActionCompleted
	unchanged := &cardState{
		ID:         "card-promo",
		ColumnName: columnNoAction,
		Tags:       []string{"urgency:low"},
		Category:   "promotions",
		Action:     "read",
		Urgency:    "low",
	}

	result := rescoreBoard(kanban.client(), boardOf(changed, completed, unchanged), idleGate(), now, false)

	if result.Scanned != 3 {
		t.Errorf("Scanned=%d, want 3 — every card on the board", result.Scanned)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped=%d, want 1 — the completed card only", result.Skipped)
	}
	if result.Repriced != 1 || result.Retagged != 1 {
		t.Errorf("result = %+v, want the one lapsed card repriced and retagged", result)
	}
}
