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

// What the two record methods write is what a derivation is later seeded from:
// the cost in spend_usd, and the breakdown with both inputs of that cost.
func TestSessionSpend_RoundTripsCostAndBreakdown(t *testing.T) {
	s := spendFixture(t, "br_spend_roundtrip")

	if err := s.RecordAPISpendBreakdown("br_spend_roundtrip", SessionSpendDetail{
		Usage:         msg.TokenUsage{InputTokens: 120, OutputTokens: 34, TotalTokens: 154},
		Calls:         7,
		ByModel:       map[string]float64{"claude-opus-4-7": 3.25, "claude-haiku-4-5": 0.05},
		ByQuerySource: map[string]float64{"sdk": 3.20, "generate_session_title": 0.10},
		APISpendUSD:   3.30,
	}); err != nil {
		t.Fatalf("record breakdown: %v", err)
	}
	if _, err := s.RecordSessionCost("br_spend_roundtrip", 3.45, 3.45); err != nil {
		t.Fatalf("record cost: %v", err)
	}

	totalUSD, got, err := s.SessionSpend("br_spend_roundtrip")
	if err != nil {
		t.Fatalf("read spend: %v", err)
	}
	if totalUSD != 3.45 {
		t.Errorf("cost = $%.2f; want $3.45", totalUSD)
	}
	if got.APISpendUSD != 3.30 || got.TurnResultUSD != 3.45 {
		t.Errorf("inputs = api $%.2f, results $%.2f; want 3.30, 3.45", got.APISpendUSD, got.TurnResultUSD)
	}
	if got.Calls != 7 || got.Usage.InputTokens != 120 || got.ByModel["claude-opus-4-7"] != 3.25 || got.ByQuerySource["generate_session_title"] != 0.10 {
		t.Errorf("breakdown = %+v", got)
	}
}

func TestSessionSpend_ANeverSpentSessionReadsAsZero(t *testing.T) {
	s := spendFixture(t, "br_spend_fresh")
	totalUSD, detail, err := s.SessionSpend("br_spend_fresh")
	if err != nil {
		t.Fatalf("read spend: %v", err)
	}
	if totalUSD != 0 || detail.Calls != 0 || len(detail.ByModel) != 0 || detail.APISpendUSD != 0 {
		t.Errorf("fresh session reads as $%.2f / %+v", totalUSD, detail)
	}
}

// A derivation seeded from a failed read restarts at zero. Neither its cost nor
// its breakdown may walk the recorded figures back.
func TestRecordSpend_LowerReportsChangeNothing(t *testing.T) {
	s := spendFixture(t, "br_spend_backstop")
	if err := s.RecordAPISpendBreakdown("br_spend_backstop", SessionSpendDetail{
		Calls: 10, ByModel: map[string]float64{"claude-opus-4-7": 9.00}, APISpendUSD: 9.00,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSessionCost("br_spend_backstop", 9.50, 9.50); err != nil {
		t.Fatal(err)
	}

	if err := s.RecordAPISpendBreakdown("br_spend_backstop", SessionSpendDetail{
		Calls: 1, ByModel: map[string]float64{"claude-haiku-4-5": 0.10}, APISpendUSD: 0.10,
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := s.RecordSessionCost("br_spend_backstop", 0.10, 0.10)
	if err != nil {
		t.Fatal(err)
	}
	if stored != 9.50 {
		t.Errorf("stored cost = $%.2f after a lower report; want $9.50", stored)
	}
	totalUSD, detail, err := s.SessionSpend("br_spend_backstop")
	if err != nil {
		t.Fatal(err)
	}
	if totalUSD != 9.50 || detail.Calls != 10 || detail.APISpendUSD != 9.00 || detail.TurnResultUSD != 9.50 {
		t.Errorf("after lower reports: $%.2f %+v", totalUSD, detail)
	}
	if _, ok := detail.ByModel["claude-haiku-4-5"]; ok {
		t.Errorf("by_model took the smaller run's attribution: %v", detail.ByModel)
	}
}

// A breakdown covering more API spend replaces the old one outright, and keeps
// the recorded result total, which it does not carry.
func TestRecordAPISpendBreakdown_AHigherTotalReplacesItAndKeepsTheResultTotal(t *testing.T) {
	s := spendFixture(t, "br_spend_advance")
	if _, err := s.RecordSessionCost("br_spend_advance", 1.20, 1.20); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAPISpendBreakdown("br_spend_advance", SessionSpendDetail{
		Calls: 1, ByModel: map[string]float64{"claude-opus-4-7": 1.00}, APISpendUSD: 1.00,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAPISpendBreakdown("br_spend_advance", SessionSpendDetail{
		Calls: 2, ByModel: map[string]float64{"claude-opus-4-7": 2.00}, APISpendUSD: 2.00,
	}); err != nil {
		t.Fatal(err)
	}
	_, detail, err := s.SessionSpend("br_spend_advance")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Calls != 2 || detail.ByModel["claude-opus-4-7"] != 2.00 || detail.TurnResultUSD != 1.20 {
		t.Errorf("breakdown = %+v", detail)
	}
}

func TestRecordSessionCost_UnknownSessionIsAnError(t *testing.T) {
	s := testStore(t)
	if _, err := s.RecordSessionCost("br_nope", 1, 1); err == nil {
		t.Fatal("recording cost for a session with no row succeeded")
	}
}

// Rows written before the cost estimate carry the API sum only in spend_usd; the
// migration copies it into the breakdown so api_spend_total continues from it.
func TestMigration_CopiesTheOldAPISumIntoTheBreakdown(t *testing.T) {
	s := spendFixture(t, "br_spend_legacy")
	if _, err := s.db.Exec(`UPDATE sessions SET spend_usd=4.25, api_spend_detail='{"calls":3}' WHERE bridge_id='br_spend_legacy'`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	totalUSD, detail, err := s.SessionSpend("br_spend_legacy")
	if err != nil {
		t.Fatal(err)
	}
	if totalUSD != 4.25 || detail.APISpendUSD != 4.25 || detail.Calls != 3 {
		t.Errorf("after migration: $%.2f %+v", totalUSD, detail)
	}
}
