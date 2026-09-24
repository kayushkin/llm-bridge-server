package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/slava/scheduler/internal/kanbanclient"
)

// ============================ due-date backfill ============================
//
// Every card written before due_at existed carries its deadline only as prose,
// in the summary the model wrote at filing time: "due August 1st", "expires Aug
// 14". The information is there; it was simply never stored as a date.
//
// This pass recovers it. It runs on demand rather than on the tick, because it
// costs a model call over the whole board and only ever needs doing once per
// card — after this, filing stores the date directly.
//
// It reads no mailbox. The card summaries already carry the deadlines, which
// makes this both cheap and safe to re-run: a card that already has a due date
// is never a candidate.

var backfillSystemPrompt = `You are reading kanban cards that were filed from email before deadlines were stored as dates. Each card's summary was written at filing time and often states its deadline in prose ("due August 1st", "expires Aug 14", "appointment Sept 5").

Recover that deadline as a date.

For each card you are given, return a due_date ONLY when the card's own text states or clearly implies one. Return nothing for cards that have no deadline — most will not, and that is the expected answer. Inventing a date is far worse than omitting one: an invented deadline will rank the card above things that genuinely matter.

Today's date is given below. Use it to resolve anything relative, and to pick the year when the text names only a month and day. A deadline that has already passed is still correct to report — say August 1st when the text says August 1st, even though it is now later than that.

Format: YYYY-MM-DD, or full RFC3339 when the card names a time of day that matters.

Report by outputting a single JSON object conforming to the provided schema, exactly once.`

// cardDeadline is one recovered date.
type cardDeadline struct {
	CardID  string `json:"card_id"`
	DueDate string `json:"due_date"`
}

type backfillReport struct {
	Deadlines []cardDeadline `json:"deadlines"`
}

// backfillSchema is the JSON Schema the model's deadline reply must conform
// to (formerly a forced tool's input_schema; an llm.completion operation
// takes the bare schema).
func backfillSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"deadlines": map[string]any{
				"type":        "array",
				"description": "Only cards whose own text states a deadline. Omit the rest entirely.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"card_id":  map[string]any{"type": "string", "description": "The card_id exactly as given."},
						"due_date": map[string]any{"type": "string", "description": "YYYY-MM-DD or RFC3339."},
					},
					"required": []string{"card_id", "due_date"},
				},
			},
		},
		"required": []string{"deadlines"},
	}
}

// backfillCandidates are the cards worth asking about: no stored deadline, and
// not gated to zero stakes.
//
// Buckets are excluded because a deadline could not change their score even if
// one were found — "Meetup event digests" is action:none, so it sits at priority
// zero whatever date is attached. Asking about them would spend tokens to
// produce nothing, and the board is mostly buckets.
func backfillCandidates(state *boardState) []*cardState {
	var out []*cardState
	for _, card := range state.Cards {
		if card.DueAt != nil {
			continue
		}
		if stakesOf(card.Category, card.Action) == stakesNone {
			continue
		}
		out = append(out, card)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// buildBackfillDigest renders the candidates for the model. The body is trimmed
// of the evidence block the classifier appends, which lists linked emails and
// carries no deadline prose of its own.
func buildBackfillDigest(cards []*cardState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[TODAY IS %s]\n\n[CARDS]\n", time.Now().Format("2006-01-02 (Monday)"))
	for _, c := range cards {
		fmt.Fprintf(&b, "\n--- card_id=%s\ntitle: %s\ncategory: %s | action: %s | urgency-as-filed: %s\nsummary: %s\n",
			c.ID, c.Title, c.Category, c.Action, c.Urgency, summaryOf(c.Body))
	}
	return b.String()
}

// summaryOf strips the linked-email block from a card body, leaving the prose
// the model wrote — which is where any deadline will be stated.
func summaryOf(body string) string {
	if i := strings.Index(body, evidenceMarker); i >= 0 {
		body = body[:i]
	}
	return collapseWhitespace(body)
}

// runBackfillPass recovers deadlines for every candidate card and re-scores it.
func runBackfillPass(ctx context.Context, cfg config, kc *kanbanclient.Client, state *boardState, gate *sessionGate) error {
	candidates := backfillCandidates(state)
	log.Printf("[email] backfill: %d cards have no deadline and could carry one (of %d on the board)",
		len(candidates), len(state.Cards))
	if len(candidates) == 0 {
		return nil
	}

	digest := buildBackfillDigest(candidates)
	if cfg.DumpDigest {
		fmt.Println(digest)
		return nil
	}

	var report backfillReport
	if err := invokeSchema(ctx, cfg, cfg.Model, backfillSystemPrompt, backfillSchema(), digest, &report); err != nil {
		return fmt.Errorf("backfill model call: %w", err)
	}
	log.Printf("[email] backfill: model reported %d deadlines", len(report.Deadlines))

	applied := 0
	for _, d := range report.Deadlines {
		card, ok := state.Cards[d.CardID]
		if !ok {
			// The model returning an id that is not on the board means it
			// invented one; the card it meant cannot be identified, so this is
			// reported rather than guessed at.
			log.Printf("[email] backfill: WARNING model reported card_id %q which is not on this board", d.CardID)
			continue
		}
		if card.DueAt != nil {
			// Only possible if the model reported the same card twice. The
			// first answer stands.
			continue
		}
		due, err := parseDueDate(d.DueDate, time.Local)
		if err != nil {
			log.Printf("[email] backfill: WARNING card %q has unusable due_date: %v", card.Title, err)
			continue
		}
		card.DueAt = &due
		applied++

		if cfg.DryRun {
			score := scoreCard(card.Category, card.Action, card.Urgency, card.DueAt, time.Now())
			log.Printf("[email] DRY-RUN backfill %q: due=%s -> priority %d, urgency %s",
				card.Title, formatDue(card.DueAt), score.Priority, score.Urgency)
			continue
		}
		if err := kc.PatchCard(card.ID, map[string]any{"due_at": due.Format(time.RFC3339)}); err != nil {
			log.Printf("[email] backfill: writing due_at for %q failed: %v", card.Title, err)
			card.DueAt = nil
			applied--
			continue
		}
		log.Printf("[email] backfill: %q due=%s", card.Title, formatDue(card.DueAt))
	}

	// One sweep at the end turns the recovered dates into scores, rather than
	// duplicating the scoring rules here.
	sweep := rescoreBoard(kc, state, gate, time.Now(), cfg.DryRun)
	log.Printf("[email] backfill done: deadlines_written=%d repriced=%d retagged=%d",
		applied, sweep.Repriced, sweep.Retagged)
	return nil
}
