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
	"github.com/slava/scheduler/internal/demoteam"
	"github.com/slava/scheduler/internal/oneshot"
)

const (
	defaultRepo    = "/home/kayushkincom/repos/northwind-api"
	defaultAccount = "demo-work"
	defaultModel   = "claude-sonnet-4-6"

	// Sized for a handful of full email bodies. Too small does not fail
	// silently — oneshot reports truncation — but it does fail.
	maxOutputTokens = 8192
)

type config struct {
	Repo      string
	Account   string
	MailURL   string
	MailToken string

	BridgeURL       string
	OneshotInstance string
	Model           string

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
	flag.StringVar(&c.BridgeURL, "bridge-url", envOr("LLM_BRIDGE_URL", "http://localhost:8160"), "llm-bridge-server base URL")
	flag.StringVar(&c.OneshotInstance, "oneshot-instance", envOr("ONESHOT_INSTANCE_ID", "inst-cc-local"),
		"claude_code harness instance for the subscription oneshot call")
	flag.StringVar(&c.Model, "model", envOr("DEMO_MAIL_MODEL", defaultModel), "model id for the oneshot call")
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
		fmt.Print(buildBrief(project, plan))
		fmt.Println(strings.Repeat("─", 72))
		fmt.Println("(-dry-run: nothing was sent and no mail was delivered)")
		return nil
	}

	var written struct {
		Messages []struct {
			Key     string `json:"key"`
			Subject string `json:"subject"`
			Body    string `json:"body"`
		} `json:"messages"`
	}
	// Composed through llm-bridge's oneshot endpoint, which forces the schema
	// and runs on the Claude Code subscription login. No credential passes
	// through here at all.
	//
	// The composing is the model's job and the DELIVERY is not: an agent asked
	// to curl its own output has to get shell quoting right on a body full of
	// newlines and apostrophes, and a body mangled that way looks delivered.
	// Go posts it instead.
	if err := oneshot.Call(ctx, oneshot.Config{
		BridgeURL:  cfg.BridgeURL,
		InstanceID: cfg.OneshotInstance,
		Model:      cfg.Model,
		MaxTokens:  maxOutputTokens,
		Timeout:    300 * time.Second,
	}, systemPrompt, buildBrief(project, plan), writeSchema(), &written); err != nil {
		return err
	}

	byKey := map[string]struct{ subject, body string }{}
	for _, m := range written.Messages {
		byKey[m.Key] = struct{ subject, body string }{strings.TrimSpace(m.Subject), strings.TrimSpace(m.Body)}
	}

	delivered := 0
	for _, want := range plan {
		got, ok := byKey[want.Key]
		if !ok || got.subject == "" || got.body == "" {
			// Loudly, and keep going: one unusable message must not cost the
			// others, and must not pass unnoticed either.
			fmt.Printf("  WARNING: nothing usable came back for %s (%s)\n", want.Key, want.Kind.Name)
			continue
		}
		locator, err := mail.Append(ctx, cfg.Account, demomail.Message{
			Subject:  got.subject,
			From:     demomail.Address{Name: want.From.Name, Email: want.From.Email},
			To:       []demomail.Address{{Name: "Slava", Email: "slava@northwind-eng.example"}},
			BodyText: got.body,
		})
		if err != nil {
			return fmt.Errorf("deliver %s: %w", want.Key, err)
		}
		fmt.Printf("  %-14s %-20s %s\n", want.Kind.Name, want.From.Name, truncate(got.subject, 52))
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
		// Only members with an address can send mail. WithRole also returns
		// the agents — northwind-reviewer shares RoleReviewer with Dinesh —
		// and an agent has no Email, so picking one produced a message
		// mailstack refused with "from.email is required". Measured on
		// scheduler job 82, 2026-09-03: two of the first eight ticks.
		senders := sendersWithMail(demoteam.WithRole(kind.From))
		if len(senders) == 0 {
			senders = sendersWithMail(demoteam.Roster)
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

// sendersWithMail keeps the members who have an address to send from.
func sendersWithMail(members []demoteam.Member) []demoteam.Member {
	var out []demoteam.Member
	for _, m := range members {
		if m.SendsMail() {
			out = append(out, m)
		}
	}
	return out
}
