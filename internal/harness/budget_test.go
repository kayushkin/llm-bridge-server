package harness

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// interruptRecorder is a HarnessProcess that counts the interrupts the
// budget gate sends it. fakeProcess swallows them, and "did the gate
// actually try to stop the process" is the thing these tests are about.
type interruptRecorder struct {
	sid string
	ch  chan msg.Event

	mu         sync.Mutex
	interrupts int
	kills      int
}

func (p *interruptRecorder) PID() int                                             { return 0 }
func (p *interruptRecorder) SessionID() string                                    { return p.sid }
func (p *interruptRecorder) Events() <-chan msg.Event                             { return p.ch }
func (p *interruptRecorder) Send(message string, blocks []msg.ContentBlock) error { return nil }
func (p *interruptRecorder) SendCommand(cmd string) error                         { return nil }
func (p *interruptRecorder) SendJSONRPC(method string, params json.RawMessage) error {
	return nil
}

func (p *interruptRecorder) Interrupt() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.interrupts++
	return nil
}

func (p *interruptRecorder) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.kills++
	return nil
}

func (p *interruptRecorder) interruptCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.interrupts
}

func (p *interruptRecorder) killCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.kills
}

// budgetFixture wires a session with a ceiling to a live registered
// process, and returns both plus the manager.
func budgetFixture(t *testing.T, bridgeID string, maxBudgetUSD float64) (*Manager, *interruptRecorder) {
	t.Helper()
	m := newTestManager(t)
	if err := m.store.CreateSession(&store.Session{
		SessionID:    bridgeID,
		Harness:      "claude_code",
		State:        string(msg.SessionRunning),
		MaxBudgetUSD: maxBudgetUSD,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	proc := &interruptRecorder{sid: bridgeID, ch: make(chan msg.Event)}
	m.mu.Lock()
	m.processes[bridgeID] = proc
	m.mu.Unlock()
	return m, proc
}

// spend drives one API call of costUSD through the derivation, which is
// what produces the api_spend_total the gate reads.
func spend(t *testing.T, m *Manager, bridgeID string, costUSD float64) {
	t.Helper()
	m.deriveAndBroadcast(bridgeID, &msg.Event{
		Type:            msg.EventAPICall,
		BridgeSessionID: bridgeID,
		APICall: &msg.APICallEvent{
			Model: "claude-opus-4-7", InputTokens: 10, OutputTokens: 5,
			CostUSD: costUSD, QuerySource: "sdk",
		},
	})
}

// budgetErrors returns the budget_exceeded errors persisted for a session.
func budgetErrors(t *testing.T, m *Manager, bridgeID string) []msg.Event {
	t.Helper()
	rows, err := m.store.ListEventsSinceID(bridgeID, 0)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	var out []msg.Event
	for _, raw := range rows {
		var ev msg.Event
		if err := json.Unmarshal(raw.Data, &ev); err != nil {
			continue
		}
		if ev.Type == msg.EventError && ev.Error != nil && ev.Error.Code == msg.ErrCodeBudgetExceeded {
			out = append(out, ev)
		}
	}
	return out
}

func TestBudget_HaltsSessionThatReachesItsCeiling(t *testing.T) {
	m, proc := budgetFixture(t, "br_budget_halt", 1.00)

	spend(t, m, "br_budget_halt", 0.40)
	if got := proc.interruptCount(); got != 0 {
		t.Fatalf("interrupts after $0.40 of a $1.00 ceiling = %d; want 0", got)
	}
	if over, _, _ := m.SessionOverBudget("br_budget_halt"); over {
		t.Fatal("session reported over budget at $0.40 of $1.00")
	}

	spend(t, m, "br_budget_halt", 0.70) // cumulative $1.10, past the ceiling
	if got := proc.interruptCount(); got != 1 {
		t.Errorf("interrupts after crossing the ceiling = %d; want 1", got)
	}
	if got := proc.killCount(); got != 0 {
		t.Errorf("kills = %d; want 0 — the gate interrupts, it does not kill", got)
	}

	errs := budgetErrors(t, m, "br_budget_halt")
	if len(errs) != 1 {
		t.Fatalf("budget_exceeded errors = %d; want 1", len(errs))
	}

	over, spendUSD, maxUSD := m.SessionOverBudget("br_budget_halt")
	if !over {
		t.Error("SessionOverBudget = false after crossing the ceiling; want true")
	}
	if maxUSD != 1.00 {
		t.Errorf("ceiling = %v; want 1.00", maxUSD)
	}
	if spendUSD < 1.09 || spendUSD > 1.11 {
		t.Errorf("spend = %v; want ~1.10", spendUSD)
	}
}

func TestBudget_CeilingIsReachedNotExceeded(t *testing.T) {
	// Spending exactly the ceiling has spent the ceiling. A gate that only
	// fires strictly above it lets the last call through and then waits
	// for a call that may never come, which on a session that stops right
	// there means the halt never happens at all.
	m, proc := budgetFixture(t, "br_budget_exact", 2.00)

	spend(t, m, "br_budget_exact", 2.00)

	if got := proc.interruptCount(); got != 1 {
		t.Errorf("interrupts at exactly the ceiling = %d; want 1", got)
	}
	if over, _, _ := m.SessionOverBudget("br_budget_exact"); !over {
		t.Error("SessionOverBudget = false at exactly the ceiling; want true")
	}
}

func TestBudget_NoCeilingNeverHalts(t *testing.T) {
	// The overwhelming majority of sessions on this box have no ceiling,
	// including every session that predates the column. Spending freely
	// is the correct behaviour for all of them.
	m, proc := budgetFixture(t, "br_budget_none", 0)

	for i := 0; i < 5; i++ {
		spend(t, m, "br_budget_none", 25.00)
	}

	if got := proc.interruptCount(); got != 0 {
		t.Errorf("interrupts on an uncapped session = %d; want 0", got)
	}
	if errs := budgetErrors(t, m, "br_budget_none"); len(errs) != 0 {
		t.Errorf("budget_exceeded errors on an uncapped session = %d; want 0", len(errs))
	}
	if over, _, _ := m.SessionOverBudget("br_budget_none"); over {
		t.Error("uncapped session reported over budget")
	}

	// Spend is still recorded for it — the number is worth having whether
	// or not anything gates on it.
	sess, err := m.store.GetSession("br_budget_none")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.SpendUSD < 124.9 {
		t.Errorf("recorded spend = %v; want ~125", sess.SpendUSD)
	}
}

func TestBudget_AnnouncesOnceButKeepsInterrupting(t *testing.T) {
	// A harness that ignores SIGINT keeps spending. The error event is a
	// statement about crossing the ceiling and belongs in the transcript
	// once; the interrupt is an attempt to stop a process that is still
	// burning money and is worth repeating.
	m, proc := budgetFixture(t, "br_budget_stubborn", 1.00)

	spend(t, m, "br_budget_stubborn", 1.50)
	spend(t, m, "br_budget_stubborn", 0.50)
	spend(t, m, "br_budget_stubborn", 0.50)

	if got := len(budgetErrors(t, m, "br_budget_stubborn")); got != 1 {
		t.Errorf("budget_exceeded errors = %d; want exactly 1", got)
	}
	if got := proc.interruptCount(); got != 3 {
		t.Errorf("interrupts = %d; want 3 — one per over-ceiling API call", got)
	}
}

func TestBudget_RaisingTheCeilingRevivesTheSessionAndRearmsTheGate(t *testing.T) {
	// The escape hatch. Raising the ceiling above the spend must both let
	// the session run again AND leave the gate able to announce the next
	// breach — a gate that fires once per process lifetime would let the
	// second overrun pass in silence.
	m, proc := budgetFixture(t, "br_budget_raise", 1.00)

	spend(t, m, "br_budget_raise", 1.50)
	if over, _, _ := m.SessionOverBudget("br_budget_raise"); !over {
		t.Fatal("not over budget after spending $1.50 of $1.00")
	}
	if got := len(budgetErrors(t, m, "br_budget_raise")); got != 1 {
		t.Fatalf("first breach errors = %d; want 1", got)
	}

	if err := m.store.SetSessionMaxBudgetUSD("br_budget_raise", 10.00); err != nil {
		t.Fatalf("raise ceiling: %v", err)
	}
	if over, _, _ := m.SessionOverBudget("br_budget_raise"); over {
		t.Error("still over budget after the ceiling was raised to $10")
	}

	// Under the new ceiling: no further interrupts.
	interruptsBefore := proc.interruptCount()
	spend(t, m, "br_budget_raise", 1.00) // cumulative $2.50 of $10
	if got := proc.interruptCount(); got != interruptsBefore {
		t.Errorf("interrupts while under the raised ceiling = %d; want %d", got, interruptsBefore)
	}

	// Past the new ceiling: announced again.
	spend(t, m, "br_budget_raise", 10.00)
	if got := len(budgetErrors(t, m, "br_budget_raise")); got != 2 {
		t.Errorf("budget_exceeded errors after the second breach = %d; want 2", got)
	}
}

func TestBudget_RecordedSpendNeverFallsBack(t *testing.T) {
	// The running total the gate is fed comes from an in-memory
	// derivation that restarts at zero with the process. If the persisted
	// spend followed it down, a restart would hand an exhausted budget a
	// fresh full allowance — the failure this MAX() exists to prevent.
	m, _ := budgetFixture(t, "br_budget_restart", 5.00)

	spend(t, m, "br_budget_restart", 6.00)
	if over, _, _ := m.SessionOverBudget("br_budget_restart"); !over {
		t.Fatal("not over budget after spending $6.00 of $5.00")
	}

	// Simulate the restart: a brand-new derivation for the same session,
	// whose first api_spend_total reports a total starting from zero.
	m.mu.Lock()
	delete(m.derivation, "br_budget_restart")
	delete(m.budgetHalted, "br_budget_restart")
	m.mu.Unlock()

	spend(t, m, "br_budget_restart", 0.10) // post-restart total is $0.10

	over, spendUSD, _ := m.SessionOverBudget("br_budget_restart")
	if !over {
		t.Error("session came back under budget after a restart; the spend high-water mark was lost")
	}
	if spendUSD < 6.0 {
		t.Errorf("recorded spend = %v after restart; want it held at >= 6.00", spendUSD)
	}
}
