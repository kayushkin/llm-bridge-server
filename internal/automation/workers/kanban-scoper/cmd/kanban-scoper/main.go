// kanban-scoper decomposes a top-level "task-completion-loop" card into
// actionable sub-cards with one model call per goal.
//
// Cadence:
//
//	Registered as a scheduler shell job running every ~5 minutes. Each tick:
//
//	  1. Walk the "Task completion loop" board.
//	  2. Pick parent cards in the "Goals" column tagged with the trigger tag
//	     ("task-completion-loop") that don't already carry the "scoped"
//	     entity_tag.
//	  3. Call the model to decompose the goal into sub-tasks grouped by context.
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
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	schedulerconfig "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/config"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/boardvocabulary"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/bridgeauth"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/bridgestores"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/degradations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/oneshot"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/schedulerjobs"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/schedulersettings"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/workersettings"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

const (
	columnGoals      = boardvocabulary.ColumnGoals
	columnBacklog    = boardvocabulary.ColumnBacklog
	columnInProgress = boardvocabulary.ColumnInProgress
	columnDone       = boardvocabulary.ColumnDone
	columnFailed     = boardvocabulary.ColumnFailed
	tagTrigger       = boardvocabulary.TagTrigger
	tagSubtask       = boardvocabulary.TagSubtask
	tagScoped        = "scoped"
	// tagGroupPrefix must stay identical to kanban-dispatcher's constant of the
	// same name — this is the wire format between the two binaries. It is spelled
	// out in both rather than shared, along with every other constant in this
	// block; see the residual filed alongside this change.
	tagGroupPrefix  = boardvocabulary.TagGroupPrefix
	linkTypeParent  = boardvocabulary.LinkTypeParentCard
	maxScopePerTick = 5
)

var defaultColumns = []string{columnGoals, columnBacklog, columnInProgress, columnDone, columnFailed}

type config struct {
	// BridgeURL is llm-bridge-server's address. The board is under its
	// /issues, the model role under its /scheduler, and decomposition calls
	// its /oneshot.
	BridgeURL string
	// ModelRole is the model-store role decomposition asks for. run reads it
	// from the scheduler's stored settings once a tick has a parent to scope;
	// the operator changes it on the scheduler's settings page.
	ModelRole string
	BoardID   string
	MaxScope  int
	DryRun    bool
	RoleTags  bool // TEAM-ORCHESTRATION step 1: emit capability:/role: tags. OFF by default.
}

func main() {
	settings, err := workersettings.KanbanScoper.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatal(err)
	}
	var (
		registerFlag = flag.Bool("register", false, "Register/upsert this binary as a scheduler cron job and exit.")
		dryRun       = flag.Bool("dry-run", false, "Decompose but do not write to the board.")
		boardID      = flag.String("board-id", "", "Kanban board id to scope (required). Pinned by id so a rename in the UI cannot fork a second board.")
		bridgeURL    = flag.String("bridge-url", settings.String(workersettings.SettingBridgeURL), "llm-bridge-server base URL; the board, the scheduler and the model calls are all under it")
		maxScope     = flag.Int("max", maxScopePerTick, "Max parents to decompose per tick.")
		roleTags     = flag.Bool("role-tags", settings.Boolean(workersettings.SettingRoleTags), "TEAM-ORCHESTRATION step 1: require capability+role in decomposition and emit capability:/role: tags on sub-cards. OFF by default — when off, behavior is identical to the pre-team-orchestration scoper.")
	)
	flag.Parse()

	// llm-bridge-server gates every route; stamp this process's calls to it
	// with its service principal token. Every store this binary calls is
	// under the bridge, so every call carries it.
	if err := bridgeauth.StampRequestsToBridge(*bridgeURL, settings.String(workersettings.SettingBridgeServicePrincipalToken)); err != nil {
		log.Fatal(err)
	}

	if *boardID == "" {
		log.Fatal("-board-id is required; there is no board to default to")
	}

	if *registerFlag {
		if err := registerSchedulerJob(bridgestores.SchedulerURL(*bridgeURL), *boardID); err != nil {
			log.Fatalf("register: %v", err)
		}
		return
	}

	os.Exit(run(config{
		BridgeURL: *bridgeURL,
		BoardID:   *boardID,
		MaxScope:  *maxScope,
		DryRun:    *dryRun,
		RoleTags:  *roleTags,
	}))
}

// run is one tick, and its return value is the process exit status.
//
// ==================== a tick that scoped nothing it meant to ====================
//
// Everything that can stop the whole tick is still fatal: with no board, no board
// view and no model role there is no tick to have, and exiting 1 already
// says so. What this adds is the ways the scoper used to fall short of its job
// PER PARENT or PER SUB-TASK and still exit 0, because the answer to every one of
// them is `continue`.
//
// Two are on the read path: a Goals card whose `scoped` tag could not be read, so
// it was never considered, and a decomposition the model would not produce. The
// rest are on the WRITE path, and three of them cost money rather than only
// accuracy:
//
//   - A sub-card the board refused. The parent is then left unstamped, so the next
//     tick decomposes it again and re-creates every sub-card that DID land — a
//     duplicate set every five minutes, each one paying for a model call.
//   - The `scoped` stamp itself refused. Same loop, and this one fires after the
//     whole plan has been written, so the duplicate set is complete.
//   - A parent link the board refused. The sub-card exists on the board with no
//     parent_card edge, so kanban-dispatcher's closure roll-up will never find it
//     and it cannot be rolled up by anything.
//
// The quietest: a sub-task the decomposer returned with an empty title is
// dropped, and the parent is stamped `scoped` anyway — so that slice of the goal
// is lost permanently, on a card nothing will ever revisit.
//
// The other direction matters as much and is tested as hard. An empty Goals
// column, a card without the trigger tag, a card already stamped `scoped`, a
// deferral at the per-tick cap and a dry run are the scoper having nothing to do
// or being told not to write, not falling short. Job 15 logs
// `parents-needing-scope=0` on 100 of 100 retained ticks, and a job that goes red
// every tick carries as little as the green it replaces.
func run(cfg config) int {
	degraded := degradations.New("scoper")
	ctx := context.Background()

	kc := issuesclient.New(bridgestores.IssuesURL(cfg.BridgeURL))
	board, err := kc.EnsureBoardColumns(cfg.BoardID, defaultColumns)
	if err != nil {
		log.Fatalf("board %s: %v", cfg.BoardID, err)
	}

	view, err := kc.GetBoardView(kc.BoardID)
	if err != nil {
		log.Fatalf("board view: %v", err)
	}

	parents := pickParents(view, kc, degraded)
	log.Printf("[scoper] board=%q id=%s parents-needing-scope=%d", board.Name, board.ID, len(parents))
	if len(parents) == 0 {
		return degraded.ExitCode()
	}

	if cfg.ModelRole, err = schedulersettings.ReadModelRole(ctx, bridgestores.SchedulerURL(cfg.BridgeURL), schedulerconfig.SettingKanbanScoperModelRole); err != nil {
		log.Fatal(err)
	}

	scoped := 0
	for _, p := range parents {
		if scoped >= cfg.MaxScope {
			// Deliberate, and not a shortfall: the deferred parents keep their
			// unstamped state and the next tick five minutes later takes them.
			log.Printf("[scoper] hit per-tick cap (%d); deferring %d parents to next tick",
				cfg.MaxScope, len(parents)-scoped)
			break
		}
		// scopeParent records every shortfall itself, at the site it happens,
		// with the consequence attached; the error here is control flow only.
		if err := scopeParent(ctx, cfg, kc, p, degraded); err != nil {
			log.Printf("[%s] scope failed: %v", p.title(), err)
			continue
		}
		scoped++
	}
	log.Printf("[scoper] done: scoped=%d", scoped)
	return degraded.ExitCode()
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
func pickParents(view *issuesclient.BoardView, kc *issuesclient.Client, degraded *degradations.Recorder) []parentCard {
	var goalsCards []issuesclient.CardView
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
		etags, err := kc.ListEntityTags(boardvocabulary.EntityTypeNote, c.Placement.CardID)
		if err != nil {
			log.Printf("[%s] list entity tags: %v — skip", c.Placement.CardID, err)
			degraded.Record("goal card %s (%s): its scoped tag could not be read (%v), so it was never considered for decomposition",
				c.Placement.CardID, cardTitle(c.Item), err)
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

// scopeParent decomposes one parent and writes the result. It records each way
// it can fall short at the site where that happens, with the consequence
// attached, and returns an error only as control flow for the caller's loop —
// the caller must not record again.
func scopeParent(ctx context.Context, cfg config, kc *issuesclient.Client, p parentCard, degraded *degradations.Recorder) error {
	digest := buildDigest(p)
	plan, err := decompose(ctx, cfg, digest)
	if err != nil {
		degraded.Record("goal card %s (%s): the decomposer failed (%v), so it was not broken down and stays unscoped",
			p.cardID, p.title(), err)
		return fmt.Errorf("decompose: %w", err)
	}
	if len(plan.Subtasks) == 0 {
		degraded.Record("goal card %s (%s): the decomposer returned 0 sub-tasks, so nothing was written and it stays unscoped",
			p.cardID, p.title())
		return fmt.Errorf("decomposer returned 0 sub-tasks")
	}
	log.Printf("[%s] %d sub-tasks: %s", p.title(), len(plan.Subtasks), summarizePlan(plan))

	if cfg.DryRun {
		return nil
	}

	backlogCol := kc.ColumnsByName[columnBacklog]
	if backlogCol == "" {
		degraded.Record("goal card %s (%s): the board has no %q column, so its %d sub-tasks had nowhere to go",
			p.cardID, p.title(), columnBacklog, len(plan.Subtasks))
		return fmt.Errorf("board missing %q column", columnBacklog)
	}

	for i, st := range plan.Subtasks {
		draft := buildSubcardDraft(p, st, cfg.RoleTags)
		if draft.Title == "" {
			// The parent is stamped `scoped` at the foot of this function
			// whether or not this fired, so a dropped sub-task is not deferred
			// to the next tick — it is gone, on a card nothing will revisit.
			log.Printf("[%s] subtask %d has empty title — skip", p.title(), i)
			degraded.Record("goal card %s (%s): sub-task %d came back with an empty title and was dropped, and the parent is stamped scoped regardless — that slice of the goal is lost",
				p.cardID, p.title(), i)
			continue
		}
		// The decomposer's output can lose information without being invalid.
		// Say so per sub-task rather than dropping it in silence.
		for _, note := range draft.Notes {
			log.Printf("[%s] subtask %d %q: %s", p.title(), i, draft.Title, note)
		}
		// Persist parent linkage as a card_link, not a tag, so we can do
		// reverse lookups via /issues/entities/parent_card/<id>/cards.
		childID, err := createSubcard(kc, backlogCol, draft.Title, draft.Body, draft.Tags, p.cardID)
		if err != nil {
			if errors.Is(err, errParentLinkRefused) {
				// The card exists; only its parent_card edge is missing. Keep
				// going: abandoning the plan here would leave the parent
				// unstamped and re-create every sibling on the next tick.
				degraded.Record("goal card %s (%s): sub-card %s (%q) was created with no parent_card link (%v), so kanban-dispatcher's closure roll-up will never find it",
					p.cardID, p.title(), childID, draft.Title, err)
				continue
			}
			degraded.Record("goal card %s (%s): sub-card %q was refused by the board (%v) — the parent is left unstamped, so the next tick decomposes it again and re-creates every sub-card that did land",
				p.cardID, p.title(), draft.Title, err)
			return fmt.Errorf("create subcard %q: %w", draft.Title, err)
		}
		log.Printf("[%s] subcard %s → %q", p.title(), childID, draft.Title)
	}

	if err := kc.AddEntityTag(boardvocabulary.EntityTypeNote, p.cardID, tagScoped); err != nil {
		degraded.Record("goal card %s (%s): its %d sub-cards were written but the scoped stamp was refused (%v), so every later tick decomposes it again and writes a duplicate set",
			p.cardID, p.title(), len(plan.Subtasks), err)
		return fmt.Errorf("mark scoped: %w", err)
	}
	return nil
}

// groupKeyUnsetLabel is what a blank group_key renders as in log lines and in
// the sub-task body. It is a LABEL and never an identity: when the key is blank
// no tagGroupPrefix tag is emitted at all, so nothing keys, groups, compares or
// routes on this string, and it must never be read back to find a card.
//
// It is deliberately NOT kebab-case and deliberately NOT backticked. The prompt
// asks the decomposer for kebab-case keys and displayGroupKey wraps a real key
// in backticks, so a reader can always tell a key the decomposer returned from
// this marker standing in for one it did not. Rendering the raw key here was
// the defect: a blank one printed as an empty pair of backticks and read as a
// group whose name was the empty string.
//
// kanban-dispatcher names its own substitution the same way on the consumer
// side; the two are independent and must not be compared to each other.
const groupKeyUnsetLabel = "(unset)"

// displayGroupKey renders an already-trimmed group_key for humans. The result
// MUST NOT be compared, keyed or grouped on — use the trimmed key itself for
// that, and treat "" as "this sub-task groups with nobody".
func displayGroupKey(trimmed string) string {
	if trimmed == "" {
		return groupKeyUnsetLabel
	}
	return "`" + trimmed + "`"
}

// subcardDraft is one sub-task card, built from the decomposer's output without
// touching the network so the rendering and tagging rules can be tested directly.
type subcardDraft struct {
	Title string
	Body  string
	Tags  []string
	// Notes records information the decomposer's output lost that would
	// otherwise vanish silently — a sub-task the model declined to group, say.
	// scopeParent logs these. They are prose for a human; nothing parses them.
	Notes []string
}

// buildSubcardDraft turns one decomposed sub-task into the card we will create.
// A blank Title means "skip this sub-task"; the caller decides that, because
// only it knows the sub-task's index for the log line.
//
// This deliberately does NOT invent a group_key for a sub-task that arrived
// without one. Which sub-tasks may share a session is a scoping decision that
// belongs to the decomposer, and kanban-dispatcher is correct for an untagged
// card without help from here.
func buildSubcardDraft(p parentCard, st Subtask, roleTags bool) subcardDraft {
	d := subcardDraft{Title: strings.TrimSpace(st.Title)}

	body := st.Body
	if body == "" {
		body = "(no body)"
	}
	group := strings.TrimSpace(st.GroupKey)
	d.Body = fmt.Sprintf("%s\n\n---\n_Sub-task of [%s](kanban://%s) — group: %s_\n",
		body, p.title(), p.cardID, displayGroupKey(group))

	d.Tags = []string{tagSubtask}
	if group != "" {
		d.Tags = append(d.Tags, tagGroupPrefix+group)
	} else {
		// The schema marks group_key required and the prompt names a fallback
		// key for the degenerate case, so arriving here is a decomposer failure
		// this binary is equipped to notice. It used to notice it silently.
		d.Notes = append(d.Notes, "no group_key returned — filed with no group, will not share a session with any sibling")
	}

	if roleTags {
		if c := strings.TrimSpace(st.Capability); c != "" {
			d.Tags = append(d.Tags, "capability:"+c)
		}
		if r := strings.TrimSpace(st.Role); r != "" {
			d.Tags = append(d.Tags, "role:"+r)
		}
	}
	return d
}

// createSubcard creates a card in the given column and attaches a parent_card
// link in one go. We bypass issuesclient.CreateCard because that helper
// hard-codes autoworker-specific links.
func createSubcard(kc *issuesclient.Client, columnID, title, body string, tags []string, parentID string) (string, error) {
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
	// PathEscape keeps the board id one path segment, as issuesclient does for
	// every board id it sends.
	createCardURL := fmt.Sprintf("%s/boards/%s/cards", kc.BaseURL, url.PathEscape(kc.BoardID))
	resp, err := kc.HTTP.Post(createCardURL, "application/json", bytes.NewReader(bs))
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
		return id, fmt.Errorf("%w: %v", errParentLinkRefused, err)
	}
	return id, nil
}

// errParentLinkRefused separates the one createSubcard failure that leaves a
// card BEHIND on the board from the ones that leave nothing. The card id is
// returned alongside it, and the caller needs both facts to say what was lost:
// after this error the sub-card exists and is unreachable from its parent, so
// carrying on with the plan is right and abandoning it would re-create the
// siblings on the next tick.
var errParentLinkRefused = errors.New("parent link refused")

// cardTitle names a board card for a shortfall line when there is no parentCard
// value to ask. A card with no title is reported as "untitled" rather than as an
// empty string, because an unnamed card in a shortfall report is unactionable.
func cardTitle(item map[string]any) string {
	if item == nil {
		return "untitled"
	}
	if t, _ := item["title"].(string); t != "" {
		return t
	}
	return "untitled"
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
		// Same blank-key trap as the card body had: the raw key rendered a
		// missing group as "Title[]", which reads as a group named "".
		titles = append(titles, fmt.Sprintf("%s[%s]", st.Title, displayGroupKey(strings.TrimSpace(st.GroupKey))))
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
		"title": map[string]any{"type": "string", "maxLength": 100},
		"body":  map[string]any{"type": "string"},
		// minLength 1 because listing group_key in "required" does not reject "" —
		// an empty string satisfies {"type":"string"}, so the requirement bought
		// nothing on its own. This is advisory: the schema is a hint to the model,
		// nothing validates the tool result on this side, so buildSubcardDraft
		// still has to handle a blank key and say so. Both halves are needed.
		"group_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 60},
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

// decompose runs one schema-forced call through llm-bridge-server's POST
// /oneshot, asking for the configured model-store role; the bridge picks the
// model and the instance.
func decompose(ctx context.Context, cfg config, digest string) (*Plan, error) {
	sys := systemPrompt
	if cfg.RoleTags {
		sys += "\n\n" + roleTagsInstructions
	}
	var p Plan
	answer, err := oneshot.Call(ctx, oneshot.Config{
		BridgeURL: cfg.BridgeURL,
		ModelRole: cfg.ModelRole,
		Caller:    "kanban-scoper",
		MaxTokens: 4096,
		Timeout:   180 * time.Second,
	}, sys, digest, decomposeSchema(cfg.RoleTags), &p)
	if err != nil {
		return nil, err
	}
	log.Printf("[scoper] decomposed with role %s: %s", cfg.ModelRole, answer)
	return &p, nil
}

// ============================ Scheduler self-registration ============================

// registerSchedulerJob creates the tick job with the board id baked into the
// command: the scheduler runs jobs as `sh -c` with its own thin environment,
// so it would reach the process no other way. The model role is not in the
// command; the scoper reads it from the scheduler's settings each tick.
func registerSchedulerJob(schedURL, boardID string) error {
	home, _ := os.UserHomeDir()
	command := filepath.Join(home, "bin", "kanban-scoper") + " -board-id " + boardID
	return schedulerjobs.RegisterIfAbsent(schedURL, schedulerjobs.Job{
		Name:        "kanban-scoper-tick",
		Description: "Decomposes task-completion-loop parent cards into sub-cards.",
		Schedule:    "*/5 * * * *",
		Command:     command,
	})
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

func anyTagEquals(tags []issuesclient.EntityTag, want string) bool {
	for _, t := range tags {
		if t.Tag == want {
			return true
		}
	}
	return false
}
