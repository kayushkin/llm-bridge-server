package harness

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// restartProcess throws the session's derivation away, which is what the
// manager does when a harness process exits — on every stop, crash, idle
// reap and bridge-server restart, not only when the session ends. The next
// event rebuilds the derivation from the persisted row.
//
// budgetHalted goes with it for the same reason the manager drops it: it is
// per-process announcement bookkeeping, not the verdict.
func restartProcess(m *Manager, bridgeID string) {
	m.mu.Lock()
	delete(m.derivation, bridgeID)
	delete(m.budgetHalted, bridgeID)
	m.mu.Unlock()
}

// lastSpendTotal returns the most recent api_spend_total the session has
// emitted — the number the gate reads and the UI's top-line cost displays.
func lastSpendTotal(t *testing.T, m *Manager, bridgeID string) *msg.APISpendTotalEvent {
	t.Helper()
	rows, err := m.store.ListEventsSinceID(bridgeID, 0)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	var last *msg.APISpendTotalEvent
	for _, raw := range rows {
		var ev msg.Event
		if err := json.Unmarshal(raw.Data, &ev); err != nil {
			continue
		}
		if ev.Type == msg.EventAPISpendTotal && ev.APISpendTotal != nil {
			last = ev.APISpendTotal
		}
	}
	if last == nil {
		t.Fatalf("session %s emitted no api_spend_total", bridgeID)
	}
	return last
}

// TestBudget_SpendContinuesAcrossAProcessRestart is the defect this seeding
// exists for, and it is the one the MAX() backstop cannot catch.
//
// TestBudget_RecordedSpendNeverFallsBack already pins that the recorded
// figure does not go DOWN across a restart. It passes whether or not the
// derivation is seeded, because it never spends anything after the restart —
// so it says nothing about whether the second run's dollars are counted.
// They were not: MAX(previous, this-run) ignores every dollar under the old
// high-water mark, so a ceiling re-armed once per resume.
//
// Measured on this box's log-store 2026-08-01: 64 of 3,691 sessions show the
// running total going backwards, $1,174 of real spend is invisible to the
// high-water mark, and one session recorded $83.77 while actually spending
// $201.76 across sixteen runs.
func TestBudget_SpendContinuesAcrossAProcessRestart(t *testing.T) {
	m, proc := budgetFixture(t, "br_spend_resume", 10.00)

	spend(t, m, "br_spend_resume", 9.00)
	if over, _, _ := m.SessionOverBudget("br_spend_resume"); over {
		t.Fatal("halted at $9.00 of a $10.00 ceiling")
	}

	restartProcess(m, "br_spend_resume")

	// $2.00 more. On its own that is far under the ceiling; added to the
	// $9.00 the session already spent it is over.
	spend(t, m, "br_spend_resume", 2.00)

	total := lastSpendTotal(t, m, "br_spend_resume")
	if total.TotalUSD < 11.0 {
		t.Errorf("api_spend_total after the restart = $%.2f; want $11.00 — the second run started its own spend history", total.TotalUSD)
	}

	over, spendUSD, _ := m.SessionOverBudget("br_spend_resume")
	if !over {
		t.Errorf("session is under budget having spent $11.00 of a $10.00 ceiling (recorded $%.2f)", spendUSD)
	}
	if proc.interruptCount() == 0 {
		t.Error("the gate never interrupted a session that spent past its ceiling on a second run")
	}
}

// TestBudget_ASpentCeilingStaysSpentAcrossManyRestarts is the shape the live
// data actually took: no single run reaches the ceiling, so a gate that only
// ever sees one run at a time never fires however many runs there are.
func TestBudget_ASpentCeilingStaysSpentAcrossManyRestarts(t *testing.T) {
	m, _ := budgetFixture(t, "br_spend_many", 10.00)

	for run := 0; run < 5; run++ {
		spend(t, m, "br_spend_many", 3.00)
		restartProcess(m, "br_spend_many")
	}

	over, spendUSD, _ := m.SessionOverBudget("br_spend_many")
	if spendUSD < 15.0 {
		t.Errorf("recorded spend after 5 runs of $3.00 = $%.2f; want $15.00", spendUSD)
	}
	if !over {
		t.Error("five $3.00 runs against a $10.00 ceiling left the session under budget")
	}
}

// TestBudget_TheSpendBreakdownContinuesAcrossAProcessRestart pins the rest of
// the aggregate, not just the dollar figure.
//
// Seeding only TotalUSD would be worse than not seeding at all: the event
// documents every one of these fields as session-cumulative, and a payload
// whose headline covers the whole session while its per-model drill-down
// covers only the current run does not add up, in the UI or anywhere else.
func TestBudget_TheSpendBreakdownContinuesAcrossAProcessRestart(t *testing.T) {
	m, _ := budgetFixture(t, "br_spend_detail", 0)

	spend(t, m, "br_spend_detail", 1.00)
	spend(t, m, "br_spend_detail", 1.00)
	restartProcess(m, "br_spend_detail")
	spend(t, m, "br_spend_detail", 1.00)

	total := lastSpendTotal(t, m, "br_spend_detail")
	if total.Calls != 3 {
		t.Errorf("calls = %d; want 3 — the count restarted with the process", total.Calls)
	}
	if got := total.ByModel["claude-opus-4-7"]; got < 3.0 {
		t.Errorf("by_model[claude-opus-4-7] = $%.2f; want $3.00", got)
	}
	if got := total.ByQuerySource["sdk"]; got < 3.0 {
		t.Errorf("by_query_source[sdk] = $%.2f; want $3.00", got)
	}
	// spend() bills 10 input and 5 output tokens per call.
	if total.Usage.InputTokens != 30 || total.Usage.OutputTokens != 15 {
		t.Errorf("usage = %d in / %d out; want 30 / 15", total.Usage.InputTokens, total.Usage.OutputTokens)
	}
	if total.TotalUSD < 3.0 {
		t.Errorf("total = $%.2f; want $3.00", total.TotalUSD)
	}
}

// TestBudget_SeedingDoesNotDoubleCountWithinOneRun guards the direction the
// fix could fail in. seedAPISpend assigns, and it runs once when the
// derivation is built; if it ran again — or added to a total it had already
// seeded — a session's spend would climb without anything being spent, and
// the gate would halt sessions that are nowhere near their ceiling.
func TestBudget_SeedingDoesNotDoubleCountWithinOneRun(t *testing.T) {
	m, proc := budgetFixture(t, "br_spend_nodouble", 10.00)

	for i := 0; i < 4; i++ {
		spend(t, m, "br_spend_nodouble", 1.00)
	}

	total := lastSpendTotal(t, m, "br_spend_nodouble")
	if total.TotalUSD > 4.0+1e-9 {
		t.Errorf("total = $%.2f after four $1.00 calls; want $4.00 — spend is being counted more than once", total.TotalUSD)
	}
	if total.Calls != 4 {
		t.Errorf("calls = %d; want 4", total.Calls)
	}
	if proc.interruptCount() != 0 {
		t.Error("a session $6.00 under its ceiling was interrupted")
	}
}
