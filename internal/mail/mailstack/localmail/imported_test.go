package localmail

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/mailstack/backend"
)

func strPtr(s string) *string { return &s }

func exported(providerID string) ImportedMessage {
	return ImportedMessage{
		ProviderMessageID: providerID,
		SourceRevision:    "ck-1",
		MessageID:         "<" + providerID + "@outlook.example>",
		ConversationID:    "conv-" + providerID,
		Subject:           "Rechnung über 1.200 € — 発注書",
		From:              backend.Address{Name: "Zoë Müller", Email: "zoe@customer.example"},
		To:                []backend.Address{{Email: "support@desk.example"}},
		Cc:                []backend.Address{{Name: "Ops", Email: "ops@desk.example"}},
		Bcc:               []backend.Address{{Email: "audit@desk.example"}},
		ReceivedAt:        time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC),
		BodyText:          strPtr("Hallo — bitte prüfen. 発注書を添付します。"),
		BodyHTML:          strPtr("<p>Hallo — <b>bitte</b> prüfen.</p><script>alert(1)</script>"),
		Outlook:           &OutlookLocation{StoreID: "0000STORE", FolderID: "0000FOLDER", FolderPath: `\\Support\Inbox\Rechnungen`},
	}
}

func TestAnImportedMessageSurvivesWithoutLoss(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	in := exported("AAMkAD1")
	outcomes, _, _, err := store.Import(ctx, "desk", []ImportedMessage{in}, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if outcomes[0].Outcome != ImportCreated || outcomes[0].Revision != 1 {
		t.Fatalf("outcome %+v, want created at revision 1", outcomes[0])
	}

	got, err := store.Metadata(ctx, "desk", outcomes[0].ID)
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if got.Subject != in.Subject || got.From != in.From || got.MessageID != "AAMkAD1@outlook.example" ||
		got.ThreadID != in.ConversationID || got.ProviderMessageID != in.ProviderMessageID ||
		got.SourceRevision != "ck-1" || !got.Date.Equal(in.ReceivedAt) {
		t.Fatalf("metadata changed on the way through: %+v", got)
	}
	if got.Outlook == nil || *got.Outlook != *in.Outlook {
		t.Fatalf("outlook location %+v, want %+v", got.Outlook, in.Outlook)
	}
	if len(got.Bcc) != 1 || got.Bcc[0] != in.Bcc[0] || len(got.Cc) != 1 || got.Cc[0] != in.Cc[0] {
		t.Fatalf("recipients changed: cc %+v bcc %+v", got.Cc, got.Bcc)
	}
	if got.Bodies.Text == nil || got.Bodies.Text.Bytes != int64(len(*in.BodyText)) ||
		got.Bodies.HTML == nil || got.Bodies.HTML.Bytes != int64(len(*in.BodyHTML)) {
		t.Fatalf("body sizes %+v", got.Bodies)
	}

	for format, want := range map[BodyFormat]string{BodyFormatText: *in.BodyText, BodyFormatHTML: *in.BodyHTML} {
		body, _, err := store.Body(ctx, "desk", outcomes[0].ID, format)
		if err != nil {
			t.Fatalf("Body %s: %v", format, err)
		}
		if body != want {
			t.Fatalf("%s body %q, want %q", format, body, want)
		}
	}
}

// Missing identity stays missing: the export is the source, and a Message-ID
// minted here would join threads and dedup against nothing real.
func TestAnImportInventsNoIdentity(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	in := exported("AAMkAD2")
	in.MessageID, in.ConversationID = "", ""
	outcomes, _, _, err := store.Import(ctx, "desk", []ImportedMessage{in}, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	got, err := store.Get(ctx, "desk", outcomes[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Meta.MessageID != "" || got.Meta.ThreadID != "" {
		t.Fatalf("message id %q thread id %q, want both empty", got.Meta.MessageID, got.Meta.ThreadID)
	}
	if _, ok := got.Headers["Message-ID"]; ok {
		t.Fatalf("a Message-ID header was synthesized: %q", got.Headers["Message-ID"])
	}
}

func TestAnAbsentBodyIsNotAnEmptyOne(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	in := exported("AAMkAD3")
	in.BodyText = strPtr("")
	in.BodyHTML = nil
	outcomes, _, _, err := store.Import(ctx, "desk", []ImportedMessage{in}, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	id := outcomes[0].ID

	body, _, err := store.Body(ctx, "desk", id, BodyFormatText)
	if err != nil || body != "" {
		t.Fatalf("text body %q err %v, want present and empty", body, err)
	}
	if _, _, err := store.Body(ctx, "desk", id, BodyFormatHTML); !errors.Is(err, ErrBodyAbsent) {
		t.Fatalf("html err %v, want ErrBodyAbsent", err)
	}
	if _, _, err := store.Body(ctx, "desk", "lm-missing", BodyFormatText); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing message err %v, want ErrNotFound", err)
	}
	meta, err := store.Metadata(ctx, "desk", id)
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if meta.Bodies.Text == nil || meta.Bodies.Text.Bytes != 0 || meta.Bodies.HTML != nil {
		t.Fatalf("bodies %+v, want text present with 0 bytes and html absent", meta.Bodies)
	}
}

func TestReimportIsIdempotentAndAChangeIsARevision(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	in := exported("AAMkAD4")
	first, _, _, err := store.Import(ctx, "desk", []ImportedMessage{in}, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	id := first[0].ID
	// Mailbox state set here must survive any re-import.
	if err := store.SetUnread(ctx, "desk", id, false); err != nil {
		t.Fatalf("SetUnread: %v", err)
	}

	again, changed, _, err := store.Import(ctx, "desk", []ImportedMessage{in}, nil)
	if err != nil {
		t.Fatalf("re-Import: %v", err)
	}
	if again[0].Outcome != ImportUnchanged || again[0].Revision != 1 || again[0].ID != id || len(changed) != 0 {
		t.Fatalf("replay outcome %+v changed %d, want unchanged at revision 1 on the same id", again[0], len(changed))
	}

	edited := in
	edited.BodyHTML = strPtr("<p>edited</p>")
	edited.SourceRevision = "ck-2"
	third, changed, _, err := store.Import(ctx, "desk", []ImportedMessage{edited}, nil)
	if err != nil {
		t.Fatalf("edited Import: %v", err)
	}
	if third[0].Outcome != ImportUpdated || third[0].Revision != 2 || third[0].PreviousRevision != 1 || len(changed) != 1 {
		t.Fatalf("edit outcome %+v, want updated 1 → 2", third[0])
	}
	body, meta, err := store.Body(ctx, "desk", id, BodyFormatHTML)
	if err != nil || body != "<p>edited</p>" {
		t.Fatalf("body after edit %q err %v", body, err)
	}
	if meta.Unread {
		t.Fatal("re-import reset the read state the mailbox had set")
	}
	if meta.SourceRevision != "ck-2" {
		t.Fatalf("source revision %q, want ck-2", meta.SourceRevision)
	}
}

func TestOneInvalidMessageRefusesTheWholeImport(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	good := exported("AAMkAD5")
	noDate := exported("AAMkAD6")
	noDate.ReceivedAt = time.Time{}
	noBody := exported("AAMkAD7")
	noBody.BodyText, noBody.BodyHTML = nil, nil
	duplicate := exported("AAMkAD5")

	_, _, _, err := store.Import(ctx, "desk", []ImportedMessage{good, noDate, noBody, duplicate},
		&ImportSource{Ref: "export-1.json", SHA256: "abc"})
	var invalid *ImportValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("err %v, want ImportValidationError", err)
	}
	if len(invalid.Items) != 3 {
		t.Fatalf("%d problems reported, want 3: %+v", len(invalid.Items), invalid.Items)
	}
	if _, err := store.MetadataByProviderID(ctx, "desk", "AAMkAD5"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the valid message was stored from a refused batch: %v", err)
	}
	sources, err := store.ImportSources(ctx, "desk")
	if err != nil {
		t.Fatalf("ImportSources: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("a refused import was checkpointed: %+v", sources)
	}
}

func TestAnImportSourceIsCheckpointedWithItsMessages(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	src := &ImportSource{Ref: "2026-10-01/export-17.json", SHA256: "1f2e"}
	if _, _, _, err := store.Import(ctx, "desk", []ImportedMessage{exported("A1"), exported("A2")}, src); err != nil {
		t.Fatalf("Import: %v", err)
	}
	sources, err := store.ImportSources(ctx, "desk")
	if err != nil {
		t.Fatalf("ImportSources: %v", err)
	}
	if len(sources) != 1 || sources[0].Ref != src.Ref || sources[0].SHA256 != src.SHA256 || sources[0].MessageCount != 2 {
		t.Fatalf("sources %+v", sources)
	}
	other, err := store.ImportSources(ctx, "other-account")
	if err != nil || len(other) != 0 {
		t.Fatalf("another account sees %+v (err %v)", other, err)
	}
}

// The same source id in two accounts is two messages. The account is the scope
// of every key here, so one account's export can never answer for another's.
func TestProviderIDsAreScopedToTheAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	a, _, _, err := store.Import(ctx, "desk-a", []ImportedMessage{exported("SAME")}, nil)
	if err != nil {
		t.Fatalf("Import a: %v", err)
	}
	b, _, _, err := store.Import(ctx, "desk-b", []ImportedMessage{exported("SAME")}, nil)
	if err != nil {
		t.Fatalf("Import b: %v", err)
	}
	if a[0].ID == b[0].ID || b[0].Outcome != ImportCreated {
		t.Fatalf("a %+v b %+v, want two separate created messages", a[0], b[0])
	}
	if _, err := store.Metadata(ctx, "desk-b", a[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("desk-b reads desk-a's message: %v", err)
	}
}

func TestPermDeleteRemovesTheBody(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	outcomes, _, _, err := store.Import(ctx, "desk", []ImportedMessage{exported("GONE")}, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if err := store.PermDelete(ctx, "desk", outcomes[0].ID); err != nil {
		t.Fatalf("PermDelete: %v", err)
	}
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM local_message_bodies`).Scan(&n); err != nil {
		t.Fatalf("count bodies: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d body rows left after PermDelete", n)
	}
}

// A database written before migrations existed sits at user_version 0 with the
// bodies inline. Opening it must move them out without losing one.
func TestTheOldInlineBodySchemaMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-mail.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(localMessagesSchema); err != nil {
		t.Fatalf("old schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO local_messages (account_id, id, message_id, subject, from_email,
		date_ms, body_text, body_html, created_at) VALUES
		('demo','lm1','m1@x','Text only','a@x',1000,'héllo','',1000),
		('demo','lm2','m2@x','HTML only','a@x',2000,'','<p>hi</p>',2000)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	db.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open old database: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("user_version %d err %v, want %d", version, err, len(migrations))
	}

	text, meta, err := store.Body(ctx, "demo", "lm1", BodyFormatText)
	if err != nil || text != "héllo" {
		t.Fatalf("lm1 text %q err %v", text, err)
	}
	if meta.Bodies.Text == nil || meta.Bodies.Text.Bytes != int64(len("héllo")) || meta.Bodies.HTML != nil {
		t.Fatalf("lm1 bodies %+v, want text of %d bytes and no html", meta.Bodies, len("héllo"))
	}
	if _, _, err := store.Body(ctx, "demo", "lm1", BodyFormatHTML); !errors.Is(err, ErrBodyAbsent) {
		t.Fatalf("lm1 html err %v, want ErrBodyAbsent: a blank inline body was never given", err)
	}
	full, err := store.Get(ctx, "demo", "lm2")
	if err != nil || full.BodyHTML != "<p>hi</p>" || full.BodyText != "" {
		t.Fatalf("lm2 %+v err %v", full, err)
	}
	if meta.Revision != 1 || meta.UpdatedAt.UnixMilli() != 1000 {
		t.Fatalf("revision %d updated_at %v, want 1 and the created time", meta.Revision, meta.UpdatedAt)
	}
}

// A lookup by provider id must use the partial unique index. Without it an
// import of n messages into a mailbox of m costs n×m row reads, which went
// unnoticed until a 50,000-message mailbox took over ten minutes to fill.
func TestLookupsByProviderIDUseTheIndex(t *testing.T) {
	store := testStore(t)
	for _, query := range []string{
		`SELECT revision, content_sha256 FROM local_messages WHERE account_id = ? AND provider_message_id = ? AND ` + providerIDIndexed,
		`SELECT ` + storedColumns + ` FROM local_messages WHERE account_id = ? AND provider_message_id = ? AND ` + providerIDIndexed,
	} {
		rows, err := store.db.Query(`EXPLAIN QUERY PLAN `+query, "desk", "AAMk")
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatalf("scan plan: %v", err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		if len(plan) != 1 || !strings.Contains(plan[0], "idx_local_messages_provider_message_id") {
			t.Fatalf("plan %q does not use the provider id index", plan)
		}
	}
}
