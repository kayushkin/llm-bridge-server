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

	"github.com/slava/scheduler/internal/anthropic"
	"github.com/slava/scheduler/internal/demomail"
	"github.com/slava/scheduler/internal/demoteam"
)

const (
	defaultRepo    = "/home/kayushkincom/repos/northwind-api"
	defaultAccount = "demo-work"
	defaultModel   = "claude-sonnet-4-6"

	maxOutputTokens = 4096
	writeToolName   = "write_work_email"
)

type config struct {
	Repo      string
	Account   string
	MailURL   string
	MailToken string

	AuthStoreURL string
	AuthToken    string
	Model        string

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
	flag.StringVar(&c.AuthStoreURL, "auth-store-url", envOr("AUTH_STORE_URL", "http://localhost:8303"), "auth-store base URL")
	flag.StringVar(&c.AuthToken, "auth-token", os.Getenv("AUTH_STORE_TOKEN"), "auth-store bearer token")
	flag.StringVar(&c.Model, "model", envOr("DEMO_MAIL_MODEL", defaultModel), "Anthropic model id")
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
	brief := buildBrief(project, plan)

	if cfg.DryRun {
		fmt.Println(strings.Repeat("─", 72))
		fmt.Print(brief)
		fmt.Println(strings.Repeat("─", 72))
		fmt.Println("(-dry-run: nothing was sent and no mail was delivered)")
		return nil
	}

	key, err := anthropic.ResolveKey(ctx, cfg.AuthStoreURL, cfg.AuthToken,
		"demo-mail-generator", "write-work-email-about-a-real-repository")
	if err != nil {
		return err
	}
	var written struct {
		Messages []struct {
			Key     string `json:"key"`
			Subject string `json:"subject"`
			Body    string `json:"body"`
		} `json:"messages"`
	}
	if err := anthropic.InvokeTool(ctx, anthropic.Config{
		APIKey: key, Model: cfg.Model, MaxTokens: maxOutputTokens, Timeout: 120 * time.Second,
	}, systemPrompt, writeToolSchema(), writeToolName, brief, &written); err != nil {
		return err
	}

	byKey := map[string]string{}
	subjects := map[string]string{}
	for _, m := range written.Messages {
		byKey[m.Key] = strings.TrimSpace(m.Body)
		subjects[m.Key] = strings.TrimSpace(m.Subject)
	}

	delivered := 0
	for _, want := range plan {
		body, subject := byKey[want.Key], subjects[want.Key]
		if body == "" || subject == "" {
			// Loudly, and keep going: one unusable message should not cost the
			// others, but it must not pass unnoticed either.
			fmt.Printf("  WARNING: the model returned nothing usable for %s (%s)\n", want.Key, want.Kind)
			continue
		}
		locator, err := mail.Append(ctx, cfg.Account, demomail.Message{
			Subject:  subject,
			From:     demomail.Address{Name: want.From.Name, Email: want.From.Email},
			To:       []demomail.Address{{Name: "Vlad", Email: "vlad@northwind-eng.example"}},
			BodyText: body,
		})
		if err != nil {
			return fmt.Errorf("deliver %s: %w", want.Key, err)
		}
		fmt.Printf("  %-14s %-22s %s\n", want.Kind.Name, want.From.Name, truncate(subject, 54))
		fmt.Printf("      %s\n", locator)
		delivered++
	}
	fmt.Printf("delivered %d of %d\n", delivered, len(plan))
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
