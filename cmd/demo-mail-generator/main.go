// demo-mail-generator writes work email about a real repository.
//
// It reads the project first — the README, the packages, the routes it actually
// serves, its recent commits and its open pull requests — and only then asks a
// model to write mail. That order is the whole point. The first version of this
// demo used hand-written requests, and one asked an agent to "bump the build
// image to Node 22 LTS" in a repository that has never contained a line of
// JavaScript. The agent checked every branch and all twelve commits, refused,
// and wrote down why. That was the agent behaving well and a real session spent
// on a request that could not be satisfied.
//
// Mail is appended at the moment it is generated, with no backdating. Every
// other timestamp in this system is real — when a card was created, when an
// agent ran, when a note was written — and mail invented in the past disagrees
// with all of them.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/slava/scheduler/internal/demomail"
	"github.com/slava/scheduler/internal/demosession"
	"github.com/slava/scheduler/internal/demoteam"
)

const (
	defaultRepo    = "/home/kayushkincom/repos/northwind-api"
	defaultAccount = "demo-work"
)

type config struct {
	Repo      string
	Account   string
	MailURL   string
	MailToken string

	NATSURL    string
	WriterSlug string
	MaxBudget  float64

	Count  int
	Purge  bool
	DryRun bool
	Seed   int64
}

func main() {
	if err := run(parseFlags()); err != nil {
		fmt.Fprintf(os.Stderr, "demo-mail-generator: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.Repo, "repo", defaultRepo, "Repository the mail is about.")
	flag.StringVar(&c.Account, "account", defaultAccount, "The LOCAL mailstack account to deliver to.")
	flag.StringVar(&c.MailURL, "mail-url", envOr("MAILSTACK_URL", "http://localhost:8195"), "mailstack base URL")
	flag.StringVar(&c.MailToken, "mail-token", os.Getenv("MAILSTACK_TOKEN"), "mailstack bearer token")
	flag.StringVar(&c.NATSURL, "nats-url", envOr("NATS_URL", "nats://localhost:4222"), "NATS URL")
	flag.StringVar(&c.WriterSlug, "writer", "northwind-priya", "Roster identity the writing session acts as.")
	flag.Float64Var(&c.MaxBudget, "max-budget-usd", 2, "Spend ceiling for the writing session.")
	flag.IntVar(&c.Count, "count", 2, "How many messages to write this tick.")
	flag.BoolVar(&c.Purge, "purge", false, "Delete every message in the account first.")
	flag.BoolVar(&c.DryRun, "dry-run", false, "Print what would be sent to the model and stop.")
	flag.Int64Var(&c.Seed, "seed", 0, "Sender/kind seed. 0 uses the clock, so consecutive ticks differ.")
	flag.Parse()
	return c
}

func run(cfg config) error {
	ctx := context.Background()
	mail := demomail.New(cfg.MailURL, cfg.MailToken)

	if cfg.Purge {
		removed, err := mail.Purge(ctx, cfg.Account)
		if err != nil {
			return fmt.Errorf("purge %s: %w", cfg.Account, err)
		}
		fmt.Printf("purged %d message(s) from %s\n", removed, cfg.Account)
		if cfg.Count == 0 {
			return nil
		}
	}

	project, err := demomail.ReadProject(cfg.Repo)
	if err != nil {
		return err
	}
	fmt.Printf("read %s — %d packages, %d routes, %d tests, %d open PR(s)\n",
		project.Name, len(project.Packages), len(project.Endpoints), project.Tests, len(project.OpenPRs))

	seed := cfg.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	plan := planMessages(cfg.Count, rand.New(rand.NewSource(seed)))
	if cfg.DryRun {
		fmt.Println(strings.Repeat("─", 72))
		fmt.Print(sessionPrompt(project, plan, cfg.Account, cfg.MailURL, "<mailstack token>"))
		fmt.Println(strings.Repeat("─", 72))
		fmt.Println("(-dry-run: no session was spawned and no mail was delivered)")
		return nil
	}

	sessionID, err := demosession.Spawn(cfg.NATSURL, demosession.Task{
		Prompt:      sessionPrompt(project, plan, cfg.Account, cfg.MailURL, cfg.MailToken),
		AgentSlug:   cfg.WriterSlug,
		DisplayName: fmt.Sprintf("✉️ mail for %s %s", project.Name, time.Now().UTC().Format("Jan 2 15:04")),
		WorkingDir:  cfg.Repo,
		// Capped, because an agent writing four emails should never be able to
		// spend like one refactoring a service.
		MaxBudgetUSD: cfg.MaxBudget,
	})
	if err != nil {
		return err
	}
	fmt.Printf("dispatched %s as %s — it writes and posts the mail itself\n", sessionID, cfg.WriterSlug)
	for _, m := range plan {
		fmt.Printf("  %-14s %s\n", m.Kind.Name, m.From.Name)
	}
	return nil
}

// planned is one message before it has any words.
type planned struct {
	Key  string
	Kind requestKind
	From demoteam.Member
}

// planMessages decides who writes and what sort of thing they want, with no
// model involved, so the mix is controlled rather than whatever the model felt
// like producing.
func planMessages(count int, random *rand.Rand) []planned {
	var out []planned
	for i := 0; i < count; i++ {
		kind := requestKinds[random.Intn(len(requestKinds))]
		senders := demoteam.WithRole(kind.From)
		if len(senders) == 0 {
			senders = demoteam.Roster
		}
		out = append(out, planned{
			Key:  fmt.Sprintf("msg-%d", i+1),
			Kind: kind,
			From: senders[random.Intn(len(senders))],
		})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
