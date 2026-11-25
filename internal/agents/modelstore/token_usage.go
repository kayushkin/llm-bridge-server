package modelstore

import "fmt"

// TokenUsageTotals is one agent, orchestrator and model's token totals over a
// period of the old daily usage rows.
type TokenUsageTotals struct {
	Agent        string
	Orchestrator string
	Model        string
	InputTokens  int64
	OutputTokens int64
	Requests     int64
	CostUSD      float64
}

// TokenUsageOverTheLastDays totals the daily token counts an older
// orchestrator wrote into the usage table, over the UTC days from days days
// ago to today, per agent, orchestrator and model, the most tokens first. It
// is what usage-store's GET /api/usage has always answered; nothing writes the
// rows any more.
func (s *Store) TokenUsageOverTheLastDays(days int) ([]TokenUsageTotals, error) {
	rows, err := s.db.Query(`SELECT agent, orchestrator, model,
			SUM(input_tokens), SUM(output_tokens), SUM(requests), SUM(cost_usd)
		FROM usage
		WHERE date >= to_char((now() AT TIME ZONE 'UTC')::date - $1::integer, 'YYYY-MM-DD')
		GROUP BY agent, orchestrator, model
		ORDER BY SUM(input_tokens) + SUM(output_tokens) DESC, agent, orchestrator, model`, days)
	if err != nil {
		return nil, fmt.Errorf("token usage over the last %d days: %w", days, err)
	}
	defer rows.Close()
	var totals []TokenUsageTotals
	for rows.Next() {
		var row TokenUsageTotals
		if err := rows.Scan(&row.Agent, &row.Orchestrator, &row.Model, &row.InputTokens, &row.OutputTokens, &row.Requests, &row.CostUSD); err != nil {
			return nil, fmt.Errorf("token usage over the last %d days: %w", days, err)
		}
		totals = append(totals, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("token usage over the last %d days: %w", days, err)
	}
	return totals, nil
}
