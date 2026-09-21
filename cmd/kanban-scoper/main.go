// kanban-scoper decomposes a top-level "task-completion-loop" card into
// actionable sub-cards using Anthropic Haiku.
//
// Cadence:
//
//	Registered as a scheduler shell job running every ~5 minutes. Each tick:
//
//	  1. Walk the "Task completion loop" board.
//	  2. Pick parent cards in the "Goals" column tagged with the trigger tag
//	     ("task-completion-loop") that don't already carry the "scoped"
//	     entity_tag.
//	  3. Call Haiku to decompose the goal into sub-tasks grouped by context.
//	  4. For each sub-task, create a card in the "Backlog" column linked
//	     back to the parent (entity_type=parent_card) and tagged with the
//	     subtask + group:<key> markers, plus capability:<x> + role:<r>
//	     markers ONLY when --role-tags is enabled (off by default).
//	     (capability/role drive team-formation per TEAM-ORCHESTRATION.md §4/§15.)
//	  5. Stamp the parent with the "scoped" entity_tag so we don't re-decompose.
//
// Why entity_tag (and not a noteboard tag) for "scoped": kanban-store's
// entity_tags table is idempotent on (entity_type, entity_ref, tag), so we
// can add it without a PATCH-then-merge dance against noteboard.
//
// Sibling components:
//   - kanban-dispatcher : picks sub-cards from Backlog, spawns/reuses sessions
//   - kanban-curator    : moves In Progress → Done/Failed on session terminal state
package main

import (
	"bytes"
	"context"
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
	"github.com/slava/scheduler/internal/oneshot"
)

const (
	columnGoals      = "Goals"
	columnBacklog    = "Backlog"
	columnInProgress = "In Progress"
	columnDone       = "Done"
	columnFailed     = "Failed"
	tagTrigger       = "task-completion-loop"
	tagSubtask       = "task-completion-loop-subtask"
	tagScoped        = "scoped"
	linkTypeParent   = "parent_card"
	maxScopePerTick  = 5
)

var defaultColumns = []string{columnGoals, columnBacklog, columnInProgress, columnDone, columnFailed}

type config struct {
	KanbanURL       string
	BridgeURL       string
	OneshotInstance string
	Model           string
	BoardID         string
	DryRun          bool
	RoleTags        bool // TEAM-ORCHESTRATION step 1: emit capability:/role: tags. OFF by default.
}

func main() {
	settings, err := schedulerconfig.KanbanScoper.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatal(err)
	}
	var (
		registerFlag    = flag.Bool("register", false, "Register/upsert this binary as a scheduler cron job and exit.")
		dryRun          = flag.Bool("dry-run", false, "Decompose but do not write to kanban-store.")
		boardID         = flag.String("board-id", "", "Kanban board id to scope (required). Pinned by id so a rename in the UI cannot fork a second board.")
		kanbanURL       = flag.String("kanban-url", settings.String(schedulerconfig.SettingKanbanStoreURL), "kanban-store base URL")
		bridgeURL       = flag.String("bridge-url", settings.String(schedulerconfig.SettingBridgeURL), "llm-bridge-server base URL")
		oneshotInstance = flag.String("oneshot-instance", settings.String(schedulerconfig.SettingOneshotInstanceID), "claude_code harness instance id for subscription oneshot calls (required to decompose)")
		model           = flag.String("model", settings.String(schedulerconfig.SettingModel), "Anthropic model id")
		maxScope        = flag.Int("max", maxScopePerTick, "Max parents to decompose per tick.")
		roleTags        = flag.Bool("role-tags", settings.Boolean(schedulerconfig.SettingRoleTags), "TEAM-ORCHESTRATION step 1: require capability+role in decomposition and emit capability:/role: tags on sub-cards. OFF by default — when off, behavior is identical to the pre-team-orchestration scoper.")
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

	if *registerFlag {
		if err := registerSchedulerJob(settings.String(schedulerconfig.SettingSchedulerURL), *boardID, *oneshotInstance); err != nil {
			log.Fatalf("register: %v", err)
		}
		return
	}

	ctx := context.Background()
	cfg := config{
		KanbanURL:       *kanbanURL,
		BridgeURL:       *bridgeURL,
		OneshotInstance: *oneshotInstance,
		Model:           *model,
		BoardID:         *boardID,
		DryRun:          *dryRun,
		RoleTags:        *roleTags,
	}

	kc := kanbanclient.New(cfg.KanbanURL, settings.String(schedulerconfig.SettingKanbanStoreServiceToken))
	board, err := kc.EnsureBoardColumns(cfg.BoardID, defaultColumns)
	if err != nil {
		log.Fatalf("board %s: %v", cfg.BoardID, err)
	}

	view, err := kc.GetBoardView(kc.BoardID)
	if err != nil {
		log.Fatalf("board view: %v", err)
	}

	parents := pickParents(view, kc)
	log.Printf("[scoper] board=%q id=%s parents-needing-scope=%d", board.Name, board.ID, len(parents))
	if len(parents) == 0 {
		return
	}

	if cfg.OneshotInstance == "" {
		log.Fatal("-oneshot-instance is required: decomposition routes through llm-bridge-server's oneshot endpoint on the subscription login")
	}

	scoped := 0
	for _, p := range parents {
		if scoped >= *maxScope {
			log.Printf("[scoper] hit per-tick cap (%d); deferring %d parents to next tick",
				*maxScope, len(parents)-scoped)
			break
		}
		if err := scopeParent(ctx, cfg, kc, p); err != nil {
			log.Printf("[%s] scope failed: %v", p.title(), err)
			continue
		}
		scoped++
	}
	log.Printf("[scoper] done: scoped=%d", scoped)
}

// ============================ Parent selection ============================

type parentCard struct {
	cardID  string
	item    map[string]any
	boardID string
	tags    []string
}

func (p parentCard) title() string {
	t, _ := p.item["title"].(string)
	if t == "" {
		t = p.cardID
	}
	return t
}

func (p parentCard) body() string {
	b, _ := p.item["body"].(string)
	return b
}

// pickParents returns Goals-column cards tagged with the trigger tag that
// don't yet carry the "scoped" entity_tag.
func pickParents(view *kanbanclient.BoardView, kc *kanbanclient.Client) []parentCard {
	var goalsCards []kanbanclient.CardView
	for _, cv := range view.Columns {
		if cv.Column.Name == columnGoals {
			goalsCards = cv.Cards
			break
		}
	}
	var out []parentCard
	for _, c := range goalsCards {
		if c.Item == nil {
			continue
		}
		tags := stringSlice(c.Item["tags"])
		if !contains(tags, tagTrigger) {
			continue
		}
		// Check entity_tags for "scoped".
		etags, err := kc.ListEntityTags("note", c.Placement.CardID)
		if err != nil {
			log.Printf("[%s] list entity tags: %v — skip", c.Placement.CardID, err)
			continue
		}
		if anyTagEquals(etags, tagScoped) {
			continue
		}
		out = append(out, parentCard{
			cardID:  c.Placement.CardID,
			item:    c.Item,
			boardID: view.Board.ID,
			tags:    tags,
		})
	}
	return out
}

// ============================ Decomposition ============================

func scopeParent(ctx context.Context, cfg config, kc *kanbanclient.Client, p parentCard) error {
	digest := buildDigest(p)
	plan, err := decompose(ctx, cfg, digest)
	if err != nil {
		return fmt.Errorf("decompose: %w", err)
	}
	if len(plan.Subtasks) == 0 {
		return fmt.Errorf("decomposer returned 0 sub-tasks")
	}
	log.Printf("[%s] %d sub-tasks: %s", p.title(), len(plan.Subtasks), summarizePlan(plan))

	if cfg.DryRun {
		return nil
	}

	backlogCol := kc.ColumnsByName[columnBacklog]
	if backlogCol == "" {
		return fmt.Errorf("board missing %q column", columnBacklog)
	}

	for i, st := range plan.Subtasks {
		title := strings.TrimSpace(st.Title)
		if title == "" {
			log.Printf("[%s] subtask %d has empty title — skip", p.title(), i)
			continue
		}
		body := st.Body
		if body == "" {
			body = "(no body)"
		}
		body = fmt.Sprintf("%s\n\n---\n_Sub-task of [%s](kanban://%s) — group: `%s`_\n",
			body, p.title(), p.cardID, st.GroupKey)
		tags := []string{tagSubtask}
		if g := strings.TrimSpace(st.GroupKey); g != "" {
			tags = append(tags, "group:"+g)
		}
		if cfg.RoleTags {
			if c := strings.TrimSpace(st.Capability); c != "" {
				tags = append(tags, "capability:"+c)
			}
			if r := strings.TrimSpace(st.Role); r != "" {
				tags = append(tags, "role:"+r)
			}
		}
		// Persist parent linkage as a card_link, not a tag, so we can do
		// reverse lookups via /api/entities/parent_card/<id>/cards.
		childID, err := createSubcard(kc, backlogCol, title, body, tags, p.cardID)
		if err != nil {
			return fmt.Errorf("create subcard %q: %w", title, err)
		}
		log.Printf("[%s] subcard %s → %q", p.title(), childID, title)
	}

	if err := kc.AddEntityTag("note", p.cardID, tagScoped); err != nil {
		return fmt.Errorf("mark scoped: %w", err)
	}
	return nil
}

// createSubcard creates a card in the given column and attaches a parent_card
// link in one go. We bypass kanbanclient.CreateCard because that helper
// hard-codes autoworker-specific links.
func createSubcard(kc *kanbanclient.Client, columnID, title, body string, tags []string, parentID string) (string, error) {
	payload := map[string]any{
		"title":     title,
		"body":      body,
		"tags":      tags,
		"column_id": columnID,
		// parent_id, not just the parent_card link below.
		//
		// These are two different edges and the gate rides on this one: noteboard
		// inherits a hold down parent_id (a held parent withholds its children) and
		// the spend ceiling rolls up over the same edge. A sub-card that carried
		// ONLY the parent_card link would escape both — it would be a fresh,
		// unheld, unmetered todo whose parent thinks it is capped. The parent_card
		// link stays because kanban-dispatcher reads it for closure roll-up.
		"parent_id": parentID,
	}
	bs, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/boards/%s/cards", kc.BaseURL, kc.BoardID)
	resp, err := kc.HTTP.Post(url, "application/json", bytes.NewReader(bs))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create card: %s — %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var created struct {
		Item map[string]any `json:"item"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", err
	}
	id, _ := created.Item["id"].(string)
	if id == "" {
		return "", fmt.Errorf("create card: empty item id in response")
	}
	if err := kc.AddCardLink(id, linkTypeParent, parentID, "scoped"); err != nil {
		return id, fmt.Errorf("link parent: %w", err)
	}
	return id, nil
}

func buildDigest(p parentCard) string {
	var b strings.Builder
	b.WriteString("# Goal\n")
	b.WriteString(p.title())
	b.WriteString("\n\n")
	if body := p.body(); body != "" {
		b.WriteString("## Body\n")
		b.WriteString(body)
		b.WriteString("\n\n")
	}
	if len(p.tags) > 0 {
		b.WriteString("## Tags\n")
		b.WriteString(strings.Join(p.tags, ", "))
		b.WriteString("\n")
	}
	return b.String()
}

func summarizePlan(plan *Plan) string {
	titles := make([]string, 0, len(plan.Subtasks))
	for _, st := range plan.Subtasks {
		titles = append(titles, fmt.Sprintf("%s[%s]", st.Title, st.GroupKey))
	}
	return strings.Join(titles, " | ")
}

// ============================ Anthropic call ============================

type Plan struct {
	Subtasks []Subtask `json:"subtasks"`
}

type Subtask struct {
	Title      string `json:"title"`
	Body       string `json:"body,omitempty"`
	GroupKey   string `json:"group_key"`
	Capability string `json:"capability"`
	Role       string `json:"role"`
}

const systemPrompt = `You are a task decomposer for an autonomous AI agent infrastructure.

You receive a high-level goal card. Produce a small set of self-contained sub-tasks (typically 3–8, max 12) such that:

- Each sub-task can be picked up by ONE agent session and finished without further decomposition.
- Sub-tasks that share an obvious context (same repo / same code surface / same dataset / same service) MUST share a "group_key" so the dispatcher will let one session handle them in sequence rather than spinning up parallel sessions for each.
- Sub-tasks with materially different context get distinct group_keys.
- Title: short imperative, ≤80 chars (e.g. "Add migration for schema X", "Audit log-store retention").
- Body: one or two paragraphs of concrete instructions — what to read, what to change, what success looks like.
- group_key: short kebab-case identifier (e.g. "kanban-store-schema", "auth-store-tests"). Reuse the same key for sibling sub-tasks that should run together.

You MUST output a single JSON object conforming to the provided schema, exactly once.

If the goal is too vague or trivial to decompose, return a single sub-task that asks the user to clarify, with group_key="clarification".`

// roleTagsInstructions is appended to systemPrompt only when --role-tags is on.
const roleTagsInstructions = `Additionally, for EACH sub-task provide:

- capability: short kebab-case identifier for the domain/skill the sub-task needs (e.g. "go-http", "sqlite-migration", "react-ui", "shell-ops"). Used to equip the working agent with the right tools.
- role: which kind of agent should work this sub-task — exactly one of: "builder" (writes code; the default for implementation), "reviewer" (code review only), "tester" (writes/runs tests), "researcher" (investigation/discovery, no writes), "docs" (documentation). Most implementation sub-tasks are "builder".`

// decomposeSchema is the JSON Schema the model's reply must conform to. It
// was the input_schema of a forced tool call when this command spoke to
// api.anthropic.com directly; the oneshot bridge path takes the bare schema.
func decomposeSchema(roleTags bool) map[string]any {
	props := map[string]any{
		"title":     map[string]any{"type": "string", "maxLength": 100},
		"body":      map[string]any{"type": "string"},
		"group_key": map[string]any{"type": "string", "maxLength": 60},
	}
	required := []string{"title", "group_key"}
	if roleTags {
		props["capability"] = map[string]any{"type": "string", "maxLength": 60, "description": "kebab-case domain/skill this sub-task needs (e.g. go-http, sqlite-migration, react-ui)."}
		props["role"] = map[string]any{"type": "string", "enum": []string{"builder", "reviewer", "tester", "researcher", "docs"}, "description": "Which archetype works this sub-task; builder is the default for code."}
		required = append(required, "capability", "role")
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"subtasks": map[string]any{
				"type":        "array",
				"description": "Ordered sub-tasks. Group siblings via shared group_key.",
				"items": map[string]any{
					"type":       "object",
					"properties": props,
					"required":   required,
				},
			},
		},
		"required": []string{"subtasks"},
	}
}

// decompose runs one schema-forced call through llm-bridge-server's oneshot
// endpoint on the Claude Code subscription login. Replaced a direct POST to
// api.anthropic.com that billed an API key resolved from auth-store.
func decompose(ctx context.Context, cfg config, digest string) (*Plan, error) {
	sys := systemPrompt
	if cfg.RoleTags {
		sys += "\n\n" + roleTagsInstructions
	}
	var p Plan
	if err := oneshot.Call(ctx, oneshot.Config{
		BridgeURL:  cfg.BridgeURL,
		InstanceID: cfg.OneshotInstance,
		Model:      cfg.Model,
		MaxTokens:  4096,
		Timeout:    120 * time.Second,
	}, sys, digest, decomposeSchema(cfg.RoleTags), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ============================ Scheduler self-registration ============================

// registerSchedulerJob creates the tick job with the board id — and the
// oneshot instance, when given — baked into the command: the scheduler runs
// jobs as `sh -c` with its own thin environment, so neither would reach the
// process any other way. The oneshot instance is the scoper's OWN model
// transport, not a board setting, which is why it stays a flag.
func registerSchedulerJob(schedURL, boardID, oneshotInstance string) error {
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, "bin", "kanban-scoper") + " -board-id " + boardID
	if oneshotInstance != "" {
		bin += " -oneshot-instance " + oneshotInstance
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
		if j.Name == "kanban-scoper-tick" {
			fmt.Println("kanban-scoper-tick already registered; no changes made")
			return nil
		}
	}
	payload := map[string]any{
		"name":        "kanban-scoper-tick",
		"description": "Decomposes task-completion-loop parent cards into sub-cards via Haiku.",
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
	fmt.Printf("registered kanban-scoper-tick: %s\n", strings.TrimSpace(string(body)))
	return nil
}

// ============================ helpers ============================

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

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func anyTagEquals(tags []kanbanclient.EntityTag, want string) bool {
	for _, t := range tags {
		if t.Tag == want {
			return true
		}
	}
	return false
}
