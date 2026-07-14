// spend-ceiling-guard holds any kanban card whose cumulative agent spend has
// reached the spend ceiling set on it (noteboard `auto_hold_at_usd`).
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
// a card past its ceiling, so this guard only ever has to notice a breach the
// harness already stopped at — never race one in progress.
//
// Registered as a scheduler shell job. Fire-and-forget, like the other curators:
// no state file, kanban-store IS the state. Holding is idempotent (noteboard
// will not move an existing hold's timestamp), so a card that is already held is
// left alone.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/slava/scheduler/internal/kanbanclient"
)

type sessionAggregate struct {
	SessionID string  `json:"session_id"`
	CostUSD   float64 `json:"cost_usd"`
}

func main() {
	var (
		boardsFlag = flag.String("boards", "Autoworker queue,Agent runs,Task completion loop",
			"Comma-separated kanban boards to guard.")
		dryRun = flag.Bool("dry-run", false, "Report what would be held; change nothing.")
	)
	flag.Parse()

	kanbanURL := envOr("KANBAN_STORE_URL", "http://localhost:8305")
	bridgeURL := envOr("LLM_BRIDGE_URL", "http://localhost:8160")

	// One fetch for every card on every board: cost is a whole-fleet table and
	// re-fetching it per card would be an O(cards) hammering of the gateway.
	aggregates, err := fetchSessionAggregates(bridgeURL)
	if err != nil {
		log.Fatalf("session aggregates: %v", err)
	}
	cost := make(map[string]float64, len(aggregates))
	for _, a := range aggregates {
		cost[a.SessionID] = a.CostUSD
	}
	log.Printf("loaded cost for %d sessions", len(cost))

	var held, checked int
	for _, boardName := range strings.Split(*boardsFlag, ",") {
		boardName = strings.TrimSpace(boardName)
		if boardName == "" {
			continue
		}
		qc := kanbanclient.New(kanbanURL)
		board, err := qc.FindBoard(boardName)
		if err != nil || board == nil {
			log.Printf("board %q: unavailable (%v) — skipping", boardName, err)
			continue
		}
		qc.BoardID = board.ID

		view, err := qc.GetBoardView(board.ID)
		if err != nil {
			log.Printf("board %q: view: %v", boardName, err)
			continue
		}

		for _, cv := range view.Columns {
			for _, c := range cv.Cards {
				if c.Item == nil {
					continue
				}
				ceiling, ok := spendCeiling(c.Item)
				if !ok {
					continue // no ceiling on this card: nothing to enforce
				}
				// Already parked. Re-holding would be a no-op anyway, but there is
				// no reason to price out a card whose gate is already shut.
				if heldAt, _ := c.Item["held_at"].(string); heldAt != "" {
					continue
				}
				checked++

				cardID := c.Placement.CardID
				spent, err := cardSpend(qc, cost, cardID)
				if err != nil {
					log.Printf("card %s: spend unreadable (%v) — NOT holding (a card is not parked on a guess)", cardID, err)
					continue
				}
				if spent < ceiling {
					continue
				}

				title, _ := c.Item["title"].(string)
				if *dryRun {
					log.Printf("WOULD HOLD %s — $%.2f spent of $%.2f ceiling — %q", cardID, spent, ceiling, title)
					held++
					continue
				}

				reason := fmt.Sprintf("spend ceiling reached: $%.2f of $%.2f", spent, ceiling)
				if err := qc.HoldCard(cardID, reason); err != nil {
					log.Printf("card %s: OVER CEILING ($%.2f of $%.2f) but the hold FAILED: %v — this card is still dispatchable",
						cardID, spent, ceiling, err)
					continue
				}
				log.Printf("HELD %s — $%.2f spent of $%.2f ceiling — %q", cardID, spent, ceiling, title)
				held++
			}
		}
	}
	log.Printf("done: %d card(s) with a ceiling checked, %d held", checked, held)
}

// cardSpend totals what every session linked to the card has cost. A session
// with no aggregate row contributes nothing — it has not billed a turn yet.
func cardSpend(qc *kanbanclient.Client, cost map[string]float64, cardID string) (float64, error) {
	links, err := qc.ListCardLinks(cardID)
	if err != nil {
		return 0, err
	}
	var total float64
	for _, l := range links {
		if l.EntityType == "session" {
			total += cost[l.EntityRef]
		}
	}
	return total, nil
}

// spendCeiling reads auto_hold_at_usd off the card's noteboard item. A missing
// key and an explicit null both mean "no ceiling", which is the default — this
// feature is opt-in and must never arm itself.
func spendCeiling(item map[string]any) (float64, bool) {
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
