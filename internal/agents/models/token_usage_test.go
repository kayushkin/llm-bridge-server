package models

import (
	"testing"
	"time"
)

func TestTokenUsageTotalsTheDaysInsideThePeriodPerAgentAndModel(t *testing.T) {
	store := tempStore(t)
	today := time.Now().UTC()
	day := func(daysAgo int) string { return today.AddDate(0, 0, -daysAgo).Format("2006-01-02") }
	for _, row := range []struct {
		date, agent, orchestrator, model string
		input, output, requests          int64
		cost                             float64
	}{
		{day(0), "agent-a", "inber", "model-a", 100, 10, 1, 1.5},
		{day(3), "agent-a", "inber", "model-a", 200, 20, 2, 0.5},
		{day(0), "agent-a", "other", "model-a", 1, 1, 1, 0},
		{day(0), "agent-b", "inber", "model-b", 1000, 100, 4, 3},
		{day(10), "agent-a", "inber", "model-a", 5000, 500, 9, 9},
	} {
		if _, err := store.db.Exec(`INSERT INTO usage (date, agent, orchestrator, model, provider, input_tokens, output_tokens, requests, cost_usd)
			VALUES ($1, $2, $3, $4, 'provider-a', $5, $6, $7, $8)`, row.date, row.agent, row.orchestrator, row.model, row.input, row.output, row.requests, row.cost); err != nil {
			t.Fatal(err)
		}
	}
	week, err := store.TokenUsageOverTheLastDays(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(week) != 3 || week[0].Agent != "agent-b" || week[1].InputTokens != 300 || week[1].Requests != 3 || week[1].CostUSD != 2 || week[2].Orchestrator != "other" {
		t.Fatalf("the week's totals: %+v", week)
	}
	day1, err := store.TokenUsageOverTheLastDays(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(day1) != 3 || day1[1].InputTokens != 100 {
		t.Fatalf("the last day's totals: %+v", day1)
	}
	none, err := tempStore(t).TokenUsageOverTheLastDays(30)
	if err != nil || len(none) != 0 {
		t.Fatalf("an empty table totals nothing: %+v, %v", none, err)
	}
}
