package activitystore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// maxAttachmentBytes bounds one attachment; a voice search is well under it.
const maxAttachmentBytes = 32 << 20

// Each attachment row has a file-store file of its own. file-store keeps the
// bytes of two files with the same sha256 once and destroys them only with
// the last file that names them, so purging one attachment's file never takes
// bytes another attachment still needs.

func attachmentsOf(ctx context.Context, q queryer, activitySeq int64) ([]Attachment, error) {
	rows, err := q.QueryContext(ctx, `SELECT seq, activity_seq, file_name, content_type, sha256, size_bytes, created_at, purged_at
		FROM attachments WHERE activity_seq = $1 ORDER BY seq`, activitySeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attachments := []Attachment{}
	for rows.Next() {
		attachment, _, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, rows.Err()
}

func scanAttachment(row rowScanner, extra ...any) (Attachment, int64, error) {
	var attachment Attachment
	var seq, activitySeq int64
	if err := row.Scan(append([]any{&seq, &activitySeq, &attachment.FileName, &attachment.ContentType, &attachment.Sha256,
		&attachment.SizeBytes, &attachment.CreatedAt, &attachment.PurgedAt}, extra...)...); err != nil {
		return Attachment{}, 0, err
	}
	attachment.ID = postgresstorage.FormatRecordID(IDPrefixAttachment, seq)
	attachment.ActivityID = postgresstorage.FormatRecordID(IDPrefixActivity, activitySeq)
	return attachment, seq, nil
}

// decodedAttachment is an ImportAttachment checked and decoded.
type decodedAttachment struct {
	fileName, contentType, sha256 string
	content                       []byte
}

func decodeAttachment(attachment ImportAttachment) (decodedAttachment, error) {
	if strings.TrimSpace(attachment.FileName) == "" || strings.ContainsAny(attachment.FileName, `/\`) {
		return decodedAttachment{}, fmt.Errorf("%w: an attachment needs a file_name with no directory in it, got %q", ErrInvalid, attachment.FileName)
	}
	if attachment.ContentType == "" {
		return decodedAttachment{}, fmt.Errorf("%w: attachment %s has no content_type", ErrInvalid, attachment.FileName)
	}
	content, err := base64.StdEncoding.DecodeString(attachment.ContentBase64)
	if err != nil {
		return decodedAttachment{}, fmt.Errorf("%w: attachment %s is not base64: %v", ErrInvalid, attachment.FileName, err)
	}
	if len(content) == 0 || len(content) > maxAttachmentBytes {
		return decodedAttachment{}, fmt.Errorf("%w: attachment %s is %d bytes; it must be 1 to %d", ErrInvalid, attachment.FileName, len(content), maxAttachmentBytes)
	}
	digest := sha256.Sum256(content)
	return decodedAttachment{fileName: attachment.FileName, contentType: attachment.ContentType, sha256: hex.EncodeToString(digest[:]), content: content}, nil
}

// addAttachment hands the file to file-store and records it on the activity,
// unless the activity already has it. The id of a file it kept goes into
// kept, so a transaction that then fails can destroy it.
func (s *Store) addAttachment(ctx context.Context, tx *sql.Tx, owner string, activitySeq int64, attachment decodedAttachment, kept *[]string) (bool, error) {
	var present int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments WHERE activity_seq = $1 AND sha256 = $2`, activitySeq, attachment.sha256).Scan(&present); err != nil {
		return false, err
	}
	if present > 0 {
		return false, nil
	}
	fileID, err := s.files.Put(ctx, bytes.NewReader(attachment.content), AttachmentFile{
		ActivityID: postgresstorage.FormatRecordID(IDPrefixActivity, activitySeq), FileName: attachment.fileName, ContentType: attachment.contentType})
	if err != nil {
		return false, fmt.Errorf("keeping attachment %s in file-store: %w", attachment.fileName, err)
	}
	*kept = append(*kept, fileID)
	_, err = tx.ExecContext(ctx, `INSERT INTO attachments (owner_principal_id, activity_seq, file_name, content_type, sha256, size_bytes, file_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, owner, activitySeq, attachment.fileName, attachment.contentType, attachment.sha256,
		len(attachment.content), fileID, now())
	return err == nil, err
}

// purgeAttachments blanks an activity's attachments and answers the file-store
// files they named, for the caller to destroy once the transaction commits.
func purgeAttachments(ctx context.Context, tx *sql.Tx, activitySeq int64, stamp int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT file_id FROM attachments WHERE activity_seq = $1 AND purged_at = 0 ORDER BY seq FOR UPDATE`, activitySeq)
	if err != nil {
		return nil, err
	}
	var files []string
	for rows.Next() {
		var fileID string
		if err := rows.Scan(&fileID); err != nil {
			rows.Close()
			return nil, err
		}
		files = append(files, fileID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attachments SET file_name = '', file_id = '', purged_at = $1
		WHERE activity_seq = $2 AND purged_at = 0`, stamp, activitySeq); err != nil {
		return nil, err
	}
	return files, nil
}

// OpenAttachment answers an attachment in scope and its bytes, which the
// caller closes. A purged attachment is ErrGone; one out of scope is
// ErrNotFound, as a missing one is.
func (s *Store) OpenAttachment(ctx context.Context, scope ownership.Scope, id string) (Attachment, io.ReadCloser, error) {
	seq, err := numberOf(IDPrefixAttachment, id)
	if err != nil {
		return Attachment{}, nil, err
	}
	args := arguments{seq}
	condition, err := scopeCondition(scope, "owner_principal_id", &args)
	if err != nil {
		return Attachment{}, nil, err
	}
	var fileID string
	attachment, _, err := scanAttachment(s.db.QueryRowContext(ctx, `SELECT seq, activity_seq, file_name, content_type, sha256, size_bytes, created_at, purged_at, file_id
		FROM attachments WHERE seq = $1 AND `+condition, args...), &fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, nil, fmt.Errorf("%w: no attachment %s", ErrNotFound, id)
	}
	if err != nil {
		return Attachment{}, nil, err
	}
	if attachment.PurgedAt != 0 {
		return Attachment{}, nil, fmt.Errorf("%w: attachment %s was purged", ErrGone, id)
	}
	content, err := s.files.Open(ctx, fileID)
	if err != nil {
		return Attachment{}, nil, fmt.Errorf("attachment %s: file-store cannot open its file %s: %w", id, fileID, err)
	}
	return attachment, content, nil
}
