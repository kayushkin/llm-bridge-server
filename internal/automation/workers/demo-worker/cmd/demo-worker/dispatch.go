package main

import (
	"fmt"
	"os"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/demo-worker/demosession"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/boardvocabulary"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/demoteam"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
)

// dispatch turns queued cards into real agent sessions.
func dispatch(kc *issuesclient.Client, cfg config) error {
	identity, err := requireIdentity(cfg.BuilderSlug, demoteam.RoleBuilder)
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
	inProgress, err := columnID(kc, workBoard.ID, columnInProgress)
	if err != nil {
		return err
	}
	view, err := kc.GetBoardView(workBoard.ID)
	if err != nil {
		return fmt.Errorf("read board %q: %w", workBoard.Name, err)
	}

	fired := 0
	for _, column := range view.Columns {
		if column.Column.Name != columnQueued {
			continue
		}
		for _, card := range column.Cards {
			if fired >= cfg.MaxPerTick {
				return nil
			}
			if card.Item == nil {
				continue
			}
			cardID := card.Placement.CardID
			title, _ := card.Item["title"].(string)
			body, _ := card.Item["body"].(string)

			if heldAt, _ := card.Item["held_at"].(string); heldAt != "" {
				reason, _ := card.Item["hold_reason"].(string)
				fmt.Printf("  skip %s %-48s HELD (%s)\n", shortID(cardID), truncate(title, 48), reason)
				continue
			}
			if hasSession(card) {
				fmt.Printf("  skip %s %-48s already has a session\n", shortID(cardID), truncate(title, 48))
				continue
			}
			workingDir, err := cardRepo(card)
			if err != nil {
				fmt.Printf("  skip %s %-48s %v\n", shortID(cardID), truncate(title, 48), err)
				continue
			}

			prompt := builderPrompt(identity, title, body, workingDir, cardID, cfg.RepoSlug, cfg.IssuesURL)
			fmt.Printf("  DISPATCH %s %-48s as=%s dir=%s instance=%s bundle=%q cap=$%.2f\n",
				shortID(cardID), truncate(title, 48), identity.Slug, workingDir, host.InstanceID, host.BundleID, cfg.MaxBudget)
			if cfg.DryRun {
				fmt.Printf("      ── prompt ──\n%s\n      ────────────\n", indent(prompt))
				fired++
				continue
			}

			sessionID, err := demosession.Spawn(cfg.BridgeURL, cfg.ModelRoles, demosession.Task{
				ModelRole:    cfg.ModelRole,
				Prompt:       prompt,
				AgentSlug:    identity.Slug,
				DisplayName:  fmt.Sprintf("%s %s %s", identity.Emoji, identity.Name, time.Now().UTC().Format("Jan 2 15:04")),
				WorkingDir:   workingDir,
				MaxBudgetUSD: cfg.MaxBudget,
				InstanceID:   host.InstanceID,
				BundleID:     host.BundleID,
				CardID:       cardID,
				// dispatcher is the one autonomous purpose whose questions reach
				// a person: llm-bridge-server triages them and lands the real
				// ones on this card. Under the default purpose (autoworker) an
				// AskUserQuestion is denied and the worker is left to guess.
				Purpose: "dispatcher",
			})
			if err != nil {
				return fmt.Errorf("dispatch %s: %w", cardID, err)
			}
			// Linking the session IS the dispatch event: kanban-store records an
			// agent_dispatched for a session link, carrying the session on the
			// event's detail so the timeline can name it. Writing our own event
			// beside it recorded the same handover twice — the mistake the email
			// arrivals already taught, in a second place.
			//
			// The actor rides on the query string, which is how kanban-store reads
			// it, so the one event has both who acted and what it acted on.
			if err := linkAs(cfg.IssuesURL, cardID, boardvocabulary.LinkTypeSession, sessionID,
				"demo-worker", actorPrefixAgent+identity.Slug); err != nil {
				return fmt.Errorf("link session %s: %w", sessionID, err)
			}
			moveResult, err := kc.MoveCard(cardID, workBoard.ID, inProgress, 0)
			if err != nil {
				return fmt.Errorf("move %s: %w", cardID, err)
			}
			if moveResult.AutoStatusError != "" {
				fmt.Fprintf(os.Stderr, "      WARNING moved %s to In Progress but its noteboard status was not set to match: %s\n", shortID(cardID), moveResult.AutoStatusError)
			}
			fmt.Printf("      session %s\n", sessionID)
			fired++
		}
	}
	if fired == 0 {
		fmt.Println("  nothing queued to dispatch")
	}
	return nil
}

func hasSession(card issuesclient.CardView) bool {
	for _, l := range card.Links {
		if l.EntityType == boardvocabulary.LinkTypeSession {
			return true
		}
	}
	return false
}

// cardRepo reads the working directory the promoter recorded. A queued card
// without one is a promotion bug, not something to dispatch into ~/repos and
// hope.
func cardRepo(card issuesclient.CardView) (string, error) {
	for _, l := range card.Links {
		if l.EntityType == "repo" && l.EntityRef != "" {
			return l.EntityRef, nil
		}
	}
	return "", fmt.Errorf("no repo link — it was never promoted properly")
}

func indent(s string) string {
	out := ""
	for _, line := range splitLines(s) {
		out += "      │ " + line + "\n"
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
