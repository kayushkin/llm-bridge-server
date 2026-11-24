package principalstore

import (
	"errors"
	"testing"
)

// TestResolveContactGivesOneRequesterOneID pins the reason contacts exist: a
// ticket points at its requester by id, so the second mail from an address
// must find the first mail's row rather than make another one.
func TestResolveContactGivesOneRequesterOneID(t *testing.T) {
	s := newTestStore(t)

	first, err := s.ResolveContact("Dana.Whitfield@example.com", "Dana Whitfield")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Contact.Kind != KindContact {
		t.Fatalf("first resolve should create a contact: %+v", first)
	}
	if first.Contact.Email != "dana.whitfield@example.com" {
		t.Fatalf("the address is stored lowercased so a later mail matches: %q", first.Contact.Email)
	}

	// Same person, different capitals, different name on the mail.
	again, err := s.ResolveContact("  DANA.WHITFIELD@example.com ", "D. Whitfield")
	if err != nil {
		t.Fatal(err)
	}
	if again.Created {
		t.Fatal("a second mail from one address must not create a second contact")
	}
	if again.Contact.ID != first.Contact.ID {
		t.Fatalf("ids differ: %s then %s", first.Contact.ID, again.Contact.ID)
	}
	if again.Contact.DisplayName != "Dana Whitfield" {
		t.Fatalf("an existing contact keeps its name, got %q", again.Contact.DisplayName)
	}

	// A human with the same address is a different row on purpose: one can log
	// in, the other must never be able to.
	human, err := s.Create(&Principal{Kind: KindHuman, DisplayName: "Dana at work", Email: "dana.whitfield@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.ResolveContact("dana.whitfield@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if third.Contact.ID == human.ID {
		t.Fatal("resolving a contact must never return a human")
	}

	if _, err := s.ResolveContact("", "Nobody"); !errors.Is(err, ErrInvalidContact) {
		t.Fatalf("an empty address must be refused, got %v", err)
	}
	if _, err := s.ResolveContact("not-an-address", "Nobody"); !errors.Is(err, ErrInvalidContact) {
		t.Fatalf("a string with no @ must be refused, got %v", err)
	}
}

// TestAContactActsOnNothing pins what the kind is for: a contact is the
// outside party on a ticket, never someone this deployment gives access to.
func TestAContactActsOnNothing(t *testing.T) {
	s := newTestStore(t)
	resolution, err := s.ResolveContact("outside@example.com", "Outside Person")
	if err != nil {
		t.Fatal(err)
	}
	contact := resolution.Contact

	if ActsInThisDeployment(KindContact) {
		t.Fatal("a contact must not act in this deployment")
	}
	if !ActsInThisDeployment(KindHuman) || !ActsInThisDeployment(KindGroup) {
		t.Fatal("a human and a group both act")
	}

	yes := true
	if _, err := s.Patch(contact.ID, Patch{IsAdministrator: &yes}); err == nil {
		t.Fatal("a contact must not be promoted to administrator")
	}

	group := mustCreate(t, s, KindGroup, "Support team", "")
	if _, err := s.AddMember(group.ID, contact.ID); err == nil {
		t.Fatal("a contact must not join a group")
	}
}
