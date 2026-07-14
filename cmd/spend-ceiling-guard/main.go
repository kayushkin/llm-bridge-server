// spend-ceiling-guard holds any task whose cumulative agent spend has reached the
// spend ceiling set on it (noteboard `auto_hold_at_usd`).
//
// This is the CUMULATIVE half of the ceiling. The other half lives in the
// autoworker, which caps each session it dispatches at (ceiling - already spent)
// and lets Claude Code stop itself in real time via --max-budget-usd. Both are
// needed, and neither is sufficient alone:
//
//   - The per-session cap cannot see across sessions. The dispatcher revives
//     cards, so a card retried three times under a $5 cap has still cost $15,
//     and "this task has cost me $15" is what the ceiling is asked to mean.
//
//   - This guard cannot run in real time. It polls, and polling alone is far too
//     slow to enforce a dollar cap: measured on this host's own session history
//     the median session burns $0.37/min and the fastest observed burned
//     $29.22/min, so a 5-minute poll would overshoot by ~$1.83 in the normal
//     case and ~$146 in the worst.
//
// Together they are correct: the per-session cap means a session can never push
// a task past its ceiling, so this guard only ever has to notice a breach the
// harness already stopped at — never race one in progress.
//
// The ceiling covers a TREE, not one item. The autoworker's prompt tells workers
// to split a too-large todo into child todos, and kanban-scoper decomposes goals
// into sub-cards; a ceiling that applied only to the row it was written on would
// be walked around by exactly that decomposition. So spend rolls up from
// descendants, a child inherits the nearest ancestor's ceiling, and a breach
// holds the item that OWNS the ceiling — holding the child that happened to trip
// it would leave the parent and its siblings spending against an empty pot.
// Holding the owner withholds the whole subtree, because noteboard inherits a
// hold down parent_id.
//
// Fire-and-forget, like the other curators: no state file, the stores ARE the
// state. Holding is idempotent, so an already-held task is left alone.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/slava/scheduler/internal/kanbanclient"
	"github.com/slava/scheduler/internal/spendtree"
)

type sessionAggregate struct {
	SessionID string  `json:"session_id"`
	CostUSD   float64 `json:"cost_usd"`
}

type noteboardItem struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	ParentID      *string  `json:"parent_id"`
	HeldAt        *string  `json:"held_at"`
	AutoHoldAtUSD *float64 `json:"auto_hold_at_usd"`
}

func main() {
	dryRun := flag.Bool("dry-run", false, "Report what would be held; change nothing.")
	flag.Parse()

	kanbanURL := envOr("KANBAN_STORE_URL", "http://localhost:8305")
	bridgeURL := envOr("LLM_BRIDGE_URL", "http://localhost:8160")
	noteboardURL := envOr("NOTEBOARD_URL", "http://localhost:8191")

	// Cost is a whole-fleet table; fetch it once rather than per item.
	aggregates, err := fetchSessionAggregates(bridgeURL)
	if err != nil {
		log.Fatalf("session aggregates: %v", err)
	}
	cost := make(map[string]float64, len(aggregates))
	for _, a := range aggregates {
		cost[a.SessionID] = a.CostUSD
	}

	// include_held=true: we must see parked items to build the tree. A held
	// ancestor is exactly the thing whose descendants we need to reason about,
	// and noteboard hides held items from discovery by default.
	items, err := fetchItems(noteboardURL)
	if err != nil {
		log.Fatalf("noteboard items: %v", err)
	}
	log.Printf("loaded cost for %d sessions, %d items", len(cost), len(items))

	tree := spendtree.NewTree(toTreeItems(items))
	qc := kanbanclient.New(kanbanURL)

	// A card's session links are how spend is attributed to an item. Cache per
	// item: a subtree walk asks for the same parent's links once per descendant
	// otherwise.
	linkCache := map[string][]string{}
	sessionsFor := func(itemID string) ([]string, error) {
		if s, ok := linkCache[itemID]; ok {
			return s, nil
		}
		links, err := qc.ListCardLinks(itemID)
		if err != nil {
			return nil, err
		}
		var sessions []string
		for _, l := range links {
			if l.EntityType == "session" {
				sessions = append(sessions, l.EntityRef)
			}
		}
		linkCache[itemID] = sessions
		return sessions, nil
	}

	// Only the ceiling OWNERS are candidates. A descendant cannot breach a
	// ceiling its ancestor owns without the ancestor breaching it too — the
	// subtree total is the same number — so evaluating owners covers every case
	// and holds the right row.
	var checked, held int
	for _, it := range items {
		if it.AutoHoldAtUSD == nil {
			continue
		}
		if it.HeldAt != nil {
			continue // already parked; no reason to price it out again
		}
		checked++

		ceiling := *it.AutoHoldAtUSD
		spent, err := tree.SubtreeSpendUSD(it.ID, sessionsFor, cost)
		if err != nil {
			// A task is not parked on a guess, and it is not let run on one either:
			// this is loud, and the autoworker independently fails CLOSED when it
			// cannot read a capped card's spend.
			log.Printf("item %s: spend unreadable (%v) — NOT holding; the autoworker will refuse to dispatch it", it.ID, err)
			continue
		}
		if spent < ceiling {
			continue
		}

		n := len(tree.Subtree(it.ID))
		if *dryRun {
			log.Printf("WOULD HOLD %s — $%.2f spent of $%.2f ceiling across %d item(s) — %q", it.ID, spent, ceiling, n, it.Title)
			held++
			continue
		}
		reason := fmt.Sprintf("spend ceiling reached: $%.2f of $%.2f (across %d item(s) in this task tree)", spent, ceiling, n)
		if err := qc.HoldCard(it.ID, reason); err != nil {
			log.Printf("item %s: OVER CEILING ($%.2f of $%.2f) but the hold FAILED: %v — this task is still dispatchable",
				it.ID, spent, ceiling, err)
			continue
		}
		log.Printf("HELD %s — $%.2f spent of $%.2f ceiling across %d item(s) — %q", it.ID, spent, ceiling, n, it.Title)
		held++
	}
	log.Printf("done: %d task(s) with a ceiling checked, %d held", checked, held)
}

func toTreeItems(items []noteboardItem) []spendtree.Item {
	out := make([]spendtree.Item, 0, len(items))
	for _, it := range items {
		parent := ""
		if it.ParentID != nil {
			parent = *it.ParentID
		}
		out = append(out, spendtree.Item{
			ID:            it.ID,
			ParentID:      parent,
			AutoHoldAtUSD: it.AutoHoldAtUSD,
		})
	}
	return out
}

func fetchItems(noteboardURL string) ([]noteboardItem, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	// include_held is required: the tree needs held ancestors in it, and a large
	// limit because the whole tree must be present or a parent edge dangles.
	resp, err := client.Get(noteboardURL + "/api/items?include_held=true&limit=10000")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("noteboard items: %s", resp.Status)
	}
	var items []noteboardItem
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}
	return items, nil
}

func fetchSessionAggregates(bridgeURL string) ([]sessionAggregate, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(bridgeURL + "/sessions/aggregates")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("aggregates: %s", resp.Status)
	}
	var aggs []sessionAggregate
	if err := json.NewDecoder(resp.Body).Decode(&aggs); err != nil {
		return nil, err
	}
	return aggs, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
