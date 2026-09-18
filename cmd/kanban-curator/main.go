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
//     will eventually move stalled ones to a Stalled column).
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
	"errors"
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
	// agentRunsBoardName is the board the first curator job was registered
	// for, back when the job took a board name and this one was its default.
	// It survives only in registerSchedulerJob's job-name rule, so that the
	// job for that board keeps its original name ("kanban-curator-tick") and
	// updates in place rather than registering a second job beside it. It is
	// never used to find a board — boards are found by id.
	agentRunsBoardName = "Agent runs"
	columnInProgress   = "In Progress"
	columnDone         = "Done"
	columnFailed       = "Failed"
	tagAutoworker      = "autoworker"
	linkTypeSession    = "session"

	// autoworkerPrefix is the canonical session_id prefix minted by autoworker.
	// Cards whose session matches it are held from closing to Done until the
	// autoworker-reviewer marks the session review-settled (see reviewSettled).
	autoworkerPrefix  = "autoworker-"
	entityTypeSession = "session"
	tagReviewSettled  = "review-settled"

	// reviewGrace is how long a completed autoworker session may sit unsettled
	// before the curator closes it anyway. Fail-safe against the reviewer being
	// down/disabled — review must never permanently wedge a card. The reviewer
	// bumps the session's updated_at on every nudge, so an actively-reviewed
	// card keeps resetting this clock.
	reviewGrace = 30 * time.Minute
)

func main() {
	var (
		registerFlag = flag.Bool("register", false, "Register/upsert this binary as a scheduler cron job and exit. Honors -board-id and -job-name.")
		jobName      = flag.String("job-name", "", "Scheduler job name for -register. Defaults to kanban-curator-tick for the \"Agent runs\" board, kanban-curator-<slug of the board name>-tick otherwise.")
		dryRun       = flag.Bool("dry-run", false, "Report decisions; do not write to kanban-store.")
		kanbanURL    = flag.String("kanban-url", envOr("KANBAN_STORE_URL", "http://localhost:8305"), "kanban-store base URL")
		bridgeURL    = flag.String("bridge-url", envOr("LLM_BRIDGE_URL", "http://localhost:8160"), "llm-bridge-server base URL")
		boardID      = flag.String("board-id", "", "Kanban board id to curate (required). Pinned by id so a rename in the UI cannot fork a second board.")
		staleAfter   = flag.Duration("stale-after", 2*time.Hour, "Reap an In Progress card whose session is idle/gone and has had no activity for longer than this.")
	)
	flag.Parse()

	// llm-bridge-server gates every route; stamp this process's calls to it
	// with the service token. Host-scoped, so noteboard, kanban-store and the
	// scheduler API are untouched.
	if err := bridgeauth.StampRequestsToBridge(*bridgeURL); err != nil {
		log.Fatal(err)
	}

	if *boardID == "" {
		log.Fatal("-board-id is required; there is no board to default to")
	}

	kc := kanbanclient.New(*kanbanURL)
	board, err := kc.GetBoard(*boardID)
	if errors.Is(err, kanbanclient.ErrBoardNotFound) {
		log.Fatalf("board %s does not exist — refusing to create a replacement, since a second board is exactly the fork pinning the id prevents", *boardID)
	}
	if err != nil {
		log.Fatalf("get board %s: %v", *boardID, err)
	}

	if *registerFlag {
		if err := registerSchedulerJob(board, *jobName); err != nil {
			log.Fatalf("register: %v", err)
		}
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
		log.Fatalf("board %q missing required columns (Done=%v Failed=%v)", board.Name, ok1, ok2)
	}

	var inProgress []kanbanclient.CardView
	for _, cv := range view.Columns {
		if cv.Column.Name != columnInProgress {
			continue
		}
		inProgress = cv.Cards
		break
	}
	log.Printf("[curator] board=%q id=%s in-progress=%d", board.Name, board.ID, len(inProgress))

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
		state, ended, notFound, err := getSessionState(*bridgeURL, sessionID)
		if err != nil && !notFound {
			log.Printf("[%s] bridge GET /sessions/%s: %v — skip", title, sessionID, err)
			errors++
			continue
		}

		var target, targetName, reapNote string
		switch {
		case notFound:
			// The bridge session no longer exists (archived/pruned). It can never
			// reach a terminal state, so left alone the card wedges In Progress
			// forever — reap it to Failed.
			target, targetName = failedCol, columnFailed
			reapNote = "reaped: bridge session not found (archived/pruned)"
		case state == "completed":
			// Handshake with autoworker-reviewer: don't close an autoworker card
			// to Done until the reviewer has signed off (review-settled tag), so
			// the agent gets its self-review pass to finish obvious leftover work.
			// Bounded by reviewGrace so a down reviewer can't wedge the card.
			if strings.HasPrefix(sessionID, autoworkerPrefix) && !reviewSettled(kc, sessionID) && !staleCompleted(ended) {
				log.Printf("[%s] completed; awaiting autoworker-reviewer sign-off — leaving In Progress (session_id=%s)", title, sessionID)
				skipped++
				continue
			}
			target, targetName = doneCol, columnDone
		case state == "error", state == "aborted", state == "disconnected":
			target, targetName = failedCol, columnFailed
		case state == "idle", state == "awaiting_permission", state == "awaiting_user":
			// An autoworker session sitting in a non-terminal wait state past
			// staleAfter is abandoned: idle = finished its turn and stopped;
			// awaiting_permission/awaiting_user = parked on a prompt that no human
			// will ever answer in an unattended run (normally auto-allow prevents
			// this, but a permission-store blip can still strand one). Reap it so it
			// doesn't wedge In Progress forever. A still-fresh waiter is left alone in
			// case the agent (or the reviewer) resumes it.
			//
			// Scoped to autoworker-origin sessions: on the Task-completion-loop board
			// the dispatcher deliberately parks sessions idle to revive later, so we
			// must not reap those out from under it. A vanished session (notFound) is
			// still reaped on any board above, since it can't be revived anywhere.
			if !strings.HasPrefix(sessionID, autoworkerPrefix) {
				continue
			}
			if !staleSince(ended, *staleAfter) {
				continue
			}
			target, targetName = failedCol, columnFailed
			reapNote = fmt.Sprintf("reaped: %s with no activity since %s (> %s)", state, ended, *staleAfter)
		default:
			continue // running / unknown — leave alone
		}

		if reapNote != "" {
			log.Printf("[%s] %s → %s (%s) (session_id=%s)", title, stateLabel(state, notFound), targetName, reapNote, sessionID)
		} else {
			log.Printf("[%s] %s → %s (session_id=%s)", title, state, targetName, sessionID)
		}
		if *dryRun {
			continue
		}

		{
			body, _ := c.Item["body"].(string)
			var suffix string
			if reapNote != "" {
				suffix = fmt.Sprintf("\n\n---\n_kanban-curator at %s: %s_\n",
					time.Now().UTC().Format(time.RFC3339), reapNote)
			} else if ended != "" {
				suffix = fmt.Sprintf("\n\n---\n_closed by kanban-curator at %s; bridge state=%s, ended_at=%s_\n",
					time.Now().UTC().Format(time.RFC3339), state, ended)
			}
			if suffix != "" {
				if err := kc.PatchCardBody(c.Placement.CardID, body+suffix); err != nil {
					log.Printf("[%s] patch body: %v (continuing to move)", title, err)
				}
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
// (state, updated_at, notFound, err). updated_at is the most recent state
// transition we can observe — used as an "ended at" hint when stamping the
// closed-card body. notFound is true (with a nil-equivalent error) when the
// bridge has no such session, which the caller treats as a reapable terminal
// condition rather than a transient error.
func getSessionState(bridgeURL, sessionID string) (state string, updatedAt string, notFound bool, err error) {
	url := fmt.Sprintf("%s/sessions/%s", strings.TrimRight(bridgeURL, "/"), sessionID)
	resp, err := http.Get(url)
	if err != nil {
		return "", "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", "", true, fmt.Errorf("not found")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", false, fmt.Errorf("HTTP %s — %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var s struct {
		State     string `json:"state"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return "", "", false, err
	}
	return s.State, s.UpdatedAt, false, nil
}

// staleSince reports whether updatedAt (RFC3339) is older than d. An empty or
// unparseable timestamp is treated as not-stale, so a card is never reaped on
// the basis of a missing timestamp alone.
func staleSince(updatedAt string, d time.Duration) bool {
	if updatedAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return false
	}
	return time.Since(t) > d
}

// stateLabel renders the session state for logging, substituting a marker when
// the session could not be found.
func stateLabel(state string, notFound bool) string {
	if notFound {
		return "not-found"
	}
	return state
}

// reviewSettled reports whether the autoworker-reviewer has signed off on this
// session (review-settled entity tag present). On a tag-lookup error it returns
// false so the grace window — not a transient kanban-store hiccup — decides.
func reviewSettled(kc *kanbanclient.Client, sessionID string) bool {
	tags, err := kc.ListEntityTags(entityTypeSession, sessionID)
	if err != nil {
		log.Printf("[curator] list entity tags for %s: %v — treating as unsettled", sessionID, err)
		return false
	}
	for _, t := range tags {
		if t.Tag == tagReviewSettled {
			return true
		}
	}
	return false
}

// staleCompleted reports whether a session has been in its terminal state longer
// than reviewGrace, based on the bridge updated_at hint. Used as the fail-safe
// to close autoworker cards the reviewer never settled (e.g. reviewer down). An
// unparseable/empty timestamp is treated as not-stale so we keep deferring.
func staleCompleted(updatedAt string) bool {
	if updatedAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return false
	}
	return time.Since(t) > reviewGrace
}

// ============================ scheduler self-registration ============================

// registerSchedulerJob registers (idempotently) a kanban-curator cron job for
// the given board, fetched by id so its name is the one the store holds today.
// The job name is derived from that name exactly as it always was —
// "kanban-curator-tick" for the "Agent runs" board, "kanban-curator-<slug>-tick"
// for any other, where slug is the board name lowercased with non-alphanumerics
// collapsed to '-' — so a re-register finds the existing job and updates in
// place instead of registering a second one. Pass jobName explicitly to
// override.
//
// The registered command always carries -board-id: the scheduler runs jobs as
// `sh -c` with its own thin environment, so nothing else would deliver it.
func registerSchedulerJob(board *kanbanclient.Board, jobName string) error {
	schedURL := envOr("SCHEDULER_URL", "http://localhost:8092")
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, "bin", "kanban-curator")

	if jobName == "" {
		jobName = defaultJobName(board.Name)
	}
	command := bin + " -board-id " + board.ID
	boardName := board.Name

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

// defaultJobName is the job-name rule registerSchedulerJob applies when no
// -job-name is given. A pure function so a test can pin that the names the
// live jobs carry today are the ones a re-register derives.
func defaultJobName(boardName string) string {
	if boardName == agentRunsBoardName {
		return "kanban-curator-tick"
	}
	return "kanban-curator-" + slugify(boardName) + "-tick"
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
