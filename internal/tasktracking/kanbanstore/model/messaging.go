package model

import (
	"fmt"
	"strings"
	"text/template"
	"time"
)

// ============================ Message triggers ============================
//
// A message trigger turns one kind of card action on one board into a text
// message to one person on a messaging app, delivered through multichat. It
// is board settings, edited on the board's settings page, and it is
// deliberately not an agent: a human wrote the template and chose the
// recipient, so the send goes out without a permission prompt.

// MessageTriggerEventKinds is the served vocabulary of actions a trigger can
// fire on — a subset of EventKind chosen because each one is something a
// person plausibly wants to hear about the moment it happens. Served by
// GET /api/message-trigger-event-kinds so no UI hardcodes it.
var MessageTriggerEventKinds = []EventKind{
	EventCardCreated,
	EventCardMoved,
	EventCardCompleted,
	EventAssigned,
	EventCardHeld,
}

// MessageTriggerColumnFilterEventKinds are the kinds a trigger may narrow to
// a destination column with to_column_id. Served alongside the event kinds so
// a UI knows when to offer the column picker without hardcoding it.
var MessageTriggerColumnFilterEventKinds = []EventKind{EventCardMoved}

func IsMessageTriggerEventKind(k EventKind) bool {
	return containsEventKind(MessageTriggerEventKinds, k)
}

func containsEventKind(kinds []EventKind, k EventKind) bool {
	for _, kind := range kinds {
		if kind == k {
			return true
		}
	}
	return false
}

// MessageTrigger is one rule: when EventKind happens to a card on BoardID
// (and the optional filters agree), render MessageTemplate and send it to
// RecipientUserID through multichat.
type MessageTrigger struct {
	ID      string `json:"id"`
	BoardID string `json:"board_id"`
	Name    string `json:"name"`
	// EventKind is one of MessageTriggerEventKinds.
	EventKind EventKind `json:"event_kind"`
	// ToColumnID narrows a card_moved trigger to moves INTO this column. Empty
	// means any column. Only meaningful with card_moved; refused otherwise.
	ToColumnID string `json:"to_column_id,omitempty"`
	// PriorityValue narrows the trigger to cards whose rung on the board's
	// priority ladder has this value (the rung's id — labels are renameable).
	// Nil means any priority, including unranked. The rung's label is carried
	// on the wire as priority_label for display only.
	PriorityValue *int   `json:"priority_value,omitempty"`
	PriorityLabel string `json:"priority_label,omitempty"`
	// RecipientUserID is multichat's id for the person: the bridge puppet id,
	// e.g. @whatsapp_15551234567:chat.kayushkin.com, from multichat's
	// GET /api/contacts/unified. RecipientDisplayName is for display only and
	// is never read back to find the person.
	RecipientUserID      string `json:"recipient_user_id"`
	RecipientDisplayName string `json:"recipient_display_name,omitempty"`
	// MessageTemplate is a Go text/template over MessageTemplateFields.
	MessageTemplate string    `json:"message_template"`
	Enabled         bool      `json:"enabled"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// MessageTemplateFields is what a trigger's template may reference.
type MessageTemplateFields struct {
	CardID         string
	Title          string
	BoardName      string
	ColumnName     string // the column the card is in after the action
	FromColumnName string // card_moved only
	PriorityLabel  string // "" when unranked or the board has no ladder
	EventKind      string
	Actor          string
	Summary        string
	AssigneeIDs    []string // principal ids
	OccurredAt     time.Time
}

// ParseMessageTemplate is the one parser for trigger templates, used both at
// write time (so a template that cannot render is refused with a 400) and at
// delivery, so what was accepted is what renders.
func ParseMessageTemplate(text string) (*template.Template, error) {
	return template.New("message").Option("missingkey=error").Parse(text)
}

// MessageTriggerOptions is what GET /api/message-trigger-options serves: the
// vocabularies a trigger editor needs and whether delivery is configured on
// this kanban-store at all, so a UI never hardcodes an event kind or a
// template field.
type MessageTriggerOptions struct {
	EventKinds             []EventKind `json:"event_kinds"`
	ColumnFilterEventKinds []EventKind `json:"column_filter_event_kinds"`
	TemplateFields         []string    `json:"template_fields"`
	DeliveryConfigured     bool        `json:"delivery_configured"`
}

// UpsertMessageTriggerRequest is the body of POST and PATCH. Every field is a
// pointer so PATCH can tell "not sent" from "cleared"; POST fills the
// required ones or is refused.
type UpsertMessageTriggerRequest struct {
	Name                 *string    `json:"name,omitempty"`
	EventKind            *EventKind `json:"event_kind,omitempty"`
	ToColumnID           *string    `json:"to_column_id,omitempty"`
	PriorityValue        *int       `json:"priority_value,omitempty"`
	ClearPriority        bool       `json:"clear_priority,omitempty"`
	RecipientUserID      *string    `json:"recipient_user_id,omitempty"`
	RecipientDisplayName *string    `json:"recipient_display_name,omitempty"`
	MessageTemplate      *string    `json:"message_template,omitempty"`
	Enabled              *bool      `json:"enabled,omitempty"`
}

// ApplyTo merges the request onto a trigger. ClearPriority wins over
// PriorityValue so a client can drop the filter without sending a sentinel.
func (r *UpsertMessageTriggerRequest) ApplyTo(t *MessageTrigger) {
	if r.Name != nil {
		t.Name = *r.Name
	}
	if r.EventKind != nil {
		t.EventKind = *r.EventKind
	}
	if r.ToColumnID != nil {
		t.ToColumnID = *r.ToColumnID
	}
	if r.ClearPriority {
		t.PriorityValue = nil
	} else if r.PriorityValue != nil {
		v := *r.PriorityValue
		t.PriorityValue = &v
	}
	if r.RecipientUserID != nil {
		t.RecipientUserID = *r.RecipientUserID
	}
	if r.RecipientDisplayName != nil {
		t.RecipientDisplayName = *r.RecipientDisplayName
	}
	if r.MessageTemplate != nil {
		t.MessageTemplate = *r.MessageTemplate
	}
	if r.Enabled != nil {
		t.Enabled = *r.Enabled
	}
}

// Validate checks everything that needs no other store. The column and the
// priority rung are checked against the board by the API, which has the store.
func (t *MessageTrigger) Validate() error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if !IsMessageTriggerEventKind(t.EventKind) {
		return fmt.Errorf("event_kind %q is not one of %s", t.EventKind, joinEventKinds(MessageTriggerEventKinds))
	}
	if t.ToColumnID != "" && !containsEventKind(MessageTriggerColumnFilterEventKinds, t.EventKind) {
		return fmt.Errorf("to_column_id only applies to %s, not %s", joinEventKinds(MessageTriggerColumnFilterEventKinds), t.EventKind)
	}
	if t.PriorityValue != nil && *t.PriorityValue == UnsetPriorityValue {
		return fmt.Errorf("priority_value %d is the unranked value and cannot be a filter; omit it to match any priority", UnsetPriorityValue)
	}
	if !strings.HasPrefix(t.RecipientUserID, "@") || !strings.Contains(t.RecipientUserID, ":") {
		return fmt.Errorf("recipient_user_id must be a multichat puppet id like @whatsapp_15551234567:chat.example.com, got %q", t.RecipientUserID)
	}
	if strings.TrimSpace(t.MessageTemplate) == "" {
		return fmt.Errorf("message_template is required")
	}
	if _, err := ParseMessageTemplate(t.MessageTemplate); err != nil {
		return fmt.Errorf("message_template: %v", err)
	}
	return nil
}

func joinEventKinds(kinds []EventKind) string {
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

// MessageDeliveryStatus is what became of one trigger firing on one event.
type MessageDeliveryStatus string

const (
	// DeliveryPending: claimed and handed to multichat, no answer recorded yet.
	// A row left here means kanban-store stopped mid-send; whether the message
	// went out is unknown, and the row says exactly that.
	DeliveryPending MessageDeliveryStatus = "pending"
	// DeliverySent: multichat accepted it; RoomID and MatrixEventID say where.
	DeliverySent MessageDeliveryStatus = "sent"
	// DeliveryFailed: rendering or multichat failed; Error says why.
	DeliveryFailed MessageDeliveryStatus = "failed"
	// DeliveryNotConfigured: the trigger matched and rendered but this
	// kanban-store has no MULTICHAT_URL, so nothing was sent. The row exists
	// so a trigger set up before the wiring is visibly firing into nothing
	// rather than silently doing nothing.
	DeliveryNotConfigured MessageDeliveryStatus = "not_configured"
)

// MessageDelivery is the audit row for one trigger on one event. The pair is
// unique: an event is delivered at most once per trigger, however many times
// the dispatcher sees it.
type MessageDelivery struct {
	ID              string                `json:"id"`
	TriggerID       string                `json:"trigger_id"`
	BoardID         string                `json:"board_id"`
	EventID         string                `json:"event_id"`
	CardID          string                `json:"card_id"`
	RecipientUserID string                `json:"recipient_user_id"`
	RenderedMessage string                `json:"rendered_message"`
	Status          MessageDeliveryStatus `json:"status"`
	Error           string                `json:"error,omitempty"`
	RoomID          string                `json:"room_id,omitempty"`
	MatrixEventID   string                `json:"matrix_event_id,omitempty"`
	CreatedAt       time.Time             `json:"created_at"`
}
