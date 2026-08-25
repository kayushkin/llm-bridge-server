package main

import (
	"fmt"
	"time"

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/bus/messages"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/slava/scheduler/internal/demoteam"
	"github.com/slava/scheduler/internal/kanbanclient"
)

// dispatch turns queued cards into real agent sessions.
func dispatch(kc *kanbanclient.Client, cfg config) error {
	identity, err := requireIdentity(cfg.BuilderSlug)
	if err != nil {
		return err
	}
	workBoard, err := kc.FindBoard(cfg.WorkBoard)
	if err != nil {
		return fmt.Errorf("find board %q: %w", cfg.WorkBoard, err)
	}
	inProgress, err := columnID(kc, workBoard.ID, columnInProgress)
	if err != nil {
		return err
	}
	view, err := kc.GetBoardView(workBoard.ID)
	if err != nil {
		return fmt.Errorf("read board %q: %w", cfg.WorkBoard, err)
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

			prompt := builderPrompt(identity, title, body, workingDir, cardID)
			fmt.Printf("  DISPATCH %s %-48s as=%s dir=%s cap=$%.2f\n",
				shortID(cardID), truncate(title, 48), identity.Slug, workingDir, cfg.MaxBudget)
			if cfg.DryRun {
				fmt.Printf("      ── prompt ──\n%s\n      ────────────\n", indent(prompt))
				fired++
				continue
			}

			sessionID, err := spawn(cfg, identity, prompt, workingDir)
			if err != nil {
				return fmt.Errorf("dispatch %s: %w", cardID, err)
			}
			if err := kc.AddCardLink(cardID, "session", sessionID, "demo-worker"); err != nil {
				return fmt.Errorf("link session %s: %w", sessionID, err)
			}
			if err := kc.MoveCard(cardID, workBoard.ID, inProgress, 0); err != nil {
				return fmt.Errorf("move %s: %w", cardID, err)
			}
			if err := addEvent(cfg.KanbanURL, cardID, workBoard.ID, "agent_dispatched",
				actorPrefixAgent+identity.Slug,
				fmt.Sprintf("Dispatched to %s in %s, capped at $%.2f", identity.Name, workingDir, cfg.MaxBudget)); err != nil {
				return fmt.Errorf("record dispatch %s: %w", cardID, err)
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

func hasSession(card kanbanclient.CardView) bool {
	for _, l := range card.Links {
		if l.EntityType == "session" {
			return true
		}
	}
	return false
}

// cardRepo reads the working directory the promoter recorded. A queued card
// without one is a promotion bug, not something to dispatch into ~/repos and
// hope.
func cardRepo(card kanbanclient.CardView) (string, error) {
	for _, l := range card.Links {
		if l.EntityType == "repo" && l.EntityRef != "" {
			return l.EntityRef, nil
		}
	}
	return "", fmt.Errorf("no repo link — it was never promoted properly")
}

// spawn publishes the session request. The session id is minted here because
// the adapter passes BusSessionID through as the bridge session_id, so this
// value is what the card's session link resolves to.
func spawn(cfg config, identity demoteam.Member, prompt, workingDir string) (string, error) {
	nc, err := natsbus.Connect(natsbus.Options{URL: cfg.NATSURL, Name: "demo-worker"})
	if err != nil {
		return "", fmt.Errorf("nats: %w", err)
	}
	defer nc.Close()

	now := time.Now().UTC()
	sessionID := fmt.Sprintf("demo-%s-%d", identity.Slug, now.UnixNano())
	budget := cfg.MaxBudget

	create := &msg.CreateSessionRequest{
		Harness:     msg.HarnessClaudeCode,
		InstanceID:  "inst-cc-local",
		DisplayName: fmt.Sprintf("%s %s %s", identity.Emoji, identity.Name, now.Format("Jan 2 15:04")),
		Purpose:     "autoworker",
		Origin:      "scheduler",
		Type:        msg.SessionTypeAutonomous,
		AutoStart:   true,
		// Who this session acts as, and where it works. Both were designed for
		// exactly this and have been empty on essentially every session.
		AgentID:    identity.Slug,
		WorkingDir: workingDir,
		MaxBudget:  &budget,
	}
	// The harness-side cap as well as the server-side one. autoworker's spend
	// notes are blunt about why both: the fastest observed session burned
	// $29.22/min, so a poller that notices afterwards is not a control.
	create.HarnessConfig = []byte(fmt.Sprintf(`{"max_budget_usd":%.2f}`, budget))

	in := messages.BridgeInbound{
		BusSessionID: sessionID,
		Agent:        identity.Slug,
		Orchestrator: "demo-worker",
		Channel:      "demo-worker",
		Timestamp:    now,
		Create:       create,
		Send:         &msg.SendMessageRequest{Message: prompt},
	}
	if err := nc.Publish(messages.BridgeInboundSubject, in); err != nil {
		return "", fmt.Errorf("publish: %w", err)
	}
	return sessionID, nil
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
