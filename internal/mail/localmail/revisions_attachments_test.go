package localmail

import (
	"context"
	"errors"
	"testing"
)

func withAttachment(m ImportedMessage, fileID, filename string, content string) ImportedMessage {
	m.Attachments = append(append([]ImportedAttachment(nil), m.Attachments...), ImportedAttachment{
		SourceAttachmentID: "src-" + filename, Filename: filename, ContentType: "application/pdf",
		ContentID: "<" + filename + "@cid>", Content: []byte(content), FileID: fileID,
	})
	return m
}

// Every revision an import replaces stays readable, whole: its metadata, both
// bodies with absent still absent, and its own attachments.
func TestReplacedRevisionsStayReadable(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	first := withAttachment(exported("HIST"), "file_000001", "invoice.pdf", "pdf v1")
	outcomes, _, _, err := store.Import(ctx, "desk", []ImportedMessage{first}, nil)
	if err != nil {
		t.Fatalf("Import 1: %v", err)
	}
	id := outcomes[0].ID

	second := exported("HIST")
	second.Subject = "second subject"
	second.BodyHTML = nil
	second = withAttachment(second, "file_000002", "invoice.pdf", "pdf v2")
	if _, _, _, err := store.Import(ctx, "desk", []ImportedMessage{second}, nil); err != nil {
		t.Fatalf("Import 2: %v", err)
	}
	third := second
	third.Subject = "third subject"
	third.Attachments = nil
	if _, _, _, err := store.Import(ctx, "desk", []ImportedMessage{third}, nil); err != nil {
		t.Fatalf("Import 3: %v", err)
	}

	revisions, err := store.Revisions(ctx, "desk", id)
	if err != nil {
		t.Fatalf("Revisions: %v", err)
	}
	if len(revisions) != 3 || revisions[0].Revision != 3 || !revisions[0].Current || revisions[2].Revision != 1 || revisions[2].ReplacedAt == nil {
		t.Fatalf("revisions %+v, want 3, 2, 1 with only 3 current", revisions)
	}

	old, err := store.Revision(ctx, "desk", id, 1)
	if err != nil {
		t.Fatalf("Revision 1: %v", err)
	}
	if old.Subject != first.Subject || old.Revision != 1 || len(old.Attachments) != 1 || old.Attachments[0].Size != int64(len("pdf v1")) {
		t.Fatalf("revision 1 %+v", old)
	}
	html, _, err := store.RevisionBody(ctx, "desk", id, 1, BodyFormatHTML)
	if err != nil || html != *first.BodyHTML {
		t.Fatalf("revision 1 html %q err %v", html, err)
	}
	if _, _, err := store.RevisionBody(ctx, "desk", id, 2, BodyFormatHTML); !errors.Is(err, ErrBodyAbsent) {
		t.Fatalf("revision 2 html err %v, want ErrBodyAbsent", err)
	}
	if _, err := store.Revision(ctx, "desk", id, 9); !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("revision 9 err %v, want ErrRevisionNotFound", err)
	}

	current, err := store.Metadata(ctx, "desk", id)
	if err != nil || current.Subject != "third subject" || len(current.Attachments) != 0 || current.HasAttach {
		t.Fatalf("current %+v err %v", current, err)
	}
	for revision, wantFile := range map[int64]string{1: "file_000001", 2: "file_000002"} {
		_, fileID, err := store.AttachmentFile(ctx, "desk", id, revision, "a1")
		if err != nil || fileID != wantFile {
			t.Fatalf("revision %d a1 file %q err %v, want %s", revision, fileID, err, wantFile)
		}
	}
	if _, _, err := store.AttachmentFile(ctx, "desk", id, 0, "a1"); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("current a1 err %v, want ErrAttachmentNotFound", err)
	}

	files, err := store.AttachmentFileIDs(ctx, "desk", id)
	if err != nil || len(files) != 2 {
		t.Fatalf("attachment files %v err %v, want both revisions'", files, err)
	}
	if err := store.PermDelete(ctx, "desk", id); err != nil {
		t.Fatalf("PermDelete: %v", err)
	}
	for _, table := range []string{"local_message_revisions", "local_message_attachments", "local_message_bodies"} {
		var n int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s has %d rows after PermDelete (err %v)", table, n, err)
		}
	}
}

func TestAnAttachmentChangesTheContentAndReplayReturnsTheUpload(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	m := withAttachment(exported("ATT"), "file_000010", "a.pdf", "one")
	if _, _, _, err := store.Import(ctx, "desk", []ImportedMessage{m}, nil); err != nil {
		t.Fatalf("Import: %v", err)
	}
	plan, err := store.PlanImport(ctx, "desk", []ImportedMessage{withAttachment(exported("ATT"), "", "a.pdf", "one")})
	if err != nil || plan[0] != ImportUnchanged {
		t.Fatalf("plan %v err %v, want unchanged for the same bytes", plan, err)
	}
	plan, err = store.PlanImport(ctx, "desk", []ImportedMessage{withAttachment(exported("ATT"), "", "a.pdf", "two")})
	if err != nil || plan[0] != ImportUpdated {
		t.Fatalf("plan %v err %v, want updated for different bytes", plan, err)
	}

	replay := withAttachment(exported("ATT"), "file_000011", "a.pdf", "one")
	outcomes, _, unused, err := store.Import(ctx, "desk", []ImportedMessage{replay}, nil)
	if err != nil || outcomes[0].Outcome != ImportUnchanged || len(unused) != 1 || unused[0] != "file_000011" {
		t.Fatalf("replay %+v unused %v err %v, want unchanged and the upload handed back", outcomes, unused, err)
	}

	notUploaded := withAttachment(exported("ATT"), "", "a.pdf", "three")
	if _, _, _, err := store.Import(ctx, "desk", []ImportedMessage{notUploaded}, nil); !errors.Is(err, ErrAttachmentsNotUploaded) {
		t.Fatalf("err %v, want ErrAttachmentsNotUploaded", err)
	}
}

func TestAnEmptyAttachmentIsRefusedBeforeUpload(t *testing.T) {
	m := withAttachment(exported("EMPTY"), "", "empty.txt", "")
	var invalid *ImportValidationError
	if err := ValidateImport([]ImportedMessage{m}); !errors.As(err, &invalid) {
		t.Fatalf("err %v, want a validation error", err)
	}
}
