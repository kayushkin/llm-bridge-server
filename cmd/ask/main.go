// ask spawns a "herald" session that relays a question or alert to the user in
// the dashboard and then waits for their reply.
//
// It is the reusable engagement primitive for agents and scheduled jobs that
// need to pull the user into the loop on something they didn't open themselves —
// e.g. the approval proposer asking "approve this update?", or a job raising an
// alert. The spawned agent presents the message and ends its turn with the
// question, which trips llm-bridge-server's awaiting_user heuristic so the
// session shows up in the dash session list with the amber "?" status. When the
// user replies, the bridge resumes the same agent (claude --resume) with full
// context to carry out the optional follow-up action.
//
// Design notes:
//   - Runs on the machine's existing claude_code instance (resolved by harness
//     type + local machine, or --instance by name). There is NO dedicated CC
//     instance — one per machine is enough; "herald" is a role, marked by the
//     herald- session-id prefix and Purpose="herald", not a separate instance.
//   - Type=herald: llm-bridge-server's isUnattendedSession treats herald like
//     autonomous, so the relay agent's tool calls auto-allow (keeping deny
//     guardrails) instead of parking on the prehook and wedging the turn — no
//     permission_mode=bypass needed. The herald- prefix keeps these out of the
//     kanban-curator idle-reaper (scoped to autoworker-).
//   - Attribution is structured: --from sets the message's Agent tag (the
//     sender shown in the UI). The prompt does not roleplay a persona.
//   - Session reuse: --session <herald-...> delivers the message as a new turn
//     in an existing herald session instead of opening another one. The caller
//     keeps the id ask printed and passes it back; reminder-coordinator uses
//     this so a nag firing after the morning digest lands in the digest's
//     session rather than a second "Needs you" entry with the same title.
//
// Usage:
//
//	ask --from approval-proposer \
//	    --on-reply "If yes, move kanban card <id> from 'Needs approval' to 'Queued'; if no, append their reason to the card body." \
//	    "Approve updating llm-bridge-gemini to add a retry on transient stream errors?"
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/bus/messages"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/slava/scheduler/internal/bridgeauth"
	schedulerconfig "github.com/slava/scheduler/internal/config"
)

func main() {
	settings, err := schedulerconfig.Ask.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatal(err)
	}
	var (
		instanceName = flag.String("instance", "", "Harness instance NAME to host the session. Empty = resolve the local machine's claude_code instance.")
		from         = flag.String("from", "herald", "Originating agent — sets the message's Agent (sender) tag shown in the dash. Use the real agent name, e.g. approval-proposer.")
		title        = flag.String("title", "", "Session display name. Defaults to a truncated form of the question.")
		onReply      = flag.String("on-reply", "", "Instruction for what the agent should do once the user replies. Empty = just relay the alert; no action expected.")
		reuseSession = flag.String("session", "", "Existing herald bus session id to deliver into as a new turn, instead of opening a new session. The adapter routes it to the bridge session it created for that id; -instance and -title are meaningless here and rejected.")
		bridgeURL    = flag.String("bridge-url", settings.String(schedulerconfig.SettingBridgeURL), "llm-bridge-server base URL (for instance resolution).")
		dryRun       = flag.Bool("dry-run", false, "Print the resolved instance and prompt; do not publish.")
	)
	flag.Parse()

	// llm-bridge-server gates every route; stamp this process's calls to it
	// with the service token. Host-scoped, so the scheduler API, noteboard and
	// kanban-store calls below are untouched.
	if err := bridgeauth.StampRequestsToBridge(*bridgeURL, settings.String(schedulerconfig.SettingBridgeServiceToken)); err != nil {
		log.Fatal(err)
	}

	question := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if question == "" {
		log.Fatal("ask: a question/alert message is required (positional argument)")
	}
	if strings.TrimSpace(*from) == "" {
		log.Fatal("ask: --from (originating agent tag) must not be empty")
	}

	now := time.Now().UTC()
	prompt := buildPrompt(*from, question, *onReply)

	// Reuse: deliver into an existing herald session as a new turn. The adapter
	// keeps a persisted bus_session_id → bridge session mapping and reuses it
	// for a Send with no Create, so the message lands in the same chat session
	// the earlier ask opened. Create-time knobs cannot apply to a session that
	// already exists, so passing one is an error, not something to ignore.
	if *reuseSession != "" {
		if *instanceName != "" {
			log.Fatal("ask: -instance sets where a NEW session is hosted; it cannot apply with -session")
		}
		if *title != "" {
			log.Fatal("ask: -title names a NEW session at creation; it cannot apply with -session")
		}
		if *dryRun {
			fmt.Printf("session:  %s (reused)\nfrom:     %s\n\n--- prompt ---\n%s\n", *reuseSession, *from, prompt)
			return
		}
		publishInbound(settings.String(schedulerconfig.SettingNATSURL), messages.BridgeInbound{
			BusSessionID: *reuseSession,
			Agent:        *from,
			Orchestrator: "claude-code",
			Channel:      "herald",
			Timestamp:    now,
			Send: &msg.SendMessageRequest{
				Message: prompt,
			},
		})
		fmt.Println(*reuseSession)
		return
	}

	instanceID, err := resolveInstance(*bridgeURL, *instanceName)
	if err != nil {
		log.Fatalf("ask: resolve instance: %v", err)
	}

	displayName := *title
	if displayName == "" {
		displayName = truncate(question, 60)
	}

	if *dryRun {
		fmt.Printf("instance: %s\nfrom:     %s\nsession:  herald-<ns>\ndisplay:  %s\n\n--- prompt ---\n%s\n", instanceID, *from, displayName, prompt)
		return
	}

	sessionID := fmt.Sprintf("herald-%d", now.UnixNano())
	publishInbound(settings.String(schedulerconfig.SettingNATSURL), messages.BridgeInbound{
		BusSessionID: sessionID,
		Agent:        *from,
		Orchestrator: "claude-code",
		Channel:      "herald",
		Timestamp:    now,
		Create: &msg.CreateSessionRequest{
			Harness:     msg.HarnessClaudeCode,
			InstanceID:  instanceID,
			DisplayName: displayName,
			Purpose:     "herald",
			// herald is unattended for the relay turn (no human resolves a tool
			// prehook until the user replies), so llm-bridge-server's
			// isUnattendedSession auto-allows tool calls while keeping deny
			// guardrails — no permission_mode=bypass needed.
			Type:      msg.SessionTypeHerald,
			Origin:    "ask",
			AutoStart: true,
		},
		Send: &msg.SendMessageRequest{
			Message: prompt,
		},
	})
	fmt.Println(sessionID)
}

// publishInbound connects to NATS, publishes the request, and exits the process
// on any failure — ask is a one-shot; there is nothing to clean up.
func publishInbound(natsURL string, in messages.BridgeInbound) {
	nc, err := natsbus.Connect(natsbus.Options{URL: natsURL, Name: "ask"})
	if err != nil {
		log.Fatalf("ask: nats connect: %v", err)
	}
	defer nc.Close()
	if err := nc.Publish(messages.BridgeInboundSubject, in); err != nil {
		log.Fatalf("ask: publish: %v", err)
	}
}

// buildPrompt frames the spawned agent as a relay: present the message
// (attributed to `from`), end the turn with the question so the awaiting_user
// heuristic fires, then act on the reply only if an on-reply instruction exists.
// No persona roleplay — attribution rides the Agent tag, not the prose.
func buildPrompt(from, question, onReply string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `Relay the following message from `+"`%s`"+` to the user, then wait. This turn: present the message in full and end on the question itself (a line finishing with "?") so it clearly awaits their answer. Do not answer it yourself or take any action yet.

MESSAGE:
`, from)
	b.WriteString(question)
	b.WriteString("\n")
	if strings.TrimSpace(onReply) != "" {
		b.WriteString("\nWHEN THE USER REPLIES (next turn only — not now):\n")
		b.WriteString(strings.TrimSpace(onReply))
		b.WriteString("\nCarry that out, then briefly confirm what you did.\n")
	} else {
		b.WriteString("\nWHEN THE USER REPLIES (next turn only — not now): acknowledge their answer. No further action is expected.\n")
	}
	return b.String()
}

type instanceView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	HarnessType string `json:"harness_type"`
	Enabled     bool   `json:"enabled"`
	Machine     *struct {
		Transport string `json:"transport"`
	} `json:"machine"`
}

// resolveInstance returns the instance id to host the session. With a name, it
// resolves by display name (must be unique). Without a name, it resolves the
// local machine's single enabled claude_code instance — one CC instance per
// machine is the rule, so there should be exactly one. Fails loudly on zero or
// ambiguous matches so a question never silently routes to the wrong agent.
func resolveInstance(bridgeURL, name string) (string, error) {
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
	var instances []instanceView
	if err := json.Unmarshal(body, &instances); err != nil {
		return "", fmt.Errorf("decode instances: %w", err)
	}

	var matches []string
	if name != "" {
		for _, i := range instances {
			if i.Name == name {
				matches = append(matches, i.ID)
			}
		}
		switch len(matches) {
		case 0:
			return "", fmt.Errorf("no instance named %q", name)
		case 1:
			return matches[0], nil
		default:
			return "", fmt.Errorf("%d instances named %q — name must be unique", len(matches), name)
		}
	}

	// No name: the local machine's claude_code instance.
	for _, i := range instances {
		if i.HarnessType != string(msg.HarnessClaudeCode) || !i.Enabled {
			continue
		}
		if i.Machine == nil || i.Machine.Transport != "local" {
			continue
		}
		matches = append(matches, i.ID)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no enabled local claude_code instance found (pass --instance <name>)")
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d local claude_code instances — pass --instance <name> to disambiguate", len(matches))
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
