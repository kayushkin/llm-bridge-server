// kanban-dispatcher picks sub-cards out of the "Task completion loop"
// board's Backlog column, decides whether to spawn a fresh bridge session
// or revive an idle sibling session, publishes a BridgeInbound on NATS,
// and rolls up parent cards once their children have all settled.
//
// Cadence:
//
//	Registered as a scheduler shell job running every ~5 minutes. Each tick:
//
//	  1. Walk the board.
//	  2. For each parent in Goals (with the "scoped" entity_tag):
//	     a. Find Backlog children for that parent (no session link yet).
//	     b. Group them by their group:<key> tag.
//	     c. For each group, pick the first eligible child:
//	        - Ask kanban-store for the child's effective defaults: the board's
//	          tag rules matched against the child's tags, falling through to
//	          the board's own defaults. That answer — not the board's fields —
//	          is the instance, agent and bundle the child runs with. A child
//	          whose effective defaults name no instance is NOT dispatched; it
//	          is logged, the other groups carry on, and the run exits non-zero.
//	        - Look at sibling cards (same parent, same group) for an idle
//	          session whose last update was within the revive window and that
//	          was started with the same instance, agent and bundle the child
//	          resolves to. If one exists, REUSE its session_id (publish Send
//	          only).
//	        - Otherwise mint a fresh session_id and publish Create+Send.
//	     d. Add a session card_link, move the child to In Progress.
//	  3. Final pass — for each scoped parent, if every child noteboard item
//	     has status "done" or "archived", move the parent to Done with a
//	     rollup body note.
//
// Session IDs are canonical post session-consolidation (2026-05-09): the
// BridgeInbound.BusSessionID we publish becomes the llm-bridge-server
// session_id verbatim, so the card_link entity_ref is the deeplink target.
//
// Closure of in-progress sub-cards is left to kanban-curator (run with
// `-board-id <this board's id>` on its own scheduler tick), which moves
// In Progress → Done/Failed based on bridge session state.
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
	"sort"
	"strings"
	"time"

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/bus/messages"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/slava/scheduler/internal/bridgeauth"
	"github.com/slava/scheduler/internal/bridgeclient"
	"github.com/slava/scheduler/internal/bridgesession"
	schedulerconfig "github.com/slava/scheduler/internal/config"
	"github.com/slava/scheduler/internal/degradations"
	"github.com/slava/scheduler/internal/kanbanclient"
	"github.com/slava/scheduler/internal/kanbanvocab"
	"github.com/slava/scheduler/internal/textcut"
)

// linkTypeSession is the canonical session card-link entity_type. Pre-2026-05
// dispatcher writes used "bus_session"; a one-shot migration rewrote those to
// "session" once bus_session_id became the bridge session_id.

const (
	columnGoals        = kanbanvocab.ColumnGoals
	columnBacklog      = kanbanvocab.ColumnBacklog
	columnInProgress   = kanbanvocab.ColumnInProgress
	columnDone         = kanbanvocab.ColumnDone
	columnFailed       = kanbanvocab.ColumnFailed
	tagSubtask         = kanbanvocab.TagSubtask
	tagScoped          = kanbanvocab.TagScoped
	tagGroupPrefix     = kanbanvocab.TagGroupPrefix
	linkTypeParent     = kanbanvocab.LinkTypeParentCard
	linkTypeSession    = kanbanvocab.LinkTypeSession
	reviveWindow       = time.Hour
	maxDispatchPerTick = 3
)

func main() {
	settings, err := schedulerconfig.KanbanDispatcher.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatal(err)
	}
	var (
		registerFlag = flag.Bool("register", false, "Register/upsert this binary as a scheduler cron job and exit.")
		dryRun       = flag.Bool("dry-run", false, "Decide but do not publish or move cards.")
		boardID      = flag.String("board-id", "", "Kanban board id to dispatch on (required). The board owns what a session runs with — per card, its tag rules matched against the card's tags, then its own default_instance_id, default_agent_id and default_bundle_id — and this job owns only when.")
		kanbanURL    = flag.String("kanban-url", settings.String(schedulerconfig.SettingKanbanStoreURL), "kanban-store base URL")
		bridgeURL    = flag.String("bridge-url", settings.String(schedulerconfig.SettingBridgeURL), "llm-bridge-server base URL")
		maxDispatch  = flag.Int("max", maxDispatchPerTick, "Max sub-card dispatches per tick.")
	)
	flag.Parse()

	// llm-bridge-server gates every route; stamp this process's calls to it
	// with the service token. Host-scoped, so the scheduler API, noteboard and
	// kanban-store calls below are untouched.
	if err := bridgeauth.StampRequestsToBridge(*bridgeURL, settings.String(schedulerconfig.SettingBridgeServiceToken)); err != nil {
		log.Fatal(err)
	}

	if *boardID == "" {
		log.Fatal("-board-id is required: the board owns what a dispatched session runs with, so there is no board to default to")
	}

	if *registerFlag {
		if err := registerSchedulerJob(settings.String(schedulerconfig.SettingSchedulerURL), *boardID); err != nil {
			log.Fatalf("register: %v", err)
		}
		return
	}

	err = run(runOptions{
		BoardID:            *boardID,
		KanbanURL:          *kanbanURL,
		KanbanServiceToken: settings.String(schedulerconfig.SettingKanbanStoreServiceToken),
		BridgeURL:          *bridgeURL,
		NATSURL:            settings.String(schedulerconfig.SettingNATSURL),
		DryRun:             *dryRun,
		MaxDispatch:        *maxDispatch,
	})
	if err != nil {
		log.Fatalf("[dispatcher] %v", err)
	}
}

type runOptions struct {
	BoardID   string
	KanbanURL string
	// KanbanServiceToken is what the kanban-store client presents; kanban-store
	// gates every route.
	KanbanServiceToken string
	BridgeURL          string
	NATSURL            string
	DryRun             bool
	MaxDispatch        int
	// Publish sends one BridgeInbound. Left nil, run connects to NATSURL and
	// publishes there. A test sets it, because a test that reached the write
	// path through the real bus would dispatch a real agent session and spend
	// real money.
	Publish func(messages.BridgeInbound) error
}

// run is one tick. It returns an error — and main exits non-zero, so the
// scheduler records the run as `error` — when the tick cannot start, and also
// when it finished but fell short of its job: a card left undispatched for a
// reason a person has to fix, such as a card nothing gives an instance, or any
// read or write the tick needed and could not make.
//
// ==================== a tick that fell short is not a success ====================
//
// Job 16 answered `success` on every retained run, and before the shortfalls
// were recorded it would have done so through any of them, because the answer
// to each is `continue`. The worst is a Goals card whose `scoped` tag cannot be
// read: it is dropped inside scopedParents, before the caller counts what came
// back, so `scoped-parents=0` prints the same for a quiet board and a blind one.
// The quietest is the rollup: a refused PatchCardBody is followed by the move to
// Done anyway, so the parent lands finished with no record of what its children
// did, on a card nothing will revisit.
//
// Having nothing to do is not a shortfall, and none of these is recorded: an
// empty board, a goal with no Backlog children, the per-tick cap, a sibling
// session that is busy, stale or gone, a child that has not settled, a child
// already on its target column or not on this board, and a dry run.
func run(options runOptions) error {
	degraded := degradations.New("dispatcher")
	kc := kanbanclient.New(options.KanbanURL, options.KanbanServiceToken)
	board, err := kc.GetBoard(options.BoardID)
	if errors.Is(err, kanbanclient.ErrBoardNotFound) {
		return fmt.Errorf("board %s does not exist — refusing to create a replacement, since a second board is exactly the fork pinning the id prevents", options.BoardID)
	}
	if err != nil {
		return fmt.Errorf("get board %s: %w", options.BoardID, err)
	}
	// There is no run-level instance any more: with tag rules one card can have
	// an instance while the board has none, so each card is resolved on its own
	// just before it is dispatched.
	log.Printf("[dispatcher] board=%q id=%s — each card runs with its effective defaults from kanban-store (tag rules matching its tags, then the board's own)",
		board.Name, board.ID)
	view, err := kc.GetBoardView(board.ID)
	if err != nil {
		return fmt.Errorf("board view: %w", err)
	}
	colByName := map[string]string{}
	for _, cv := range view.Columns {
		colByName[cv.Column.Name] = cv.Column.ID
	}
	for _, want := range []string{columnGoals, columnBacklog, columnInProgress, columnDone, columnFailed} {
		if colByName[want] == "" {
			return fmt.Errorf("board %q missing required column %q", board.Name, want)
		}
	}

	publish := options.Publish
	if publish == nil && !options.DryRun {
		natsClient, err := natsbus.Connect(natsbus.Options{URL: options.NATSURL, Name: "kanban-dispatcher"})
		if err != nil {
			return fmt.Errorf("nats connect: %w", err)
		}
		defer natsClient.Close()
		publish = func(in messages.BridgeInbound) error {
			return natsClient.Publish(messages.BridgeInboundSubject, in)
		}
	}

	parents := scopedParents(view, kc, degraded)
	log.Printf("[dispatcher] board=%q scoped-parents=%d", board.Name, len(parents))

	dispatcher := &backlogDispatcher{
		kanban:             kc,
		bridgeURL:          options.BridgeURL,
		board:              board,
		view:               view,
		inProgressColumnID: colByName[columnInProgress],
		publish:            publish,
		dryRun:             options.DryRun,
		maxDispatch:        options.MaxDispatch,
		degraded:           degraded,
	}
	dispatched, undispatched := dispatcher.dispatchBacklog(parents)

	// Closure pass: children whose noteboard status is "done" / "archived"
	// but still sit in In Progress (because their bridge session went idle
	// instead of completed) need to be moved to Done / Failed so the rollup
	// can see the parent as settled. Only acts on scoped parents' children
	// — never touches cards outside the loop.
	closed := 0
	for _, p := range parents {
		entries, err := kc.ListCardsForEntity(linkTypeParent, p.cardID)
		if err != nil {
			log.Printf("[%s] list children for closure: %v", p.title(), err)
			degraded.Record("goal %s (%s): its children could not be listed for the closure pass (%v), so no settled child of it was moved out of In Progress and it cannot roll up",
				p.cardID, p.title(), err)
			continue
		}
		for _, e := range entries {
			status, _ := e.Item["status"].(string)
			var targetCol, targetName string
			switch status {
			case "done":
				targetCol, targetName = colByName[columnDone], columnDone
			case "archived":
				targetCol, targetName = colByName[columnFailed], columnFailed
			default:
				// Still being worked on.
				continue
			}
			placement, err := currentPlacement(kc, e.CardID, board.ID)
			if err != nil {
				log.Printf("[%s] closure: read placement of child %s: %v", p.title(), shortID(e.CardID), err)
				degraded.Record("goal %s (%s): child %s is %s but its placement could not be read (%v), so it was left where it is and the parent cannot roll up",
					p.cardID, p.title(), e.CardID, status, err)
				continue
			}
			if placement == "" {
				// Not on this board, so there is no placement here to correct.
				// Logged because it used to read the same as a failed read.
				log.Printf("[%s] closure: child %s is %s but is not placed on this board — nothing to move", p.title(), shortID(e.CardID), status)
				continue
			}
			if placement == targetCol {
				continue
			}
			log.Printf("[%s] closure: child %s status=%s → %s", p.title(), shortID(e.CardID), status, targetName)
			if options.DryRun {
				continue
			}
			moveResult, err := kc.MoveCard(e.CardID, board.ID, targetCol, 0)
			if err != nil {
				log.Printf("[%s] move child to %s: %v", p.title(), targetName, err)
				degraded.Record("goal %s (%s): child %s is %s but the board refused to move it to %s (%v), so it stays in In Progress and the parent cannot roll up",
					p.cardID, p.title(), e.CardID, status, targetName, err)
				continue
			}
			if moveResult.AutoStatusError != "" {
				log.Printf("[%s] moved child %s to %s but its noteboard status was not set to match: %s", p.title(), shortID(e.CardID), targetName, moveResult.AutoStatusError)
			}
			closed++
		}
	}

	// Parent rollup: any scoped parent whose children are all settled → move to Done.
	rolled := 0
	for _, p := range parents {
		settled, total, summary, err := childrenSettlement(kc, p.cardID)
		if err != nil {
			log.Printf("[%s] rollup: list children: %v", p.title(), err)
			degraded.Record("goal %s (%s): its children could not be listed for the rollup (%v), so it was not rolled up whether or not it was finished",
				p.cardID, p.title(), err)
			continue
		}
		if total == 0 || settled < total {
			continue
		}
		log.Printf("[%s] rollup: %d/%d children settled", p.title(), settled, total)
		if options.DryRun {
			continue
		}
		body, _ := p.item["body"].(string)
		suffix := fmt.Sprintf("\n\n---\n_Rolled up by kanban-dispatcher at %s — %s_\n",
			time.Now().UTC().Format(time.RFC3339), summary)
		if err := kc.PatchCardBody(p.cardID, body+suffix); err != nil {
			log.Printf("[%s] patch parent body: %v (continuing to move)", p.title(), err)
			degraded.Record("goal %s (%s): the board refused its rollup note (%v) and it is moved to Done regardless, so it lands finished with no record of what its %d children did",
				p.cardID, p.title(), err, total)
		}
		moveResult, err := kc.MoveCard(p.cardID, board.ID, colByName[columnDone], 0)
		if err != nil {
			log.Printf("[%s] move parent to Done: %v", p.title(), err)
			degraded.Record("goal %s (%s): all %d of its children are settled but the board refused to move it to Done (%v), so it stays in Goals and every later tick re-walks it",
				p.cardID, p.title(), total, err)
			continue
		}
		if moveResult.AutoStatusError != "" {
			log.Printf("[%s] moved parent to Done but its noteboard status was not set to match: %s", p.title(), moveResult.AutoStatusError)
		}
		rolled++
	}

	log.Printf("[dispatcher] done: dispatched=%d closed=%d rolled-up=%d not-dispatched=%d", dispatched, closed, rolled, len(undispatched))
	for _, card := range undispatched {
		degraded.Record("card %s %q was not dispatched (%v)", card.CardID, card.Title, card.Reason)
	}
	if degraded.HasShortfall() {
		reasons := degraded.Reasons()
		return fmt.Errorf("run degraded: %d shortfall(s) — %s", len(reasons), strings.Join(reasons, "; "))
	}
	return nil
}

// currentPlacement returns the column id this card is on for the given board.
// Empty string means the card is not placed on this board (orphaned).
//
// It reads through the kanban client so the request carries the service token.
// It used to be a bare http.Get, which kanban-store answers 401 — and because
// the closure pass treated a failed read as "nothing to move", every closure
// was skipped in silence.
func currentPlacement(kc *kanbanclient.Client, cardID, boardID string) (string, error) {
	placements, err := kc.ListCardPlacements(cardID)
	if err != nil {
		return "", err
	}
	for _, p := range placements {
		if p.BoardID == boardID {
			return p.ColumnID, nil
		}
	}
	return "", nil
}

// ============================ Backlog dispatch ============================

// backlogDispatcher is one tick's dispatch pass over a board's Backlog.
type backlogDispatcher struct {
	kanban             *kanbanclient.Client
	bridgeURL          string
	board              *kanbanclient.Board
	view               *kanbanclient.BoardView
	inProgressColumnID string
	// publish sends one BridgeInbound. Nil on a dry run, which publishes nothing.
	publish     func(messages.BridgeInbound) error
	dryRun      bool
	maxDispatch int
	// degraded collects the reads this pass needed and could not make. A card
	// it chose and could not dispatch is returned as undispatchedCard instead.
	degraded *degradations.Recorder
}

// undispatchedCard is a Backlog card this tick chose to dispatch and could not.
type undispatchedCard struct {
	CardID string
	Title  string
	Reason error
}

// dispatchBacklog dispatches the first card of each Backlog group under each
// scoped parent, up to maxDispatch, each with its own effective defaults. A card
// that cannot be dispatched is logged where it fails and returned, and the
// other groups carry on: with tag rules, one card lacking an instance says
// nothing about the next. The group behind that card waits — its order is the
// point of a group.
func (d *backlogDispatcher) dispatchBacklog(parents []scopedParent) (dispatched int, undispatched []undispatchedCard) {
	for _, p := range parents {
		if dispatched >= d.maxDispatch {
			break
		}
		groups := backlogGroups(d.kanban, d.view, p.cardID, d.degraded)
		for _, g := range groups {
			if dispatched >= d.maxDispatch {
				break
			}
			child := g.cards[0] // first in group
			childID := child.Placement.CardID
			notDispatched := func(reason error) {
				log.Printf("[%s] NOT DISPATCHED card %s %q group=%s: %v — it stays where it is and the rest of its group waits behind it",
					p.title(), childID, childTitle(child), g.key, reason)
				undispatched = append(undispatched, undispatchedCard{CardID: childID, Title: childTitle(child), Reason: reason})
			}

			spec, err := resolveCardSessionSpec(d.kanban, d.bridgeURL, d.board, childID)
			if err != nil {
				notDispatched(err)
				continue
			}
			reusedSessionID := reusableSiblingSession(d.kanban, d.bridgeURL, d.view, g, spec, d.degraded)
			sessionID := reusedSessionID
			if sessionID == "" {
				sessionID = mintSessionID(p.cardID, g.key)
			}
			prompt := buildPrompt(p, child, g.key, reusedSessionID)
			displayName := fmt.Sprintf("tcl %s · %s", g.key, time.Now().Format("Jan 2 15:04"))

			log.Printf("[%s] dispatching %q card=%s group=%s reuse=%v session=%s %s",
				p.title(), childTitle(child), childID, g.key, reusedSessionID != "", sessionID, spec.describe())
			if d.dryRun {
				dispatched++
				continue
			}

			in := buildBridgeInbound(sessionID, displayName, prompt, reusedSessionID == "", spec)
			if err := d.publish(in); err != nil {
				notDispatched(fmt.Errorf("nats publish: %w", err))
				continue
			}
			if err := d.kanban.AddCardLink(childID, linkTypeSession, sessionID, "tcl-dispatcher"); err != nil {
				notDispatched(fmt.Errorf("session %s was published but linking it to the card failed: %w", sessionID, err))
				continue
			}
			moveResult, err := d.kanban.MoveCard(childID, d.board.ID, d.inProgressColumnID, 0)
			if err != nil {
				notDispatched(fmt.Errorf("session %s was published and linked but moving the card to In Progress failed: %w", sessionID, err))
				continue
			}
			if moveResult.AutoStatusError != "" {
				log.Printf("moved %s to In Progress but its noteboard status was not set to match: %s", shortID(childID), moveResult.AutoStatusError)
			}
			dispatched++
		}
	}
	return dispatched, undispatched
}

// ============================ Parent / group selection ============================

type scopedParent struct {
	cardID string
	item   map[string]any
}

func (p scopedParent) title() string {
	t, _ := p.item["title"].(string)
	if t == "" {
		t = p.cardID
	}
	return t
}

// scopedParents returns Goals-column cards that have the scoped entity_tag.
// A Goals card whose tags cannot be read is dropped here, before the caller
// counts what came back, so the count alone cannot tell "nothing is scoped"
// from "nothing could be read"; the shortfall is recorded instead.
func scopedParents(view *kanbanclient.BoardView, kc *kanbanclient.Client, degraded *degradations.Recorder) []scopedParent {
	var goalsCards []kanbanclient.CardView
	for _, cv := range view.Columns {
		if cv.Column.Name == columnGoals {
			goalsCards = cv.Cards
			break
		}
	}
	var out []scopedParent
	for _, c := range goalsCards {
		if c.Item == nil {
			continue
		}
		etags, err := kc.ListEntityTags(kanbanvocab.EntityTypeNote, c.Placement.CardID)
		if err != nil {
			log.Printf("[%s] list entity tags: %v — skip", c.Placement.CardID, err)
			degraded.Record("goal %s (%q): its scoped tag could not be read (%v), so it was never considered and none of its Backlog children were dispatched",
				c.Placement.CardID, childTitle(c), err)
			continue
		}
		if !anyTagEquals(etags, tagScoped) {
			continue
		}
		out = append(out, scopedParent{cardID: c.Placement.CardID, item: c.Item})
	}
	return out
}

type backlogGroup struct {
	parentID string
	key      string
	cards    []kanbanclient.CardView // sorted by placement position
}

// backlogGroups returns groups of Backlog children for a given parent.
// A child counts as "in this parent" when it has a card_link with
// entity_type=parent_card pointing to parent_id. Group key comes from a
// "group:<key>" tag; missing group → key "".
func backlogGroups(kc *kanbanclient.Client, view *kanbanclient.BoardView, parentID string, degraded *degradations.Recorder) []backlogGroup {
	var backlogCards []kanbanclient.CardView
	for _, cv := range view.Columns {
		if cv.Column.Name == columnBacklog {
			backlogCards = cv.Cards
			break
		}
	}
	byKey := map[string][]kanbanclient.CardView{}
	// List the cards linked to this parent_card entity, then intersect with
	// Backlog placements in the view.
	parentChildren := map[string]bool{}
	childIDs, err := listChildCardIDs(kc, parentID)
	if err != nil {
		log.Printf("[%s] list children: %v", parentID, err)
		degraded.Record("goal %s: its children could not be listed (%v), so its whole Backlog was invisible to this tick and nothing under it was dispatched",
			parentID, err)
		return nil
	}
	for _, id := range childIDs {
		parentChildren[id] = true
	}
	for _, c := range backlogCards {
		if c.Item == nil || !parentChildren[c.Placement.CardID] {
			continue
		}
		key := groupKey(c.Item)
		byKey[key] = append(byKey[key], c)
	}
	var out []backlogGroup
	for k, cards := range byKey {
		sort.Slice(cards, func(i, j int) bool {
			return cards[i].Placement.Position < cards[j].Placement.Position
		})
		out = append(out, backlogGroup{parentID: parentID, key: k, cards: cards})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// listChildCardIDs returns the ids of the cards linked to a parent card, read
// from the kanban-store the -kanban-url flag names.
func listChildCardIDs(kc *kanbanclient.Client, parentID string) ([]string, error) {
	entries, err := kc.ListCardsForEntity(linkTypeParent, parentID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.CardID)
	}
	return out, nil
}

func groupKey(item map[string]any) string {
	for _, t := range stringSlice(item["tags"]) {
		if strings.HasPrefix(t, tagGroupPrefix) {
			return strings.TrimPrefix(t, tagGroupPrefix)
		}
	}
	return ""
}

// ============================ Revive decision ============================

// reusableSiblingSession looks at sibling sub-cards under the same parent (any
// column, not just Backlog) that share this group's key and have a session
// link, and returns the first session that is idle, was updated within the
// revive window, and was started with exactly what the card resolves to: the
// same instance, the same agent and the same bundle. Returns "" when no
// sibling qualifies, and the caller spawns a fresh session.
//
// Why all three and not just the instance: llm-bridge-server sets each of them
// once, at session creation. Sending a card into a sibling's session started on
// another instance would run it there; one started as another agent or with
// another bundle would run it under that identity or without its bundle's
// tools — each a way of ignoring the tag rule that gave the card its defaults.
// A fresh session costs some context; the wrong session costs correctness.
//
// A sibling whose links or session state cannot be read is a sibling that
// cannot be revived, so the group may pay for a fresh session it did not need;
// that is recorded. A sibling session that is busy, stale or gone is the answer
// working, and is not.
func reusableSiblingSession(kc *kanbanclient.Client, bridgeURL string, view *kanbanclient.BoardView, g backlogGroup, spec cardSessionSpec, degraded *degradations.Recorder) string {
	candidateID := g.cards[0].Placement.CardID
	siblings := groupSiblings(kc, view, g.parentID, g.key, degraded)
	now := time.Now()
	for _, sibling := range siblings {
		// Skip the candidate itself (still in Backlog with no link yet).
		if sibling.Placement.CardID == candidateID {
			continue
		}
		links, err := kc.ListCardLinks(sibling.Placement.CardID)
		if err != nil {
			log.Printf("[reuse] card %s: sibling %s links unreadable, not reusing its session: %v", candidateID, sibling.Placement.CardID, err)
			degraded.Record("goal %s group %q: sibling card %s could not be read for an idle session to revive (%v), so card %s may pay for a fresh session it did not need",
				g.parentID, g.key, sibling.Placement.CardID, err, candidateID)
			continue
		}
		var sid string
		for _, l := range links {
			if l.EntityType == linkTypeSession {
				sid = l.EntityRef
				break
			}
		}
		if sid == "" {
			continue
		}
		session, err := getBridgeSession(bridgeURL, sid)
		if errors.Is(err, errSessionGone) {
			// Nothing to revive; minting a fresh session is right.
			continue
		}
		if err != nil {
			log.Printf("[reuse] card %s: sibling session %s unreadable, not reusing it: %v", candidateID, sid, err)
			degraded.Record("goal %s group %q: the state of sibling session %s could not be read (%v), so card %s may pay for a fresh session it did not need",
				g.parentID, g.key, sid, err, candidateID)
			continue
		}
		if session.State != "idle" {
			continue
		}
		// A timestamp that cannot be read is not evidence of freshness. Both of
		// these used to fall through to the reuse, so a bridge that omitted
		// updated_at, or wrote it in a format time.Parse does not accept,
		// revived a session of ANY age and gave it a new sub-task. Ported from
		// f119a711.
		if session.UpdatedAt == "" {
			log.Printf("[reuse] card %s: sibling session %s reports no updated_at — not reviving on an unknown age", candidateID, sid)
			continue
		}
		updatedAt, err := time.Parse(time.RFC3339, session.UpdatedAt)
		if err != nil {
			log.Printf("[reuse] card %s: sibling session %s has an unreadable updated_at %q: %v — not reviving", candidateID, sid, session.UpdatedAt, err)
			continue
		}
		if now.Sub(updatedAt) > reviveWindow {
			continue
		}
		if difference := session.differenceFrom(spec); difference != "" {
			log.Printf("[reuse] card %s: not reusing idle sibling session %s — %s", candidateID, sid, difference)
			continue
		}
		return sid
	}
	return ""
}

// groupSiblings returns ALL cards on the board that share the parent_card
// link and the same group:<key> tag — across every column, so we can find a
// freshly-completed sibling whose session is still idle.
func groupSiblings(kc *kanbanclient.Client, view *kanbanclient.BoardView, parentID, key string, degraded *degradations.Recorder) []kanbanclient.CardView {
	parentChildren := map[string]bool{}
	childIDs, err := listChildCardIDs(kc, parentID)
	if err != nil {
		log.Printf("[%s] list children for reuse: %v — no sibling session will be reused", parentID, err)
		degraded.Record("goal %s group %q: its sibling cards could not be listed (%v), so no idle session could be found to revive and the group pays for a fresh one",
			parentID, key, err)
		return nil
	}
	for _, id := range childIDs {
		parentChildren[id] = true
	}
	var out []kanbanclient.CardView
	for _, cv := range view.Columns {
		for _, c := range cv.Cards {
			if !parentChildren[c.Placement.CardID] {
				continue
			}
			if c.Item == nil {
				continue
			}
			if groupKey(c.Item) != key {
				continue
			}
			out = append(out, c)
		}
	}
	return out
}

// ============================ Bridge inbound ============================

func mintSessionID(parentID, groupKey string) string {
	now := time.Now().UTC().UnixNano()
	safeGroup := strings.ReplaceAll(groupKey, " ", "-")
	if safeGroup == "" {
		safeGroup = "default"
	}
	return fmt.Sprintf("tcl-%s-%s-%d", shortID(parentID), safeGroup, now)
}

// shortID returns at most the first 8 bytes of an id, cut on a rune boundary;
// useful for human-readable session keys without collision risk at this scale.
// A byte cut would split a rune, and mintSessionID's result crosses JSON, where
// encoding/json swaps the broken bytes for U+FFFD and returns no error. Latent:
// kanban-store mints every card id as a uuid.
func shortID(id string) string {
	return textcut.PrefixWithinByteBudgetOnRuneBoundary(id, 8)
}

// errCardHasNoInstance marks a card whose effective defaults name no instance:
// no tag rule matching its tags sets default_instance_id, and the board does
// not set one either.
var errCardHasNoInstance = errors.New("no instance to run on")

// cardSessionSpec is what a session for one card runs with, resolved from that
// card's effective defaults.
type cardSessionSpec struct {
	Harness    msg.Harness
	InstanceID string
	// AgentSlug is empty when nothing sets default_agent_id for the card; the
	// bridge then starts the session as no agent.
	AgentSlug string
	// BundleID is the card's effective default_bundle_id verbatim, or empty.
	BundleID string
	// CardID is the card the session is for. The bridge reads its tags at
	// every spawn to pick the context sections it injects.
	CardID string
	// Defaults is kanban-store's answer the spec was read from, kept so a log
	// line can say where each value came from.
	Defaults *kanbanclient.EffectiveDefaults
}

// resolveCardSessionSpec asks kanban-store for the card's effective defaults
// on the board and turns them into what POST /sessions takes. kanban-store owns
// the precedence (matching tag rules in order, then the board); this reads its
// answer and never re-derives it. The defaults are ids — the instance id and
// agent-store's numeric agent id — and the bridge wants the harness type and
// the agent's slug, so both are read from the bridge now rather than copied
// anywhere earlier and trusted since.
func resolveCardSessionSpec(kc *kanbanclient.Client, bridgeURL string, board *kanbanclient.Board, cardID string) (cardSessionSpec, error) {
	defaults, err := kc.GetCardEffectiveDefaults(board.ID, cardID)
	if err != nil {
		return cardSessionSpec{}, fmt.Errorf("effective defaults: %w", err)
	}
	instance, ok := defaults.Default(kanbanclient.DefaultFieldInstance)
	if !ok {
		return cardSessionSpec{}, fmt.Errorf("%w: neither a tag rule matching the card's tags %q nor board %q (%s) sets default_instance_id — add a tag rule (PUT /api/boards/%s/tag-rules) or set the board's (PATCH /api/boards/%s); there is no fallback instance",
			errCardHasNoInstance, defaults.Tags, board.Name, board.ID, board.ID, board.ID)
	}
	harness, err := bridgeclient.InstanceHarnessType(bridgeURL, instance.Value)
	if err != nil {
		return cardSessionSpec{}, fmt.Errorf("default_instance_id from %s: %w", instance.Source.Describe(), err)
	}
	spec := cardSessionSpec{Harness: harness, InstanceID: instance.Value, CardID: cardID, Defaults: defaults}
	if bundle, ok := defaults.Default(kanbanclient.DefaultFieldBundle); ok {
		spec.BundleID = bundle.Value
	}
	if agent, ok := defaults.Default(kanbanclient.DefaultFieldAgent); ok {
		slug, err := bridgeclient.AgentSlugByID(bridgeURL, agent.Value)
		if err != nil {
			return cardSessionSpec{}, fmt.Errorf("default_agent_id from %s: %w", agent.Source.Describe(), err)
		}
		spec.AgentSlug = slug
	}
	return spec, nil
}

// describe renders the spec for a log line, naming where each value came from.
func (s cardSessionSpec) describe() string {
	instance, _ := s.Defaults.Default(kanbanclient.DefaultFieldInstance)
	parts := []string{fmt.Sprintf("instance=%s harness=%s (from %s)", s.InstanceID, s.Harness, instance.Source.Describe())}
	if agent, ok := s.Defaults.Default(kanbanclient.DefaultFieldAgent); ok {
		parts = append(parts, fmt.Sprintf("agent=%s id=%s (from %s)", s.AgentSlug, agent.Value, agent.Source.Describe()))
	} else {
		parts = append(parts, "agent=none (nothing sets default_agent_id)")
	}
	if bundle, ok := s.Defaults.Default(kanbanclient.DefaultFieldBundle); ok {
		parts = append(parts, fmt.Sprintf("bundle=%s (from %s)", s.BundleID, bundle.Source.Describe()))
	} else {
		parts = append(parts, "bundle=none (nothing sets default_bundle_id)")
	}
	return strings.Join(parts, " ")
}

func buildBridgeInbound(sessionID, displayName, prompt string, fresh bool, spec cardSessionSpec) messages.BridgeInbound {
	in := messages.BridgeInbound{
		BusSessionID: sessionID,
		Agent:        "tcl-worker",
		Orchestrator: "task-completion-loop",
		Channel:      "task-completion-loop",
		Timestamp:    time.Now().UTC(),
		Send: &msg.SendMessageRequest{
			Message: prompt,
		},
	}
	if fresh {
		in.Create = &msg.CreateSessionRequest{
			Harness:     spec.Harness,
			InstanceID:  spec.InstanceID,
			AgentID:     spec.AgentSlug,
			BundleID:    spec.BundleID,
			CardID:      spec.CardID,
			DisplayName: displayName,
			Purpose:     "dispatcher",
			Origin:      "scheduler",
			Type:        msg.SessionTypeAutonomous,
			AutoStart:   true,
		}
	}
	return in
}

func buildPrompt(p scopedParent, child kanbanclient.CardView, group string, reusedSessionID string) string {
	childID := child.Placement.CardID
	title := childTitle(child)
	body, _ := child.Item["body"].(string)
	if body == "" {
		body = "(no body — see title)"
	}
	header := "You are picking up a sub-task in the **task-completion-loop** workflow."
	if reusedSessionID != "" {
		header = "Picking up another sub-task in the same group — you already have context from a sibling task in this session."
	}
	return fmt.Sprintf(`%s

## Sub-task
**ID:** %s
**Title:** %s

%s

## Context
- Parent goal: %q (id: %s)
- Group: %s

## Instructions
1. Do the work described above.
2. When done, PATCH http://localhost:8191/api/items/%s with {"status":"done"} and a body update describing what you did.
3. Then exit cleanly. The kanban-curator will detect session completion and move the card to Done.
4. If the task is blocked or unclear, leave a body update explaining why and exit; the card stays In Progress and a human will intervene.

Be self-contained. No chat; just work and close out the sub-task.`,
		header, childID, title, body, p.title(), p.cardID, group, childID)
}

func childTitle(c kanbanclient.CardView) string {
	t, _ := c.Item["title"].(string)
	if t == "" {
		t = c.Placement.CardID
	}
	return t
}

// ============================ Parent rollup ============================

// childrenSettlement returns (settled, total, summary) for a parent card.
// A child counts as settled if its noteboard status is "done" or "archived".
//
// A failed read is returned as an error, not folded into the summary: the
// caller reads total == 0 as "nothing to roll up", so (0, 0, "rollup error: …")
// made a failed read look like a parent with no children.
func childrenSettlement(kc *kanbanclient.Client, parentID string) (int, int, string, error) {
	entries, err := kc.ListCardsForEntity(linkTypeParent, parentID)
	if err != nil {
		return 0, 0, "", err
	}
	if len(entries) == 0 {
		return 0, 0, "no children", nil
	}
	var done, archived, other int
	for _, e := range entries {
		status, _ := e.Item["status"].(string)
		switch status {
		case "done":
			done++
		case "archived":
			archived++
		default:
			other++
		}
	}
	settled := done + archived
	total := len(entries)
	summary := fmt.Sprintf("children: done=%d failed=%d open=%d", done, archived, other)
	return settled, total, summary, nil
}

// ============================ HTTP / NATS helpers ============================

// errSessionGone is getBridgeSession's answer to a 404. A session that no
// longer exists is not one the tick failed to read: there is nothing to revive.
var errSessionGone = errors.New("session no longer exists")

// bridgeSession is the part of llm-bridge-server's GET /sessions/{id} the
// revive decision reads.
type bridgeSession struct {
	State      string `json:"state"`
	UpdatedAt  string `json:"updated_at"`
	InstanceID string `json:"instance_id"`
	// AgentID is the agent's slug, which is what a session is created with.
	AgentID  string `json:"agent_id"`
	BundleID string `json:"bundle_id"`
}

// differenceFrom says how the session was started with something other than
// what the card resolves to, or "" when instance, agent and bundle all match.
func (s bridgeSession) differenceFrom(spec cardSessionSpec) string {
	var differences []string
	if s.InstanceID != spec.InstanceID {
		differences = append(differences, fmt.Sprintf("it runs on instance %q and the card resolves to %q", s.InstanceID, spec.InstanceID))
	}
	if s.AgentID != spec.AgentSlug {
		differences = append(differences, fmt.Sprintf("it runs as agent %q and the card resolves to %q", s.AgentID, spec.AgentSlug))
	}
	if s.BundleID != spec.BundleID {
		differences = append(differences, fmt.Sprintf("it was started with bundle %q and the card resolves to %q", s.BundleID, spec.BundleID))
	}
	return strings.Join(differences, "; ")
}

func getBridgeSession(bridgeURL, sessionID string) (bridgeSession, error) {
	url, err := bridgesession.URL(bridgeURL, sessionID)
	if err != nil {
		return bridgeSession{}, err
	}
	resp, err := http.Get(url)
	if err != nil {
		return bridgeSession{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return bridgeSession{}, errSessionGone
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return bridgeSession{}, fmt.Errorf("HTTP %s — %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var session bridgeSession
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return bridgeSession{}, err
	}
	return session, nil
}

// ============================ Scheduler self-registration ============================

// registerSchedulerJob creates the tick job, with the board id baked into the
// command: the scheduler runs jobs as `sh -c` with its own thin environment,
// so nothing else would carry it to the process.
func registerSchedulerJob(schedURL, boardID string) error {
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, "bin", "kanban-dispatcher") + " -board-id " + boardID

	resp, err := http.Get(schedURL + "/api/jobs")
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// The list is read to decide whether the job is ALREADY there, so a list
	// that could not be read must stop the registration. Both of these used to
	// be discarded: a scheduler answering 500, or answering anything that is not
	// a JSON array, left `jobs` empty, which reads exactly like "not registered
	// yet" — and the POST below then adds a second tick that double-dispatches
	// every card. Ported from f119a711.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("list jobs: scheduler returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var jobs []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &jobs); err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}
	for _, j := range jobs {
		if j.Name == "kanban-dispatcher-tick" {
			fmt.Println("kanban-dispatcher-tick already registered; no changes made")
			return nil
		}
	}
	payload := map[string]any{
		"name":        "kanban-dispatcher-tick",
		"description": "Picks Backlog sub-cards under scoped parents, reuses idle sibling sessions or spawns new ones, rolls up parents.",
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
	fmt.Printf("registered kanban-dispatcher-tick: %s\n", strings.TrimSpace(string(body)))
	return nil
}

// ============================ misc ============================

func stringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func anyTagEquals(tags []kanbanclient.EntityTag, want string) bool {
	for _, t := range tags {
		if t.Tag == want {
			return true
		}
	}
	return false
}
