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

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/bridgeauth"
	schedulerconfig "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/config"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/degradations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/kanbanclient"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/kanbanvocab"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/noteboarditems"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/spendtree"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

type sessionAggregate struct {
	SessionID string  `json:"session_id"`
	CostUSD   float64 `json:"cost_usd"`
}

type noteboardItem = noteboarditems.Item

// config names the three services this guard reads and whether it is allowed to
// write. It exists so run can be given stubs by a test; main fills it from the
// service settings.
type config struct {
	kanbanURL          string
	kanbanServiceToken string
	bridgeURL          string
	noteboardURL       string
	dryRun             bool
}

func main() {
	settings, err := schedulerconfig.SpendCeilingGuard.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatal(err)
	}
	dryRun := flag.Bool("dry-run", false, "Report what would be held; change nothing.")
	flag.Parse()

	bridgeURL := settings.String(schedulerconfig.SettingBridgeURL)

	// llm-bridge-server gates every route; stamp this process's calls to it
	// with the service token. Host-scoped, so noteboard, kanban-store and the
	// scheduler API are untouched.
	if err := bridgeauth.StampRequestsToBridge(bridgeURL, settings.String(schedulerconfig.SettingBridgeServiceToken)); err != nil {
		log.Fatal(err)
	}

	os.Exit(run(config{
		kanbanURL:          settings.String(schedulerconfig.SettingKanbanStoreURL),
		kanbanServiceToken: settings.String(schedulerconfig.SettingKanbanStoreServiceToken),
		bridgeURL:          bridgeURL,
		noteboardURL:       settings.String(schedulerconfig.SettingNoteboardURL),
		dryRun:             *dryRun,
	}))
}

// run is one tick, and its return value is the process exit status.
//
// The two fetches below stay fatal: with no cost table or no items there is no
// tick to have, and exiting 1 already tells the scheduler so. What this
// function adds is the three ways the guard used to fall short of its job and
// still exit 0 — a subtree whose spend it could not price, a breach whose hold
// the board refused, and an item list it read only part of. All three left the
// run recorded `success`, which for a dollar cap is the wrong answer.
func run(cfg config) int {
	degraded := degradations.New("ceiling")

	// Cost is a whole-fleet table; fetch it once rather than per item.
	aggregates, err := fetchSessionAggregates(cfg.bridgeURL)
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
	items, truncated, err := noteboarditems.FetchEveryItemIncludingHeld(cfg.noteboardURL)
	if err != nil {
		log.Fatalf("noteboard items: %v", err)
	}
	if truncated {
		// The tree is the instrument. An item this read never saw contributes no
		// spend to its ancestor's total and is never checked as an owner of its
		// own, so a short read prices every ceiling too low — in the direction
		// that lets a task keep running.
		degraded.Record("read only %d noteboard items before hitting the %d-page safety bound: the spend tree is incomplete and every ceiling below it is priced too LOW",
			len(items), noteboarditems.MaxPages)
	}
	log.Printf("loaded cost for %d sessions, %d items", len(cost), len(items))

	tree := spendtree.NewTree(toTreeItems(items))
	qc := kanbanclient.New(cfg.kanbanURL, cfg.kanbanServiceToken)

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
			if l.EntityType == kanbanvocab.LinkTypeSession {
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
			degraded.Record("item %s: spend unreadable (%v) — its ceiling went unenforced this tick", it.ID, err)
			continue
		}
		if spent < ceiling {
			continue
		}

		n := len(tree.Subtree(it.ID))
		if cfg.dryRun {
			log.Printf("WOULD HOLD %s — $%.2f spent of $%.2f ceiling across %d item(s) — %q", it.ID, spent, ceiling, n, it.Title)
			held++
			continue
		}
		reason := fmt.Sprintf("spend ceiling reached: $%.2f of $%.2f (across %d item(s) in this task tree)", spent, ceiling, n)
		if err := qc.HoldCard(it.ID, reason); err != nil {
			log.Printf("item %s: OVER CEILING ($%.2f of $%.2f) but the hold FAILED: %v — this task is still dispatchable",
				it.ID, spent, ceiling, err)
			// The sharpest of the three. The guard has already decided this task
			// is out of money; the only thing that stops it spending more is the
			// hold, and the hold did not land.
			degraded.Record("item %s is OVER CEILING ($%.2f of $%.2f) and the hold FAILED (%v) — it is still dispatchable",
				it.ID, spent, ceiling, err)
			continue
		}
		log.Printf("HELD %s — $%.2f spent of $%.2f ceiling across %d item(s) — %q", it.ID, spent, ceiling, n, it.Title)
		held++
	}
	log.Printf("done: %d task(s) with a ceiling checked, %d held", checked, held)
	return degraded.ExitCode()
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
