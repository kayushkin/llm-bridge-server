package messagestore

import "testing"

func TestAnAccountSeesTheConversationsItsHandleTakesPartIn(t *testing.T) {
	ts := newTestServer(t)
	var bot Account
	ts.call("POST", "/accounts", AccountPut{Platform: "discord", Handle: "900", DisplayName: "Helper", Kind: "bot"}, 200, &bot)
	var again Account
	ts.call("POST", "/accounts", AccountPut{Platform: "discord", Handle: "900", Kind: "bot"}, 200, &again)
	if again.ID != bot.ID || again.DisplayName != "Helper" {
		t.Fatalf("putting the account again gave %+v, want %s keeping its name", again, bot.ID)
	}
	ts.call("POST", "/accounts", AccountPut{Platform: "discord", Handle: "901", Kind: "robot"}, 400, nil)

	var run Import
	ts.call("POST", "/imports", ImportStart{Importer: "test"}, 201, &run)
	batch := ImportBatch{
		Conversations: []ImportConversation{
			{Platform: "discord", Source: "discord", SourceRef: "c1", Title: "general", IsGroup: true,
				Participants: []Participant{{Handle: "900", DisplayName: "Helper"}}},
			{Platform: "discord", Source: "discord", SourceRef: "c2", Title: "elsewhere", IsGroup: true},
			{Platform: "skype", Source: "skype", SourceRef: "900", Title: "same handle, other platform"},
		},
		Messages: []ImportMessage{{BodySource: "platform", ConversationSource: "discord", ConversationSourceRef: "c1", SentAt: at("2026-01-02T10:00:00Z"),
			SenderHandle: "900", Kind: "text", Source: "discord", SourceRef: "m1",
			Embeds:  []Embed{{Title: "Picnic", Fields: []EmbedField{{Name: "Going", Value: "3 of 10"}}}},
			Buttons: []Button{{Label: "Join"}}}},
	}
	ts.call("POST", "/imports/"+run.ID+"/batches", batch, 200, nil)

	var conversations []Conversation
	ts.call("GET", "/conversations?account="+bot.ID, nil, 200, &conversations)
	if len(conversations) != 1 || conversations[0].SourceRef != "c1" {
		t.Fatalf("account conversations = %+v, want only c1", conversations)
	}
	ts.call("GET", "/conversations?account=message_account_999999", nil, 404, nil)

	var page MessagePage
	ts.call("GET", "/messages?conversation="+conversations[0].ID, nil, 200, &page)
	if len(page.Messages) != 1 || len(page.Messages[0].Embeds) != 1 || page.Messages[0].Embeds[0].Fields[0].Value != "3 of 10" || page.Messages[0].Buttons[0].Label != "Join" {
		t.Fatalf("message = %+v, want its embed and button", page.Messages)
	}

	// A source that sends no card keeps the stored one.
	batch.Messages[0].Embeds, batch.Messages[0].Buttons, batch.Messages[0].Body = nil, nil, "text only"
	batch.Messages[0].SourceHash = "second"
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: batch.Messages}, 200, nil)
	ts.call("GET", "/messages?conversation="+conversations[0].ID, nil, 200, &page)
	if len(page.Messages[0].Embeds) != 1 || len(page.Messages[0].Buttons) != 1 || page.Messages[0].Body != "text only" {
		t.Fatalf("after a card-less import the message is %+v, want the card kept", page.Messages[0])
	}

	var accounts []Account
	ts.call("GET", "/accounts", nil, 200, &accounts)
	if len(accounts) != 1 || accounts[0].ID != bot.ID {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestAnImportThatDoesNotKnowTheUserLeavesThemMarked(t *testing.T) {
	ts := newTestServer(t)
	var run Import
	ts.call("POST", "/imports", ImportStart{Importer: "test"}, 201, &run)
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Conversations: []ImportConversation{{Platform: "discord", Source: "discord", SourceRef: "c1", IsGroup: true,
		Participants: []Participant{{Handle: "100", DisplayName: "Me Self", IsSelf: true}}}}}, 200, nil)
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Conversations: []ImportConversation{{Platform: "discord", Source: "discord", SourceRef: "c1", IsGroup: true,
		Participants: []Participant{{Handle: "100", DisplayName: "Me Self"}}}}}, 200, nil)
	var conversations []Conversation
	ts.call("GET", "/conversations", nil, 200, &conversations)
	if !conversations[0].Participants[0].IsSelf {
		t.Fatalf("participants = %+v, want the user still marked", conversations[0].Participants)
	}
}

func TestASecondSourceReadingTheSameMessageWritesNoRevision(t *testing.T) {
	ts := newTestServer(t)
	var run Import
	ts.call("POST", "/imports", ImportStart{Importer: "test"}, 201, &run)
	message := ImportMessage{BodySource: "platform", ConversationSource: "discord", ConversationSourceRef: "c1", SentAt: at("2026-01-02T10:00:00Z"),
		SenderHandle: "7", Kind: "text", Body: "hello", Source: "discord", SourceRef: "m1", SourceHash: "first importer"}
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Conversations: []ImportConversation{{Platform: "discord", Source: "discord", SourceRef: "c1", IsGroup: true}},
		Messages: []ImportMessage{message}}, 200, nil)
	message.SourceHash = "second importer"
	var counts ImportCounts
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: []ImportMessage{message}}, 200, &counts)
	if counts.MessagesUnchanged != 1 || counts.MessagesUpdated != 0 {
		t.Fatalf("counts = %+v, want the message unchanged", counts)
	}
	var page MessagePage
	ts.call("GET", "/messages", nil, 200, &page)
	var revisions []MessageRevision
	ts.call("GET", "/messages/"+page.Messages[0].ID+"/revisions", nil, 200, &revisions)
	if len(revisions) != 0 {
		t.Fatalf("revisions = %d, want none", len(revisions))
	}
	message.Body, message.SourceHash = "hello, edited", "third"
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: []ImportMessage{message}}, 200, &counts)
	ts.call("GET", "/messages/"+page.Messages[0].ID+"/revisions", nil, 200, &revisions)
	if counts.MessagesUpdated != 1 || len(revisions) != 1 {
		t.Fatalf("an edit gave counts %+v and %d revisions, want one of each", counts, len(revisions))
	}
}

func TestAnAccountKeepsItsCredentialAndAThreadKeepsItsChannel(t *testing.T) {
	ts := newTestServer(t)
	var bot Account
	ts.call("POST", "/accounts", AccountPut{Platform: "discord", Handle: "900", Kind: "bot", CredentialID: "cred_1"}, 200, &bot)
	ts.call("POST", "/accounts", AccountPut{Platform: "discord", Handle: "900", Kind: "bot"}, 200, &bot)
	if bot.CredentialID != "cred_1" {
		t.Fatalf("credential = %q, want it kept", bot.CredentialID)
	}
	var run Import
	ts.call("POST", "/imports", ImportStart{Importer: "test"}, 201, &run)
	thread := ImportConversation{Platform: "discord", Source: "discord", SourceRef: "t1", Title: "a thread", IsGroup: true, ParentSourceRef: "c1"}
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Conversations: []ImportConversation{thread}}, 200, nil)
	thread.ParentSourceRef = ""
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Conversations: []ImportConversation{thread}}, 200, nil)
	var conversations []Conversation
	ts.call("GET", "/conversations", nil, 200, &conversations)
	if conversations[0].ParentSourceRef != "c1" {
		t.Fatalf("parent = %q, want c1 kept", conversations[0].ParentSourceRef)
	}
}

func TestABridgesTextNeverReplacesThePlatformsOwn(t *testing.T) {
	ts := newTestServer(t)
	var run Import
	ts.call("POST", "/imports", ImportStart{Importer: "test"}, 201, &run)
	conversation := ImportConversation{Platform: "discord", Source: "discord", SourceRef: "c1", IsGroup: true}
	platform := ImportMessage{BodySource: "platform", ConversationSource: "discord", ConversationSourceRef: "c1", SentAt: at("2026-01-02T10:00:00Z"),
		SenderHandle: "7", Kind: "text", Body: "starts <t:1789168500:t> with <@42>", Source: "discord", SourceRef: "m1", SourceHash: "api"}
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Conversations: []ImportConversation{conversation}, Messages: []ImportMessage{platform}}, 200, nil)
	bridge := platform
	bridge.BodySource, bridge.Body, bridge.SourceHash = "bridge", "starts **02:30 UTC** with Carol", "bridge"
	var counts ImportCounts
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: []ImportMessage{bridge}}, 200, &counts)
	var page MessagePage
	ts.call("GET", "/messages", nil, 200, &page)
	if page.Messages[0].Body != platform.Body || page.Messages[0].BodySource != "platform" || counts.MessagesUnchanged != 1 {
		t.Fatalf("after the bridge's copy: %q (%s), counts %+v; want the platform's text kept and nothing written", page.Messages[0].Body, page.Messages[0].BodySource, counts)
	}
	// A bridge's text fills a message the platform gave no text, and the
	// platform's own text replaces a bridge's.
	edited := platform
	edited.Body, edited.SourceHash = "starts <t:1789168500:t> with <@42>, edited", "api again"
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: []ImportMessage{edited}}, 200, nil)
	ts.call("GET", "/messages", nil, 200, &page)
	if page.Messages[0].Body != edited.Body {
		t.Fatalf("the platform's edit = %q, want it taken", page.Messages[0].Body)
	}
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: []ImportMessage{{BodySource: "bridge", ConversationSource: "discord", ConversationSourceRef: "c1",
		SentAt: at("2026-01-02T11:00:00Z"), SenderHandle: "7", Kind: "text", Body: "only the bridge saw this", Source: "discord", SourceRef: "m2"}}}, 200, nil)
	ts.call("GET", "/messages?source=discord&source_ref=m2", nil, 200, &page)
	if page.Messages[0].Body != "only the bridge saw this" || page.Messages[0].BodySource != "bridge" {
		t.Fatalf("bridge-only message = %+v", page.Messages[0])
	}
	// A bridge calling a platform's system line text changes nothing.
	systemLine := ImportMessage{BodySource: "platform", ConversationSource: "discord", ConversationSourceRef: "c1", SentAt: at("2026-01-02T13:00:00Z"),
		SenderHandle: "7", Kind: "system", Body: "added <@42> to the thread", Source: "discord", SourceRef: "m4", SourceHash: "api"}
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: []ImportMessage{systemLine}}, 200, nil)
	asText := systemLine
	asText.BodySource, asText.Kind, asText.Body, asText.SourceHash = "bridge", "text", "added Carol to the thread", "bridge"
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: []ImportMessage{asText}}, 200, &counts)
	ts.call("GET", "/messages?source=discord&source_ref=m4", nil, 200, &page)
	if page.Messages[0].Kind != "system" || counts.MessagesUnchanged != 1 {
		t.Fatalf("after the bridge's reading: kind %s, counts %+v; want system and nothing written", page.Messages[0].Kind, counts)
	}
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: []ImportMessage{{BodySource: "", ConversationSource: "discord", ConversationSourceRef: "c1",
		SentAt: at("2026-01-02T12:00:00Z"), Kind: "text", Body: "x", Source: "discord", SourceRef: "m3"}}}, 400, nil)
}
