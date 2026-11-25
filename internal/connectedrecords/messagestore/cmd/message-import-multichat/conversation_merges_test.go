package main

import (
	"reflect"
	"testing"

	messagestore "github.com/kayushkin/message-store"
)

func member(userID string) struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
} {
	return struct {
		UserID      string `json:"user_id"`
		DisplayName string `json:"display_name"`
	}{UserID: userID}
}

func TestPlanConversationMergesJoinsOneChatByIds(t *testing.T) {
	const (
		carolNumber = "+12025550101"
		daveNumber  = "+12025550102"
		myNumber    = "+12025550199"
	)
	carolBridge, carolWeb, carolWebAgain := "@gmessages_1.5:x", "@gmessagesweb_c1:x", "@gmessagesweb_c2:x"
	daveBridge, meSecondID := "@gmessages_1.6:x", "@gmessages_1.9:x"
	erinWeb, erinWebAgain := "@gmessagesweb_e1:x", "@gmessagesweb_e2:x"
	rooms := map[string]room{
		// Carol: one Google conversation in both bridges, plus a second
		// thread the web bridge alone carries.
		"!carol-bridge:x": {RoomID: "!carol-bridge:x", Platform: "gmessages", GoogleMessagesConversationID: "39", DirectMessagePartnerUserID: carolBridge},
		"!carol-web:x":    {RoomID: "!carol-web:x", Platform: "gmessagesweb", GoogleMessagesConversationID: "39", DirectMessagePartnerUserID: carolWeb},
		"!carol-new:x":    {RoomID: "!carol-new:x", Platform: "gmessagesweb", GoogleMessagesConversationID: "50", DirectMessagePartnerUserID: carolWebAgain},
		// A group of Carol and Dave in which Google lists my second id.
		"!group-bridge:x": {RoomID: "!group-bridge:x", Platform: "gmessages", GoogleMessagesConversationID: "22",
			Members: []struct {
				UserID      string `json:"user_id"`
				DisplayName string `json:"display_name"`
			}{member(carolBridge), member(daveBridge), member(meSecondID)}},
		"!group-web:x": {RoomID: "!group-web:x", Platform: "gmessagesweb", GoogleMessagesConversationID: "22"},
		// A web-only group: its members are only who wrote, so it is not matched.
		"!web-group:x": {RoomID: "!web-group:x", Platform: "gmessagesweb", GoogleMessagesConversationID: "60",
			Members: []struct {
				UserID      string `json:"user_id"`
				DisplayName string `json:"display_name"`
			}{member(carolWeb)}},
		// Erin has no number anywhere; her two threads join by person.
		"!erin-1:x":  {RoomID: "!erin-1:x", Platform: "gmessagesweb", GoogleMessagesConversationID: "70", DirectMessagePartnerUserID: erinWeb},
		"!erin-2:x":  {RoomID: "!erin-2:x", Platform: "gmessagesweb", GoogleMessagesConversationID: "71", DirectMessagePartnerUserID: erinWebAgain},
		"!discord:x": {RoomID: "!discord:x", Platform: "discord", DirectMessagePartnerUserID: "@discord_1:x"},
	}
	merges, report := planConversationMerges(mergeInputs{
		rooms:       rooms,
		ghostPhones: map[string]string{carolBridge: carolNumber, daveBridge: daveNumber},
		personOfAccount: map[string]string{carolBridge: "person_000001", carolWeb: "person_000001", carolWebAgain: "person_000001",
			meSecondID: "person_000009", erinWeb: "person_000003", erinWebAgain: "person_000003"},
		personPhones: map[string][]string{"person_000009": {myNumber}},
		phoneThreads: map[string]phoneThread{
			carolNumber:                    {SourceRef: carolNumber},
			carolNumber + "," + daveNumber: {SourceRef: carolNumber + "," + daveNumber, IsGroup: true},
		},
		selfPerson: "person_000009",
	})
	key := func(source, ref string) messagestore.ConversationKey {
		return messagestore.ConversationKey{Source: source, SourceRef: ref}
	}
	want := []messagestore.ImportConversationMerge{
		{Merged: roomKey("!group-web:x"), Into: roomKey("!group-bridge:x"), Rule: "google_conversation_id"},
		{Merged: roomKey("!group-bridge:x"), Into: key("sms", carolNumber+","+daveNumber), Rule: "phone_numbers"},
		{Merged: roomKey("!carol-web:x"), Into: roomKey("!carol-bridge:x"), Rule: "google_conversation_id"},
		{Merged: roomKey("!carol-bridge:x"), Into: key("sms", carolNumber), Rule: "phone_numbers"},
		{Merged: roomKey("!carol-new:x"), Into: key("sms", carolNumber), Rule: "phone_numbers"},
		{Merged: roomKey("!erin-2:x"), Into: roomKey("!erin-1:x"), Rule: "direct_person"},
	}
	if !reflect.DeepEqual(merges, want) {
		t.Fatalf("merges\n%v\nwant\n%v", merges, want)
	}
	if report.groupsMatched != 1 || report.groupsUnreadable != 1 || report.directMatched != 2 || report.directByPerson != 1 {
		t.Fatalf("report %+v", report)
	}
}
