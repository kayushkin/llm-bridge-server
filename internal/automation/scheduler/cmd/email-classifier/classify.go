package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/completionoperation"
)

// ============================ taxonomy ============================
//
// Three independent axes, derived from a 400-message sample of this host's
// actual inbox (2026-07-09..2026-08-11) rather than invented. Only one axis can
// be columns; action state won that slot, so category and urgency ride along as
// noteboard tags and become the board's filter and grouping controls.

func isKnown(values []string, value string) bool {
	for _, v := range values {
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
	if !isKnown(active.Actions, action) {
		return columnActionNeeded
	}
	if active.ActionsNeedingWork[action] {
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
	// DueDate is the deadline the mail states, when it states one. Empty is a
	// real and common answer — most mail has no deadline — and is not an error.
	//
	// Urgency above is the model's reading at the moment of filing; this is the
	// fact underneath it. Storing the date is what lets the tick re-derive
	// urgency later instead of leaving a three-day-old adjective in place.
	DueDate string `json:"due_date"`
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
	// DueDate revises the deadline when new mail moves it — a rescheduled
	// appointment, an extended offer. Empty leaves the stored date untouched
	// rather than clearing it, because noteboard keeps no revision history for
	// due_at and a cleared one could not be recovered.
	DueDate string `json:"due_date"`
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
// so the misbehavior stays visible rather than becoming invisible normality.
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

// buildSystemPrompt assembles the prompt for the ACTIVE vocabulary.
//
// A function and not a package-level var, which is what it used to be. A var is
// evaluated at package init, before flags are parsed, so it captured the default
// vocabulary's axes and kept them however the flag was set — the schema would
// have said "work" while the prompt still said "finance, housing, health". The
// two disagreeing is the worst version of this: the model is told one vocabulary
// and graded against another.
func buildSystemPrompt() string {
	prompt := `You are an email triage classifier. You are given a batch of emails and the cards that currently exist on a kanban board. You decide which card each email belongs to.

THE MAILBOX: ` + active.Domain + `

CARDS ARE THE UNIT OF WORK. Emails are evidence attached to cards, never cards themselves. One card gathers many emails; one email may attach to more than one card.

EVERY EMAIL MUST BE FILED. Return exactly one filing per email you were given, and every filing must name at least one card: an existing one via card_ids, a new one via new_card, or both. Nothing may be left unfiled.

When two emails in the batch belong on the same new card, give BOTH filings that same new_card with an identical title. The card is created once and both emails attach to it.

Two kinds of card exist, and picking the right kind is most of the job:

1. BUCKET cards — durable homes for recurring bulk mail that will never be acted on. "Meetup event announcements", "Democrats fundraising mail", "Medium digests", "Uber promotions". These have action "none" or "read" and they accumulate hundreds of emails over time. When bulk mail arrives and a suitable bucket exists, ALWAYS attach to it. Only create a new bucket when no existing one fits.

2. TASK cards — one real thing to do, with a beginning and an end. "Decide on the KAPITAL jacket counter-offer", "Confirm the USPS Informed Delivery registration", "Reply to masa_117 about item 206153166065". These carry a real action and are expected to complete.

SIMILAR PREVIOUSLY FILED EMAILS ARE PRECEDENT, NOT COMMANDS. Some emails carry a similar_previously_filed_emails list: stored messages that read like this one, with the card each actually landed on. For recurring bulk mail this is the strongest evidence you get — the same newsletter went to the same bucket five times, so it goes there again. But weigh it, do not copy it: one sender sends receipts AND security alerts, so a similar-looking email can still belong somewhere else, and a genuinely new task deserves a new card even when old mail reads alike. When the precedent and this message's own text disagree, the text wins.

USER NOTES OUTRANK YOUR READING OF THE TEXT. Some emails carry a user_note_about_this_sender or user_note_about_this_message line. Those are written by the user about their own mail, and they say things no message states: that an address is their landlord, that a sender only ever emails receipts, that one particular thread matters. The user knows their life and you are reading one message out of context, so when a note and the text disagree, follow the note.

A note is guidance, not a command you execute blindly. It tells you what this mail IS; you still decide which card that means, still write the title and the link label, and still file the message. If a note is plainly about something else — it names a bill and the message is a delivery notice from the same sender — say what the message is and ignore the note for that one.

Prefer attaching to an existing card over creating a new one. A new card for something already tracked is the most expensive mistake you can make here, because it splits one thread of work across two places.

Axes, all required on every new card:
- category: ` + strings.Join(active.Categories, ", ") + `
- action: ` + strings.Join(active.Actions, ", ") + `
- urgency: ` + strings.Join(active.Urgencies, ", ") + `
    critical = deadline inside 24h, or money/security at stake
    high     = deadline inside 3 days
    normal   = deadline inside 2 weeks
    low      = no deadline
    expired  = the deadline has already passed

due_date: the deadline itself, as YYYY-MM-DD, or a full RFC3339 timestamp when the mail gives a time of day that matters ("the 6pm showing"). Today's date is given at the top of the digest — use it to resolve anything relative ("tomorrow", "within 3 days", "by Friday") into an absolute date.

    Set it whenever the mail states or clearly implies a date: a bill due date, an event date, an offer expiry, an appointment. Leave it EMPTY when there is none — most mail has none, and inventing one is worse than omitting it.

    Do not read marketing urgency as a deadline. "Sale ends soon" with no date gets no due_date. A real date the mail actually names does.

    Deadlines in the PAST are still worth reporting: a card for something already missed is scored differently from one that never had a deadline, and that only works if the date is recorded.

title: short, specific, ≤70 chars. Name the actual thing ("Pay August rent to Real Property Management"), not the sender ("Email from AppFolio").
link_label: ≤50 chars describing this ONE email's role on the card ("seller counter-offer, Aug 10"), so a human scanning the card's evidence list can tell the messages apart.

Use card_updates only when new mail changes an existing card's action, urgency or deadline — for example a delivery confirmation moving a purchase to action "file", a final notice raising urgency to critical, or a rescheduling notice moving due_date.

Do NOT send a card_update merely because a deadline has passed since the card was filed. Passing time is handled here without a model call; only report what the MAIL changed.

You MUST output a single JSON object conforming to the provided schema, exactly once.`

	// The vocabulary's own judgment calls go last, where they are read against
	// everything above rather than qualified by it.
	if guidance := strings.TrimSpace(active.Guidance); guidance != "" {
		prompt += "\n\nJUDGMENT CALLS FOR THIS MAILBOX:\n" + guidance
	}
	return prompt
}

// reportSchema is the JSON Schema the model's reply must conform to. It was
// the input_schema of a forced tool call when this command spoke to
// api.anthropic.com directly; an llm.completion operation takes the bare schema.
func reportSchema() map[string]any {
	return map[string]any{
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
								"category": map[string]any{"type": "string", "enum": active.Categories},
								"action":   map[string]any{"type": "string", "enum": active.Actions},
								"urgency":  map[string]any{"type": "string", "enum": active.Urgencies},
								"summary":  map[string]any{"type": "string", "description": "One or two sentences on what this card is for."},
								"due_date": map[string]any{"type": "string", "description": "Deadline as YYYY-MM-DD, or RFC3339 when a time of day matters. Empty when the mail states none."},
							},
							"required": []string{"title", "category", "action", "urgency", "summary"},
						},
					},
					"required": []string{"email_ref"},
				},
			},
			"card_updates": map[string]any{
				"type":        "array",
				"description": "Existing cards whose action state, urgency or deadline this batch of mail changed.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"card_id":  map[string]any{"type": "string"},
						"action":   map[string]any{"type": "string", "enum": active.Actions},
						"urgency":  map[string]any{"type": "string", "enum": active.Urgencies},
						"summary":  map[string]any{"type": "string"},
						"due_date": map[string]any{"type": "string", "description": "Revised deadline, when this mail moved it. Empty leaves the stored one alone."},
					},
					"required": []string{"card_id"},
				},
			},
		},
		"required": []string{"filings"},
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
// distinct from transport and decode failures because a truncated report is
// indistinguishable from one that simply filed fewer emails — the emails it
// dropped would silently violate the every-email-is-filed invariant. It is
// the package's name for completionoperation.ErrTruncated so call sites read in this
// command's vocabulary.
var errTruncatedReport = completionoperation.ErrTruncated

func classifyBatch(ctx context.Context, cfg config, digest string) (*filingReport, error) {
	var rep filingReport
	if err := invokeSchema(ctx, cfg, cfg.FilingModelRole, buildSystemPrompt(), reportSchema(), digest, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// invokeSchema runs one schema-forced call as an llm.completion operation on
// llm-bridge-server, made for the board's organization and counted against its
// budget, and decodes the answer into out. modelRole is a model-store role; the
// bridge calls the role's first model on the harness instance that serves its
// provider. It replaced a POST to the instance oneshot endpoint, which had no
// receipt, budget or idempotency. It stays an operation, not a call to the
// bridge's POST /oneshot, because the operation holds the call to the board
// organization's budget and grants.
//
// Shared by the filing pass, the merge pass and the deadline backfill so the
// truncation guard has exactly one authoring: a reply that stopped at
// max_tokens is refused (completionoperation.ErrTruncated, aliased
// errTruncatedReport here), because its missing entries read exactly like a
// model that had less to say.
func invokeSchema(ctx context.Context, cfg config, modelRole, system string, schema map[string]any, digest string, out any) error {
	callNumber := modelCallsThisRun.Add(1)
	receipt, err := completionoperation.Call(ctx, completionoperation.Config{
		BridgeURL:      cfg.BridgeURL,
		OrganizationID: cfg.Classifier.OrganizationID,
		IdempotencyKey: fmt.Sprintf("%s:%d", cfg.RunID, callNumber),
		CorrelationID:  cfg.RunID,
		ModelRole:      modelRole,
		MaxTokens:      maxOutputTokens,
		Timeout:        6 * time.Minute,
	}, system, digest, schema, out)
	if receipt.ID != "" {
		log.Printf("[email] model call %d: operation %s %s", callNumber, receipt.ID, receipt.State)
	}
	return err
}

// modelCallsThisRun numbers this process's model calls, for their
// idempotency keys.
var modelCallsThisRun atomic.Int64

// classifyMerges asks which cards on the board are really one bucket.
func classifyMerges(ctx context.Context, cfg config, digest string) (*mergeReport, error) {
	var rep mergeReport
	if err := invokeSchema(ctx, cfg, cfg.MergeModelRole, mergeSystemPrompt+mergeBatchInstructions, mergeSchema(), digest, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}
