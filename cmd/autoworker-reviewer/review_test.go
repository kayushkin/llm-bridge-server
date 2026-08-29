package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/slava/scheduler/internal/kanbanclient"
)

// ============================ the fake store ============================

// fakeStore is a kanban-store whose reads and writes fail independently. That
// split is the whole point: the defect these tests pin needs a store that
// ANSWERS ListEntityTags and REFUSES AddEntityTag, which is what a partly-sick
// service does and what a fake that is simply "up" or "down" cannot express.
type fakeStore struct {
	tags []kanbanclient.EntityTag

	readErr error
	// writeErrFor returns the error to fail a tag write with, or nil to let it
	// through. Taking the tag lets a test fail review-pass-N writes while
	// letting review-settled land — the exact shape of the remedy this fix
	// rejects.
	writeErrFor func(tag string) error

	reads  int
	writes int
}

func (f *fakeStore) ListEntityTags(entityType, entityRef string) ([]kanbanclient.EntityTag, error) {
	f.reads++
	if f.readErr != nil {
		return nil, f.readErr
	}
	out := make([]kanbanclient.EntityTag, len(f.tags))
	copy(out, f.tags)
	return out, nil
}

func (f *fakeStore) AddEntityTag(entityType, entityRef, tag string) error {
	f.writes++
	if f.writeErrFor != nil {
		if err := f.writeErrFor(tag); err != nil {
			return err
		}
	}
	f.tags = append(f.tags, kanbanclient.EntityTag{
		EntityType: entityType, EntityRef: entityRef, Tag: tag,
	})
	return nil
}

func (f *fakeStore) hasTag(want string) bool { return hasTag(f.tags, want) }

func failWrites(err error) func(string) error {
	return func(string) error { return err }
}

func failPassTagsOnly(err error) func(string) error {
	return func(tag string) error {
		if strings.HasPrefix(tag, tagPassPrefix) {
			return err
		}
		return nil
	}
}

func failSettleTagOnly(err error) func(string) error {
	return func(tag string) error {
		if tag == tagSettled {
			return err
		}
		return nil
	}
}

// ============================ the tick driver ============================

// tickResult is what a run of ticks did, from the point of view of the two
// things that cost money: agent respawns, and how the loop ended.
type tickResult struct {
	sends   int
	actions []reviewAction
}

// runTicks drives reviewCompletedSession the way the scheduler does — once
// every five minutes against the same session, reading the tag state the
// previous tick left behind. A single call cannot show an unbounded loop; only
// repetition against persistent state can.
func runTicks(store *fakeStore, n int, sendErr error, dryRun bool) tickResult {
	var r tickResult
	for i := 0; i < n; i++ {
		action := reviewCompletedSession(
			store, "autoworker-test-1", dryRun,
			func(string) error { r.sends++; return sendErr },
			func(string) {},
			func(string, ...any) {}, func(string, ...any) {},
		)
		r.actions = append(r.actions, action)
	}
	return r
}

// ============================ the regression pin ============================

// TestNudgesNeverExceedThePassBudget is the pin for the defect in card
// 1c43e61a: a review-pass-N tag write that failed was logged and ignored, the
// nudge counted as sent, and the next tick — unable to see a pass it could not
// read — sent nudge 1 again. Every five minutes. Each send respawns the agent
// with `claude --resume`, so the loop guard the file calls "the hard loop guard
// against almost-done agents ping-ponging forever and burning budget" was
// defeated by an error it logged and ignored.
//
// The invariant is stated over TWENTY ticks and every store-write failure mode,
// because the property at risk is not "the right thing happens once" but "the
// spend is bounded no matter what stays broken".
func TestNudgesNeverExceedThePassBudget(t *testing.T) {
	cases := []struct {
		name string
		// writeErrFor configures which tag writes fail.
		writeErrFor func(string) error
		sendErr     error
		wantSends   int
		why         string
	}{
		{
			name:      "healthy store nudges exactly the budget and then stops",
			wantSends: maxPasses,
			why:       "the ordinary path: two nudges, then the guard settles the session",
		},
		{
			name:        "every tag write fails: nothing is ever sent",
			writeErrFor: failWrites(fmt.Errorf("kanban-store: 503")),
			wantSends:   0,
			why: "an uncountable pass must not be spent. Before the fix this sent " +
				"one nudge per tick forever — 20 agent respawns across these ticks",
		},
		{
			name:        "only the pass tag fails, settle still lands",
			writeErrFor: failPassTagsOnly(fmt.Errorf("kanban-store: 503")),
			wantSends:   0,
			why: "the remedy the card proposed (settle on tag failure) reaches the " +
				"right end state here, but only because it never sent the nudge",
		},
		{
			name:      "send fails and settle lands: it stops after one attempt",
			sendErr:   fmt.Errorf("bridge: connection refused"),
			wantSends: 1,
			why: "settle succeeds on the first failed send, so tick 2 sees " +
				"review-settled and stops. Not maxPasses — I expected 2 here and " +
				"the test corrected me",
		},
		{
			name:        "send fails AND settle fails, but pass tags land: bounded by the budget",
			writeErrFor: failSettleTagOnly(fmt.Errorf("kanban-store: 503")),
			sendErr:     fmt.Errorf("bridge: connection refused"),
			wantSends:   maxPasses,
			why: "the case the ordering is really for. With settle unavailable the " +
				"old code advanced no counter and retried forever; now the pass tag " +
				"lands on the way past the failed send, so the budget bounds it",
		},
		{
			name:        "send fails AND settle fails: still bounded",
			writeErrFor: failWrites(fmt.Errorf("kanban-store: 503")),
			sendErr:     fmt.Errorf("bridge: connection refused"),
			wantSends:   0,
			why:         "both remedies unavailable and the spend is still zero",
		},
	}

	const ticks = 20
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{writeErrFor: tc.writeErrFor}
			got := runTicks(store, ticks, tc.sendErr, false)
			if got.sends != tc.wantSends {
				t.Errorf("over %d ticks: sent %d nudges, want %d\n  why: %s",
					ticks, got.sends, tc.wantSends, tc.why)
			}
			if got.sends > maxPasses {
				t.Errorf("THE LOOP GUARD IS DEFEATED: %d sends over %d ticks "+
					"exceeds maxPasses=%d; each send respawns an agent",
					got.sends, ticks, maxPasses)
			}
		})
	}
}

// TestAPassIsCountedBeforeItIsSpent states the ordering directly rather than
// through its consequence. Swapping the AddEntityTag call back below the send
// turns this red on its own.
func TestAPassIsCountedBeforeItIsSpent(t *testing.T) {
	var order []string
	store := &fakeStore{writeErrFor: func(tag string) error {
		order = append(order, "tag:"+tag)
		return nil
	}}
	action := reviewCompletedSession(
		store, "autoworker-test-1", false,
		func(string) error { order = append(order, "send"); return nil },
		func(string) {}, func(string, ...any) {}, func(string, ...any) {},
	)
	if action != actionNudged {
		t.Fatalf("action = %v, want actionNudged", action)
	}
	want := []string{"tag:" + tagPassPrefix + "1", "send"}
	if len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Errorf("call order = %v, want %v\n"+
			"  the tag must be durable BEFORE the nudge respawns an agent, or the "+
			"next tick cannot see the pass it paid for", order, want)
	}
}

// TestAFailedPassTagSendsNothing is the single-tick statement of the same
// thing, kept separate because it is the sentence a reader will look for.
func TestAFailedPassTagSendsNothing(t *testing.T) {
	store := &fakeStore{writeErrFor: failPassTagsOnly(fmt.Errorf("kanban-store: 503"))}
	sends := 0
	action := reviewCompletedSession(
		store, "autoworker-test-1", false,
		func(string) error { sends++; return nil },
		func(string) {}, func(string, ...any) {}, func(string, ...any) {},
	)
	if sends != 0 {
		t.Errorf("sent %d nudges after the pass tag failed, want 0", sends)
	}
	if action != actionError {
		t.Errorf("action = %v, want actionError", action)
	}
	if !store.hasTag(tagSettled) {
		t.Error("session was not settled after an unwritable pass tag — " +
			"the card is wedged, which this file's own contract forbids")
	}
}

// TestAnUnwritableStoreDoesNotWedgeSilently pins the other half of the
// contract. When even the settle write fails there is nothing this binary can
// do but report it: the card waits for the curator's idle grace window. What it
// must NOT do is send.
func TestAnUnwritableStoreDoesNotWedgeSilently(t *testing.T) {
	store := &fakeStore{writeErrFor: failWrites(fmt.Errorf("kanban-store: 503"))}
	sends := 0
	action := reviewCompletedSession(
		store, "autoworker-test-1", false,
		func(string) error { sends++; return nil },
		func(string) {}, func(string, ...any) {}, func(string, ...any) {},
	)
	if action != actionError {
		t.Errorf("action = %v, want actionError — an unwritable store must be "+
			"reported, not counted as a successful nudge", action)
	}
	if sends != 0 {
		t.Errorf("sent %d nudges with the store unwritable, want 0", sends)
	}
	if store.hasTag(tagSettled) {
		t.Error("store reported the settle tag present after refusing every write")
	}
}

// ============================ the surrounding decisions ============================

func TestSettledSessionIsLeftAlone(t *testing.T) {
	store := &fakeStore{tags: entityTags(tagSettled)}
	sends := 0
	action := reviewCompletedSession(
		store, "autoworker-test-1", false,
		func(string) error { sends++; return nil },
		func(string) {}, func(string, ...any) {}, func(string, ...any) {},
	)
	if action != actionAlreadySettled {
		t.Errorf("action = %v, want actionAlreadySettled", action)
	}
	if sends != 0 || store.writes != 0 {
		t.Errorf("sends=%d writes=%d, want 0 and 0 — a settled session is the "+
			"curator's now", sends, store.writes)
	}
}

func TestSpentBudgetSettlesInsteadOfNudging(t *testing.T) {
	store := &fakeStore{tags: entityTags(tagPassPrefix+"1", tagPassPrefix+"2")}
	sends := 0
	action := reviewCompletedSession(
		store, "autoworker-test-1", false,
		func(string) error { sends++; return nil },
		func(string) {}, func(string, ...any) {}, func(string, ...any) {},
	)
	if action != actionSettled {
		t.Errorf("action = %v, want actionSettled", action)
	}
	if sends != 0 {
		t.Errorf("sent %d nudges with the budget spent, want 0", sends)
	}
	if !store.hasTag(tagSettled) {
		t.Error("budget spent but the session was not settled — the curator can " +
			"never close this card")
	}
}

func TestAnUnreadableStoreSkipsWithoutSending(t *testing.T) {
	store := &fakeStore{readErr: fmt.Errorf("kanban-store: 503")}
	sends := 0
	action := reviewCompletedSession(
		store, "autoworker-test-1", false,
		func(string) error { sends++; return nil },
		func(string) {}, func(string, ...any) {}, func(string, ...any) {},
	)
	if action != actionError {
		t.Errorf("action = %v, want actionError", action)
	}
	if sends != 0 || store.writes != 0 {
		t.Errorf("sends=%d writes=%d, want 0 and 0 — with the tags unreadable the "+
			"pass count is unknown and nothing may be spent against it", sends, store.writes)
	}
}

// TestDryRunSpendsNothing pins that -dry-run reaches the decision and stops
// short of both the send and the tag write. The ordering fix moved a write onto
// the nudge path, so this is the assertion that keeps -dry-run honest.
func TestDryRunSpendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []kanbanclient.EntityTag
	}{
		{"would nudge", nil},
		{"would settle", entityTags(tagPassPrefix+"1", tagPassPrefix+"2")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{tags: tc.tags}
			got := runTicks(store, 5, nil, true)
			if got.sends != 0 {
				t.Errorf("-dry-run sent %d nudges, want 0", got.sends)
			}
			if store.writes != 0 {
				t.Errorf("-dry-run wrote %d tags, want 0", store.writes)
			}
			for _, a := range got.actions {
				if a != actionDryRun {
					t.Errorf("action = %v, want actionDryRun", a)
				}
			}
		})
	}
}

// TestNudgeTextCarriesThePassItWasCountedAs guards the pairing between the tag
// and the prompt. They are written from the same `pass` variable; if a later
// edit splits them, the agent is told "pass 1 of 2" on the tick that stored
// review-pass-2 and the budget reads as larger than it is.
func TestNudgeTextCarriesThePassItWasCountedAs(t *testing.T) {
	store := &fakeStore{}
	var sent []string
	for i := 0; i < maxPasses; i++ {
		reviewCompletedSession(
			store, "autoworker-test-1", false,
			func(m string) error { sent = append(sent, m); return nil },
			func(string) {}, func(string, ...any) {}, func(string, ...any) {},
		)
	}
	if len(sent) != maxPasses {
		t.Fatalf("sent %d nudges, want %d", len(sent), maxPasses)
	}
	for i, m := range sent {
		pass := i + 1
		if !strings.Contains(m, fmt.Sprintf("pass %d of %d", pass, maxPasses)) {
			t.Errorf("nudge %d does not announce itself as pass %d of %d", pass, pass, maxPasses)
		}
		if !store.hasTag(fmt.Sprintf("%s%d", tagPassPrefix, pass)) {
			t.Errorf("nudge %d was sent but %s%d is not stored", pass, tagPassPrefix, pass)
		}
	}
}
