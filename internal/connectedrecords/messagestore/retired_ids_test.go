package messagestore

import (
	"net/http"
	"testing"
)

// A merge removes the merged conversation's row and keeps its id as a
// redirect. When that row was the newest, the next conversation filed must
// still get an id of its own: reusing the retired id would make every read by
// it follow the redirect to the other conversation.
func TestAConversationFiledAfterTheNewestWasMergedGetsAFreshID(t *testing.T) {
	ts := newTestServer(t)
	var run Import
	ts.call("POST", "/imports", ImportStart{Importer: "test"}, http.StatusCreated, &run)
	survivor := ConversationKey{Source: "matrix", SourceRef: "!survivor:example.org"}
	newest := ConversationKey{Source: "matrix", SourceRef: "!newest:example.org"}
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Conversations: []ImportConversation{
		{Platform: "sms", Source: survivor.Source, SourceRef: survivor.SourceRef, Title: "Carol", Participants: []Participant{{Handle: "@gm_1:example.org"}}},
		{Platform: "sms", Source: newest.Source, SourceRef: newest.SourceRef, Title: "Carol", Participants: []Participant{{Handle: "@gmweb_1:example.org"}}},
	}}, http.StatusOK, nil)
	var conversations []Conversation
	ts.call("GET", "/conversations", nil, http.StatusOK, &conversations)
	mergedID := ""
	for _, conversation := range conversations {
		if conversation.SourceRef == newest.SourceRef {
			mergedID = conversation.ID
		}
	}
	var counts ImportCounts
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{ConversationMerges: []ImportConversationMerge{
		{Merged: newest, Into: survivor, Rule: "google_conversation_id"}}}, http.StatusOK, &counts)
	if counts.ConversationsMerged != 1 {
		t.Fatalf("merge counts %+v", counts)
	}

	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{
		Conversations: []ImportConversation{{Platform: "discord", Source: "discord", SourceRef: "5000", Participants: []Participant{{Handle: "7"}, {Handle: "8"}}}},
		Messages: []ImportMessage{{BodySource: "platform", ConversationSource: "discord", ConversationSourceRef: "5000", SentAt: at("2026-11-11T21:18:14Z"),
			SenderHandle: "7", Kind: "text", Body: "you have admin now", Source: "discord", SourceRef: "6000"}},
	}, http.StatusOK, nil)
	ts.call("GET", "/conversations", nil, http.StatusOK, &conversations)
	filedID := ""
	for _, conversation := range conversations {
		if conversation.SourceRef == "5000" {
			filedID = conversation.ID
		}
	}
	if filedID == "" || filedID == mergedID {
		t.Fatalf("the new conversation got id %q; the merged conversation's retired id is %q", filedID, mergedID)
	}
	var page MessagePage
	ts.call("GET", "/messages?conversation="+filedID, nil, http.StatusOK, &page)
	if len(page.Messages) != 1 || page.Messages[0].Body != "you have admin now" {
		t.Fatalf("messages by the new conversation's id: %+v", page.Messages)
	}
}

// A superseded message's row is removed and its id redirects to the row that
// holds it now; a message filed after the newest was superseded must not get
// that id again.
func TestAMessageFiledAfterTheNewestWasSupersededGetsAFreshID(t *testing.T) {
	ts := newTestServer(t)
	importSample(ts)
	var run Import
	ts.call("POST", "/imports", ImportStart{Importer: "test"}, http.StatusCreated, &run)
	liveCopy := ImportMessage{BodySource: "platform", ConversationSource: "discord", ConversationSourceRef: "1000", SentAt: at("2016-03-01T12:00:00Z"), SenderHandle: "3",
		Kind: "text", Body: "game night?", Source: "multichat", SourceRef: "message_000042", SourceHash: "a"}
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: []ImportMessage{liveCopy}}, http.StatusOK, nil)
	var page MessagePage
	ts.call("GET", "/messages?q=game", nil, http.StatusOK, &page)
	retiredID := ""
	for _, message := range page.Messages {
		if message.Source == "multichat" {
			retiredID = message.ID
		}
	}
	merged := sampleBatch().Messages[2]
	merged.SourceHash = "b"
	merged.Supersedes = []MessageKey{{Source: "multichat", SourceRef: "message_000042"}}
	var counts ImportCounts
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: []ImportMessage{merged}}, http.StatusOK, &counts)
	if counts.MessagesSuperseded != 1 {
		t.Fatalf("supersede counts %+v", counts)
	}

	fresh := ImportMessage{BodySource: "platform", ConversationSource: "discord", ConversationSourceRef: "1000", SentAt: at("2016-03-02T12:00:00Z"), SenderHandle: "3",
		Kind: "text", Body: "a later message", Source: "discord", SourceRef: "9100"}
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: []ImportMessage{fresh}}, http.StatusOK, nil)
	ts.call("GET", "/messages?q=later", nil, http.StatusOK, &page)
	if len(page.Messages) != 1 || page.Messages[0].ID == retiredID {
		t.Fatalf("the later message got id %+v; the superseded message's retired id is %q", page.Messages, retiredID)
	}
}
