package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ============================ taxonomy ============================
//
// Three independent axes, derived from a 400-message sample of this host's
// actual inbox (2026-07-09..2026-08-11) rather than invented. Only one axis can
// be columns; action state won that slot, so category and urgency ride along as
// noteboard tags and become the board's filter and grouping controls.

// cardCategories is the topic axis: which part of life the card belongs to.
var cardCategories = []string{
	"finance",          // bills, payments, receipts, banking (Venmo, PayPal, Link)
	"housing",          // rent, landlord, apartment hunting (AppFolio, Apartment List)
	"health",           // appointments, insurance, providers (Tebra, Anthem)
	"commerce",         // orders, shipping, returns, marketplace (eBay, Etsy, USPS)
	"work-career",      // jobs, recruiters, professional (Glassdoor, Indeed)
	"dev-infra",        // repos, domains, model providers, service alerts
	"social-events",    // meetups, classes, community (Meetup, dance, ballet)
	"government-legal", // DMV, USPS official, taxes, governing bodies
	"media-subs",       // digests and subscriptions (Medium, MyAnimeList, Plex)
	"politics",         // campaign and advocacy mail
	"promotions",       // pure marketing
	"personal",         // mail from an actual human
}

// cardActions is the "what would you have to do" axis.
var cardActions = []string{
	"reply",    // a counterparty is waiting on a response
	"pay",      // money is due
	"decide",   // accept/decline/choose, usually against a deadline
	"schedule", // book, attend, or move an appointment
	"verify",   // confirm identity or a security event
	"fix",      // something is broken and needs work
	"read",     // worth reading, nothing owed
	"file",     // receipt or record, keep for reference
	"none",     // no action, ever
}

// cardUrgencies is the time-pressure axis. "expired" earns its place because in
// email a large share of what reads as urgent is a deadline already missed, and
// collapsing that into "low" hides the difference between "ignore this" and
// "you missed it".
var cardUrgencies = []string{"critical", "high", "normal", "low", "expired"}

// actionsNeedingWork are the action values that put a card in the Action needed
// column. Everything else lands in No action. A set rather than a chain of
// string comparisons so the vocabulary has exactly one authoring.
var actionsNeedingWork = map[string]bool{
	"reply": true, "pay": true, "decide": true,
	"schedule": true, "verify": true, "fix": true,
}

func isKnown(vocabulary []string, value string) bool {
	for _, v := range vocabulary {
		if v == value {
			return true
		}
	}
	return false
}

// columnForAction maps the action axis onto the board's action-state columns.
// A card whose action the model reported outside the vocabulary is treated as
// needing action: an unrecognized value means the classifier misbehaved, and
// parking that in No action would hide it forever.
func columnForAction(action string) string {
	if !isKnown(cardActions, action) {
		return columnActionNeeded
	}
	if actionsNeedingWork[action] {
		return columnActionNeeded
	}
	return columnNoAction
}

// ============================ report shape ============================

// newCardSpec is a card the model wants created, stated inline on the filing
// that needs it.
//
// It carries no id or key on purpose. An earlier shape had a separate new_cards
// array that filings referenced by a made-up key, and the model mangled that
// cross-reference repeatedly on live mail — stringifying the arrays and once
// leaking its own `<parameter name=...>` markup into a field. Two filings that
// propose the same card are reconciled here instead, by normalized title, which
// is machinery the applier already needed for the case where the model proposes
// a card that already exists on the board.
type newCardSpec struct {
	Title    string `json:"title"`
	Category string `json:"category"`
	Action   string `json:"action"`
	Urgency  string `json:"urgency"`
	Summary  string `json:"summary"`
}

// emailFiling is the model's decision for one message. CardIDs points at cards
// that already exist; NewCard describes one that does not yet. Both may be set —
// that is the many-to-many case, an email that belongs on an existing card and
// on a new one.
type emailFiling struct {
	EmailRef  string       `json:"email_ref"`
	CardIDs   []string     `json:"card_ids"`
	NewCard   *newCardSpec `json:"new_card"`
	LinkLabel string       `json:"link_label"`
}

// cardUpdate re-states an existing card whose action state or urgency the new
// mail changed — a delivery notice completing a purchase, a reminder raising
// urgency.
type cardUpdate struct {
	CardID  string `json:"card_id"`
	Action  string `json:"action"`
	Urgency string `json:"urgency"`
	Summary string `json:"summary"`
}

type filingReport struct {
	Filings     filingList     `json:"filings"`
	CardUpdates cardUpdateList `json:"card_updates"`
}

// The three list fields below tolerate a shape the model produces intermittently:
// the array arrives as a JSON-encoded *string* rather than a JSON array, e.g.
//
//	{"filings": "[\n  {\n    \"email_ref\": ...}]"}
//
// Measured on live runs — two of three consecutive batches came back this way,
// and a strict decode rejected the whole report. That failure is safe (the
// emails stay unfiled and retry next tick) but it is also permanent: the same
// input produces the same stringified reply, so a batch could never drain.
//
// Accepting both shapes is a decode-edge concession, not a silent fixup: the
// model's intent is unambiguous either way, and the fallback logs when it fires
// so the misbehaviour stays visible rather than becoming invisible normality.
type filingList []emailFiling
type cardUpdateList []cardUpdate

func (l *filingList) UnmarshalJSON(data []byte) error {
	return unmarshalArrayOrEncodedString(data, (*[]emailFiling)(l), "filings")
}

func (l *cardUpdateList) UnmarshalJSON(data []byte) error {
	return unmarshalArrayOrEncodedString(data, (*[]cardUpdate)(l), "card_updates")
}

// unmarshalArrayOrEncodedString decodes a JSON array, falling back to decoding a
// JSON string whose contents are that array.
func unmarshalArrayOrEncodedString[T any](data []byte, out *[]T, field string) error {
	var direct []T
	if err := json.Unmarshal(data, &direct); err == nil {
		*out = direct
		return nil
	}
	var encoded string
	if err := json.Unmarshal(data, &encoded); err != nil {
		// Neither shape. Report the array error, since that is the shape the
		// schema asked for.
		return fmt.Errorf("%s is neither an array nor an encoded array: %s", field, truncateForError(data))
	}
	if err := json.Unmarshal([]byte(encoded), out); err != nil {
		return fmt.Errorf("%s arrived as a string but its contents are not a valid array: %w", field, err)
	}
	log.Printf("[email] NOTE: model returned %s as an encoded string rather than an array; decoded it anyway", field)
	return nil
}

func truncateForError(data []byte) string {
	const limit = 200
	if len(data) <= limit {
		return string(data)
	}
	return string(data[:limit]) + "…"
}

const reportToolName = "report_email_filing"

var systemPrompt = `You are an email triage classifier. You are given a batch of emails and the cards that currently exist on an "Email" kanban board. You decide which card each email belongs to.

CARDS ARE THE UNIT OF WORK. Emails are evidence attached to cards, never cards themselves. One card gathers many emails; one email may attach to more than one card.

EVERY EMAIL MUST BE FILED. Return exactly one filing per email you were given, and every filing must name at least one card: an existing one via card_ids, a new one via new_card, or both. Nothing may be left unfiled.

When two emails in the batch belong on the same new card, give BOTH filings that same new_card with an identical title. The card is created once and both emails attach to it.

Two kinds of card exist, and picking the right kind is most of the job:

1. BUCKET cards — durable homes for recurring bulk mail that will never be acted on. "Meetup event announcements", "Democrats fundraising mail", "Medium digests", "Uber promotions". These have action "none" or "read" and they accumulate hundreds of emails over time. When bulk mail arrives and a suitable bucket exists, ALWAYS attach to it. Only create a new bucket when no existing one fits.

2. TASK cards — one real thing to do, with a beginning and an end. "Decide on the KAPITAL jacket counter-offer", "Confirm the USPS Informed Delivery registration", "Reply to masa_117 about item 206153166065". These carry a real action and are expected to complete.

A RECEIPT IS NOT A TASK. Payment confirmations, order receipts, statements and delivery notices are records: nothing is owed and nothing will ever be completed. File them on a durable bucket card for that source or topic — "Payment receipts", "Rent payments", "Amazon orders" — never on a card of their own. A board with one card per receipt is a board nobody can read. The same holds for one-off newsletters and announcements: they belong on a bucket for that sender.

Judge the email, not its vocabulary. Marketing mail routinely says "REMINDER", "Action required", "expiring", "last chance" — that is copywriting, not a task. A real task has a specific counterparty expecting something specific from this user.

Prefer attaching to an existing card over creating a new one. A new card for something already tracked is the most expensive mistake you can make here, because it splits one thread of work across two places.

Axes, all required on every new card:
- category: ` + strings.Join(cardCategories, ", ") + `
- action: ` + strings.Join(cardActions, ", ") + `
- urgency: ` + strings.Join(cardUrgencies, ", ") + `
    critical = deadline inside 24h, or money/security at stake
    high     = deadline inside 3 days
    normal   = deadline inside 2 weeks
    low      = no deadline
    expired  = the deadline has already passed

title: short, specific, ≤70 chars. Name the actual thing ("Pay August rent to Real Property Management"), not the sender ("Email from AppFolio").
link_label: ≤50 chars describing this ONE email's role on the card ("seller counter-offer, Aug 10"), so a human scanning the card's evidence list can tell the messages apart.

Use card_updates only when new mail changes an existing card's action or urgency — for example a delivery confirmation moving a purchase to action "file", or a final notice raising urgency to critical.

You MUST output by calling the ` + reportToolName + ` tool exactly once.`

func reportToolSchema() map[string]any {
	return map[string]any{
		"name":        reportToolName,
		"description": "Report which card each email belongs to. Call exactly once.",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"filings": map[string]any{
					"type":        "array",
					"description": "Exactly one entry per email supplied. Every entry must reference at least one card, via card_ids or new_card or both.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"email_ref":  map[string]any{"type": "string", "description": "The email's ref exactly as given in the prompt."},
							"card_ids":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Ids of existing cards this email attaches to."},
							"link_label": map[string]any{"type": "string", "maxLength": 50},
							"new_card": map[string]any{
								"type":        "object",
								"description": "A card that does not exist yet. Two emails may describe the same new card with the same title; it is created once.",
								"properties": map[string]any{
									"title":    map[string]any{"type": "string", "maxLength": 70},
									"category": map[string]any{"type": "string", "enum": cardCategories},
									"action":   map[string]any{"type": "string", "enum": cardActions},
									"urgency":  map[string]any{"type": "string", "enum": cardUrgencies},
									"summary":  map[string]any{"type": "string", "description": "One or two sentences on what this card is for."},
								},
								"required": []string{"title", "category", "action", "urgency", "summary"},
							},
						},
						"required": []string{"email_ref"},
					},
				},
				"card_updates": map[string]any{
					"type":        "array",
					"description": "Existing cards whose action state or urgency this batch of mail changed.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"card_id": map[string]any{"type": "string"},
							"action":  map[string]any{"type": "string", "enum": cardActions},
							"urgency": map[string]any{"type": "string", "enum": cardUrgencies},
							"summary": map[string]any{"type": "string"},
						},
						"required": []string{"card_id"},
					},
				},
			},
			"required": []string{"filings"},
		},
	}
}

// maxOutputTokens bounds one filing report. A filing costs roughly 60 output
// tokens and a new card roughly 90, so this ceiling comfortably covers a full
// batch of emailsPerModelCall plus a realistic number of new cards. It is
// deliberately far above kanban-classifier's 1024: that command was measured
// silently truncating at 1024 with 69 cards to report, and the failure is
// invisible because a truncated report parses cleanly.
const maxOutputTokens = 8192

const stopReasonMaxTokens = "max_tokens"

// errTruncatedReport marks a report the model did not finish writing. Kept
// distinct from transport and decode failures because the call still cost
// money, and because a truncated report is indistinguishable from one that
// simply filed fewer emails — the emails it dropped would silently violate the
// every-email-is-filed invariant.
var errTruncatedReport = errors.New("filing report truncated")

// anthropicMessagesURL is a package var rather than a constant so tests can
// point classifyBatch at an httptest server; nothing else reassigns it.
var anthropicMessagesURL = "https://api.anthropic.com/v1/messages"

func classifyBatch(ctx context.Context, cfg config, digest string) (*filingReport, error) {
	var rep filingReport
	if err := invokeTool(ctx, cfg, systemPrompt, reportToolSchema(), reportToolName, digest, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// invokeTool runs one forced single-tool call and decodes its input into out.
//
// Shared by the filing pass and the merge pass so the truncation guard below has
// exactly one authoring. That guard is the whole reason this is not three lines:
// a reply cut off at max_tokens still carries a tool_use block whose partial
// input parses as valid JSON, because the API drops the unfinished array rather
// than emitting broken JSON. stop_reason is the only evidence the answer is
// incomplete, and an incomplete answer here is silently indistinguishable from a
// model that simply had less to say.
func invokeTool(ctx context.Context, cfg config, system string, schema map[string]any, toolName, digest string, out any) error {
	body := map[string]any{
		"model":       cfg.Model,
		"max_tokens":  maxOutputTokens,
		"system":      system,
		"tools":       []any{schema},
		"tool_choice": map[string]any{"type": "tool", "name": toolName},
		"messages":    []any{map[string]any{"role": "user", "content": digest}},
	}
	bs, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", anthropicMessagesURL, bytes.NewReader(bs))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cfg.AnthropicAPIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	httpC := &http.Client{Timeout: 180 * time.Second}
	resp, err := httpC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("anthropic %s: %s", resp.Status, string(rb))
	}
	var apiResp struct {
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Content []struct {
			Type  string         `json:"type"`
			Name  string         `json:"name,omitempty"`
			Input map[string]any `json:"input,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rb, &apiResp); err != nil {
		return fmt.Errorf("decode anthropic response: %w", err)
	}
	if apiResp.StopReason == stopReasonMaxTokens {
		return fmt.Errorf("%w: hit max_tokens=%d after %d output tokens", errTruncatedReport, maxOutputTokens, apiResp.Usage.OutputTokens)
	}
	for _, blk := range apiResp.Content {
		if blk.Type != "tool_use" || blk.Name != toolName {
			continue
		}
		raw, _ := json.Marshal(blk.Input)
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode tool input: %w (raw=%s)", err, truncateForError(raw))
		}
		return nil
	}
	return fmt.Errorf("no tool_use block named %s in response: %s", toolName, truncateForError(rb))
}

// classifyMerges asks which cards on the board are really one bucket.
func classifyMerges(ctx context.Context, cfg config, digest string) (*mergeReport, error) {
	var rep mergeReport
	if err := invokeTool(ctx, cfg, mergeSystemPrompt, mergeToolSchema(), mergeToolName, digest, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}
