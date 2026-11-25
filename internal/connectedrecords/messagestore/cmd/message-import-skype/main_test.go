package main

import (
	"strconv"
	"strings"
	"testing"
	"time"

	messagestore "github.com/kayushkin/message-store"
	"github.com/kayushkin/message-store/internal/skypelog"
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

func record(number int, kind, at, user, display, duration, message, chat string) string {
	return "==================================================\r\n" +
		"Record Number     : " + itoa(number) + "\r\nAction Type       : " + kind + "\r\nAction Time       : " + at + "\r\n" +
		"End Time          : \r\nUser Name         : " + user + "\r\nDisplay Name      : " + display + "\r\nDuration          : " + duration + "\r\n" +
		"Chat Message      : " + message + "\r\nChatID            : " + chat + "\r\nFilename          : \r\n" +
		"==================================================\r\n"
}

func itoa(number int) string { return strconv.Itoa(number) }

func TestSendWritesMessagesCallsAndTheJournalDays(t *testing.T) {
	lisbon, _ := time.LoadLocation("Europe/Lisbon")
	export := record(10, "Chat Message", "9/14/2015 11:05:31 PM", "carol_c", "Carol", "", "hello", "#carol_c/$me_self;ab12") +
		record(11, "Chat Message", "9/15/2015 0:10:00 AM", "me_self", "Me Self", "", "late reply", "#carol_c/$me_self;ab12") +
		record(12, "Outgoing Call", "9/15/2015 9:00:00 AM", "me_self", "Me Self", "00:02:05", "", "#carol_c/$me_self;ab12")
	records, err := skypelog.Parse(strings.NewReader(export), lisbon)
	if err != nil {
		t.Fatal(err)
	}
	conversations, _ := skypelog.Conversations(records, skypelog.Options{Self: "me_self", EarliestYear: 2000})
	sink := &recorder{}
	if err := send(sink, conversations, lisbon, "me_self", map[string]string{"carol_c": "person_000003"}); err != nil {
		t.Fatal(err)
	}
	if len(sink.conversations) != 1 || sink.conversations[0].SourceRef != "#carol_c/$me_self;ab12" || sink.conversations[0].Title != "Carol" {
		t.Fatalf("conversations = %+v", sink.conversations)
	}
	participants := sink.conversations[0].Participants
	if len(participants) != 2 || participants[0].PersonID != "person_000003" || participants[0].IsSelf || !participants[1].IsSelf {
		t.Errorf("participants = %+v", participants)
	}
	if len(sink.messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(sink.messages))
	}
	call := sink.messages[2]
	if call.Kind != "call" || call.CallSeconds != 125 || call.SourceRef != "skypelogview-record-12" || call.SourceHash == "" {
		t.Errorf("call = %+v", call)
	}
	if sink.messages[0].Kind != "text" || sink.messages[0].Body != "hello" || sink.messages[0].SenderHandle != "carol_c" {
		t.Errorf("first message = %+v", sink.messages[0])
	}
	if len(sink.days) != 2 || sink.days[0].Day != "2015-09-14" || sink.days[1].JournalSourceRef != "#carol_c/$me_self;ab12/2015-09-15" ||
		sink.days[0].TimeZone != "Europe/Lisbon" || sink.days[0].JournalSource != "skype" {
		t.Errorf("days = %+v", sink.days)
	}
}
