package main

import (
	"fmt"
	"log"
	"time"

	"github.com/slava/scheduler/internal/kanbanclient"
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
func rescoreBoard(kc *kanbanclient.Client, state *boardState, gate *sessionGate, now time.Time, dryRun bool) sweepResult {
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
