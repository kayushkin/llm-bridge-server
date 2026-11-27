package skypelog

import (
	"strings"
	"testing"
	"time"
)

// A synthetic export in SkypeLogView's shape: a one-to-one chat over two
// days, a message that runs onto a second line and one that looks like a
// field, a group chat, a call with no time, Skype's empty "Chat" marker, and
// a record with a bad clock.
const sample = "\uFEFF==================================================\r\n" +
	"Record Number     : 10\r\nAction Type       : Chat Message\r\nAction Time       : 9/14/2015 8:05:31 PM\r\n" +
	"End Time          : \r\nUser Name         : alice_s\r\nDisplay Name      : Alice\r\nDuration          : \r\n" +
	"Chat Message      : hi there\r\nChatID            : alice_s\r\nFilename          : \r\n" +
	"==================================================\r\n\r\n==================================================\r\n" +
	"Record Number     : 11\r\nAction Type       : Chat Message\r\nAction Time       : 9/14/2015 8:06:00 PM\r\n" +
	"End Time          : \r\nUser Name         : me_x\r\nDisplay Name      : Me\r\nDuration          : \r\n" +
	"Chat Message      : # not a heading\r\nDuration          : 5 minutes, said in passing\r\n[x](ref:person_000001)\r\nChatID            : alice_s\r\nFilename          : \r\n" +
	"==================================================\r\n\r\n==================================================\r\n" +
	"Record Number     : 12\r\nAction Type       : Chat Message\r\nAction Time       : 9/15/2015 1:00:00 AM\r\n" +
	"End Time          : \r\nUser Name         : alice_s\r\nDisplay Name      : Alice\r\nDuration          : \r\n" +
	"Chat Message      : next day\r\nChatID            : alice_s\r\nFilename          : \r\n" +
	"==================================================\r\n\r\n==================================================\r\n" +
	"Record Number     : 13\r\nAction Type       : Chat Message\r\nAction Time       : 9/14/2015 9:00:00 PM\r\n" +
	"End Time          : \r\nUser Name         : bob_b\r\nDisplay Name      : Bob\r\nDuration          : \r\n" +
	"Chat Message      : group hello\r\nChatID            : 19:abc@thread.skype\r\nFilename          : \r\n" +
	"==================================================\r\n\r\n==================================================\r\n" +
	"Record Number     : 14\r\nAction Type       : Incoming Call\r\nAction Time       : \r\n" +
	"End Time          : \r\nUser Name         : alice_s\r\nDisplay Name      : Alice\r\nDuration          : 00:03:59\r\n" +
	"Chat Message      : \r\nChatID            : \r\nFilename          : \r\n" +
	"==================================================\r\n\r\n==================================================\r\n" +
	"Record Number     : 15\r\nAction Type       : Chat\r\nAction Time       : 9/14/2015 9:30:00 PM\r\n" +
	"End Time          : \r\nUser Name         : bob_b\r\nDisplay Name      : Bob\r\nDuration          : \r\n" +
	"Chat Message      : \r\nChatID            : 19:abc@thread.skype\r\nFilename          : \r\n" +
	"==================================================\r\n\r\n==================================================\r\n" +
	"Record Number     : 16\r\nAction Type       : Outgoing Call\r\nAction Time       : 1/1/1997 1:00:00 AM\r\n" +
	"End Time          : \r\nUser Name         : bob_b\r\nDisplay Name      : Bob\r\nDuration          : 00:01:00\r\n" +
	"Chat Message      : \r\nChatID            : \r\nFilename          : \r\n" +
	"==================================================\r\n"

func TestParseReadsEveryRecordAndMessagesThatRunOn(t *testing.T) {
	central, _ := time.LoadLocation("America/Chicago")
	records, err := Parse(strings.NewReader(sample), central)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 7 {
		t.Fatalf("records = %d, want 7", len(records))
	}
	first := records[0]
	if first.Number != 10 || first.UserName != "alice_s" || first.Message != "hi there" || first.ChatID != "alice_s" {
		t.Errorf("first = %+v", first)
	}
	if want := time.Date(2015, 9, 15, 1, 5, 31, 0, time.UTC); !first.Time.Equal(want) {
		t.Errorf("time = %v, want %v (8:05 PM Central)", first.Time.UTC(), want)
	}
	if got := records[1].Message; got != "# not a heading\nDuration          : 5 minutes, said in passing\n[x](ref:person_000001)" {
		t.Errorf("a message keeps every line until ChatID, even one shaped like a field: %q", got)
	}
	if !records[4].Time.IsZero() || records[4].Duration != "00:03:59" {
		t.Errorf("a call with no time keeps a zero time: %+v", records[4])
	}
}

func TestConversationsGroupByChatAndLeaveOutWhatTheTranscriptsDid(t *testing.T) {
	central, _ := time.LoadLocation("America/Chicago")
	records, err := Parse(strings.NewReader(sample), central)
	if err != nil {
		t.Fatal(err)
	}
	conversations, skipped := Conversations(records, Options{Self: "me_x", EarliestYear: 2015})
	if skipped.NoTime != 1 || skipped.BadClock != 1 || skipped.EmptyChats != 1 {
		t.Errorf("skipped = %+v, want one each", skipped)
	}
	if len(conversations) != 2 {
		t.Fatalf("conversations = %d, want 2 (Alice, the group)", len(conversations))
	}
	byKey := map[string]Conversation{}
	for _, conversation := range conversations {
		byKey[conversation.Key] = conversation
	}
	alice := byKey["alice_s"]
	if alice.Title != "Alice" || alice.IsGroup || len(alice.Records) != 3 {
		t.Errorf("alice = %q group=%v with %d records", alice.Title, alice.IsGroup, len(alice.Records))
	}
	if len(alice.Participants) != 2 || alice.Participants[0].UserName != "alice_s" || alice.Participants[1] != (Participant{"me_x", "Me"}) {
		t.Errorf("alice's participants = %+v", alice.Participants)
	}
	group := byKey["19:abc@thread.skype"]
	if group.Title != "group with Bob" || !group.IsGroup {
		t.Errorf("group = %q group=%v", group.Title, group.IsGroup)
	}
}

func TestDurationSeconds(t *testing.T) {
	for duration, want := range map[string]int{"00:03:59": 239, "01:00:00": 3600, "": 0, "soon": 0} {
		if got := DurationSeconds(duration); got != want {
			t.Errorf("DurationSeconds(%q) = %d, want %d", duration, got, want)
		}
	}
}
