// kanban-curator closes autoworker-tagged cards on the "Agent runs" kanban
// board when their bridge sessions reach a terminal state.
//
// Registered as a scheduler shell job running every ~5 minutes. Each tick:
//
//  1. Pull the "Agent runs" board view from kanban-store.
//  2. For each card in the In Progress column with a session card_link:
//     - Look up the bridge session state via GET /sessions/{session_id}.
//     - completed                          → move to Done   (column has auto_status=done   → noteboard status=done)
//     - error / aborted / disconnected     → move to Failed (column has auto_status=archived)
//     - else                               → leave it (idle / running stays In Progress; the watchdog
//       will eventually move stalled ones to a Stalled column).
//
// Done/Failed transitions are append-only — once a card is in Done or Failed,
// it isn't revisited.
//
// Closure is idempotent: if a card has already been moved (e.g. by manual
// edit), or if its session can't be located, the curator logs and continues.
//
// Fire-and-forget design (matches autoworker): no state file needed; the
// kanban-store IS the state. Post session-consolidation (2026-05-09) the
// session card_link entity_ref is the canonical llm-bridge-server session_id,
// so no adapter translation step is needed.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
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
	defaultBoardName = "Agent runs"
	columnInProgress = "In Progress"
	columnDone       = "Done"
	columnFailed     = "Failed"
	tagAutoworker    = "autoworker"
	linkTypeSession  = "session"
)

func main() {
	var (
		registerFlag = flag.Bool("register", false, "Register/upsert this binary as a scheduler cron job and exit. Honors -board and -job-name.")
		jobName      = flag.String("job-name", "", "Scheduler job name for -register. Defaults to kanban-curator-tick for the default board, kanban-curator-<slug>-tick otherwise.")
		dryRun       = flag.Bool("dry-run", false, "Report decisions; do not write to kanban-store.")
		kanbanURL    = flag.String("kanban-url", envOr("KANBAN_STORE_URL", "http://localhost:8305"), "kanban-store base URL")
		bridgeURL    = flag.String("bridge-url", envOr("LLM_BRIDGE_URL", "http://localhost:8160"), "llm-bridge-server base URL")
		boardName    = flag.String("board", defaultBoardName, "Kanban board name to curate")
	)
	flag.Parse()

	if *registerFlag {
		if err := registerSchedulerJob(*boardName, *jobName); err != nil {
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
		log.Printf("board %q does not exist yet — nothing to curate", *boardName)
		return
	}

	view, err := kc.GetBoardView(board.ID)
	if err != nil {
		log.Fatalf("board view: %v", err)
	}

	colByName := map[string]string{}
	for _, cv := range view.Columns {
		colByName[cv.Column.Name] = cv.Column.ID
	}
	doneCol, ok1 := colByName[columnDone]
	failedCol, ok2 := colByName[columnFailed]
	if !ok1 || !ok2 {
		log.Fatalf("board %q missing required columns (Done=%v Failed=%v)", *boardName, ok1, ok2)
	}

	var inProgress []kanbanclient.CardView
	for _, cv := range view.Columns {
		if cv.Column.Name != columnInProgress {
			continue
		}
		inProgress = cv.Cards
		break
	}
	log.Printf("[curator] board=%q in-progress=%d", *boardName, len(inProgress))

	var moved, skipped, errors int
	for _, c := range inProgress {
		if c.Item == nil {
			skipped++
			continue
		}
		title, _ := c.Item["title"].(string)
		links, err := kc.ListCardLinks(c.Placement.CardID)
		if err != nil {
			log.Printf("[%s] list links: %v", title, err)
			errors++
			continue
		}
		var sessionID string
		for _, l := range links {
			if l.EntityType == linkTypeSession {
				sessionID = l.EntityRef
				break
			}
		}
		if sessionID == "" {
			skipped++
			continue
		}
		state, ended, err := getSessionState(*bridgeURL, sessionID)
		if err != nil {
			log.Printf("[%s] bridge GET /sessions/%s: %v — skip", title, sessionID, err)
			errors++
			continue
		}

		var target, targetName string
		switch state {
		case "completed":
			target, targetName = doneCol, columnDone
		case "error", "aborted", "disconnected":
			target, targetName = failedCol, columnFailed
		default:
			continue // idle / running / unknown — leave alone
		}

		log.Printf("[%s] %s → %s (session_id=%s)", title, state, targetName, sessionID)
		if *dryRun {
			continue
		}

		if ended != "" {
			body, _ := c.Item["body"].(string)
			suffix := fmt.Sprintf("\n\n---\n_closed by kanban-curator at %s; bridge state=%s, ended_at=%s_\n",
				time.Now().UTC().Format(time.RFC3339), state, ended)
			if err := kc.PatchCardBody(c.Placement.CardID, body+suffix); err != nil {
				log.Printf("[%s] patch body: %v (continuing to move)", title, err)
			}
		}
		if err := kc.MoveCard(c.Placement.CardID, board.ID, target, 0); err != nil {
			log.Printf("[%s] move card: %v", title, err)
			errors++
			continue
		}
		moved++
	}
	log.Printf("[curator] done: moved=%d skipped=%d errors=%d", moved, skipped, errors)
}

// ============================ helpers ============================

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// getSessionState fetches one session from llm-bridge-server and returns
// (state, updated_at, err). updated_at is the most recent state transition we
// can observe — used as an "ended at" hint when stamping the closed-card body.
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

// ============================ scheduler self-registration ============================

// registerSchedulerJob registers (idempotently) a kanban-curator cron job for
// the given board. The job name defaults to "kanban-curator-tick" for the
// default "Agent runs" board, or "kanban-curator-<slug>-tick" for any other
// board, where slug is the board name lowercased with non-alphanumerics
// collapsed to '-'. Pass jobName explicitly to override.
//
// The registered command preserves a -board="..." flag when the curator is
// targeted at a non-default board so each cron tick curates the right board.
func registerSchedulerJob(boardName, jobName string) error {
	schedURL := envOr("SCHEDULER_URL", "http://localhost:8092")
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, "bin", "kanban-curator")

	if jobName == "" {
		if boardName == defaultBoardName {
			jobName = "kanban-curator-tick"
		} else {
			jobName = "kanban-curator-" + slugify(boardName) + "-tick"
		}
	}
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
		"description": fmt.Sprintf("Closes autoworker kanban cards on board %q when their bridge sessions reach a terminal state.", boardName),
		"schedule":    "*/5 * * * *",
		"type":        "shell",
		"command":     command,
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", schedURL+"/api/jobs", strings.NewReader(string(data)))
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

// slugify lowercases s and collapses runs of non-alphanumeric characters to a
// single '-', trimming leading/trailing dashes. Used to derive a stable job
// name from a kanban board name.
func slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
