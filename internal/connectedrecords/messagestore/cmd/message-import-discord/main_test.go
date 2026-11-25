package main

import (
	"testing"
	"time"

	messagestore "github.com/kayushkin/message-store"
	"github.com/kayushkin/message-store/internal/discordpackage"
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

func TestSendKeysMessagesBySnowflakeAndFilesDaysWhereTheOwnerLived(t *testing.T) {
	zones, err := residencezones.Parse("Europe/Lisbon,2021-07-11=America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	at := func(text string) time.Time {
		parsed, _ := time.Parse(time.RFC3339, text)
		return parsed
	}
	channel := discordpackage.Channel{ID: "100", Type: discordpackage.TypeDirectMessage, IndexName: "Direct Message with carol_c", Recipients: []string{"9", "1"},
		Messages: []discordpackage.Message{
			{ID: "12", Time: at("2021-07-11T03:00:00Z"), Contents: "after the move"},
			{ID: "11", Time: at("2021-07-10T15:00:00Z"), Attachments: []discordpackage.Attachment{{ID: "555", Name: "cat.png", ContentType: "image/png"}}},
			{ID: "13", Time: at("2021-07-12T15:00:00Z"), AuthorID: "1", AuthorName: "Carol", IsCall: true, CallSeconds: 60, Event: "call"},
		}}
	sink := &recorder{}
	err = send(sink, []discordpackage.Channel{channel}, options{selfID: "9", selfName: "Me Self", journalSource: "discord-package",
		people: map[string]string{"carol_c": "person_000003"}, zones: zones, photoIDs: map[string]string{"555": "photo_000010"}})
	if err != nil {
		t.Fatal(err)
	}
	participants := sink.conversations[0].Participants
	if len(participants) != 2 || !participants[0].IsSelf || participants[1].Handle != "1" || participants[1].PersonID != "person_000003" || participants[1].DisplayName != "Carol" {
		t.Errorf("participants = %+v", participants)
	}
	if len(sink.messages) != 3 {
		t.Fatalf("messages = %d", len(sink.messages))
	}
	picture := sink.messages[0]
	if picture.SourceRef != "11" || picture.Source != "discord" || picture.Kind != "media" || picture.SenderHandle != "9" || picture.Attachments[0].PhotoID != "photo_000010" {
		t.Errorf("picture = %+v", picture)
	}
	if call := sink.messages[2]; call.Kind != "call" || call.CallSeconds != 60 || call.SenderHandle != "1" {
		t.Errorf("call = %+v", call)
	}
	// 15:00 UTC on the 10th is Lisbon; 03:00 UTC on the 11th is after the move,
	// so it is still the 10th in Chicago and joins that day.
	if len(sink.days) != 2 || sink.days[0].JournalSourceRef != "100/2021-07-10" || sink.days[0].TimeZone != "Europe/Lisbon" || sink.days[1].TimeZone != "America/Chicago" {
		t.Errorf("days = %+v", sink.days)
	}
}
