package main

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"
)

// classifyTick is the per-tick spend ceiling. The other three gates are
// per-session — each one failing costs one extra model call for one session.
// This one bounds a whole tick, and selectCandidates hands it every session
// updated in the last seven days, so losing it turns a ten-call tick into an
// every-session tick, ninety-six times a day.
//
// Nothing downstream re-checks it and nothing in the logs looks wrong when it
// stops firing: `classified=N` simply reads higher. So what these tests assert
// is how many times the classifier was REACHED, not what it wrote.

// tickRecorder counts the sessions classifyTick handed to the classifier, in
// order, and decides which of them report a classification.
//
// classifiedEvery is the interesting knob: processSession returns false when a
// per-session gate skipped the session, and a skip must not consume the tick's
// budget. Setting it to 2 means every other candidate is skipped, so a cap of N
// has to look at 2N candidates to spend N.
type tickRecorder struct {
	visited         []string
	classifiedEvery int // classify candidate i when i%classifiedEvery == 0
}

func (r *tickRecorder) classify(sess session) bool {
	r.visited = append(r.visited, sess.SessionID)
	return (len(r.visited)-1)%r.classifiedEvery == 0
}

// nCandidates returns n sessions with distinguishable ids, so a test can tell
// WHICH candidates were looked at and not merely how many.
func nCandidates(n int) []session {
	out := make([]session, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, session{SessionID: fmt.Sprintf("br_%d", i)})
	}
	return out
}

// The cap is written as a literal in every test below, never as
// maxClassifyPerTick. A fixture seeded with the constant under test moves with
// it: drop the default from 10 to 10,000 and a test that read the constant
// stays green while the tick's spend ceiling rises a thousandfold.

func TestATickStopsClassifyingOnceItHitsTheCap(t *testing.T) {
	r := &tickRecorder{classifiedEvery: 1}

	got := classifyTick(nCandidates(25), 10, r.classify)

	if got != 10 {
		t.Errorf("classifyTick returned %d, want 10", got)
	}
	if len(r.visited) != 10 {
		t.Fatalf("the classifier was reached %d time(s) for a cap of 10 — a tick that does not stop bills for every candidate",
			len(r.visited))
	}
}

// The two tests below straddle the boundary. Alone, either one is satisfied by
// a cap that is off by one in the direction it does not test: `>` instead of
// `>=` passes the exactly-at-the-cap case and admits an eleventh, and a cap
// that fires one early passes the over-the-cap case while under-spending.
func TestACandidateListExactlyTheSizeOfTheCapIsFullyClassified(t *testing.T) {
	r := &tickRecorder{classifiedEvery: 1}

	got := classifyTick(nCandidates(10), 10, r.classify)

	if got != 10 || len(r.visited) != 10 {
		t.Fatalf("10 candidates under a cap of 10: classified %d, reached the classifier %d time(s), want 10 and 10",
			got, len(r.visited))
	}
}

func TestTheCandidateOnePastTheCapIsNeverLookedAt(t *testing.T) {
	r := &tickRecorder{classifiedEvery: 1}

	classifyTick(nCandidates(11), 10, r.classify)

	if len(r.visited) != 10 {
		t.Fatalf("the classifier was reached %d time(s) for 11 candidates under a cap of 10, want 10", len(r.visited))
	}
	for _, id := range r.visited {
		if id == "br_10" {
			t.Fatal("the eleventh candidate was classified under a cap of 10")
		}
	}
}

// A candidate a gate skipped cost nothing, so it must not consume the tick's
// budget. If it does, a run of do-not-track sessions at the head of the list
// silently eats the whole tick and the sessions that actually changed are
// deferred — the cap would be rationing candidates looked at rather than money
// spent.
func TestASkippedCandidateDoesNotConsumeTheTicksBudget(t *testing.T) {
	r := &tickRecorder{classifiedEvery: 2} // every other candidate is gate-skipped

	got := classifyTick(nCandidates(40), 5, r.classify)

	if got != 5 {
		t.Errorf("classifyTick returned %d, want 5 — only a classification spends budget", got)
	}
	if len(r.visited) != 9 {
		t.Fatalf("the classifier was reached %d time(s), want 9 — five classifications interleaved with four skips",
			len(r.visited))
	}
}

// Every candidate fitting under the cap is the ordinary case, and it is the one
// that tells an over-eager cap from a working one: a cap that fires when it
// should not also skips a tick that had room to spare.
func TestATickUnderTheCapClassifiesEveryCandidate(t *testing.T) {
	r := &tickRecorder{classifiedEvery: 1}

	got := classifyTick(nCandidates(3), 10, r.classify)

	if got != 3 || len(r.visited) != 3 {
		t.Fatalf("3 candidates under a cap of 10: classified %d, reached the classifier %d time(s), want 3 and 3",
			got, len(r.visited))
	}
}

// selectCandidates sorts newest-updated first precisely so the tick's budget
// goes to the likely-active sessions. That sort buys nothing if the tick does
// not spend the budget from the front of the list.
func TestTheTickSpendsItsBudgetOnTheHeadOfTheCandidateList(t *testing.T) {
	r := &tickRecorder{classifiedEvery: 1}
	candidates := nCandidates(20)

	classifyTick(candidates, 4, r.classify)

	want := []string{candidates[0].SessionID, candidates[1].SessionID, candidates[2].SessionID, candidates[3].SessionID}
	if len(r.visited) != len(want) {
		t.Fatalf("reached the classifier %d time(s), want %d", len(r.visited), len(want))
	}
	for i := range want {
		if r.visited[i] != want[i] {
			t.Fatalf("classified %v, want the first four candidates %v", r.visited, want)
		}
	}
}

// An empty tick must not reach the classifier at all, and must not report a
// spend it did not make.
func TestATickWithNoCandidatesSpendsNothing(t *testing.T) {
	r := &tickRecorder{classifiedEvery: 1}

	if got := classifyTick(nil, 10, r.classify); got != 0 {
		t.Errorf("classifyTick returned %d for no candidates, want 0", got)
	}
	if len(r.visited) != 0 {
		t.Fatalf("the classifier was reached %d time(s) with no candidates", len(r.visited))
	}
}

// ------------------------------------------------- what the tick tells the operator

// The deferral line is the only signal that a backlog is building — nothing
// else reports it, and a tick that defers work looks exactly like one that had
// nothing to do. So the number has to name the candidates still waiting.
//
// The trap is that a gate-skipped candidate is not deferred. It was looked at
// and decided, cost nothing, and will be skipped again for nothing next tick.
// Counting the deferral from the spend rather than from the loop position
// inflates it by exactly the number of sessions the gates skipped, which is the
// commonest case on this box: most candidates in any tick are unchanged.
func TestTheDeferralCountsOnlyTheCandidatesNeverLookedAt(t *testing.T) {
	r := &tickRecorder{classifiedEvery: 2} // every other candidate is gate-skipped

	logged := captureLog(t, func() {
		classifyTick(nCandidates(20), 5, r.classify)
	})

	// Five classifications interleaved with four skips is nine candidates
	// looked at, so eleven are genuinely waiting. Counting from the cap instead
	// would report fifteen.
	if len(r.visited) != 9 {
		t.Fatalf("reached the classifier %d time(s), want 9 — the fixture no longer sets up the case", len(r.visited))
	}
	if !strings.Contains(logged, "deferring 11 sessions") {
		t.Fatalf("want a deferral of 11 (the candidates never looked at), got: %s", strings.TrimSpace(logged))
	}
}

// The mirror of the above: a tick with room to spare has deferred nothing, and
// must not announce a backlog that does not exist.
func TestATickThatNeverHitTheCapAnnouncesNoDeferral(t *testing.T) {
	r := &tickRecorder{classifiedEvery: 1}

	logged := captureLog(t, func() {
		classifyTick(nCandidates(3), 10, r.classify)
	})

	if strings.Contains(logged, "deferring") {
		t.Fatalf("a tick that classified every candidate reported a deferral: %s", strings.TrimSpace(logged))
	}
}

// captureLog collects what fn writes to the standard logger and restores the
// logger afterwards, whatever fn does — a test that leaves log output
// redirected silently swallows every later test's diagnostics.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}

// parseFlags refuses -max 0, so no configured run reaches this. It pins the
// function's own contract: a cap of zero must reach the paid classifier zero
// times, not once, whatever a later caller passes it.
func TestACapOfZeroReachesTheClassifierNotAtAll(t *testing.T) {
	r := &tickRecorder{classifiedEvery: 1}

	if got := classifyTick(nCandidates(5), 0, r.classify); got != 0 {
		t.Errorf("classifyTick returned %d under a cap of 0, want 0", got)
	}
	if len(r.visited) != 0 {
		t.Fatalf("a cap of 0 still reached the paid classifier %d time(s)", len(r.visited))
	}
}
