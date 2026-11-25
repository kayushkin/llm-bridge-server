package localmail

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/backend"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/mailmodule/mailmoduletest"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	return New(mailmoduletest.NewDB(t))
}

func msg(subject, from string, sentAt time.Time) backend.Message {
	return backend.Message{
		Meta: backend.MessageMeta{
			Subject: subject,
			From:    backend.Address{Name: "Sender", Email: from},
			Date:    sentAt,
			Unread:  true,
		},
		BodyText: "Body of " + subject,
	}
}

func TestAppendAssignsWhatAProviderWouldAssign(t *testing.T) {
	store := testStore(t)
	meta, err := store.Append(context.Background(), "demo", msg("Hello", "a@x.example", time.Now()))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if meta.ID == "" {
		t.Fatal("no opaque id assigned")
	}
	if meta.MessageID == "" {
		t.Fatal("no Message-ID minted")
	}
	if meta.ThreadID != meta.MessageID {
		t.Fatalf("thread id %q, want it to be the message's own id for a new thread", meta.ThreadID)
	}
	if meta.Snippet == "" {
		t.Fatal("no snippet derived from the body")
	}
	if meta.FolderID != DefaultFolderID {
		t.Fatalf("folder %q, want %q", meta.FolderID, DefaultFolderID)
	}
}

func TestAppendRefusesAMessageWithNoBody(t *testing.T) {
	store := testStore(t)
	m := msg("Empty", "a@x.example", time.Now())
	m.BodyText = ""
	if _, err := store.Append(context.Background(), "demo", m); err == nil {
		t.Fatal("stored a message with no body")
	}
}

func TestAppendRefusesAnUnknownFolder(t *testing.T) {
	store := testStore(t)
	m := msg("Misfiled", "a@x.example", time.Now())
	m.Meta.FolderID = "NOWHERE"
	if _, err := store.Append(context.Background(), "demo", m); err == nil {
		t.Fatal("stored a message into a folder that does not exist")
	}
}

// Appending the same message twice is a collision, not a second copy. A corpus
// generator that reruns must not double every message in the mailbox.
func TestAppendingTheSameMessageTwiceCollides(t *testing.T) {
	store := testStore(t)
	sent := time.Now()
	if _, err := store.Append(context.Background(), "demo", msg("Once", "a@x.example", sent)); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	if _, err := store.Append(context.Background(), "demo", msg("Once", "a@x.example", sent)); err == nil {
		t.Fatal("the same message stored twice")
	}
}

func TestAReplyJoinsItsParentsThread(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	parent, err := store.Append(ctx, "demo", msg("Original", "a@x.example", time.Now().Add(-time.Hour)))
	if err != nil {
		t.Fatalf("Append parent: %v", err)
	}

	reply := msg("Re: Original", "b@x.example", time.Now())
	reply.Meta.InReplyTo = parent.MessageID
	reply.Meta.References = []string{parent.MessageID}
	child, err := store.Append(ctx, "demo", reply)
	if err != nil {
		t.Fatalf("Append reply: %v", err)
	}
	if child.ThreadID != parent.ThreadID {
		t.Fatalf("reply thread %q, parent thread %q — they must match", child.ThreadID, parent.ThreadID)
	}
}

func TestAngleBracketsAreStrippedFromIdentifiers(t *testing.T) {
	store := testStore(t)
	m := msg("Bracketed", "a@x.example", time.Now())
	m.Meta.MessageID = "<abc@mail.example>"
	m.Meta.InReplyTo = "<parent@mail.example>"
	meta, err := store.Append(context.Background(), "demo", m)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if meta.MessageID != "abc@mail.example" {
		t.Fatalf("message id %q still bracketed", meta.MessageID)
	}
	if meta.InReplyTo != "parent@mail.example" {
		t.Fatalf("in-reply-to %q still bracketed", meta.InReplyTo)
	}
}

func TestListIsNewestFirstAndPagesWithoutRepeating(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	base := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 7; i++ {
		if _, err := store.Append(ctx, "demo", msg("Message "+string(rune('A'+i)), "a@x.example", base.Add(time.Duration(i)*time.Hour))); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	seen := map[string]int{}
	var cursor string
	newest := time.Time{}
	for page := 0; ; page++ {
		if page > 20 {
			t.Fatal("paging did not terminate")
		}
		msgs, next, err := store.List(ctx, "demo", "INBOX", cursor, 3)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for i, m := range msgs {
			seen[m.ID]++
			if page == 0 && i == 0 {
				newest = m.Date
			} else if m.Date.After(newest) {
				t.Fatalf("message %s at %s is newer than the first page's first result %s", m.ID, m.Date, newest)
			}
			newest = m.Date
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 7 {
		t.Fatalf("paged over %d messages, want 7", len(seen))
	}
	for id, times := range seen {
		if times != 1 {
			t.Fatalf("message %s returned %d times", id, times)
		}
	}
}

func TestAMalformedCursorIsRefusedNotRestarted(t *testing.T) {
	store := testStore(t)
	if _, _, err := store.List(context.Background(), "demo", "INBOX", "!!!not-a-cursor", 10); err == nil {
		t.Fatal("a malformed cursor silently restarted the listing")
	}
}

func TestGetReturnsTheBodyAndSynthesizedHeaders(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	meta, err := store.Append(ctx, "demo", msg("Readable", "a@x.example", time.Now()))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := store.Get(ctx, "demo", meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.BodyText != "Body of Readable" {
		t.Fatalf("body %q", got.BodyText)
	}
	if got.Headers["Message-ID"] != "<"+meta.MessageID+">" {
		t.Fatalf("Message-ID header %q", got.Headers["Message-ID"])
	}
}

func TestGetOnAnUnknownMessageIsNotFound(t *testing.T) {
	store := testStore(t)
	if _, err := store.Get(context.Background(), "demo", "lm0000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestMessagesAreScopedToTheirAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	meta, err := store.Append(ctx, "demo", msg("Private", "a@x.example", time.Now()))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := store.Get(ctx, "other-account", meta.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account could read the message: %v", err)
	}
}

func TestFoldersAlwaysIncludeAnInbox(t *testing.T) {
	// A consumer that resolves the mailbox by canonical kind errors out on an
	// account with no inbox, so this is not cosmetic.
	folders, err := testStore(t).Folders(context.Background(), "demo")
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	for _, f := range folders {
		if f.Canonical == backend.KindInbox {
			return
		}
	}
	t.Fatalf("no folder with canonical=inbox in %+v", folders)
}

func TestFoldersReportUnreadCounts(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := store.Append(ctx, "demo", msg("Unread "+string(rune('A'+i)), "a@x.example", time.Now().Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	folders, err := store.Folders(ctx, "demo")
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	for _, f := range folders {
		if f.Canonical == backend.KindInbox && f.UnreadCount != 3 {
			t.Fatalf("inbox unread %d, want 3", f.UnreadCount)
		}
	}
}

func TestMarkingReadAndMovingAreRealOperations(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	meta, err := store.Append(ctx, "demo", msg("Mutable", "a@x.example", time.Now()))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.SetUnread(ctx, "demo", meta.ID, false); err != nil {
		t.Fatalf("SetUnread: %v", err)
	}
	got, err := store.Get(ctx, "demo", meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Meta.Unread {
		t.Fatal("message still unread after being marked read")
	}

	if err := store.Delete(ctx, "demo", meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err = store.Get(ctx, "demo", meta.ID)
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if got.Meta.FolderID != "TRASH" {
		t.Fatalf("folder %q after delete, want TRASH — a plain delete must be reversible", got.Meta.FolderID)
	}
}

// A mutation that matched no row must say so. Reporting success for a message
// that does not exist is the failure that looks like working software.
func TestMutatingAnUnknownMessageIsNotFound(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.SetUnread(ctx, "demo", "lm-missing", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetUnread err = %v, want ErrNotFound", err)
	}
	if err := store.PermDelete(ctx, "demo", "lm-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PermDelete err = %v, want ErrNotFound", err)
	}
}
