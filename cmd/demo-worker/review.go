package main

import (
	"fmt"
	"time"

	"github.com/slava/scheduler/internal/demosession"

	"github.com/slava/scheduler/internal/demoteam"
	"github.com/slava/scheduler/internal/kanbanclient"
)

// review dispatches a reviewer for work that has produced a pull request and
// not yet been looked at.
//
// The trigger is the card, not GitHub. A pull request the builder recorded on
// its card is one this system knows the provenance of — which card asked for
// it, which agent wrote it, under which budget. Polling GitHub for open pull
// requests instead would pick up anything anybody opened by hand and review it
// with no idea what it was for.
func review(kc *kanbanclient.Client, cfg config) error {
	reviewer, err := requireIdentity(cfg.ReviewerSlug, demoteam.RoleReviewer)
	if err != nil {
		return err
	}
	workBoard, err := kc.GetBoard(cfg.WorkBoardID)
	if err != nil {
		return fmt.Errorf("work board: %w", err)
	}
	host, err := sessionHostFromBoard(workBoard)
	if err != nil {
		return err
	}
	view, err := kc.GetBoardView(workBoard.ID)
	if err != nil {
		return fmt.Errorf("read board %q: %w", workBoard.Name, err)
	}

	reviewed := 0
	for _, column := range view.Columns {
		for _, card := range column.Cards {
			if reviewed >= cfg.MaxPerTick {
				return nil
			}
			if card.Item == nil {
				continue
			}
			cardID := card.Placement.CardID
			title, _ := card.Item["title"].(string)

			prRef := pullRequestRef(card)
			if prRef == "" {
				continue
			}
			if heldAt, _ := card.Item["held_at"].(string); heldAt != "" {
				fmt.Printf("  skip %s %-46s HELD\n", shortID(cardID), truncate(title, 46))
				continue
			}
			done, err := alreadyReviewed(cfg.KanbanURL, cardID)
			if err != nil {
				return err
			}
			if done {
				fmt.Printf("  skip %s %-46s already reviewed\n", shortID(cardID), truncate(title, 46))
				continue
			}
			workingDir, err := cardRepo(card)
			if err != nil {
				fmt.Printf("  skip %s %-46s %v\n", shortID(cardID), truncate(title, 46), err)
				continue
			}

			fmt.Printf("  REVIEW %s %-46s pr=%s as=%s instance=%s bundle=%q\n",
				shortID(cardID), truncate(title, 46), prRef, reviewer.Slug, host.InstanceID, host.BundleID)
			prompt := reviewerPrompt(reviewer, title, prRef, workingDir, cardID, cfg.KanbanURL)
			if cfg.DryRun {
				fmt.Printf("      ── prompt ──\n%s      ────────────\n", indent(prompt))
				reviewed++
				continue
			}

			sessionID, err := demosession.Spawn(cfg.BridgeURL, demosession.Task{
				Prompt:       prompt,
				AgentSlug:    reviewer.Slug,
				DisplayName:  fmt.Sprintf("%s %s %s", reviewer.Emoji, reviewer.Name, time.Now().UTC().Format("Jan 2 15:04")),
				WorkingDir:   workingDir,
				MaxBudgetUSD: cfg.MaxBudget,
				InstanceID:   host.InstanceID,
				BundleID:     host.BundleID,
			})
			if err != nil {
				return fmt.Errorf("dispatch reviewer for %s: %w", cardID, err)
			}
			// Linking the session records the dispatch, attributed to the
			// reviewer — the same one call that carries both who acted and what
			// they acted on.
			if err := linkAs(cfg.KanbanURL, cardID, "session", sessionID,
				"demo-worker review", actorPrefixAgent+reviewer.Slug); err != nil {
				return fmt.Errorf("link reviewer session: %w", err)
			}
			fmt.Printf("      session %s\n", sessionID)
			reviewed++
		}
	}
	if reviewed == 0 {
		fmt.Println("  nothing waiting on a review")
	}
	return nil
}

// pullRequestRef returns the pull request a card produced, if it has one.
//
// A card carrying two is left alone rather than reviewed twice or half: it does
// not say which one the review is of, and guessing would put a review on a diff
// nobody asked about.
func pullRequestRef(card kanbanclient.CardView) string {
	var found []string
	for _, l := range card.Links {
		if l.EntityType == "pull_request" && l.EntityRef != "" {
			found = append(found, l.EntityRef)
		}
	}
	if len(found) != 1 {
		return ""
	}
	return found[0]
}

// alreadyReviewed reports whether this card's pull request has been reviewed.
//
// Read from the event log rather than from a tag, because the log is the record
// of what happened and a tag is a second copy of it. A reviewer that ran and
// then failed to write its verdict leaves no pull_request_reviewed event, and
// the card is picked up again — which is the behaviour worth having.
func alreadyReviewed(kanbanURL, cardID string) (bool, error) {
	events, err := listEvents(kanbanURL, cardID)
	if err != nil {
		return false, err
	}
	for _, e := range events {
		if e.Kind == "pull_request_reviewed" {
			return true, nil
		}
	}
	return false, nil
}
