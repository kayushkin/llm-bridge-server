package main

import "testing"

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
// change behaviour until they set a number.
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
	}
}
