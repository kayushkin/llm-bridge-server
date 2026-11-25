package main

import (
	"strings"
	"testing"
	"time"

	messagestore "github.com/kayushkin/message-store"
	"github.com/kayushkin/message-store/internal/androidmessages"
	"github.com/kayushkin/message-store/internal/residencezones"
)

type recorder struct {
	conversations []messagestore.ImportConversation
	messages      []messagestore.ImportMessage
	days          []messagestore.ImportConversationDay
}

func (r *recorder) AddConversation(c messagestore.ImportConversation) error {
	r.conversations = append(r.conversations, c)
	return nil
}
func (r *recorder) AddMessage(m messagestore.ImportMessage) error {
	r.messages = append(r.messages, m)
	return nil
}
func (r *recorder) AddConversationDay(d messagestore.ImportConversationDay) error {
	r.days = append(r.days, d)
	return nil
}

func TestSendWritesMessagesCallsAndTheJournalDays(t *testing.T) {
	zones, err := residencezones.Parse("Europe/Lisbon,2023-11-16=America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	carol := "+12025550100"
	at := func(value string) time.Time {
		moment, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatal(err)
		}
		return moment
	}
	conversation := &androidmessages.Conversation{
		Others: []string{carol},
		Messages: []*androidmessages.Message{
			{Others: []string{carol}, Sender: carol, Time: at("2023-11-14T22:13:20Z"), Text: "hello", DedupeKey: "sms|a"},
			{Others: []string{carol}, FromSelf: true, Time: at("2023-11-14T22:14:20Z"), Text: "your verification code is 123456", DedupeKey: "sms|b"},
			{Others: []string{carol}, Sender: carol, Time: at("2023-11-17T03:00:00Z"), Picture: true, DedupeKey: "mms-id|c",
				Attachments: []androidmessages.Attachment{{ContentType: "image/png", Path: "/made/up.png", Sha256: "abc"}, {ContentType: "audio/amr"}}},
		},
		Calls: []*androidmessages.Call{
			{Number: carol, Time: at("2023-11-20T18:00:00Z"), DurationSeconds: 125, Type: androidmessages.CallOutgoing, DedupeKey: "call|d"},
			{Number: carol, Time: at("2023-11-21T18:00:00Z"), Type: androidmessages.CallMissed, DedupeKey: "call|e"},
		},
	}
	options := options{selfNumber: "+12025550199", selfName: "Me Self", zones: zones, journalSource: "phone-texts", foldIntoSource: "older-texts",
		folded: map[string]bool{carol + "/2023-11-14": true}, people: map[string]person{carol: {ID: "person_000012", DisplayName: "Carol"}},
		pictures: map[string]string{"abc": "photo_000001"}}
	sink := &recorder{}
	counts, err := send(sink, []*androidmessages.Conversation{conversation}, options)
	if err != nil {
		t.Fatal(err)
	}
	if counts.messages != 3 || counts.secrets != 1 || counts.calls != 2 || counts.days != 2 || counts.foldedDays != 1 {
		t.Errorf("counts = %+v", counts)
	}
	if len(sink.conversations) != 1 {
		t.Fatalf("conversations = %+v", sink.conversations)
	}
	c := sink.conversations[0]
	if c.Platform != "sms" || c.Source != "sms" || c.SourceRef != carol || c.Title != "Carol" || c.IsGroup || len(c.Participants) != 2 ||
		!c.Participants[0].IsSelf || c.Participants[1].PersonID != "person_000012" {
		t.Errorf("conversation = %+v", c)
	}
	for _, message := range sink.messages {
		if strings.Contains(message.SourceRef, "sms|") || !strings.HasPrefix(message.SourceRef, "message:") && !strings.HasPrefix(message.SourceRef, "call:") {
			t.Errorf("source_ref %q is not a hash", message.SourceRef)
		}
		if strings.Contains(message.Body, "123456") {
			t.Error("a login code kept its text")
		}
		if message.SourceHash == "" {
			t.Error("a message has no source hash")
		}
	}
	if got := sink.messages[1]; got.Body != "[a login code, left out]" || got.SenderHandle != "+12025550199" {
		t.Errorf("secret = %+v", got)
	}
	if got := sink.messages[2]; got.Kind != "media" || len(got.Attachments) != 2 || got.Attachments[0].PhotoID != "photo_000001" || got.Attachments[1].PhotoID != "" {
		t.Errorf("picture message = %+v", got)
	}
	if got := sink.messages[3]; got.Kind != "call" || got.CallSeconds != 125 || got.SenderHandle != "+12025550199" {
		t.Errorf("outgoing call = %+v", got)
	}
	if got := sink.messages[4]; got.Kind != "call" || got.Body != "missed call" || got.SenderHandle != carol {
		t.Errorf("missed call = %+v", got)
	}
	// Calls make no journal day: the journal importer wrote days of texts only.
	want := []messagestore.ImportConversationDay{
		{ConversationSource: "sms", ConversationSourceRef: carol, Day: "2023-11-14", TimeZone: "Europe/Lisbon", JournalSource: "older-texts", JournalSourceRef: carol + "/2023-11-14"},
		{ConversationSource: "sms", ConversationSourceRef: carol, Day: "2023-11-16", TimeZone: "America/Chicago", JournalSource: "phone-texts", JournalSourceRef: carol + "/2023-11-16"},
	}
	if len(sink.days) != len(want) {
		t.Fatalf("days = %+v", sink.days)
	}
	for index := range want {
		if sink.days[index] != want[index] {
			t.Errorf("day %d = %+v, want %+v", index, sink.days[index], want[index])
		}
	}
}
