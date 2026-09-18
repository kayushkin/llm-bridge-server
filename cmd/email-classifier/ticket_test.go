package main

import (
	"errors"
	"testing"

	"github.com/slava/scheduler/internal/principalstore"
)

// fakeContacts stands in for principal-store's POST /contacts/resolve.
type fakeContacts struct {
	idByAddress map[string]string
	namesSeen   map[string]string
	failure     error
	calls       int
}

func (f *fakeContacts) ResolveContact(email, displayName string) (principalstore.Contact, bool, error) {
	f.calls++
	if f.failure != nil {
		return principalstore.Contact{}, false, f.failure
	}
	if f.namesSeen == nil {
		f.namesSeen = map[string]string{}
	}
	f.namesSeen[email] = displayName
	id, known := f.idByAddress[email]
	if !known {
		id = "principal_00001" + string(rune('0'+len(f.idByAddress)))
		if f.idByAddress == nil {
			f.idByAddress = map[string]string{}
		}
		f.idByAddress[email] = id
		return principalstore.Contact{ID: id, Kind: "contact", Email: email}, true, nil
	}
	return principalstore.Contact{ID: id, Kind: "contact", Email: email}, false, nil
}

// recordingTicketWriter captures what writeTicket asked kanban-store for.
type ticketWrite struct{ cardID, requester, channel string }

func TestOnlyWorkSomebodyAskedForBecomesATicket(t *testing.T) {
	contacts := &fakeContacts{}
	writes := []ticketWrite{}
	classifier := &applier{contacts: contacts}
	classifier.putTicket = func(cardID, requester, channel string) error {
		writes = append(writes, ticketWrite{cardID, requester, channel})
		return nil
	}

	// A card in an action column is a ticket from the person who wrote in.
	classifier.writeTicket("card-1", columnActionNeeded, "dana@example.com", "Dana Whitfield")
	if len(writes) != 1 || writes[0].cardID != "card-1" || writes[0].channel != ticketChannelEmail {
		t.Fatalf("an action card should become a ticket: %+v", writes)
	}
	if contacts.namesSeen["dana@example.com"] != "Dana Whitfield" {
		t.Fatalf("the sender's name should reach principal-store: %+v", contacts.namesSeen)
	}
	if classifier.ticketsWritten != 1 {
		t.Fatalf("tickets_written = %d", classifier.ticketsWritten)
	}

	// A bucket card in the no-action column is not one ticket: it collects mail
	// from many senders, so it has no one requester.
	classifier.writeTicket("card-2", columnNoAction, "newsletter@example.com", "A Newsletter")
	if len(writes) != 1 {
		t.Fatalf("a no-action bucket card must not become a ticket: %+v", writes)
	}

	// The same person writing again resolves to the same contact.
	classifier.writeTicket("card-3", columnActionNeeded, "dana@example.com", "D. Whitfield")
	if writes[1].requester != writes[0].requester {
		t.Fatalf("one address is one requester: %+v", writes)
	}
}

func TestAFailedResolveCostsTheTicketAndNotTheFiling(t *testing.T) {
	contacts := &fakeContacts{failure: errors.New("principal-store is restarting")}
	classifier := &applier{contacts: contacts}
	classifier.putTicket = func(string, string, string) error {
		t.Fatal("kanban-store must not be asked when the requester could not be resolved")
		return nil
	}
	classifier.writeTicket("card-1", columnActionNeeded, "dana@example.com", "Dana")
	if classifier.ticketsFailed != 1 || classifier.ticketsWritten != 0 {
		t.Fatalf("a failure is counted, not fatal: written=%d failed=%d", classifier.ticketsWritten, classifier.ticketsFailed)
	}

	// And a kanban-store refusal is counted the same way.
	refusing := &applier{contacts: &fakeContacts{}}
	refusing.putTicket = func(string, string, string) error { return errors.New("400 refused") }
	refusing.writeTicket("card-1", columnActionNeeded, "dana@example.com", "Dana")
	if refusing.ticketsFailed != 1 {
		t.Fatalf("a refused ticket is counted: %d", refusing.ticketsFailed)
	}
}

// TestNoResolverLeavesCardsAlone — a run configured without principal-store
// files mail exactly as it did before tickets existed.
func TestNoResolverLeavesCardsAlone(t *testing.T) {
	classifier := &applier{}
	classifier.putTicket = func(string, string, string) error {
		t.Fatal("no resolver means no ticket")
		return nil
	}
	classifier.writeTicket("card-1", columnActionNeeded, "dana@example.com", "Dana")
	if classifier.ticketsWritten != 0 || classifier.ticketsFailed != 0 {
		t.Fatal("nothing should be counted")
	}
}

// TestACardKeepsItsFirstRequester pins the rule that makes this safe to run on
// a board of existing cards: mail landing on a card with no requester makes it
// a ticket, and mail landing on one that has a requester changes nothing.
func TestACardKeepsItsFirstRequester(t *testing.T) {
	contacts := &fakeContacts{}
	writes := []ticketWrite{}
	classifier := &applier{
		contacts: contacts,
		state: &boardState{Cards: map[string]*cardState{
			"card-new":  {ID: "card-new", ColumnName: columnActionNeeded},
			"card-held": {ID: "card-held", ColumnName: columnActionNeeded, HasTicket: true},
		}},
	}
	classifier.putTicket = func(cardID, requester, channel string) error {
		writes = append(writes, ticketWrite{cardID, requester, channel})
		return nil
	}

	classifier.writeTicket("card-new", columnActionNeeded, "dana@example.com", "Dana")
	if len(writes) != 1 || writes[0].cardID != "card-new" {
		t.Fatalf("an existing card with no requester becomes a ticket: %+v", writes)
	}
	if !classifier.state.Cards["card-new"].HasTicket {
		t.Fatal("the card should be remembered as having a ticket now")
	}

	// A second message on the same card does not re-write it.
	classifier.writeTicket("card-new", columnActionNeeded, "someone-else@example.com", "Copied In")
	if len(writes) != 1 {
		t.Fatalf("the first requester stands: %+v", writes)
	}
	// Nor does mail on a card that already had one when the board was loaded.
	classifier.writeTicket("card-held", columnActionNeeded, "someone-else@example.com", "Copied In")
	if len(writes) != 1 {
		t.Fatalf("a card that arrived with a ticket keeps it: %+v", writes)
	}
	if contacts.calls != 1 {
		t.Fatalf("principal-store should be asked once, not per message: %d", contacts.calls)
	}
}
