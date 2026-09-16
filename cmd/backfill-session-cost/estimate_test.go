package main

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestSessionCostEstimate(t *testing.T) {
	events := []costEvent{
		// process 1: calls 4.00, results reach 4.10 (a call's telemetry lost)
		{CallUSD: 1.5}, {CallUSD: 2.5}, {ResultCumulativeUSD: 2.0}, {ResultCumulativeUSD: 4.1},
		// process 2 (result drops): calls 3.00, results 1.00 (an unfinished turn)
		{CallUSD: 3.0}, {ResultCumulativeUSD: 1.0},
	}
	total, calls, results := sessionCostEstimate(events)
	if !near(total, 4.1+3.0) || !near(calls, 7.0) || !near(results, 5.1) {
		t.Fatalf("total %.2f calls %.2f results %.2f; want 7.10, 7.00, 5.10", total, calls, results)
	}
}

func TestSessionCostEstimate_ARunningTotalIsNotSummedPerTurn(t *testing.T) {
	// One process, three turns: 1.00, 2.50, 3.00 cumulative, calls matching.
	events := []costEvent{
		{CallUSD: 1.0}, {ResultCumulativeUSD: 1.0},
		{CallUSD: 1.5}, {ResultCumulativeUSD: 2.5},
		{CallUSD: 0.5}, {ResultCumulativeUSD: 3.0},
	}
	if total, _, results := sessionCostEstimate(events); !near(total, 3.0) || !near(results, 3.0) {
		t.Fatalf("total %.2f results %.2f; want 3.00 and 3.00", total, results)
	}
}

func TestSessionCostEstimate_NoEventsCostNothing(t *testing.T) {
	if total, calls, results := sessionCostEstimate(nil); total != 0 || calls != 0 || results != 0 {
		t.Fatalf("empty session: %v %v %v", total, calls, results)
	}
}
