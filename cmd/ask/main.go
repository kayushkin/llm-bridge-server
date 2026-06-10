// ask spawns a "Herald" session that relays a question or alert to the user in
// the dashboard and then waits for their reply.
//
// It is the reusable engagement primitive for agents and scheduled jobs that
// need to pull the user into the loop on something they didn't open themselves —
// e.g. the approval proposer asking "approve this update?", or a job raising an
// alert. The spawned agent presents the message verbatim and ends its turn with
// the question, which trips llm-bridge-server's awaiting_user heuristic so the
// session shows up in the dash session list with the amber "?" status. When the
// user replies, the bridge resumes the same agent (claude --resume) with full
// context to carry out the follow-up action.
//
// Mechanism mirrors autoworker's publishWorker: publish a BridgeInbound on NATS
// bridge.inbound targeting the Herald instance, Create+Send in one message. The
// minted session_id is "herald-<ns>" — NOT the "autoworker-" prefix — so the
// kanban-curator's idle-reaper leaves these sessions alone while they wait on
// the user (which can be a long time).
//
// Usage:
//
//	ask "Approve updating llm-bridge-gemini with change X? It will ..." \
//	    --on-reply "If yes, move kanban card <id> on the Autoworker queue board from 'Needs approval' to 'Queued'. If no, leave it and append their reason to the card body."
//
//	ask --title "disk-root alert" "Root disk is at 92%. Want me to prune old logs now?"
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/bus/messages"
	"github.com/kayushkin/llm-bridge/msg"
)

func main() {
	var (
		instanceName = flag.String("instance", "Herald", "Harness instance NAME to host the question session (resolved to id at runtime).")
		title        = flag.String("title", "", "Session display name. Defaults to a truncated form of the question.")
		onReply      = flag.String("on-reply", "", "Instruction for what the agent should do once the user replies. Empty = just relay the alert; no action expected.")
		bridgeURL    = flag.String("bridge-url", envOr("LLM_BRIDGE_URL", "http://localhost:8160"), "llm-bridge-server base URL (for instance resolution).")
		dryRun       = flag.Bool("dry-run", false, "Print the resolved instance and prompt; do not publish.")
	)
	flag.Parse()

	question := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if question == "" {
		log.Fatal("ask: a question/alert message is required (positional argument)")
	}

	instanceID, err := resolveInstanceID(*bridgeURL, *instanceName)
	if err != nil {
		log.Fatalf("ask: resolve instance %q: %v", *instanceName, err)
	}

	now := time.Now().UTC()
	displayName := *title
	if displayName == "" {
		displayName = truncate(question, 60)
	}
	prompt := buildPrompt(question, *onReply)

	if *dryRun {
		fmt.Printf("instance: %s (%s)\nsession:  herald-<ns>\ndisplay:  %s\n\n--- prompt ---\n%s\n", *instanceName, instanceID, displayName, prompt)
		return
	}

	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	nc, err := natsbus.Connect(natsbus.Options{URL: natsURL, Name: "ask"})
	if err != nil {
		log.Fatalf("ask: nats connect: %v", err)
	}
	defer nc.Close()

	sessionID := fmt.Sprintf("herald-%d", now.UnixNano())
	in := messages.BridgeInbound{
		BusSessionID: sessionID,
		Agent:        "herald",
		Orchestrator: "claude-code",
		Channel:      "herald",
		Timestamp:    now,
		Create: &msg.CreateSessionRequest{
			Harness:     msg.HarnessClaudeCode,
			InstanceID:  instanceID,
			DisplayName: displayName,
			Purpose:     "question",
			Origin:      "ask",
			// Autonomous, not interactive: llm-bridge-server only auto-allows tool
			// prehooks for autonomous sessions (isUnattendedSession). An interactive
			// Herald session parks the relay agent's first tool call awaiting human
			// approval and wedges the turn. The amber awaiting_user "?" status, the
			// session-list visibility, and replyability all come from the turn-end
			// heuristic and message flow — not from the session type — so autonomous
			// delivers the question UX while keeping the relay (and the post-reply
			// action turn) from hanging.
			Type:      msg.SessionTypeAutonomous,
			AutoStart: true,
		},
		Send: &msg.SendMessageRequest{
			Message: prompt,
		},
	}
	if err := nc.Publish(messages.BridgeInboundSubject, in); err != nil {
		log.Fatalf("ask: publish: %v", err)
	}
	fmt.Println(sessionID)
}

// buildPrompt frames the spawned agent as a relay: present the message, end the
// turn with the question (so the awaiting_user heuristic fires), then act on the
// reply if an on-reply instruction was given.
func buildPrompt(question, onReply string) string {
	var b strings.Builder
	b.WriteString(`You are Herald, relaying a question/alert to the user in their dashboard. This turn you must NOT answer the question yourself or take any action yet.

Do exactly this:
1. Present the message below to the user, clearly and in full.
2. End your turn with the question itself, phrased so it unmistakably awaits their answer (end on a line that finishes with "?"). Then stop and wait — do not continue working.

MESSAGE TO RELAY:
`)
	b.WriteString(question)
	b.WriteString("\n")
	if strings.TrimSpace(onReply) != "" {
		b.WriteString(`
WHEN THE USER REPLIES (next turn only — not now):
`)
		b.WriteString(strings.TrimSpace(onReply))
		b.WriteString("\nCarry that out, then briefly confirm what you did.\n")
	} else {
		b.WriteString(`
WHEN THE USER REPLIES (next turn only — not now): acknowledge their answer. No further action is expected.
`)
	}
	return b.String()
}

// resolveInstanceID looks up a harness instance by display name and returns its
// id. Fails loudly if zero or more than one instance carries the name, so a
// misconfigured inbox never silently routes questions to the wrong agent.
func resolveInstanceID(bridgeURL, name string) (string, error) {
	url := strings.TrimRight(bridgeURL, "/") + "/instances"
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /instances: HTTP %s — %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var instances []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &instances); err != nil {
		return "", fmt.Errorf("decode instances: %w", err)
	}
	var matches []string
	for _, i := range instances {
		if i.Name == name {
			matches = append(matches, i.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no instance named %q (create one with harness_type=claude_code)", name)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d instances named %q — name must be unique to route questions", len(matches), name)
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
