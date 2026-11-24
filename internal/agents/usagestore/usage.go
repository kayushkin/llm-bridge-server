package usagestore

import (
	"fmt"
	"time"
)

// ModelPricing provides cost information for a model.
// Implemented by model-store or passed in directly.
type ModelPricing interface {
	InputCost() float64  // per million tokens
	OutputCost() float64 // per million tokens
	Provider() string
}

// Track records token usage for an agent/model/session.
// If pricing is provided, cost is calculated automatically.
// If costUSD is non-zero, it's used directly (e.g., from Claude Code billing).
func (s *Store) Track(agent, orchestrator, model string, inputTokens, outputTokens int64, costUSD float64, pricing ModelPricing, session, harness string) error {
	if orchestrator == "" {
		orchestrator = "inber"
	}

	provider := ""
	if pricing != nil {
		provider = pricing.Provider()
		if costUSD == 0 {
			costUSD = float64(inputTokens)/1_000_000*pricing.InputCost() + float64(outputTokens)/1_000_000*pricing.OutputCost()
		}
	}

	date := time.Now().Format("2006-01-02")
	_, err := s.db.Exec(`
		INSERT INTO usage (date, agent, orchestrator, model, provider, session, harness, input_tokens, output_tokens, requests, cost_usd)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT(date, agent, orchestrator, model, session) DO UPDATE SET
			input_tokens = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			requests = requests + 1,
			cost_usd = cost_usd + excluded.cost_usd
	`, date, agent, orchestrator, model, provider, session, harness, inputTokens, outputTokens, costUSD)
	return err
}

// buildWhere builds a WHERE clause from a QueryFilter.
func buildWhere(f QueryFilter) (string, []interface{}) {
	where := " WHERE 1=1"
	var args []interface{}

	if f.Agent != "" {
		where += " AND agent = ?"
		args = append(args, f.Agent)
	}
	if f.Orchestrator != "" {
		where += " AND orchestrator = ?"
		args = append(args, f.Orchestrator)
	}
	if f.Model != "" {
		where += " AND model = ?"
		args = append(args, f.Model)
	}
	if f.Session != "" {
		where += " AND session = ?"
		args = append(args, f.Session)
	}
	if f.Harness != "" {
		where += " AND harness = ?"
		args = append(args, f.Harness)
	}
	if f.DateFrom != "" {
		where += " AND date >= ?"
		args = append(args, f.DateFrom)
	}
	if f.DateTo != "" {
		where += " AND date <= ?"
		args = append(args, f.DateTo)
	}

	return where, args
}

// Query returns usage records matching the filter.
func (s *Store) Query(f QueryFilter) ([]UsageRecord, error) {
	where, args := buildWhere(f)
	query := `SELECT date, agent, orchestrator, model, provider, session, harness, input_tokens, output_tokens, requests, cost_usd FROM usage` + where + ` ORDER BY date DESC, agent, model`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []UsageRecord
	for rows.Next() {
		var r UsageRecord
		if err := rows.Scan(&r.Date, &r.Agent, &r.Orchestrator, &r.Model, &r.Provider, &r.Session, &r.Harness, &r.InputTokens, &r.OutputTokens, &r.Requests, &r.CostUSD); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// Stats returns aggregated statistics matching the filter.
func (s *Store) Stats(f QueryFilter) (*Stats, error) {
	where, args := buildWhere(f)

	// Aggregates
	var st Stats
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(requests),0) FROM usage`+where, args...).
		Scan(&st.TotalCostUSD, &st.TotalInput, &st.TotalOutput, &st.TotalRequests)
	if err != nil {
		return nil, err
	}

	// Distinct values
	if st.Models, err = s.distinctCol("model", where, args); err != nil {
		return nil, err
	}
	if st.Agents, err = s.distinctCol("agent", where, args); err != nil {
		return nil, err
	}
	if st.Harnesses, err = s.distinctCol("harness", where, args); err != nil {
		return nil, err
	}
	if st.Sessions, err = s.distinctCol("session", where, args); err != nil {
		return nil, err
	}

	return &st, nil
}

func (s *Store) distinctCol(col, where string, args []interface{}) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT `+col+` FROM usage`+where+` AND `+col+` != '' ORDER BY `+col, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vals []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err == nil && v != "" {
			vals = append(vals, v)
		}
	}
	return vals, nil
}

// Summary returns aggregated usage for a date range grouped by date and agent.
func (s *Store) Summary(from, to string) ([]UsageRecord, error) {
	rows, err := s.db.Query(`
		SELECT date, agent, '' as model, '' as provider, '' as session, '' as harness,
			SUM(input_tokens) as input_tokens, SUM(output_tokens) as output_tokens,
			SUM(requests) as requests, SUM(cost_usd) as cost_usd
		FROM usage
		WHERE date >= ? AND date <= ?
		GROUP BY date, agent
		ORDER BY date DESC, agent
	`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []UsageRecord
	for rows.Next() {
		var r UsageRecord
		if err := rows.Scan(&r.Date, &r.Agent, &r.Model, &r.Provider, &r.Session, &r.Harness, &r.InputTokens, &r.OutputTokens, &r.Requests, &r.CostUSD); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// TotalCost returns total cost for a date range, optionally filtered by agent.
func (s *Store) TotalCost(from, to, agent string) (float64, error) {
	query := `SELECT COALESCE(SUM(cost_usd), 0) FROM usage WHERE date >= ? AND date <= ?`
	args := []interface{}{from, to}
	if agent != "" {
		query += ` AND agent = ?`
		args = append(args, agent)
	}
	var total float64
	err := s.db.QueryRow(query, args...).Scan(&total)
	return total, err
}

// Print formats usage records for display.
func Print(records []UsageRecord) string {
	if len(records) == 0 {
		return "No usage recorded."
	}

	result := fmt.Sprintf("%-12s %-14s %-30s %10s %10s %6s %8s\n",
		"Date", "Agent", "Model", "In", "Out", "Reqs", "Cost")
	result += "────────────────────────────────────────────────────────────────────────────────────────────\n"

	var totalIn, totalOut int64
	var totalCost float64
	var totalReqs int

	for _, r := range records {
		model := r.Model
		if model == "" {
			model = "(all)"
		}
		result += fmt.Sprintf("%-12s %-14s %-30s %10d %10d %6d %7s\n",
			r.Date, r.Agent, model, r.InputTokens, r.OutputTokens, r.Requests,
			fmt.Sprintf("$%.2f", r.CostUSD))
		totalIn += r.InputTokens
		totalOut += r.OutputTokens
		totalCost += r.CostUSD
		totalReqs += r.Requests
	}

	result += "────────────────────────────────────────────────────────────────────────────────────────────\n"
	result += fmt.Sprintf("%-12s %-14s %-30s %10d %10d %6d %7s\n",
		"", "TOTAL", "", totalIn, totalOut, totalReqs, fmt.Sprintf("$%.2f", totalCost))

	return result
}
