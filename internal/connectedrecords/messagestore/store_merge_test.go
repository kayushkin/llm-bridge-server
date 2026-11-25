package messagestore

import (
	"net/http"
	"testing"
)

func TestConversationMergeMovesEverythingAndTheMergedKeyResolves(t *testing.T) {
	ts := newTestServer(t)
	var run Import
	ts.call("POST", "/imports", ImportStart{Importer: "test"}, http.StatusCreated, &run)
	phone := ConversationKey{Source: "sms", SourceRef: "+12025550101"}
	bridgeRoom := ConversationKey{Source: "matrix", SourceRef: "!bridge:example.org"}
	webRoom := ConversationKey{Source: "matrix", SourceRef: "!web:example.org"}
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{
		Conversations: []ImportConversation{
			{Platform: "sms", Source: phone.Source, SourceRef: phone.SourceRef, Title: "Carol", Participants: []Participant{
				{Handle: "+12025550101", DisplayName: "Carol", PersonID: "person_000012"}, {Handle: "+12025550199", DisplayName: "Me Self", IsSelf: true}}},
			{Platform: "sms", Source: bridgeRoom.Source, SourceRef: bridgeRoom.SourceRef, Title: "Carol, Me Self", IsGroup: true, Participants: []Participant{
				{Handle: "@gm_5:example.org", DisplayName: "Carol", PersonID: "person_000012"}}},
			{Platform: "sms", Source: webRoom.Source, SourceRef: webRoom.SourceRef, Title: "Carol", Participants: []Participant{
				{Handle: "@gmweb_c:example.org", DisplayName: "Carol"}}},
		},
		Messages: []ImportMessage{
			{BodySource: "platform", ConversationSource: "sms", ConversationSourceRef: phone.SourceRef, SentAt: at("2025-01-01T10:00:00Z"), SenderHandle: "+12025550101", Kind: "text", Body: "old", Source: "sms", SourceRef: "p1"},
			{BodySource: "platform", ConversationSource: "matrix", ConversationSourceRef: bridgeRoom.SourceRef, SentAt: at("2026-07-01T10:00:00Z"), SenderHandle: "@gm_5:example.org", Kind: "text", Body: "bridge", Source: "multichat", SourceRef: "m1"},
			{BodySource: "platform", ConversationSource: "matrix", ConversationSourceRef: webRoom.SourceRef, SentAt: at("2026-10-01T10:00:00Z"), SenderHandle: "@gmweb_c:example.org", Kind: "text", Body: "web", Source: "multichat", SourceRef: "m2"},
		},
	}, http.StatusOK, nil)
	var conversations []Conversation
	ts.call("GET", "/conversations", nil, http.StatusOK, &conversations)
	ids := map[string]string{}
	for _, conversation := range conversations {
		ids[conversation.SourceRef] = conversation.ID
	}
	// The web room merges into the bridge room, and the bridge room, carrying
	// it along, into the phone thread.
	var counts ImportCounts
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{ConversationMerges: []ImportConversationMerge{
		{Merged: webRoom, Into: bridgeRoom, Rule: "google_conversation_id"},
		{Merged: bridgeRoom, Into: phone, Rule: "phone_numbers"},
		{Merged: webRoom, Into: phone, Rule: "phone_numbers"},
		{Merged: ConversationKey{Source: "matrix", SourceRef: "!never:example.org"}, Into: phone, Rule: "phone_numbers"},
	}}, http.StatusOK, &counts)
	if counts.ConversationsMerged != 2 || counts.ConversationMergesSkipped != 1 {
		t.Fatalf("counts %+v", counts)
	}
	var phoneThread Conversation
	ts.call("GET", "/conversations/"+ids[phone.SourceRef], nil, http.StatusOK, &phoneThread)
	if phoneThread.MessageCount != 3 || phoneThread.Title != "Carol" || phoneThread.IsGroup || len(phoneThread.Participants) != 4 {
		t.Fatalf("phone thread after the merge: %+v", phoneThread)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := noFollow.Get(ts.server.URL + "/conversations/" + ids[webRoom.SourceRef])
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMovedPermanently || response.Header.Get("Location") != ids[phone.SourceRef] {
		t.Fatalf("merged id answered %d to %q", response.StatusCode, response.Header.Get("Location"))
	}
	var page MessagePage
	ts.call("GET", "/messages?conversation="+ids[webRoom.SourceRef], nil, http.StatusOK, &page)
	if len(page.Messages) != 3 {
		t.Fatalf("messages by the merged id: %d", len(page.Messages))
	}
	// A later import under the merged room's key lands in the phone thread
	// and leaves its title alone.
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{
		Conversations: []ImportConversation{{Platform: "sms", Source: webRoom.Source, SourceRef: webRoom.SourceRef, Title: "Carol, Me Self", IsGroup: true,
			Participants: []Participant{{Handle: "@gmweb_c:example.org", PersonID: "person_000012"}}}},
		Messages: []ImportMessage{{BodySource: "platform", ConversationSource: "matrix", ConversationSourceRef: webRoom.SourceRef, SentAt: at("2026-10-02T10:00:00Z"),
			SenderHandle: "@gmweb_c:example.org", Kind: "text", Body: "later", Source: "multichat", SourceRef: "m3"}},
	}, http.StatusOK, &counts)
	if counts.ConversationsCreated != 0 || counts.MessagesCreated != 1 {
		t.Fatalf("later import counts %+v", counts)
	}
	ts.call("GET", "/conversations/"+ids[phone.SourceRef], nil, http.StatusOK, &phoneThread)
	if phoneThread.MessageCount != 4 || phoneThread.Title != "Carol" || phoneThread.IsGroup {
		t.Fatalf("phone thread after a later import: %+v", phoneThread)
	}
	var merges []ConversationMerge
	ts.call("GET", "/conversation-merges?merged_into="+ids[phone.SourceRef], nil, http.StatusOK, &merges)
	if len(merges) != 2 {
		t.Fatalf("merges %+v", merges)
	}
	for _, merge := range merges {
		if merge.MergedInto != ids[phone.SourceRef] || len(merge.Row) == 0 {
			t.Fatalf("merge %+v", merge)
		}
	}
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{ConversationMerges: []ImportConversationMerge{{Merged: webRoom, Into: phone, Rule: "same name"}}},
		http.StatusBadRequest, nil)
}
