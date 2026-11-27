package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func notesServing(t *testing.T, items []noteItem) *notesClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A bare array, which is what notes actually sends. An earlier
		// version of this fake wrapped it in {"items": …}; the tests passed and
		// the live run failed on the first call.
		if items == nil {
			items = []noteItem{}
		}
		json.NewEncoder(w).Encode(items)
	}))
	t.Cleanup(srv.Close)
	return newNotesClient(srv.URL)
}

func pendingFrom(address, locator, subject string) pendingEmail {
	return pendingEmail{
		Meta:    messageMeta{From: mailAddress{Email: address}, Subject: subject, Date: time.Now()},
		Locator: locator,
	}
}

// TestARoutingNoteReachesTheModel is the point of the whole feature. A note the
// user wrote is worthless if it never reaches the digest the model reads.
func TestARoutingNoteReachesTheModel(t *testing.T) {
	nb := notesServing(t, []noteItem{{
		ID:    "note-1",
		Body:  "This is my landlord. Anything from here needs a reply.",
		Links: []string{"email_sender:landlord@example.com"},
	}})
	notes, err := loadRoutingNotes(nb)
	if err != nil {
		t.Fatalf("loadRoutingNotes: %v", err)
	}

	state := &boardState{Cards: map[string]*cardState{}}
	batch := []pendingEmail{
		pendingFrom("landlord@example.com", "gmail-personal:a1", "Rent"),
		pendingFrom("promos@shop.example", "gmail-personal:b2", "Sale"),
	}
	digest := buildDigest(state, notes, batch)

	if !strings.Contains(digest, "This is my landlord") {
		t.Error("the note never reached the digest, so the model files as if it did not exist")
	}
	// The note must attach to the right message. A note bleeding onto every
	// email in the batch is worse than no note.
	landlord := digest[strings.Index(digest, "gmail-personal:a1"):strings.Index(digest, "gmail-personal:b2")]
	if !strings.Contains(landlord, "user_note_about_this_sender") {
		t.Error("the note did not land on the email it is about")
	}
	if strings.Count(digest, "user_note_about_this_sender") != 1 {
		t.Error("the note leaked onto mail from other senders")
	}
}

// TestASenderNoteAndAMessageNoteBothSurvive — they say different things. A note
// about one message does not cancel the standing instruction about the address.
func TestASenderNoteAndAMessageNoteBothSurvive(t *testing.T) {
	nb := notesServing(t, []noteItem{
		{ID: "n1", Body: "Only ever sends receipts.", Links: []string{"email_sender:billing@example.com"}},
		{ID: "n2", Body: "This one is the disputed charge.", Links: []string{"email:gmail-personal:a1"}},
	})
	notes, _ := loadRoutingNotes(nb)
	digest := buildDigest(&boardState{Cards: map[string]*cardState{}}, notes,
		[]pendingEmail{pendingFrom("billing@example.com", "gmail-personal:a1", "Charge")})

	for _, want := range []string{"Only ever sends receipts.", "This one is the disputed charge."} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest lost %q", want)
		}
	}
}

// TestMailWithNoNoteGetsNoNoteLine — an empty label in every email would train
// the model to skim past the field on the messages that do carry one.
func TestMailWithNoNoteGetsNoNoteLine(t *testing.T) {
	notes, _ := loadRoutingNotes(notesServing(t, nil))
	digest := buildDigest(&boardState{Cards: map[string]*cardState{}}, notes,
		[]pendingEmail{pendingFrom("anyone@example.com", "gmail-personal:a1", "Hello")})
	if strings.Contains(digest, "user_note") {
		t.Error("an email with no note still got a note line")
	}
}

// TestNotesAreIndexedCaseInsensitivelyByAddress. Addresses arrive from providers
// in whatever case the sender used; a note written as Landlord@Example.com must
// still match mail from landlord@example.com.
func TestNotesAreIndexedCaseInsensitivelyByAddress(t *testing.T) {
	nb := notesServing(t, []noteItem{
		{ID: "n1", Body: "the landlord", Links: []string{"email_sender:Landlord@Example.com"}},
	})
	notes, _ := loadRoutingNotes(nb)
	if _, ok := notes.forSender("landlord@example.com"); !ok {
		t.Error("a note written with capitals does not match the same address in lowercase")
	}
}

// TestUnrelatedLinksOnANoteAreIgnored. A noteboard note may link to anything;
// only the two email entity types mean something here.
func TestUnrelatedLinksOnANoteAreIgnored(t *testing.T) {
	nb := notesServing(t, []noteItem{
		{ID: "n1", Body: "about a repo", Links: []string{"repo:scheduler", "session:br_123"}},
	})
	notes, err := loadRoutingNotes(nb)
	if err != nil {
		t.Fatalf("loadRoutingNotes: %v", err)
	}
	if notes.count() != 0 {
		t.Errorf("count = %d, want 0 — a note about a repo is not a routing note", notes.count())
	}
}

func TestEntityRefForRefusesTheAmbiguousAndTheMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config
		want string
	}{
		{"both", config{SenderQuery: "a@b.com", MessageQuery: "acct:1"}, "pass one"},
		{"neither", config{}, "pass -sender or -message"},
		{"not an address", config{SenderQuery: "landlord"}, "not an email address"},
		{"bad locator", config{MessageQuery: "no-colon"}, "must be account:message_id"},
	} {
		if _, err := entityRefFor(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", tc.name, err, tc.want)
		}
	}
	ref, err := entityRefFor(config{SenderQuery: "  Landlord@Example.com "})
	if err != nil || ref != "email_sender:landlord@example.com" {
		t.Errorf("ref = %q, err = %v — want the address trimmed and lowercased", ref, err)
	}
	ref, err = entityRefFor(config{MessageQuery: "gmail-personal:19fc857e"})
	if err != nil || ref != "email:gmail-personal:19fc857e" {
		t.Errorf("ref = %q, err = %v", ref, err)
	}
}
