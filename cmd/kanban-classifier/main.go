// kanban-classifier is the cheap-model curator that creates and updates
// kanban cards on the "Agent runs" board for any active llm-bridge session.
//
// Cadence:
//
//   Registered as a scheduler shell job running every 15 minutes. On each
//   tick, walks bridge-server's /sessions list, filters down to recently-
//   updated candidates, skips ones whose check signature matches the
//   already-recorded one (no-change short-circuit), and classifies the rest
//   with Anthropic Haiku.
//
// Decisions per session:
//
//   - trackable=false → set kanban:do-not-track tag on the session, never
//     classify again unless message_count grows by ≥20 messages.
//   - existing_cards updates → move card to matching column on the Agent
//     runs board, append summary to body. Bridge state has NO automatic
//     mapping to columns; the model owns column placement.
//   - new_tasks → create a new card per task on the board, link to the
//     session, land in the column matching the model's reported state.
//
// Signature is stored as a single entity_tag on session/<bridge_id>:
//
//     kanban-check-sig:msg=<n>:state=<s>:t=<YYYYMMDDhhmm>
//
// Old sigs are deleted before a new one is written. A session reaching a
// terminal bridge state (completed/error) is marked with
// kanban-final-classified after the next classify and never reprocessed.
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
	"sort"
	"strings"
	"time"

	"github.com/slava/scheduler/internal/kanbanclient"
)

const (
	defaultBoardName       = "Agent runs"
	tagDoNotTrack          = "kanban-do-not-track"
	tagFinalClassified     = "kanban-final-classified"
	tagCheckSigPrefix      = "kanban-check-sig:"
	cardLinkSession        = "session"
	cardLinkSource         = "kanban-source"
	cardSourceClassifier   = "classifier"
	maxClassifyPerTick     = 10
	// digestCharBudget is the size of one classifier prompt in characters
	// (~4 chars/token, so ~4k tokens). Characters, not tokens, because that
	// is what the builder can actually measure.
	digestCharBudget = 16000
	// Ceiling on the digest share the already-filed cards may take, and share
	// of what is left after that for recent assistant turns. The rest goes to
	// user prompts. The cards section only ever consumes what it needs, so a
	// generous ceiling costs nothing on a session with few cards and buys
	// complete card identity on one with many — which is exactly the session
	// that duplicates.
	existingCardsBudgetShare  = 0.55
	assistantTurnsBudgetShare = 0.35
	terminalStateCompleted = "completed"
	terminalStateError     = "error"
	defaultModel           = "claude-haiku-4-5-20251001"
	candidateLookback      = 7 * 24 * time.Hour
	doNotTrackReevalDelta  = 20 // re-evaluate skipped sessions only after this many new messages
)

func main() {
	var (
		registerFlag = flag.Bool("register", false, "Register/upsert this binary as a scheduler cron job and exit.")
		dryRun       = flag.Bool("dry-run", false, "Classify but do not write to kanban-store.")
		oneSession   = flag.String("session", "", "Classify exactly one session id and exit (no candidate filtering).")
		boardName    = flag.String("board", defaultBoardName, "Kanban board name to write cards to.")
		kanbanURL    = flag.String("kanban-url", envOr("KANBAN_STORE_URL", "http://localhost:8305"), "kanban-store base URL")
		bridgeURL    = flag.String("bridge-url", envOr("LLM_BRIDGE_URL", "http://localhost:8160"), "llm-bridge-server base URL")
		authStoreURL = flag.String("auth-store-url", envOr("AUTH_STORE_URL", "http://localhost:8303"), "auth-store base URL")
		authToken    = flag.String("auth-token", envOr("AUTH_STORE_TOKEN", "changeme"), "auth-store bearer token")
		model        = flag.String("model", envOr("KANBAN_CLASSIFIER_MODEL", defaultModel), "Anthropic model id")
		maxClassify  = flag.Int("max", maxClassifyPerTick, "Max classifications per tick.")
		digestBudget = flag.Int("digest-budget", digestCharBudget, "Digest size in characters (~4 chars per token).")
		dumpDigest   = flag.Bool("dump-digest", false, "With -session: print the digest that would be sent and exit, without calling the model.")
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
		BridgeURL:    *bridgeURL,
		AuthStoreURL: *authStoreURL,
		AuthToken:    *authToken,
		Model:        *model,
		BoardName:    *boardName,
		DryRun:       *dryRun,
		DigestBudget: *digestBudget,
		DumpDigest:   *dumpDigest,

		ExplicitSession: *oneSession != "",
	}

	// A digest dump never reaches the model, so it must not need its key.
	if !cfg.DumpDigest {
		apiKey, err := resolveAnthropicKey(ctx, cfg)
		if err != nil {
			log.Fatalf("resolve anthropic key: %v", err)
		}
		cfg.AnthropicAPIKey = apiKey
	}

	kc := kanbanclient.New(cfg.KanbanURL)
	board, err := kc.FindBoard(cfg.BoardName)
	if err != nil {
		log.Fatalf("find board %q: %v", cfg.BoardName, err)
	}
	if board == nil {
		// First-ever invocation: bootstrap the board so cards have somewhere to go.
		if err := kc.EnsureBoard(cfg.BoardName, []string{"Queued", "In Progress", "Blocked", "Done", "Failed"}); err != nil {
			log.Fatalf("bootstrap board: %v", err)
		}
		board = &kanbanclient.Board{ID: kc.BoardID, Name: cfg.BoardName}
	} else {
		// Cache columns onto the client.
		cols, err := kc.ListColumns(board.ID)
		if err != nil {
			log.Fatalf("list columns: %v", err)
		}
		kc.BoardID = board.ID
		kc.ColumnsByName = map[string]string{}
		for _, col := range cols {
			kc.ColumnsByName[col.Name] = col.ID
		}
	}
	requireColumns(kc, []string{"In Progress", "Blocked", "Done", "Failed"})

	if *oneSession != "" {
		sess, err := getSession(cfg.BridgeURL, *oneSession)
		if err != nil {
			log.Fatalf("get session %s: %v", *oneSession, err)
		}
		processSession(ctx, cfg, kc, sess)
		return
	}

	candidates, err := selectCandidates(cfg)
	if err != nil {
		log.Fatalf("select candidates: %v", err)
	}
	log.Printf("[classifier] candidates=%d", len(candidates))

	classified := 0
	for _, sess := range candidates {
		if classified >= *maxClassify {
			log.Printf("[classifier] hit per-tick cap (%d); deferring %d sessions to next tick",
				*maxClassify, len(candidates)-classified)
			break
		}
		ran := processSession(ctx, cfg, kc, sess)
		if ran {
			classified++
		}
	}
	log.Printf("[classifier] done: classified=%d", classified)
}

type config struct {
	KanbanURL       string
	BridgeURL       string
	AuthStoreURL    string
	AuthToken       string
	AnthropicAPIKey string
	Model           string
	BoardName       string
	DryRun          bool
	DigestBudget    int
	DumpDigest      bool

	// ExplicitSession is set when an operator named a single session on the
	// command line. The dedupe gates below exist to ration the cron sweep, not
	// to overrule a human who asked for this exact session by id, so they are
	// bypassed — loudly — when this is set.
	ExplicitSession bool
}

// ============================ Candidate selection ============================

type session struct {
	SessionID   string `json:"session_id"`
	State       string `json:"state"`
	Purpose     string `json:"purpose"`
	Origin      string `json:"origin"`
	DisplayName string `json:"display_name"`
	Harness     string `json:"harness"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func selectCandidates(cfg config) ([]session, error) {
	all, err := listSessions(cfg.BridgeURL)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-candidateLookback)
	var out []session
	for _, s := range all {
		t, err := time.Parse(time.RFC3339, s.UpdatedAt)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			continue
		}
		out = append(out, s)
	}
	// Most recently updated first — likely-active wins the per-tick budget.
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out, nil
}

// ============================ Process one session ============================

// processSession runs the full pipeline for one session. Returns true if a
// classifier call was made (vs. dedupe-skipped or final-classified).
func processSession(ctx context.Context, cfg config, kc *kanbanclient.Client, sess session) bool {
	tags, err := kc.ListEntityTags(cardLinkSession, sess.SessionID)
	if err != nil {
		log.Printf("[%s] list tags: %v", sess.SessionID, err)
		return false
	}
	tagSet := tagSet(tags)

	// Permanent skip: previously classified as terminal.
	if tagSet[tagFinalClassified] {
		if !cfg.ExplicitSession {
			return false
		}
		log.Printf("[%s] already final-classified — proceeding anyway because this session was named explicitly",
			sess.SessionID)
	}

	msgs, err := getSessionMessages(cfg.BridgeURL, sess.SessionID)
	if err != nil {
		log.Printf("[%s] get messages: %v", sess.SessionID, err)
		return false
	}
	msgCount := len(msgs)

	// do-not-track: only re-evaluate after a big delta.
	if tagSet[tagDoNotTrack] {
		priorN := priorMsgCount(tagSet)
		if msgCount-priorN < doNotTrackReevalDelta {
			if !cfg.ExplicitSession {
				return false
			}
			log.Printf("[%s] do-not-track and Δmsg=%d < %d — proceeding anyway because this session was named explicitly",
				sess.SessionID, msgCount-priorN, doNotTrackReevalDelta)
		} else {
			log.Printf("[%s] do-not-track but Δmsg=%d ≥ %d — re-evaluating",
				sess.SessionID, msgCount-priorN, doNotTrackReevalDelta)
		}
	}

	// Compute current signature; bail if unchanged.
	curSig := buildSig(msgCount, sess.State, sess.UpdatedAt)
	priorSig := priorSigTag(tagSet)
	if priorSig == curSig {
		if !cfg.ExplicitSession {
			return false
		}
		log.Printf("[%s] signature unchanged (%s) — proceeding anyway because this session was named explicitly",
			sess.SessionID, curSig)
	}

	// Look up existing cards already linked to this session.
	cards, err := kc.ListCardsForEntity(cardLinkSession, sess.SessionID)
	if err != nil {
		log.Printf("[%s] list cards: %v", sess.SessionID, err)
		return false
	}

	digest := buildDigest(sess, msgs, cards, cfg.DigestBudget)
	if cfg.DumpDigest {
		fmt.Printf("%s\n", digest)
		log.Printf("[%s] dump-digest: %d chars, %d existing card(s)", sess.SessionID, len(digest), len(cards))
		return false
	}
	report, err := classify(ctx, cfg, digest)
	if errors.Is(err, errTruncatedReport) {
		// Acting on a truncated report is worse than not acting: its missing
		// existing_cards look like "no opinion" (so cards silently stay in the
		// column they were first filed in) and any new_tasks it did manage to
		// emit get filed against a list of existing cards we know is short, so
		// the dedupe in applyNew is running blind. Refuse the whole report.
		//
		// The signature is still written. The input is deterministic, so a
		// retry would truncate identically and bill for it every tick; the
		// session is instead left alone until it actually changes.
		log.Printf("[%s] WARNING: %d existing card(s) NOT updated — %v. "+
			"This session needs more than max_tokens=%d to classify; raise it or narrow what the model must report "+
			"(see noteboard todo 55aab37a). Re-run: kanban-classifier -session %s -dry-run",
			sess.SessionID, len(cards), err, maxOutputTokens, sess.SessionID)
		if !cfg.DryRun {
			writeSig(kc, sess, tagSet, curSig)
		}
		return true
	}
	if err != nil {
		log.Printf("[%s] classifier failed: %v — leaving sig unchanged for retry", sess.SessionID, err)
		return false
	}
	log.Printf("[%s] trackable=%v existing=%d new=%d summary=%q",
		sess.SessionID, report.Trackable, len(report.ExistingCards), len(report.NewTasks),
		truncate(report.SessionSummary, 100))

	if cfg.DryRun {
		return true
	}

	if !report.Trackable {
		applyDoNotTrack(kc, sess, msgCount)
		writeSig(kc, sess, tagSet, curSig)
		return true
	}

	// A session that has cards but comes back with an opinion on none of them
	// has had none of them updated, whether or not it also filed new work — the
	// original form of this check only fired when new_tasks was non-empty, so
	// the commonest case (existing=0, new=0) passed in silence.
	//
	// Truncation is handled above and cannot reach here, so the remaining
	// causes are a digest that does not carry [EXISTING CARDS] or a model that
	// read them and declined to answer. -dump-digest distinguishes the two.
	if len(cards) > 0 && len(report.ExistingCards) == 0 {
		log.Printf("[%s] WARNING: %d existing card(s) went unaddressed (filed %d new task(s)) — "+
			"confirm the digest carries [EXISTING CARDS] with: kanban-classifier -session %s -dump-digest",
			sess.SessionID, len(cards), len(report.NewTasks), sess.SessionID)
	}

	applyExisting(kc, sess, report)
	applyNew(kc, sess, cards, report)

	writeSig(kc, sess, tagSet, curSig)

	// If bridge state is terminal, mark the session as final-classified.
	if sess.State == terminalStateCompleted || sess.State == terminalStateError {
		_ = kc.AddEntityTag(cardLinkSession, sess.SessionID, tagFinalClassified)
	}
	return true
}

// ============================ Tag helpers ============================

func tagSet(tags []kanbanclient.EntityTag) map[string]bool {
	out := map[string]bool{}
	for _, t := range tags {
		out[t.Tag] = true
	}
	return out
}

func priorSigTag(tagSet map[string]bool) string {
	for tag := range tagSet {
		if strings.HasPrefix(tag, tagCheckSigPrefix) {
			return tag
		}
	}
	return ""
}

// priorMsgCount parses the msg= component from the prior sig, or returns 0
// if no prior sig exists / parseable.
func priorMsgCount(tagSet map[string]bool) int {
	sig := priorSigTag(tagSet)
	if sig == "" {
		return 0
	}
	for _, part := range strings.Split(strings.TrimPrefix(sig, tagCheckSigPrefix), ":") {
		if strings.HasPrefix(part, "msg=") {
			var n int
			fmt.Sscanf(part, "msg=%d", &n)
			return n
		}
	}
	return 0
}

func buildSig(msgCount int, state, updatedAt string) string {
	t, _ := time.Parse(time.RFC3339, updatedAt)
	minute := t.UTC().Format("200601021504")
	return fmt.Sprintf("%smsg=%d:state=%s:t=%s", tagCheckSigPrefix, msgCount, state, minute)
}

// writeSig deletes the prior check-sig tag (if any) and adds the new one.
func writeSig(kc *kanbanclient.Client, sess session, oldTags map[string]bool, sig string) {
	for tag := range oldTags {
		if strings.HasPrefix(tag, tagCheckSigPrefix) && tag != sig {
			_ = kc.DeleteEntityTag(cardLinkSession, sess.SessionID, tag)
		}
	}
	if err := kc.AddEntityTag(cardLinkSession, sess.SessionID, sig); err != nil {
		log.Printf("[%s] add check sig: %v", sess.SessionID, err)
	}
}

func applyDoNotTrack(kc *kanbanclient.Client, sess session, msgCount int) {
	if err := kc.AddEntityTag(cardLinkSession, sess.SessionID, tagDoNotTrack); err != nil {
		log.Printf("[%s] add do-not-track: %v", sess.SessionID, err)
	}
}

// ============================ Apply classifier output ============================

func applyExisting(kc *kanbanclient.Client, sess session, report *Report) {
	for _, ec := range report.ExistingCards {
		colName := stateToColumn(ec.State)
		colID, ok := kc.ColumnsByName[colName]
		if !ok {
			log.Printf("[%s] column %q missing for existing card update; skipping", sess.SessionID, colName)
			continue
		}
		if err := kc.MoveCard(ec.CardID, kc.BoardID, colID, 0); err != nil {
			log.Printf("[%s] move card %s → %s: %v", sess.SessionID, ec.CardID, colName, err)
			continue
		}
		body := fmt.Sprintf("%s\n\n_classifier @ %s — state=%s_\n",
			ec.Summary, time.Now().UTC().Format(time.RFC3339), ec.State)
		if ec.Blocker != "" {
			body += "**Blocker:** " + ec.Blocker + "\n"
		}
		if err := kc.PatchCardBody(ec.CardID, body); err != nil {
			log.Printf("[%s] patch existing card %s: %v", sess.SessionID, ec.CardID, err)
		}
	}
}

func applyNew(kc *kanbanclient.Client, sess session, existingCards []kanbanclient.EntityCardEntry, report *Report) {
	filed := map[string]bool{}
	for _, ec := range existingCards {
		if title, _ := ec.Item["title"].(string); title != "" {
			filed[normalizeTitle(title)] = true
		}
	}

	for _, nt := range report.NewTasks {
		// Last line of defence against re-filing work that is already on the
		// board. The digest is what actually prevents this; an exact title
		// repeat means even that failed.
		if filed[normalizeTitle(nt.Title)] {
			log.Printf("[%s] skipping new task %q — a card with that title is already linked to this session",
				sess.SessionID, truncate(nt.Title, 80))
			continue
		}
		filed[normalizeTitle(nt.Title)] = true

		colName := stateToColumn(nt.State)
		body := nt.Summary
		if nt.Blocker != "" {
			body += "\n\n**Blocker:** " + nt.Blocker
		}
		body += fmt.Sprintf("\n\n_created by classifier @ %s · session=%s · state=%s_\n",
			time.Now().UTC().Format(time.RFC3339), sess.SessionID, nt.State)
		args := kanbanclient.CreateCardArgs{
			ColumnName: colName,
			Title:      truncate(nt.Title, 80),
			Body:       body,
			Tags:       []string{cardSourceClassifier, sess.Harness, sess.Purpose},
			SessionID:  "", // classifier adds its own session link with label "classifier" below
			Provider:   "",
		}
		cardID, err := kc.CreateCard(args)
		if err != nil {
			log.Printf("[%s] create new task card: %v", sess.SessionID, err)
			continue
		}
		if err := kc.AddCardLink(cardID, cardLinkSession, sess.SessionID, "classifier"); err != nil {
			log.Printf("[%s] link new card to session: %v", sess.SessionID, err)
		}
		if err := kc.AddCardLink(cardID, cardLinkSource, cardSourceClassifier, ""); err != nil {
			log.Printf("[%s] mark source on new card: %v", sess.SessionID, err)
		}
	}
}

// normalizeTitle folds case and punctuation so two spellings of the same
// title compare equal. It is deliberately exact on words: a fuzzy match here
// would suppress genuinely new tasks that merely read alike.
func normalizeTitle(title string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevSpace = false
		default:
			if !prevSpace {
				b.WriteRune(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func stateToColumn(state string) string {
	switch state {
	case "in_progress":
		return "In Progress"
	case "blocked":
		return "Blocked"
	case "done":
		return "Done"
	case "abandoned":
		return "Failed"
	default:
		return "In Progress"
	}
}

func requireColumns(kc *kanbanclient.Client, names []string) {
	for _, n := range names {
		if kc.ColumnsByName[n] == "" {
			log.Fatalf("column %q missing on board %s", n, kc.BoardID)
		}
	}
}

// ============================ Bridge-server client ============================

func listSessions(bridgeURL string) ([]session, error) {
	resp, err := http.Get(strings.TrimRight(bridgeURL, "/") + "/sessions")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list sessions: %s — %s", resp.Status, string(body))
	}
	var ss []session
	if err := json.NewDecoder(resp.Body).Decode(&ss); err != nil {
		return nil, err
	}
	return ss, nil
}

func getSession(bridgeURL, id string) (session, error) {
	var s session
	resp, err := http.Get(fmt.Sprintf("%s/sessions/%s", strings.TrimRight(bridgeURL, "/"), id))
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return s, fmt.Errorf("get session %s: %s — %s", id, resp.Status, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return s, err
	}
	if s.SessionID == "" {
		s.SessionID = id
	}
	return s, nil
}

type bridgeMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Events    []map[string]any `json:"events"`
	Timestamp string           `json:"timestamp"`
}

func getSessionMessages(bridgeURL, id string) ([]bridgeMessage, error) {
	resp, err := http.Get(fmt.Sprintf("%s/sessions/%s/messages", strings.TrimRight(bridgeURL, "/"), id))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get messages: %s — %s", resp.Status, string(body))
	}
	var ms []bridgeMessage
	if err := json.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, err
	}
	return ms, nil
}

// ============================ Digest builder ============================

// buildDigest renders one session as the classifier's prompt input, within
// digestCharBudget characters.
//
// Section ORDER IS LOAD-BEARING, and so is the per-section budgeting. The
// digest used to be assembled in conversation order and then chopped at the
// budget with a single slice, which put [EXISTING CARDS] last — so on any
// long session the whole section fell off the end. The model was then told
// "use new_tasks only for tasks not already represented by an existing card"
// while being shown no cards at all, and duly re-filed every task it had
// already filed, once per tick. One live session minted six near-identical
// cards for one task in ninety minutes that way.
//
// So the dedupe-critical section is emitted FIRST and gets its own reserve,
// and the conversational bulk is fitted into what remains — newest first,
// because the recent turns carry the "is it done / is it blocked" signal.
// Nothing is ever cut mid-section, and every drop is announced in the text
// so the model (and a human reading a dump) can see what is missing.
func buildDigest(sess session, msgs []bridgeMessage, existingCards []kanbanclient.EntityCardEntry, charBudget int) string {
	if charBudget <= 0 {
		charBudget = digestCharBudget
	}

	meta := buildMetaSection(sess, len(msgs))
	cards := buildExistingCardsSection(existingCards, budgetShare(charBudget, existingCardsBudgetShare))
	tools := buildToolActivitySection(msgs)

	remaining := charBudget - len(meta) - len(cards) - len(tools)
	if remaining < 0 {
		remaining = 0
	}
	turns := buildAssistantTurnsSection(msgs, budgetShare(remaining, assistantTurnsBudgetShare))
	prompts := buildUserPromptsSection(msgs, remaining-len(turns))

	return meta + cards + prompts + turns + tools
}

// budgetShare is `fraction` of total, as a character count.
func budgetShare(total int, fraction float64) int {
	if total <= 0 {
		return 0
	}
	return int(float64(total) * fraction)
}

func buildMetaSection(sess session, msgCount int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[SESSION META]\n")
	fmt.Fprintf(&b, "bridge_id: %s\n", sess.SessionID)
	fmt.Fprintf(&b, "display_name: %s\n", sess.DisplayName)
	fmt.Fprintf(&b, "harness: %s\n", sess.Harness)
	fmt.Fprintf(&b, "source: %s\n", sess.Purpose)
	fmt.Fprintf(&b, "state: %s\n", sess.State)
	fmt.Fprintf(&b, "created: %s\n", sess.CreatedAt)
	fmt.Fprintf(&b, "last_message: %s\n", sess.UpdatedAt)
	fmt.Fprintf(&b, "turn_count: %d\n\n", msgCount)
	return b.String()
}

// buildExistingCardsSection renders the cards already linked to this session.
// Card identity (card_id + title) is what stops the model re-filing work, so
// it is never dropped to save space: bodies are shed first, and only if the
// titles alone still overflow are the oldest cards omitted — loudly.
func buildExistingCardsSection(existingCards []kanbanclient.EntityCardEntry, budget int) string {
	const header = "[EXISTING CARDS — already on the kanban for this session]\n"
	if len(existingCards) == 0 {
		return header + "(none)\n\n"
	}

	withBodies := make([]string, 0, len(existingCards))
	titlesOnly := make([]string, 0, len(existingCards))
	for _, ec := range existingCards {
		title, _ := ec.Item["title"].(string)
		status, _ := ec.Item["status"].(string)
		body, _ := ec.Item["body"].(string)
		withBodies = append(withBodies, fmt.Sprintf("- card_id=%s title=%q status=%s\n  body=%q\n",
			ec.CardID, truncate(title, 80), status, truncate(body, 400)))
		titlesOnly = append(titlesOnly, fmt.Sprintf("- card_id=%s title=%q status=%s\n",
			ec.CardID, truncate(title, 80), status))
	}

	entries := withBodies
	if totalLen(entries) > budget-len(header) {
		entries = titlesOnly
	}
	return header + fitKeepingNewest(entries, budget-len(header), "older card(s) omitted") + "\n"
}

func buildUserPromptsSection(msgs []bridgeMessage, budget int) string {
	const header = "[USER PROMPTS — verbatim, newest last]\n"
	entries := []string{}
	for _, m := range msgs {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			entries = append(entries, fmt.Sprintf("%d. %s\n", len(entries)+1, truncate(m.Content, 1500)))
		}
	}
	if len(entries) == 0 {
		return header + "(none)\n\n"
	}
	return header + fitKeepingNewest(entries, budget-len(header), "earlier prompt(s) omitted") + "\n"
}

func buildAssistantTurnsSection(msgs []bridgeMessage, budget int) string {
	const header = "[ASSISTANT TURNS — recent, with text content]\n"
	withText := []bridgeMessage{}
	for _, m := range msgs {
		if m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {
			withText = append(withText, m)
		}
	}
	if len(withText) == 0 {
		return header + "(no assistant text content)\n\n"
	}
	// Take the last ~10 turns, then fit whatever share of those the budget allows.
	if len(withText) > 10 {
		withText = withText[len(withText)-10:]
	}
	entries := make([]string, 0, len(withText))
	for i, m := range withText {
		entries = append(entries, fmt.Sprintf("  turn %d (%s):\n  %s\n\n", i+1, m.Timestamp, truncate(m.Content, 600)))
	}
	return header + fitKeepingNewest(entries, budget-len(header), "earlier turn(s) omitted") + "\n"
}

func buildToolActivitySection(msgs []bridgeMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[TOOL ACTIVITY — last ~15 tool calls]\n")
	tools := []string{}
	for _, m := range msgs {
		for _, e := range m.Events {
			if e["type"] != "tool_call" {
				continue
			}
			name, _ := e["tool_name"].(string)
			if name == "" {
				if tc, ok := e["tool_call"].(map[string]any); ok {
					name, _ = tc["name"].(string)
				}
			}
			if name == "" {
				continue
			}
			tools = append(tools, name)
		}
	}
	if len(tools) > 15 {
		tools = tools[len(tools)-15:]
	}
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s\n", t)
	}
	if len(tools) == 0 {
		fmt.Fprintf(&b, "(none)\n")
	}
	b.WriteString("\n")
	return b.String()
}

// fitKeepingNewest returns as many trailing entries as fit in budget, in
// order, with a marker naming how many leading ones were dropped. It keeps at
// least one entry even when that entry alone overflows — a section that
// silently renders empty is how the truncation bug hid for so long.
func fitKeepingNewest(entries []string, budget int, omittedNoun string) string {
	if len(entries) == 0 {
		return ""
	}
	first := len(entries)
	used := 0
	for i := len(entries) - 1; i >= 0; i-- {
		if used+len(entries[i]) > budget && i != len(entries)-1 {
			break
		}
		used += len(entries[i])
		first = i
	}
	var b strings.Builder
	if first > 0 {
		fmt.Fprintf(&b, "(… %d %s)\n", first, omittedNoun)
	}
	for _, e := range entries[first:] {
		b.WriteString(e)
	}
	return b.String()
}

func totalLen(entries []string) int {
	n := 0
	for _, e := range entries {
		n += len(e)
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ============================ Anthropic call ============================

// Report mirrors the report_classification tool's input schema.
type Report struct {
	Trackable      bool          `json:"trackable"`
	SessionSummary string        `json:"session_summary,omitempty"`
	ExistingCards  []ExistingRec `json:"existing_cards,omitempty"`
	NewTasks       []NewRec      `json:"new_tasks,omitempty"`
}

type ExistingRec struct {
	CardID  string `json:"card_id"`
	State   string `json:"state"`
	Summary string `json:"summary"`
	Blocker string `json:"blocker,omitempty"`
}

type NewRec struct {
	Title   string `json:"title"`
	State   string `json:"state"`
	Summary string `json:"summary"`
	Blocker string `json:"blocker,omitempty"`
}

const systemPrompt = `You are an internal task-tracking classifier for an AI agent infrastructure. You analyze a single chat session between a user and an autonomous agent, and decide what work-tracking cards should exist on a kanban board to represent the tasks happening in that session.

Trackable cards represent meaningful units of user-directed work — bug fixes, feature builds, investigations, migrations, refactors, infra work. NOT: trivial questions, casual chat, one-line edits, or pure exploration without changes.

A session may contain ZERO, ONE, or MULTIPLE distinct tasks. Emit one card per distinct task. If the session pivoted from finishing task A to starting task B, both should be reflected (A as done, B as in_progress).

You will be given:
- Session metadata (display name, harness, state, message count)
- All user prompts verbatim (intent signal)
- Recent assistant turns (current activity / completion signals)
- Recent tool-call activity
- Any cards already linked to this session, with their current state and body

You MUST output by calling the report_classification tool exactly once.

Rules:
- For every existing_card you are given, return a record (don't drop any). If you have no opinion, repeat its current state.
- Use new_tasks only for tasks that are NOT already represented by an existing card.
- title for new tasks: short imperative phrase, ≤60 chars (e.g. "Migrate auth middleware to auth-store").
- summary: one or two sentences on what's happening / what was done / what's blocking.
- state must be one of: in_progress, blocked, done, abandoned.
- If the session is purely chit-chat / Q&A with no real task, set trackable=false and omit existing_cards / new_tasks.

Bridge state ≠ task state. A session can be "completed" while tasks are still in_progress (session ended early), or "idle" with all tasks done. Trust the conversational evidence over bridge state.`

const reportToolName = "report_classification"

func reportToolSchema() map[string]any {
	return map[string]any{
		"name":        reportToolName,
		"description": "Report the classification of this session's tasks. Call exactly once.",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"trackable":       map[string]any{"type": "boolean", "description": "False if the session has no real work to track."},
				"session_summary": map[string]any{"type": "string", "description": "1-3 sentence overall description."},
				"existing_cards": map[string]any{
					"type":        "array",
					"description": "One entry per pre-existing card on this session. Required: include every card you were given.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"card_id": map[string]any{"type": "string"},
							"state":   map[string]any{"type": "string", "enum": []string{"in_progress", "blocked", "done", "abandoned"}},
							"summary": map[string]any{"type": "string"},
							"blocker": map[string]any{"type": "string"},
						},
						"required": []string{"card_id", "state", "summary"},
					},
				},
				"new_tasks": map[string]any{
					"type":        "array",
					"description": "Tasks happening in this session that are NOT yet represented by an existing card.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title":   map[string]any{"type": "string", "maxLength": 80},
							"state":   map[string]any{"type": "string", "enum": []string{"in_progress", "blocked", "done", "abandoned"}},
							"summary": map[string]any{"type": "string"},
							"blocker": map[string]any{"type": "string"},
						},
						"required": []string{"title", "state", "summary"},
					},
				},
			},
			"required": []string{"trackable"},
		},
	}
}

// maxOutputTokens bounds the classifier's reply. A full sweep costs roughly 69
// output tokens per existing card, so this ceiling is also a cap on how many
// cards one session can have its state refreshed in a single call; past it the
// reply is truncated and rejected (see errTruncatedReport).
const maxOutputTokens = 1024

const stopReasonMaxTokens = "max_tokens"

// errTruncatedReport marks a classification that the model did not finish
// writing. It is deliberately distinct from a transport or decode failure: the
// call succeeded and cost money, so it must not be retried on identical input.
var errTruncatedReport = errors.New("classification truncated")

// anthropicMessagesURL is a package var rather than a constant so tests can
// point classify at an httptest server; nothing else reassigns it.
var anthropicMessagesURL = "https://api.anthropic.com/v1/messages"

func classify(ctx context.Context, cfg config, digest string) (*Report, error) {
	body := map[string]any{
		"model":      cfg.Model,
		"max_tokens": maxOutputTokens,
		"system":     systemPrompt,
		"tools":      []any{reportToolSchema()},
		"tool_choice": map[string]any{
			"type": "tool",
			"name": reportToolName,
		},
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": digest,
			},
		},
	}
	bs, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", anthropicMessagesURL, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cfg.AnthropicAPIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	httpC := &http.Client{Timeout: 60 * time.Second}
	resp, err := httpC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic %s: %s", resp.Status, string(rb))
	}
	var apiResp struct {
		StopReason string `json:"stop_reason"`
		Usage      struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Content []struct {
			Type  string         `json:"type"`
			Name  string         `json:"name,omitempty"`
			Input map[string]any `json:"input,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rb, &apiResp); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
	}
	// A response cut off at max_tokens still carries a tool_use block, and its
	// partial input still parses as valid JSON — the API drops the unfinished
	// array rather than emitting broken JSON. So there is no decode error to
	// catch here: stop_reason is the ONLY evidence that the classification is
	// incomplete, and a truncated report reads exactly like a model that had no
	// opinion on any card. Measured on session br_1784416900461396845 (69
	// cards): at max_tokens=1024 the report comes back existing=0 new=0 and
	// parses cleanly; the same digest at 8192 returns all 69 in 4,771 tokens.
	if apiResp.StopReason == stopReasonMaxTokens {
		return nil, fmt.Errorf("%w: model hit max_tokens=%d after %d output tokens; "+
			"the report is incomplete and its missing cards are indistinguishable from 'no opinion'",
			errTruncatedReport, maxOutputTokens, apiResp.Usage.OutputTokens)
	}
	for _, blk := range apiResp.Content {
		if blk.Type != "tool_use" || blk.Name != reportToolName {
			continue
		}
		raw, _ := json.Marshal(blk.Input)
		var rep Report
		if err := json.Unmarshal(raw, &rep); err != nil {
			return nil, fmt.Errorf("decode tool input: %w (raw=%s)", err, string(raw))
		}
		return &rep, nil
	}
	return nil, fmt.Errorf("no tool_use block in response: %s", string(rb))
}

// ============================ Auth-store key fetch ============================

func resolveAnthropicKey(ctx context.Context, cfg config) (string, error) {
	u := strings.TrimRight(cfg.AuthStoreURL, "/") + "/api/resolve/anthropic"
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req.Header.Set("X-Auth-App", "kanban-classifier")
	req.Header.Set("X-Auth-Reason", "classify-bridge-sessions")
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
		AuthType    string `json:"auth_type"`
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

// ============================ Scheduler self-registration ============================

func registerSchedulerJob() error {
	schedURL := envOr("SCHEDULER_URL", "http://localhost:8092")
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, "bin", "kanban-classifier")

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
		if j.Name == "kanban-classifier-tick" {
			fmt.Println("kanban-classifier-tick already registered; no changes made")
			return nil
		}
	}
	payload := map[string]any{
		"name":        "kanban-classifier-tick",
		"description": "Cheap-model classifier that creates and updates kanban cards from bridge sessions.",
		"schedule":    "*/15 * * * *",
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
	fmt.Printf("registered kanban-classifier-tick: %s\n", strings.TrimSpace(string(body)))
	return nil
}

// ============================ misc ============================

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// silence unused import on net/url — kept in case we need query encoding later.
var _ = url.PathEscape
