package main

import (
	"fmt"
	"log"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
)

// ============================ the re-score sweep ============================
//
// Filing mail only touches the cards that mail arrived for. Deadlines move
// without anybody's help, so a board scored only at filing time is wrong by the
// next morning — which is exactly what happened here: a concert "tonight" still
// reading critical two days later, an event the following day still reading
// normal.
//
// This sweep closes that gap by re-scoring every card on every tick. It calls no
// model. The whole pass is arithmetic over data already fetched for the digest,
// which is what makes it cheap enough to run every fifteen minutes rather than
// once a night.

// sweepResult reports what a sweep changed, for the run summary.
type sweepResult struct {
	Scanned  int
	Repriced int
	Retagged int
	Skipped  int
}

// rescoreBoard recomputes priority and urgency for every card and writes back
// only the ones that moved.
//
// Two classes of card are deliberately left alone:
//
//   - cards an agent is working, for the same reason applyCardUpdate skips them:
//     retagging underneath a live session fights whatever it is doing.
//   - cards in Action completed, which a human has already settled. A deadline
//     passing does not un-complete anything.
//
// Cards with no due date are still scored — on stakes alone — but their urgency
// tag is left exactly as the model wrote it, because there is no date to derive
// a better one from. That is what keeps this safe to run against the cards that
// predate due_at.
func rescoreBoard(kc *issuesclient.Client, state *boardState, gate *sessionGate, now time.Time, dryRun bool) sweepResult {
	var result sweepResult

	for _, card := range state.Cards {
		result.Scanned++

		if card.ColumnName == columnActionCompleted {
			result.Skipped++
			continue
		}
		if owned, session := gate.agentOwns(card); owned {
			log.Printf("[email] sweep: card %q is held by an agent — leaving its score alone (session=%s)",
				card.Title, session)
			result.Skipped++
			continue
		}

		score := scoreCard(card.Category, card.Action, card.Urgency, card.DueAt, now)

		retag := score.Urgency != "" && score.Urgency != card.Urgency
		reprice := score.Priority != card.Priority
		if !retag && !reprice {
			continue
		}

		if dryRun {
			// The urgency clause is conditional because an empty score.Urgency
			// means "no deadline, so the stored tag stands" — printing it as
			// `normal -> ` reads as a value being cleared, which is the one
			// thing this sweep is careful never to do.
			change := fmt.Sprintf("priority %d -> %d", card.Priority, score.Priority)
			if retag {
				change += fmt.Sprintf(", urgency %s -> %s", card.Urgency, score.Urgency)
			}
			log.Printf("[email] DRY-RUN sweep %q: %s", card.Title, change)
			if reprice {
				result.Repriced++
			}
			if retag {
				result.Retagged++
			}
			continue
		}

		if retag {
			newTags := replaceTag(card.Tags, tagPrefixUrgent, score.Urgency)
			if err := kc.PatchCard(card.ID, map[string]any{"tags": newTags}); err != nil {
				// One card failing to write must not abandon the rest of the
				// board: the next tick would find the same wrong scores and the
				// same failure, and nothing would ever recover.
				log.Printf("[email] sweep: retag %q failed: %v", card.Title, err)
				continue
			}
			log.Printf("[email] sweep: %q urgency %s -> %s (deadline %s)",
				card.Title, card.Urgency, score.Urgency, formatDue(card.DueAt))
			card.Tags = newTags
			card.Urgency = score.Urgency
			result.Retagged++
		}

		if reprice {
			if err := kc.PatchCard(card.ID, map[string]any{"priority": score.Priority}); err != nil {
				log.Printf("[email] sweep: reprice %q failed: %v", card.Title, err)
				continue
			}
			log.Printf("[email] sweep: %q priority %d -> %d (%s stakes, %s)",
				card.Title, card.Priority, score.Priority, score.Stakes, formatDue(card.DueAt))
			card.Priority = score.Priority
			result.Repriced++
		}
	}

	return result
}

// formatDue renders a deadline for a log line, including the no-deadline case so
// the logs distinguish "no date" from a date that failed to print.
func formatDue(due *time.Time) string {
	if due == nil {
		return "none"
	}
	return due.Format("2006-01-02")
}

// ============================ status ↔ column reconciler ============================
//
// A card's column and its noteboard status are two separate facts, and only one
// direction between them was ever wired. kanban-store applies a column's
// auto_status when a card lands there — drag a card into Action completed and
// the todo is marked done. Nothing ran the other way: tick the todo done in
// noteboard, or have an agent mark it done, and the card stayed in Action
// needed, still scored, still displayed as outstanding. Measured 2026-09-04:
// four done cards sitting in action columns, the oldest for fifteen days.
//
// This pass closes that direction. It calls no model and, like rescoreBoard,
// runs on every tick over data already fetched for the digest.

// reconcileResult reports what the reconciler moved, for the run summary.
type reconcileResult struct {
	Scanned int
	// Completed is done cards moved into Action completed.
	Completed int
	// Reopened is open cards moved out of Action completed, back to the column
	// their action axis says they belong in.
	Reopened int
	Skipped  int
}

// reconcileStatusWithColumn moves every card to the column its noteboard status
// implies, in both directions:
//
//   - status done, column anything else → Action completed
//   - status open, column Action completed → the column for its action axis
//
// Cards an agent is working are left alone, for the reason applyCardUpdate and
// rescoreBoard give: moving a card underneath a live session fights whatever
// the agent is doing. Any other status (archived, or something a human typed)
// is left where it is — this pass knows what open and done mean for placement
// and does not guess about the rest.
//
// Moving into Action completed makes kanban-store re-apply that column's
// auto_status, which is done already; the write is idempotent.
func reconcileStatusWithColumn(kc *issuesclient.Client, state *boardState, gate *sessionGate, dryRun bool) reconcileResult {
	var result reconcileResult

	for _, card := range state.Cards {
		result.Scanned++

		var wantColumn string
		switch {
		case card.Status == statusDone && card.ColumnName != columnActionCompleted:
			wantColumn = columnActionCompleted
		case card.Status == statusOpen && card.ColumnName == columnActionCompleted:
			wantColumn = columnForAction(card.Action)
		default:
			continue
		}

		if owned, session := gate.agentOwns(card); owned {
			log.Printf("[email] reconcile: card %q is held by an agent — leaving it in %s (session=%s)",
				card.Title, card.ColumnName, session)
			result.Skipped++
			continue
		}

		if dryRun {
			log.Printf("[email] DRY-RUN reconcile %q: status=%s, %s -> %s",
				card.Title, card.Status, card.ColumnName, wantColumn)
			result.count(wantColumn)
			continue
		}

		colID, ok := kc.ColumnsByName[wantColumn]
		if !ok {
			// requireColumns checked the three action-state columns at startup,
			// so this is a board edited underneath a running tick. Loud, and on
			// to the next card rather than abandoning the board.
			log.Printf("[email] reconcile: column %q missing on board; cannot move %q", wantColumn, card.Title)
			result.Skipped++
			continue
		}
		moveResult, err := kc.MoveCard(card.ID, kc.BoardID, colID, 0)
		if err != nil {
			log.Printf("[email] reconcile: move %q to %s failed: %v", card.Title, wantColumn, err)
			result.Skipped++
			continue
		}
		if moveResult.AutoStatusError != "" {
			log.Printf("[email] reconcile: moved %q to %s but its noteboard status was not set to match: %s", card.Title, wantColumn, moveResult.AutoStatusError)
		}
		log.Printf("[email] reconcile: %q status=%s, moved %s -> %s",
			card.Title, card.Status, card.ColumnName, wantColumn)
		card.ColumnName = wantColumn
		result.count(wantColumn)
	}

	return result
}

func (r *reconcileResult) count(movedTo string) {
	if movedTo == columnActionCompleted {
		r.Completed++
	} else {
		r.Reopened++
	}
}
