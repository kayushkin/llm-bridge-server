package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/slava/scheduler/internal/kanbanclient"
)

// ============================ card merge pass ============================
//
// Housekeeping over the board, not over the mail. It reads no mailbox at all.
//
// It exists because better filing cannot repair filing already done. The board
// carries six separate cards named "File Uber payment receipt ($11.94)", "File
// Spotify payment receipt ($24.88)" and so on — each title encodes one amount,
// so no second receipt can ever join the first, and every future receipt makes
// a seventh card. That is one bucket wearing six hats, and only a pass that
// looks at the whole board at once can see it.
//
// The dangerous verb here is not "create" but "move": merging relocates the
// evidence of one card onto another and takes the first off the board. So the
// safety rules below are enforced in code, not asked for in the prompt.

const mergeToolName = "report_card_merges"

// mergeGroup is one set of cards the model believes are a single bucket.
type mergeGroup struct {
	SurvivorCardID string   `json:"survivor_card_id"`
	SurvivorTitle  string   `json:"survivor_title"`
	CardIDs        []string `json:"card_ids"`
	Category       string   `json:"category"`
	Action         string   `json:"action"`
	Urgency        string   `json:"urgency"`
	Reason         string   `json:"reason"`
}

type mergeReport struct {
	Merges []mergeGroup `json:"merges"`
}

var mergeSystemPrompt = `You are tidying a kanban board whose cards gather emails. Each card is either:

- a BUCKET: a durable home for recurring mail nobody acts on ("Medium Daily Digest", "Uber promotions"). It should accumulate messages forever.
- a TASK: one specific thing to do, which ends ("Pay August rent", "Decide on the watch offer").

Your job is to find groups of cards that are really ONE bucket, split apart because each was named after a single message instead of the category. The clearest tell is a title containing a specific amount, date, or order number where the underlying mail is routine: "File Uber payment receipt ($11.94)" and "File Spotify payment receipt ($24.88)" are one "Payment receipts" bucket, not two cards.

Rules:
- NEVER merge TASK cards. Two different things to do are two cards even if they look similar. If in doubt, do not merge.
- Only group cards that would genuinely receive the same future mail.
- A group needs at least two cards.
- survivor_card_id: if one existing card is already correctly named for the whole group, give its id and the others fold into it. If none is, leave it empty and give a survivor_title naming the CATEGORY ("Payment receipts", "Substack newsletters") — never an instance.
- reason: one short sentence a human can check.

Groups must not overlap in purpose. Two groups that would both end up being "receipts" are one group, not two — decide the boundary once and put every card on the right side of it. Emitting overlapping buckets just moves the fragmentation you were asked to remove.

Returning an empty merges list is a perfectly good answer. Do not invent groupings to look useful.

You MUST output by calling the ` + mergeToolName + ` tool exactly once.`

func mergeToolSchema() map[string]any {
	return map[string]any{
		"name":        mergeToolName,
		"description": "Report groups of cards that are really one bucket. Call exactly once.",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"merges": map[string]any{
					"type":        "array",
					"description": "Groups of cards to combine. Empty is a valid answer.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"survivor_card_id": map[string]any{"type": "string", "description": "Existing card to absorb the others. Empty to create a new one."},
							"survivor_title":   map[string]any{"type": "string", "maxLength": 70, "description": "Category name, used when survivor_card_id is empty."},
							"card_ids":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Every card in the group, at least two."},
							"category":         map[string]any{"type": "string", "enum": cardCategories},
							"action":           map[string]any{"type": "string", "enum": cardActions},
							"urgency":          map[string]any{"type": "string", "enum": cardUrgencies},
							"reason":           map[string]any{"type": "string"},
						},
						"required": []string{"card_ids", "reason"},
					},
				},
			},
			"required": []string{"merges"},
		},
	}
}

// buildMergeDigest renders the whole board. Unlike the filing digest this is
// never truncated: a merge decision is precisely a whole-board judgement, and a
// board trimmed to fit would hide the very duplicate being looked for. At ~90
// cards of one line each this is a few thousand tokens.
func buildMergeDigest(state *boardState, gate *sessionGate) string {
	cards := make([]*cardState, 0, len(state.Cards))
	for _, c := range state.Cards {
		// Keeping agent-held cards out of the digest is an economy, not the
		// safeguard — applyMerge refuses them regardless. Offering them would
		// spend tokens on proposals that are then refused, every tick, for as
		// long as the agent runs.
		if owned, _ := gate.agentOwns(c); owned {
			continue
		}
		cards = append(cards, c)
	}
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].ColumnName != cards[j].ColumnName {
			return cards[i].ColumnName < cards[j].ColumnName
		}
		return cards[i].Title < cards[j].Title
	})

	var b strings.Builder
	b.WriteString("[EVERY CARD ON THE EMAIL BOARD]\n")
	for _, c := range cards {
		senders := c.Senders
		if len(senders) > 3 {
			senders = senders[:3]
		}
		fmt.Fprintf(&b, "- card_id=%s | %s | column=%s | cat=%s action=%s urgency=%s | emails=%d | senders=%s\n",
			c.ID, c.Title, c.ColumnName, c.Category, c.Action, c.Urgency,
			len(c.EmailLinks), strings.Join(senders, ","))
	}
	return b.String()
}

func runMergePass(ctx context.Context, cfg config, kc *kanbanclient.Client, state *boardState, gate *sessionGate) error {
	digest := buildMergeDigest(state, gate)
	if cfg.DumpDigest {
		fmt.Println(digest)
		return nil
	}
	log.Printf("[merge] considering %d cards", len(state.Cards))

	report, err := classifyMerges(ctx, cfg, digest)
	if err != nil {
		return err
	}
	if len(report.Merges) == 0 {
		log.Printf("[merge] nothing to merge")
		return nil
	}

	applied := 0
	for _, g := range report.Merges {
		ok, err := applyMerge(cfg, kc, state, g, gate)
		if err != nil {
			// One bad group must not abort the pass; the others are independent.
			log.Printf("[merge] group %q failed: %v", g.SurvivorTitle, err)
			continue
		}
		if ok {
			applied++
		}
	}
	log.Printf("[merge] done: %d group(s) merged", applied)
	return nil
}

// applyMerge validates one group and performs it. Returns false when the group
// was refused, which is not an error — refusing is the safe outcome.
func applyMerge(cfg config, kc *kanbanclient.Client, state *boardState, g mergeGroup, gate *sessionGate) (bool, error) {
	members := make([]*cardState, 0, len(g.CardIDs))
	seen := map[string]bool{}
	for _, id := range g.CardIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		c, ok := state.Cards[id]
		if !ok {
			log.Printf("[merge] refused: card %s is not on this board", id)
			return false, nil
		}
		members = append(members, c)
	}
	if len(members) < 2 {
		log.Printf("[merge] refused %q: a group needs at least two cards", g.SurvivorTitle)
		return false, nil
	}

	// Enforced here rather than trusted to the prompt. All three rules protect
	// against the same failure: a merge that silently destroys a distinction a
	// human deliberately made.
	for _, c := range members {
		// A card someone finished is not raw material for tidying.
		if c.ColumnName == columnActionCompleted {
			log.Printf("[merge] refused %q: card %q is in %s", g.SurvivorTitle, c.Title, columnActionCompleted)
			return false, nil
		}
		// Cards in different columns are in different states of work; folding
		// them together would move work across the board as a side effect of
		// housekeeping.
		if c.ColumnName != members[0].ColumnName {
			log.Printf("[merge] refused %q: cards span columns (%s vs %s)",
				g.SurvivorTitle, members[0].ColumnName, c.ColumnName)
			return false, nil
		}
		// Only buckets merge. Two things to do stay two things to do, however
		// alike they look.
		if actionsNeedingWork[c.Action] {
			log.Printf("[merge] refused %q: card %q is a task (action=%s), not a bucket",
				g.SurvivorTitle, c.Title, c.Action)
			return false, nil
		}
		// An agent is working this card. Merging would retire it — archiving the
		// card and moving its links away — while a live session is still writing
		// to it, which is the most destructive thing this binary can do. Refuse
		// the whole group rather than the one card: dropping a member silently
		// changes what the model proposed into something it never approved.
		if owned, session := gate.agentOwns(c); owned {
			log.Printf("[merge] refused %q: card %q is held by an agent (session=%s)",
				g.SurvivorTitle, c.Title, session)
			return false, nil
		}
	}

	survivor, losers, err := resolveSurvivor(cfg, kc, state, g, members, gate)
	if err != nil {
		return false, err
	}
	if survivor == nil {
		return false, nil
	}

	totalMoved := 0
	for _, loser := range losers {
		moved, err := moveLinks(cfg, kc, loser, survivor)
		if err != nil {
			return false, fmt.Errorf("move links from %q: %w", loser.Title, err)
		}
		totalMoved += moved
		if err := retireCard(cfg, kc, loser, survivor); err != nil {
			return false, fmt.Errorf("retire %q: %w", loser.Title, err)
		}
	}
	log.Printf("[merge] %q absorbed %d card(s), %d link(s) moved — %s",
		survivor.Title, len(losers), totalMoved, g.Reason)
	return true, nil
}

// resolveSurvivor picks or creates the card the group folds into.
func resolveSurvivor(cfg config, kc *kanbanclient.Client, state *boardState, g mergeGroup, members []*cardState, gate *sessionGate) (*cardState, []*cardState, error) {
	if g.SurvivorCardID != "" {
		survivor, ok := state.Cards[g.SurvivorCardID]
		if !ok {
			log.Printf("[merge] refused %q: survivor %s is not on this board", g.SurvivorTitle, g.SurvivorCardID)
			return nil, nil, nil
		}
		var losers []*cardState
		for _, c := range members {
			if c.ID != survivor.ID {
				losers = append(losers, c)
			}
		}
		return survivor, losers, nil
	}

	if strings.TrimSpace(g.SurvivorTitle) == "" {
		log.Printf("[merge] refused: group has neither a survivor card nor a title")
		return nil, nil, nil
	}
	if cfg.DryRun {
		log.Printf("[merge] DRY-RUN create survivor %q absorbing %d cards: %s",
			g.SurvivorTitle, len(members), g.Reason)
		for _, c := range members {
			log.Printf("[merge] DRY-RUN   <- %q (%d emails)", c.Title, len(c.EmailLinks))
		}
		return nil, nil, nil
	}

	ap := &applier{cfg: cfg, kc: kc, state: state, gate: gate}
	id, err := ap.createCard(newCardSpec{
		Title:    g.SurvivorTitle,
		Category: firstNonEmpty(g.Category, members[0].Category),
		Action:   firstNonEmpty(g.Action, members[0].Action),
		Urgency:  firstNonEmpty(g.Urgency, members[0].Urgency),
		Summary:  "Merged bucket. " + g.Reason,
	})
	if err != nil {
		return nil, nil, err
	}
	return state.Cards[id], members, nil
}

// moveLinks relocates a card's evidence onto the survivor.
//
// Move, not copy: a link left behind would keep its email attached to a card
// that is no longer on the board, and the next filing tick — which reads the
// board to learn what is already filed — would see that email as unfiled and
// file it a second time.
func moveLinks(cfg config, kc *kanbanclient.Client, loser, survivor *cardState) (int, error) {
	existing := map[string]bool{}
	for _, l := range survivor.AllLinks {
		existing[l.EntityType+"\x00"+l.EntityRef] = true
	}

	moved := 0
	for _, l := range loser.AllLinks {
		key := l.EntityType + "\x00" + l.EntityRef
		if cfg.DryRun {
			moved++
			continue
		}
		if !existing[key] {
			if err := kc.AddCardLink(survivor.ID, l.EntityType, l.EntityRef, l.Label); err != nil {
				return moved, fmt.Errorf("add %s/%s: %w", l.EntityType, l.EntityRef, err)
			}
			existing[key] = true
			survivor.AllLinks = append(survivor.AllLinks, l)
			if l.EntityType == linkTypeEmail {
				survivor.EmailLinks = append(survivor.EmailLinks, l)
			}
		}
		if err := kc.DeleteCardLink(l.ID); err != nil {
			return moved, fmt.Errorf("delete link %s: %w", l.ID, err)
		}
		moved++
	}
	loser.AllLinks = nil
	loser.EmailLinks = nil
	return moved, nil
}

// retireCard takes a merged-away card off the board.
//
// It is archived rather than deleted, and its body records where its evidence
// went. Deleting would be tidier to look at and much worse to live with: the
// underlying noteboard item is what a human may have edited, and a merge is a
// judgement that deserves to be reversible.
func retireCard(cfg config, kc *kanbanclient.Client, loser, survivor *cardState) error {
	note := fmt.Sprintf("%s\n\n%s\n\nMerged into %q (card %s) by email-classifier -merge. "+
		"Its linked emails were moved to that card; this one is kept for history.",
		strings.TrimSpace(stripEvidence(loser.Body)), evidenceMarker, survivor.Title, survivor.ID)
	if cfg.DryRun {
		log.Printf("[merge] DRY-RUN retire %q into %q", loser.Title, survivor.Title)
		return nil
	}
	if err := kc.PatchCard(loser.ID, map[string]any{"body": note, "status": "archived"}); err != nil {
		return err
	}
	return kc.DetachCardFromBoard(kc.BoardID, loser.ID)
}

func stripEvidence(body string) string {
	if i := strings.Index(body, evidenceMarker); i >= 0 {
		return body[:i]
	}
	return body
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
