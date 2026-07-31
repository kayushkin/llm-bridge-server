// autoworker decides whether to spawn a budgeted todo-worker agent based on
// current subscription-limit utilisation relative to a time-until-reset curve.
//
// Registered as a scheduler shell job running every ~30 minutes. On each tick:
//
//  1. Pull /api/usage/limits from usage-store.
//  2. For each provider (anthropic → claude_code, codex → codex):
//     - Skip if the provider snapshot is stale (codex when no recent session).
//     - Locate the weekly window (seven_day for anthropic, weekly for codex).
//     - Compute elapsed fraction of the 7-day window from resets_at.
//     - Interpolate the target utilisation curve against that fraction.
//     - Skip unless local PT time is inside the provider's safe window.
//     - Skip unless we're outside the cooldown since the last fire.
//     - Skip unless actual used_percent < target − slack (default 15pp).
//  3. When all gates pass, publish a BridgeInbound message on NATS
//     bridge.inbound targeting the configured llm-bridge instance_id, so the
//     fresh session shows up in the dash chat UI alongside the user's manual
//     sessions for that harness instance.
//
// Cooldown state lives in ~/.config/autoworker/state.json (one map of provider
// → last_fire_unix). Everything else is stateless.
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

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/bus/messages"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/slava/scheduler/internal/kanbanclient"
)

const (
	kanbanBoardName    = "Agent runs"
	kanbanColumnTarget = "In Progress"
	kanbanCardOrigin   = "autoworker"

	// Queue-backed dispatch source: human (or curator) drops cards into the
	// Queued column; the worker picks the top eligible card on each fire and
	// uses its title/body as the worker prompt.
	autoworkerQueueBoard            = "Autoworker queue"
	autoworkerQueueColumnFrom       = "Queued"
	autoworkerQueueColumnInProgress = "In Progress"
)

var kanbanDefaultColumns = []string{"Queued", "In Progress", "Blocked", "Done", "Failed"}

// defaultMaxBudget is the fleet-wide per-session spend cap: the safety net for
// sessions no per-card ceiling can cover.
//
// It is OFF by default (0), and 0 is the only safe "off" value here only because
// this code never passes a zero cap through to the harness — llm-bridge-claudecode
// omits --max-budget-usd when the value is zero, which the CLI reads as unlimited.
// So "off" and "unlimited" genuinely are the same thing at this layer, which is
// exactly why a spent-out CARD is held rather than dispatched with a cap of 0.
//
// A per-card ceiling always overrides this; the default only fills the gap.
var defaultMaxBudget = flag.Float64("default-max-budget-usd", envFloat("AUTOWORKER_DEFAULT_MAX_BUDGET_USD", 0),
	"Fleet-wide per-session spend cap in USD for sessions with no per-card ceiling (0 = off/uncapped). A card's own ceiling always wins.")

type limitWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes *int64  `json:"window_minutes,omitempty"`
	ResetsAt      *int64  `json:"resets_at,omitempty"`
}

type providerLimits struct {
	Provider   string                  `json:"provider"`
	PlanType   string                  `json:"plan_type,omitempty"`
	Tier       string                  `json:"tier,omitempty"`
	Windows    map[string]*limitWindow `json:"windows"`
	SnapshotAt int64                   `json:"snapshot_at"`
	Source     string                  `json:"source"`
	StaleAfter *int64                  `json:"stale_after,omitempty"`
}

type limitsResponse map[string]*providerLimits

// providerSpec wires a usage-store provider to the llm-bridge instance that
// should host each fired session, plus the time window to gate against.
//
// All fires publish to messages.BridgeInboundSubject ("bridge.inbound"); the
// llm-bridge-adapter passes BusSessionID through as the bridge session_id —
// post session-consolidation (2026-05-09) the two are the same canonical id.
// The resulting session is visible in the dash chat UI under the chosen
// instance.
type providerSpec struct {
	Name         string        // "anthropic" / "codex" — key in limitsResponse
	Harness      msg.Harness   // llm-bridge harness kind (e.g. "claude_code")
	InstanceID   string        // llm-bridge instance_id (row in harness-store)
	Channel      string        // BridgeInbound.Channel metadata (unused for routing)
	Orchestrator string        // BridgeInbound.Orchestrator metadata
	WindowKey    string        // "seven_day" or "weekly" in ProviderLimits.Windows
	SafeStart    int           // minutes past local midnight, inclusive
	SafeEnd      int           // minutes past local midnight, exclusive
	WeekdayOnly  bool          // if true, safe window only applies Mon–Fri
	Cooldown     time.Duration // minimum time between fires
}

var providers = []providerSpec{
	{
		Name:         "anthropic",
		Harness:      msg.HarnessClaudeCode,
		InstanceID:   "inst-cc-local", // "Clawd" — same instance the user chats in
		Channel:      "autoworker",
		Orchestrator: "claude-code",
		WindowKey:    "seven_day",
		SafeStart:    0,       // 00:00 PT
		SafeEnd:      24 * 60, // 24:00 PT — whole day (catch-up before reset)
		Cooldown:     15 * time.Minute,
	},
	{
		Name:         "codex",
		Harness:      msg.HarnessCodex,
		InstanceID:   "inst-codex-local", // "Jipitee"
		Channel:      "autoworker",
		Orchestrator: "codex",
		WindowKey:    "weekly",
		SafeStart:    0,      // 00:00 PT
		SafeEnd:      6 * 60, // 06:00 PT
		Cooldown:     2 * time.Hour,
	},
}

// targetCurve is the (fraction_elapsed, target_used_percent) anchor points.
// Linear interpolation between anchors. Applied against the weekly window's
// elapsed fraction so we ramp budget consumption toward ~100% by reset.
//
// Spec (from user): "first couple days leave it blank, then the last % of
// the week we should aim to increase … near 100% as it ends".
var targetCurve = []struct{ frac, pct float64 }{
	{0.00, 0},   // day 0 of 7 → blank
	{0.286, 0},  // ~day 2    → still blank
	{0.429, 25}, // ~day 3    → start budgeting
	{0.571, 50}, // ~day 4    → half
	{0.714, 70}, // ~day 5
	{0.857, 85}, // ~day 6
	{1.00, 100}, // reset     → full
}

type decision struct {
	provider string
	target   float64
	actual   float64
	fraction float64
	fire     bool
	reason   string
}

type state struct {
	LastFire map[string]int64 `json:"last_fire"` // provider → unix seconds
}

func main() {
	var (
		registerFlag = flag.Bool("register", false, "Register (upsert) this binary as a scheduler cron job and exit.")
		dryRun       = flag.Bool("dry-run", false, "Report decisions; do not publish or update state.")
		slackPP      = flag.Float64("slack", 15, "Percent points below target before firing.")
		prompt       = flag.String("prompt", defaultPrompt, "Prompt text sent to the worker agent.")
		tz           = flag.String("tz", "America/Los_Angeles", "IANA timezone for safe-window gating.")
	)
	flag.Parse()

	if *defaultMaxBudget < 0 {
		log.Fatalf("-default-max-budget-usd must not be negative (got %v)", *defaultMaxBudget)
	}

	if *registerFlag {
		if err := registerSchedulerJob(); err != nil {
			log.Fatalf("register: %v", err)
		}
		return
	}

	loc, err := time.LoadLocation(*tz)
	if err != nil {
		log.Fatalf("timezone %q: %v", *tz, err)
	}

	lim, err := fetchLimits()
	if err != nil {
		log.Fatalf("fetch limits: %v", err)
	}

	st, statePath, err := loadState()
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	now := time.Now()
	nowLocal := now.In(loc)

	var natsClient *natsbus.Client
	if !*dryRun {
		natsURL := envOr("NATS_URL", "nats://localhost:4222")
		c, err := natsbus.Connect(natsbus.Options{URL: natsURL, Name: "autoworker"})
		if err != nil {
			log.Fatalf("nats connect: %v", err)
		}
		defer c.Close()
		natsClient = c
	}

	// Lazy kanban-store clients; only initialised when actually needed so
	// dry-run passes and skip-everything ticks don't hit kanban-store.
	var (
		kc          *kanbanclient.Client // "Agent runs" telemetry board
		queueClient *kanbanclient.Client // "Autoworker queue" dispatch source
		queueTried  bool                 // avoid re-attempting bootstrap each provider
	)

	for _, p := range providers {
		d := evaluate(p, lim[p.Name], nowLocal, st, *slackPP)
		log.Printf("[%s] %s", d.provider, d.reason)
		if !d.fire || *dryRun {
			continue
		}

		// Pick the prompt: queue head card if available, else fall back to
		// the noteboard-discovery default so an empty queue doesn't waste
		// the fire decision.
		if !queueTried {
			queueClient = initQueueClient(false)
			queueTried = true
		}
		var queueCard *queueCandidate
		if queueClient != nil {
			cand, err := selectQueuedCard(queueClient)
			if err != nil {
				log.Printf("queue: select: %v (using default prompt)", err)
			} else {
				queueCard = cand
			}
		}
		workerPrompt := *prompt
		if queueCard != nil {
			workerPrompt = buildQueuePrompt(queueCard)
		}

		maxBudgetUSD, why := resolveSessionBudget(queueCard, *defaultMaxBudget)
		if maxBudgetUSD > 0 {
			log.Printf("[%s] session capped at $%.2f — %s", d.provider, maxBudgetUSD, why)
		} else {
			log.Printf("[%s] session is UNCAPPED — %s", d.provider, why)
		}
		sessionID, err := publishWorker(natsClient, p, workerPrompt, maxBudgetUSD)
		if err != nil {
			log.Printf("[%s] publish failed: %v", d.provider, err)
			continue
		}
		st.LastFire[p.Name] = now.Unix()
		log.Printf("[%s] FIRED worker → bridge.inbound (harness=%s instance=%s target=%.0f%% actual=%.0f%% queue_card=%s)",
			d.provider, p.Harness, p.InstanceID, d.target, d.actual, queueCardLogID(queueCard))

		if queueCard != nil && queueClient != nil {
			if err := markQueueCardDispatched(queueClient, queueCard, p, sessionID, now); err != nil {
				log.Printf("[%s] queue: dispatch side-effects failed: %v", d.provider, err)
			}
		}

		if kc == nil {
			kc = kanbanclient.New(envOr("KANBAN_STORE_URL", "http://localhost:8305"))
			if err := kc.EnsureBoard(kanbanBoardName, kanbanDefaultColumns); err != nil {
				log.Printf("kanban: bootstrap %q failed: %v (continuing)", kanbanBoardName, err)
				kc = nil // disable for the rest of this tick
			}
		}
		if kc != nil {
			title := fmt.Sprintf("autoworker · %s · %s", p.Name, now.Format("Jan 2 15:04"))
			body := fmt.Sprintf(
				"Dispatched by autoworker.\n\n- provider: %s\n- harness: %s\n- instance: %s\n- session_id: `%s`\n- target/actual: %.0f%% / %.0f%%\n",
				p.Name, p.Harness, p.InstanceID, sessionID, d.target, d.actual,
			)
			if queueCard != nil {
				body += fmt.Sprintf("- queue_card: `%s` — %s\n", queueCard.cardID, queueCard.title)
			}
			cardID, err := kc.CreateCard(kanbanclient.CreateCardArgs{
				ColumnName: kanbanColumnTarget,
				Title:      title,
				Body:       body,
				Tags:       []string{kanbanCardOrigin, p.Name},
				SessionID:  sessionID,
				Provider:   p.Name,
			})
			if err != nil {
				log.Printf("[%s] kanban: create card failed: %v", d.provider, err)
			} else {
				log.Printf("[%s] kanban: card %s in %q on %q", d.provider, cardID, kanbanColumnTarget, kanbanBoardName)
			}
		}
	}

	// Observability: if no provider fired (queue never bootstrapped above),
	// still surface the head of the Queued column so dry-run / quiet ticks
	// show what would dispatch next. Bootstrap is read-only on dry-run so a
	// fresh install doesn't quietly create the board.
	if !queueTried {
		if qc := initQueueClient(*dryRun); qc != nil {
			if cand, err := selectQueuedCard(qc); err != nil {
				log.Printf("queue: %v", err)
			} else if cand != nil {
				log.Printf("queue: head card=%s title=%q tags=%v body_len=%d",
					cand.cardID, cand.title, cand.tags, len(cand.body))
			}
		}
	}

	if !*dryRun {
		if err := saveState(statePath, st); err != nil {
			log.Printf("save state: %v", err)
		}
	}
}

// initQueueClient resolves the Autoworker queue board into a kanbanclient
// configured with BoardID + ColumnsByName ready for selection / move /
// link calls. On real runs the board is created lazily via EnsureBoard.
// On dry-run the bootstrap is read-only via FindBoard so a fresh install
// doesn't quietly create the board.
//
// Returns nil (with logging) when the board is unavailable; callers are
// expected to fall back to default behavior so kanban-store outages don't
// break the fire path.
func initQueueClient(dryRun bool) *kanbanclient.Client {
	qc := kanbanclient.New(envOr("KANBAN_STORE_URL", "http://localhost:8305"))
	if dryRun {
		board, err := qc.FindBoard(autoworkerQueueBoard)
		if err != nil {
			log.Printf("queue: find board %q: %v", autoworkerQueueBoard, err)
			return nil
		}
		if board == nil {
			log.Printf("queue: board %q does not exist yet — first non-dry-run will bootstrap it", autoworkerQueueBoard)
			return nil
		}
		qc.BoardID = board.ID
		cols, err := qc.ListColumns(board.ID)
		if err != nil {
			log.Printf("queue: list columns: %v", err)
			return nil
		}
		qc.ColumnsByName = map[string]string{}
		for _, col := range cols {
			qc.ColumnsByName[col.Name] = col.ID
		}
		return qc
	}
	if err := qc.EnsureBoard(autoworkerQueueBoard, kanbanDefaultColumns); err != nil {
		log.Printf("queue: bootstrap %q: %v (using default prompt)", autoworkerQueueBoard, err)
		return nil
	}
	return qc
}

// queueCandidate is one eligible card resolved from the Autoworker queue's
// Queued column.
type queueCandidate struct {
	cardID string
	title  string
	body   string
	tags   []string
	item   map[string]any
	// maxBudgetUSD is the per-session cost cap handed to the harness, derived
	// from the card's spend ceiling minus what it has already cost. Zero means
	// "no ceiling on this card" — NOT "no budget" — and is why the dispatch path
	// only sets max_budget_usd when this is > 0 (the harness omits the flag at 0,
	// which reads as unlimited).
	maxBudgetUSD float64
}

func queueCardLogID(c *queueCandidate) string {
	if c == nil {
		return "(none)"
	}
	return c.cardID
}

// markQueueCardDispatched performs the post-publish side-effects: attach a
// session card link, move the card to In Progress, and append a short dispatch
// note to the noteboard body so the card carries its own audit trail.
func markQueueCardDispatched(qc *kanbanclient.Client, cand *queueCandidate, p providerSpec, sessionID string, now time.Time) error {
	inProgressID, ok := qc.ColumnsByName[autoworkerQueueColumnInProgress]
	if !ok {
		return fmt.Errorf("column %q missing on queue board", autoworkerQueueColumnInProgress)
	}
	if err := qc.AddCardLink(cand.cardID, "session", sessionID, kanbanCardOrigin); err != nil {
		return fmt.Errorf("add session link: %w", err)
	}
	if err := qc.MoveCard(cand.cardID, qc.BoardID, inProgressID, 0); err != nil {
		return fmt.Errorf("move to In Progress: %w", err)
	}
	note := fmt.Sprintf(
		"\n\n---\n_Dispatched by autoworker at %s — provider=%s harness=%s instance=%s session=`%s`_\n",
		now.UTC().Format(time.RFC3339), p.Name, p.Harness, p.InstanceID, sessionID,
	)
	if err := qc.PatchCardBody(cand.cardID, cand.body+note); err != nil {
		return fmt.Errorf("patch body: %w", err)
	}
	return nil
}

// buildQueuePrompt renders the worker prompt from a queued card. The card id
// is the noteboard item id (kanban cards are noteboard items), so closing the
// noteboard item also closes the card via the column's auto_status mapping.
func buildQueuePrompt(c *queueCandidate) string {
	body := strings.TrimSpace(c.body)
	if body == "" {
		body = "(no body — see title)"
	}
	return fmt.Sprintf(`You are picking up a queued task from the autoworker queue. The user is asleep; their subscription budget is under-utilised this week and I am dispatching you to make progress on this specific task without disturbing them.

## Task
**Card ID:** %s
**Title:** %s

%s

## Instructions
1. Do the work described above.
2. When done, PATCH http://localhost:8191/api/items/%s with {"status":"done"} and a body update describing what you did. Kanban cards mirror noteboard items, so closing the noteboard item also closes the card.
3. If the task is ambiguous in intent or needs user input, leave a body update explaining why and exit; the card stays In Progress and a human will intervene.
4. If the task is clear in intent but too large to finish in one session, split it into smaller queue cards rather than skipping it — preserve the parent's tags so children stay queued.

Be self-contained. No chat; just work and close out the task.`,
		c.cardID, c.title, body, c.cardID)
}

// selectQueuedCard returns the top eligible card in the Queued column, in
// placement order. Eligibility: non-nil noteboard item with a non-empty title.
// Skip reasons are logged so dry-run output explains the choice. Returns
// (nil, nil) when the column is empty or no card is eligible.
func selectQueuedCard(qc *kanbanclient.Client) (*queueCandidate, error) {
	view, err := qc.GetBoardView(qc.BoardID)
	if err != nil {
		return nil, err
	}
	var queued []kanbanclient.CardView
	for _, cv := range view.Columns {
		if cv.Column.Name == autoworkerQueueColumnFrom {
			queued = cv.Cards
			break
		}
	}
	if len(queued) == 0 {
		log.Printf("queue: %q column empty", autoworkerQueueColumnFrom)
		return nil, nil
	}
	for _, c := range queued {
		if c.Item == nil {
			log.Printf("queue: skip card %s — no noteboard item", c.Placement.CardID)
			continue
		}
		// The hold gate. The board view deliberately hydrates held cards — a card
		// you cannot see is a card you cannot press play on — so the filter that
		// noteboard applies to its own discovery path has to be applied again here,
		// against the item the board handed us.
		if heldAt, _ := c.Item["held_at"].(string); heldAt != "" {
			reason, _ := c.Item["hold_reason"].(string)
			if reason == "" {
				reason = "no reason given"
			}
			log.Printf("queue: skip card %s — HELD since %s (%s)", c.Placement.CardID, heldAt, reason)
			continue
		}
		title, _ := c.Item["title"].(string)
		if strings.TrimSpace(title) == "" {
			log.Printf("queue: skip card %s — empty title", c.Placement.CardID)
			continue
		}
		body, _ := c.Item["body"].(string)
		var tags []string
		if rawTags, ok := c.Item["tags"].([]any); ok {
			for _, t := range rawTags {
				if s, ok := t.(string); ok {
					tags = append(tags, s)
				}
			}
		}
		cand := &queueCandidate{
			cardID: c.Placement.CardID,
			title:  title,
			body:   body,
			tags:   tags,
			item:   c.Item,
		}

		// Spend ceiling. A card that has already spent its ceiling is held here
		// and skipped, rather than dispatched: firing a session for it would
		// either burn money past the ceiling or (worse) be handed a cap of 0,
		// which the harness reads as unlimited.
		budget, dispatchable := budgetForCard(qc,
			envOr("LLM_BRIDGE_URL", "http://localhost:8160"),
			envOr("NOTEBOARD_URL", "http://localhost:8191"),
			cand)
		if !dispatchable {
			if err := qc.HoldCard(cand.cardID, "spend ceiling reached"); err != nil {
				log.Printf("queue: card %s is over its spend ceiling but the hold FAILED (%v) — skipping it this tick, but it is not parked",
					cand.cardID, err)
			}
			continue
		}
		cand.maxBudgetUSD = budget
		return cand, nil
	}
	log.Printf("queue: no eligible cards in %q (n=%d)", autoworkerQueueColumnFrom, len(queued))
	return nil, nil
}

func evaluate(p providerSpec, lim *providerLimits, nowLocal time.Time, st *state, slackPP float64) decision {
	d := decision{provider: p.Name}

	if lim == nil {
		d.reason = "no snapshot available — skip"
		return d
	}
	if lim.StaleAfter != nil && time.Now().Unix() > *lim.StaleAfter {
		d.reason = fmt.Sprintf("snapshot stale (snapshot_at=%s) — skip",
			time.Unix(lim.SnapshotAt, 0).Format(time.RFC3339))
		return d
	}

	win, ok := lim.Windows[p.WindowKey]
	if !ok || win == nil {
		d.reason = fmt.Sprintf("weekly window %q not in snapshot — skip", p.WindowKey)
		return d
	}
	if win.ResetsAt == nil {
		d.reason = "weekly window has no resets_at — skip"
		return d
	}

	d.actual = win.UsedPercent
	d.fraction = elapsedFraction(*win.ResetsAt, time.Now().Unix())
	d.target = interpolate(d.fraction)

	if !inSafeWindow(p, nowLocal) {
		d.reason = fmt.Sprintf("target=%.0f%% actual=%.0f%% — outside safe window (now=%s)",
			d.target, d.actual, nowLocal.Format("Mon 15:04 MST"))
		return d
	}

	if last := st.LastFire[p.Name]; last > 0 {
		if time.Since(time.Unix(last, 0)) < p.Cooldown {
			d.reason = fmt.Sprintf("target=%.0f%% actual=%.0f%% — cooldown active (last fire %s ago)",
				d.target, d.actual, time.Since(time.Unix(last, 0)).Truncate(time.Minute))
			return d
		}
	}

	if d.actual >= d.target-slackPP {
		d.reason = fmt.Sprintf("target=%.0f%% actual=%.0f%% — within slack (%0.fpp) of target, skip",
			d.target, d.actual, slackPP)
		return d
	}

	d.fire = true
	d.reason = fmt.Sprintf("target=%.0f%% actual=%.0f%% (frac=%.2f) — fire",
		d.target, d.actual, d.fraction)
	return d
}

// elapsedFraction returns how far we are through the 7-day window, based on
// resetsAt (unix seconds) and nowSec. Clamped to [0, 1).
func elapsedFraction(resetsAt, nowSec int64) float64 {
	const weekSec = int64(7 * 24 * 60 * 60)
	windowStart := resetsAt - weekSec
	elapsed := nowSec - windowStart
	if elapsed < 0 {
		return 0
	}
	if elapsed >= weekSec {
		return 0.999
	}
	return float64(elapsed) / float64(weekSec)
}

// interpolate finds the target utilisation for a given elapsed fraction.
func interpolate(frac float64) float64 {
	if frac <= targetCurve[0].frac {
		return targetCurve[0].pct
	}
	for i := 1; i < len(targetCurve); i++ {
		if frac <= targetCurve[i].frac {
			a, b := targetCurve[i-1], targetCurve[i]
			span := b.frac - a.frac
			if span == 0 {
				return b.pct
			}
			t := (frac - a.frac) / span
			return a.pct + t*(b.pct-a.pct)
		}
	}
	return targetCurve[len(targetCurve)-1].pct
}

func inSafeWindow(p providerSpec, nowLocal time.Time) bool {
	if p.WeekdayOnly {
		wd := nowLocal.Weekday()
		if wd == time.Saturday || wd == time.Sunday {
			return false
		}
	}
	minutesOfDay := nowLocal.Hour()*60 + nowLocal.Minute()
	return minutesOfDay >= p.SafeStart && minutesOfDay < p.SafeEnd
}

func fetchLimits() (limitsResponse, error) {
	url := envOr("USAGE_STORE_URL", "http://localhost:8185") + "/api/usage/limits"
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("usage-store returned %d: %s", resp.StatusCode, string(body))
	}
	var lim limitsResponse
	if err := json.Unmarshal(body, &lim); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return lim, nil
}

// applySessionSpendCeiling writes one resolved ceiling to both of the enforcers
// a dispatched session has.
//
// They are not redundant, and neither one covers the other:
//
//   - CreateSessionRequest.MaxBudget is the server-side ceiling. llm-bridge-server
//     stores it on the session row and halts the session when its derived spend
//     reaches it, whichever harness the session runs on. It is what survives a
//     resume, a fork, a mode switch and a bridge-server restart — and it is the
//     ONLY ceiling the codex provider gets at all, because llm-bridge-codex has
//     no max_budget handling of any kind.
//
//   - HarnessConfig.max_budget_usd is the harness-native ceiling. bridge-server
//     treats HarnessConfig as an opaque blob and merges it verbatim into the
//     harness start params, so llm-bridge-claudecode turns it into
//     `claude --max-budget-usd N` and Claude Code stops itself mid-call, in real
//     time, rather than at the next event bridge-server manages to account for.
//
// A zero ceiling reaches neither: 0 means "no ceiling" on both sides, so writing
// it through would turn "cap this at $0" into "no cap at all".
func applySessionSpendCeiling(create *msg.CreateSessionRequest, maxBudgetUSD float64) error {
	if maxBudgetUSD <= 0 {
		return nil
	}
	harnessConfig, err := json.Marshal(map[string]any{"max_budget_usd": maxBudgetUSD})
	if err != nil {
		return fmt.Errorf("marshal harness config: %w", err)
	}
	create.MaxBudget = &maxBudgetUSD
	create.HarnessConfig = harnessConfig
	return nil
}

// publishWorker dispatches a worker session.
//
// maxBudgetUSD, when > 0, becomes the session's per-session cost cap on both the
// server-side and the harness-native enforcer — see applySessionSpendCeiling.
func publishWorker(nc *natsbus.Client, p providerSpec, prompt string, maxBudgetUSD float64) (string, error) {
	now := time.Now().UTC()
	// Mint the canonical session_id here. Adapter passes BusSessionID through
	// as bridge-server session_id (see llm-bridge-adapter@73c7cb5), so this
	// value is also the bridge session_id referenced by kanban session links.
	sessionID := fmt.Sprintf("autoworker-%s-%d", p.Name, now.UnixNano())

	create := &msg.CreateSessionRequest{
		Harness:     p.Harness,
		InstanceID:  p.InstanceID,
		DisplayName: fmt.Sprintf("autoworker %s %s", p.Name, now.Format("Jan 2 15:04")),
		Purpose:     "autoworker",
		Origin:      "scheduler",
		Type:        msg.SessionTypeAutonomous,
		AutoStart:   true,
	}
	if err := applySessionSpendCeiling(create, maxBudgetUSD); err != nil {
		return "", err
	}

	in := messages.BridgeInbound{
		BusSessionID: sessionID,
		Agent:        "todo-worker",
		Orchestrator: p.Orchestrator,
		Channel:      p.Channel,
		Timestamp:    now,
		Create:       create,
		Send: &msg.SendMessageRequest{
			Message: prompt,
		},
	}
	if err := nc.Publish(messages.BridgeInboundSubject, in); err != nil {
		return "", err
	}
	return sessionID, nil
}

func loadState() (*state, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(home, ".config", "autoworker", "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, path, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &state{LastFire: map[string]int64{}}, path, nil
	}
	if err != nil {
		return nil, path, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, path, err
	}
	if st.LastFire == nil {
		st.LastFire = map[string]int64{}
	}
	return &st, path, nil
}

func saveState(path string, st *state) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// registerSchedulerJob upserts an autoworker-tick job in the scheduler.
//
// Creates a type=shell job running `autoworker` every 30 minutes. If a job
// with the same name already exists, leaves it alone (the scheduler has no
// PUT-by-name primitive; users edit via the API or UI).
func registerSchedulerJob() error {
	schedURL := envOr("SCHEDULER_URL", "http://localhost:8092")
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, "bin", "autoworker")

	// See if a job with this name already exists.
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
		if j.Name == "autoworker-tick" {
			fmt.Println("autoworker-tick already registered; no changes made")
			return nil
		}
	}

	payload := map[string]any{
		"name":        "autoworker-tick",
		"description": "Gated todo-worker dispatcher. Fires budgeted agents when usage is below the weekly-reset curve.",
		"schedule":    "*/30 * * * *",
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
	fmt.Printf("registered autoworker-tick: %s\n", strings.TrimSpace(string(body)))
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

const defaultPrompt = `You are the nightly todo-worker. The user is asleep; their subscription budget is under-utilised for this week and I am dispatching you to make useful progress on open todos without disturbing them.

Steps:
1. GET http://localhost:8191/api/items?type=todo&status=open&exclude_tag=autoworker&exclude_tag=personal&sort=priority&limit=200 to read the noteboard todo list. (exclude_tag=autoworker drops machine-generated autoworker dispatch cards, which otherwise saturate the window and hide real todos. exclude_tag=personal drops the user's personal/life todos — those are theirs to do, never yours. sort=priority is what makes step 2 answerable: noteboard's default order is newest-created-first, so a limit applied to it drops the oldest todos whatever their priority, and the long-standing backlog is exactly the oldest part of the list. Keep the limit above the number of open todos; if the response ever comes back full, raise it rather than working from the truncated list.)
   Noteboard withholds HELD todos from this list automatically — those are tasks the user has parked behind the hold gate and has not cleared for unattended work. NEVER pass include_held=true, here or to /api/search: that parameter exists for the board and notes UIs, which need to show the user their own parked work so they can press play on it. Using it to widen your own search would walk straight through the one gate that is there to stop you.
2. Pick the highest-priority open todo that you can make real progress on autonomously without needing clarifying questions from the user. Prefer codebase tasks that have clear scope. Never pick a todo tagged "personal" even if one slips through — those are the user's own life tasks, not engineering work.
3. Do the work. Use your normal tools: explore repos under ~/repos/, edit, build, test, commit only if the task is a true "commit me" (match the user's normal guidance about explicit commit requests).
4. When the task is complete, PATCH http://localhost:8191/api/items/<id> with {"status":"done"} and a note in the body describing what you did.
5. If the top todo is ambiguous in intent or needs user input, leave it alone and try the next one.
6. If the top todo is clear in intent but too large to finish in one session, do NOT skip it. Split it instead:
   - Break it into several smaller noteboard todos, each scoped to something an unattended session could plausibly finish. Preserve the parent's priority and tags on the children so its place in the queue is respected.
   - Set "parent_id" to the parent's id on every child. This is not bookkeeping: the hold gate and the spend ceiling both roll up over parent_id. A child created without it is a child that escapes the parent's hold and escapes the dollar limit the user put on that task, which is the one way an unattended worker can spend money the user explicitly capped. Do NOT copy the parent's auto_hold_at_usd onto the children — the ceiling is a shared pot for the whole tree, and copying it would give each child its own full budget.
   - POST a noteboard note (type "note") as a starting-point / onboarding doc for the parent: architecture pointers, open questions, first-steps checklist.
   - Link children back to the parent (mention parent id in their bodies) and either keep the parent todo open with a body update pointing at the children + onboarding note, or mark it done once the split is in place — pick whichever keeps the work most discoverable.
   Splitting + documenting IS the night's progress; treat it as completing a todo.
7. If every top todo is blocked, stop — better to do nothing than the wrong thing.

Be self-contained. No chat; just work and close out the todo.`
