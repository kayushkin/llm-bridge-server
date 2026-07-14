package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/slava/scheduler/internal/kanbanclient"
)

// The spend ceiling ("stop this card once it has cost me $N") is enforced at two
// points, and it needs both.
//
// The per-session cap below is the real-time half: the dispatcher hands the
// harness a budget and Claude Code stops ITSELF when it hits it, mid-run. The
// cumulative half — the curator that holds the card once its total reaches the
// ceiling — can only ever run on a poll, and polling alone is far too slow to
// enforce a dollar cap. Measured against this host's own session history: the
// median session burns $0.37/min and the fastest observed burned $29.22/min, so
// a 5-minute poll would overshoot a ceiling by ~$1.83 in the normal case and by
// ~$146 in the worst. A cap that can be overrun by two orders of magnitude is
// not a cap.
//
// So the dispatcher caps each session at (ceiling - already spent). No single
// session can push the card past its ceiling, which is precisely what makes the
// polled cumulative check safe: the poll only ever has to notice a breach that
// the harness already stopped at, never race one in progress.

// sessionAggregate is the slice of llm-bridge's SessionAggregate we need. Cost
// lives there, not on the card — the card stores the ceiling, never the spend,
// so there is no second copy to go stale.
type sessionAggregate struct {
	SessionID string  `json:"session_id"`
	CostUSD   float64 `json:"cost_usd"`
}

// cardSpendUSD totals what every session ever linked to this card has cost.
//
// Cumulative across sessions, not per session, because the dispatcher revives
// cards: a card retried three times under a $5 per-session cap has still cost
// $15, and "this task has cost me $15" is what the ceiling is asked to mean.
//
// A session with no aggregate row contributes 0 — it has not billed a turn yet.
func cardSpendUSD(qc *kanbanclient.Client, bridgeURL, cardID string) (float64, error) {
	links, err := qc.ListCardLinks(cardID)
	if err != nil {
		return 0, fmt.Errorf("list card links: %w", err)
	}
	linked := map[string]bool{}
	for _, l := range links {
		if l.EntityType == "session" {
			linked[l.EntityRef] = true
		}
	}
	if len(linked) == 0 {
		return 0, nil
	}

	aggs, err := fetchSessionAggregates(bridgeURL)
	if err != nil {
		return 0, err
	}
	var total float64
	for _, a := range aggs {
		if linked[a.SessionID] {
			total += a.CostUSD
		}
	}
	return total, nil
}

func fetchSessionAggregates(bridgeURL string) ([]sessionAggregate, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(bridgeURL + "/sessions/aggregates")
	if err != nil {
		return nil, fmt.Errorf("bridge aggregates: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bridge aggregates: %s", resp.Status)
	}
	var aggs []sessionAggregate
	if err := json.NewDecoder(resp.Body).Decode(&aggs); err != nil {
		return nil, fmt.Errorf("decode aggregates: %w", err)
	}
	return aggs, nil
}

// budgetForCard resolves the per-session cap to hand the harness.
//
// Returns (cap, ok). ok=false means "do not dispatch this card": its ceiling is
// already spent. The caller holds it rather than firing a session that would be
// killed on its first turn anyway.
//
// A cap of exactly 0 is never returned as dispatchable — Claude Code treats
// max_budget_usd=0 as "no budget set" (the flag is omitted when the value is
// zero), so passing 0 would silently mean UNLIMITED. That inversion — the
// tightest possible ceiling becoming no ceiling at all — is the one failure mode
// here that spends real money, so a spent-out card is held instead.
func budgetForCard(qc *kanbanclient.Client, bridgeURL string, cand *queueCandidate) (float64, bool) {
	ceiling, ok := cardSpendCeiling(cand.item)
	if !ok {
		return 0, true // no ceiling on this card: dispatch uncapped, as before
	}

	spent, err := cardSpendUSD(qc, bridgeURL, cand.cardID)
	if err != nil {
		// Fail closed. If we cannot read what the card has already cost, we
		// cannot honour its ceiling, and guessing "probably nothing" is how a
		// capped card runs uncapped.
		log.Printf("queue: card %s has a $%.2f ceiling but its spend is unreadable (%v) — holding rather than dispatching uncapped",
			cand.cardID, ceiling, err)
		return 0, false
	}

	remaining := ceiling - spent
	if remaining <= 0 {
		log.Printf("queue: card %s has spent $%.2f of its $%.2f ceiling — holding",
			cand.cardID, spent, ceiling)
		return 0, false
	}
	log.Printf("queue: card %s spend ceiling $%.2f, spent $%.2f — capping this session at $%.2f",
		cand.cardID, ceiling, spent, remaining)
	return remaining, true
}

// cardSpendCeiling reads auto_hold_at_usd off the noteboard item the card
// carries. Returns (ceiling, true) only when the card actually has one — a
// missing key and a null both mean "no ceiling", which is the default.
func cardSpendCeiling(item map[string]any) (float64, bool) {
	raw, ok := item["auto_hold_at_usd"]
	if !ok || raw == nil {
		return 0, false
	}
	v, ok := raw.(float64)
	if !ok {
		return 0, false
	}
	return v, true
}

// envFloat reads a float from the environment, falling back to def when unset.
// A malformed value is fatal rather than silently ignored: this configures a
// SPEND CAP, and a typo that quietly became "uncapped" is the failure this whole
// mechanism exists to prevent.
func envFloat(key string, def float64) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		log.Fatalf("%s=%q is not a number — refusing to run uncapped on a typo", key, raw)
	}
	return v
}

// resolveSessionBudget decides the cost cap for one dispatched session, and says
// why. This is the money decision, so it is a function rather than a few lines
// inside the fire loop: it is the thing worth testing.
//
// Precedence:
//
//  1. The card's own ceiling. cand.maxBudgetUSD is already (ceiling - spent),
//     the only number that keeps THAT card under THAT ceiling. A fleet default
//     must never raise or lower it — not even when the default is tighter, since
//     the ceiling is the promise the user actually made about that task.
//
//  2. The fleet default, for everything the card ceiling cannot reach. Chiefly
//     the noteboard-discovery fallback, which runs whenever the Queued column is
//     empty — most nights — and has no card at all to carry a ceiling. That path
//     produced this host's most expensive unattended sessions, and it is
//     dispatched uncapped without a default.
//
//  3. Uncapped, when neither is set. This is the historical behaviour and stays
//     the default: arming a fleet-wide cap is the user's call, not ours.
//
// The returned reason is logged verbatim. An uncapped session should be able to
// say out loud why it is uncapped — silence is how a cap that never armed goes
// unnoticed for months.
func resolveSessionBudget(cand *queueCandidate, fleetDefaultUSD float64) (float64, string) {
	if cand != nil && cand.maxBudgetUSD > 0 {
		return cand.maxBudgetUSD, "the card's own spend ceiling (minus what it has already cost)"
	}
	if fleetDefaultUSD > 0 {
		if cand != nil {
			return fleetDefaultUSD, "fleet default; this card sets no ceiling of its own"
		}
		return fleetDefaultUSD, "fleet default; noteboard-discovery session, which has no card to carry a ceiling"
	}
	if cand != nil {
		return 0, "this card sets no ceiling and no fleet default is configured"
	}
	return 0, "noteboard-discovery session and no fleet default is configured (-default-max-budget-usd)"
}
