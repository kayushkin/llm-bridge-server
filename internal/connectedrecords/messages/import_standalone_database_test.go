package messages

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// newStandaloneDatabase writes a SQLite file the way the standalone store
// kept one, with a few made-up rows, gaps in every numbering, and retired
// numbers above the live ones.
func newStandaloneDatabase(t *testing.T, extra ...string) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "message-store.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("standalone schema: %v", err)
	}
	statements := []string{
		`INSERT INTO imports (seq, id, importer, description, started_at, finished_at, counts) VALUES
			(3, 'message_import_000003', 'message-import-skype', 'a log', 1700000000000, 1700000001000, '{"messages_created":2}'),
			(4, 'message_import_000004', 'message-import-multichat', '', 1700000100000, 0, '{}')`,
		`INSERT INTO accounts (seq, id, platform, handle, display_name, kind, created_at, updated_at, credential_id) VALUES
			(2, 'message_account_000002', 'discord', '900', 'Helper', 'bot', 1700000000000, 1700000000000, 'cred_1')`,
		`INSERT INTO conversations (seq, id, platform, source, source_ref, title, is_group, created_at, updated_at, parent_source_ref) VALUES
			(5, 'conversation_000005', 'skype', 'skype', 'chat-carol', '', 0, 1700000000000, 1700000000000, ''),
			(8, 'conversation_000008', 'discord', 'discord', '1000', 'Board games', 1, 1700000000000, 1700000100000, '')`,
		`INSERT INTO participants (conversation_id, handle, display_name, person_id, is_self) VALUES
			('conversation_000005', 'me_self', 'Me Self', '', 1),
			('conversation_000005', 'carol_s', 'Carol', 'person_000012', 0),
			('conversation_000008', '2', 'Carol', 'person_000012', 0)`,
		`INSERT INTO messages (seq, id, conversation_id, sent_at, sender_handle, kind, body, attachments, reply_to_source_ref, call_seconds, edited,
			deleted_on_platform, source, source_ref, source_hash, imported_by, created_at, updated_at, embeds, buttons, body_source) VALUES
			(10, 'chat_message_000010', 'conversation_000005', 1456875000000, 'carol_s', 'text', 'late night hello', '[]', '', 0, 1, 0, 'skype', 'r1', 'a',
				'message_import_000003', 1700000000000, 1700000000000, '[]', '[]', 'platform'),
			(11, 'chat_message_000011', 'conversation_000008', 1456833600000, '2', 'media', '', '[{"photo_id":"photo_000001"}]', '', 0, 0, 1, 'discord', 'm1', '',
				'', 1700000000000, 1700000000000, '[{"title":"Picnic"}]', '[{"label":"Join"}]', 'platform')`,
		`INSERT INTO conversation_days (seq, id, conversation_id, day, time_zone, starts_at, ends_at, journal_source, journal_source_ref, journal_entry_id, created_at, updated_at)
			VALUES (6, 'conversation_day_000006', 'conversation_000005', '2016-03-01', 'Europe/Lisbon', 1456790400000, 1456876800000, 'skype', 'chat-carol/2016-03-01',
				'entry_000007', 1700000000000, 1700000000000)`,
		`INSERT INTO superseded_messages (id, source, source_ref, superseded_by, import_id, superseded_at, row_json) VALUES
			('chat_message_000014', 'multichat', 'message_000042', 'chat_message_000011', 'message_import_000004', 1700000100000, '{"id":"chat_message_000014","body":"x"}')`,
		`INSERT INTO message_revisions (seq, message_id, import_id, revised_at, row_json) VALUES
			(2, 'chat_message_000010', 'message_import_000004', 1700000100000, '{"id":"chat_message_000010","body":"late night hi"}')`,
		`UPDATE sqlite_sequence SET seq = 9 WHERE name = 'message_revisions'`,
		`INSERT INTO conversation_merges (conversation_id, source, source_ref, merged_into, rule, import_id, merged_at, messages_moved, row_json) VALUES
			('conversation_000012', 'matrix', '!web:example.org', 'conversation_000005', 'phone_numbers', 'message_import_000004', 1700000100000, 3,
				'{"id":"conversation_000012","participants":[]}')`,
	}
	for _, statement := range append(statements, extra...) {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

func TestImportCopiesEveryRowAsItWasAndNumbersOnAfterIt(t *testing.T) {
	ts := newTestServer(t)
	counts, err := ImportStandaloneDatabase(t.Context(), ts.store.db, newStandaloneDatabase(t), aliceID)
	if err != nil {
		t.Fatal(err)
	}
	want := StandaloneImportCounts{Imports: 2, Accounts: 1, Conversations: 2, Participants: 3, Messages: 2, ConversationDays: 1,
		SupersededMessages: 1, MessageRevisions: 1, ConversationMerges: 1}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}
	if !strings.Contains(counts.Summary(), "messages=2") {
		t.Fatalf("summary = %q", counts.Summary())
	}

	var message Message
	ts.call("GET", "/messages/chat_message_000011", nil, http.StatusOK, &message)
	if message.OwnerPrincipalID != aliceID || message.ConversationID != "conversation_000008" || message.ImportedBy != "" || !message.DeletedOnPlatform ||
		message.Attachments[0].PhotoID != "photo_000001" || message.Embeds[0].Title != "Picnic" || message.Buttons[0].Label != "Join" || message.SenderPersonID != "person_000012" {
		t.Fatalf("message = %+v", message)
	}
	ts.call("GET", "/messages/chat_message_000010", nil, http.StatusOK, &message)
	if message.ImportedBy != "message_import_000003" || !message.Edited || message.Body != "late night hello" {
		t.Fatalf("message = %+v", message)
	}
	response := ts.do(noFollow, "GET", "/messages/chat_message_000014", nil, http.StatusMovedPermanently, nil)
	if response.Header.Get("Location") != "chat_message_000011" {
		t.Fatalf("superseded id redirects to %q", response.Header.Get("Location"))
	}
	response = ts.do(noFollow, "GET", "/conversations/conversation_000012", nil, http.StatusMovedPermanently, nil)
	if response.Header.Get("Location") != "conversation_000005" {
		t.Fatalf("merged id redirects to %q", response.Header.Get("Location"))
	}
	var conversation Conversation
	ts.call("GET", "/conversations/conversation_000005", nil, http.StatusOK, &conversation)
	if conversation.MessageCount != 1 || len(conversation.Participants) != 2 || !conversation.Participants[0].IsSelf {
		t.Fatalf("conversation = %+v", conversation)
	}
	var revisions []MessageRevision
	ts.call("GET", "/messages/chat_message_000010/revisions", nil, http.StatusOK, &revisions)
	if len(revisions) != 1 || revisions[0].ImportID != "message_import_000004" || string(revisions[0].Row) != `{"id":"chat_message_000010","body":"late night hi"}` {
		t.Fatalf("revisions = %+v", revisions)
	}
	var day ConversationDayWithMessages
	ts.call("GET", "/conversation-days/conversation_day_000006", nil, http.StatusOK, &day)
	if day.JournalEntryID != "entry_000007" || len(day.Messages) != 1 {
		t.Fatalf("day = %+v", day)
	}
	var run Import
	ts.call("GET", "/imports/message_import_000003", nil, http.StatusOK, &run)
	if run.Counts.MessagesCreated != 2 || run.FinishedAt == 0 {
		t.Fatalf("import = %+v", run)
	}
	var accounts []Account
	ts.call("GET", "/accounts", nil, http.StatusOK, &accounts)
	if len(accounts) != 1 || accounts[0].ID != "message_account_000002" || accounts[0].CredentialID != "cred_1" {
		t.Fatalf("accounts = %+v", accounts)
	}

	// New records number on after the highest number ever used, retired
	// numbers and the revision counter included.
	var next Import
	ts.call("POST", "/imports", ImportStart{Importer: "test"}, http.StatusCreated, &next)
	if next.ID != "message_import_000005" {
		t.Fatalf("next import = %s", next.ID)
	}
	ts.call("POST", "/imports/"+next.ID+"/batches", ImportBatch{
		Conversations: []ImportConversation{{Platform: "discord", Source: "discord", SourceRef: "2000"}},
		Messages: []ImportMessage{{BodySource: "platform", ConversationSource: "discord", ConversationSourceRef: "2000", SentAt: at("2026-01-01T00:00:00Z"),
			Kind: "text", Body: "new", Source: "discord", SourceRef: "n1"},
			{BodySource: "platform", ConversationSource: "skype", ConversationSourceRef: "chat-carol", SentAt: 1456875000000, SenderHandle: "carol_s",
				Kind: "text", Body: "late night hello, edited", Source: "skype", SourceRef: "r1", SourceHash: "b"}},
		ConversationDays: []ImportConversationDay{{ConversationSource: "discord", ConversationSourceRef: "2000", Day: "2026-01-01", TimeZone: "UTC"}},
	}, http.StatusOK, nil)
	var page MessagePage
	ts.call("GET", "/messages?source=discord&source_ref=n1", nil, http.StatusOK, &page)
	if page.Messages[0].ID != "chat_message_000015" || page.Messages[0].ConversationID != "conversation_000013" {
		t.Fatalf("new message = %+v", page.Messages[0])
	}
	var days []ConversationDay
	ts.call("GET", "/conversation-days?conversation=conversation_000013", nil, http.StatusOK, &days)
	if len(days) != 1 || days[0].ID != "conversation_day_000007" {
		t.Fatalf("new day = %+v", days)
	}
	ts.call("GET", "/messages/chat_message_000010/revisions", nil, http.StatusOK, &revisions)
	var row map[string]any
	if len(revisions) != 2 || json.Unmarshal(revisions[1].Row, &row) != nil || row["imported_by"] != "message_import_000003" || row["edited"] != float64(1) {
		t.Fatalf("revisions after an edit = %+v", revisions)
	}
	var seq int64
	if err := ts.store.db.QueryRow(`SELECT MAX(seq) FROM message_revisions`).Scan(&seq); err != nil || seq != 10 {
		t.Fatalf("the new revision's number = %d (%v), want 10", seq, err)
	}
}

func TestImportRefusesWhatItCannotCopyWhole(t *testing.T) {
	ts := newTestServer(t)
	path := newStandaloneDatabase(t)
	if _, err := ImportStandaloneDatabase(t.Context(), ts.store.db, path, "nobody"); err == nil {
		t.Fatal("an owner that is not a principal id was taken")
	}
	if _, err := ImportStandaloneDatabase(t.Context(), ts.store.db, newStandaloneDatabase(t, `ALTER TABLE messages ADD COLUMN mood TEXT NOT NULL DEFAULT ''`), aliceID); err == nil ||
		!strings.Contains(err.Error(), "mood") {
		t.Fatalf("a column the import does not know: %v", err)
	}
	reused := newStandaloneDatabase(t, `INSERT INTO conversations (seq, id, platform, source, source_ref, title, is_group, created_at, updated_at, parent_source_ref)
		VALUES (12, 'conversation_000012', 'discord', 'discord', '5000', '', 1, 1, 1, '')`)
	if _, err := ImportStandaloneDatabase(t.Context(), ts.store.db, reused, aliceID); err == nil || !strings.Contains(err.Error(), "merged") {
		t.Fatalf("a live conversation holding a merged number: %v", err)
	}
	dangling := newStandaloneDatabase(t, `INSERT INTO participants (conversation_id, handle) VALUES ('conversation_000099', 'x')`)
	if _, err := ImportStandaloneDatabase(t.Context(), ts.store.db, dangling, aliceID); err == nil {
		t.Fatal("a participant of a conversation the file does not hold was taken")
	}
	var held int
	if err := ts.store.db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&held); err != nil || held != 0 {
		t.Fatalf("a refused import left %d conversations (%v)", held, err)
	}
	if _, err := ImportStandaloneDatabase(t.Context(), ts.store.db, path, aliceID); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportStandaloneDatabase(t.Context(), ts.store.db, path, bobID); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("a second import into a module that holds rows: %v", err)
	}
}

// Imported rows are their owner's: anyone else is answered as if they did
// not exist, retired ids included.
func TestImportedRowsAreTheOwners(t *testing.T) {
	ts := newTestServer(t)
	if _, err := ImportStandaloneDatabase(t.Context(), ts.store.db, newStandaloneDatabase(t), aliceID); err != nil {
		t.Fatal(err)
	}
	bob := ts.as(bobID, false)
	bob.call("GET", "/messages/chat_message_000010", nil, http.StatusNotFound, nil)
	bob.do(noFollow, "GET", "/messages/chat_message_000014", nil, http.StatusNotFound, nil)
	bob.do(noFollow, "GET", "/conversations/conversation_000012", nil, http.StatusNotFound, nil)
}
