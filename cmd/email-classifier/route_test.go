package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// messageFixture is one message the fake mailstack will serve, keyed by the
// provider message id the pendingEmail carries.
type messageFixture struct {
	MessageID string
	InReplyTo string
}

// fakeMailstack serves just the single-message route, which is all
// routeByMessageIdentity calls.
func fakeMailstack(t *testing.T, fixtures map[string]messageFixture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/messages/")
		fixture, ok := fixtures[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		headers := map[string]string{"Message-ID": "<" + fixture.MessageID + ">"}
		if fixture.InReplyTo != "" {
			headers["In-Reply-To"] = "<" + fixture.InReplyTo + ">"
		}
		json.NewEncoder(w).Encode(fullMessage{
			Meta:     messageMeta{ID: id},
			BodyText: "body",
			Headers:  headers,
		})
	}))
}

func bucketCardFedBy(sender string) *boardState {
	card := &cardState{
		ID:         "bucket-card",
		Title:      "Wells Fargo Deals eligibility update",
		ColumnName: columnNoAction,
		Senders:    []string{sender},
	}
	return &boardState{
		Cards:            map[string]*cardState{card.ID: card},
		FiledLocators:    map[string]bool{},
		CardsByMessageID: map[string][]string{},
		CardsBySender:    map[string][]string{sender: {card.ID}},
	}
}

// TestMailFromAKnownSenderIsReadRatherThanFiledOnThatSendersCard guards the rule
// this command no longer has.
//
// A learned rule used to send an address's mail straight to whichever bucket
// card their previous message landed on — no fetch, no model, no reading. But
// wellsfargoonline@mail1.wellsfargo.com sends a marketing blast and an
// address-change alert from the same mailbox, and both were filed onto "Wells
// Fargo Deals eligibility update" unread. One address is not one topic. The only
// thing a repeat sender earns now is a line in the model's digest.
func TestMailFromAKnownSenderIsReadRatherThanFiledOnThatSendersCard(t *testing.T) {
	const sender = "wellsfargoonline@mail1.wellsfargo.com"
	state := bucketCardFedBy(sender)

	mailstack := fakeMailstack(t, map[string]messageFixture{
		"alert-1": {MessageID: "address-change@wellsfargo.com"},
	})
	defer mailstack.Close()

	email := pendingEmail{
		Meta:    messageMeta{ID: "alert-1", AccountID: "gmail-personal", From: mailAddress{Email: sender}},
		Locator: "gmail-personal:alert-1",
	}

	routed, remaining := routeByMessageIdentity(newMailClient(mailstack.URL, "test-token"), state, []pendingEmail{email})

	if len(routed) != 0 {
		t.Fatalf("mail was filed without being classified, onto %v — a repeat sender is not a repeat topic", routed[0].CardIDs)
	}
	if len(remaining) != 1 {
		t.Fatalf("remaining = %d, want the message handed to the model", len(remaining))
	}
	if remaining[0].Full == nil {
		t.Error("the message reached the model unfetched, so the model would classify it from a snippet")
	}
	if remaining[0].MessageID != "address-change@wellsfargo.com" {
		t.Errorf("MessageID = %q, want it recorded so replies and cross-account copies can find this card later",
			remaining[0].MessageID)
	}
}

// TestMessageIdentityStillPlacesDuplicatesAndReplies pins the two paths that
// survived. Both are facts stated in the headers rather than guesses about
// content, which is exactly what separates them from the sender rule.
func TestMessageIdentityStillPlacesDuplicatesAndReplies(t *testing.T) {
	state := bucketCardFedBy("someone@example.com")
	state.CardsByMessageID["original@example.com"] = []string{"bucket-card"}

	mailstack := fakeMailstack(t, map[string]messageFixture{
		"copy":  {MessageID: "original@example.com"},
		"reply": {MessageID: "reply@example.com", InReplyTo: "original@example.com"},
	})
	defer mailstack.Close()

	emails := []pendingEmail{
		{Meta: messageMeta{ID: "copy", AccountID: "imap-pretender"}, Locator: "imap-pretender:copy"},
		{Meta: messageMeta{ID: "reply", AccountID: "gmail-personal"}, Locator: "gmail-personal:reply"},
	}

	routed, remaining := routeByMessageIdentity(newMailClient(mailstack.URL, "test-token"), state, emails)

	if len(remaining) != 0 {
		t.Fatalf("%d message(s) went to the model that the headers already placed", len(remaining))
	}
	if len(routed) != 2 {
		t.Fatalf("routed = %d, want the duplicate and the reply both placed", len(routed))
	}
	for _, r := range routed {
		if len(r.CardIDs) != 1 || r.CardIDs[0] != "bucket-card" {
			t.Errorf("%s went to %v, want the card holding the message it belongs to", r.Email.Locator, r.CardIDs)
		}
	}
}
