package main

import (
	"log"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/principalstore"
)

// Mail becomes a ticket.
//
// A card this classifier files for a piece of work somebody asked for is a
// ticket: it has a requester and it arrived through a channel. kanban-store
// stores both — and stores the requester as a principal-store **contact id**,
// never as an address, because one person writes from a phone, a work account
// and a web form, and an address is a name.
//
// Two cards deliberately do not become tickets:
//
//   - A **bucket** card in the no-action column. Those collect mail from many
//     senders — newsletters, receipts, notifications — and a card with twenty
//     senders has no one requester. They keep the email_sender links they
//     always had, which is the right shape for a digest.
//   - A card that **already** has a ticket. The first requester stands: later
//     mail from a colleague who was copied in does not take the ticket over.
//
// A card the classifier did not create this tick becomes a ticket too, the
// first time mail lands on it after this shipped. Most mail joins a card that
// already exists — the first live run filed two messages and created no card —
// so writing tickets only for new cards would have left the board's existing
// work without requesters for as long as those cards live.
//
// A failure here is logged and counted, never fatal. The card is filed and
// correct without its ticket; refusing to file mail because principal-store
// was restarting would lose the filing as well as the ticket.

// ticketChannelEmail is kanban-store's channel for mail. Its vocabulary is
// served at GET /api/ticket-channels; this is the one value this binary uses.
const ticketChannelEmail = "email"

// writeTicket gives a card its requester, its channel and the message that
// made it a ticket.
//
// emailLocator is that message's mailstack locator, the ref its email link
// carries; kanban-store keeps it as the ticket's source, so the ticket log can
// say which email a ticket came from after merges have moved the links.
// senderAddress is senderKey's output — lowercased and trimmed — and
// senderName is what the mail called them, used only if this is the first time
// that address has been seen.
func (a *applier) writeTicket(cardID, columnName, emailLocator, senderAddress, senderName string) {
	if a.contacts == nil || senderAddress == "" || columnName == columnNoAction {
		return
	}
	if card := a.cardState(cardID); card != nil && card.HasTicket {
		return
	}
	contact, created, err := a.contacts.ResolveContact(senderAddress, senderName)
	if err != nil {
		a.ticketsFailed++
		log.Printf("[email] WARNING: no ticket for card %s: resolving %s: %v", cardID, senderAddress, err)
		return
	}
	if created {
		log.Printf("[email] new contact %s for %s", contact.ID, senderAddress)
	}
	put := a.putTicket
	if put == nil {
		put = a.kc.PutTicket
	}
	if err := put(cardID, contact.ID, ticketChannelEmail, linkTypeEmail, emailLocator); err != nil {
		a.ticketsFailed++
		log.Printf("[email] WARNING: ticket for card %s (requester %s): %v", cardID, contact.ID, err)
		return
	}
	a.ticketsWritten++
	if card := a.cardState(cardID); card != nil {
		card.HasTicket = true
	}
	log.Printf("[email] card %s is a ticket from %s (%s) via %s", cardID, contact.ID, senderAddress, emailLocator)
}

// contactResolver is what writeTicket needs, so a test can stand in for
// principal-store without an HTTP server.
type contactResolver interface {
	ResolveContact(email, displayName string) (principalstore.Contact, bool, error)
}

// ticketWriter is the kanban-store call, named so a test can watch it without
// a kanban-store. Nil means the real client, which is what production runs.
type ticketWriter func(cardID, requesterPrincipalID, channel, sourceEntityType, sourceEntityRef string) error

// cardState is the board's record of one card, or nil when this applier has no
// board state — which is the case in the unit tests and on the dry-run path.
func (a *applier) cardState(cardID string) *cardState {
	if a.state == nil {
		return nil
	}
	return a.state.Cards[cardID]
}
