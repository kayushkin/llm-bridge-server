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
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	schedulerconfig "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/config"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/boardvocabulary"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/bridgeauth"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/bridgesession"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/degradations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/schedulerjobs"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

const (
	// agentRunsBoardName is the board the first curator job was registered
	// for, back when the job took a board name and this one was its default.
	// It survives only in registerSchedulerJob's job-name rule, so that the
	// job for that board keeps its original name ("kanban-curator-tick") and
	// updates in place rather than registering a second job beside it. It is
	// never used to find a board — boards are found by id.
	agentRunsBoardName = boardvocabulary.BoardAgentRuns
	columnInProgress   = boardvocabulary.ColumnInProgress
	columnDone         = boardvocabulary.ColumnDone
	columnFailed       = boardvocabulary.ColumnFailed
	tagAutoworker      = "autoworker"
	linkTypeSession    = boardvocabulary.LinkTypeSession

	// purposeAutoworker is the value llm-bridge-server stores in a session's
	// "purpose" field for a session autoworker created. Cards whose session
	// carries it are held from closing to Done until the autoworker-reviewer
	// marks the session review-settled (see reviewSettled), and are the only
	// cards this binary reaps out of a non-terminal wait state.
	//
	// Both gates used to prefix-match the session_id against "autoworker-".
	// llm-bridge-server owns the answer to "what created this session" and
	// publishes it as a field, so matching a naming convention embedded in the
	// id was a name-based join onto a record that has an authoritative one.
	// Measured 2026-08-08 across all 8,669 live sessions: 4,988 carry
	// purpose="autoworker" and 613 of them (12.3%) have a session_id that does
	// NOT start with "autoworker-". Every one of those 613 was created between
	// 2026-04-23 and 2026-06-03, so the id convention changed under the gate
	// once already and nothing here could see it happen.
	purposeAutoworker = "autoworker"
	entityTypeSession = boardvocabulary.EntityTypeSession
	tagReviewSettled  = "review-settled"

	// reviewGrace is how long a completed autoworker session may sit unsettled
	// before the curator closes it anyway. Fail-safe against the reviewer being
	// down/disabled — review must never permanently wedge a card. The reviewer
	// bumps the session's updated_at on every nudge, so an actively-reviewed
	// card keeps resetting this clock.
	reviewGrace = 30 * time.Minute
)

// config names the two services this curator talks to, the board it curates and
// whether it is allowed to write. It exists so run can be handed stubs by a
// test; main fills it from flags and the service settings.
type config struct {
	kanbanURL          string
	kanbanServiceToken string
	bridgeURL          string
	boardID            string
	staleAfter         time.Duration
	dryRun             bool
}

func main() {
	settings, err := schedulerconfig.KanbanCurator.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatal(err)
	}
	var (
		registerFlag = flag.Bool("register", false, "Register/upsert this binary as a scheduler cron job and exit. Honors -board-id and -job-name.")
		jobName      = flag.String("job-name", "", "Scheduler job name for -register. Defaults to kanban-curator-tick for the \"Agent runs\" board, kanban-curator-<slug of the board name>-tick otherwise.")
		dryRun       = flag.Bool("dry-run", false, "Report decisions; do not write to kanban-store.")
		kanbanURL    = flag.String("kanban-url", settings.String(schedulerconfig.SettingKanbanStoreURL), "kanban-store base URL")
		bridgeURL    = flag.String("bridge-url", settings.String(schedulerconfig.SettingBridgeURL), "llm-bridge-server base URL")
		boardID      = flag.String("board-id", "", "Kanban board id to curate (required). Pinned by id so a rename in the UI cannot fork a second board.")
		staleAfter   = flag.Duration("stale-after", 2*time.Hour, "Reap an In Progress card whose session is idle/gone and has had no activity for longer than this.")
	)
	flag.Parse()

	// llm-bridge-server gates every route; stamp this process's calls to it
	// with the service token. Host-scoped, so noteboard, kanban-store and the
	// scheduler API are untouched.
	if err := bridgeauth.StampRequestsToBridge(*bridgeURL, settings.String(schedulerconfig.SettingBridgeServiceToken)); err != nil {
		log.Fatal(err)
	}

	if *boardID == "" {
		log.Fatal("-board-id is required; there is no board to default to")
	}

	cfg := config{
		kanbanURL:          *kanbanURL,
		kanbanServiceToken: settings.String(schedulerconfig.SettingKanbanStoreServiceToken),
		bridgeURL:          *bridgeURL,
		boardID:            *boardID,
		staleAfter:         *staleAfter,
		dryRun:             *dryRun,
	}

	if *registerFlag {
		board, err := getPinnedBoard(issuesclient.New(cfg.kanbanURL, cfg.kanbanServiceToken), cfg.boardID)
		if err != nil {
			log.Fatal(err)
		}
		if err := registerSchedulerJob(settings.String(schedulerconfig.SettingSchedulerURL), board, *jobName); err != nil {
			log.Fatalf("register: %v", err)
		}
		return
	}

	os.Exit(run(cfg))
}

// getPinnedBoard reads the board the curator is pinned to. A missing board is an
// error rather than something to create: kanban-store resolves boards by name
// with no unique constraint, so a replacement is exactly the fork pinning the id
// prevents.
func getPinnedBoard(kc *issuesclient.Client, boardID string) (*issuesclient.Board, error) {
	board, err := kc.GetBoard(boardID)
	if errors.Is(err, issuesclient.ErrBoardNotFound) {
		return nil, fmt.Errorf("board %s does not exist — refusing to create a replacement, since a second board is exactly the fork pinning the id prevents", boardID)
	}
	if err != nil {
		return nil, fmt.Errorf("get board %s: %w", boardID, err)
	}
	return board, nil
}

// run is one tick, and its return value is the process exit status.
//
// ==================== a tick that closed nothing it meant to ====================
//
// Everything below that can stop the whole tick is still fatal: with no board
// view there is no tick to have, and exiting 1 already says so. What this adds is
// the six ways the curator used to fall short of its job PER CARD and still exit
// 0, because the loop's answer to every one of them is `continue`.
//
// Five are errors it hit while trying: a card whose links it could not read, a
// session the bridge would not answer for, a review sign-off it could not look
// up, a closing note the board refused, and — the sharp one — a MOVE the board
// refused. By that last line the curator has decided the card is finished; the
// move is the only thing that takes it out of In Progress, and when it fails the
// card wedges there while the run is recorded `success`. The sixth is a
// placement whose noteboard item did not resolve at all, so the card could not
// even be read.
//
// The other direction matters as much and is tested as hard. A card with no
// session link, a card awaiting the reviewer's sign-off, a still-running session
// and an empty board are the curator having nothing to do, not falling short —
// scheduler jobs 17 and 18 curate boards that are empty on 100 of 100 ticks, and
// a job that goes red every tick carries as little as the green it replaces.
func run(cfg config) int {
	degraded := degradations.New("curator")

	kc := issuesclient.New(cfg.kanbanURL, cfg.kanbanServiceToken)
	board, err := getPinnedBoard(kc, cfg.boardID)
	if err != nil {
		log.Fatal(err)
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

	var inProgress []issuesclient.CardView
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
			// A placement with no item behind it. kanban-store stores placement and
			// treats noteboard as the source of truth for content, so this is a card
			// whose content is gone: it cannot be read, cannot be judged, and will sit
			// In Progress forever. Measured 2026-08-29 across all 7 boards and 7,970
			// cards: none. So this is not the curator having nothing to do, it is the
			// curator being handed a card it cannot see.
			log.Printf("[curator] card %s: placement has no noteboard item — cannot be read or closed", c.Placement.CardID)
			degraded.Record("card %s: its noteboard item did not resolve, so the card was not examined", c.Placement.CardID)
			skipped++
			continue
		}
		title, _ := c.Item["title"].(string)
		links, err := kc.ListCardLinks(c.Placement.CardID)
		if err != nil {
			log.Printf("[%s] list links: %v", title, err)
			degraded.Record("card %s (%s): its links could not be read (%v), so it was not examined", c.Placement.CardID, title, err)
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
			// Nothing to close against. This is NOT a shortfall: the curator is
			// session-driven and a card with no session is not its business. It is
			// logged because it used to be silent, and silence is what let card
			// f1107651-67dd-4be5-b03a-a9c964f5f4ce sit In Progress from 2026-07-13 to
			// 2026-08-29 — skipped on every one of the ~13,000 ticks in between with
			// nothing in the run history naming it. Recording it instead would paint
			// job 13 red every five minutes for a card the curator can never close.
			log.Printf("[%s] no session link — nothing to close against (card_id=%s)", title, c.Placement.CardID)
			skipped++
			continue
		}
		sess, notFound, err := getSession(cfg.bridgeURL, sessionID)
		state, ended := sess.State, sess.UpdatedAt
		if err != nil && !notFound {
			log.Printf("[%s] bridge GET /sessions/%s: %v — skip", title, sessionID, err)
			degraded.Record("card %s (%s): session %s could not be read from the bridge (%v), so it was not examined", c.Placement.CardID, title, sessionID, err)
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
			if createdByAutoworker(sess) && !staleCompleted(ended) {
				settled, err := reviewSettled(kc, sessionID)
				if err != nil {
					// The sign-off question went unanswered, so the card is deferred on a
					// kanban-store error rather than on the review state. Deferring is the
					// right local choice — the grace window still closes it — but a tick
					// that could not ask is not a tick that asked and got "no".
					degraded.Record("card %s (%s): the review sign-off for session %s could not be looked up (%v), so closing was deferred without asking", c.Placement.CardID, title, sessionID, err)
				}
				if !settled {
					log.Printf("[%s] completed; awaiting autoworker-reviewer sign-off — leaving In Progress (session_id=%s)", title, sessionID)
					skipped++
					continue
				}
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
			//
			// This exit used to be the one `continue` in the loop that incremented
			// no counter at all, so a card left through it without appearing in any
			// number the tick line prints.
			if !createdByAutoworker(sess) {
				skipped++
				continue
			}
			if !staleSince(ended, cfg.staleAfter) {
				continue
			}
			target, targetName = failedCol, columnFailed
			reapNote = fmt.Sprintf("reaped: %s with no activity since %s (> %s)", state, ended, cfg.staleAfter)
		default:
			continue // running / unknown — leave alone
		}

		if reapNote != "" {
			log.Printf("[%s] %s → %s (%s) (session_id=%s)", title, stateLabel(state, notFound), targetName, reapNote, sessionID)
		} else {
			log.Printf("[%s] %s → %s (session_id=%s)", title, state, targetName, sessionID)
		}
		if cfg.dryRun {
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
					degraded.Record("card %s (%s): closed to %s with no note saying why — the body patch was refused (%v)", c.Placement.CardID, title, targetName, err)
				}
			}
		}
		moveResult, err := kc.MoveCard(c.Placement.CardID, board.ID, target, 0)
		if err != nil {
			log.Printf("[%s] move card: %v", title, err)
			degraded.Record("card %s (%s): judged %s and the move was refused (%v) — it is still In Progress", c.Placement.CardID, title, targetName, err)
			errors++
			continue
		}
		if moveResult.AutoStatusError != "" {
			log.Printf("[%s] moved card but its noteboard status was not set to match: %s", title, moveResult.AutoStatusError)
		}
		moved++
	}
	log.Printf("[curator] done: moved=%d skipped=%d errors=%d", moved, skipped, errors)
	return degraded.ExitCode()
}

// ============================ helpers ============================

// bridgeSession is the part of an llm-bridge-server session this binary reads.
type bridgeSession struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at"`
	Purpose   string `json:"purpose"`
}

// createdByAutoworker reports whether autoworker created this session. It reads
// Purpose and deliberately ignores SessionID — see purposeAutoworker for the
// measurement, and TestCreatedByAutoworkerIgnoresTheSessionID for the pin.
func createdByAutoworker(s bridgeSession) bool {
	return s.Purpose == purposeAutoworker
}

// getSession fetches one session from llm-bridge-server and returns
// (session, notFound, err). UpdatedAt is the most recent state transition we
// can observe — used as an "ended at" hint when stamping the closed-card body.
// Purpose is what created the session and drives both autoworker gates (see
// purposeAutoworker). notFound is true (with a nil-equivalent error) when the
// bridge has no such session, which the caller treats as a reapable terminal
// condition rather than a transient error.
//
// Named getSessionState until it started returning purpose, at which point the
// old name described only half of what came back.
func getSession(bridgeURL, sessionID string) (sess bridgeSession, notFound bool, err error) {
	url, err := bridgesession.URL(bridgeURL, sessionID)
	if err != nil {
		return bridgeSession{}, false, err
	}
	resp, err := http.Get(url)
	if err != nil {
		return bridgeSession{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return bridgeSession{}, true, fmt.Errorf("not found")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return bridgeSession{}, false, fmt.Errorf("HTTP %s — %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var s bridgeSession
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return bridgeSession{}, false, err
	}
	// A session the bridge found but reports with no purpose cannot be gated on.
	// Refuse rather than hand the caller an empty string: it would compare
	// unequal to purposeAutoworker and silently read as "somebody else's
	// session", which is the exact failure the purpose gate replaced.
	if s.Purpose == "" {
		return bridgeSession{}, false, fmt.Errorf("session %s: llm-bridge-server returned no purpose field", sessionID)
	}
	return s, false, nil
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
// session (review-settled entity tag present), and separately whether it could
// find out. On a tag-lookup error it returns (false, err): deferring is still
// the right local choice — the grace window, not a transient kanban-store
// hiccup, then decides — but the caller needs the error to say that this tick
// deferred without ever asking. Folding the two into one bool is what made that
// invisible.
func reviewSettled(kc *issuesclient.Client, sessionID string) (settled bool, err error) {
	tags, err := kc.ListEntityTags(entityTypeSession, sessionID)
	if err != nil {
		log.Printf("[curator] list entity tags for %s: %v — treating as unsettled", sessionID, err)
		return false, err
	}
	for _, t := range tags {
		if t.Tag == tagReviewSettled {
			return true, nil
		}
	}
	return false, nil
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
func registerSchedulerJob(schedURL string, board *issuesclient.Board, jobName string) error {
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, "bin", "kanban-curator")

	if jobName == "" {
		jobName = defaultJobName(board.Name)
	}
	return schedulerjobs.RegisterIfAbsent(schedURL, schedulerjobs.Job{
		Name:        jobName,
		Description: fmt.Sprintf("Closes autoworker kanban cards on board %q when their bridge sessions reach a terminal state.", board.Name),
		Schedule:    "*/5 * * * *",
		Command:     bin + " -board-id " + board.ID,
	})
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
