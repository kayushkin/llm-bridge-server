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
// board whose session llm-bridge-server reports with purpose="autoworker" and
// state "completed":
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
//
// One ordering is load-bearing rather than incidental: the review-pass-N tag is
// written BEFORE the nudge is sent, and the nudge is sent only if that write
// succeeded. See reviewCompletedSession.
package main

import (
	"bytes"
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

	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/slava/scheduler/internal/bridgeauth"
	schedulerconfig "github.com/slava/scheduler/internal/config"
	"github.com/slava/scheduler/internal/kanbanclient"
)

const (
	defaultBoardName  = "Agent runs"
	columnInProgress  = "In Progress"
	linkTypeSession   = "session"
	entityTypeSession = "session"

	// purposeAutoworker is the value llm-bridge-server stores in a session's
	// "purpose" field for a session autoworker created. It is the origin gate:
	// we never nudge dispatcher or attended sessions.
	//
	// This gate used to prefix-match the session_id against "autoworker-".
	// llm-bridge-server owns the answer to "what created this session" and
	// publishes it as a field, so matching a naming convention embedded in the
	// id was a name-based join onto a record that has an authoritative one.
	// Measured 2026-08-08 across all 8,669 live sessions: 4,988 carry
	// purpose="autoworker" and 613 of them (12.3%) have a session_id that does
	// NOT start with "autoworker-" — they are "br_"-prefixed, created between
	// 2026-04-23 and 2026-06-03, and the prefix gate silently classed every one
	// of them as somebody else's session.
	purposeAutoworker = "autoworker"

	// maxPasses caps self-review continuations per session before we force
	// settle. Two nudges is the agreed budget/flap tradeoff.
	maxPasses = 2

	tagSettled    = "review-settled"
	tagPassPrefix = "review-pass-" // review-pass-1, review-pass-2, …
)

func main() {
	settings, err := schedulerconfig.AutoworkerReviewer.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatal(err)
	}
	var (
		registerFlag = flag.Bool("register", false, "Register/upsert this binary as a scheduler cron job and exit.")
		jobName      = flag.String("job-name", "autoworker-reviewer-tick", "Scheduler job name for -register.")
		dryRun       = flag.Bool("dry-run", false, "Report decisions; do not nudge sessions or tag.")
		kanbanURL    = flag.String("kanban-url", settings.String(schedulerconfig.SettingKanbanStoreURL), "kanban-store base URL")
		bridgeURL    = flag.String("bridge-url", settings.String(schedulerconfig.SettingBridgeURL), "llm-bridge-server base URL")
		boardName    = flag.String("board", defaultBoardName, "Kanban board name to review")
	)
	flag.Parse()

	// llm-bridge-server gates every route; stamp this process's calls to it
	// with the service token. Host-scoped, so the scheduler API, noteboard and
	// kanban-store calls below are untouched.
	if err := bridgeauth.StampRequestsToBridge(*bridgeURL, settings.String(schedulerconfig.SettingBridgeServiceToken)); err != nil {
		log.Fatal(err)
	}

	if *registerFlag {
		if err := registerSchedulerJob(settings.String(schedulerconfig.SettingSchedulerURL), *jobName, *boardName); err != nil {
			log.Fatalf("register: %v", err)
		}
		return
	}

	kc := kanbanclient.New(*kanbanURL, settings.String(schedulerconfig.SettingKanbanStoreServiceToken))
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

	// Every skip is counted under the reason it happened. A single "skipped"
	// total is what let this binary report success for eleven days while doing
	// nothing: "skipped=139" is identical whether the board holds 139 attended
	// cards or the origin gate is broken and rejecting every autoworker card on
	// it. The counters below are the difference between those two readings.
	var counts skipCounts
	var nudged, settled, errors int
	for _, c := range inProgress {
		if c.Item == nil {
			counts.noItem++
			continue
		}
		title, _ := c.Item["title"].(string)

		sessionID, err := sessionLink(kc, c.Placement.CardID)
		if err != nil {
			log.Printf("[%s] list links: %v", title, err)
			errors++
			continue
		}
		if sessionID == "" {
			counts.noSessionLink++
			continue
		}

		sess, err := getSession(*bridgeURL, sessionID)
		if err != nil {
			log.Printf("[%s] bridge GET /sessions/%s: %v — skip", title, sessionID, err)
			errors++
			continue
		}
		// Origin gate: only autoworker sessions; skip dispatcher/attended cards.
		// Asks llm-bridge-server what made the session rather than reading a
		// naming convention out of its id — see purposeAutoworker.
		if !createdByAutoworker(sess) {
			counts.notAutoworker++
			continue
		}
		// Only completed sessions get reviewed. error/aborted/disconnected are the
		// curator's to fail; idle/running mean the agent is mid-flight.
		if sess.State != "completed" {
			counts.notCompleted++
			continue
		}

		action := reviewCompletedSession(
			kc, sessionID, *dryRun,
			func(message string) error { return sendMessage(*bridgeURL, sessionID, message) },
			func(note string) { stampBody(kc, c, note) },
			func(format string, args ...any) {
				log.Printf("[%s] "+format, append([]any{title}, args...)...)
			},
		)
		switch action {
		case actionAlreadySettled:
			counts.alreadySettled++
		case actionNudged:
			nudged++
		case actionSettled:
			settled++
		case actionError:
			errors++
		case actionDryRun:
			// Reported by the logger; counted nowhere, as before.
		}
	}
	log.Printf("[reviewer] done: nudged=%d settled=%d skipped=%d (%s) errors=%d",
		nudged, settled, counts.total(), counts.breakdown(), errors)
}

// ============================ skip accounting ============================

// skipCounts records why each In Progress card was passed over. Five different
// exits used to share one "skipped" counter, so the tick line could not tell a
// board of attended cards apart from an origin gate rejecting everything.
type skipCounts struct {
	noItem         int // card carries no noteboard item
	noSessionLink  int // card has no session link to review
	notAutoworker  int // session exists but something else created it
	notCompleted   int // autoworker session still mid-flight, or already failed
	alreadySettled int // reviewed to settlement; waiting on the curator to close
}

func (s skipCounts) total() int {
	return s.noItem + s.noSessionLink + s.notAutoworker + s.notCompleted + s.alreadySettled
}

// breakdown renders every non-zero reason. Reasons that did not fire are left
// out so the common case stays short, and "skipped=N ()" with an empty
// breakdown is impossible by construction: total() sums these same fields.
func (s skipCounts) breakdown() string {
	parts := make([]string, 0, 5)
	for _, r := range []struct {
		name string
		n    int
	}{
		{"no-item", s.noItem},
		{"no-session-link", s.noSessionLink},
		{"not-autoworker", s.notAutoworker},
		{"not-completed", s.notCompleted},
		{"already-settled", s.alreadySettled},
	} {
		if r.n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", r.name, r.n))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

// ============================ the review decision ============================

// reviewAction names what one tick did with one completed autoworker session.
type reviewAction int

const (
	actionError reviewAction = iota
	actionAlreadySettled
	actionNudged
	actionSettled
	actionDryRun
)

// reviewStore is the slice of kanban-store the review decision needs. Narrow so
// a test can supply a store whose writes fail while its reads succeed — which is
// the exact failure mode this function is shaped around.
type reviewStore interface {
	ListEntityTags(entityType, entityRef string) ([]kanbanclient.EntityTag, error)
	AddEntityTag(entityType, entityRef, tag string) error
}

// reviewCompletedSession runs one review tick against one completed autoworker
// session: settle it if the pass budget is spent, otherwise send the next
// self-review nudge.
//
// # The ordering is the loop guard
//
// The review-pass-N tag is written BEFORE the nudge is sent, and the nudge is
// sent only if that write succeeded. This is not bookkeeping. maxPasses is
// enforced by counting review-pass-N tags on the session, so a nudge sent
// without its tag durably stored is a nudge the next tick cannot see: passes
// never advances, nudge 1 goes out again, and again every five minutes. Each
// send respawns the agent with `claude --resume`, so an uncounted nudge is an
// unbounded agent-spawn loop — precisely the budget burn maxPasses exists to
// cap. Writing the tag first means the money is never spent ahead of the record
// of spending it.
//
// # Why settling on a failed tag write is not enough on its own
//
// The obvious remedy — send, and if the tag write fails, settle instead — does
// not close this. settle() writes review-settled through the same
// POST /api/entities/{type}/{ref}/tags call that just failed, against the same
// store. In the one failure mode that produces the defect (kanban-store
// answering reads and refusing writes) the remedy fails too, and it fails with
// the nudge already sent. The loop is left uncapped exactly as before. Only
// refusing to SEND until the counter is durable closes it.
//
// That ordering also makes the cap hold under a failing settle, which the old
// order did not: a send failure now advances the counter on its way past, so
// maxPasses bounds the number of sends whichever store write is broken.
//
// # It must still never wedge a card
//
// A card whose tag write fails is settled so the curator can close it, matching
// the precedent for a failed send. If that settle write fails too, nothing is
// sent and the tick reports an error — the card waits for the curator's idle
// grace window rather than being nudged again.
func reviewCompletedSession(
	store reviewStore,
	sessionID string,
	dryRun bool,
	send func(message string) error,
	stamp func(note string),
	logf func(format string, args ...any),
) reviewAction {
	tags, err := store.ListEntityTags(entityTypeSession, sessionID)
	if err != nil {
		logf("list entity tags: %v — skip", err)
		return actionError
	}
	if hasTag(tags, tagSettled) {
		return actionAlreadySettled // awaiting curator close
	}
	passes := passCount(tags)

	if passes >= maxPasses {
		logf("%d/%d passes done → settle (session_id=%s)", passes, maxPasses, sessionID)
		if dryRun {
			return actionDryRun
		}
		if err := settleSession(store, stamp, sessionID,
			fmt.Sprintf("review loop guard hit (%d passes)", passes)); err != nil {
			logf("settle: %v", err)
			return actionError
		}
		return actionSettled
	}

	pass := passes + 1
	logf("completed, sending self-review nudge %d/%d (session_id=%s)", pass, maxPasses, sessionID)
	if dryRun {
		return actionDryRun
	}

	// Count the pass before spending it. A nudge whose tag did not land is
	// invisible to the next tick's budget check, so we must not send one.
	if err := store.AddEntityTag(entityTypeSession, sessionID,
		fmt.Sprintf("%s%d", tagPassPrefix, pass)); err != nil {
		logf("tag review-pass-%d: %v — not sending nudge %d, settling so curator can close",
			pass, err, pass)
		if serr := settleSession(store, stamp, sessionID,
			fmt.Sprintf("review pass tag unwritable: %v", err)); serr != nil {
			logf("settle after tag failure: %v", serr)
		}
		return actionError
	}

	if err := send(reviewNudge(pass, maxPasses)); err != nil {
		// Best-effort: never wedge the card on a send failure. Settle so the
		// curator closes it as it would have without review. The pass tag is
		// already stored, so the cap holds even if this settle fails.
		logf("nudge send failed: %v — settling so curator can close", err)
		if serr := settleSession(store, stamp, sessionID,
			fmt.Sprintf("review nudge send failed: %v", err)); serr != nil {
			logf("settle after send failure: %v", serr)
		}
		return actionError
	}
	stamp(fmt.Sprintf("self-review nudge %d/%d sent", pass, maxPasses))
	return actionNudged
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

// settleSession tags the session review-settled (so the curator may close the
// card) and stamps a short audit note on the card body.
//
// The tag write is the part that matters and the part that can fail; the stamp
// is cosmetic and is only reached once the tag has landed. Callers must treat a
// returned error as "this session is NOT settled" — it is the same
// POST .../tags call every other write here uses, so it fails whenever they do.
func settleSession(store reviewStore, stamp func(note string), sessionID, reason string) error {
	if err := store.AddEntityTag(entityTypeSession, sessionID, tagSettled); err != nil {
		return err
	}
	stamp("review settled — " + reason)
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

// bridgeSession is the part of an llm-bridge-server session this binary reads.
// Purpose is what created the session; it is the origin gate (purposeAutoworker).
type bridgeSession struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at"`
	Purpose   string `json:"purpose"`
}

// createdByAutoworker reports whether autoworker created this session.
//
// It reads Purpose and deliberately ignores SessionID. The id is carried on the
// struct anyway because this gate used to match it against an "autoworker-"
// prefix, and 613 live sessions carry purpose="autoworker" without that prefix;
// TestCreatedByAutoworkerIgnoresTheSessionID pins the distinction, so putting a
// prefix check back in here turns that test red rather than silently dropping
// an eighth of the population again.
func createdByAutoworker(s bridgeSession) bool {
	return s.Purpose == purposeAutoworker
}

// getSession fetches one session from llm-bridge-server. It replaced
// getSessionState when the origin gate stopped reading the session_id and
// started reading the field the bridge owns, so the name no longer described
// what came back.
func getSession(bridgeURL, sessionID string) (bridgeSession, error) {
	url := fmt.Sprintf("%s/sessions/%s", strings.TrimRight(bridgeURL, "/"), sessionID)
	resp, err := http.Get(url)
	if err != nil {
		return bridgeSession{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return bridgeSession{}, fmt.Errorf("not found")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return bridgeSession{}, fmt.Errorf("HTTP %s — %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var s bridgeSession
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return bridgeSession{}, err
	}
	// A session with no purpose cannot be gated on. Refuse rather than let an
	// empty string fall through to a comparison it can only lose: the caller
	// would silently read "not an autoworker session" from a field the bridge
	// simply did not send, which is the exact failure this gate was changed to
	// remove.
	if s.Purpose == "" {
		return bridgeSession{}, fmt.Errorf("session %s: llm-bridge-server returned no purpose field", sessionID)
	}
	return s, nil
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

// ============================ scheduler self-registration ============================

func registerSchedulerJob(schedURL, jobName, boardName string) error {
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
