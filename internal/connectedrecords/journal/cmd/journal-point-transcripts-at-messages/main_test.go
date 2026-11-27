package main

import (
	"strings"
	"testing"
)

func TestThePointerKeepsEveryPersonAndPictureTheOldBodyLinkedOnce(t *testing.T) {
	old := "**[Carol](ref:person_000012)** 10:01 hi\n**[Carol](ref:person_000012)** 10:02 look ![x](ref:photo_000003)\n[Dave](ref:person_000044): ok"
	body := pointerBody(conversationDay{ID: "conversation_day_000007", ConversationID: "conversation_000002", Day: "2016-03-01", MessageCount: 3},
		conversation{Platform: "skype"}, old)
	for _, want := range []string{"(ref:conversation_day_000007)", "(ref:conversation_000002)", "3 messages on skype",
		"With [Carol](ref:person_000012), [Dave](ref:person_000044).", "Pictures: [x](ref:photo_000003)."} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Count(body, "person_000012") != 1 {
		t.Errorf("Carol linked more than once:\n%s", body)
	}
}
