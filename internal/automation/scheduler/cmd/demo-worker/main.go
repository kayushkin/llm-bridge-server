// demo-worker runs the simulated Northwind team.
//
// It has two jobs, and they are deliberately separate steps:
//
//	PROMOTE  a card the classifier filed on the Work board becomes work: it is
//	         placed on the Northwind work board's Queued column, told which
//	         repo it concerns, which identity will do it, and what it may cost.
//	DISPATCH a queued card becomes a real agent session, working in that repo,
//	         acting as that identity, capped at that spend.
//
// Why this exists rather than autoworker doing it. autoworker dispatches only
// when a usage curve allows, which by its own logic is never for much of the
// week — correct for protecting real spend, useless for a demo that must run on
// a cadence. This ignores the curve and nothing else: the spend ceiling, the
// hold gate and the working-directory rules are the same ones autoworker
// enforces, because they are the parts that stop an agent hurting something.
//
// It also does NOT use the "Autoworker queue" board. autoworker polls that board
// every five minutes, and two dispatchers over one queue would each spawn a
// session for the same card.
//
// Roles are enforced here rather than in permission-store. A builder gets a
// prompt that says branch-and-open-a-PR; a reviewer gets one that says read the
// diff and say what you think. Neither can do the other's job because neither is
// told how — which is weaker than a permission rule and is the honest state of
// it today.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/bridgeauth"
	schedulerconfig "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/config"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/demoteam"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/kanbanclient"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/kanbanvocab"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/schedulersettings"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/sessionmodelrole"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

const (
	columnQueued     = kanbanvocab.ColumnQueued
	columnInProgress = kanbanvocab.ColumnInProgress
	columnActionNeed = "Action needed"

	// tagWorkNotStarted is the seam email-classifier already writes on every
	// card it files and that nothing has ever read. Its own comment says
	// "spawning an agent to work a card is a later step, and this is the field
	// it will move through". This is that step.
	tagWorkNotStarted = "work:not-started"
	tagWorkQueued     = "work:queued"

	// actorPrefixAgent namespaces attribution. The actor column today holds
	// three incompatible conventions over an 89%-empty field: bare handles,
	// harness names, and email addresses. Anything this tool writes says what
	// KIND of actor it was, so the board can tell a person from a process.
	actorPrefixAgent = "agent:"
	actorService     = "service:demo-worker"
)

type config struct {
	Promote  bool
	Dispatch bool
	Review   bool
	DryRun   bool

	// EmailBoardID is the board the classifier files onto; WorkBoardID the
	// board agents work from. Ids, never names: kanban-store resolves a name
	// with auto-create and no unique constraint, so a rename forks the board.
	EmailBoardID string
	WorkBoardID  string
	KanbanURL    string
	NoteboardURL string
	BridgeURL    string
	RepoPath     string

	// The tokens this binary presents as an internal service. Both stores
	// gate every route.
	KanbanServiceToken string
	BridgeServiceToken string

	BuilderSlug  string
	ReviewerSlug string
	RepoSlug     string
	MaxBudget    float64
	MaxPerTick   int

	// ModelRole is the model-store role every session this run starts runs
	// on: the scheduler's demo_worker_model_role setting, read at start when
	// the run dispatches or reviews. ModelRoles checks it against each
	// session's harness.
	ModelRole  string
	ModelRoles sessionmodelrole.RoleChecker
}

func main() {
	settings, err := schedulerconfig.DemoWorker.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo-worker: %v\n", err)
		os.Exit(1)
	}
	cfg := parseFlags(settings)
	// llm-bridge-server gates every route; stamp this process's calls to it
	// with the service token. Host-scoped, so nothing else sees the token.
	if err := bridgeauth.StampRequestsToBridge(cfg.BridgeURL, cfg.BridgeServiceToken); err != nil {
		fmt.Fprintf(os.Stderr, "demo-worker: %v\n", err)
		os.Exit(1)
	}
	// kanban-store gates every route too, and this binary writes cards with its
	// own client as well as through kanbanclient. Host-scoped, like the above.
	if err := kanbanclient.StampRequestsToKanbanStore(cfg.KanbanURL, cfg.KanbanServiceToken); err != nil {
		fmt.Fprintf(os.Stderr, "demo-worker: %v\n", err)
		os.Exit(1)
	}
	if cfg.Dispatch || cfg.Review {
		cfg.ModelRole, err = schedulersettings.ReadModelRole(context.Background(), settings.String(schedulerconfig.SettingSchedulerURL), schedulerconfig.SettingDemoWorkerModelRole)
		if err != nil {
			fmt.Fprintf(os.Stderr, "demo-worker: %v\n", err)
			os.Exit(1)
		}
		cfg.ModelRoles = sessionmodelrole.New(settings.String(schedulerconfig.SettingModelStoreURL), cfg.BridgeURL)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "demo-worker: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags(settings *servicesettings.Registry) config {
	var c config
	flag.BoolVar(&c.Promote, "promote", false, "Move eligible Work board cards onto the work board's Queued column.")
	flag.BoolVar(&c.Dispatch, "dispatch", false, "Spawn a real agent session for queued cards.")
	flag.BoolVar(&c.Review, "review", false, "Spawn a reviewer for cards whose pull request has not been reviewed.")
	flag.BoolVar(&c.DryRun, "dry-run", false, "Say what would happen and change nothing.")
	flag.StringVar(&c.EmailBoardID, "email-board-id", "", "Id of the board the classifier files onto (required).")
	flag.StringVar(&c.WorkBoardID, "work-board-id", "", "Id of the board the agents work from (required). Its default_instance_id says where sessions run and its default_bundle_id what they are provisioned with.")
	flag.StringVar(&c.KanbanURL, "kanban-url", settings.String(schedulerconfig.SettingKanbanStoreURL), "kanban-store base URL")
	flag.StringVar(&c.NoteboardURL, "noteboard-url", settings.String(schedulerconfig.SettingNoteboardURL), "noteboard base URL")
	flag.StringVar(&c.BridgeURL, "bridge-url", settings.String(schedulerconfig.SettingBridgeURL),
		"llm-bridge-server base URL. Sessions are created here directly rather than published to NATS, so a failure to start is an error rather than silence.")
	flag.StringVar(&c.RepoPath, "repo", "", "The repo agents work in. Required: this machine's path is the caller's to give.")
	flag.StringVar(&c.BuilderSlug, "builder", "northwind-builder", "Identity that does the building.")
	flag.StringVar(&c.ReviewerSlug, "reviewer", "northwind-reviewer", "Identity that does the reviewing.")
	flag.StringVar(&c.RepoSlug, "repo-slug", "kayushkin/northwind-api", "owner/name of the repo, for pull request refs.")
	flag.Float64Var(&c.MaxBudget, "max-budget-usd", 10, "Per-card spend ceiling.")
	flag.IntVar(&c.MaxPerTick, "max", 1, "Most cards to act on in one tick.")
	flag.Parse()
	c.KanbanServiceToken = settings.String(schedulerconfig.SettingKanbanStoreServiceToken)
	c.BridgeServiceToken = settings.String(schedulerconfig.SettingBridgeServiceToken)
	return c
}

func run(cfg config) error {
	if !cfg.Promote && !cfg.Dispatch && !cfg.Review {
		flag.Usage()
		return fmt.Errorf("nothing to do: pass -promote, -dispatch or -review")
	}
	if cfg.EmailBoardID == "" || cfg.WorkBoardID == "" {
		return fmt.Errorf("-email-board-id and -work-board-id are both required; there is no board to default to")
	}
	if cfg.MaxBudget <= 0 {
		// Zero is not "no ceiling" to a harness, it is "unlimited" — the exact
		// confusion autoworker's spend code warns about.
		return fmt.Errorf("-max-budget-usd must be positive; 0 reads as UNLIMITED to the harness")
	}
	if (cfg.Dispatch || cfg.Review) && (cfg.ModelRole == "" || cfg.ModelRoles == nil) {
		return fmt.Errorf("no model role to start sessions on: a session that names none runs on whatever the bridge's harness default is")
	}
	if (cfg.Dispatch || cfg.Review) && cfg.RepoPath == "" {
		return fmt.Errorf("give -repo, the repo agents work in")
	}
	if cfg.DryRun {
		fmt.Println("DRY RUN — nothing will be written or spawned")
	}

	kc := kanbanclient.New(cfg.KanbanURL, cfg.KanbanServiceToken)
	if cfg.Promote {
		if err := promote(kc, cfg); err != nil {
			return err
		}
	}
	if cfg.Dispatch {
		if err := dispatch(kc, cfg); err != nil {
			return err
		}
	}
	if cfg.Review {
		if err := review(kc, cfg); err != nil {
			return err
		}
	}
	return nil
}

// requireIdentity resolves the identity work will be done as, and refuses to
// proceed without it.
//
// Attribution is the whole reason this tool sets AgentID, so dispatching as
// nobody would quietly give back the thing that was just built.
func requireIdentity(slug string, want demoteam.Role) (demoteam.Member, error) {
	m, ok := demoteam.BySlug(slug)
	if !ok {
		return demoteam.Member{}, fmt.Errorf("identity %q is not on the roster; add it to internal/demoteam", slug)
	}
	if m.Role != want {
		return demoteam.Member{}, fmt.Errorf(
			"identity %q holds role %q, and this work needs a %s — a reviewer must not be sent to write code, nor a builder to sign off its own",
			slug, m.Role, want)
	}
	return m, nil
}

// sessionHost is where a session for a card on the work board runs and what it
// is provisioned with, read from that board's settings.
type sessionHost struct {
	InstanceID string
	BundleID   string
}

// sessionHostFromBoard reads the work board's default_instance_id and
// default_bundle_id. The instance is required — there is no fallback, because
// the one demosession used to carry ("inst-cc-local") was a second copy of a
// board setting and the two would drift. The bundle is optional and passed on
// verbatim.
//
// default_agent_id is deliberately NOT read here. The identity a session acts
// as is this demo's own rotation — the builder for -dispatch, the reviewer for
// -review (identity.Slug and reviewer.Slug, both checked against the roster by
// requireIdentity) — because the prompt each gets is written for that role and
// the card's attribution names it. One board-wide default agent would put the
// reviewer's session under the builder's name, or the other way round.
func sessionHostFromBoard(board *kanbanclient.Board) (sessionHost, error) {
	if board.DefaultInstanceID == "" {
		return sessionHost{}, fmt.Errorf("board %q (%s) has no default_instance_id — set it (PATCH /api/boards/%s {\"default_instance_id\":…}); sessions have nowhere to run",
			board.Name, board.ID, board.ID)
	}
	return sessionHost{InstanceID: board.DefaultInstanceID, BundleID: board.DefaultBundleID}, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func stamp() string { return time.Now().UTC().Format(time.RFC3339) }

func hasTag(item map[string]any, want string) bool {
	raw, ok := item["tags"].([]any)
	if !ok {
		return false
	}
	for _, t := range raw {
		if s, ok := t.(string); ok && s == want {
			return true
		}
	}
	return false
}

func tagsOf(item map[string]any) []string {
	raw, _ := item["tags"].([]any)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if s, ok := t.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func actionOf(item map[string]any) string {
	for _, t := range tagsOf(item) {
		if strings.HasPrefix(t, "action:") {
			return strings.TrimPrefix(t, "action:")
		}
	}
	return ""
}
