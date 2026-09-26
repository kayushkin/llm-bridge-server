package main

import (
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"

	"github.com/slava/scheduler/internal/demoteam"
	"github.com/slava/scheduler/internal/kanbanclient"
	"github.com/slava/scheduler/internal/kanbanvocab"
)

// buildableActions are the work-vocabulary actions an agent can actually carry
// out against a repo.
//
// respond, decide and escalate are deliberately absent: those need a person's
// judgement or a person's authority, and handing them to a builder produces a
// confident answer nobody asked for. They stay on the Work board for a human.
var buildableActions = map[string]bool{
	"implement":   true,
	"investigate": true,
	"document":    true,
}

// promote turns a classified email card into work.
//
// The card is not copied. It is PLACED on a second board — the same noteboard
// item, two placements — so the email view and the work view are the same card
// seen twice, and closing it in one closes it in the other.
func promote(kc *kanbanclient.Client, cfg config) error {
	identity, err := requireIdentity(cfg.BuilderSlug, demoteam.RoleBuilder)
	if err != nil {
		return err
	}

	emailBoard, err := kc.GetBoard(cfg.EmailBoardID)
	if err != nil {
		return fmt.Errorf("email board: %w", err)
	}
	workBoard, err := kc.GetBoard(cfg.WorkBoardID)
	if err != nil {
		return fmt.Errorf("work board: %w", err)
	}
	queuedColumn, err := columnID(kc, workBoard.ID, columnQueued)
	if err != nil {
		return err
	}

	view, err := kc.GetBoardView(emailBoard.ID)
	if err != nil {
		return fmt.Errorf("read board %q: %w", emailBoard.Name, err)
	}

	promoted := 0
	for _, column := range view.Columns {
		if column.Column.Name != columnActionNeed {
			continue
		}
		for _, card := range column.Cards {
			if promoted >= cfg.MaxPerTick {
				return nil
			}
			cardID := card.Placement.CardID
			if card.Item == nil {
				continue
			}
			title, _ := card.Item["title"].(string)

			why, ok := eligible(card)
			if !ok {
				fmt.Printf("  skip %s %-52s %s\n", shortID(cardID), truncate(title, 52), why)
				continue
			}

			fmt.Printf("  PROMOTE %s %-52s action=%s\n", shortID(cardID), truncate(title, 52), actionOf(card.Item))
			if cfg.DryRun {
				fmt.Printf("      would place on %q/%s, link repo=%s agent=%s, cap $%.2f\n",
					workBoard.Name, columnQueued, cfg.RepoPath, identity.Slug, cfg.MaxBudget)
				promoted++
				continue
			}
			if err := place(kc, cfg, workBoard.ID, queuedColumn, cardID, identity); err != nil {
				return fmt.Errorf("promote %s: %w", cardID, err)
			}
			promoted++
		}
	}
	if promoted == 0 {
		fmt.Println("  nothing eligible to promote")
	}
	return nil
}

// eligible reports whether a card is work an agent can take, and says why not
// when it is not. The reason is printed rather than swallowed: a board where
// nothing is ever promoted and nothing says why is impossible to debug.
func eligible(card kanbanclient.CardView) (string, bool) {
	item := card.Item
	if heldAt, _ := item["held_at"].(string); heldAt != "" {
		// A card parked BY the classifier is parked for this pipeline to pick
		// up — that is what the hold is for, keeping unattended workers off it.
		// A hold anybody else set means somebody decided this should not run,
		// and that decision is not ours to overrule.
		reason, _ := item["hold_reason"].(string)
		if !strings.Contains(reason, "email-classifier") {
			return "held: " + truncate(reason, 40), false
		}
	}
	if status, _ := item["status"].(string); status != "open" {
		return "status=" + status, false
	}
	if !hasTag(item, tagWorkNotStarted) {
		return "already picked up", false
	}
	action := actionOf(item)
	if action == "" {
		return "no action tag", false
	}
	if !buildableActions[action] {
		return "action=" + action + " needs a person", false
	}
	for _, l := range card.Links {
		if l.EntityType == kanbanvocab.LinkTypeSession {
			return "already has a session", false
		}
	}
	return "", true
}

// place attaches the card to the work board and records what an agent needs to
// know to act on it.
func place(kc *kanbanclient.Client, cfg config, workBoardID, queuedColumn, cardID string, identity demoteam.Member) error {
	// Released as it is taken. The card was parked so that whoever found it
	// first would leave it alone; this pipeline is the one it was waiting for.
	if err := unhold(cfg.KanbanURL, cardID); err != nil {
		return fmt.Errorf("release the hold: %w", err)
	}
	if err := attachCard(cfg.KanbanURL, workBoardID, cardID, queuedColumn); err != nil {
		return err
	}
	// The repo is what the agent works in; the identity is who it acts as. Both
	// are read at dispatch — see autoworker's repoWorkingDir and
	// sessionAgentSlug, which this deliberately matches so either dispatcher
	// reads a card the same way.
	if err := kc.AddCardLink(cardID, "repo", cfg.RepoPath, "northwind-api"); err != nil {
		return fmt.Errorf("link repo: %w", err)
	}
	if err := kc.AddCardLink(cardID, "agent", identity.Slug, identity.Name); err != nil {
		return fmt.Errorf("link identity: %w", err)
	}
	// The spend ceiling covers the card and everything under it, and is what
	// both the harness cap and spend-ceiling-guard read.
	tags, err := retag(cfg.NoteboardURL, cardID, tagWorkNotStarted, tagWorkQueued)
	if err != nil {
		return fmt.Errorf("re-read tags: %w", err)
	}
	if err := patchItem(cfg.KanbanURL, cardID, map[string]any{
		"auto_hold_at_usd": cfg.MaxBudget,
		"tags":             tags,
	}); err != nil {
		return fmt.Errorf("set ceiling: %w", err)
	}
	return addNote(cfg.KanbanURL, cardID, workBoardID, actorService,
		fmt.Sprintf("Queued for %s (%s) in %s, capped at $%.2f.",
			identity.Name, identity.Role, cfg.RepoPath, cfg.MaxBudget))
}

func columnID(kc *kanbanclient.Client, boardID, name string) (string, error) {
	columns, err := kc.ListColumns(boardID)
	if err != nil {
		return "", fmt.Errorf("list columns: %w", err)
	}
	for _, c := range columns {
		if c.Name == name {
			return c.ID, nil
		}
	}
	return "", fmt.Errorf("board has no %q column", name)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// attachCard places an existing card on another board. PUT, not POST: POST
// would create a second noteboard item and the two boards would drift apart.
func attachCard(kanbanURL, boardID, cardID, columnID string) error {
	return doJSON(http.MethodPut,
		fmt.Sprintf("%s/api/boards/%s/cards/%s", strings.TrimRight(kanbanURL, "/"), neturl.PathEscape(boardID), neturl.PathEscape(cardID)),
		map[string]any{"column_id": columnID}, http.StatusCreated, http.StatusOK, http.StatusConflict)
}
