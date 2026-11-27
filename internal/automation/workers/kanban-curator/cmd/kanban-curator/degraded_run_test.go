package main

import (
	"strings"
	"testing"
	"time"
)

// These tests are about one question: can a tick of the curator tell the
// scheduler that it fell short? Before internal/degradations it could not — the
// loop's answer to every per-card failure is `continue`, the counters it prints
// are the only trace, and the process exited 0 either way, so scheduler run
// history recorded `success`. Measured on the live jobs 13, 17 and 18 on
// 2026-08-29: 300 of 300 retained runs `success`, `errors=0` on every one.
// Latent, not live — which is exactly when a defect is cheapest to close and
// when a test is the only thing that can hold it closed.
//
// Each test below drives run() against stub services and asserts on the exit
// code and the single `run degraded:` line, never on the whole log.

func testConfig(k *stubKanban, b *stubBridge) config {
	return config{
		issuesURL:  k.URL(),
		bridgeURL:  b.URL(),
		boardID:    stubBoardID,
		staleAfter: 2 * time.Hour,
	}
}

// ==================== the six shortfalls ====================

func TestACardWhoseLinksCannotBeReadIsAShortfall(t *testing.T) {
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "autoworker-x"})
	k.failLinks = 503
	b := newStubBridge(t)

	var code int
	out := captureLog(t, func() { code = run(testConfig(k, b)) })

	if code != 1 {
		t.Fatalf("exit code = %d, want 1: a card the curator could not examine is not a success", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "card-1") {
		t.Errorf("shortfall does not name the card: %s", line)
	}
	if !strings.Contains(line, "links could not be read") {
		t.Errorf("shortfall does not say what failed: %s", line)
	}
}

func TestASessionTheBridgeWillNotAnswerForIsAShortfall(t *testing.T) {
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "autoworker-x"})
	b := newStubBridge(t)
	b.fail = 500 // not 404: a vanished session is a reapable terminal state, not an error

	var code int
	out := captureLog(t, func() { code = run(testConfig(k, b)) })

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "autoworker-x") {
		t.Errorf("shortfall does not name the session: %s", line)
	}
}

func TestAReviewSignOffThatCannotBeLookedUpIsAShortfall(t *testing.T) {
	// A completed autoworker session inside the grace window. The curator asks
	// the board whether the reviewer signed off; the board refuses to answer, so
	// the card is deferred without the question ever being put.
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "autoworker-x"})
	k.failTags = 503
	b := newStubBridge(t)
	b.states["autoworker-x"] = "completed"
	b.purposes["autoworker-x"] = purposeAutoworker
	b.updatedAt["autoworker-x"] = minutesAgo(1)

	var code int
	out := captureLog(t, func() { code = run(testConfig(k, b)) })

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "review sign-off") {
		t.Errorf("shortfall does not say the sign-off went unasked: %s", line)
	}
	if k.moved["card-1"] != "" {
		t.Errorf("the card was closed anyway, to %s", k.moved["card-1"])
	}
}

func TestACardClosedWithNoNoteSayingWhyIsAShortfall(t *testing.T) {
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "session-x"})
	k.failPatch = 500
	b := newStubBridge(t)
	b.states["session-x"] = "error"

	var code int
	out := captureLog(t, func() { code = run(testConfig(k, b)) })

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "card-1") {
		t.Errorf("shortfall does not name the card: %s", line)
	}
	// Moving on is the right local choice; the card belongs in Failed either way.
	if k.moved["card-1"] != stubColFailed {
		t.Errorf("the card was not moved despite the note failing: moved to %q", k.moved["card-1"])
	}
}

func TestAMoveTheBoardRefusedIsAShortfall(t *testing.T) {
	// The sharp one. By this line the curator has decided the card is finished
	// and the move is the only thing that takes it out of In Progress.
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "session-x"})
	k.failMove = 404
	b := newStubBridge(t)
	b.states["session-x"] = "completed"

	var code int
	out := captureLog(t, func() { code = run(testConfig(k, b)) })

	if code != 1 {
		t.Fatalf("exit code = %d, want 1: a card judged done and left In Progress is not a success", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "card-1") {
		t.Errorf("shortfall does not name the card: %s", line)
	}
	if !strings.Contains(line, "still In Progress") {
		t.Errorf("shortfall does not say where the card was left: %s", line)
	}
}

func TestAPlacementWithNoNoteboardItemIsAShortfall(t *testing.T) {
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", noItem: true})
	b := newStubBridge(t)

	var code int
	out := captureLog(t, func() { code = run(testConfig(k, b)) })

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "card-1") {
		t.Errorf("shortfall does not name the card: %s", line)
	}
}

func TestEveryShortfallIsNamedNotJustTheFirst(t *testing.T) {
	// Report joins the reasons; a run that fell short over three cards must say
	// so about all three, because a person reading run history acts on the list.
	k := newStubKanban(t, "Agent runs",
		stubCard{cardID: "card-1", title: "one", sessionID: "s1"},
		stubCard{cardID: "card-2", title: "two", sessionID: "s2"},
		stubCard{cardID: "card-3", title: "three", sessionID: "s3"},
	)
	k.failMove = 500
	b := newStubBridge(t)
	b.states["s1"] = "completed"
	b.states["s2"] = "error"
	b.states["s3"] = "aborted"

	var code int
	out := captureLog(t, func() { code = run(testConfig(k, b)) })

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	line := shortfallLine(t, out)
	for _, id := range []string{"card-1", "card-2", "card-3"} {
		if !strings.Contains(line, id) {
			t.Errorf("shortfall report does not name %s: %s", id, line)
		}
	}
	if !strings.Contains(line, "3 shortfall(s)") {
		t.Errorf("shortfall report does not count them: %s", line)
	}
}

// ==================== the other direction: a deliberate skip is not a shortfall ====================
//
// A job that goes red every tick carries as little as the green it replaced.
// Each test below is a tick the curator meant to take no action on.

func TestAnEmptyBoardIsNotAShortfall(t *testing.T) {
	// This is jobs 17 and 18 on 100 of their last 100 ticks: in-progress=0,
	// moved=0, skipped=0, errors=0.
	k := newStubKanban(t, "Autoworker queue")
	b := newStubBridge(t)

	var code int
	out := captureLog(t, func() { code = run(testConfig(k, b)) })

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 for a board with nothing on it", code)
	}
	requireNoShortfall(t, out)
}

func TestACardWithNoSessionLinkIsNotAShortfall(t *testing.T) {
	// Live on 2026-08-29: card f1107651-67dd-4be5-b03a-a9c964f5f4ce has sat In
	// Progress on "Agent runs" since 2026-07-13 with no session link at all, and
	// is the whole of job 13's standing `skipped=1`. The curator is
	// session-driven, so it has nothing to close this against and never will —
	// recording it would paint the job red every five minutes forever.
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "autoworker · anthropic · Jul 13 11:47"})
	b := newStubBridge(t)

	var code int
	out := captureLog(t, func() { code = run(testConfig(k, b)) })

	if code != 0 {
		t.Fatalf("exit code = %d, want 0: a card with no session is not the curator falling short", code)
	}
	requireNoShortfall(t, out)
}

func TestACardWithNoSessionLinkIsStillNamedInTheLog(t *testing.T) {
	// Not a shortfall is not the same as invisible. This skip was silent for the
	// ~13,000 ticks the card above sat there: `skipped=1` and not one line saying
	// which card or why.
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "autoworker · anthropic · Jul 13 11:47"})
	b := newStubBridge(t)

	out := captureLog(t, func() { run(testConfig(k, b)) })

	if !strings.Contains(out, "no session link") {
		t.Errorf("the skip is still silent:\n%s", out)
	}
	if !strings.Contains(out, "card-1") {
		t.Errorf("the log does not say WHICH card was skipped:\n%s", out)
	}
}

func TestACardAwaitingReviewerSignOffIsNotAShortfall(t *testing.T) {
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "autoworker-x"})
	b := newStubBridge(t)
	b.states["autoworker-x"] = "completed"
	b.purposes["autoworker-x"] = purposeAutoworker
	b.updatedAt["autoworker-x"] = minutesAgo(1)

	var code int
	out := captureLog(t, func() { code = run(testConfig(k, b)) })

	if code != 0 {
		t.Fatalf("exit code = %d, want 0: waiting for the reviewer is the handshake working", code)
	}
	requireNoShortfall(t, out)
	if k.moved["card-1"] != "" {
		t.Errorf("the card was closed before sign-off, to %s", k.moved["card-1"])
	}
}

func TestARunningSessionIsNotAShortfall(t *testing.T) {
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "session-x"})
	b := newStubBridge(t)
	b.states["session-x"] = "running"

	var code int
	out := captureLog(t, func() { code = run(testConfig(k, b)) })

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	requireNoShortfall(t, out)
}

func TestACleanTickThatClosesCardsIsNotAShortfall(t *testing.T) {
	k := newStubKanban(t, "Agent runs",
		stubCard{cardID: "card-done", title: "finished", body: "original", sessionID: "s-done"},
		stubCard{cardID: "card-failed", title: "broke", body: "original", sessionID: "s-failed"},
	)
	k.settled["s-done"] = true
	b := newStubBridge(t)
	b.states["s-done"] = "completed"
	b.states["s-failed"] = "error"

	var code int
	out := captureLog(t, func() { code = run(testConfig(k, b)) })

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 for a tick that did exactly its job:\n%s", code, out)
	}
	requireNoShortfall(t, out)
	if k.moved["card-done"] != stubColDone {
		t.Errorf("completed card went to %q, want Done", k.moved["card-done"])
	}
	if k.moved["card-failed"] != stubColFailed {
		t.Errorf("errored card went to %q, want Failed", k.moved["card-failed"])
	}
	if !strings.HasPrefix(k.patched["card-done"], "original") {
		t.Errorf("the closing note replaced the body instead of appending to it: %q", k.patched["card-done"])
	}
}

func TestADryRunWritesNothingAndIsNotAShortfall(t *testing.T) {
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "s1"})
	b := newStubBridge(t)
	b.states["s1"] = "error"

	cfg := testConfig(k, b)
	cfg.dryRun = true
	var code int
	out := captureLog(t, func() { code = run(cfg) })

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	requireNoShortfall(t, out)
	if len(k.moved) != 0 || len(k.patched) != 0 {
		t.Errorf("a dry run wrote: moved=%v patched=%v", k.moved, k.patched)
	}
}

func TestAVanishedBridgeSessionIsReapedAndIsNotAShortfall(t *testing.T) {
	// 404 is a terminal state the curator acts on, not an error it fell over.
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "s-gone"})
	b := newStubBridge(t) // no state registered -> 404

	var code int
	out := captureLog(t, func() { code = run(testConfig(k, b)) })

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	requireNoShortfall(t, out)
	if k.moved["card-1"] != stubColFailed {
		t.Errorf("a vanished session was not reaped: moved to %q", k.moved["card-1"])
	}
}

func TestTheGraceWindowStillClosesAnUnsettledCardWithoutAsking(t *testing.T) {
	// The fail-safe: past reviewGrace the card closes whether or not the reviewer
	// ever signed off, so a down reviewer cannot wedge it. Because the grace check
	// short-circuits the lookup, the tag store is never asked — and a tick that
	// did not need to ask has not fallen short.
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "autoworker-x"})
	k.failTags = 503 // would be a shortfall if it were reached
	b := newStubBridge(t)
	b.states["autoworker-x"] = "completed"
	b.purposes["autoworker-x"] = purposeAutoworker
	b.updatedAt["autoworker-x"] = minutesAgo(int(reviewGrace/time.Minute) + 5)

	var code int
	out := captureLog(t, func() { code = run(testConfig(k, b)) })

	if code != 0 {
		t.Fatalf("exit code = %d, want 0:\n%s", code, out)
	}
	requireNoShortfall(t, out)
	if k.moved["card-1"] != stubColDone {
		t.Errorf("the grace window did not close the card: moved to %q", k.moved["card-1"])
	}
}

// ==================== the values, spelled out ====================
//
// The tests above read reviewGrace by name, which is how a mechanism test should
// be written — but a suite in which every test reads the constant cannot tell
// 30 minutes from 3 hours. So one test spells the number absolutely, per the
// convention that made this fleet's value rows clean.

func TestTheReviewGraceWindowIsThirtyMinutesAndNotSomeOtherLength(t *testing.T) {
	if reviewGrace != 30*time.Minute {
		t.Fatalf("reviewGrace = %s, want 30m: it is how long a completed autoworker card waits on a reviewer that may be down", reviewGrace)
	}
}
