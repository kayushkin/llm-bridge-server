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
	digestTokenBudget      = 4000 // approximate; very rough
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
	}

	apiKey, err := resolveAnthropicKey(ctx, cfg)
	if err != nil {
		log.Fatalf("resolve anthropic key: %v", err)
	}
	cfg.AnthropicAPIKey = apiKey

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
}

// ============================ Candidate selection ============================

type session struct {
	BridgeID    string `json:"bridge_id"`
	State       string `json:"state"`
	Source      string `json:"source"`
	ClientID    string `json:"client_id"`
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
	tags, err := kc.ListEntityTags(cardLinkSession, sess.BridgeID)
	if err != nil {
		log.Printf("[%s] list tags: %v", sess.BridgeID, err)
		return false
	}
	tagSet := tagSet(tags)

	// Permanent skip: previously classified as terminal.
	if tagSet[tagFinalClassified] {
		return false
	}

	msgs, err := getSessionMessages(cfg.BridgeURL, sess.BridgeID)
	if err != nil {
		log.Printf("[%s] get messages: %v", sess.BridgeID, err)
		return false
	}
	msgCount := len(msgs)

	// do-not-track: only re-evaluate after a big delta.
	if tagSet[tagDoNotTrack] {
		priorN := priorMsgCount(tagSet)
		if msgCount-priorN < doNotTrackReevalDelta {
			return false
		}
		log.Printf("[%s] do-not-track but Δmsg=%d ≥ %d — re-evaluating",
			sess.BridgeID, msgCount-priorN, doNotTrackReevalDelta)
	}

	// Compute current signature; bail if unchanged.
	curSig := buildSig(msgCount, sess.State, sess.UpdatedAt)
	priorSig := priorSigTag(tagSet)
	if priorSig == curSig {
		return false
	}

	// Look up existing cards already linked to this session.
	cards, err := kc.ListCardsForEntity(cardLinkSession, sess.BridgeID)
	if err != nil {
		log.Printf("[%s] list cards: %v", sess.BridgeID, err)
		return false
	}

	digest := buildDigest(sess, msgs, cards)
	report, err := classify(ctx, cfg, digest)
	if err != nil {
		log.Printf("[%s] classifier failed: %v — leaving sig unchanged for retry", sess.BridgeID, err)
		return false
	}
	log.Printf("[%s] trackable=%v existing=%d new=%d summary=%q",
		sess.BridgeID, report.Trackable, len(report.ExistingCards), len(report.NewTasks),
		truncate(report.SessionSummary, 100))

	if cfg.DryRun {
		return true
	}

	if !report.Trackable {
		applyDoNotTrack(kc, sess, msgCount)
		writeSig(kc, sess, tagSet, curSig)
		return true
	}

	applyExisting(kc, sess, report)
	applyNew(kc, sess, report)

	writeSig(kc, sess, tagSet, curSig)

	// If bridge state is terminal, mark the session as final-classified.
	if sess.State == terminalStateCompleted || sess.State == terminalStateError {
		_ = kc.AddEntityTag(cardLinkSession, sess.BridgeID, tagFinalClassified)
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
			_ = kc.DeleteEntityTag(cardLinkSession, sess.BridgeID, tag)
		}
	}
	if err := kc.AddEntityTag(cardLinkSession, sess.BridgeID, sig); err != nil {
		log.Printf("[%s] add check sig: %v", sess.BridgeID, err)
	}
}

func applyDoNotTrack(kc *kanbanclient.Client, sess session, msgCount int) {
	if err := kc.AddEntityTag(cardLinkSession, sess.BridgeID, tagDoNotTrack); err != nil {
		log.Printf("[%s] add do-not-track: %v", sess.BridgeID, err)
	}
}

// ============================ Apply classifier output ============================

func applyExisting(kc *kanbanclient.Client, sess session, report *Report) {
	for _, ec := range report.ExistingCards {
		colName := stateToColumn(ec.State)
		colID, ok := kc.ColumnsByName[colName]
		if !ok {
			log.Printf("[%s] column %q missing for existing card update; skipping", sess.BridgeID, colName)
			continue
		}
		if err := kc.MoveCard(ec.CardID, kc.BoardID, colID, 0); err != nil {
			log.Printf("[%s] move card %s → %s: %v", sess.BridgeID, ec.CardID, colName, err)
			continue
		}
		body := fmt.Sprintf("%s\n\n_classifier @ %s — state=%s_\n",
			ec.Summary, time.Now().UTC().Format(time.RFC3339), ec.State)
		if ec.Blocker != "" {
			body += "**Blocker:** " + ec.Blocker + "\n"
		}
		if err := kc.PatchCardBody(ec.CardID, body); err != nil {
			log.Printf("[%s] patch existing card %s: %v", sess.BridgeID, ec.CardID, err)
		}
	}
}

func applyNew(kc *kanbanclient.Client, sess session, report *Report) {
	for _, nt := range report.NewTasks {
		colName := stateToColumn(nt.State)
		body := nt.Summary
		if nt.Blocker != "" {
			body += "\n\n**Blocker:** " + nt.Blocker
		}
		body += fmt.Sprintf("\n\n_created by classifier @ %s · session=%s · state=%s_\n",
			time.Now().UTC().Format(time.RFC3339), sess.BridgeID, nt.State)
		args := kanbanclient.CreateCardArgs{
			ColumnName:   colName,
			Title:        truncate(nt.Title, 80),
			Body:         body,
			Tags:         []string{cardSourceClassifier, sess.Harness, sess.Source},
			BusSessionID: "",
			Provider:     "",
		}
		cardID, err := kc.CreateCard(args)
		if err != nil {
			log.Printf("[%s] create new task card: %v", sess.BridgeID, err)
			continue
		}
		if err := kc.AddCardLink(cardID, cardLinkSession, sess.BridgeID, "classifier"); err != nil {
			log.Printf("[%s] link new card to session: %v", sess.BridgeID, err)
		}
		if err := kc.AddCardLink(cardID, cardLinkSource, cardSourceClassifier, ""); err != nil {
			log.Printf("[%s] mark source on new card: %v", sess.BridgeID, err)
		}
	}
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
	if s.BridgeID == "" {
		s.BridgeID = id
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

func buildDigest(sess session, msgs []bridgeMessage, existingCards []kanbanclient.EntityCardEntry) string {
	var b strings.Builder

	fmt.Fprintf(&b, "[SESSION META]\n")
	fmt.Fprintf(&b, "bridge_id: %s\n", sess.BridgeID)
	fmt.Fprintf(&b, "display_name: %s\n", sess.DisplayName)
	fmt.Fprintf(&b, "harness: %s\n", sess.Harness)
	fmt.Fprintf(&b, "source: %s\n", sess.Source)
	fmt.Fprintf(&b, "client_id: %s\n", sess.ClientID)
	fmt.Fprintf(&b, "state: %s\n", sess.State)
	fmt.Fprintf(&b, "created: %s\n", sess.CreatedAt)
	fmt.Fprintf(&b, "last_message: %s\n", sess.UpdatedAt)
	fmt.Fprintf(&b, "turn_count: %d\n\n", len(msgs))

	// All user prompts verbatim — intent signal.
	fmt.Fprintf(&b, "[USER PROMPTS — verbatim, all of them]\n")
	userIdx := 0
	for _, m := range msgs {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			userIdx++
			fmt.Fprintf(&b, "%d. %s\n", userIdx, truncate(m.Content, 1500))
		}
	}
	if userIdx == 0 {
		fmt.Fprintf(&b, "(none)\n")
	}
	b.WriteString("\n")

	// Last K assistant turns with text content. Sized to fit budget.
	fmt.Fprintf(&b, "[ASSISTANT TURNS — recent, with text content]\n")
	withText := []bridgeMessage{}
	for _, m := range msgs {
		if m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {
			withText = append(withText, m)
		}
	}
	// Take the last ~10 turns.
	keep := withText
	if len(withText) > 10 {
		keep = withText[len(withText)-10:]
	}
	for i, m := range keep {
		fmt.Fprintf(&b, "  turn %d (%s):\n  %s\n\n", i+1, m.Timestamp, truncate(m.Content, 600))
	}
	if len(keep) == 0 {
		fmt.Fprintf(&b, "(no assistant text content)\n")
	}

	// Recent tool activity.
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

	// Existing cards — let the model reason about them.
	fmt.Fprintf(&b, "[EXISTING CARDS — already on the kanban for this session]\n")
	if len(existingCards) == 0 {
		fmt.Fprintf(&b, "(none)\n")
	}
	for _, ec := range existingCards {
		title, _ := ec.Item["title"].(string)
		status, _ := ec.Item["status"].(string)
		body, _ := ec.Item["body"].(string)
		fmt.Fprintf(&b, "- card_id=%s title=%q status=%s\n  body=%q\n",
			ec.CardID, truncate(title, 80), status, truncate(body, 400))
	}

	// Hard cap: trim to budget. Crude but fail-safe.
	out := b.String()
	if len(out) > digestTokenBudget*4 { // ~4 chars/token rough heuristic
		out = out[:digestTokenBudget*4] + "\n…[truncated]"
	}
	return out
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

func classify(ctx context.Context, cfg config, digest string) (*Report, error) {
	body := map[string]any{
		"model":      cfg.Model,
		"max_tokens": 1024,
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
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(bs))
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
		Content []struct {
			Type  string         `json:"type"`
			Name  string         `json:"name,omitempty"`
			Input map[string]any `json:"input,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rb, &apiResp); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
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
