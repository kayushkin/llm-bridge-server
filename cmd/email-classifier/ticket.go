package main

import (
	"log"

	"github.com/slava/scheduler/internal/principalstore"
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
// A failure here is logged and counted, never fatal. The card is filed and
// correct without its ticket; refusing to file mail because principal-store
// was restarting would lose the filing as well as the ticket.

// ticketChannelEmail is kanban-store's channel for mail. Its vocabulary is
// served at GET /api/ticket-channels; this is the one value this binary uses.
const ticketChannelEmail = "email"

// writeTicket gives a newly created card its requester and channel.
//
// senderAddress is senderKey's output — lowercased and trimmed — and
// senderName is what the mail called them, used only if this is the first time
// that address has been seen.
func (a *applier) writeTicket(cardID, columnName, senderAddress, senderName string) {
	if a.contacts == nil || senderAddress == "" || columnName == columnNoAction {
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
	if err := put(cardID, contact.ID, ticketChannelEmail); err != nil {
		a.ticketsFailed++
		log.Printf("[email] WARNING: ticket for card %s (requester %s): %v", cardID, contact.ID, err)
		return
	}
	a.ticketsWritten++
	log.Printf("[email] card %s is a ticket from %s (%s)", cardID, contact.ID, senderAddress)
}

// contactResolver is what writeTicket needs, so a test can stand in for
// principal-store without an HTTP server.
type contactResolver interface {
	ResolveContact(email, displayName string) (principalstore.Contact, bool, error)
}

// ticketWriter is the kanban-store call, named so a test can watch it without
// a kanban-store. Nil means the real client, which is what production runs.
type ticketWriter func(cardID, requesterPrincipalID, channel string) error
