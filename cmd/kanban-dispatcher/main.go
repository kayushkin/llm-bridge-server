// kanban-dispatcher picks sub-cards out of the "Task completion loop"
// board's Backlog column, decides whether to spawn a fresh bridge session
// or revive an idle sibling session, publishes a BridgeInbound on NATS,
// and rolls up parent cards once their children have all settled.
//
// Cadence:
//
//   Registered as a scheduler shell job running every ~5 minutes. Each tick:
//
//     1. Read llm-bridge-adapter's bus_session_id → bridge_id map.
//     2. Walk the board.
//     3. For each parent in Goals (with the "scoped" entity_tag):
//        a. Find Backlog children for that parent (no bus_session link yet).
//        b. Group them by their group:<key> tag.
//        c. For each group, pick the first eligible child:
//           - Look at sibling cards (same parent, same group) for an idle
//             session whose last update was within the revive window. If one
//             exists, REUSE its bus_session_id (publish Send only).
//           - Otherwise mint a fresh bus_session_id and publish Create+Send.
//        d. Add a bus_session card_link, move the child to In Progress.
//     4. Final pass — for each scoped parent, if every child noteboard item
//        has status "done" or "archived", move the parent to Done with a
//        rollup body note.
//
// Closure of in-progress sub-cards is left to kanban-curator (run with
// `-board="Task completion loop"` on its own scheduler tick), which moves
// In Progress → Done/Failed based on bridge session state.
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
	"sort"
	"strings"
	"time"

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/bus/messages"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/slava/scheduler/internal/kanbanclient"
)

const (
	defaultBoardName  = "Task completion loop"
	columnGoals       = "Goals"
	columnBacklog     = "Backlog"
	columnInProgress  = "In Progress"
	columnDone        = "Done"
	columnFailed      = "Failed"
	tagSubtask        = "task-completion-loop-subtask"
	tagScoped         = "scoped"
	tagGroupPrefix    = "group:"
	linkTypeParent    = "parent_card"
	linkTypeBusSess   = "bus_session"
	defaultHarness    = msg.HarnessClaudeCode
	defaultInstanceID = "inst-cc-local"
	reviveWindow      = time.Hour
	maxDispatchPerTick = 3
)

func main() {
	var (
		registerFlag = flag.Bool("register", false, "Register/upsert this binary as a scheduler cron job and exit.")
		dryRun       = flag.Bool("dry-run", false, "Decide but do not publish or move cards.")
		boardName    = flag.String("board", defaultBoardName, "Kanban board name to dispatch on.")
		kanbanURL    = flag.String("kanban-url", envOr("KANBAN_STORE_URL", "http://localhost:8305"), "kanban-store base URL")
		bridgeURL    = flag.String("bridge-url", envOr("LLM_BRIDGE_URL", "http://localhost:8160"), "llm-bridge-server base URL")
		adapterFile  = flag.String("adapter-sessions", envOr("ADAPTER_SESSIONS_FILE", defaultAdapterSessionsPath()), "Path to llm-bridge-adapter sessions.json")
		maxDispatch  = flag.Int("max", maxDispatchPerTick, "Max sub-card dispatches per tick.")
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
		log.Printf("board %q does not exist yet — nothing to dispatch", *boardName)
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
	for _, want := range []string{columnGoals, columnBacklog, columnInProgress, columnDone, columnFailed} {
		if colByName[want] == "" {
			log.Fatalf("board %q missing required column %q", *boardName, want)
		}
	}

	var natsClient *natsbus.Client
	if !*dryRun {
		natsURL := envOr("NATS_URL", "nats://localhost:4222")
		c, err := natsbus.Connect(natsbus.Options{URL: natsURL, Name: "kanban-dispatcher"})
		if err != nil {
			log.Fatalf("nats connect: %v", err)
		}
		defer c.Close()
		natsClient = c
	}

	parents := scopedParents(view, kc)
	log.Printf("[dispatcher] board=%q scoped-parents=%d", *boardName, len(parents))

	dispatched := 0
	for _, p := range parents {
		if dispatched >= *maxDispatch {
			break
		}
		groups := backlogGroups(view, p.cardID)
		if len(groups) == 0 {
			continue
		}
		for _, g := range groups {
			if dispatched >= *maxDispatch {
				break
			}
			child := g.cards[0] // first in group
			busSessionID, reused := decideReuse(g, view, mapping, *bridgeURL)
			prompt := buildPrompt(p, child, g.key, reused)
			displayName := fmt.Sprintf("tcl %s · %s", g.key, time.Now().Format("Jan 2 15:04"))

			if reused == "" {
				busSessionID = mintBusSessionID(p.cardID, g.key)
			}

			log.Printf("[%s] dispatching %q group=%s reuse=%v bus=%s",
				p.title(), childTitle(child), g.key, reused != "", busSessionID)
			if *dryRun {
				dispatched++
				continue
			}

			in := buildBridgeInbound(busSessionID, displayName, prompt, reused == "")
			if err := natsClient.Publish(messages.BridgeInboundSubject, in); err != nil {
				log.Printf("[%s] nats publish: %v", p.title(), err)
				continue
			}
			if err := kc.AddCardLink(child.Placement.CardID, linkTypeBusSess, busSessionID, "tcl-dispatcher"); err != nil {
				log.Printf("[%s] add bus_session link: %v", p.title(), err)
				continue
			}
			if err := kc.MoveCard(child.Placement.CardID, board.ID, colByName[columnInProgress], 0); err != nil {
				log.Printf("[%s] move to In Progress: %v", p.title(), err)
				continue
			}
			dispatched++
		}
	}

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
				continue
			}
			placement, err := currentPlacement(e.CardID, board.ID)
			if err != nil || placement == "" || placement == targetCol {
				continue
			}
			log.Printf("[%s] closure: child %s status=%s → %s", p.title(), e.CardID[:8], status, targetName)
			if *dryRun {
				continue
			}
			if err := kc.MoveCard(e.CardID, board.ID, targetCol, 0); err != nil {
				log.Printf("[%s] move child to %s: %v", p.title(), targetName, err)
				continue
			}
			closed++
		}
	}

	// Parent rollup: any scoped parent whose children are all settled → move to Done.
	rolled := 0
	for _, p := range parents {
		settled, total, summary := childrenSettlement(kc, p.cardID)
		if total == 0 || settled < total {
			continue
		}
		log.Printf("[%s] rollup: %d/%d children settled", p.title(), settled, total)
		if *dryRun {
			continue
		}
		body, _ := p.item["body"].(string)
		suffix := fmt.Sprintf("\n\n---\n_Rolled up by kanban-dispatcher at %s — %s_\n",
			time.Now().UTC().Format(time.RFC3339), summary)
		if err := kc.PatchCardBody(p.cardID, body+suffix); err != nil {
			log.Printf("[%s] patch parent body: %v (continuing to move)", p.title(), err)
		}
		if err := kc.MoveCard(p.cardID, board.ID, colByName[columnDone], 0); err != nil {
			log.Printf("[%s] move parent to Done: %v", p.title(), err)
			continue
		}
		rolled++
	}

	log.Printf("[dispatcher] done: dispatched=%d closed=%d rolled-up=%d", dispatched, closed, rolled)
}

// currentPlacement returns the column id this card is on for the given board.
// Empty string means the card is not placed on this board (orphaned).
func currentPlacement(cardID, boardID string) (string, error) {
	url := fmt.Sprintf("%s/api/cards/%s/placements",
		strings.TrimRight(envOr("KANBAN_STORE_URL", "http://localhost:8305"), "/"), cardID)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %s — %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var placements []struct {
		BoardID  string `json:"board_id"`
		ColumnID string `json:"column_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&placements); err != nil {
		return "", err
	}
	for _, p := range placements {
		if p.BoardID == boardID {
			return p.ColumnID, nil
		}
	}
	return "", nil
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
func scopedParents(view *kanbanclient.BoardView, kc *kanbanclient.Client) []scopedParent {
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
		etags, err := kc.ListEntityTags("note", c.Placement.CardID)
		if err != nil {
			log.Printf("[%s] list entity tags: %v — skip", c.Placement.CardID, err)
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
func backlogGroups(view *kanbanclient.BoardView, parentID string) []backlogGroup {
	var backlogCards []kanbanclient.CardView
	for _, cv := range view.Columns {
		if cv.Column.Name == columnBacklog {
			backlogCards = cv.Cards
			break
		}
	}
	byKey := map[string][]kanbanclient.CardView{}
	for _, c := range backlogCards {
		if c.Item == nil {
			continue
		}
		// Cheap parent check: pull links inline. Avoids a per-card RTT for
		// non-children. But we only have placement+item here — no links list.
		// Caller responsible for filtering via parent lookup separately.
		_ = c
	}
	// We don't have links in the BoardView, so do the inverse: list cards
	// linked to this parent_card entity, then intersect with Backlog
	// placements in the view.
	parentChildren := map[string]bool{}
	if entries, err := listCardsForEntity(view.Board.ID, parentID); err == nil {
		for _, e := range entries {
			parentChildren[e] = true
		}
	} else {
		log.Printf("[%s] list children: %v", parentID, err)
		return nil
	}
	for _, c := range backlogCards {
		if !parentChildren[c.Placement.CardID] {
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

// listCardsForEntity is a thin wrapper around the kanbanclient helper plus a
// projection to card_id strings, since callers here don't need the whole item.
func listCardsForEntity(boardID, parentID string) ([]string, error) {
	url := fmt.Sprintf("%s/api/entities/%s/%s/cards",
		strings.TrimRight(envOr("KANBAN_STORE_URL", "http://localhost:8305"), "/"),
		linkTypeParent, parentID)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %s — %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var entries []struct {
		CardID string `json:"card_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
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

// decideReuse looks at sibling sub-cards under the same parent (any column,
// not just Backlog) that share this group's key and have a bus_session link.
// If any such session is currently idle and updated within the revive window,
// returns its bus_session_id; else returns "".
func decideReuse(g backlogGroup, view *kanbanclient.BoardView, mapping map[string]string, bridgeURL string) (busSessionID string, reused string) {
	siblings := groupSiblings(view, g.parentID, g.key)
	now := time.Now()
	for _, sibling := range siblings {
		// Skip the candidate itself (still in Backlog with no link yet).
		if sibling.Placement.CardID == g.cards[0].Placement.CardID {
			continue
		}
		links, err := fetchLinks(sibling.Placement.CardID)
		if err != nil {
			continue
		}
		var busID string
		for _, l := range links {
			if l.EntityType == linkTypeBusSess {
				busID = l.EntityRef
				break
			}
		}
		if busID == "" {
			continue
		}
		bridgeID, ok := mapping[busID]
		if !ok {
			continue
		}
		state, updatedAt, err := getSessionState(bridgeURL, bridgeID)
		if err != nil {
			continue
		}
		if state != "idle" {
			continue
		}
		if updatedAt != "" {
			if t, err := time.Parse(time.RFC3339, updatedAt); err == nil {
				if now.Sub(t) > reviveWindow {
					continue
				}
			}
		}
		return busID, busID
	}
	return "", ""
}

// groupSiblings returns ALL cards on the board that share the parent_card
// link and the same group:<key> tag — across every column, so we can find a
// freshly-completed sibling whose session is still idle.
func groupSiblings(view *kanbanclient.BoardView, parentID, key string) []kanbanclient.CardView {
	parentChildren := map[string]bool{}
	if cardIDs, err := listCardsForEntity(view.Board.ID, parentID); err == nil {
		for _, id := range cardIDs {
			parentChildren[id] = true
		}
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

func fetchLinks(cardID string) ([]kanbanclient.CardLink, error) {
	url := fmt.Sprintf("%s/api/cards/%s/links",
		strings.TrimRight(envOr("KANBAN_STORE_URL", "http://localhost:8305"), "/"), cardID)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %s — %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var links []kanbanclient.CardLink
	if err := json.NewDecoder(resp.Body).Decode(&links); err != nil {
		return nil, err
	}
	return links, nil
}

// ============================ Bridge inbound ============================

func mintBusSessionID(parentID, groupKey string) string {
	now := time.Now().UTC().UnixNano()
	safeGroup := strings.ReplaceAll(groupKey, " ", "-")
	if safeGroup == "" {
		safeGroup = "default"
	}
	return fmt.Sprintf("tcl-%s-%s-%d", shortID(parentID), safeGroup, now)
}

// shortID returns the leading characters of an id; useful for human-readable
// session keys without collision risk at this scale.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func buildBridgeInbound(busSessionID, displayName, prompt string, fresh bool) messages.BridgeInbound {
	in := messages.BridgeInbound{
		BusSessionID: busSessionID,
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
			Harness:     defaultHarness,
			InstanceID:  defaultInstanceID,
			DisplayName: displayName,
			ClientID:    "kanban-dispatcher",
			Source:      "kanban-dispatcher",
			SessionType: msg.SessionTypeAutonomous,
			AutoStart:   true,
		}
	}
	return in
}

func buildPrompt(p scopedParent, child kanbanclient.CardView, group string, reusedBusSessionID string) string {
	childID := child.Placement.CardID
	title := childTitle(child)
	body, _ := child.Item["body"].(string)
	if body == "" {
		body = "(no body — see title)"
	}
	header := "You are picking up a sub-task in the **task-completion-loop** workflow."
	if reusedBusSessionID != "" {
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
func childrenSettlement(kc *kanbanclient.Client, parentID string) (int, int, string) {
	entries, err := kc.ListCardsForEntity(linkTypeParent, parentID)
	if err != nil {
		return 0, 0, fmt.Sprintf("rollup error: %v", err)
	}
	if len(entries) == 0 {
		return 0, 0, "no children"
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
	return settled, total, summary
}

// ============================ HTTP / NATS helpers ============================

func defaultAdapterSessionsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".llm-bridge-adapter", "sessions.json")
}

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

// ============================ Scheduler self-registration ============================

func registerSchedulerJob() error {
	schedURL := envOr("SCHEDULER_URL", "http://localhost:8092")
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, "bin", "kanban-dispatcher")

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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

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
