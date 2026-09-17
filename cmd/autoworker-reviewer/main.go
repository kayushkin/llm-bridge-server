// autoworker-reviewer nudges completed autoworker sessions to finish obvious
// remaining work before kanban-curator closes their cards.
//
// Autoworkers run a single autonomous turn: the agent works, its process exits,
// and the bridge flips the session to "completed". But "completed" only means
// "the process exited" — not "the task is actually finished". Agents frequently
// stop with self-contained work still on the table that they could have done in
// the same session without any user input.
//
// Registered as a scheduler shell job running every ~5 minutes (alongside
// kanban-curator). Each tick, for every In Progress card on the "Agent runs"
// board whose session_id is an autoworker session (prefix "autoworker-") and is
// in state "completed":
//
//   - passes < maxPasses → POST /sessions/{id}/send a self-review nudge. The
//     bridge transparently respawns the harness with `claude --resume <native
//     id>` (see llm-bridge-claudecode handler.go), so the SAME agent picks back
//     up with full context and either finishes the obvious remainder or stops.
//     We tag the session entity review-pass-N and leave the card In Progress.
//   - passes >= maxPasses → tag the session review-settled so the curator may
//     close the card. This is the hard loop guard against "almost done" agents
//     ping-ponging forever and burning budget.
//
// Handshake with kanban-curator: the curator defers closing an autoworker card
// to Done until the session carries the review-settled tag (or has been idle in
// "completed" past a grace window, in case this reviewer is down). The session
// entity tags are the shared state — no extra state file, matching the
// fire-and-forget design of autoworker/curator.
//
// Review is best-effort: if a nudge send fails, we settle the session anyway so
// the curator closes the card normally. Review must never wedge a card.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/slava/scheduler/internal/bridgeauth"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/slava/scheduler/internal/kanbanclient"
)

const (
	defaultBoardName  = "Agent runs"
	columnInProgress  = "In Progress"
	linkTypeSession   = "session"
	entityTypeSession = "session"

	// autoworkerPrefix is the canonical session_id prefix minted by autoworker's
	// publishWorker ("autoworker-<name>-<ns>"). Used as the origin gate so we
	// never nudge dispatcher ("tcl-") or attended sessions.
	autoworkerPrefix = "autoworker-"

	// maxPasses caps self-review continuations per session before we force
	// settle. Two nudges is the agreed budget/flap tradeoff.
	maxPasses = 2

	tagSettled    = "review-settled"
	tagPassPrefix = "review-pass-" // review-pass-1, review-pass-2, …
)

func main() {
	var (
		registerFlag = flag.Bool("register", false, "Register/upsert this binary as a scheduler cron job and exit.")
		jobName      = flag.String("job-name", "autoworker-reviewer-tick", "Scheduler job name for -register.")
		dryRun       = flag.Bool("dry-run", false, "Report decisions; do not nudge sessions or tag.")
		kanbanURL    = flag.String("kanban-url", envOr("KANBAN_STORE_URL", "http://localhost:8305"), "kanban-store base URL")
		bridgeURL    = flag.String("bridge-url", envOr("LLM_BRIDGE_URL", "http://localhost:8160"), "llm-bridge-server base URL")
		boardName    = flag.String("board", defaultBoardName, "Kanban board name to review")
	)
	flag.Parse()

	// llm-bridge-server gates every route; stamp this process's calls to it
	// with the service token. Host-scoped, so the scheduler API, noteboard and
	// kanban-store calls below are untouched.
	if err := bridgeauth.StampRequestsToBridge(*bridgeURL); err != nil {
		log.Fatal(err)
	}

	if *registerFlag {
		if err := registerSchedulerJob(*jobName, *boardName); err != nil {
			log.Fatalf("register: %v", err)
		}
		return
	}

	kc := kanbanclient.New(*kanbanURL)
	board, err := kc.FindBoard(*boardName)
	if err != nil {
		log.Fatalf("find board %q: %v", *boardName, err)
	}
	if board == nil {
		log.Printf("board %q does not exist yet — nothing to review", *boardName)
		return
	}

	view, err := kc.GetBoardView(board.ID)
	if err != nil {
		log.Fatalf("board view: %v", err)
	}

	var inProgress []kanbanclient.CardView
	for _, cv := range view.Columns {
		if cv.Column.Name == columnInProgress {
			inProgress = cv.Cards
			break
		}
	}
	log.Printf("[reviewer] board=%q in-progress=%d", *boardName, len(inProgress))

	var nudged, settled, skipped, errors int
	for _, c := range inProgress {
		if c.Item == nil {
			skipped++
			continue
		}
		title, _ := c.Item["title"].(string)

		sessionID, err := sessionLink(kc, c.Placement.CardID)
		if err != nil {
			log.Printf("[%s] list links: %v", title, err)
			errors++
			continue
		}
		// Origin gate: only autoworker sessions; skip dispatcher/attended cards.
		if sessionID == "" || !strings.HasPrefix(sessionID, autoworkerPrefix) {
			skipped++
			continue
		}

		state, _, err := getSessionState(*bridgeURL, sessionID)
		if err != nil {
			log.Printf("[%s] bridge GET /sessions/%s: %v — skip", title, sessionID, err)
			errors++
			continue
		}
		// Only completed sessions get reviewed. error/aborted/disconnected are the
		// curator's to fail; idle/running mean the agent is mid-flight.
		if state != "completed" {
			skipped++
			continue
		}

		tags, err := kc.ListEntityTags(entityTypeSession, sessionID)
		if err != nil {
			log.Printf("[%s] list entity tags: %v — skip", title, err)
			errors++
			continue
		}
		if hasTag(tags, tagSettled) {
			skipped++ // already settled; awaiting curator close
			continue
		}
		passes := passCount(tags)

		if passes >= maxPasses {
			log.Printf("[%s] %d/%d passes done → settle (session_id=%s)", title, passes, maxPasses, sessionID)
			if *dryRun {
				continue
			}
			if err := settle(kc, c, sessionID, fmt.Sprintf("review loop guard hit (%d passes)", passes)); err != nil {
				log.Printf("[%s] settle: %v", title, err)
				errors++
				continue
			}
			settled++
			continue
		}

		pass := passes + 1
		log.Printf("[%s] completed, sending self-review nudge %d/%d (session_id=%s)", title, pass, maxPasses, sessionID)
		if *dryRun {
			continue
		}
		if err := sendMessage(*bridgeURL, sessionID, reviewNudge(pass, maxPasses)); err != nil {
			// Best-effort: never wedge the card on a send failure. Settle so the
			// curator closes it as it would have without review.
			log.Printf("[%s] nudge send failed: %v — settling so curator can close", title, err)
			if serr := settle(kc, c, sessionID, fmt.Sprintf("review nudge send failed: %v", err)); serr != nil {
				log.Printf("[%s] settle after send failure: %v", title, serr)
			}
			errors++
			continue
		}
		if err := kc.AddEntityTag(entityTypeSession, sessionID, fmt.Sprintf("%s%d", tagPassPrefix, pass)); err != nil {
			log.Printf("[%s] tag review-pass-%d: %v", title, pass, err)
		}
		stampBody(kc, c, fmt.Sprintf("self-review nudge %d/%d sent", pass, maxPasses))
		nudged++
	}
	log.Printf("[reviewer] done: nudged=%d settled=%d skipped=%d errors=%d", nudged, settled, skipped, errors)
}

// ============================ review logic helpers ============================

// reviewNudge is the self-review prompt sent to a completed autoworker session.
// Sending it respawns the agent with full prior context (claude --resume).
func reviewNudge(pass, max int) string {
	return fmt.Sprintf(`Automated self-review check (pass %d of %d).

You earlier finished a turn on this autoworker task and your process exited, so it's queued to be marked complete. Before it closes:

1. Re-read the original task and what you actually did this session.
2. If there is obvious, self-contained work still remaining that you can finish RIGHT NOW without any user input or decision, do it now and update the noteboard item (PATCH http://localhost:8191/api/items/<id>) to reflect the new state.
3. If the task is genuinely complete, or the only work left needs a user decision / is blocked / is out of scope, do nothing further and state in one line why you're done.

Do not invent or pad scope — only finish work that clearly belongs to this exact task. No chat; just work or sign off.`, pass, max)
}

// settle tags the session review-settled (so the curator may close the card)
// and stamps a short audit note on the card body.
func settle(kc *kanbanclient.Client, c kanbanclient.CardView, sessionID, reason string) error {
	if err := kc.AddEntityTag(entityTypeSession, sessionID, tagSettled); err != nil {
		return err
	}
	stampBody(kc, c, "review settled — "+reason)
	return nil
}

// stampBody appends a single timestamped reviewer line to the card body. Best
// effort: a failed stamp is logged but never blocks the decision.
func stampBody(kc *kanbanclient.Client, c kanbanclient.CardView, note string) {
	body, _ := c.Item["body"].(string)
	suffix := fmt.Sprintf("\n\n---\n_autoworker-reviewer %s: %s_\n",
		time.Now().UTC().Format(time.RFC3339), note)
	if err := kc.PatchCardBody(c.Placement.CardID, body+suffix); err != nil {
		title, _ := c.Item["title"].(string)
		log.Printf("[%s] stamp body: %v (continuing)", title, err)
	}
}

func sessionLink(kc *kanbanclient.Client, cardID string) (string, error) {
	links, err := kc.ListCardLinks(cardID)
	if err != nil {
		return "", err
	}
	for _, l := range links {
		if l.EntityType == linkTypeSession {
			return l.EntityRef, nil
		}
	}
	return "", nil
}

func hasTag(tags []kanbanclient.EntityTag, want string) bool {
	for _, t := range tags {
		if t.Tag == want {
			return true
		}
	}
	return false
}

// passCount returns how many review-pass-N tags are present on the session.
func passCount(tags []kanbanclient.EntityTag) int {
	n := 0
	for _, t := range tags {
		if strings.HasPrefix(t.Tag, tagPassPrefix) {
			n++
		}
	}
	return n
}

// ============================ bridge client ============================

func getSessionState(bridgeURL, sessionID string) (state string, updatedAt string, err error) {
	url := fmt.Sprintf("%s/sessions/%s", strings.TrimRight(bridgeURL, "/"), sessionID)
	resp, err := http.Get(url)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", "", fmt.Errorf("not found")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("HTTP %s — %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var s struct {
		State     string `json:"state"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return "", "", err
	}
	return s.State, s.UpdatedAt, nil
}

// sendMessage posts a follow-up message to a session. For a completed session
// (process exited) the bridge transparently respawns the harness with --resume
// before delivering the message, so the agent resumes with full context.
func sendMessage(bridgeURL, sessionID, message string) error {
	url := fmt.Sprintf("%s/sessions/%s/send", strings.TrimRight(bridgeURL, "/"), sessionID)
	payload, _ := json.Marshal(map[string]any{"message": message})
	resp, err := http.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %s — %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// ============================ helpers ============================

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ============================ scheduler self-registration ============================

func registerSchedulerJob(jobName, boardName string) error {
	schedURL := envOr("SCHEDULER_URL", "http://localhost:8092")
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, "bin", "autoworker-reviewer")
	command := bin
	if boardName != defaultBoardName {
		command = fmt.Sprintf("%s -board=%q", bin, boardName)
	}

	resp, err := http.Get(schedURL + "/api/jobs")
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var jobs []struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(body, &jobs)
	for _, j := range jobs {
		if j.Name == jobName {
			fmt.Printf("%s already registered; no changes made\n", jobName)
			return nil
		}
	}
	payload := map[string]any{
		"name":        jobName,
		"description": "Nudges completed autoworker sessions to finish obvious remaining work before kanban-curator closes their cards.",
		"schedule":    "*/5 * * * *",
		"type":        "shell",
		"command":     command,
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", schedURL+"/api/jobs", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST /api/jobs: %w", err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("scheduler returned %d: %s", resp.StatusCode, string(body))
	}
	fmt.Printf("registered %s: %s\n", jobName, strings.TrimSpace(string(body)))
	return nil
}
