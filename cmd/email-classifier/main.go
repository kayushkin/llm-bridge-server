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

	defaultModel = "claude-haiku-4-5-20251001"

	// emailsPerModelCall bounds one batch. Emails are small, so batching is far
	// cheaper than a call each; the ceiling exists so one report stays inside
	// maxOutputTokens with room to spare.
	emailsPerModelCall = 25

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
)

func main() {
	var (
		registerFlag = flag.Bool("register", false, "Register this binary as a scheduler cron job and exit.")
		dryRun       = flag.Bool("dry-run", false, "Classify but write nothing to kanban-store.")
		dumpDigest   = flag.Bool("dump-digest", false, "Print the digest that would be sent and exit, without calling the model.")
		boardID      = flag.String("board-id", envOr("EMAIL_BOARD_ID", ""), "Kanban board id. Pinning the id avoids the name-fork failure; falls back to lookup by -board when empty.")
		boardName    = flag.String("board", defaultBoardName, "Kanban board name, used only when -board-id is empty.")
		kanbanURL    = flag.String("kanban-url", envOr("KANBAN_STORE_URL", "http://localhost:8305"), "kanban-store base URL")
		mailURL      = flag.String("mail-url", envOr("MAILSTACK_URL", "http://localhost:8195"), "mailstack base URL")
		mailToken    = flag.String("mail-token", envOr("MAILSTACK_TOKEN", "changeme"), "mailstack bearer token")
		authStoreURL = flag.String("auth-store-url", envOr("AUTH_STORE_URL", "http://localhost:8303"), "auth-store base URL")
		authToken    = flag.String("auth-token", envOr("AUTH_STORE_TOKEN", "changeme"), "auth-store bearer token")
		model        = flag.String("model", envOr("EMAIL_CLASSIFIER_MODEL", defaultModel), "Anthropic model id")
		maxEmails    = flag.Int("max", maxEmailsPerTick, "Max emails to file per tick.")
		account      = flag.String("account", "", "Restrict to one mailstack account id.")
		backfill     = flag.Bool("backfill", false, "Keep walking past fully-filed pages, for working through mail history.")
	)
	flag.Parse()

	if *registerFlag {
		if err := registerSchedulerJob(); err != nil {
			log.Fatalf("register: %v", err)
		}
		return
	}

	ctx := context.Background()
	cfg := config{
		KanbanURL:    *kanbanURL,
		MailURL:      *mailURL,
		MailToken:    *mailToken,
		AuthStoreURL: *authStoreURL,
		AuthToken:    *authToken,
		Model:        *model,
		DryRun:       *dryRun,
		DumpDigest:   *dumpDigest,
		Backfill:     *backfill,
		MaxEmails:    *maxEmails,
		OnlyAccount:  *account,
	}

	// A digest dump never reaches the model, so it must not require the key.
	if !cfg.DumpDigest {
		key, err := resolveAnthropicKey(ctx, cfg)
		if err != nil {
			log.Fatalf("resolve anthropic key: %v", err)
		}
		cfg.AnthropicAPIKey = key
	}

	kc := kanbanclient.New(cfg.KanbanURL)
	board, err := resolveBoard(kc, *boardID, *boardName)
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
	log.Printf("[email] board state: cards=%d filed_emails=%d known_message_ids=%d sender_rules=%d",
		len(state.Cards), len(state.FiledLocators), len(state.CardsByMessageID), state.routableSenderCount())

	mc := newMailClient(cfg.MailURL, cfg.MailToken)
	accounts, err := mc.listAccounts()
	if err != nil {
		log.Fatalf("list mail accounts: %v", err)
	}

	unfiled, err := collectUnfiled(cfg, mc, accounts, state)
	if err != nil {
		log.Fatalf("collect unfiled: %v", err)
	}
	if len(unfiled) == 0 {
		log.Printf("[email] done: nothing new to file")
		return
	}
	log.Printf("[email] unfiled=%d", len(unfiled))

	// Cheap path first: anything a learned sender rule or an existing thread can
	// place needs neither a message fetch nor a model call.
	routed, remaining := routeByAffinity(cfg, mc, state, unfiled)
	log.Printf("[email] routed without model: %d (sender/thread affinity); to model: %d", len(routed), len(remaining))

	applier := &applier{cfg: cfg, kc: kc, state: state}
	if err := applier.applyDirectRoutes(routed); err != nil {
		log.Fatalf("apply direct routes: %v", err)
	}

	for start := 0; start < len(remaining); start += emailsPerModelCall {
		end := start + emailsPerModelCall
		if end > len(remaining) {
			end = len(remaining)
		}
		batch := remaining[start:end]
		digest := buildDigest(state, batch)

		if cfg.DumpDigest {
			fmt.Println(digest)
			continue
		}
		report, err := classifyBatch(ctx, cfg, digest)
		if err != nil {
			// One bad batch must not abort the tick, and it must not be recorded
			// as filed — leaving these emails unlinked is precisely what makes
			// the next tick retry them.
			log.Printf("[email] classify batch %d-%d failed: %v — leaving %d emails unfiled for the next tick",
				start, end, err, len(batch))
			continue
		}
		if err := applier.applyReport(batch, report); err != nil {
			log.Printf("[email] apply batch %d-%d failed: %v", start, end, err)
		}
	}

	if cfg.DumpDigest {
		return
	}
	if err := applier.rewriteTouchedCardBodies(); err != nil {
		log.Printf("[email] rewrite card bodies: %v", err)
	}
	log.Printf("[email] done: filed=%d cards_created=%d cards_updated=%d",
		applier.filed, applier.cardsCreated, applier.cardsUpdated)
}

type config struct {
	KanbanURL       string
	MailURL         string
	MailToken       string
	AuthStoreURL    string
	AuthToken       string
	AnthropicAPIKey string
	Model           string
	DryRun          bool
	DumpDigest      bool
	Backfill        bool
	MaxEmails       int
	OnlyAccount     string
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
	// EmailLinks is every email attached to this card, in link order.
	EmailLinks []kanbanclient.CardLink
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
	CardsBySender map[string][]string
}

func (s *boardState) routableSenderCount() int {
	n := 0
	for sender := range s.CardsBySender {
		if s.routeForSender(sender) != "" {
			n++
		}
	}
	return n
}

// routeForSender returns the card a sender's mail may be auto-filed onto without
// a model call, or "" when no rule applies.
//
// It deliberately only fires for a single unambiguous bucket card in No action.
// A sender whose mail has landed on two cards is genuinely ambiguous, and a
// sender whose card needs action is exactly the mail that must not be filed
// blind — google.com sends both a Play Store receipt and a security alert.
func (s *boardState) routeForSender(sender string) string {
	ids := s.CardsBySender[sender]
	if len(ids) != 1 {
		return ""
	}
	card, ok := s.Cards[ids[0]]
	if !ok || card.ColumnName != columnNoAction {
		return ""
	}
	return card.ID
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

			for _, link := range cv.Links {
				switch link.EntityType {
				case linkTypeEmail:
					st.FiledLocators[link.EntityRef] = true
					card.EmailLinks = append(card.EmailLinks, link)
				case linkTypeEmailMessageID:
					st.CardsByMessageID[link.EntityRef] = append(st.CardsByMessageID[link.EntityRef], card.ID)
				case linkTypeEmailSender:
					st.CardsBySender[link.EntityRef] = append(st.CardsBySender[link.EntityRef], card.ID)
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
func collectUnfiled(cfg config, mc *mailClient, accounts []mailAccount, state *boardState) ([]pendingEmail, error) {
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

// routeByAffinity applies the two cheap placements and returns what is left for
// the model. Sender affinity is checked before any message fetch, so bulk mail
// costs nothing beyond the page listing that surfaced it.
func routeByAffinity(cfg config, mc *mailClient, state *boardState, emails []pendingEmail) ([]directRoute, []pendingEmail) {
	var routed []directRoute
	var remaining []pendingEmail

	for _, em := range emails {
		if cardID := state.routeForSender(senderKey(em.Meta.From)); cardID != "" {
			routed = append(routed, directRoute{
				Email:   em,
				CardIDs: []string{cardID},
				Reason:  "sender affinity",
				Label:   emailLinkLabel(em.Meta, ""),
			})
			continue
		}
		remaining = append(remaining, em)
	}

	// Everything past this point needs the message itself — for Message-ID, for
	// the reply chain, and for the body the model reads.
	var stillRemaining []pendingEmail
	for i := range remaining {
		em := remaining[i]
		full, err := mc.getMessage(em.Meta.AccountID, em.Meta.ID)
		if err != nil {
			log.Printf("[email] fetch %s: %v — filing on headers alone", em.Locator, err)
			stillRemaining = append(stillRemaining, em)
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
		stillRemaining = append(stillRemaining, em)
	}
	return routed, stillRemaining
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
		fmt.Fprintf(&b, "- card_id=%s | %s | column=%s | category=%s action=%s urgency=%s | emails_attached=%d\n",
			c.ID, c.Title, c.ColumnName, c.Category, c.Action, c.Urgency, len(c.EmailLinks))
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
// cleaning wins. The snippet is the floor, used when the message was never
// fetched — which is every message routed by sender affinity.
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

func registerSchedulerJob() error {
	schedURL := envOr("SCHEDULER_URL", "http://localhost:8092")
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, "bin", "email-classifier")
	const jobName = "email-classifier-tick"

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
		if j.Name == jobName {
			fmt.Println(jobName + " already registered; no changes made")
			return nil
		}
	}
	payload := map[string]any{
		"name":        jobName,
		"description": "Files incoming mail onto cards on the Email kanban board (cards are work, emails attach as links).",
		"schedule":    "*/15 * * * *",
		"type":        "shell",
		"command":     bin,
		// A tick fetches messages one live provider round trip at a time, so it
		// legitimately outruns the scheduler's 5-minute default.
		"timeout_seconds": 900,
	}
	data, _ := json.Marshal(payload)
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
	fmt.Printf("registered %s: %s\n", jobName, strings.TrimSpace(string(body)))
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
