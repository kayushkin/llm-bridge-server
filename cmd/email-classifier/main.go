// email-classifier files incoming mail onto cards on the "Email" kanban board.
//
// The relationship is deliberately inverted from the obvious design: an email is
// never a card. Cards are the unit of work, emails attach to them as links, and
// the relationship is many-to-many — one card gathers many emails, and one email
// may attach to several cards. A 400-message sample of this host's inbox held
// roughly 8% genuinely actionable mail, and six separate eBay messages about one
// jacket; card-per-email would have produced six cards for one decision and a
// board too large to read within a quarter.
//
// The one invariant: every email this command processes ends up on at least one
// card. Bulk mail earns durable "bucket" cards in the No action column that
// accumulate hundreds of links; real work earns task cards in Action needed.
// An email the model declines to file lands on a triage card rather than
// vanishing.
//
// Cadence:
//
//	Registered as a scheduler shell job. Each tick reads the board once, walks
//	the newest mail per account, and files what it has not filed before.
//
// Cost control, in the order the tick applies it:
//
//  1. Already-linked  — the card links ARE the watermark. No separate state
//     store, so the board and the record of what is filed
//     cannot drift apart.
//  2. Sender affinity — a sender whose mail already lands on exactly one
//     bucket card is routed there with no message fetch and
//     no model call. This is learned from the links, not
//     hardcoded, so it tightens on its own as the board fills.
//  3. Thread affinity — a reply whose References name an already-filed
//     Message-ID joins that message's cards directly.
//  4. The model       — only what survives the three cheap paths above.
//
// mailstack caches nothing and has no search route, so every message fetch is a
// live provider round trip. That is why enrichment happens after routing, not
// before: bulk mail is filed without ever being fetched.
package main

import (
	"context"
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

	"github.com/slava/scheduler/internal/kanbanclient"
)

const (
	defaultBoardName = "Email"

	columnNoAction        = "No action"
	columnActionNeeded    = "Action needed"
	columnActionCompleted = "Action completed"

	// Link types. entity_ref for linkTypeEmail is the mailstack locator
	// (account_id:message_id) because that is the only id mailstack can resolve
	// back to a message; linkTypeEmailMessageID carries the RFC 5322 identity
	// for cross-account dedup and threading, which the locator cannot express.
	linkTypeEmail          = "email"
	linkTypeEmailMessageID = "email_msgid"
	linkTypeEmailSender    = "email_sender"

	// Tags on the noteboard item behind each card. tagEmail is the "tag them all
	// as emails" requirement; the prefixed ones carry the two axes that are not
	// columns, and are what the board UI filters and regroups on.
	tagEmail        = "email"
	tagSource       = "email-classifier"
	tagPrefixCat    = "cat:"
	tagPrefixAction = "action:"
	tagPrefixUrgent = "urgency:"
	tagPrefixWork   = "work:"

	// workStateNotStarted is the default for every card. Start/stop is a state
	// flag for now; spawning an agent to work a card is a later step, and this
	// is the field it will move through.
	workStateNotStarted = tagPrefixWork + "not-started"

	// triageCardTitle is the home for mail the model returned no card for. It
	// sits in Action needed on purpose: an unfiled email is a classifier failure,
	// and parking it somewhere quiet would hide exactly the case worth seeing.
	triageCardTitle = "Unfiled email — needs triage"

	// evidenceMarker separates the part of a card body a human owns from the
	// linked-email list this command rewrites. Everything above it is preserved
	// verbatim on every pass.
	evidenceMarker = "<!-- email-classifier:evidence -->"

	// A better model than the sibling classifiers use, deliberately. The cheap
	// affinity layer already routes bulk mail with no model at all, so what
	// reaches the model is the novel mail — and measured across runs, 44-50% of
	// it results in a NEW card. Card creation is therefore the median case here,
	// not a rare one, and it is a judgement call: telling a task ("pay this
	// rent") from a record ("another receipt") is exactly what the cheap model
	// got wrong, naming six receipt cards after their amounts so no two could
	// ever merge. At ~20 batch calls a month the cost difference is not the
	// deciding factor.
	defaultModel = "claude-sonnet-4-6"

	// emailsPerModelCall bounds one batch. Emails are small, so batching is far
	// cheaper than a call each.
	//
	// Lowered from 25 after live runs: the batches that came back undecodable
	// were consistently the ones producing the most new cards — 25 filings plus
	// 14 new cards — and one reply degraded far enough to leak the model's own
	// `<parameter name=...>` markup into a string field. Reply size is the
	// pressure, and this is the knob that controls it.
	emailsPerModelCall = 10

	// maxEmailsPerTick bounds a tick's provider fetches and model spend. The
	// backlog drains across ticks rather than in one long run.
	maxEmailsPerTick = 50

	// mailPageSize is how many message headers one mailstack page returns. On
	// Gmail a page of 50 costs 51 upstream API calls, so pages are worth
	// minimizing but not worth splitting.
	mailPageSize = 50

	// scanPagesPerAccount caps how deep a tick walks back per account. In steady
	// state the first page is nearly all already-filed and the walk stops on the
	// first fully-filed page; this only binds on a cold or backfilling mailbox.
	scanPagesPerAccount = 4

	// digestCharBudget is one prompt's size in characters (~4 chars/token).
	// Characters because that is what the builder can actually measure.
	digestCharBudget = 60000

	// bodyCharsPerEmail is how much message body reaches the prompt. The signal
	// that separates a real task from marketing is almost always in the first
	// paragraph; the rest is footer and legal boilerplate.
	bodyCharsPerEmail = 700

	// stubBodyTextLimit is the length below which a plain-text part is treated
	// as a placeholder ("View this email in your browser") rather than the
	// message, so the HTML part is read instead.
	stubBodyTextLimit = 200

	// The scheduler job -register creates. These are named constants rather than
	// literals inside the registration call so a test can read the cadence and
	// the timeout the job actually ships with.
	//
	// schedulerJobTimeoutSeconds is the one with something to get wrong: a tick
	// fetches messages one live provider round trip at a time, so it legitimately
	// outruns the scheduler's 5-minute default, and 0 here means that default —
	// which kills the job's whole process group mid-tick.
	schedulerJobName           = "email-classifier-tick"
	schedulerJobSchedule       = "*/15 * * * *"
	schedulerJobTimeoutSeconds = 900
)

// environmentLookup has os.Getenv's shape. Taking it as an argument rather than
// calling os.Getenv directly is what lets a test state which variables were set
// without setting them: eight of the eighteen flag defaults below are read at
// flag-registration time, so a test that exported MAILSTACK_URL to observe it
// would be racing every other test in this package for one process-wide
// variable. This package has more of those than any of its siblings.
type environmentLookup func(key string) string

// parseFlags builds the flag set and parses args into a config.
//
// The flag set lives here rather than in main() so that a test can reach the
// defaults. Registered inside main() they were unreachable — nothing in a test
// can call main() — so the eighteen values this binary actually starts with had
// never been read by anything.
//
// Five of those eighteen are early-exit mode switches (-register, -merge,
// -backfill-due, -refile-unread-since, -dump-digest) and a sixth (-dry-run)
// neuters the writes. Any
// one of them shipping ON leaves a tick that reads the board, reports success
// and files no mail — which from outside is indistinguishable from a quiet
// mailbox. The two that decide spend are -model, which picks which model reads
// the digest, and -max, which bounds how much mail one tick pays to classify.
//
// The flag set is a fresh FlagSet, not the package-level flag.CommandLine: a
// test calling this twice would panic on the redefined flag, and that failure
// reads as a broken fixture rather than as the seam being unusable.
// ContinueOnError is what makes it callable at all — ExitOnError would take the
// test binary down with it on a bad argument instead of returning the error.
//
// homeDirectory is an argument for the same reason as lookupEnvironment: the
// registered command is home-relative, and a test should not have to move HOME
// out from under the process to assert what it resolves to.
func parseFlags(args []string, lookupEnvironment environmentLookup, homeDirectory string) (config, error) {
	fs := flag.NewFlagSet("email-classifier", flag.ContinueOnError)
	var c config
	fs.BoolVar(&c.RegisterJob, "register", false, "Register this binary as a scheduler cron job and exit.")
	fs.BoolVar(&c.DryRun, "dry-run", false, "Classify but write nothing to kanban-store.")
	fs.BoolVar(&c.DumpDigest, "dump-digest", false, "Print the digest that would be sent and exit, without calling the model.")
	fs.StringVar(&c.BoardID, "board-id", envOr(lookupEnvironment, "EMAIL_BOARD_ID", ""), "Kanban board id. Pinning the id avoids the name-fork failure; falls back to lookup by -board when empty.")
	fs.StringVar(&c.BoardName, "board", defaultBoardName, "Kanban board name, used only when -board-id is empty.")
	fs.StringVar(&c.KanbanURL, "kanban-url", envOr(lookupEnvironment, "KANBAN_STORE_URL", "http://localhost:8305"), "kanban-store base URL")
	fs.StringVar(&c.MailURL, "mail-url", envOr(lookupEnvironment, "MAILSTACK_URL", "http://localhost:8195"), "mailstack base URL")
	fs.StringVar(&c.MailToken, "mail-token", envOr(lookupEnvironment, "MAILSTACK_TOKEN", "changeme"), "mailstack bearer token")
	fs.StringVar(&c.BridgeURL, "bridge-url", envOr(lookupEnvironment, "LLM_BRIDGE_URL", "http://localhost:8160"), "llm-bridge-server base URL, used to tell whether an agent is still working on a card")
	fs.StringVar(&c.AuthStoreURL, "auth-store-url", envOr(lookupEnvironment, "AUTH_STORE_URL", "http://localhost:8303"), "auth-store base URL")
	fs.StringVar(&c.AuthToken, "auth-token", envOr(lookupEnvironment, "AUTH_STORE_TOKEN", "changeme"), "auth-store bearer token")
	fs.StringVar(&c.Model, "model", envOr(lookupEnvironment, "EMAIL_CLASSIFIER_MODEL", defaultModel), "Anthropic model id")
	fs.IntVar(&c.MaxEmails, "max", maxEmailsPerTick, "Max emails to file per tick.")
	fs.StringVar(&c.OnlyAccount, "account", "", "Restrict to one mailstack account id.")
	fs.BoolVar(&c.Backfill, "backfill", false, "Keep walking past fully-filed pages, for working through mail history.")
	fs.BoolVar(&c.MergePass, "merge", false, "Run the card-merge pass over the board and exit, filing no new mail.")
	fs.BoolVar(&c.BackfillDue, "backfill-due", false, "Recover deadlines from existing card summaries into due_at, re-score, and exit. One-off: cards filed after this store their own date.")
	fs.StringVar(&c.RefileUnreadSince, "refile-unread-since", "", "Unfile mail attached on or after this date (YYYY-MM-DD) that was never fetched or read, and exit. The next ticks classify it. Empty means off.")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	c.SchedulerURL = envOr(lookupEnvironment, "SCHEDULER_URL", "http://localhost:8092")
	c.BinaryPath = filepath.Join(homeDirectory, "bin", "email-classifier")
	return c, nil
}

// validate rejects a configuration that would run, but wrongly.
//
// It is a method rather than a few lines in main for the usual reason — nothing
// in a test can call main() — and it holds exactly one rule, because there is
// exactly one input here that fails silently instead of loudly.
//
// A -max below 1 makes collectUnfiled stop before it reads a single message
// (its budget check is `len(out) >= cfg.MaxEmails`, which is already true at
// zero). The tick then logs "nothing new to file" and exits 0, every 15
// minutes, while mail piles up. Every other bad value in this configuration
// fails at the first HTTP call and says so.
func (c config) validate() error {
	if c.MaxEmails < 1 {
		return fmt.Errorf("-max must be at least 1 (got %d): a tick with no budget files nothing and still reports success", c.MaxEmails)
	}
	return nil
}

func main() {
	os.Exit(run())
}

// run is the whole command. It returns the process exit status rather than
// calling os.Exit itself, so that every early return below settles the run's
// verdict through the same path and a new one cannot forget to.
func run() int {
	// Error ignored exactly as before: an unset HOME leaves this empty and the
	// registered command resolves relative to the working directory, which is a
	// path that fails at exec time either way.
	homeDirectory, _ := os.UserHomeDir()

	cfg, err := parseFlags(os.Args[1:], os.Getenv, homeDirectory)
	if err != nil {
		// Both codes are the ones this binary used before the flag set moved out
		// of main(): flag.ExitOnError exited 2 on a bad argument after printing
		// the error and the usage message, and 0 on -h. Parse has already printed
		// both, so nothing a caller sees moved.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	if err := cfg.validate(); err != nil {
		log.Fatal(err)
	}

	if cfg.RegisterJob {
		if err := registerSchedulerJob(cfg); err != nil {
			log.Fatalf("register: %v", err)
		}
		return 0
	}

	ctx := context.Background()

	// A digest dump never reaches the model, so it must not require the key.
	if !cfg.DumpDigest {
		key, err := resolveAnthropicKey(ctx, cfg)
		if err != nil {
			log.Fatalf("resolve anthropic key: %v", err)
		}
		cfg.AnthropicAPIKey = key
	}

	// Collects the ways this run falls short of doing its whole job. Threaded
	// rather than global so the failure sites stay testable in isolation.
	degraded := &degradations{}

	kc := kanbanclient.New(cfg.KanbanURL)
	board, err := resolveBoard(kc, cfg.BoardID, cfg.BoardName)
	if err != nil {
		log.Fatalf("resolve board: %v", err)
	}
	cols, err := kc.ListColumns(board.ID)
	if err != nil {
		log.Fatalf("list columns: %v", err)
	}
	kc.BoardID = board.ID
	kc.ColumnsByName = map[string]string{}
	for _, col := range cols {
		kc.ColumnsByName[col.Name] = col.ID
	}
	requireColumns(kc, []string{columnNoAction, columnActionNeeded, columnActionCompleted})
	log.Printf("[email] board=%q id=%s columns=%d", board.Name, board.ID, len(cols))

	state, err := loadBoardState(kc, board.ID)
	if err != nil {
		log.Fatalf("load board state: %v", err)
	}
	// One gate per run, shared by the merge pass and the filing pass, so a
	// session is resolved against the bridge at most once per tick however many
	// cards carry it.
	gate := newSessionGate(cfg.BridgeURL)

	if cfg.MergePass {
		// Housekeeping over the board, not over the mail — so it exits here
		// rather than going on to file. It reads no mailbox at all.
		if err := runMergePass(ctx, cfg, kc, state, gate); err != nil {
			log.Fatalf("merge pass: %v", err)
		}
		return degraded.exitCode()
	}
	if cfg.BackfillDue {
		// Same shape as the merge pass, and for the same reason: board
		// housekeeping that reads no mailbox and is not part of a tick.
		if err := runBackfillPass(ctx, cfg, kc, state, gate); err != nil {
			log.Fatalf("backfill: %v", err)
		}
		return degraded.exitCode()
	}
	if cfg.RefileUnreadSince != "" {
		// This one does read the mailbox — it has to fetch each message to find
		// out whether the card holding it ever saw it — but it files nothing, so
		// it exits here with the other passes rather than falling through.
		since, err := time.Parse("2006-01-02", cfg.RefileUnreadSince)
		if err != nil {
			log.Fatalf("-refile-unread-since %q: want YYYY-MM-DD: %v", cfg.RefileUnreadSince, err)
		}
		if err := runRefileUnreadPass(cfg, kc, state, gate, since); err != nil {
			log.Fatalf("refile: %v", err)
		}
		return degraded.exitCode()
	}
	log.Printf("[email] board state: cards=%d filed_emails=%d known_message_ids=%d",
		len(state.Cards), len(state.FiledLocators), len(state.CardsByMessageID))

	// Before any mailbox work, and deliberately before the "nothing new to file"
	// return below. Most ticks file nothing, and deadlines move on those ticks
	// just the same — scoring only when mail happens to arrive is the bug this
	// sweep exists to fix, so it must not sit behind that early exit.
	sweep := rescoreBoard(kc, state, gate, time.Now(), cfg.DryRun)
	log.Printf("[email] sweep: scanned=%d repriced=%d retagged=%d skipped=%d",
		sweep.Scanned, sweep.Repriced, sweep.Retagged, sweep.Skipped)

	mc := newMailClient(cfg.MailURL, cfg.MailToken)
	accounts, err := mc.listAccounts()
	if err != nil {
		log.Fatalf("list mail accounts: %v", err)
	}

	unfiled, err := collectUnfiled(cfg, mc, accounts, state, degraded)
	if err != nil {
		log.Fatalf("collect unfiled: %v", err)
	}
	if len(unfiled) == 0 {
		log.Printf("[email] done: nothing new to file")
		return degraded.exitCode()
	}
	log.Printf("[email] unfiled=%d", len(unfiled))

	// Message identity first: a second copy of a message already filed, and a
	// reply to one, belong where that message went. Both are facts stated in the
	// headers. Everything else is a judgement about what the mail says, and that
	// is the model's job, not a rule's.
	routed, remaining := routeByMessageIdentity(mc, state, unfiled)
	log.Printf("[email] routed by message identity: %d; to model: %d", len(routed), len(remaining))

	applier := &applier{cfg: cfg, kc: kc, state: state, gate: gate}
	if err := applier.applyDirectRoutes(routed); err != nil {
		log.Fatalf("apply direct routes: %v", err)
	}

	for start := 0; start < len(remaining); start += emailsPerModelCall {
		end := start + emailsPerModelCall
		if end > len(remaining) {
			end = len(remaining)
		}
		batch := remaining[start:end]
		if cfg.DumpDigest {
			fmt.Println(buildDigest(state, batch))
			continue
		}
		classifyAndApply(ctx, cfg, applier, state, batch, degraded)
	}

	if cfg.DumpDigest {
		return degraded.exitCode()
	}
	if err := applier.rewriteTouchedCardBodies(); err != nil {
		log.Printf("[email] rewrite card bodies: %v", err)
	}
	log.Printf("[email] done: filed=%d cards_created=%d cards_updated=%d skipped_agent_owned=%d",
		applier.filed, applier.cardsCreated, applier.cardsUpdated, applier.skippedAgentOwned)
	return degraded.exitCode()
}

// classifyAndApply classifies one batch and applies it, halving the batch and
// retrying whichever half fails when the model returns something undecodable.
//
// A plain retry would not work: a decode failure here is deterministic, since
// the same digest reproduces the same malformed reply, so repeating the request
// only spends money to fail identically. What varies is the size of the reply —
// the batches that failed on live mail were the ones with the most new cards to
// describe — so the retry shrinks the reply rather than repeating the request.
//
// Recursion bottoms out at a single email, which is then left unfiled. That is
// the safe end state: an email with no card link is exactly what the next tick
// looks for, so nothing is lost, it is only deferred.
func classifyAndApply(ctx context.Context, cfg config, ap *applier, state *boardState, batch []pendingEmail, degraded *degradations) {
	if len(batch) == 0 {
		return
	}
	report, err := classifyBatch(ctx, cfg, buildDigest(state, batch))
	if err == nil {
		if applyErr := ap.applyReport(batch, report); applyErr != nil {
			log.Printf("[email] apply batch of %d failed: %v", len(batch), applyErr)
		}
		return
	}
	if len(batch) == 1 {
		degraded.record("model could not classify %s, left unfiled", batch[0].Locator)
		log.Printf("[email] classify failed for %s (%q): %v — leaving it unfiled for the next tick",
			batch[0].Locator, batch[0].Meta.Subject, err)
		return
	}
	mid := len(batch) / 2
	log.Printf("[email] classify failed for a batch of %d, splitting into %d + %d and retrying: %v",
		len(batch), mid, len(batch)-mid, err)
	classifyAndApply(ctx, cfg, ap, state, batch[:mid], degraded)
	classifyAndApply(ctx, cfg, ap, state, batch[mid:], degraded)
}

// config is every value this command is configured with: the eighteen flags,
// plus the two pieces of wiring that used to be read from the environment
// inside registerSchedulerJob, plus the resolved API key.
//
// The four base URLs and the two bearer tokens are the half of this struct with
// something to get wrong. They are all strings, and two pairs of them are
// consumed positionally — newMailClient takes (MailURL, MailToken) and
// resolveBoard takes (BoardID, BoardName) — so a swap compiles clean and is read
// by nothing else. Registering each flag directly into its own named field, and
// giving a test a distinct value per variable, is what makes a swap fail.
type config struct {
	// The eighteen flags.
	RegisterJob  bool
	DryRun       bool
	DumpDigest   bool
	BoardID      string
	BoardName    string
	KanbanURL    string
	MailURL      string
	MailToken    string
	BridgeURL    string
	AuthStoreURL string
	AuthToken    string
	Model        string
	MaxEmails    int
	OnlyAccount  string
	Backfill     bool
	MergePass    bool
	BackfillDue  bool
	// RefileUnreadSince is a YYYY-MM-DD date, empty when off. A string rather
	// than a time.Time because it is a flag value: an unparseable date must be
	// refused by name at startup, not silently become the zero time, which would
	// sweep the entire history of the board.
	RefileUnreadSince string

	// The wiring: where -register posts, and what it registers.
	SchedulerURL string
	BinaryPath   string

	// Resolved after parsing, from auth-store rather than from a flag.
	AnthropicAPIKey string
}

// ============================ board resolution ============================

// resolveBoard prefers an explicitly pinned id. kanban-store resolves boards by
// name with auto-create and no unique constraint on the name, so a rename in the
// UI silently forks a second board and every cron writer follows the new one.
// A pinned id that no longer resolves is fatal rather than re-created, because
// creating a replacement is how the fork happens.
func resolveBoard(kc *kanbanclient.Client, boardID, boardName string) (*kanbanclient.Board, error) {
	if boardID != "" {
		boards, err := kc.ListBoards()
		if err != nil {
			return nil, err
		}
		for i := range boards {
			if boards[i].ID == boardID {
				return &boards[i], nil
			}
		}
		return nil, fmt.Errorf("pinned board id %s not found — refusing to create a replacement, "+
			"since a second board with the same name is exactly the failure pinning the id prevents", boardID)
	}
	board, err := kc.FindBoard(boardName)
	if err != nil {
		return nil, err
	}
	if board == nil {
		return nil, fmt.Errorf("board %q not found; create it and pin its id with -board-id/EMAIL_BOARD_ID", boardName)
	}
	log.Printf("[email] WARNING: board resolved by name %q. Pin EMAIL_BOARD_ID=%s so a rename cannot fork the board.",
		boardName, board.ID)
	return board, nil
}

func requireColumns(kc *kanbanclient.Client, names []string) {
	var missing []string
	for _, n := range names {
		if _, ok := kc.ColumnsByName[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		log.Fatalf("board is missing required column(s): %s", strings.Join(missing, ", "))
	}
}

// ============================ board state ============================

// cardState is one card as this command needs it: enough to write a digest, to
// decide whether a sender rule may auto-route to it, and to rewrite its body.
type cardState struct {
	ID         string
	Title      string
	Body       string
	ColumnName string
	Tags       []string
	Category   string
	Action     string
	Urgency    string
	// DueAt is the deadline this card's mail stated, nil when it stated none.
	// The tick re-derives urgency and priority from it, so it is the one field
	// here that keeps meaning something as time passes on its own.
	DueAt *time.Time
	// Priority is the last score written, kept so a sweep can skip cards whose
	// score has not moved rather than PATCHing the whole board every tick.
	Priority int
	// EmailLinks is every email attached to this card, in link order.
	EmailLinks []kanbanclient.CardLink
	// AllLinks is every link of every type. The merge pass needs these to move
	// a card's evidence onto its survivor, and they are already in the board
	// view, so keeping them costs nothing.
	AllLinks []kanbanclient.CardLink
	// Senders is the distinct addresses whose mail lands here. Evidence for the
	// merge pass — two cards fed by the same addresses are candidates for one
	// bucket — and for the never-actionable domain override. Not a routing rule;
	// see CardsBySender.
	Senders []string
}

// boardState is the whole board read once per tick. The card links are the
// watermark: an email with a link has been filed, an email without one has not.
// Keeping that in kanban-store rather than a local file is what stops the record
// of "what is filed" from drifting away from the board a human is looking at.
type boardState struct {
	Cards map[string]*cardState

	// FiledLocators holds every account_id:message_id already attached to a card.
	FiledLocators map[string]bool
	// CardsByMessageID maps an RFC 5322 Message-ID to the cards carrying it.
	// This is what dedups one message that reached two accounts, and what lets a
	// reply join its parent's card.
	CardsByMessageID map[string][]string
	// CardsBySender maps a sender address to the cards its mail has landed on.
	// Evidence only. It tells the merge pass which cards draw on the same
	// sources, and it answers the never-actionable domain override. No mail is
	// routed onto a card because of it: one address is not one topic.
	CardsBySender map[string][]string
}

func loadBoardState(kc *kanbanclient.Client, boardID string) (*boardState, error) {
	view, err := kc.GetBoardView(boardID)
	if err != nil {
		return nil, err
	}
	st := &boardState{
		Cards:            map[string]*cardState{},
		FiledLocators:    map[string]bool{},
		CardsByMessageID: map[string][]string{},
		CardsBySender:    map[string][]string{},
	}
	for _, col := range view.Columns {
		for _, cv := range col.Cards {
			// kanban-store returns orphaned placements with a null item; skipping
			// them here rather than dereferencing is the difference between a
			// logged oddity and a nil-map panic mid-tick.
			if cv.Item == nil {
				log.Printf("[email] WARNING: card %s has no noteboard item (orphaned placement); skipping",
					cv.Placement.CardID)
				continue
			}
			card := &cardState{
				ID:         cv.Placement.CardID,
				Title:      stringField(cv.Item, "title"),
				Body:       stringField(cv.Item, "body"),
				ColumnName: col.Column.Name,
				Tags:       stringSliceField(cv.Item, "tags"),
			}
			card.Category = tagValue(card.Tags, tagPrefixCat)
			card.Action = tagValue(card.Tags, tagPrefixAction)
			card.Urgency = tagValue(card.Tags, tagPrefixUrgent)
			card.Priority = intField(cv.Item, "priority")
			card.DueAt = timeField(cv.Item, "due_at", card.ID)

			card.AllLinks = append(card.AllLinks, cv.Links...)
			for _, link := range cv.Links {
				switch link.EntityType {
				case linkTypeEmail:
					st.FiledLocators[link.EntityRef] = true
					card.EmailLinks = append(card.EmailLinks, link)
				case linkTypeEmailMessageID:
					st.CardsByMessageID[link.EntityRef] = append(st.CardsByMessageID[link.EntityRef], card.ID)
				case linkTypeEmailSender:
					st.CardsBySender[link.EntityRef] = append(st.CardsBySender[link.EntityRef], card.ID)
					card.Senders = append(card.Senders, link.EntityRef)
				}
			}
			st.Cards[card.ID] = card
		}
	}
	return st, nil
}

func stringField(item map[string]any, key string) string {
	v, _ := item[key].(string)
	return v
}

// intField reads a numeric noteboard field. JSON decoding gives every number as
// a float64, so the assertion has to be against that rather than int.
func intField(item map[string]any, key string) int {
	v, _ := item[key].(float64)
	return int(v)
}

// timeField reads an RFC3339 noteboard timestamp, nil when the field is absent
// or empty. A present-but-unparseable value is logged rather than swallowed:
// noteboard writes these itself, so a malformed one means something upstream is
// wrong and silently treating it as "no deadline" would score the card as if it
// never had one.
func timeField(item map[string]any, key, cardID string) *time.Time {
	raw, ok := item[key].(string)
	if !ok || raw == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		log.Printf("[email] WARNING: card %s has unparseable %s %q: %v", cardID, key, raw, err)
		return nil
	}
	return &t
}

func stringSliceField(item map[string]any, key string) []string {
	raw, ok := item[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// tagValue returns the value part of the first tag carrying the given prefix.
func tagValue(tags []string, prefix string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, prefix) {
			return strings.TrimPrefix(t, prefix)
		}
	}
	return ""
}

// ============================ candidate selection ============================

// pendingEmail is a message that has no card link yet, carried through routing
// and enrichment. Full is nil until the message is actually fetched, which only
// happens for mail the cheap paths could not place.
type pendingEmail struct {
	Meta      messageMeta
	Locator   string
	MessageID string
	Full      *fullMessage
}

// collectUnfiled walks each account newest-first and returns messages with no
// card link. In steady state the newest page is almost entirely filed already,
// so the walk stops on the first fully-filed page and the tick costs one
// mailstack page per account.
func collectUnfiled(cfg config, mc *mailClient, accounts []mailAccount, state *boardState, degraded *degradations) ([]pendingEmail, error) {
	var out []pendingEmail
	for _, acct := range accounts {
		if !acct.Enabled {
			log.Printf("[email] account %s disabled; skipping", acct.ID)
			continue
		}
		if cfg.OnlyAccount != "" && acct.ID != cfg.OnlyAccount {
			continue
		}
		folder, err := mc.inboxFolder(acct.ID)
		if err != nil {
			// One unreachable account must not stop the others: a broken IMAP
			// connection should not block Gmail triage.
			log.Printf("[email] account %s: %v — skipping this account", acct.ID, err)
			degraded.record("account %s unreachable, filed nothing from it", acct.ID)
			continue
		}

		cursor := ""
		for page := 0; page < scanPagesPerAccount; page++ {
			if len(out) >= cfg.MaxEmails {
				break
			}
			metas, next, err := mc.listMessages(acct.ID, folder, cursor, mailPageSize)
			if err != nil {
				log.Printf("[email] account %s page %d: %v — skipping the rest of this account", acct.ID, page, err)
				degraded.record("account %s scanned only %d page(s) before erroring", acct.ID, page)
				break
			}
			if len(metas) == 0 {
				break
			}
			newOnPage := 0
			for _, meta := range metas {
				loc := locatorOf(acct.ID, meta.ID)
				if state.FiledLocators[loc] {
					continue
				}
				newOnPage++
				if len(out) >= cfg.MaxEmails {
					continue
				}
				out = append(out, pendingEmail{Meta: meta, Locator: loc})
			}
			// A page with nothing new means we have caught up with history.
			// Backfill overrides this to keep walking into the archive.
			if newOnPage == 0 && !cfg.Backfill {
				break
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
	// Oldest first, so a conversation's earlier message creates the card and the
	// later ones attach to it rather than racing to invent competing cards.
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Date.Before(out[j].Meta.Date) })
	return out, nil
}

// ============================ affinity routing ============================

// directRoute is an email placed on cards without consulting the model.
type directRoute struct {
	Email   pendingEmail
	CardIDs []string
	Reason  string
	Label   string
}

// routeByMessageIdentity places the mail whose card is settled by its RFC 5322
// headers rather than by what it says: a second copy of a message already filed,
// and a reply to one. Everything it cannot place that way is returned for the
// model to read.
//
// There was a third and cheaper path here, and it was wrong. A learned rule sent
// a sender's mail to whichever bucket card their previous message landed on,
// without fetching the message or reading a word of it. One address is not one
// topic: wellsfargoonline@mail1.wellsfargo.com sends a marketing blast and an
// address-change alert from the same mailbox, and the rule filed both onto
// "Wells Fargo Deals eligibility update" unread. The rule also grew, because
// every bucket filing taught another one — by the tenth day the board carried
// 103 of them and roughly seven of every eight messages reached a card without
// being classified at all.
//
// Dropping it costs one message fetch and a share of a batched model call per
// email, on a mailbox taking about thirty a day. It buys two things: every
// message is read before it is filed, and every message now has its Message-ID
// recorded. That second one matters here — the two paths below run on
// Message-IDs, and the sender rule was starving them by filing mail it never
// fetched, leaving 228 identities recorded against 645 filed messages.
func routeByMessageIdentity(mc *mailClient, state *boardState, emails []pendingEmail) ([]directRoute, []pendingEmail) {
	var routed []directRoute
	var remaining []pendingEmail

	// Every path from here needs the message itself — for Message-ID, for the
	// reply chain, and for the body the model reads.
	for i := range emails {
		em := emails[i]
		full, err := mc.getMessage(em.Meta.AccountID, em.Meta.ID)
		if err != nil {
			log.Printf("[email] fetch %s: %v — filing on headers alone", em.Locator, err)
			remaining = append(remaining, em)
			continue
		}
		em.Full = full
		em.MessageID = messageIDOf(full)

		// The same message reaching two accounts is one message. File the new
		// locator onto the cards the first copy already sits on.
		if em.MessageID != "" {
			if cards := state.CardsByMessageID[em.MessageID]; len(cards) > 0 {
				routed = append(routed, directRoute{
					Email:   em,
					CardIDs: cards,
					Reason:  "duplicate of an already-filed Message-ID",
					Label:   emailLinkLabel(em.Meta, ""),
				})
				continue
			}
		}
		// A reply belongs where its parent was filed.
		if cards := threadCards(state, full); len(cards) > 0 {
			routed = append(routed, directRoute{
				Email:   em,
				CardIDs: cards,
				Reason:  "reply in an already-filed thread",
				Label:   emailLinkLabel(em.Meta, ""),
			})
			continue
		}
		remaining = append(remaining, em)
	}
	return routed, remaining
}

// threadCards returns the cards holding any message this one references.
func threadCards(state *boardState, full *fullMessage) []string {
	seen := map[string]bool{}
	var out []string
	for _, ref := range referencedMessageIDs(full) {
		for _, cardID := range state.CardsByMessageID[ref] {
			if seen[cardID] {
				continue
			}
			seen[cardID] = true
			out = append(out, cardID)
		}
	}
	return out
}

// emailLinkLabel renders the short human description stored on a card link, so
// the evidence list on a card distinguishes its messages at a glance.
func emailLinkLabel(meta messageMeta, modelLabel string) string {
	if modelLabel != "" {
		return truncate(modelLabel, 50)
	}
	sender := meta.From.Name
	if sender == "" {
		sender = meta.From.Email
	}
	return truncate(fmt.Sprintf("%s · %s", meta.Date.Format("Jan 2"), sender), 50)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// ============================ digest ============================

// buildDigest renders the prompt: the cards that already exist, then the emails
// to file. Card identity comes first and is never dropped, because an incomplete
// picture of existing cards is what makes a classifier invent duplicates of work
// it is already tracking.
func buildDigest(state *boardState, batch []pendingEmail) string {
	var b strings.Builder

	// The model is asked to resolve relative deadlines ("tomorrow", "by Friday")
	// into absolute dates, which it cannot do without knowing what day it is.
	// Each email carries its own date below, but those are send dates and a
	// batch routinely spans several days.
	fmt.Fprintf(&b, "[TODAY IS %s]\n\n", time.Now().Format("2006-01-02 (Monday)"))

	b.WriteString("[EXISTING CARDS ON THE EMAIL BOARD]\n")
	if len(state.Cards) == 0 {
		b.WriteString("(none yet — this is the first pass, so every email needs a new card)\n")
	}
	cards := make([]*cardState, 0, len(state.Cards))
	for _, c := range state.Cards {
		cards = append(cards, c)
	}
	// Stable order, and buckets first: the model's most common correct answer is
	// "this belongs on an existing bucket", so those are what it should read first.
	sort.Slice(cards, func(i, j int) bool {
		if (cards[i].ColumnName == columnNoAction) != (cards[j].ColumnName == columnNoAction) {
			return cards[i].ColumnName == columnNoAction
		}
		return cards[i].Title < cards[j].Title
	})
	for _, c := range cards {
		due := ""
		if c.DueAt != nil {
			due = " due=" + c.DueAt.Format("2006-01-02")
		}
		fmt.Fprintf(&b, "- card_id=%s | %s | column=%s | category=%s action=%s urgency=%s%s | emails_attached=%d\n",
			c.ID, c.Title, c.ColumnName, c.Category, c.Action, c.Urgency, due, len(c.EmailLinks))
	}

	b.WriteString("\n[EMAILS TO FILE — file every one of these]\n")
	perEmail := digestCharBudget
	if len(batch) > 0 {
		perEmail = (digestCharBudget - b.Len()) / len(batch)
	}
	if perEmail > bodyCharsPerEmail+400 {
		perEmail = bodyCharsPerEmail + 400
	}
	for _, em := range batch {
		fmt.Fprintf(&b, "\n--- email_ref=%s\n", em.Locator)
		fmt.Fprintf(&b, "date: %s\n", em.Meta.Date.Format(time.RFC3339))
		fmt.Fprintf(&b, "from: %s <%s>\n", em.Meta.From.Name, em.Meta.From.Email)
		fmt.Fprintf(&b, "subject: %s\n", unescapeCommonEntities(em.Meta.Subject))
		if em.Meta.Unread {
			b.WriteString("state: unread\n")
		}
		fmt.Fprintf(&b, "body: %s\n", truncate(bestBodyText(em), perEmail))
	}
	return b.String()
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// bestBodyText picks the most informative rendering of a message and cleans it.
//
// The plain-text part is preferred, but it is not always the richer one: senders
// that build HTML-first mail often ship a text part that is a single "view this
// in your browser" line, so an HTML part with substantially more prose after
// cleaning wins. The snippet is the floor, used when the message could not be
// fetched at all and is being filed on its headers alone.
func bestBodyText(em pendingEmail) string {
	if em.Full == nil {
		return cleanBodyText(em.Meta.Snippet)
	}
	fromText := cleanBodyText(em.Full.BodyText)
	fromHTML := cleanBodyText(em.Full.BodyHTML)
	// The HTML part wins only when the text part is too short to be a real
	// message. A ratio test was tried first and was wrong: marketing HTML stays
	// several times longer than its text part even after cleaning, so the ratio
	// replaced perfectly good prose with markup residue.
	if len(fromText) < stubBodyTextLimit && len(fromHTML) > len(fromText) {
		return fromHTML
	}
	if fromText != "" {
		return fromText
	}
	if fromHTML != "" {
		return fromHTML
	}
	return cleanBodyText(em.Meta.Snippet)
}

// ============================ misc ============================

func resolveAnthropicKey(ctx context.Context, cfg config) (string, error) {
	u := strings.TrimRight(cfg.AuthStoreURL, "/") + "/api/resolve/anthropic"
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req.Header.Set("X-Auth-App", "email-classifier")
	req.Header.Set("X-Auth-Reason", "file-email-onto-kanban-cards")
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth-store resolve: %s — %s", resp.Status, string(body))
	}
	var r struct {
		APIKey      string `json:"api_key"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("decode resolve: %w", err)
	}
	if r.APIKey != "" {
		return r.APIKey, nil
	}
	if r.AccessToken != "" {
		return r.AccessToken, nil
	}
	return "", fmt.Errorf("auth-store: anthropic credential has no api_key or access_token")
}

// registeredCommand renders the shell command the scheduler job runs.
//
// The board id is baked into the command rather than left to the environment:
// scheduler runs jobs as `sh -c` with only its own thin env, so EMAIL_BOARD_ID
// would not reach the process. It has to be right at creation too — the
// scheduler binary serving :8092 accepts only enabled, description and
// timeout_seconds on PATCH and silently drops `command` with a 200, so there is
// no second chance to correct it through the API.
//
// A pure function rather than a few lines inside the POST, because this string
// is the whole point of -register and it is written exactly once, by hand, on a
// host nobody re-checks.
func registeredCommand(binaryPath, boardID string) string {
	if boardID == "" {
		return binaryPath
	}
	return binaryPath + " -board-id " + boardID
}

// schedulerJobPayload is the job -register creates.
func schedulerJobPayload(cfg config) map[string]any {
	return map[string]any{
		"name":        schedulerJobName,
		"description": "Files incoming mail onto cards on the Email kanban board (cards are work, emails attach as links).",
		"schedule":    schedulerJobSchedule,
		"type":        "shell",
		"command":     registeredCommand(cfg.BinaryPath, cfg.BoardID),
		// A tick fetches messages one live provider round trip at a time, so it
		// legitimately outruns the scheduler's 5-minute default.
		"timeout_seconds": schedulerJobTimeoutSeconds,
	}
}

func registerSchedulerJob(cfg config) error {
	schedURL := cfg.SchedulerURL
	if cfg.BoardID == "" {
		log.Printf("WARNING: registering without a pinned board id. Set EMAIL_BOARD_ID and re-register, " +
			"or the job resolves the board by name and a rename will fork it.")
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
		if j.Name == schedulerJobName {
			fmt.Println(schedulerJobName + " already registered; no changes made")
			return nil
		}
	}
	data, _ := json.Marshal(schedulerJobPayload(cfg))
	req, err := http.NewRequest("POST", schedURL+"/api/jobs", strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST /api/jobs: %w", err)
	}
	defer resp2.Body.Close()
	body, _ = io.ReadAll(resp2.Body)
	if resp2.StatusCode >= 300 {
		return fmt.Errorf("scheduler returned %d: %s", resp2.StatusCode, string(body))
	}
	fmt.Printf("registered %s: %s\n", schedulerJobName, strings.TrimSpace(string(body)))
	return nil
}

// envOr takes the lookup as an argument for the same reason homeDirectory is
// passed in: eight of the flag defaults are read at flag-registration time, so a
// test that exported one of these variables to observe it would be racing every
// other test in the package for one process-wide variable.
func envOr(lookupEnvironment environmentLookup, key, fallback string) string {
	if v := lookupEnvironment(key); v != "" {
		return v
	}
	return fallback
}
