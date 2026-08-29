package main

import (
	"strings"
	"testing"
)

// These tests are about one question: does a tick that failed to review what it
// meant to review say so, and does a tick that had nothing to review stay quiet?
//
// Both halves are load-bearing. The first is why the recorder exists. The second
// is why it is worth anything: on the live "Agent runs" board, 231 of the 234 In
// Progress cards are skipped by the origin gate on every tick, so a reviewer
// that called any of those a shortfall would be red every five minutes forever
// and no reader would look at it twice.
//
// Every assertion reads the single `run degraded:` line rather than the whole
// captured log — see shortfallLine.

const (
	awSession    = "autoworker-anthropic-1788000000000000000"
	classifierID = "br_1785384006993227294"
)

func newRun(k *stubKanban, b *stubBridge) config {
	return config{kanbanURL: k.URL(), bridgeURL: b.URL(), boardName: defaultBoardName}
}

// awCard is a card the reviewer is supposed to act on: autoworker-minted session,
// completed, no passes yet.
func awCard(k *stubKanban, b *stubBridge) {
	k.cards = []stubCard{{cardID: "card-1", title: "fix the thing", sessionID: awSession}}
	b.states[awSession] = "completed"
	b.purposes[awSession] = purposeAutoworker
}

// ==================== the shortfalls ====================

func TestACardWhoseNoteboardItemDoesNotResolveIsAShortfall(t *testing.T) {
	k := newStubKanban(t, stubCard{cardID: "card-1", noItem: true})
	b := newStubBridge(t)
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "card-1") {
		t.Errorf("the report does not name the card:\n%s", line)
	}
}

func TestACardWhoseLinksCannotBeReadIsAShortfall(t *testing.T) {
	k := newStubKanban(t, stubCard{cardID: "card-1", title: "fix the thing", sessionID: awSession})
	k.failLinks = 503
	b := newStubBridge(t)
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "card-1") {
		t.Errorf("the report does not name the card:\n%s", line)
	}
}

func TestASessionTheBridgeWillNotAnswerForIsAShortfall(t *testing.T) {
	k := newStubKanban(t)
	b := newStubBridge(t)
	awCard(k, b)
	b.failRead = 502
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, awSession) {
		t.Errorf("the report does not name the session:\n%s", line)
	}
}

// A vanished session is a 404 from the bridge, which getSession turns into
// an error like any other. It is still a card this tick failed to review.
func TestAVanishedSessionIsAShortfall(t *testing.T) {
	k := newStubKanban(t, stubCard{cardID: "card-1", title: "fix the thing", sessionID: awSession})
	b := newStubBridge(t) // no state registered -> 404
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(shortfallLine(t, out), "card-1") {
		t.Errorf("the report does not name the card:\n%s", out)
	}
}

func TestPassesThatCannotBeLookedUpAreAShortfall(t *testing.T) {
	k := newStubKanban(t)
	b := newStubBridge(t)
	awCard(k, b)
	k.failTags = 500
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(shortfallLine(t, out), awSession) {
		t.Errorf("the report does not name the session:\n%s", out)
	}
}

// The loop guard fired, the settle tag was refused, and the curator is waiting
// on a tag that will never arrive. The card is wedged and the run used to be
// green.
func TestASettleRefusedAtTheLoopGuardIsAShortfall(t *testing.T) {
	k := newStubKanban(t, stubCard{
		cardID: "card-1", title: "fix the thing", sessionID: awSession,
		tags: []string{"review-pass-1", "review-pass-2"},
	})
	b := newStubBridge(t)
	b.states[awSession] = "completed"
	b.purposes[awSession] = purposeAutoworker
	k.failAddTag = 503
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "card-1") || !strings.Contains(line, awSession) {
		t.Errorf("the report names neither the card nor the session:\n%s", line)
	}
}

// The nudge is the whole job. Settling instead keeps the card moving, which is
// the right local choice, and the review still did not happen.
func TestANudgeTheBridgeWillNotDeliverIsAShortfall(t *testing.T) {
	k := newStubKanban(t)
	b := newStubBridge(t)
	awCard(k, b)
	b.failSend = 500
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "card-1") {
		t.Errorf("the report does not name the card:\n%s", line)
	}
	// The pass tag lands before the send, so the settle is the second write.
	if added := k.added[awSession]; len(added) != 2 || added[0] != "review-pass-1" || added[1] != tagSettled {
		t.Errorf("a failed nudge must still settle so the curator can close; added %v", added)
	}
}

// Both halves failed: the nudge did not go and the settle that covers for it was
// refused too. Nothing is left to take the card out of In Progress. The pass tag
// lands first (main writes it before sending), so only the settle write fails.
func TestANudgeFailureWhoseSettleAlsoFailsIsTwoShortfalls(t *testing.T) {
	k := newStubKanban(t)
	b := newStubBridge(t)
	awCard(k, b)
	b.failSend = 500
	k.failSettleTag = 503
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "still In Progress") {
		t.Errorf("the wedged card is not reported:\n%s", line)
	}
	if !strings.Contains(line, "2 shortfall(s)") {
		t.Errorf("want both the undelivered nudge and the refused settle reported, got:\n%s", line)
	}
}

// The sharp one. The pass tag is the only record a nudge happened, and
// passCount reads it back next tick, so a nudge sent without it would go out
// again in five minutes, past maxPasses, without end. The tag is written first
// and the nudge withheld when it is refused — which keeps the loop guard intact
// and still means this tick reviewed nothing. The settle that follows goes
// through the same refused write, so the card is wedged as well.
func TestANudgeWhosePassTagIsRefusedIsAShortfall(t *testing.T) {
	k := newStubKanban(t)
	b := newStubBridge(t)
	awCard(k, b)
	k.failAddTag = 503
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if b.sent[awSession] != 0 {
		t.Fatalf("a nudge went out with its pass tag refused; sent=%d", b.sent[awSession])
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, awSession) {
		t.Errorf("the report does not name the session:\n%s", line)
	}
	if !strings.Contains(line, "loop guard") {
		t.Errorf("the report does not say why the nudge was withheld:\n%s", line)
	}
	if !strings.Contains(line, "still In Progress") {
		t.Errorf("the refused settle that wedges the card is not reported:\n%s", line)
	}
}

func TestANudgeStampedOnNoBodyIsAShortfall(t *testing.T) {
	k := newStubKanban(t)
	b := newStubBridge(t)
	awCard(k, b)
	k.failPatch = 500
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if b.sent[awSession] != 1 {
		t.Fatalf("the nudge should still have been delivered; sent=%d", b.sent[awSession])
	}
	if !strings.Contains(shortfallLine(t, out), "card-1") {
		t.Errorf("the report does not name the card:\n%s", out)
	}
}

func TestASettleStampedOnNoBodyIsAShortfall(t *testing.T) {
	k := newStubKanban(t, stubCard{
		cardID: "card-1", title: "fix the thing", sessionID: awSession,
		tags: []string{"review-pass-1", "review-pass-2"},
	})
	b := newStubBridge(t)
	b.states[awSession] = "completed"
	b.purposes[awSession] = purposeAutoworker
	k.failPatch = 500
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if len(k.added[awSession]) != 1 || k.added[awSession][0] != tagSettled {
		t.Fatalf("the settle tag should still have landed; added %v", k.added[awSession])
	}
	if !strings.Contains(shortfallLine(t, out), "card-1") {
		t.Errorf("the report does not name the card:\n%s", out)
	}
}

// ==================== the deliberate skips ====================
//
// Without these, every test above is satisfied by a recorder that records
// everything, which is the same as one that records nothing.

// The population case. 231 of the 234 live In Progress cards look exactly like
// this, on every tick.
func TestAClassifierMintedSessionIsNotAShortfall(t *testing.T) {
	k := newStubKanban(t, stubCard{cardID: "card-1", title: "some attended session", sessionID: classifierID})
	b := newStubBridge(t)
	b.states[classifierID] = "completed"
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	requireNoShortfall(t, out)
	if b.sent[classifierID] != 0 {
		t.Errorf("the origin gate let a classifier session through and nudged it")
	}
}

// Loud, not red: this reviewer is session-driven and no tick of it can ever move
// such a card, so recording it would paint the job red every five minutes for
// f1107651-67dd-4be5-b03a-a9c964f5f4ce and nothing else.
func TestACardWithNoSessionLinkIsLoudAndNotAShortfall(t *testing.T) {
	k := newStubKanban(t, stubCard{cardID: "card-1", title: "orphaned card"})
	b := newStubBridge(t)
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	requireNoShortfall(t, out)
	if !strings.Contains(out, "no session link") || !strings.Contains(out, "card-1") {
		t.Errorf("the skipped card is not named in the log:\n%s", out)
	}
}

func TestAStillRunningSessionIsNotAShortfall(t *testing.T) {
	k := newStubKanban(t, stubCard{cardID: "card-1", title: "mid-flight", sessionID: awSession})
	b := newStubBridge(t)
	b.states[awSession] = "tool_running"
	b.purposes[awSession] = purposeAutoworker
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	requireNoShortfall(t, out)
}

func TestAnAlreadySettledSessionIsNotAShortfall(t *testing.T) {
	k := newStubKanban(t, stubCard{
		cardID: "card-1", title: "awaiting close", sessionID: awSession,
		tags: []string{tagSettled},
	})
	b := newStubBridge(t)
	b.states[awSession] = "completed"
	b.purposes[awSession] = purposeAutoworker
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	requireNoShortfall(t, out)
}

func TestAnEmptyBoardIsNotAShortfall(t *testing.T) {
	k := newStubKanban(t)
	b := newStubBridge(t)
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	requireNoShortfall(t, out)
}

func TestABoardThatDoesNotExistYetIsNotAShortfall(t *testing.T) {
	k := newStubKanban(t)
	k.noBoard = true
	b := newStubBridge(t)
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	requireNoShortfall(t, out)
}

func TestADryRunIsNotAShortfall(t *testing.T) {
	k := newStubKanban(t)
	b := newStubBridge(t)
	awCard(k, b)
	cfg := newRun(k, b)
	cfg.dryRun = true
	var code int
	out := captureLog(t, func() { code = run(cfg) })
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	requireNoShortfall(t, out)
	if b.sent[awSession] != 0 {
		t.Errorf("a dry run sent a nudge")
	}
}

// The one that keeps the two above from being satisfied by a recorder that never
// records: a tick that did its whole job, end to end, and stayed green.
func TestAFullTickThatDidItsJobIsGreen(t *testing.T) {
	k := newStubKanban(t)
	b := newStubBridge(t)
	awCard(k, b)
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	requireNoShortfall(t, out)
	if b.sent[awSession] != 1 {
		t.Errorf("the nudge was not delivered; sent=%d", b.sent[awSession])
	}
	if len(k.added[awSession]) != 1 || k.added[awSession][0] != "review-pass-1" {
		t.Errorf("the pass tag was not recorded; added %v", k.added[awSession])
	}
	if !strings.Contains(k.patched["card-1"], "self-review nudge 1/2 sent") {
		t.Errorf("the card body was not stamped; got %q", k.patched["card-1"])
	}
}

// The other end-to-end green: the loop guard fires, the session settles, and the
// curator is unblocked.
func TestALoopGuardSettleThatLandedIsGreen(t *testing.T) {
	k := newStubKanban(t, stubCard{
		cardID: "card-1", title: "ping-ponger", sessionID: awSession,
		tags: []string{"review-pass-1", "review-pass-2"},
	})
	b := newStubBridge(t)
	b.states[awSession] = "completed"
	b.purposes[awSession] = purposeAutoworker
	var code int
	out := captureLog(t, func() { code = run(newRun(k, b)) })
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	requireNoShortfall(t, out)
	if b.sent[awSession] != 0 {
		t.Errorf("the loop guard nudged instead of settling")
	}
	if len(k.added[awSession]) != 1 || k.added[awSession][0] != tagSettled {
		t.Errorf("the settle tag was not added; added %v", k.added[awSession])
	}
}
