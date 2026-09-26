package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file is about one distinction: a tick that decided not to dispatch, and a
// tick that could not tell whether it should. Both end with no worker fired and
// both used to exit 0, so scheduler run history recorded `success` either way.
//
// Half the tests here exist to keep a deliberate skip GREEN. Without them a
// recorder that records everything passes every red test in the file and is
// worse than no recorder at all — a job red every tick is as uninformative as
// the green it replaces.

// ---------------------------------------------------------------- shortfalls

// TestAWindowMissingFromAFreshSnapshotIsAShortfall — the snapshot arrived and is
// not stale, so the provider is live; it just has no window under the key this
// binary reads. The tick cannot tell whether there was room to dispatch into.
func TestAWindowMissingFromAFreshSnapshotIsAShortfall(t *testing.T) {
	limits := map[string]any{
		"anthropic": map[string]any{
			"provider":    "anthropic",
			"snapshot_at": time.Now().Unix(),
			"windows":     map[string]any{"five_hour": map[string]any{"used_percent": 3.0}},
		},
	}
	usage := newStubUsage(t, limits)
	kanban := newStubKanban(t)
	spend := newStubSpend(t)
	cfg := tickConfig(t, usage.URL(), kanban.URL(), spend.URL())

	var code int
	out := captureLog(t, func() { code = run(cfg, (&recordingPublisher{}).publish) })

	if code != 1 {
		t.Errorf("a provider that was never evaluated exited %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "anthropic") || !strings.Contains(line, "seven_day") {
		t.Errorf("the report does not name the provider and the window it wanted: %s", line)
	}
	// The window's own absence is the cause, not a missing resets_at inside it.
	// Scoring an empty stand-in window falls through to the resets_at check, whose
	// report also names the provider and the window, so only this tells them apart.
	if strings.Contains(line, "resets_at") {
		t.Errorf("the report blames a missing resets_at on a snapshot with no window at all: %s", line)
	}
}

// TestAWindowWithNoResetsAtIsAShortfall — the elapsed fraction is computed from
// resets_at, so without it there is no target to compare against and the whole
// decision is uncomputable.
func TestAWindowWithNoResetsAtIsAShortfall(t *testing.T) {
	limits := map[string]any{
		"anthropic": map[string]any{
			"provider":    "anthropic",
			"snapshot_at": time.Now().Unix(),
			"windows":     map[string]any{"seven_day": map[string]any{"used_percent": 3.0}},
		},
	}
	usage := newStubUsage(t, limits)
	cfg := tickConfig(t, usage.URL(), newStubKanban(t).URL(), newStubSpend(t).URL())

	var code int
	out := captureLog(t, func() { code = run(cfg, (&recordingPublisher{}).publish) })

	if code != 1 {
		t.Errorf("a window with no resets_at exited %d, want 1", code)
	}
	if line := shortfallLine(t, out); !strings.Contains(line, "resets_at") {
		t.Errorf("the report does not say what was missing: %s", line)
	}
}

// TestAFailedPublishIsAShortfall — every gate opened and no worker exists. This
// is the one shortfall that loses the entire point of the tick.
func TestAFailedPublishIsAShortfall(t *testing.T) {
	usage := newStubUsage(t, firingLimits())
	cfg := tickConfig(t, usage.URL(), newStubKanban(t).URL(), newStubSpend(t).URL())

	failing := func(providerSpec, string, float64, string, string, string) (string, error) {
		return "", errors.New("bus refused the publish")
	}
	var code int
	out := captureLog(t, func() { code = run(cfg, failing) })

	if code != 1 {
		t.Errorf("a tick that fired nothing exited %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "anthropic") || !strings.Contains(line, "bus refused the publish") {
		t.Errorf("the report does not name the provider and the cause: %s", line)
	}
}

// TestADispatchedCardLeftInQueuedIsAShortfall — the sharp one. The session is
// already running against this card's prompt and the board still shows it as
// Queued, so the next tick can pick the same card up and dispatch a second
// worker onto the same work, paying twice.
func TestADispatchedCardLeftInQueuedIsAShortfall(t *testing.T) {
	usage := newStubUsage(t, firingLimits())
	kanban := newStubKanban(t, stubQueueCard{cardID: "card-1", title: "do the thing"})
	kanban.failMove = 503
	cfg := tickConfig(t, usage.URL(), kanban.URL(), newStubSpend(t).URL())

	pub := &recordingPublisher{}
	var code int
	out := captureLog(t, func() { code = run(cfg, pub.publish) })

	if pub.calls != 1 {
		t.Fatalf("the worker was dispatched %d times, want 1 — the fixture is not exercising the write path", pub.calls)
	}
	if code != 1 {
		t.Errorf("a card left in Queued after dispatch exited %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "card-1") {
		t.Errorf("the report does not name the card that was left behind: %s", line)
	}
	if !strings.Contains(line, autoworkerQueueColumnFrom) {
		t.Errorf("the report does not say where the card was left: %s", line)
	}
}

// TestASessionWithNoTelemetryCardIsAShortfall — the session exists and nothing
// on any board points at it, so neither kanban-curator nor autoworker-reviewer
// will ever see it: it runs, finishes, and is never signed off or closed.
func TestASessionWithNoTelemetryCardIsAShortfall(t *testing.T) {
	usage := newStubUsage(t, firingLimits())
	kanban := newStubKanban(t)
	kanban.failCreateCard = 500
	cfg := tickConfig(t, usage.URL(), kanban.URL(), newStubSpend(t).URL())

	pub := &recordingPublisher{}
	var code int
	out := captureLog(t, func() { code = run(cfg, pub.publish) })

	if pub.calls != 1 {
		t.Fatalf("the worker was dispatched %d times, want 1", pub.calls)
	}
	if code != 1 {
		t.Errorf("a session with no telemetry card exited %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "autoworker-anthropic-1") {
		t.Errorf("the report does not name the session nothing will see: %s", line)
	}
}

// TestAnUnreadableQueueIsAShortfall — the board could not be reached, so the
// tick dispatches the default discovery prompt instead of the work somebody
// queued. It still fires, which is why this is a shortfall and not a failure:
// budget is spent, on the wrong thing.
func TestAnUnreadableQueueIsAShortfall(t *testing.T) {
	usage := newStubUsage(t, firingLimits())
	kanban := newStubKanban(t)
	kanban.failQueueBoards = 503
	cfg := tickConfig(t, usage.URL(), kanban.URL(), newStubSpend(t).URL())

	pub := &recordingPublisher{}
	var code int
	out := captureLog(t, func() { code = run(cfg, pub.publish) })

	if pub.calls != 1 {
		t.Fatalf("the worker was dispatched %d times, want 1", pub.calls)
	}
	if got := pub.prompts[0]; got != cfg.prompt {
		t.Errorf("dispatched %q, want the default prompt — the fixture is not exercising the fallback", got)
	}
	if code != 1 {
		t.Errorf("a tick that dispatched the wrong prompt exited %d, want 1", code)
	}
	if line := shortfallLine(t, out); !strings.Contains(line, autoworkerQueueBoard) {
		t.Errorf("the report does not name the board it could not read: %s", line)
	}
}

// TestAnUnparkedOverCeilingCardIsAShortfall — the card is over its ceiling and
// the hold was refused, so it is skipped this tick but every later tick has to
// re-derive that and re-attempt the hold. The code's own log line says "it is
// not parked"; the exit code did not.
func TestAnUnparkedOverCeilingCardIsAShortfall(t *testing.T) {
	usage := newStubUsage(t, firingLimits())
	kanban := newStubKanban(t, stubQueueCard{cardID: "card-broke", title: "expensive work", ceiling: 1, sessions: []string{"s1"}})
	kanban.failHold = 503
	spend := newStubSpend(t)
	spend.items = []map[string]any{
		{"id": "card-broke", "title": "expensive work", "auto_hold_at_usd": 1.0},
	}
	spend.aggs = []map[string]any{
		{"session_id": "s1", "cost_usd": 99.0},
	}
	cfg := tickConfig(t, usage.URL(), kanban.URL(), spend.URL())

	var code int
	out := captureLog(t, func() { code = run(cfg, (&recordingPublisher{}).publish) })

	if !strings.Contains(out, "hold FAILED") {
		t.Fatalf("fixture did not reach the ceiling branch; log was:\n%s", out)
	}
	if code != 1 {
		t.Errorf("an over-ceiling card that was not parked exited %d, want 1", code)
	}
	if line := shortfallLine(t, out); !strings.Contains(line, "card-broke") {
		t.Errorf("the report does not name the unparked card: %s", line)
	}
}

// TestAQueueThatCannotBeReadIsAShortfall — the board resolved and the read of
// the Queued column itself failed, which is a different path from the board
// being unreachable: the client exists, so the tick gets as far as asking and
// then dispatches the default prompt anyway. Budget is spent, on the wrong
// thing.
func TestAQueueThatCannotBeReadIsAShortfall(t *testing.T) {
	usage := newStubUsage(t, firingLimits())
	kanban := newStubKanban(t, stubQueueCard{cardID: "card-1", title: "do the thing"})
	kanban.failBoardView = 503
	cfg := tickConfig(t, usage.URL(), kanban.URL(), newStubSpend(t).URL())

	pub := &recordingPublisher{}
	var code int
	out := captureLog(t, func() { code = run(cfg, pub.publish) })

	if pub.calls != 1 {
		t.Fatalf("the worker was dispatched %d times, want 1", pub.calls)
	}
	if got := pub.prompts[0]; got != cfg.prompt {
		t.Errorf("dispatched %q, want the default prompt — the fixture is not exercising the fallback", got)
	}
	if code != 1 {
		t.Errorf("a tick that could not read the queue exited %d, want 1", code)
	}
	line := shortfallLine(t, out)
	if !strings.Contains(line, "anthropic") || !strings.Contains(line, "default prompt") {
		t.Errorf("the report does not say which dispatch used the wrong prompt: %s", line)
	}
}

// TestAnUnsavedCooldownIsAShortfall — LastFire is how the cooldown is enforced
// across ticks. Dropped, the next tick five minutes from now sees no last fire
// and can dispatch again inside a cooldown that was supposed to be fifteen
// minutes, so this one costs money rather than only visibility.
func TestAnUnsavedCooldownIsAShortfall(t *testing.T) {
	usage := newStubUsage(t, firingLimits())
	cfg := tickConfig(t, usage.URL(), newStubKanban(t).URL(), newStubSpend(t).URL())

	// The directory exists, so loadState's MkdirAll succeeds and the absent file
	// reads as a fresh state; it is not writable, so the save at the end fails.
	// Restored in Cleanup or the temp dir cannot be removed.
	dir := filepath.Dir(cfg.statePath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	pub := &recordingPublisher{}
	var code int
	out := captureLog(t, func() { code = run(cfg, pub.publish) })

	if pub.calls != 1 {
		t.Fatalf("the worker was dispatched %d times, want 1 — the fixture never reached the save", pub.calls)
	}
	if code != 1 {
		t.Errorf("a tick that lost its cooldown exited %d, want 1", code)
	}
	if line := shortfallLine(t, out); !strings.Contains(line, "cooldown") {
		t.Errorf("the report does not say what was lost: %s", line)
	}
}

// ---------------------------------------------------------- deliberate skips

// TestAProviderWithinSlackOfTargetIsNotAShortfall — the commonest green tick
// there is. The binary looked, found no room to dispatch into, and said so.
// Without this test a recorder that records every non-fire passes everything
// above.
func TestAProviderWithinSlackOfTargetIsNotAShortfall(t *testing.T) {
	resets := time.Now().Add(24 * time.Hour).Unix()
	limits := map[string]any{
		"anthropic": map[string]any{
			"provider":    "anthropic",
			"snapshot_at": time.Now().Unix(),
			"windows": map[string]any{
				"seven_day": map[string]any{"used_percent": 99.0, "resets_at": resets},
			},
		},
	}
	usage := newStubUsage(t, limits)
	cfg := tickConfig(t, usage.URL(), newStubKanban(t).URL(), newStubSpend(t).URL())

	pub := &recordingPublisher{}
	var code int
	out := captureLog(t, func() { code = run(cfg, pub.publish) })

	if pub.calls != 0 {
		t.Errorf("a provider already at target dispatched %d workers", pub.calls)
	}
	if code != 0 {
		t.Errorf("a provider already at target exited %d, want 0", code)
	}
	requireNoShortfall(t, out)
}

// TestAStaleSnapshotIsNotAShortfall — the package comment names a stale snapshot
// as the expected state for a provider nobody has used lately, and codex reaches
// it whenever nobody has used codex. Recording it would paint this job red every
// five minutes for as long as that is true, which on 2026-08-29 was five days.
func TestAStaleSnapshotIsNotAShortfall(t *testing.T) {
	staleAt := time.Now().Add(-5 * 24 * time.Hour).Unix()
	limits := map[string]any{
		"codex": map[string]any{
			"provider":    "codex",
			"snapshot_at": staleAt,
			"stale_after": staleAt + 7200,
			"windows":     map[string]any{"weekly": map[string]any{"used_percent": 12.0}},
		},
	}
	usage := newStubUsage(t, limits)
	cfg := tickConfig(t, usage.URL(), newStubKanban(t).URL(), newStubSpend(t).URL())

	var code int
	out := captureLog(t, func() { code = run(cfg, (&recordingPublisher{}).publish) })

	if code != 0 {
		t.Errorf("a stale snapshot exited %d, want 0 — it is the documented expected state", code)
	}
	requireNoShortfall(t, out)
}

// TestAStaleSnapshotSaysHowStaleItIs — the other half of the decision above. A
// skip that is not recorded has to be readable instead, and the line used to
// print the snapshot timestamp alone, so two hours stale and five days stale
// read identically. Five days means this binary has not evaluated that provider
// at all since.
func TestAStaleSnapshotSaysHowStaleItIs(t *testing.T) {
	staleAt := time.Now().Add(-50 * time.Hour).Unix()
	limits := map[string]any{
		"codex": map[string]any{
			"provider":    "codex",
			"snapshot_at": staleAt,
			"stale_after": staleAt + 7200,
			"windows":     map[string]any{"weekly": map[string]any{"used_percent": 12.0}},
		},
	}
	usage := newStubUsage(t, limits)
	cfg := tickConfig(t, usage.URL(), newStubKanban(t).URL(), newStubSpend(t).URL())

	out := captureLog(t, func() { run(cfg, (&recordingPublisher{}).publish) })

	if !strings.Contains(out, "50h0m0s old") {
		t.Errorf("the stale line does not say how old the snapshot is:\n%s", out)
	}
}

// TestAHeldCardIsNotAShortfall — the hold gate is the user parking work behind a
// gate, and stepping over it is the one thing an unattended dispatcher must
// never do. Skipping it is the binary obeying, not falling short.
func TestAHeldCardIsNotAShortfall(t *testing.T) {
	usage := newStubUsage(t, firingLimits())
	kanban := newStubKanban(t, stubQueueCard{
		cardID: "card-held", title: "parked work",
		heldAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	cfg := tickConfig(t, usage.URL(), kanban.URL(), newStubSpend(t).URL())

	pub := &recordingPublisher{}
	var code int
	out := captureLog(t, func() { code = run(cfg, pub.publish) })

	if !strings.Contains(out, "HELD") {
		t.Fatalf("the fixture did not reach the hold gate:\n%s", out)
	}
	if code != 0 {
		t.Errorf("a held card exited %d, want 0", code)
	}
	requireNoShortfall(t, out)
}

// TestAnEmptyQueueIsNotAShortfall — the queue is empty on most ticks, and the
// designed answer is to dispatch the default discovery prompt. Nothing fell
// short.
func TestAnEmptyQueueIsNotAShortfall(t *testing.T) {
	usage := newStubUsage(t, firingLimits())
	cfg := tickConfig(t, usage.URL(), newStubKanban(t).URL(), newStubSpend(t).URL())

	pub := &recordingPublisher{}
	var code int
	out := captureLog(t, func() { code = run(cfg, pub.publish) })

	if pub.calls != 1 {
		t.Errorf("an empty queue dispatched %d workers, want 1 (the default prompt)", pub.calls)
	}
	if code != 0 {
		t.Errorf("an empty queue exited %d, want 0", code)
	}
	requireNoShortfall(t, out)
}

// TestAFullyHealthyDispatchIsNotAShortfall — the whole job done end to end:
// queued card selected, worker published, card moved out of Queued, telemetry
// card created. Without this the two tests above are satisfied by a recorder
// that never records, because neither of them reaches the write path at all.
func TestAFullyHealthyDispatchIsNotAShortfall(t *testing.T) {
	usage := newStubUsage(t, firingLimits())
	kanban := newStubKanban(t, stubQueueCard{cardID: "card-1", title: "do the thing", body: "the details"})
	cfg := tickConfig(t, usage.URL(), kanban.URL(), newStubSpend(t).URL())

	pub := &recordingPublisher{}
	var code int
	out := captureLog(t, func() { code = run(cfg, pub.publish) })

	if pub.calls != 1 {
		t.Fatalf("dispatched %d workers, want 1", pub.calls)
	}
	if !strings.Contains(pub.prompts[0], "do the thing") {
		t.Errorf("the queued card's title did not reach the prompt: %q", pub.prompts[0])
	}
	if kanban.moved["card-1"] != stubColInProgQ {
		t.Errorf("the dispatched card was not moved out of %q (moved=%v)", autoworkerQueueColumnFrom, kanban.moved)
	}
	if len(kanban.created) != 1 {
		t.Errorf("created %d telemetry cards, want 1", len(kanban.created))
	}
	if code != 0 {
		t.Errorf("a tick that did its whole job exited %d, want 0", code)
	}
	requireNoShortfall(t, out)
}

// TestADryRunIsNotAShortfall — a dry run evaluates every provider, publishes
// nothing and must stay green, or -dry-run becomes unusable for the thing it is
// for.
func TestADryRunIsNotAShortfall(t *testing.T) {
	usage := newStubUsage(t, firingLimits())
	cfg := tickConfig(t, usage.URL(), newStubKanban(t).URL(), newStubSpend(t).URL())
	cfg.dryRun = true

	pub := &recordingPublisher{}
	var code int
	out := captureLog(t, func() { code = run(cfg, pub.publish) })

	if pub.calls != 0 {
		t.Errorf("a dry run published %d workers", pub.calls)
	}
	if code != 0 {
		t.Errorf("a dry run exited %d, want 0", code)
	}
	requireNoShortfall(t, out)
}

// TestAQueueBoardThatDoesNotExistYetIsNotAShortfallOnADryRun — a fresh install
// has not bootstrapped the board, and a dry run is read-only by design so it
// does not create it. That is the state the system is supposed to be in before
// its first real tick.
func TestAQueueBoardThatDoesNotExistYetIsNotAShortfallOnADryRun(t *testing.T) {
	client, why := initQueueClient(newStubEmptyBoards(t), "", true)
	if client != nil {
		t.Errorf("a missing board returned a client")
	}
	if why != "" {
		t.Errorf("a board that does not exist yet was reported as a shortfall: %q", why)
	}
}
