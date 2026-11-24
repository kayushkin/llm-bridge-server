package usagestore

import (
	"path/filepath"
	"testing"
)

// newTestStore opens a fresh store backed by a temp-file SQLite DB.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// insertRow writes a usage row directly so the test controls the date
// (Track() always stamps time.Now()).
func insertRow(t *testing.T, s *Store, r UsageRecord) {
	t.Helper()
	if r.Orchestrator == "" {
		r.Orchestrator = "inber"
	}
	_, err := s.db.Exec(`
		INSERT INTO usage (date, agent, orchestrator, model, provider, session, harness, input_tokens, output_tokens, requests, cost_usd)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Date, r.Agent, r.Orchestrator, r.Model, r.Provider, r.Session, r.Harness,
		r.InputTokens, r.OutputTokens, r.Requests, r.CostUSD)
	if err != nil {
		t.Fatalf("insertRow: %v", err)
	}
}

// seed inserts a known fixture: 4 rows across 2 agents, 2 models, 3 dates.
func seed(t *testing.T, s *Store) {
	t.Helper()
	rows := []UsageRecord{
		{Date: "2026-01-01", Agent: "alice", Model: "opus", Provider: "anthropic", Session: "s1", Harness: "claude_code", InputTokens: 100, OutputTokens: 10, Requests: 1, CostUSD: 1.50},
		{Date: "2026-01-02", Agent: "alice", Model: "sonnet", Provider: "anthropic", Session: "s2", Harness: "claude_code", InputTokens: 200, OutputTokens: 20, Requests: 2, CostUSD: 0.75},
		{Date: "2026-01-02", Agent: "bob", Model: "opus", Provider: "anthropic", Session: "s3", Harness: "codex", InputTokens: 300, OutputTokens: 30, Requests: 1, CostUSD: 3.00},
		{Date: "2026-01-03", Agent: "bob", Model: "gpt", Provider: "openai", Session: "s4", Harness: "codex", InputTokens: 400, OutputTokens: 40, Requests: 4, CostUSD: 2.25},
	}
	for _, r := range rows {
		insertRow(t, s, r)
	}
}

func TestTrack(t *testing.T) {
	s := newTestStore(t)

	if err := s.Track("alice", "", "opus", 1000, 500, 0, nil, "sess1", "claude_code"); err != nil {
		t.Fatalf("Track: %v", err)
	}
	recs, err := s.Query(QueryFilter{Agent: "alice"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.Orchestrator != "inber" {
		t.Errorf("default orchestrator: want inber, got %q", r.Orchestrator)
	}
	if r.InputTokens != 1000 || r.OutputTokens != 500 || r.Requests != 1 {
		t.Errorf("tokens/requests: got in=%d out=%d req=%d", r.InputTokens, r.OutputTokens, r.Requests)
	}

	// Second Track with same key (same date/agent/orch/model/session) upserts.
	if err := s.Track("alice", "", "opus", 1000, 500, 1.25, nil, "sess1", "claude_code"); err != nil {
		t.Fatalf("Track upsert: %v", err)
	}
	recs, _ = s.Query(QueryFilter{Agent: "alice"})
	if len(recs) != 1 {
		t.Fatalf("upsert should merge into 1 row, got %d", len(recs))
	}
	if recs[0].InputTokens != 2000 || recs[0].OutputTokens != 1000 || recs[0].Requests != 2 {
		t.Errorf("upsert accumulation: got in=%d out=%d req=%d",
			recs[0].InputTokens, recs[0].OutputTokens, recs[0].Requests)
	}
	if recs[0].CostUSD != 1.25 {
		t.Errorf("upsert cost: want 1.25, got %v", recs[0].CostUSD)
	}
}

// fakePricing implements ModelPricing for cost-calculation tests.
type fakePricing struct {
	in, out float64
	prov    string
}

func (p fakePricing) InputCost() float64  { return p.in }
func (p fakePricing) OutputCost() float64 { return p.out }
func (p fakePricing) Provider() string    { return p.prov }

func TestTrackPricingCost(t *testing.T) {
	s := newTestStore(t)

	// 2M input @ $3/M + 1M output @ $15/M = $6 + $15 = $21
	pricing := fakePricing{in: 3, out: 15, prov: "anthropic"}
	if err := s.Track("alice", "", "opus", 2_000_000, 1_000_000, 0, pricing, "s1", "cc"); err != nil {
		t.Fatalf("Track: %v", err)
	}
	recs, _ := s.Query(QueryFilter{Agent: "alice"})
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].CostUSD != 21 {
		t.Errorf("computed cost: want 21, got %v", recs[0].CostUSD)
	}
	if recs[0].Provider != "anthropic" {
		t.Errorf("provider: want anthropic, got %q", recs[0].Provider)
	}

	// Explicit costUSD overrides pricing computation.
	s2 := newTestStore(t)
	if err := s2.Track("bob", "", "opus", 2_000_000, 1_000_000, 9.99, pricing, "s1", "cc"); err != nil {
		t.Fatalf("Track explicit cost: %v", err)
	}
	recs, _ = s2.Query(QueryFilter{Agent: "bob"})
	if recs[0].CostUSD != 9.99 {
		t.Errorf("explicit cost should win: want 9.99, got %v", recs[0].CostUSD)
	}
}

func TestQueryFiltering(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)

	cases := []struct {
		name   string
		filter QueryFilter
		want   int
	}{
		{"no filter", QueryFilter{}, 4},
		{"by agent", QueryFilter{Agent: "alice"}, 2},
		{"by model", QueryFilter{Model: "opus"}, 2},
		{"by agent+model", QueryFilter{Agent: "bob", Model: "opus"}, 1},
		{"by session", QueryFilter{Session: "s4"}, 1},
		{"by harness", QueryFilter{Harness: "codex"}, 2},
		{"date from", QueryFilter{DateFrom: "2026-01-02"}, 3},
		{"date to", QueryFilter{DateTo: "2026-01-02"}, 3},
		{"date range", QueryFilter{DateFrom: "2026-01-02", DateTo: "2026-01-02"}, 2},
		{"no match", QueryFilter{Agent: "nobody"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recs, err := s.Query(c.filter)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(recs) != c.want {
				t.Errorf("want %d records, got %d", c.want, len(recs))
			}
		})
	}
}

func TestQueryOrdering(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)

	recs, err := s.Query(QueryFilter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// ORDER BY date DESC, agent, model
	if recs[0].Date != "2026-01-03" {
		t.Errorf("first row should be newest date, got %q", recs[0].Date)
	}
	for i := 1; i < len(recs); i++ {
		if recs[i-1].Date < recs[i].Date {
			t.Errorf("dates not in DESC order at %d: %q before %q", i, recs[i-1].Date, recs[i].Date)
		}
	}
}

func TestStatsAggregation(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)

	st, err := s.Stats(QueryFilter{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	// Totals across all 4 rows.
	if st.TotalInput != 1000 { // 100+200+300+400
		t.Errorf("TotalInput: want 1000, got %d", st.TotalInput)
	}
	if st.TotalOutput != 100 { // 10+20+30+40
		t.Errorf("TotalOutput: want 100, got %d", st.TotalOutput)
	}
	if st.TotalRequests != 8 { // 1+2+1+4
		t.Errorf("TotalRequests: want 8, got %d", st.TotalRequests)
	}
	if got := round2(st.TotalCostUSD); got != 7.50 { // 1.50+0.75+3.00+2.25
		t.Errorf("TotalCostUSD: want 7.50, got %v", got)
	}

	// Distinct columns, sorted ascending.
	wantEq(t, "models", st.Models, []string{"gpt", "opus", "sonnet"})
	wantEq(t, "agents", st.Agents, []string{"alice", "bob"})
	wantEq(t, "harnesses", st.Harnesses, []string{"claude_code", "codex"})
	wantEq(t, "sessions", st.Sessions, []string{"s1", "s2", "s3", "s4"})
}

func TestStatsFiltered(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)

	st, err := s.Stats(QueryFilter{Agent: "alice"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.TotalInput != 300 { // 100+200
		t.Errorf("filtered TotalInput: want 300, got %d", st.TotalInput)
	}
	if got := round2(st.TotalCostUSD); got != 2.25 { // 1.50+0.75
		t.Errorf("filtered TotalCostUSD: want 2.25, got %v", got)
	}
	wantEq(t, "models", st.Models, []string{"opus", "sonnet"})
	wantEq(t, "agents", st.Agents, []string{"alice"})
}

func TestStatsEmpty(t *testing.T) {
	s := newTestStore(t)

	st, err := s.Stats(QueryFilter{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	// COALESCE guards mean an empty table returns zeroes, not nulls.
	if st.TotalInput != 0 || st.TotalOutput != 0 || st.TotalRequests != 0 || st.TotalCostUSD != 0 {
		t.Errorf("empty stats should be zero, got %+v", st)
	}
	if len(st.Models) != 0 || len(st.Agents) != 0 {
		t.Errorf("empty stats should have no distinct values, got models=%v agents=%v", st.Models, st.Agents)
	}
}

func TestSummary(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)

	recs, err := s.Summary("2026-01-01", "2026-01-03")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	// GROUP BY date, agent → (01-01 alice), (01-02 alice), (01-02 bob), (01-03 bob) = 4 groups.
	if len(recs) != 4 {
		t.Fatalf("want 4 grouped rows, got %d", len(recs))
	}
	// ORDER BY date DESC, agent → newest first.
	if recs[0].Date != "2026-01-03" || recs[0].Agent != "bob" {
		t.Errorf("first group: want 2026-01-03/bob, got %q/%q", recs[0].Date, recs[0].Agent)
	}
	// Grouped rows blank out model/provider/session/harness.
	if recs[0].Model != "" || recs[0].Harness != "" {
		t.Errorf("summary rows should blank model/harness, got model=%q harness=%q", recs[0].Model, recs[0].Harness)
	}

	// Find the 2026-01-02 alice group (single row: sonnet).
	var found bool
	for _, r := range recs {
		if r.Date == "2026-01-02" && r.Agent == "alice" {
			found = true
			if r.InputTokens != 200 || r.Requests != 2 || round2(r.CostUSD) != 0.75 {
				t.Errorf("01-02 alice agg: got in=%d req=%d cost=%v", r.InputTokens, r.Requests, r.CostUSD)
			}
		}
	}
	if !found {
		t.Error("missing 2026-01-02 alice group")
	}
}

func TestSummaryGroupsMultipleModels(t *testing.T) {
	s := newTestStore(t)
	// Two models same date+agent should collapse to one summary row with summed tokens.
	insertRow(t, s, UsageRecord{Date: "2026-02-01", Agent: "alice", Model: "opus", Session: "a", InputTokens: 100, OutputTokens: 10, Requests: 1, CostUSD: 1.0})
	insertRow(t, s, UsageRecord{Date: "2026-02-01", Agent: "alice", Model: "sonnet", Session: "b", InputTokens: 50, OutputTokens: 5, Requests: 3, CostUSD: 0.5})

	recs, err := s.Summary("2026-02-01", "2026-02-01")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 collapsed group, got %d", len(recs))
	}
	r := recs[0]
	if r.InputTokens != 150 || r.OutputTokens != 15 || r.Requests != 4 || round2(r.CostUSD) != 1.5 {
		t.Errorf("collapsed agg: got in=%d out=%d req=%d cost=%v", r.InputTokens, r.OutputTokens, r.Requests, r.CostUSD)
	}
}

func TestSummaryDateBounds(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)

	// Range excludes 2026-01-03.
	recs, err := s.Summary("2026-01-01", "2026-01-02")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	for _, r := range recs {
		if r.Date > "2026-01-02" || r.Date < "2026-01-01" {
			t.Errorf("date %q out of requested bounds", r.Date)
		}
	}
	// 01-01 alice, 01-02 alice, 01-02 bob = 3 groups.
	if len(recs) != 3 {
		t.Errorf("want 3 groups in range, got %d", len(recs))
	}
}

func TestTotalCost(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)

	cases := []struct {
		name           string
		from, to, agent string
		want           float64
	}{
		{"all rows", "2026-01-01", "2026-01-03", "", 7.50},
		{"agent alice", "2026-01-01", "2026-01-03", "alice", 2.25},
		{"agent bob", "2026-01-01", "2026-01-03", "bob", 5.25},
		{"date range subset", "2026-01-02", "2026-01-02", "", 3.75}, // 0.75+3.00
		{"agent within range", "2026-01-02", "2026-01-02", "alice", 0.75},
		{"empty range", "2025-12-01", "2025-12-31", "", 0},
		{"unknown agent", "2026-01-01", "2026-01-03", "nobody", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.TotalCost(c.from, c.to, c.agent)
			if err != nil {
				t.Fatalf("TotalCost: %v", err)
			}
			if round2(got) != c.want {
				t.Errorf("want %v, got %v", c.want, round2(got))
			}
		})
	}
}

// round2 rounds to 2 decimals to dodge float noise in cost sums.
func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

func wantEq(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: want %v, got %v", label, want, got)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: want %v, got %v", label, want, got)
			return
		}
	}
}
