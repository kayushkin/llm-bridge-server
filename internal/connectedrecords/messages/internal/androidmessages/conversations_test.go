package androidmessages

import (
	"testing"
	"time"
)

// A phone number the user only called gets a direct conversation of its
// calls; a private caller and a blocked call are counted and left out.
func TestCallsToANumberNeverTextedMakeAConversationOfTheirOwn(t *testing.T) {
	at := time.Date(2022, 1, 2, 3, 4, 5, 0, time.UTC)
	archive := &Archive{
		Messages: []*Message{{Others: []string{"+12025550100"}, FromSelf: true, Time: at, Text: "hello"}},
		Calls: []*Call{
			{Number: "+12025550100", Time: at, Type: CallIncoming},
			{Number: "+12025550177", Time: at, Type: CallOutgoing, DurationSeconds: 60},
			{Number: "+12025550177", Time: at.Add(time.Hour), Type: CallMissed},
			{Number: "Private", Time: at, Type: CallIncoming},
			{Number: "", Time: at, Type: CallMissed},
			{Number: "+12025550188", Time: at, Type: CallBlocked},
		},
	}
	conversations, skipped := Conversations(archive)
	if len(conversations) != 2 || skipped.CallsNotPhoneNumber != 2 || skipped.BlockedCalls != 1 {
		t.Fatalf("conversations = %d, skipped = %+v", len(conversations), skipped)
	}
	texted, calledOnly := conversations[0], conversations[1]
	if texted.CallsOnly || len(texted.Calls) != 1 {
		t.Errorf("texted = %+v", texted)
	}
	if !calledOnly.CallsOnly || calledOnly.Key() != "+12025550177" || len(calledOnly.Calls) != 2 || len(calledOnly.Messages) != 0 {
		t.Errorf("called only = %+v", calledOnly)
	}
}
