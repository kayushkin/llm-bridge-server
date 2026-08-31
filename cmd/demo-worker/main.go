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
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/slava/scheduler/internal/demoteam"
	"github.com/slava/scheduler/internal/kanbanclient"
)

const (
	defaultEmailBoard = "Work board"
	defaultWorkBoard  = "Northwind work"
	defaultRepoPath   = "/home/kayushkincom/repos/northwind-api"

	columnQueued     = "Queued"
	columnInProgress = "In Progress"
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

	EmailBoard   string
	WorkBoard    string
	KanbanURL    string
	NoteboardURL string
	NATSURL      string
	RepoPath     string

	BuilderSlug  string
	ReviewerSlug string
	RepoSlug     string
	MaxBudget    float64
	MaxPerTick   int
}

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "demo-worker: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var c config
	flag.BoolVar(&c.Promote, "promote", false, "Move eligible Work board cards onto the work board's Queued column.")
	flag.BoolVar(&c.Dispatch, "dispatch", false, "Spawn a real agent session for queued cards.")
	flag.BoolVar(&c.Review, "review", false, "Spawn a reviewer for cards whose pull request has not been reviewed.")
	flag.BoolVar(&c.DryRun, "dry-run", false, "Say what would happen and change nothing.")
	flag.StringVar(&c.EmailBoard, "email-board", defaultEmailBoard, "Board the classifier files onto.")
	flag.StringVar(&c.WorkBoard, "work-board", defaultWorkBoard, "Board the agents work from.")
	flag.StringVar(&c.KanbanURL, "kanban-url", envOr("KANBAN_STORE_URL", "http://localhost:8305"), "kanban-store base URL")
	flag.StringVar(&c.NoteboardURL, "noteboard-url", envOr("NOTEBOARD_URL", "http://localhost:8191"), "noteboard base URL")
	flag.StringVar(&c.NATSURL, "nats-url", envOr("NATS_URL", "nats://localhost:4222"), "NATS URL")
	flag.StringVar(&c.RepoPath, "repo", defaultRepoPath, "The repo agents work in.")
	flag.StringVar(&c.BuilderSlug, "builder", "northwind-builder", "Identity that does the building.")
	flag.StringVar(&c.ReviewerSlug, "reviewer", "northwind-reviewer", "Identity that does the reviewing.")
	flag.StringVar(&c.RepoSlug, "repo-slug", "kayushkin/northwind-api", "owner/name of the repo, for pull request refs.")
	flag.Float64Var(&c.MaxBudget, "max-budget-usd", 10, "Per-card spend ceiling.")
	flag.IntVar(&c.MaxPerTick, "max", 1, "Most cards to act on in one tick.")
	flag.Parse()
	return c
}

func run(cfg config) error {
	if !cfg.Promote && !cfg.Dispatch && !cfg.Review {
		flag.Usage()
		return fmt.Errorf("nothing to do: pass -promote, -dispatch or -review")
	}
	if cfg.MaxBudget <= 0 {
		// Zero is not "no ceiling" to a harness, it is "unlimited" — the exact
		// confusion autoworker's spend code warns about.
		return fmt.Errorf("-max-budget-usd must be positive; 0 reads as UNLIMITED to the harness")
	}
	if cfg.DryRun {
		fmt.Println("DRY RUN — nothing will be written or spawned")
	}

	kc := kanbanclient.New(cfg.KanbanURL)
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
