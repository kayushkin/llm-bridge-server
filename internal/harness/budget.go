package harness

import (
	"fmt"
	"log"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The spend halt gate.
//
// Until this existed, the only cap on what a session could spend was the
// --max-budget-usd flag handed to the Claude Code binary. Every other
// harness had none, and no path other than a fresh chat started in the UI
// set even that one: CreateSessionRequest.MaxBudget and
// ConfigSessionRequest.MaxBudget were declared on the wire and read by no
// server code at all. An autoworker fleet running with auto-allow could
// therefore spend without any server-side stop. Measured against this
// box's own log-store on 2026-07-31: 3,617 sessions have spent $4,766 of
// API-priced credit, the largest single session reached $83.77, and a
// $10-per-session ceiling would have stopped $503 of that. Two of the
// twelve largest spenders were unattended autoworker sessions.
//
// The gate has two halves, and the second is the one that matters:
//
//   - Mid-turn, enforceBudget interrupts a session that has reached its
//     ceiling. This is best effort. It sends the same SIGINT the user's
//     own Interrupt button sends, so the harness settles normally and the
//     transcript survives; a harness that ignores the signal keeps going,
//     and gets interrupted again on its next API call.
//   - Between turns, the HTTP layer refuses to send to, start, or resume a
//     session whose recorded spend has reached its ceiling. That half is
//     deterministic: it needs no cooperation from the harness, and it
//     holds across a bridge-server restart because the spend it reads is
//     persisted and monotonic.

// enforceBudget records the session's spend and halts it if the spend has
// reached the session's ceiling. Called for every derived event; returns
// immediately for anything that is not a spend total.
//
// Sessions with no ceiling (max_budget_usd <= 0, which is every session
// that predates the ceiling and every caller that does not ask for one)
// still get their spend recorded — the number is worth having whether or
// not anything gates on it — and are never halted.
func (m *Manager) enforceBudget(bridgeID string, ev *msg.Event) {
	if ev.Type != msg.EventAPISpendTotal || ev.APISpendTotal == nil {
		return
	}

	spendUSD, err := m.store.RecordSessionSpendUSD(bridgeID, ev.APISpendTotal.TotalUSD)
	if err != nil {
		log.Printf("[harness] budget: failed to record spend for %s: %v", bridgeID, err)
		return
	}

	sess, err := m.store.GetSession(bridgeID)
	if err != nil {
		log.Printf("[harness] budget: failed to read session %s: %v", bridgeID, err)
		return
	}
	if sess == nil || sess.MaxBudgetUSD <= 0 {
		return
	}

	if spendUSD < sess.MaxBudgetUSD {
		// Under the ceiling. Clear any earlier breach so that raising the
		// ceiling genuinely revives the session: without this, a session
		// halted at $5 and then raised to $50 would never announce the
		// second breach, because the gate would still believe it had
		// already reported one.
		m.mu.Lock()
		delete(m.budgetHalted, bridgeID)
		m.mu.Unlock()
		return
	}

	m.haltForBudget(bridgeID, spendUSD, sess.MaxBudgetUSD)
}

// haltForBudget announces the breach once and interrupts the session every
// time it is called.
//
// The asymmetry is deliberate. The error event is a statement about the
// session crossing its ceiling, and it crosses once; repeating it on every
// subsequent API call would bury the conversation under duplicates. The
// interrupt is an attempt to stop a process that is still spending money,
// and an attempt that did not work is worth repeating.
func (m *Manager) haltForBudget(bridgeID string, spendUSD, maxBudgetUSD float64) {
	m.mu.Lock()
	firstBreach := !m.budgetHalted[bridgeID]
	m.budgetHalted[bridgeID] = true
	m.mu.Unlock()

	if firstBreach {
		log.Printf("[harness] budget: session %s reached its ceiling ($%.2f spent, ceiling $%.2f) — interrupting", bridgeID, spendUSD, maxBudgetUSD)
		// Emitted through broadcastDerived rather than BroadcastEvent: the
		// latter feeds the event back into deriveAndBroadcast, which is
		// where this call came from, and re-entering the derivation from
		// inside its own output loop is a trap not worth setting.
		errorEvent := msg.Event{
			Type:            msg.EventError,
			BridgeSessionID: bridgeID,
			Timestamp:       time.Now(),
			Error: &msg.ErrorEvent{
				Code:    msg.ErrCodeBudgetExceeded,
				Message: fmt.Sprintf("session halted: spent $%.2f of its $%.2f ceiling. Raise max_budget to continue.", spendUSD, maxBudgetUSD),
			},
		}
		m.broadcastDerived(bridgeID, &errorEvent)
	}

	if err := m.Stop(bridgeID); err != nil {
		// "session not running" is the ordinary case for a session whose
		// process has already exited — the spend event can arrive after
		// the turn settled. Logged, not escalated: the refusal half of
		// the gate is what stops it running again.
		log.Printf("[harness] budget: interrupt of over-budget session %s did not land: %v", bridgeID, err)
	}
}

// SessionOverBudget reports whether the session has spent its ceiling, and
// with it the numbers a caller needs to say so.
//
// This is the durable half of the gate: it reads the persisted spend and
// ceiling, so it holds for a session whose process is not running, and it
// holds across a bridge-server restart. The HTTP layer calls it before
// sending to, starting, or resuming a session.
//
// A session with no ceiling is never over budget.
func (m *Manager) SessionOverBudget(bridgeID string) (over bool, spendUSD, maxBudgetUSD float64) {
	sess, err := m.store.GetSession(bridgeID)
	if err != nil || sess == nil {
		// Fail open. A session row we cannot read is not evidence of
		// overspending, and refusing every send whenever the store
		// hiccups would be a worse failure than the one being prevented.
		if err != nil {
			log.Printf("[harness] budget: failed to read session %s for the over-budget check: %v", bridgeID, err)
		}
		return false, 0, 0
	}
	if sess.MaxBudgetUSD <= 0 {
		return false, sess.SpendUSD, 0
	}
	return sess.SpendUSD >= sess.MaxBudgetUSD, sess.SpendUSD, sess.MaxBudgetUSD
}
