package model

import (
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/kanban-store/internal/boundedtext"
)

// A ticket is a card that came from outside.
//
// The card is still the card: its title, body and tags live in noteboard, its
// placement and history live here. A ticket adds the two facts a card has no
// room for — who asked, and how it reached us — and nothing else.
//
// What it deliberately does not add is a status. A ticket's lifecycle is the
// column it sits in (see Column.LifecycleState): a board already says where
// work is by where the card is, and a second writable status would disagree
// with the first one within a week. TicketPlacementState below derives it.

// TicketChannel is how a ticket reached us.
type TicketChannel string

const (
	TicketChannelEmail  TicketChannel = "email"
	TicketChannelPortal TicketChannel = "portal"
	TicketChannelChat   TicketChannel = "chat"
	TicketChannelPhone  TicketChannel = "phone"
	// TicketChannelAgent is a ticket an agent raised on someone's behalf.
	TicketChannelAgent TicketChannel = "agent"
)

// TicketChannels is the whole vocabulary, in the order GET /api/ticket-channels
// serves it. Served rather than inferred from the rows, so no caller builds a
// filter out of whichever channels happen to have been used so far.
var TicketChannels = []TicketChannel{
	TicketChannelEmail, TicketChannelPortal, TicketChannelChat, TicketChannelPhone, TicketChannelAgent,
}

// NormalizeTicketChannel resolves a caller's word to a channel.
func NormalizeTicketChannel(raw string) (TicketChannel, bool) {
	key := TicketChannel(strings.ToLower(strings.TrimSpace(raw)))
	for _, channel := range TicketChannels {
		if key == channel {
			return channel, true
		}
	}
	return "", false
}

// ErrUnknownTicketChannel names the whole vocabulary, because a caller who
// guessed wrong cannot guess right from a bare rejection.
func ErrUnknownTicketChannel(raw string) error {
	names := make([]string, 0, len(TicketChannels))
	for _, channel := range TicketChannels {
		names = append(names, string(channel))
	}
	return fmt.Errorf("unknown channel %q: use one of %s", raw, strings.Join(names, ", "))
}

// TicketLifecycleState is where a ticket stands. It is a property of a
// **column**, not of the ticket: classify the columns of a board once, and
// every card in them reports its state from where it sits.
type TicketLifecycleState string

const (
	// TicketLifecycleNew — arrived, nobody has picked it up.
	TicketLifecycleNew TicketLifecycleState = "new"
	// TicketLifecycleOpen — someone here is working on it.
	TicketLifecycleOpen TicketLifecycleState = "open"
	// TicketLifecycleWaitingOnRequester — we answered; the ball is theirs.
	TicketLifecycleWaitingOnRequester TicketLifecycleState = "waiting_on_requester"
	// TicketLifecycleResolved — we believe it is done; the requester has not
	// said otherwise yet.
	TicketLifecycleResolved TicketLifecycleState = "resolved"
	// TicketLifecycleClosed — over, and not coming back without a new ticket.
	TicketLifecycleClosed TicketLifecycleState = "closed"
)

// TicketLifecycleStates is the whole vocabulary, in the order a ticket usually
// travels it and the order GET /api/ticket-lifecycle-states serves it.
var TicketLifecycleStates = []TicketLifecycleState{
	TicketLifecycleNew, TicketLifecycleOpen, TicketLifecycleWaitingOnRequester,
	TicketLifecycleResolved, TicketLifecycleClosed,
}

// NormalizeTicketLifecycleState resolves a caller's word to a state.
func NormalizeTicketLifecycleState(raw string) (TicketLifecycleState, bool) {
	key := TicketLifecycleState(strings.ToLower(strings.TrimSpace(raw)))
	for _, state := range TicketLifecycleStates {
		if key == state {
			return state, true
		}
	}
	return "", false
}

// ErrUnknownTicketLifecycleState names the whole vocabulary.
func ErrUnknownTicketLifecycleState(raw string) error {
	names := make([]string, 0, len(TicketLifecycleStates))
	for _, state := range TicketLifecycleStates {
		names = append(names, string(state))
	}
	return fmt.Errorf("unknown lifecycle_state %q: use one of %s", boundedtext.Value(raw), strings.Join(names, ", "))
}

// Ticket is the stored row: the two facts a card cannot hold.
type Ticket struct {
	CardID string `json:"card_id"`
	// RequesterPrincipalID is a principal-store **contact** — the outside
	// person who asked. Checked with principal-store on write, and stored as
	// an id: an address is a name, and one person writes from three of them.
	RequesterPrincipalID string        `json:"requester_principal_id"`
	Channel              TicketChannel `json:"channel"`
	// SourceEntityType and SourceEntityRef name the record that made this card
	// a ticket — for an email ticket, the message: type "email", ref
	// "<account_id>:<provider message id>", the same pair its card link
	// carries. Written once and never changed, unlike the card's links, which
	// merges and refiles move: this is the record of where the ticket came
	// from, and the links are where its mail sits now. Both are empty on a
	// ticket written before the field existed, or by a caller with no source.
	SourceEntityType string    `json:"source_entity_type,omitempty"`
	SourceEntityRef  string    `json:"source_entity_ref,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// TicketPlacementState is the lifecycle a ticket has **on one board**, read
// from the column the card sits in there.
type TicketPlacementState struct {
	BoardID    string `json:"board_id"`
	ColumnID   string `json:"column_id"`
	ColumnName string `json:"column_name"`
	// LifecycleState is absent when that column has not been classified. That
	// is the honest answer for a board whose columns nobody has mapped: it is
	// not "new", and saying "new" would start an SLA clock on a guess.
	LifecycleState *TicketLifecycleState `json:"lifecycle_state,omitempty"`
}

// TicketView is what a reader gets: the row, the requester's name for display
// only, and one lifecycle per board the card sits on.
//
// Plural on purpose. A card can be on two boards at once — the mail board it
// arrived on and the team board it was pulled onto — and those columns can
// disagree. Answering every placement lets the reader say which board it is
// asking about; answering one would be this store picking, and picking wrong
// is indistinguishable from picking right until someone is paged.
type TicketView struct {
	Ticket *Ticket `json:"ticket"`
	// RequesterDisplayName is a copy of principal-store's name, for rendering
	// a board without a second fetch per card. Nothing joins on it: the id
	// above is the only handle, and this string is stale the moment the
	// contact is renamed.
	RequesterDisplayName string                 `json:"requester_display_name,omitempty"`
	States               []TicketPlacementState `json:"states"`
}

// TicketWriteRequest is the body of PUT /api/cards/{id}/ticket.
type TicketWriteRequest struct {
	RequesterPrincipalID string `json:"requester_principal_id"`
	Channel              string `json:"channel"`
	// SourceEntityType and SourceEntityRef are optional and go together. A
	// ticket that already has a source keeps it: a write that omits the pair
	// leaves it alone, the same pair again is accepted, and a different pair is
	// a 409.
	SourceEntityType string `json:"source_entity_type,omitempty"`
	SourceEntityRef  string `json:"source_entity_ref,omitempty"`
}

// TicketLogEntry is one row of GET /api/tickets: a ticket, its lifecycle, and
// when its card was created, so a reader can tell a ticket that arrived as a
// new card from an existing card that became a ticket.
type TicketLogEntry struct {
	TicketView `tstype:",extends"`
	// CardCreatedAt is the card's card_created event. Absent when the card
	// predates that event (2026-08-21); such a card is older than any ticket.
	CardCreatedAt *time.Time `json:"card_created_at,omitempty"`
}

// TicketList is the body of GET /api/tickets, newest ticket first.
// NextBefore is the created_at to pass as ?before= for the next page, absent
// on the last page.
type TicketList struct {
	Tickets    []TicketLogEntry `json:"tickets"`
	NextBefore *time.Time       `json:"next_before,omitempty"`
}
