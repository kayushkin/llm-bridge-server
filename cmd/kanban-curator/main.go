// kanban-curator closes autoworker-tagged cards on the "Agent runs" kanban
// board when their bridge sessions reach a terminal state.
//
// Registered as a scheduler shell job running every ~5 minutes. Each tick:
//
//  1. Read the adapter's session map (~/.llm-bridge-adapter/sessions.json)
//     to translate the bus_session_id stored on each card into a bridge
//     session id.
//  2. Pull the "Agent runs" board view from kanban-store.
//  3. For each card in the In Progress column with a bus_session card_link:
//     - Look up the bridge session state via GET /sessions/{bridge_id}.
//     - completed → move to Done   (column has auto_status=done   → noteboard status=done)
//     - error     → move to Failed (column has auto_status=archived)
//     - else      → leave it (idle / running stays In Progress; the watchdog
//       will eventually move stalled ones to a Stalled column).
//
// Done/Failed transitions are append-only — once a card is in Done or Failed,
// it isn't revisited.
//
// Closure is idempotent: if a card has already been moved (e.g. by manual
// edit), or if its session can't be located, the curator logs and continues.
//
// Fire-and-forget design (matches autoworker): no state file needed; the
// kanban-store IS the state.
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
	defaultBoardName    = "Agent runs"
	columnInProgress    = "In Progress"
	columnDone          = "Done"
	columnFailed        = "Failed"
	tagAutoworker       = "autoworker"
	linkTypeBusSession  = "bus_session"
)

func main() {
	var (
		registerFlag  = flag.Bool("register", false, "Register/upsert this binary as a scheduler cron job and exit.")
		dryRun        = flag.Bool("dry-run", false, "Report decisions; do not write to kanban-store.")
		kanbanURL     = flag.String("kanban-url", envOr("KANBAN_STORE_URL", "http://localhost:8305"), "kanban-store base URL")
		bridgeURL     = flag.String("bridge-url", envOr("LLM_BRIDGE_URL", "http://localhost:8160"), "llm-bridge-server base URL")
		adapterFile   = flag.String("adapter-sessions", envOr("ADAPTER_SESSIONS_FILE", defaultAdapterSessionsPath()), "Path to llm-bridge-adapter sessions.json")
		boardName     = flag.String("board", defaultBoardName, "Kanban board name to curate")
	)
	flag.Parse()

	if *registerFlag {
		if err := registerSchedulerJob(); err != nil {
			log.Fatalf("register: %v", err)
		}
		return
	}

	mapping, err := readAdapterSessions(*adapterFile)
	if err != nil {
		log.Fatalf("read adapter sessions %s: %v", *adapterFile, err)
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
		var busSessionID string
		for _, l := range links {
			if l.EntityType == linkTypeBusSession {
				busSessionID = l.EntityRef
				break
			}
		}
		if busSessionID == "" {
			skipped++
			continue
		}
		bridgeID, ok := mapping[busSessionID]
		if !ok {
			log.Printf("[%s] no adapter mapping for bus_session=%s — skip", title, busSessionID)
			skipped++
			continue
		}
		state, ended, err := getSessionState(*bridgeURL, bridgeID)
		if err != nil {
			log.Printf("[%s] bridge GET /sessions/%s: %v — skip", title, bridgeID, err)
			errors++
			continue
		}

		var target, targetName string
		switch state {
		case "completed":
			target, targetName = doneCol, columnDone
		case "error":
			target, targetName = failedCol, columnFailed
		default:
			continue // idle / running / unknown — leave alone
		}

		log.Printf("[%s] %s → %s (bridge_id=%s)", title, state, targetName, bridgeID)
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

func defaultAdapterSessionsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".llm-bridge-adapter", "sessions.json")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// readAdapterSessions parses the adapter's bus_session_id → bridge_id map.
// File format: a single JSON object {bus_session_id: bridge_id, ...}.
func readAdapterSessions(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
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

func registerSchedulerJob() error {
	schedURL := envOr("SCHEDULER_URL", "http://localhost:8092")
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, "bin", "kanban-curator")

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
		if j.Name == "kanban-curator-tick" {
			fmt.Println("kanban-curator-tick already registered; no changes made")
			return nil
		}
	}
	payload := map[string]any{
		"name":        "kanban-curator-tick",
		"description": "Closes autoworker kanban cards when their bridge sessions reach a terminal state.",
		"schedule":    "*/5 * * * *",
		"type":        "shell",
		"command":     bin,
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
	fmt.Printf("registered kanban-curator-tick: %s\n", strings.TrimSpace(string(body)))
	return nil
}
