package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/boardvocabulary"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issueitemsclient"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/spendtree"
)

// The spend ceiling ("stop this task once it has cost me $N") is enforced at two
// points, and it needs both.
//
// The per-session cap below is the real-time half: the dispatcher hands the
// harness a budget and Claude Code stops ITSELF when it hits it, mid-run. The
// cumulative half — spend-ceiling-guard, which holds the task once its total
// reaches the ceiling — can only ever run on a poll, and polling alone is far too
// slow to enforce a dollar cap. Measured against this host's own session history:
// the median session burns $0.37/min and the fastest observed burned $29.22/min,
// so a 5-minute poll would overshoot a ceiling by ~$1.83 in the normal case and
// by ~$146 in the worst. A cap that can be overrun by two orders of magnitude is
// not a cap.
//
// So the dispatcher caps each session at (ceiling - already spent). No single
// session can push the task past its ceiling, which is precisely what makes the
// polled cumulative check safe: the poll only ever has to notice a breach that
// the harness already stopped at, never race one in progress.
//
// Both halves compute over the same tree (internal/spendtree), because a ceiling
// covers a task AND everything split out of it. If they used different trees the
// dispatcher would budget against one number while the guard enforced another.

type sessionAggregate struct {
	SessionID string  `json:"session_id"`
	CostUSD   float64 `json:"cost_usd"`
}

// budgetForCard resolves the per-session cap to hand the harness.
//
// Returns (cap, ok). ok=false means "do not dispatch this card": its ceiling is
// already spent. The caller holds it rather than firing a session that would be
// killed on its first turn anyway.
//
// The ceiling may be INHERITED from an ancestor, and the spend is the whole
// subtree's — a task's children are part of that task, and a cap they could walk
// around is not a cap.
//
// A cap of exactly 0 is never returned as dispatchable: llm-bridge-claudecode
// omits --max-budget-usd when the value is zero, which the CLI reads as
// UNLIMITED. That inversion — the tightest possible ceiling becoming no ceiling
// at all — is the one failure mode here that spends real money.
func budgetForCard(qc *issuesclient.Client, bridgeURL, issuesURL string, cand *queueCandidate) (float64, bool) {
	tree, cost, err := loadSpendTree(bridgeURL, issuesURL)
	if errors.Is(err, errSpendTreeTruncated) {
		// Unlike an unreadable tree, a truncated one says nothing about whether
		// this card inherits a ceiling: its owner may be among the rows the read
		// never reached, and EffectiveCeiling cannot tell a missing ancestor from
		// one that sets no ceiling. Dispatching would send it out uncapped.
		log.Printf("queue: card %s not dispatched — %v", cand.cardID, err)
		return 0, false
	}
	if err != nil {
		// Fail closed, but only for work that actually has a ceiling to honor: an
		// unreadable tree must not park uncapped work that never had one.
		if _, hasOwn := cardSpendCeiling(cand.item); !hasOwn {
			log.Printf("queue: spend tree unreadable (%v) — card %s sets no ceiling of its own; dispatching as before",
				err, cand.cardID)
			return 0, true
		}
		log.Printf("queue: card %s is capped but the spend tree is unreadable (%v) — holding rather than dispatching uncapped",
			cand.cardID, err)
		return 0, false
	}

	ceiling, ownerID, ok := tree.EffectiveCeiling(cand.cardID)
	if !ok {
		return 0, true // nothing in this card's ancestry sets a ceiling
	}

	// Spend is measured over the ceiling OWNER's subtree, not this card's. The pot
	// belongs to the ancestor: a sibling that already drained it has to stop this
	// card too, or "$5 for this task" quietly becomes $5 per branch.
	spent, err := tree.SubtreeSpendUSD(ownerID, sessionsForItem(qc), cost)
	if err != nil {
		log.Printf("queue: card %s has a $%.2f ceiling but its spend is unreadable (%v) — holding rather than dispatching uncapped",
			cand.cardID, ceiling, err)
		return 0, false
	}

	whose := "its own"
	if ownerID != cand.cardID {
		whose = "inherited from " + ownerID
	}

	remaining := ceiling - spent
	if remaining <= 0 {
		log.Printf("queue: card %s has spent $%.2f of its $%.2f ceiling (%s) — holding", cand.cardID, spent, ceiling, whose)
		return 0, false
	}
	log.Printf("queue: card %s spend ceiling $%.2f (%s), spent $%.2f across the task tree — capping this session at $%.2f",
		cand.cardID, ceiling, whose, spent, remaining)
	return remaining, true
}

// sessionsForItem maps an item to the bridge sessions that worked it, via its
// card's session links. Memoised: a subtree walk would otherwise re-ask for the
// same ancestor's links once per descendant.
func sessionsForItem(qc *issuesclient.Client) func(string) ([]string, error) {
	cache := map[string][]string{}
	return func(itemID string) ([]string, error) {
		if s, ok := cache[itemID]; ok {
			return s, nil
		}
		links, err := qc.ListCardLinks(itemID)
		if err != nil {
			return nil, fmt.Errorf("list card links: %w", err)
		}
		var sessions []string
		for _, l := range links {
			if l.EntityType == boardvocabulary.LinkTypeSession {
				sessions = append(sessions, l.EntityRef)
			}
		}
		cache[itemID] = sessions
		return sessions, nil
	}
}

// errSpendTreeTruncated means the item read hit its page bound before the end of
// the list. It is not an ordinary read failure, and budgetForCard does not treat
// it as one.
var errSpendTreeTruncated = errors.New("spend tree truncated")

func loadSpendTree(bridgeURL, issuesURL string) (*spendtree.Tree, map[string]float64, error) {
	items, truncated, err := issueitemsclient.FetchEveryItemIncludingHeld(issuesURL)
	if err != nil {
		return nil, nil, err
	}
	if truncated {
		return nil, nil, fmt.Errorf("%w: read %d noteboard items and stopped at the %d-page bound; a ceiling owner past it would be invisible",
			errSpendTreeTruncated, len(items), issueitemsclient.MaxPages)
	}
	aggs, err := fetchSessionAggregates(bridgeURL)
	if err != nil {
		return nil, nil, err
	}
	cost := make(map[string]float64, len(aggs))
	for _, a := range aggs {
		cost[a.SessionID] = a.CostUSD
	}
	treeItems := make([]spendtree.Item, 0, len(items))
	for _, it := range items {
		parent := ""
		if it.ParentID != nil {
			parent = *it.ParentID
		}
		treeItems = append(treeItems, spendtree.Item{
			ID:            it.ID,
			ParentID:      parent,
			AutoHoldAtUSD: it.AutoHoldAtUSD,
		})
	}
	return spendtree.NewTree(treeItems), cost, nil
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

// cardSpendCeiling reads auto_hold_at_usd off the noteboard item the card
// carries. Returns (ceiling, true) only when the card ITSELF has one — a missing
// key and a null both mean "no ceiling of its own", which says nothing about what
// it may inherit from an ancestor.
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

// errMalformedEnvironmentValue marks a config error that came from the
// environment rather than from the command line.
//
// main keeps the two apart because they used to exit differently and still
// should: a bad argument is the flag package's exit 2, a bad environment value
// was a log.Fatalf and stays exit 1. Returning it rather than dying in place is
// what makes it reachable — it used to fire during package variable
// initialization, before main() ran, so no test could observe it at all.
var errMalformedEnvironmentValue = errors.New("malformed environment value")

// resolveSessionBudget decides the cost cap for one dispatched session, and says
// why. This is the money decision, so it is a function rather than a few lines
// inside the fire loop: it is the thing worth testing.
//
// Precedence:
//
//  1. The task's ceiling. cand.maxBudgetUSD is already (ceiling - spent across
//     the whole task tree), the only number that keeps THAT task under THAT
//     ceiling. A fleet default must never raise or lower it — not even when the
//     default is tighter, since the ceiling is the promise the user actually made
//     about that task.
//
//  2. The fleet default, for everything a task ceiling cannot reach. Chiefly the
//     noteboard-discovery fallback, which runs whenever the Queued column is
//     empty — most nights — and has no card at all to carry a ceiling. That path
//     produced this host's most expensive unattended sessions, and it is
//     dispatched uncapped without a default.
//
//  3. Uncapped, when neither is set. This is the historical behavior and stays
//     the default: arming a fleet-wide cap is the user's call, not ours.
//
// The returned reason is logged verbatim. An uncapped session should be able to
// say out loud why it is uncapped — silence is how a cap that never armed goes
// unnoticed for months.
func resolveSessionBudget(cand *queueCandidate, fleetDefaultUSD float64) (float64, string) {
	if cand != nil && cand.maxBudgetUSD > 0 {
		return cand.maxBudgetUSD, "the task's spend ceiling (minus what its whole tree has already cost)"
	}
	if fleetDefaultUSD > 0 {
		if cand != nil {
			return fleetDefaultUSD, "fleet default; this task sets no ceiling of its own"
		}
		return fleetDefaultUSD, "fleet default; noteboard-discovery session, which has no card to carry a ceiling"
	}
	if cand != nil {
		return 0, "this task sets no ceiling and no fleet default is configured"
	}
	return 0, "noteboard-discovery session and no fleet default is configured (-default-max-budget-usd)"
}
