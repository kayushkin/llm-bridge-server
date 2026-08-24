// synthetic-mail writes a mailbox that does not exist.
//
// It generates believable work email for a synthetic mail account, so that the
// email classifier, the kanban board and the card drawer can be exercised
// against real messages rather than against references to messages nobody can
// open.
//
// The split of responsibilities is the whole design:
//
//   - The PLANNER decides everything a sender and a mail system decide: who
//     writes to whom, when, what kind of message it is, which work item it
//     concerns, and how it threads. No model is involved, so the shape of the
//     mailbox is reproducible from a seed.
//   - The MODEL writes only the subject and the body, given the plan. It is
//     never asked to invent a date, a threading header or a recipient, because
//     those are the fields everything downstream joins on.
//   - The CORPUS is committed. Re-seeding the demo board costs nothing and
//     produces the same mailbox twice, and generated prose that lives in version
//     control can actually be read in a review.
//
// Arrival times come from the board, not from this tool: each card's timeline
// already records when its work started, and mail that disagreed with it would
// put every message at odds with the timeline it is supposed to explain.
//
// Usage:
//
//	synthetic-mail -generate                 # plan from the board, write the prose, save the corpus
//	synthetic-mail -generate -dump-brief     # print what would be sent to the model, and stop
//	synthetic-mail -append                   # push the corpus into mailstack, recording locators
//
// -generate leaves alone any message that already has prose, so a run that
// failed part way can be repeated without paying to rewrite what succeeded.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/slava/scheduler/internal/anthropic"
	"github.com/slava/scheduler/internal/kanbanclient"
)

const (
	defaultModel     = "claude-sonnet-4-6"
	defaultBoardName = "Work board"
	defaultAccount   = "demo-work"
	defaultCorpus    = "cmd/synthetic-mail/corpus.json"
)

type config struct {
	Generate   bool
	Append     bool
	DumpBrief  bool
	CorpusPath string

	BoardName string
	KanbanURL string

	MailURL   string
	MailToken string
	Account   string

	AuthStoreURL string
	AuthToken    string
	Model        string

	Noise int
	Seed  int64
}

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "synthetic-mail: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var c config
	flag.BoolVar(&c.Generate, "generate", false, "Plan from the board and write the prose, saving the corpus.")
	flag.BoolVar(&c.Append, "append", false, "Push the corpus into mailstack, recording each message's locator.")
	flag.BoolVar(&c.DumpBrief, "dump-brief", false, "With -generate: print what would be sent to the model and stop, calling nothing.")
	flag.StringVar(&c.CorpusPath, "corpus", defaultCorpus, "Where the corpus lives.")
	flag.StringVar(&c.BoardName, "board", defaultBoardName, "Board whose cards the mail is about.")
	flag.StringVar(&c.KanbanURL, "kanban-url", envOr("KANBAN_STORE_URL", "http://localhost:8305"), "kanban-store base URL")
	flag.StringVar(&c.MailURL, "mail-url", envOr("MAILSTACK_URL", "http://localhost:8195"), "mailstack base URL")
	flag.StringVar(&c.MailToken, "mail-token", os.Getenv("MAILSTACK_TOKEN"), "mailstack bearer token")
	flag.StringVar(&c.Account, "account", defaultAccount, "The LOCAL mailstack account to append to. mailstack refuses any other kind.")
	flag.StringVar(&c.AuthStoreURL, "auth-store-url", envOr("AUTH_STORE_URL", "http://localhost:8303"), "auth-store base URL")
	flag.StringVar(&c.AuthToken, "auth-token", os.Getenv("AUTH_STORE_TOKEN"), "auth-store bearer token")
	flag.StringVar(&c.Model, "model", envOr("SYNTHETIC_MAIL_MODEL", defaultModel), "Anthropic model id")
	flag.IntVar(&c.Noise, "noise", 5, "How many messages about no work item at all — the bulk and the unfilable.")
	flag.Int64Var(&c.Seed, "seed", 20260824, "Planner seed. The same seed plans the same mailbox.")
	flag.Parse()
	return c
}

func run(cfg config) error {
	if !cfg.Generate && !cfg.Append {
		flag.Usage()
		return fmt.Errorf("nothing to do: pass -generate or -append")
	}
	ctx := context.Background()

	if cfg.Generate {
		if err := runGenerate(ctx, cfg); err != nil {
			return err
		}
	}
	if cfg.Append {
		if err := runAppend(ctx, cfg); err != nil {
			return err
		}
	}
	return nil
}

func runGenerate(ctx context.Context, cfg config) error {
	fmt.Printf("reading board %q from %s\n", cfg.BoardName, cfg.KanbanURL)
	items, err := readWorkItems(cfg.KanbanURL, cfg.BoardName)
	if err != nil {
		return err
	}
	fmt.Printf("  %d work item(s)\n", len(items))

	now := time.Now().UTC()
	messages, err := plan(items, cfg.Noise, cfg.Seed, now)
	if err != nil {
		return err
	}
	fmt.Printf("planned %d message(s) across %d thread(s)\n", len(messages), countThreads(messages))

	// Carry over any prose already written for the same message key, so a
	// re-plan does not throw away work the model has already been paid for.
	if existing, err := loadCorpus(cfg.CorpusPath); err == nil {
		carried := carryOverProse(existing.Messages, messages)
		if carried > 0 {
			fmt.Printf("  kept prose for %d message(s) already in %s\n", carried, cfg.CorpusPath)
		}
	}

	itemsByTitle := make(map[string]workItem, len(items))
	for _, item := range items {
		itemsByTitle[item.Title] = item
	}

	if cfg.DumpBrief {
		batch := make([]int, 0, len(messages))
		for i := range messages {
			if !messages[i].Generated() {
				batch = append(batch, i)
			}
		}
		fmt.Println(strings.Repeat("─", 72))
		fmt.Print(buildBrief(messages, batch, itemsByTitle))
		fmt.Println(strings.Repeat("─", 72))
		fmt.Println("(-dump-brief: nothing was sent and no corpus was written)")
		return nil
	}

	// The credential is resolved only when there is prose to write. A re-plan
	// that carries every message over calls no model, and should not demand a
	// key to do nothing.
	var modelCfg anthropic.Config
	if anyUngenerated(messages) {
		key, err := anthropic.ResolveKey(ctx, cfg.AuthStoreURL, cfg.AuthToken,
			"synthetic-mail", "write-synthetic-work-email-for-the-demo-board")
		if err != nil {
			return err
		}
		fmt.Printf("writing prose with %s\n", cfg.Model)
		modelCfg = anthropic.Config{
			APIKey: key, Model: cfg.Model, MaxTokens: maxOutputTokens, Timeout: 180 * time.Second,
		}
	}
	if err := generate(ctx, modelCfg, messages, itemsByTitle); err != nil {
		return err
	}

	corpus := &Corpus{
		Model: cfg.Model, GeneratedAt: time.Now().UTC(), Account: cfg.Account, Messages: messages,
	}
	if err := saveCorpus(cfg.CorpusPath, corpus); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", cfg.CorpusPath)
	summarize(messages)
	return nil
}

func runAppend(ctx context.Context, cfg config) error {
	corpus, err := loadCorpus(cfg.CorpusPath)
	if err != nil {
		return err
	}
	client := newMailstackClient(cfg.MailURL, cfg.MailToken)
	fmt.Printf("appending %d message(s) to account %q\n", len(corpus.Messages), cfg.Account)

	appended, skipped := 0, 0
	for i := range corpus.Messages {
		m := &corpus.Messages[i]
		if !m.Generated() {
			return fmt.Errorf("message %s has no prose; run -generate first", m.Key)
		}
		if m.Locator != "" {
			skipped++
			continue
		}
		locator, err := client.Append(ctx, cfg.Account, *m)
		if err != nil {
			// Save what did land before giving up, so a retry does not append
			// the successful ones a second time.
			_ = saveCorpus(cfg.CorpusPath, corpus)
			return fmt.Errorf("append %s: %w", m.Key, err)
		}
		m.Locator = locator
		appended++
		fmt.Printf("  %-8s %-20s %s\n", m.Key, m.Kind, truncateForDisplay(m.Subject, 52))
	}

	if err := saveCorpus(cfg.CorpusPath, corpus); err != nil {
		return err
	}
	fmt.Printf("appended %d, already present %d\n", appended, skipped)
	return nil
}

// readWorkItems reads the board and each card's timeline.
//
// The timeline read is what pins a message to the moment its card says the work
// arrived. It costs one request per card, which is affordable on a demo board
// and would not be on a real one.
func readWorkItems(kanbanURL, boardName string) ([]workItem, error) {
	client := kanbanclient.New(kanbanURL)
	board, err := client.FindBoard(boardName)
	if err != nil {
		return nil, fmt.Errorf("find board %q: %w", boardName, err)
	}
	view, err := client.GetBoardView(board.ID)
	if err != nil {
		return nil, fmt.Errorf("read board %q: %w", boardName, err)
	}

	var items []workItem
	for _, column := range view.Columns {
		for _, card := range column.Cards {
			if card.Item == nil {
				continue
			}
			title, _ := card.Item["title"].(string)
			if title == "" {
				continue
			}
			body, _ := card.Item["body"].(string)
			arrived, err := earliestActivity(kanbanURL, card.Placement.CardID)
			if err != nil {
				return nil, err
			}
			items = append(items, workItem{
				Title:     title,
				Body:      body,
				Action:    tagValue(card.Item, "action:"),
				ArrivedAt: arrived,
			})
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("board %q has no cards to write mail about", boardName)
	}
	return items, nil
}

// earliestActivity is when this card's story starts, according to the card.
func earliestActivity(kanbanURL, cardID string) (time.Time, error) {
	url := fmt.Sprintf("%s/api/cards/%s/events", strings.TrimRight(kanbanURL, "/"), cardID)
	resp, err := http.Get(url)
	if err != nil {
		return time.Time{}, fmt.Errorf("read events for %s: %w", cardID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("read events for %s: HTTP %d", cardID, resp.StatusCode)
	}
	var events []struct {
		Kind       string    `json:"kind"`
		OccurredAt time.Time `json:"occurred_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return time.Time{}, fmt.Errorf("decode events for %s: %w", cardID, err)
	}
	if len(events) == 0 {
		return time.Time{}, fmt.Errorf("card %s has no recorded activity, so there is no arrival time to write mail against", cardID)
	}
	earliest := events[0].OccurredAt
	for _, e := range events {
		if e.OccurredAt.Before(earliest) {
			earliest = e.OccurredAt
		}
	}
	return earliest.UTC(), nil
}

// tagValue reads a prefixed tag such as "action:task" off a noteboard item.
func tagValue(item map[string]any, prefix string) string {
	raw, ok := item["tags"].([]any)
	if !ok {
		return ""
	}
	for _, t := range raw {
		tag, ok := t.(string)
		if ok && strings.HasPrefix(tag, prefix) {
			return strings.TrimPrefix(tag, prefix)
		}
	}
	return ""
}

// carryOverProse copies already-written subjects and bodies onto a fresh plan,
// matched by message key.
func carryOverProse(existing, planned []Message) int {
	byKey := make(map[string]Message, len(existing))
	for _, m := range existing {
		byKey[m.Key] = m
	}
	carried := 0
	for i := range planned {
		previous, ok := byKey[planned[i].Key]
		if !ok || !previous.Generated() {
			continue
		}
		// Only carry prose when the plan for that key has not changed shape.
		// Re-using a body written for a different kind or a different work item
		// would keep the corpus looking generated while quietly making it wrong.
		if previous.Kind != planned[i].Kind || previous.WorkItem != planned[i].WorkItem {
			continue
		}
		planned[i].Subject = previous.Subject
		planned[i].Body = previous.Body
		planned[i].Locator = previous.Locator
		carried++
	}
	return carried
}

func anyUngenerated(messages []Message) bool {
	for _, m := range messages {
		if !m.Generated() {
			return true
		}
	}
	return false
}

func countThreads(messages []Message) int {
	seen := map[string]struct{}{}
	for _, m := range messages {
		seen[m.ThreadKey] = struct{}{}
	}
	return len(seen)
}

func summarize(messages []Message) {
	byKind := map[Kind]int{}
	replies := 0
	for _, m := range messages {
		byKind[m.Kind]++
		if m.ReplyToKey != "" {
			replies++
		}
	}
	kinds := make([]string, 0, len(byKind))
	for k, n := range byKind {
		kinds = append(kinds, fmt.Sprintf("%s=%d", k, n))
	}
	sort.Strings(kinds)
	fmt.Printf("  %s\n", strings.Join(kinds, "  "))
	fmt.Printf("  %d of %d are replies continuing a thread\n", replies, len(messages))
}

func truncateForDisplay(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit-1] + "…"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
