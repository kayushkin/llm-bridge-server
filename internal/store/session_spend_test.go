package store

import (
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func spendFixture(t *testing.T, bridgeID string) *Store {
	t.Helper()
	s := testStore(t)
	if err := s.CreateSession(&Session{
		SessionID: bridgeID,
		Harness:   "claude_code",
		State:     string(msg.SessionIdle),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s
}

// TestSessionSpend_RoundTripsTheWholeAggregate pins that what
// RecordSessionSpend writes is what a derivation is later seeded from. The
// dollar total and the breakdown live in different columns — a REAL the gate
// can compare in SQL and a JSON blob it cannot — so "they both came back"
// is the property that keeps that split honest.
func TestSessionSpend_RoundTripsTheWholeAggregate(t *testing.T) {
	s := spendFixture(t, "br_spend_roundtrip")

	detail := SessionSpendDetail{
		Usage:         msg.TokenUsage{InputTokens: 120, OutputTokens: 34, TotalTokens: 154},
		Calls:         7,
		ByModel:       map[string]float64{"claude-opus-4-7": 3.25, "claude-haiku-4-5": 0.05},
		ByQuerySource: map[string]float64{"sdk": 3.20, "generate_session_title": 0.10},
	}
	if _, err := s.RecordSessionSpend("br_spend_roundtrip", 3.30, detail); err != nil {
		t.Fatalf("record spend: %v", err)
	}

	totalUSD, got, err := s.SessionSpend("br_spend_roundtrip")
	if err != nil {
		t.Fatalf("read spend: %v", err)
	}
	if totalUSD != 3.30 {
		t.Errorf("total = $%.2f; want $3.30", totalUSD)
	}
	if got.Calls != 7 {
		t.Errorf("calls = %d; want 7", got.Calls)
	}
	if got.Usage.InputTokens != 120 || got.Usage.OutputTokens != 34 {
		t.Errorf("usage = %d in / %d out; want 120 / 34", got.Usage.InputTokens, got.Usage.OutputTokens)
	}
	if got.ByModel["claude-opus-4-7"] != 3.25 || got.ByModel["claude-haiku-4-5"] != 0.05 {
		t.Errorf("by_model = %v", got.ByModel)
	}
	if got.ByQuerySource["generate_session_title"] != 0.10 {
		t.Errorf("by_query_source = %v", got.ByQuerySource)
	}
}

// TestSessionSpend_ANeverSpentSessionReadsAsZero covers the seed a brand-new
// session gets. Nil maps are the honest answer for a session that has spent
// nothing — there is no model to attribute zero dollars to.
func TestSessionSpend_ANeverSpentSessionReadsAsZero(t *testing.T) {
	s := spendFixture(t, "br_spend_fresh")

	totalUSD, detail, err := s.SessionSpend("br_spend_fresh")
	if err != nil {
		t.Fatalf("read spend: %v", err)
	}
	if totalUSD != 0 || detail.Calls != 0 || len(detail.ByModel) != 0 {
		t.Errorf("fresh session reads as $%.2f / %d calls / %v", totalUSD, detail.Calls, detail.ByModel)
	}
}

// TestRecordSessionSpend_ALowerTotalChangesNothing is the backstop for the
// one case seeding cannot cover: a store read that fails at
// derivation-creation time seeds zero, and the run that follows reports
// totals below what the row already knows.
//
// The dollar figure holding is the half that stops a spent ceiling re-arming.
// The breakdown holding is the half that stops the attribution being replaced
// by a smaller one — dollars the row is still counting would otherwise belong
// to no model at all.
func TestRecordSessionSpend_ALowerTotalChangesNothing(t *testing.T) {
	s := spendFixture(t, "br_spend_backstop")

	full := SessionSpendDetail{
		Calls:   10,
		Usage:   msg.TokenUsage{InputTokens: 1000},
		ByModel: map[string]float64{"claude-opus-4-7": 9.00},
	}
	if _, err := s.RecordSessionSpend("br_spend_backstop", 9.00, full); err != nil {
		t.Fatalf("record spend: %v", err)
	}

	// An unseeded second run: one $0.10 call, reported as the whole total.
	partial := SessionSpendDetail{
		Calls:   1,
		Usage:   msg.TokenUsage{InputTokens: 10},
		ByModel: map[string]float64{"claude-haiku-4-5": 0.10},
	}
	stored, err := s.RecordSessionSpend("br_spend_backstop", 0.10, partial)
	if err != nil {
		t.Fatalf("record spend: %v", err)
	}
	if stored != 9.00 {
		t.Errorf("stored total = $%.2f after a lower report; want $9.00", stored)
	}

	totalUSD, detail, err := s.SessionSpend("br_spend_backstop")
	if err != nil {
		t.Fatalf("read spend: %v", err)
	}
	if totalUSD != 9.00 {
		t.Errorf("total = $%.2f; want $9.00", totalUSD)
	}
	if detail.Calls != 10 {
		t.Errorf("calls = %d after a lower report; want the recorded 10", detail.Calls)
	}
	if detail.ByModel["claude-opus-4-7"] != 9.00 {
		t.Errorf("by_model lost the $9.00 it was attributing: %v", detail.ByModel)
	}
	if _, ok := detail.ByModel["claude-haiku-4-5"]; ok {
		t.Errorf("by_model took the smaller run's attribution: %v", detail.ByModel)
	}
}

// TestRecordSessionSpend_AHigherTotalReplacesTheBreakdown is the other side
// of the same guard. A seeded derivation's report always covers everything
// the row already knew plus the new call, so a report that advances the total
// is the more complete one and must win outright — merging the two would
// double-count every dollar the seed carried in.
func TestRecordSessionSpend_AHigherTotalReplacesTheBreakdown(t *testing.T) {
	s := spendFixture(t, "br_spend_advance")

	if _, err := s.RecordSessionSpend("br_spend_advance", 1.00, SessionSpendDetail{
		Calls: 1, ByModel: map[string]float64{"claude-opus-4-7": 1.00},
	}); err != nil {
		t.Fatalf("record spend: %v", err)
	}
	if _, err := s.RecordSessionSpend("br_spend_advance", 3.00, SessionSpendDetail{
		Calls: 3, ByModel: map[string]float64{"claude-opus-4-7": 3.00},
	}); err != nil {
		t.Fatalf("record spend: %v", err)
	}

	totalUSD, detail, err := s.SessionSpend("br_spend_advance")
	if err != nil {
		t.Fatalf("read spend: %v", err)
	}
	if totalUSD != 3.00 {
		t.Errorf("total = $%.2f; want $3.00", totalUSD)
	}
	if detail.Calls != 3 {
		t.Errorf("calls = %d; want 3", detail.Calls)
	}
	if detail.ByModel["claude-opus-4-7"] != 3.00 {
		t.Errorf("by_model[claude-opus-4-7] = $%.2f; want $3.00 — the breakdowns were merged rather than replaced", detail.ByModel["claude-opus-4-7"])
	}
}

// TestRecordSessionSpend_UnknownSessionIsAnError keeps the gate loud. A spend
// event for a session with no row is a producer bug, and silently succeeding
// would drop real dollars on the floor.
func TestRecordSessionSpend_UnknownSessionIsAnError(t *testing.T) {
	s := testStore(t)
	if _, err := s.RecordSessionSpend("br_does_not_exist", 1.00, SessionSpendDetail{}); err == nil {
		t.Error("recording spend against a missing session returned no error")
	}
}
