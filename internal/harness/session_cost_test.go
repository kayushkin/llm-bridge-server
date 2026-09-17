package harness

import (
	"math"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func apiCall(usd float64) *msg.Event {
	return &msg.Event{Type: msg.EventAPICall, APICall: &msg.APICallEvent{Model: "m", QuerySource: "sdk", CostUSD: usd}}
}

func pricedResult(turnUSD float64) *msg.Event {
	return &msg.Event{Type: msg.EventResult, Result: &msg.ResultEvent{Text: "ok", Cost: &msg.Cost{TotalUSD: turnUSD}}}
}

func lastSessionCost(t *testing.T, out []msg.Event) *msg.SessionCostEvent {
	t.Helper()
	var last *msg.SessionCostEvent
	for _, ev := range out {
		if ev.Type == msg.EventSessionCost {
			last = ev.SessionCost
		}
	}
	if last == nil {
		t.Fatalf("no session_cost in %d derived events", len(out))
	}
	return last
}

// Within one process the estimate is the larger of API spend and turn results;
// before the process it is what the session was recorded as costing.
func TestSessionCost_TakesTheLargerLowerBoundPerProcess(t *testing.T) {
	d := newDerivationState()
	d.seedAPISpend(10, 5, msg.TokenUsage{}, nil, nil)
	d.seedSessionCost(12, 11)

	// API spend leads: two calls, no result yet.
	d.derive(apiCall(0.30))
	cost := lastSessionCost(t, d.derive(apiCall(0.20)))
	if !near(cost.TotalUSD, 12.50) || !near(cost.APISpendUSD, 10.50) || !near(cost.TurnResultUSD, 11) {
		t.Fatalf("after calls: %+v; want total 12.50, api 10.50, results 11", cost)
	}

	// The turn's result reports more than the calls did (a call's telemetry was
	// lost): results lead.
	cost = lastSessionCost(t, d.derive(pricedResult(0.80)))
	if !near(cost.TotalUSD, 12.80) || !near(cost.TurnResultUSD, 11.80) {
		t.Fatalf("after result: %+v; want total 12.80, results 11.80", cost)
	}

	// The late telemetry for that turn's last call arrives after the result. It is
	// already inside the result's 0.80, so it must not be added on top.
	cost = lastSessionCost(t, d.derive(apiCall(0.25)))
	if !near(cost.TotalUSD, 12.80) {
		t.Fatalf("late call counted twice: total %.2f, want 12.80", cost.TotalUSD)
	}

	// A turn that never produces a result keeps spending through calls; API spend
	// overtakes the results again.
	cost = lastSessionCost(t, d.derive(apiCall(0.40)))
	if !near(cost.TotalUSD, 13.15) || !near(cost.APISpendUSD, 11.15) {
		t.Fatalf("after unresulted calls: %+v; want total 13.15", cost)
	}
}

// A new process starts from the recorded cost, not from either input, and the
// estimate never goes down.
func TestSessionCost_ANewProcessStartsFromTheRecordedCost(t *testing.T) {
	d := newDerivationState()
	d.seedAPISpend(3.00, 0, msg.TokenUsage{}, nil, nil)
	d.seedSessionCost(3.60, 3.60)

	cost := lastSessionCost(t, d.derive(apiCall(0.10)))
	if !near(cost.TotalUSD, 3.70) {
		t.Fatalf("total %.2f, want 3.70 (recorded 3.60 + this process's 0.10)", cost.TotalUSD)
	}
	// A zero-cost priced result cannot lower it.
	cost = lastSessionCost(t, d.derive(pricedResult(0.05)))
	if !near(cost.TotalUSD, 3.70) {
		t.Fatalf("total %.2f after a smaller result, want 3.70", cost.TotalUSD)
	}
}

// An unpriced result emits no session_cost: nothing about the cost moved.
func TestSessionCost_AnUnpricedResultEmitsNothing(t *testing.T) {
	d := newDerivationState()
	for _, ev := range d.derive(&msg.Event{Type: msg.EventResult, Result: &msg.ResultEvent{Text: "ok"}}) {
		if ev.Type == msg.EventSessionCost {
			t.Fatalf("unpriced result emitted %+v", ev.SessionCost)
		}
	}
}
