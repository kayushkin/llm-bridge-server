package main

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/kanbanclient"
)

// noAgentGate is a gate pointed at a port nothing listens on, so a test that
// accidentally makes it dial fails loudly instead of hanging.
func noAgentGate() *sessionGate { return newSessionGate("http://127.0.0.1:1") }

func emailLink(id, locator string, at time.Time) kanbanclient.CardLink {
	return kanbanclient.CardLink{ID: id, EntityType: linkTypeEmail, EntityRef: locator, CreatedAt: at}
}

// TestRefileFindsOnlyTheMailItsCardNeverSaw pins the discriminator the whole
// pass rests on.
//
// A message that was fetched had its Message-ID recorded on the card it landed
// on. A message the retired sender rule placed has no such link, because the
// rule never fetched it. That difference — not the shape of the link label, not
// the column, not a guess — is what decides whether mail gets re-read.
func TestRefileFindsOnlyTheMailItsCardNeverSaw(t *testing.T) {
	recent := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	ancient := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	since := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)

	mailstack := fakeMailstack(t, map[string]messageFixture{
		"read":       {MessageID: "read@example.com", Subject: "Statement ready", Sender: "bank@example.com"},
		"unread":     {MessageID: "unread@example.com", Subject: "Address changed", Sender: "bank@example.com"},
		"old-unread": {MessageID: "old@example.com", Subject: "Older blast", Sender: "bank@example.com"},
	})
	defer mailstack.Close()

	card := &cardState{
		ID:         "bucket",
		Title:      "Bank marketing",
		ColumnName: columnNoAction,
		EmailLinks: []kanbanclient.CardLink{
			emailLink("link-read", "gmail-personal:read", recent),
			emailLink("link-unread", "gmail-personal:unread", recent),
			emailLink("link-old", "gmail-personal:old-unread", ancient),
		},
	}
	state := &boardState{
		Cards:            map[string]*cardState{card.ID: card},
		FiledLocators:    map[string]bool{},
		CardsByMessageID: map[string][]string{"read@example.com": {"bucket"}},
		CardsBySender:    map[string][]string{},
	}

	found, read, unreachable := findMailFiledUnread(
		newMailClient(mailstack.URL, "t"), state, noAgentGate(), since)

	if unreachable != 0 {
		t.Fatalf("unreachable = %d, want 0", unreachable)
	}
	if read != 1 {
		t.Errorf("filed-and-read = %d, want the one message whose Message-ID its card holds", read)
	}
	if len(found) != 1 {
		t.Fatalf("found %d candidates, want only the unread one inside the window: %+v", len(found), found)
	}
	if found[0].Link.EntityRef != "gmail-personal:unread" {
		t.Errorf("candidate = %s, want the message its card never saw", found[0].Link.EntityRef)
	}
	if found[0].Subject != "Address changed" {
		t.Errorf("Subject = %q, want the fetched subject so the log names the mail", found[0].Subject)
	}
}

// TestRefileLeavesMailItCannotRefetch guards the one way this pass could lose
// mail outright. Unfiling a message means the next tick has to fetch it to file
// it again — so if the fetch is failing now, unlinking it drops it off the board
// with nothing left pointing at it.
func TestRefileLeavesMailItCannotRefetch(t *testing.T) {
	at := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	since := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)

	// Serves nothing: every fetch 404s, standing in for a dead IMAP host.
	mailstack := fakeMailstack(t, map[string]messageFixture{})
	defer mailstack.Close()

	card := &cardState{
		ID: "bucket", Title: "Bank marketing", ColumnName: columnNoAction,
		EmailLinks: []kanbanclient.CardLink{emailLink("link-1", "imap-pretender:404", at)},
	}
	state := &boardState{
		Cards:            map[string]*cardState{card.ID: card},
		FiledLocators:    map[string]bool{},
		CardsByMessageID: map[string][]string{},
		CardsBySender:    map[string][]string{},
	}

	found, _, unreachable := findMailFiledUnread(
		newMailClient(mailstack.URL, "t"), state, noAgentGate(), since)

	if len(found) != 0 {
		t.Error("a message that cannot be fetched was queued for unfiling; the tick could not file it again")
	}
	if unreachable != 1 {
		t.Errorf("unreachable = %d, want 1 so the shortfall is reported rather than read as a clean pass", unreachable)
	}
}

// TestRefileWillNotStripACardAnAgentIsWorkingOn — the same rule every other
// mutation in this command obeys. Evidence pulled out from under a running
// session is worse than one message left filed where it does not belong.
func TestRefileWillNotStripACardAnAgentIsWorkingOn(t *testing.T) {
	at := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	since := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)

	mailstack := fakeMailstack(t, map[string]messageFixture{
		"unread": {MessageID: "unread@example.com", Subject: "Blast", Sender: "bank@example.com"},
	})
	defer mailstack.Close()

	card := &cardState{
		ID: "held", Title: "Held card", ColumnName: columnNoAction,
		EmailLinks: []kanbanclient.CardLink{emailLink("link-1", "gmail-personal:unread", at)},
		AllLinks:   []kanbanclient.CardLink{{EntityType: linkTypeSession, EntityRef: "session-1"}},
	}
	state := &boardState{
		Cards:            map[string]*cardState{card.ID: card},
		FiledLocators:    map[string]bool{},
		CardsByMessageID: map[string][]string{},
		CardsBySender:    map[string][]string{},
	}

	// gateAgainst freezes now, so the gate's dormancy window is not in play here:
	// model_generating is a working state either way.
	gate := gateAgainst(t, "model_generating", time.Date(2026, 8, 13, 11, 0, 0, 0, time.UTC))

	found, _, _ := findMailFiledUnread(newMailClient(mailstack.URL, "t"), state, gate, since)

	if len(found) != 0 {
		t.Error("mail was pulled off a card an agent is mid-turn on")
	}
}
