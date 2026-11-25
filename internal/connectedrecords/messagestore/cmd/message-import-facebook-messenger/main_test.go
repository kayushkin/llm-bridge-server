package main

import (
	"testing"
	"time"

	messagestore "github.com/kayushkin/message-store"
	"github.com/kayushkin/message-store/internal/facebookmessages"
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

func TestSendNamesPeopleLinksPhotosAndFilesJournalDays(t *testing.T) {
	zones, _ := residencezones.Parse("Europe/Lisbon")
	at := time.Date(2020, 5, 20, 23, 30, 0, 0, time.UTC) // 00:30 on the 21st in Lisbon
	thread := facebookmessages.Thread{Folder: "inbox", ID: "carol_111", Title: "Carol", Participants: []string{"Carol", "Me Self"}, ParticipantCount: 2,
		Messages: []facebookmessages.Message{
			{Ref: "inbox/carol_111/1/a", Sender: "Me Self", Time: at, Files: []facebookmessages.File{{Kind: "photo", URI: "x/photos/cat.jpg"}}},
			{Ref: "inbox/carol_111/2/b", Sender: "Carol", Time: at.Add(time.Minute), Content: "look", ShareLink: "https://example.com/a"},
			{Ref: "inbox/carol_111/3/c", Sender: "Carol", Time: at.Add(2 * time.Minute), IsCall: true, CallSeconds: 61},
		}}
	sink := &recorder{}
	err := send(sink, []facebookmessages.Thread{thread}, options{selfName: "Me Self", people: map[string]string{"Carol": "person_000003"}, zones: zones,
		photoID: func(file facebookmessages.File) (string, error) { return "photo_000004", nil }})
	if err != nil {
		t.Fatal(err)
	}
	conversation := sink.conversations[0]
	if conversation.SourceRef != "inbox/carol_111" || conversation.IsGroup || conversation.Participants[0].PersonID != "person_000003" || !conversation.Participants[1].IsSelf {
		t.Errorf("conversation = %+v", conversation)
	}
	picture := sink.messages[0]
	if picture.Kind != "media" || picture.Attachments[0] != (messagestore.Attachment{PhotoID: "photo_000004", MimeType: "image/jpeg", Name: "cat.jpg"}) {
		t.Errorf("picture = %+v", picture)
	}
	if link := sink.messages[1]; link.Body != "look\nhttps://example.com/a" || link.Kind != "text" {
		t.Errorf("link = %+v", link)
	}
	if call := sink.messages[2]; call.Kind != "call" || call.CallSeconds != 61 {
		t.Errorf("call = %+v", call)
	}
	if len(sink.days) != 1 || sink.days[0].JournalSourceRef != "inbox/carol_111/2020-05-21" || sink.days[0].TimeZone != "Europe/Lisbon" {
		t.Errorf("days = %+v", sink.days)
	}
}
