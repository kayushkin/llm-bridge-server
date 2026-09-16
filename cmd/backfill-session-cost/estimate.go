package main

// The historical form of the session cost rule in internal/harness/derivation.go
// (msg.SessionCostEvent), for events recorded before harnesses reported per-turn
// costs.
//
// Before 2026-09-17 Claude Code's result cost was its CLI process's running
// total, so a result figure LOWER than the previous one marks a new process; its
// last figure is what that process's results cost. api_spend_total is not read:
// before 2026-08-01 it restarted at zero with every process and afterwards it
// resumed from a recorded high-water mark, so differences between its values do
// not measure spend. The per-call api_call costs do.
//
// ⚠️ system/init is NOT a process boundary. Claude Code emits it on every turn;
// splitting there summed each process's running total once per turn.

// costEvent is the part of one stored event the rule reads.
type costEvent struct {
	// ResultCumulativeUSD is a result's cost, cumulative for its process; zero on
	// any other event and on an unpriced result.
	ResultCumulativeUSD float64
	// CallUSD is an api_call's cost; zero on any other event.
	CallUSD float64
}

// sessionCostEstimate is the per-process rule over a session's events in order:
// for each process, the larger of its per-call costs and its last result figure,
// summed. It also returns the two inputs over the whole session.
func sessionCostEstimate(events []costEvent) (totalUSD, callUSD, turnResultUSD float64) {
	// Calls made since the last result are held apart: a restart only shows at the
	// NEXT result, and a call before it belongs to the new process if that result
	// shows one, and to the current process if it does not.
	var processCallUSD, callsSinceResultUSD, processResultUSD float64
	for _, ev := range events {
		if ev.CallUSD > 0 {
			callsSinceResultUSD += ev.CallUSD
			callUSD += ev.CallUSD
		}
		if ev.ResultCumulativeUSD > 0 {
			if ev.ResultCumulativeUSD < processResultUSD {
				totalUSD += max(processCallUSD, processResultUSD)
				processCallUSD, processResultUSD = 0, 0
			}
			processCallUSD += callsSinceResultUSD
			callsSinceResultUSD = 0
			turnResultUSD += ev.ResultCumulativeUSD - processResultUSD
			processResultUSD = ev.ResultCumulativeUSD
		}
	}
	totalUSD += max(processCallUSD+callsSinceResultUSD, processResultUSD)
	return totalUSD, callUSD, turnResultUSD
}
