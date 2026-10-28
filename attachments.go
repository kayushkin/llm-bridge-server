package activitystore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// attachmentDirectory is where attachment files live, under the data dir,
// each named by its sha256 so the same file is kept once.
const attachmentDirectory = "attachments"

// maxAttachmentBytes bounds one attachment; a voice search is well under it.
const maxAttachmentBytes = 32 << 20

func attachmentsOf(q queryer, activityID string) ([]Attachment, error) {
	rows, err := q.Query(`SELECT id, activity_id, file_name, content_type, sha256, size_bytes, created_at, purged_at
		FROM attachments WHERE activity_id = ? ORDER BY seq`, activityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attachments := []Attachment{}
	for rows.Next() {
		var attachment Attachment
		if err := rows.Scan(&attachment.ID, &attachment.ActivityID, &attachment.FileName, &attachment.ContentType, &attachment.Sha256,
			&attachment.SizeBytes, &attachment.CreatedAt, &attachment.PurgedAt); err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, rows.Err()
}

// decodedAttachment is an ImportAttachment checked and decoded.
type decodedAttachment struct {
	fileName, contentType, sha256, storedPath string
	content                                   []byte
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
	hash := hex.EncodeToString(digest[:])
	extension := strings.ToLower(filepath.Ext(attachment.FileName))
	return decodedAttachment{fileName: attachment.FileName, contentType: attachment.ContentType, sha256: hash, content: content,
		storedPath: filepath.Join(attachmentDirectory, hash[:2], hash+extension)}, nil
}

// addAttachment writes the file (once per hash) and records it on the
// activity, unless the activity already has it.
func (s *Store) addAttachment(tx *sql.Tx, activityID string, attachment decodedAttachment) (bool, error) {
	var present int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM attachments WHERE activity_id = ? AND sha256 = ?`, activityID, attachment.sha256).Scan(&present); err != nil {
		return false, err
	}
	if present > 0 {
		return false, nil
	}
	path := filepath.Join(s.dataDir, attachment.storedPath)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return false, err
		}
		temporary := path + ".partial"
		if err := os.WriteFile(temporary, attachment.content, 0o600); err != nil {
			return false, err
		}
		if err := os.Rename(temporary, path); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	id, seq, err := nextID(tx, "attachments", IDPrefixAttachment)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(`INSERT INTO attachments (id, seq, activity_id, file_name, content_type, sha256, size_bytes, stored_path, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, seq, activityID, attachment.fileName, attachment.contentType, attachment.sha256,
		len(attachment.content), attachment.storedPath, now())
	return err == nil, err
}

// purgeAttachments blanks an activity's attachments and answers the files no
// other live attachment still uses, for the caller to remove once the
// transaction commits.
func purgeAttachments(tx *sql.Tx, activityID string, stamp int64) ([]string, error) {
	rows, err := tx.Query(`SELECT id, sha256, stored_path FROM attachments WHERE activity_id = ? AND purged_at = 0`, activityID)
	if err != nil {
		return nil, err
	}
	type purged struct{ id, sha256, storedPath string }
	var found []purged
	for rows.Next() {
		var attachment purged
		if err := rows.Scan(&attachment.id, &attachment.sha256, &attachment.storedPath); err != nil {
			rows.Close()
			return nil, err
		}
		found = append(found, attachment)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var files []string
	for _, attachment := range found {
		if _, err := tx.Exec(`UPDATE attachments SET file_name = '', stored_path = '', purged_at = ? WHERE id = ?`, stamp, attachment.id); err != nil {
			return nil, err
		}
		var stillUsed int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM attachments WHERE sha256 = ? AND purged_at = 0`, attachment.sha256).Scan(&stillUsed); err != nil {
			return nil, err
		}
		if stillUsed == 0 {
			files = append(files, attachment.storedPath)
		}
	}
	return files, nil
}

// AttachmentFile answers an attachment and the path of its file. A purged
// attachment is ErrGone.
func (s *Store) AttachmentFile(id string) (Attachment, string, error) {
	var attachment Attachment
	var storedPath string
	err := s.db.QueryRow(`SELECT id, activity_id, file_name, content_type, sha256, size_bytes, created_at, purged_at, stored_path
		FROM attachments WHERE id = ?`, id).Scan(&attachment.ID, &attachment.ActivityID, &attachment.FileName, &attachment.ContentType,
		&attachment.Sha256, &attachment.SizeBytes, &attachment.CreatedAt, &attachment.PurgedAt, &storedPath)
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, "", fmt.Errorf("%w: no attachment %s", ErrNotFound, id)
	}
	if err != nil {
		return Attachment{}, "", err
	}
	if attachment.PurgedAt != 0 {
		return Attachment{}, "", fmt.Errorf("%w: attachment %s was purged", ErrGone, id)
	}
	return attachment, filepath.Join(s.dataDir, storedPath), nil
}
