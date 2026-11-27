package messages

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// Alice and Bob each import the same sample: the same sources and refs, each
// in their own records.
func twoOwners(t *testing.T) (alice, bob, administrator *testServer) {
	t.Helper()
	alice = newTestServer(t)
	importSample(alice)
	bob = alice.as(bobID, false)
	importSample(bob)
	return alice, bob, alice.as("", true)
}

func TestOwnersReachOnlyTheirOwnRecords(t *testing.T) {
	alice, bob, administrator := twoOwners(t)
	var alicePage, bobPage MessagePage
	alice.call("GET", "/messages", nil, http.StatusOK, &alicePage)
	bob.call("GET", "/messages", nil, http.StatusOK, &bobPage)
	if len(alicePage.Messages) != 4 || len(bobPage.Messages) != 4 {
		t.Fatalf("alice sees %d messages, bob %d; want 4 each", len(alicePage.Messages), len(bobPage.Messages))
	}
	for _, message := range bobPage.Messages {
		if message.OwnerPrincipalID != bobID {
			t.Fatalf("bob's list holds %+v", message)
		}
	}
	aliceMessage := alicePage.Messages[0]
	bob.call("GET", "/messages/"+aliceMessage.ID, nil, http.StatusNotFound, nil)
	bob.call("GET", "/conversations/"+aliceMessage.ConversationID, nil, http.StatusNotFound, nil)
	var revisions []MessageRevision
	bob.call("GET", "/messages/"+aliceMessage.ID+"/revisions", nil, http.StatusOK, &revisions)
	if len(revisions) != 0 {
		t.Fatalf("bob read alice's revisions: %+v", revisions)
	}
	var aliceDays []ConversationDay
	alice.call("GET", "/conversation-days", nil, http.StatusOK, &aliceDays)
	if len(aliceDays) != 1 {
		t.Fatalf("alice's days = %+v", aliceDays)
	}
	bob.call("GET", "/conversation-days/"+aliceDays[0].ID, nil, http.StatusNotFound, nil)
	var page MessagePage
	bob.call("GET", "/messages?conversation="+aliceMessage.ConversationID, nil, http.StatusOK, &page)
	if len(page.Messages) != 0 {
		t.Fatalf("bob read alice's conversation by its id: %+v", page.Messages)
	}
	bob.call("GET", "/messages?q=hello", nil, http.StatusOK, &page)
	if len(page.Messages) != 1 || page.Messages[0].OwnerPrincipalID != bobID {
		t.Fatalf("bob's search = %+v", page.Messages)
	}
	var conversations []Conversation
	bob.call("GET", "/conversations", nil, http.StatusOK, &conversations)
	if len(conversations) != 2 {
		t.Fatalf("bob's conversations = %d, want 2", len(conversations))
	}
	var imports []Import
	bob.call("GET", "/imports", nil, http.StatusOK, &imports)
	if len(imports) != 1 || imports[0].OwnerPrincipalID != bobID {
		t.Fatalf("bob's imports = %+v", imports)
	}
	var summary Summary
	bob.call("GET", "/summary", nil, http.StatusOK, &summary)
	total := 0
	for _, platform := range summary.Platforms {
		total += platform.Messages
	}
	if total != 4 {
		t.Fatalf("bob's summary counts %d messages, want 4", total)
	}
	var buckets []ActivityBucket
	bob.call("GET", "/activity?group_by=platform&interval=year&time_zone=UTC", nil, http.StatusOK, &buckets)
	total = 0
	for _, bucket := range buckets {
		total += bucket.Sent + bucket.Received
	}
	if total != 4 {
		t.Fatalf("bob's activity counts %d messages, want 4", total)
	}

	// A caller who sees every owner sees both, and may narrow to one.
	administrator.call("GET", "/messages", nil, http.StatusOK, &page)
	if len(page.Messages) != 8 {
		t.Fatalf("the administrator sees %d messages, want 8", len(page.Messages))
	}
	administrator.call("GET", "/messages?owner_principal_id="+aliceID, nil, http.StatusOK, &page)
	if len(page.Messages) != 4 || page.Messages[0].OwnerPrincipalID != aliceID {
		t.Fatalf("narrowed to alice: %+v", page.Messages)
	}
	administrator.call("GET", "/messages/"+aliceMessage.ID, nil, http.StatusOK, nil)
	// Anyone else may name only itself.
	bob.call("GET", "/messages?owner_principal_id="+aliceID, nil, http.StatusForbidden, nil)
}

func TestAnotherOwnersImportCannotBeWrittenTo(t *testing.T) {
	alice := newTestServer(t)
	run := importSample(alice)
	bob := alice.as(bobID, false)
	bob.call("POST", "/imports/"+run.ID+"/batches", sampleBatch(), http.StatusNotFound, nil)
	bob.call("POST", "/imports/"+run.ID+"/finish", nil, http.StatusNotFound, nil)
	bob.call("GET", "/imports/"+run.ID, nil, http.StatusNotFound, nil)
	var conversations []Conversation
	alice.call("GET", "/conversations", nil, http.StatusOK, &conversations)
	bob.call("PATCH", "/conversations/"+conversations[0].ID+"/participants/carol_s", map[string]string{"person_id": "person_000099"}, http.StatusNotFound, nil)
	// A batch of bob's own names alice's conversation keys, and finds none.
	var bobRun Import
	bob.call("POST", "/imports", ImportStart{Importer: "test"}, http.StatusCreated, &bobRun)
	bob.call("POST", "/imports/"+bobRun.ID+"/batches", ImportBatch{Messages: sampleBatch().Messages}, http.StatusBadRequest, nil)
	var result JournalEntryLinkResult
	bob.call("POST", "/conversation-days/journal-entries", []JournalEntryLink{
		{JournalSource: "skype", JournalSourceRef: "chat-carol/2016-03-01", JournalEntryID: "entry_000009"}}, http.StatusOK, &result)
	if result.Linked != 0 {
		t.Fatalf("bob linked alice's day: %+v", result)
	}
}

func TestARetiredIDRedirectsOnlyItsOwner(t *testing.T) {
	alice := newTestServer(t)
	run := importSample(alice)
	liveCopy := ImportMessage{BodySource: "platform", ConversationSource: "discord", ConversationSourceRef: "1000", SentAt: at("2016-03-01T12:00:00Z"), SenderHandle: "3",
		Kind: "text", Body: "game night?", Source: "multichat", SourceRef: "message_000042", SourceHash: "a"}
	alice.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: []ImportMessage{liveCopy}}, http.StatusOK, nil)
	var page MessagePage
	alice.call("GET", "/messages?source=multichat&source_ref=message_000042", nil, http.StatusOK, &page)
	oldID := page.Messages[0].ID
	kept := sampleBatch().Messages[2]
	kept.SourceHash = "b"
	kept.Supersedes = []MessageKey{{Source: "multichat", SourceRef: "message_000042"}}
	alice.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: []ImportMessage{kept}}, http.StatusOK, nil)
	alice.do(noFollow, "GET", "/messages/"+oldID, nil, http.StatusMovedPermanently, nil)
	bob := alice.as(bobID, false)
	bob.do(noFollow, "GET", "/messages/"+oldID, nil, http.StatusNotFound, nil)
	var retired []SupersededMessage
	bob.call("GET", "/superseded-messages", nil, http.StatusOK, &retired)
	if len(retired) != 0 {
		t.Fatalf("bob listed alice's retired rows: %d", len(retired))
	}
}

func TestAnAccountOfAnotherOwnerIsNotFound(t *testing.T) {
	alice := newTestServer(t)
	var account Account
	alice.call("POST", "/accounts", AccountPut{Platform: "discord", Handle: "900", Kind: "bot"}, http.StatusOK, &account)
	if account.OwnerPrincipalID != aliceID {
		t.Fatalf("account = %+v", account)
	}
	bob := alice.as(bobID, false)
	bob.call("GET", "/accounts/"+account.ID, nil, http.StatusNotFound, nil)
	bob.call("GET", "/conversations?account="+account.ID, nil, http.StatusNotFound, nil)
	var accounts []Account
	bob.call("GET", "/accounts", nil, http.StatusOK, &accounts)
	if len(accounts) != 0 {
		t.Fatalf("bob's accounts = %+v", accounts)
	}
	// The same handle is bob's own account, apart from alice's.
	var bobs Account
	bob.call("POST", "/accounts", AccountPut{Platform: "discord", Handle: "900", Kind: "bot"}, http.StatusOK, &bobs)
	if bobs.ID == account.ID || bobs.OwnerPrincipalID != bobID {
		t.Fatalf("bob's account = %+v, alice's %+v", bobs, account)
	}
}

func TestARequestWithNoCallerIsRefused(t *testing.T) {
	nobody := newTestServer(t).as("", false)
	for _, path := range []string{"/health", "/vocabulary", "/messages", "/conversations", "/imports", "/summary"} {
		nobody.call("GET", path, nil, http.StatusUnauthorized, nil)
	}
	nobody.call("POST", "/imports", ImportStart{Importer: "test"}, http.StatusUnauthorized, nil)
}

// The server's own service acting as itself reads every owner's records, and
// cannot make one, since a new record is owned by whoever makes it.
func TestTheServiceActingAsNobodyCannotMakeARecord(t *testing.T) {
	service := newTestServer(t).as("", true)
	service.call("POST", "/imports", ImportStart{Importer: "test"}, http.StatusForbidden, nil)
	service.call("POST", "/accounts", AccountPut{Platform: "discord", Handle: "900", Kind: "bot"}, http.StatusForbidden, nil)
	service.call("GET", "/messages", nil, http.StatusOK, nil)
	service.call("GET", "/health", nil, http.StatusOK, nil)
}

func TestAZeroScopeReachesNothing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.ListMessages(ctx, ownership.Scope{}, MessageFilter{}); err == nil {
		t.Fatal("a zero scope listed messages")
	}
	if _, err := store.ListConversations(ctx, ownership.Scope{}, ConversationFilter{}); err == nil {
		t.Fatal("a zero scope listed conversations")
	}
	if _, err := store.Summarize(ctx, ownership.Scope{}); err == nil {
		t.Fatal("a zero scope summarized")
	}
}

// A search splits words at punctuation, as FTS5 did, and a query of
// punctuation alone matches nothing.
func TestSearchReadsWordsAsTheStandaloneStoreDid(t *testing.T) {
	ts := newTestServer(t)
	run := importSample(ts)
	ts.call("POST", "/imports/"+run.ID+"/batches", ImportBatch{Messages: []ImportMessage{{BodySource: "platform", ConversationSource: "skype",
		ConversationSourceRef: "chat-carol", SentAt: at("2016-03-03T10:00:00Z"), SenderHandle: "carol_s", Kind: "text",
		Body: "Well-being matters; see e-mail.", Source: "skype", SourceRef: "r9"}}}, http.StatusOK, nil)
	for query, want := range map[string]int{"well-being": 1, "being": 1, "WELL": 1, "e-mail": 1, "mail": 1, "matter": 0, "hello night": 1, "???": 0, "late hi": 0} {
		var page MessagePage
		ts.call("GET", "/messages?q="+urlQuery(query), nil, http.StatusOK, &page)
		if len(page.Messages) != want {
			t.Errorf("q=%q found %d, want %d", query, len(page.Messages), want)
		}
	}
}

func urlQuery(text string) string { return url.QueryEscape(text) }
