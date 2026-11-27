package main

import (
	"strings"
	"testing"
)

// ==================== reading the verdict ====================
//
// Every assertion below reads the single `run degraded:` line rather than the
// whole captured log. A `strings.Contains` over the whole capture is answered by
// whatever else happened to print — and this binary prints the same card id and
// title from `scopeParent`'s own failure line one call above the Record, so a
// shortfall report could go anonymous with the case still green. Measured as a
// live MISSED on the sibling spend-ceiling row (card `2920becb` row 34).

// degradedLine returns the one line degradations.Report writes, or "" if the run
// reported no shortfall at all.
func degradedLine(t *testing.T, captured string) string {
	t.Helper()
	for _, l := range strings.Split(captured, "\n") {
		if strings.Contains(l, "run degraded:") {
			return l
		}
	}
	return ""
}

// assertShortfall checks the run went red and that its report names each of the
// given fragments — on the report line, not anywhere in the log.
func assertShortfall(t *testing.T, code int, captured string, fragments ...string) {
	t.Helper()
	if code == 0 {
		t.Fatalf("run exited 0; want non-zero. log:\n%s", captured)
	}
	line := degradedLine(t, captured)
	if line == "" {
		t.Fatalf("run exited %d but wrote no `run degraded:` line — the exit code is unexplained. log:\n%s", code, captured)
	}
	if !strings.HasPrefix(line, "[scoper] run degraded:") {
		t.Errorf("report line does not carry the scoper's own prefix, so a reader of run history cannot tell which of the eleven cron binaries fell short:\n%s", line)
	}
	for _, f := range fragments {
		if !strings.Contains(line, f) {
			t.Errorf("report line does not name %q:\n%s", f, line)
		}
	}
}

// assertClean checks the run went green and said nothing about a shortfall.
func assertClean(t *testing.T, code int, captured string) {
	t.Helper()
	if code != 0 {
		t.Fatalf("run exited %d; want 0. log:\n%s", code, captured)
	}
	if line := degradedLine(t, captured); line != "" {
		t.Fatalf("run exited 0 but reported a shortfall anyway:\n%s", line)
	}
}

func goalNeedingScope() stubGoal {
	return stubGoal{cardID: "goal-1", title: "Ship the thing", body: "make it work", tags: []string{tagTrigger}}
}

// ==================== the deliberate skips ====================
//
// These six exist to keep the recorder honest in the other direction. Job 15
// logs `parents-needing-scope=0` on 100 of 100 retained ticks, so if any of
// these went red the job would be red essentially always and its exit code would
// carry less than the green it replaced. Without the full-tick case in
// particular, the five below it are all satisfied by a recorder that never
// records anything at all.

func TestAnEmptyGoalsColumnIsNotAShortfall(t *testing.T) {
	k := newStubKanban(t)
	decomposer := newStubModel(t)
	var code int
	out := captureLog(t, func() { code = run(testConfig(k, decomposer)) })
	assertClean(t, code, out)
	if decomposer.calls != 0 {
		t.Errorf("an empty board called the decomposer %d times; a tick with nothing to do must not pay for a model call", decomposer.calls)
	}
}

func TestACardWithoutTheTriggerTagIsNotAShortfall(t *testing.T) {
	k := newStubKanban(t, stubGoal{cardID: "goal-1", title: "Someone else's card", tags: []string{"unrelated"}})
	decomposer := newStubModel(t, plainSubtask("a"))
	var code int
	out := captureLog(t, func() { code = run(testConfig(k, decomposer)) })
	assertClean(t, code, out)
	if len(k.created) != 0 {
		t.Errorf("scoped an untagged card: created %d sub-cards", len(k.created))
	}
}

func TestAnAlreadyScopedCardIsNotAShortfall(t *testing.T) {
	g := goalNeedingScope()
	g.isScoped = true
	k := newStubKanban(t, g)
	decomposer := newStubModel(t, plainSubtask("a"))
	var code int
	out := captureLog(t, func() { code = run(testConfig(k, decomposer)) })
	assertClean(t, code, out)
	if decomposer.calls != 0 {
		t.Errorf("re-decomposed an already-scoped card (%d model calls)", decomposer.calls)
	}
}

// TestAFullTickThatScopedEverythingExitsZero is the load-bearing green. It drives
// the whole write path — decompose, three sub-cards, three parent links, the
// scoped stamp — and asserts each write really happened. A recorder that records
// nothing passes every other green case in this file and fails none of them; it
// passes this one only because this one checks the work as well as the verdict.
func TestAFullTickThatScopedEverythingExitsZero(t *testing.T) {
	k := newStubKanban(t, goalNeedingScope())
	decomposer := newStubModel(t, plainSubtask("one"), plainSubtask("two"), plainSubtask("three"))
	var code int
	out := captureLog(t, func() { code = run(testConfig(k, decomposer)) })
	assertClean(t, code, out)

	if len(k.created) != 3 {
		t.Fatalf("created %d sub-cards, want 3", len(k.created))
	}
	for i, c := range k.created {
		if got, _ := c["parent_id"].(string); got != "goal-1" {
			t.Errorf("sub-card %d carries parent_id %q, want goal-1 — the hold gate and the spend ceiling both roll up over that edge", i, got)
		}
		if got, _ := c["column_id"].(string); got != stubColBacklog {
			t.Errorf("sub-card %d landed in column %q, want the Backlog column", i, got)
		}
	}
	if len(k.linked) != 3 {
		t.Errorf("attached %d parent_card links, want 3", len(k.linked))
	}
	if len(k.stamped) != 1 || k.stamped[0] != "goal-1" {
		t.Errorf("stamped %v, want exactly [goal-1] — an unstamped parent is re-decomposed every five minutes", k.stamped)
	}
}

func TestHittingThePerTickCapIsNotAShortfall(t *testing.T) {
	g2 := goalNeedingScope()
	g2.cardID, g2.title = "goal-2", "The deferred one"
	k := newStubKanban(t, goalNeedingScope(), g2)
	decomposer := newStubModel(t, plainSubtask("one"))
	cfg := testConfig(k, decomposer)
	cfg.MaxScope = 1
	var code int
	out := captureLog(t, func() { code = run(cfg) })
	assertClean(t, code, out)
	if len(k.stamped) != 1 {
		t.Errorf("stamped %d parents under a cap of 1: %v", len(k.stamped), k.stamped)
	}
	if !strings.Contains(out, "deferring 1 parents") {
		t.Errorf("the cap fired without saying what it deferred; the next tick is the only thing that picks those up:\n%s", out)
	}
}

func TestADryRunWritesNothingAndIsNotAShortfall(t *testing.T) {
	k := newStubKanban(t, goalNeedingScope())
	decomposer := newStubModel(t, plainSubtask("one"))
	cfg := testConfig(k, decomposer)
	cfg.DryRun = true
	var code int
	out := captureLog(t, func() { code = run(cfg) })
	assertClean(t, code, out)
	if len(k.created) != 0 || len(k.stamped) != 0 {
		t.Errorf("a dry run wrote: %d cards, %d stamps", len(k.created), len(k.stamped))
	}
}

// ==================== the shortfalls ====================

// The read-path pair.

func TestAGoalWhoseTagsCouldNotBeReadIsAShortfall(t *testing.T) {
	k := newStubKanban(t, goalNeedingScope())
	k.failTags = 503
	decomposer := newStubModel(t, plainSubtask("one"))
	var code int
	out := captureLog(t, func() { code = run(testConfig(k, decomposer)) })
	assertShortfall(t, code, out, "goal-1", "Ship the thing", "never considered")
}

func TestADecomposerFailureIsAShortfall(t *testing.T) {
	k := newStubKanban(t, goalNeedingScope())
	decomposer := newStubModel(t, plainSubtask("one"))
	decomposer.status = 400 // the shape the credit-balance outage took on the sibling binaries
	var code int
	out := captureLog(t, func() { code = run(testConfig(k, decomposer)) })
	assertShortfall(t, code, out, "goal-1", "decomposer failed", "stays unscoped")
	if len(k.stamped) != 0 {
		t.Errorf("stamped a parent that was never decomposed: %v", k.stamped)
	}
}

func TestAZeroSubtaskPlanIsAShortfall(t *testing.T) {
	k := newStubKanban(t, goalNeedingScope())
	decomposer := newStubModel(t) // a well-formed tool call carrying no sub-tasks
	var code int
	out := captureLog(t, func() { code = run(testConfig(k, decomposer)) })
	assertShortfall(t, code, out, "goal-1", "0 sub-tasks")
}

// TestABacklogColumnWithNoIDIsAShortfall. An ABSENT Backlog column is not
// reachable — EnsureBoard runs first and creates any column the board lacks. A
// column row present under the right name and carrying a blank id is what
// actually gets past it, and it lands on exactly the same guard.
func TestABacklogColumnWithNoIDIsAShortfall(t *testing.T) {
	k := newStubKanban(t, goalNeedingScope())
	k.blankBacklogID = true
	decomposer := newStubModel(t, plainSubtask("one"))
	var code int
	out := captureLog(t, func() { code = run(testConfig(k, decomposer)) })
	assertShortfall(t, code, out, "goal-1", columnBacklog, "nowhere to go")
}

// The write-path four. These are the ones this card's second rule says to look
// for, and three of them cost money rather than only accuracy.

// TestARefusedSubcardIsAShortfall also pins the consequence: the parent must be
// left UNSTAMPED, because that is what makes the next tick re-decompose it.
func TestARefusedSubcardIsAShortfall(t *testing.T) {
	k := newStubKanban(t, goalNeedingScope())
	k.failCreate = 500
	decomposer := newStubModel(t, plainSubtask("one"), plainSubtask("two"))
	var code int
	out := captureLog(t, func() { code = run(testConfig(k, decomposer)) })
	assertShortfall(t, code, out, "goal-1", "refused by the board", "re-creates every sub-card")
	if len(k.stamped) != 0 {
		t.Errorf("stamped a parent whose plan was only half written: %v", k.stamped)
	}
}

// TestARefusedParentLinkIsAShortfall pins the opposite consequence: the card
// exists, so the run must carry on and finish the plan. Abandoning it here would
// leave the parent unstamped and re-create the siblings on the next tick — the
// failure mode the case above is about, caused by the repair for this one.
func TestARefusedParentLinkIsAShortfall(t *testing.T) {
	k := newStubKanban(t, goalNeedingScope())
	k.failLink = 500
	decomposer := newStubModel(t, plainSubtask("one"), plainSubtask("two"))
	var code int
	out := captureLog(t, func() { code = run(testConfig(k, decomposer)) })
	assertShortfall(t, code, out, "goal-1", "no parent_card link", "closure roll-up")
	if len(k.created) != 2 {
		t.Errorf("created %d sub-cards; a refused link must not abandon the rest of the plan", len(k.created))
	}
	if len(k.stamped) != 1 {
		t.Errorf("stamped %v; the plan was written in full, so the parent must not be re-decomposed", k.stamped)
	}
}

func TestARefusedScopedStampIsAShortfall(t *testing.T) {
	k := newStubKanban(t, goalNeedingScope())
	k.failStamp = 500
	decomposer := newStubModel(t, plainSubtask("one"), plainSubtask("two"))
	var code int
	out := captureLog(t, func() { code = run(testConfig(k, decomposer)) })
	assertShortfall(t, code, out, "goal-1", "scoped stamp was refused", "duplicate set")
	if len(k.created) != 2 {
		t.Errorf("created %d sub-cards, want 2 — they are what the next tick will duplicate", len(k.created))
	}
}

// TestADroppedEmptyTitledSubtaskIsAShortfall is the quietest of the eight and
// the only one that is permanent. Everything else above either stays unstamped
// and is retried, or leaves a card behind that can be found. This one drops a
// slice of the goal and stamps the parent anyway, so nothing revisits it.
func TestADroppedEmptyTitledSubtaskIsAShortfall(t *testing.T) {
	k := newStubKanban(t, goalNeedingScope())
	decomposer := newStubModel(t, plainSubtask("one"), Subtask{Title: "   ", GroupKey: "grp"})
	var code int
	out := captureLog(t, func() { code = run(testConfig(k, decomposer)) })
	assertShortfall(t, code, out, "goal-1", "empty title", "lost")
	if len(k.created) != 1 {
		t.Errorf("created %d sub-cards, want 1 — the titled one", len(k.created))
	}
	if len(k.stamped) != 1 {
		t.Fatalf("stamped %v, want [goal-1] — this case is a shortfall precisely BECAUSE the parent is stamped despite the loss", k.stamped)
	}
}

// TestEveryShortfallOnOneTickIsReportedTogether pins that the report names all
// of them rather than the first. A recorder that stopped at one would still pass
// every case above, and a person reading run history would repair one cause,
// see the job go red again, and repair the next.
func TestEveryShortfallOnOneTickIsReportedTogether(t *testing.T) {
	g2 := goalNeedingScope()
	g2.cardID, g2.title = "goal-2", "The second one"
	k := newStubKanban(t, goalNeedingScope(), g2)
	decomposer := newStubModel(t, plainSubtask("one"), Subtask{Title: "", GroupKey: "grp"})
	var code int
	out := captureLog(t, func() { code = run(testConfig(k, decomposer)) })
	assertShortfall(t, code, out, "goal-1", "goal-2")
	line := degradedLine(t, out)
	if !strings.Contains(line, "2 shortfall(s)") {
		t.Errorf("report does not count both parents' losses:\n%s", line)
	}
}
