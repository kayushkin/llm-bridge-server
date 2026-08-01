package harness

import (
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// The tests here pin seedAPISpend's own contract rather than the manager
// behaviour it produces. They exist because the manager path cannot see
// these properties: it builds a derivation, seeds it once, and never seeds
// again, so on that path "assign" and "add" are the same statement and a
// shared map is never written to twice. Both become wrong the moment a
// second caller appears, which is exactly when nobody re-reads the doc
// comment.

// TestSeedAPISpend_AssignsRatherThanAdds pins the precondition in
// seedAPISpend's doc: it must be called before the derivation counts
// anything, because it replaces the accumulators instead of adding to them.
// If it added, seeding a derivation twice would invent spending nobody did.
func TestSeedAPISpend_AssignsRatherThanAdds(t *testing.T) {
	d := newDerivationState()

	d.seedAPISpend(5.00, 2, msg.TokenUsage{InputTokens: 100, OutputTokens: 50},
		map[string]float64{"opus": 5.00}, map[string]float64{"sdk": 5.00})
	d.seedAPISpend(5.00, 2, msg.TokenUsage{InputTokens: 100, OutputTokens: 50},
		map[string]float64{"opus": 5.00}, map[string]float64{"sdk": 5.00})

	if d.apiSpendUSD != 5.00 {
		t.Errorf("total = $%.2f after seeding $5.00 twice; want $5.00 — the seed adds instead of assigning", d.apiSpendUSD)
	}
	if d.apiSpendCalls != 2 {
		t.Errorf("calls = %d; want 2", d.apiSpendCalls)
	}
	if d.apiSpendUsage.InputTokens != 100 {
		t.Errorf("input tokens = %d; want 100", d.apiSpendUsage.InputTokens)
	}
	if d.apiSpendByModel["opus"] != 5.00 {
		t.Errorf("by_model[opus] = $%.2f; want $5.00", d.apiSpendByModel["opus"])
	}
	if d.apiSpendBySource["sdk"] != 5.00 {
		t.Errorf("by_query_source[sdk] = $%.2f; want $5.00", d.apiSpendBySource["sdk"])
	}
}

// TestSeedAPISpend_CopiesTheMapsItIsGiven pins that the derivation does not
// take ownership of the caller's maps. The caller's copy comes out of a
// store read it is free to reuse; sharing the map would let every priced API
// call mutate data the derivation does not own.
func TestSeedAPISpend_CopiesTheMapsItIsGiven(t *testing.T) {
	byModel := map[string]float64{"opus": 1.00}
	byQuerySource := map[string]float64{"sdk": 1.00}

	d := newDerivationState()
	d.seedAPISpend(1.00, 1, msg.TokenUsage{}, byModel, byQuerySource)

	d.applyAPICall(&msg.APICallEvent{Model: "opus", QuerySource: "sdk", CostUSD: 2.00})

	if byModel["opus"] != 1.00 {
		t.Errorf("caller's by_model was mutated to $%.2f; seedAPISpend kept the caller's map instead of copying it", byModel["opus"])
	}
	if byQuerySource["sdk"] != 1.00 {
		t.Errorf("caller's by_query_source was mutated to $%.2f", byQuerySource["sdk"])
	}
	if d.apiSpendByModel["opus"] != 3.00 {
		t.Errorf("derivation's by_model[opus] = $%.2f; want $3.00", d.apiSpendByModel["opus"])
	}
}

// TestSeedAPISpend_NilMapsStayUsable covers the ordinary case for a session
// that has spent nothing yet: the store hands back a zero SessionSpendDetail
// whose maps are nil, and the derivation must still be able to record into
// them rather than panicking on the first priced call.
func TestSeedAPISpend_NilMapsStayUsable(t *testing.T) {
	d := newDerivationState()
	d.seedAPISpend(0, 0, msg.TokenUsage{}, nil, nil)

	d.applyAPICall(&msg.APICallEvent{Model: "opus", QuerySource: "sdk", CostUSD: 1.00})

	if d.apiSpendByModel["opus"] != 1.00 {
		t.Errorf("by_model[opus] = $%.2f; want $1.00", d.apiSpendByModel["opus"])
	}
	if d.apiSpendBySource["sdk"] != 1.00 {
		t.Errorf("by_query_source[sdk] = $%.2f; want $1.00", d.apiSpendBySource["sdk"])
	}
}
