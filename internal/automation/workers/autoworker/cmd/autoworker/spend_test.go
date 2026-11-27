package main

import (
	"strings"
	"testing"
)

// The card's own ceiling always wins. It is already (ceiling - spent), so it is
// the only number that keeps that card under the ceiling the user set on it.
func TestCardCeilingBeatsFleetDefault(t *testing.T) {
	card := &queueCandidate{cardID: "c1", maxBudgetUSD: 2.50}

	// Even a TIGHTER fleet default must not override the card. The ceiling is the
	// promise made about that specific task; the default is only a backstop.
	got, _ := resolveSessionBudget(card, 1.00)
	if got != 2.50 {
		t.Errorf("fleet default overrode the card's ceiling: got %v, want 2.50", got)
	}

	got, _ = resolveSessionBudget(card, 100.00)
	if got != 2.50 {
		t.Errorf("fleet default overrode the card's ceiling: got %v, want 2.50", got)
	}
}

// The gap this default exists to close: the noteboard-discovery fallback has no
// card at all, so no per-card ceiling can ever reach it.
func TestDiscoverySessionGetsTheFleetDefault(t *testing.T) {
	got, why := resolveSessionBudget(nil, 8.00)
	if got != 8.00 {
		t.Fatalf("discovery session was not capped by the fleet default: got %v", got)
	}
	if why == "" {
		t.Error("a capped session must say why")
	}
}

// A card with no ceiling of its own still gets the backstop.
func TestUncappedCardGetsTheFleetDefault(t *testing.T) {
	got, _ := resolveSessionBudget(&queueCandidate{cardID: "c1"}, 8.00)
	if got != 8.00 {
		t.Fatalf("card without a ceiling was not covered by the fleet default: got %v", got)
	}
}

// Off by default. Arming a fleet-wide cap is the user's call — this must not
// change behavior until they set a number.
func TestNoDefaultMeansUncapped(t *testing.T) {
	for name, cand := range map[string]*queueCandidate{
		"discovery session": nil,
		"card, no ceiling":  {cardID: "c1"},
	} {
		got, why := resolveSessionBudget(cand, 0)
		if got != 0 {
			t.Errorf("%s: expected uncapped, got a cap of %v", name, got)
		}
		// An uncapped session must be able to say so. Silence is how a cap that
		// never armed goes unnoticed for months.
		if why == "" {
			t.Errorf("%s: an uncapped session gave no reason", name)
		}
		// And it must say something DIFFERENT from an armed default. "Not empty" is
		// satisfied by the armed wording, so the gate that decides whether the
		// default applies at all could invert and every assertion here would hold —
		// while the operator reads "fleet default" over a session that has none.
		if _, armed := resolveSessionBudget(cand, 8.00); why == armed {
			t.Errorf("%s: an unarmed fleet default gives the same reason as an armed "+
				"one (%q); the reason is the only place this distinction is visible",
				name, why)
		}
	}
}

// The fire path's announcement of the cap, through run(). The branch that wrote
// the boundary-value rows declared this line unreachable because it sat inside
// main()'s tick loop; main has since become run(cfg, modelRoleFor, publish), so it is not.
//
// With no fleet default and no queue card, the session goes out uncapped, and a
// `> 0` drifted to `>= 0` would announce it as "capped at $0.00" — a cap of zero
// is what the harness reads as NO cap, so that line would tell the operator the
// opposite of what happened. The capped run is the comparison: it must publish a
// real cap, so a fixture that never reached the fire path cannot pass here.
func TestTheFirePathAnnouncesAnUncappedSessionAsUncapped(t *testing.T) {
	fire := func(fleetDefaultUSD float64) (string, *recordingPublisher) {
		usage := newStubUsage(t, firingLimits())
		cfg := tickConfig(t, usage.URL(), newStubKanban(t).URL(), newStubSpend(t).URL())
		cfg.defaultMaxBudgetUSD = fleetDefaultUSD
		publisher := &recordingPublisher{}
		out := captureLog(t, func() { run(cfg, defaultLaneModelRole, publisher.publish) })
		return out, publisher
	}

	cappedLog, capped := fire(2.50)
	if len(capped.budgets) != 1 || capped.budgets[0] != 2.50 {
		t.Fatalf("the capped run published budgets %v, want [2.50]; the fixture did not "+
			"reach the fire path, so the uncapped run below proves nothing:\n%s",
			capped.budgets, cappedLog)
	}

	uncappedLog, uncapped := fire(0)
	if len(uncapped.budgets) != 1 || uncapped.budgets[0] != 0 {
		t.Fatalf("the uncapped run published budgets %v, want [0]:\n%s", uncapped.budgets, uncappedLog)
	}
	if !strings.Contains(uncappedLog, "session is UNCAPPED") {
		t.Errorf("a session published with no cap was not announced as UNCAPPED:\n%s", uncappedLog)
	}
}
