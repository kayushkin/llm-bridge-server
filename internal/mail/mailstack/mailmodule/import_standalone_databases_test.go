package mailmodule_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/accounts"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/localmail"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/mailmodule"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/mailmodule/mailmoduletest"
)

// standaloneFile makes a SQLite file with the standalone store's schema, as
// the running store left it, and the rows given.
func standaloneFile(t *testing.T, schemaFile string, rows ...string) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", schemaFile))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), strings.TrimSuffix(schemaFile, ".sql")+".db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("schema %s: %v", schemaFile, err)
	}
	for _, row := range rows {
		if _, err := db.Exec(row); err != nil {
			t.Fatalf("seed %q: %v", row, err)
		}
	}
	return path
}

var standaloneAccountsRows = []string{
	`INSERT INTO accounts VALUES ('acct-g','Mail','google','user@example.com','google','default','','#123',' sig ',1,0,1700000000000,1700000000001)`,
	`INSERT INTO accounts VALUES ('desk','Desk','local','desk@demo.invalid','','','','#888','',0,2,1700000000002,1700000000003)`,
	`INSERT INTO imap_creds VALUES ('box','box@example.com','imap.example.com',993,'smtp.example.com',465,'box','pw','tls',1,2)`,
	`INSERT INTO imap_folder_map VALUES ('acct-i','inbox','INBOX')`,
	`INSERT INTO messages VALUES ('acct-g','','m1','root@x','t1','root@x','','[]','Invoice ready','Billing','billing@shop.example',1700000000100,'INBOX',0,1,2)`,
	`INSERT INTO message_bodies (id, account_id, folder_scope, provider_id, message_id, subject, from_name, from_email, date_ms, body_text, body_html, search_text, fetched_ms)
		VALUES (7,'acct-g','','m1','root@x','Invoice ready','Billing','billing@shop.example',1700000000100,'Your invoice is attached.','','Your invoice is attached.',3)`,
	`INSERT INTO message_bodies (id, account_id, folder_scope, provider_id, message_id, subject, from_name, from_email, date_ms, body_text, body_html, search_text, fetched_ms)
		VALUES (9,'acct-g','','m2','','Lunch','Friend','friend@mail.example',1700000000200,'Lunch on Friday?',CAST(x'3c703e4c756e6368953c2f703e' AS TEXT),'Lunch on Friday?',4)`,
}

var standaloneLocalMailRows = []string{
	`INSERT INTO local_messages (account_id, id, folder_id, message_id, thread_id, in_reply_to, references_ids, subject, from_name, from_email,
		to_addrs, cc_addrs, date_ms, snippet, unread, flagged, created_at, text_bytes, html_bytes, bcc_addrs, provider_message_id,
		source_revision, outlook_store_id, outlook_folder_id, outlook_folder_path, revision, content_sha256, updated_at)
		VALUES ('desk','im1','INBOX','a@x','conv','','[]','Help','Pat','pat@demo.invalid','[]','[]',1700000000300,'help please',1,0,5,
		11,NULL,'[]','SRC-1','r2','','','',2,'hash2',6)`,
	`INSERT INTO local_message_bodies VALUES ('desk','im1','help please',NULL)`,
	`INSERT INTO local_import_sources VALUES ('desk','export-1.json','abc',1,7)`,
	`INSERT INTO local_message_revisions VALUES ('desk','im1',1,'{"revision":1,"source_revision":"r1","content_sha256":"hash1"}','old text',NULL,8)`,
	`INSERT INTO local_message_attachments VALUES ('desk','im1',2,'a1',0,'s1','a.pdf','application/pdf',3,'sha','',0,'file_000004')`,
	`INSERT INTO ingress_credentials VALUES ('ingress_00','tokenhash','custody','',9,0)`,
	`INSERT INTO ingress_credential_accounts VALUES ('ingress_00','desk')`,
	`INSERT INTO delivery_receipts (receipt_id, credential_id, source, delivery_id, account_id, provider_message_id, source_revision,
		content_sha256, status, message_id, revision, detail, received_at) VALUES (3,'ingress_00','custody','d1','desk','SRC-1','r2','hash2','accepted','im1',2,'',10)`,
	`INSERT INTO message_purges VALUES ('desk','SRC-9','im9','asked to',11)`,
	`INSERT INTO message_events (seq, kind, origin, account_id, id, provider_message_id, revision, created_at)
		VALUES (5,'message.revision_available','ingress','desk','im1','SRC-1',2,12)`,
	// Rows the standalone store deleted still used up their numbers.
	`UPDATE sqlite_sequence SET seq = 40 WHERE name = 'message_events'`,
}

func TestAnImportCopiesBothFilesWholeAndKeepsEveryNumber(t *testing.T) {
	ctx := context.Background()
	db := mailmoduletest.NewDB(t)
	counts, err := mailmodule.ImportStandaloneDatabases(ctx, db.Pool(),
		standaloneFile(t, "standalone_accounts_schema.sql", standaloneAccountsRows...),
		standaloneFile(t, "standalone_local_mail_schema.sql", standaloneLocalMailRows...))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want := mailmodule.ImportCounts{Accounts: 2, IMAPLogins: 1, IMAPFolderMaps: 1, IndexedMessages: 1, RecordedBodies: 2,
		LocalMessages: 1, LocalMessageBodies: 1, LocalImportSources: 1, LocalMessageRevisions: 1, LocalMessageAttachments: 1,
		IngressCredentials: 1, IngressCredentialAccounts: 1, DeliveryReceipts: 1, MessagePurges: 1, MessageEvents: 1,
		LastReceiptID: 3, LastEventSequence: 40, LastRecordedBody: 9, RepairedText: map[string]int64{"message_bodies.body_html": 1}}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("counts %+v, want %+v", counts, want)
	}

	store := accounts.New(db)
	desk, err := store.Get("desk")
	if err != nil {
		t.Fatal(err)
	}
	if desk.Enabled || desk.SortOrder != 2 || desk.CreatedAt != 1700000000002 || desk.UpdatedAt != 1700000000003 {
		t.Errorf("desk read back as %+v", desk)
	}
	if cred, err := store.GetImapCred("box"); err != nil || cred.Password != "pw" || cred.ImapPort != 993 {
		t.Errorf("imap login read back as %+v, %v", cred, err)
	}
	// The trigger built the search columns from the copied text.
	hits, err := store.SearchMessages("invoice", "", 10)
	if err != nil || len(hits) != 1 || hits[0].ProviderID != "m1" || hits[0].Score >= 0 {
		t.Errorf("search for invoice: %+v, %v", hits, err)
	}
	// A body that was not UTF-8 is held with U+FFFD for the byte that was not.
	var html string
	if err := db.QueryRow(`SELECT body_html FROM message_bodies WHERE id = 9`).Scan(&html); err != nil || html != "<p>Lunch\uFFFD</p>" {
		t.Errorf("the repaired body reads %q, %v", html, err)
	}
	if err := store.UpsertMessageBody(accounts.MessageBody{AccountID: "acct-g", ProviderID: "m3", Subject: "new"}); err != nil {
		t.Fatal(err)
	}
	var newBody int64
	if err := db.QueryRow(`SELECT id FROM message_bodies WHERE provider_id = 'm3'`).Scan(&newBody); err != nil || newBody != 10 {
		t.Errorf("a new body took id %d (%v), want 10", newBody, err)
	}

	local := localmail.New(db)
	stored, err := local.Metadata(ctx, "desk", "im1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != 2 || stored.Bodies.Text == nil || stored.Bodies.Text.Bytes != 11 || stored.Bodies.HTML != nil || len(stored.Attachments) != 1 {
		t.Errorf("im1 read back as %+v", stored)
	}
	if text, _, err := local.Body(ctx, "desk", "im1", localmail.BodyFormatText); err != nil || text != "help please" {
		t.Errorf("im1 text %q, %v", text, err)
	}
	if _, _, err := local.Body(ctx, "desk", "im1", localmail.BodyFormatHTML); err != localmail.ErrBodyAbsent {
		t.Errorf("im1 html: %v, want the body absent, as it was", err)
	}
	if _, fileID, err := local.AttachmentFile(ctx, "desk", "im1", 0, "a1"); err != nil || fileID != "file_000004" {
		t.Errorf("attachment file %q, %v", fileID, err)
	}
	if credential, err := local.IngressCredentials(ctx); err != nil || len(credential) != 1 || credential[0].AccountIDs[0] != "desk" {
		t.Errorf("credentials %+v, %v", credential, err)
	}
	// The outbox goes on after the counter, not after the highest row left.
	if err := local.Purge(ctx, "desk", "im1", "test"); err != nil {
		t.Fatal(err)
	}
	events, err := local.Events(ctx, 0, "", 10)
	if err != nil || len(events) != 2 || events[0].Seq != 5 || events[1].Seq != 41 {
		t.Errorf("events %+v, %v", events, err)
	}
}

func TestAnImportWithoutALocalMailFileCopiesTheAccountsAlone(t *testing.T) {
	db := mailmoduletest.NewDB(t)
	counts, err := mailmodule.ImportStandaloneDatabases(context.Background(), db.Pool(),
		standaloneFile(t, "standalone_accounts_schema.sql", standaloneAccountsRows...), "")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if counts.Accounts != 2 || counts.LocalMessages != 0 || counts.RepairedText["message_bodies.body_html"] != 1 {
		t.Errorf("counts %+v", counts)
	}
}

func TestAnImportRefusesAModuleThatHoldsRowsAndAFileAtAnotherVersion(t *testing.T) {
	ctx := context.Background()
	accountsFile := standaloneFile(t, "standalone_accounts_schema.sql", standaloneAccountsRows...)
	db := mailmoduletest.NewDB(t)
	if _, err := mailmodule.ImportStandaloneDatabases(ctx, db.Pool(), accountsFile, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := mailmodule.ImportStandaloneDatabases(ctx, db.Pool(), accountsFile, ""); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("a second import: %v, want a refusal", err)
	}

	older := standaloneFile(t, "standalone_accounts_schema.sql", "PRAGMA user_version = 1")
	if _, err := mailmodule.ImportStandaloneDatabases(ctx, mailmoduletest.NewDB(t).Pool(), older, ""); err == nil || !strings.Contains(err.Error(), "schema version 1") {
		t.Errorf("an older file: %v, want a refusal naming its version", err)
	}
}
