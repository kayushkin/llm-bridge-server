package possessionstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// The originals possession-add-photos brings in: the full-size photo, kept by
// the sha256 of its bytes, which photo-store's photo carries too. The readers
// see an original scaled down when the owner keeps one, and photo-store's
// thumbnail otherwise (images.go). The bytes are a file in file-store; the
// originals table names it.

// OriginalMediaTypes are the kinds of original kept, by file extension.
var OriginalMediaTypes = map[string]string{".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".webp": "image/webp"}

// OriginalFiles keeps the bytes of originals: file-store, in the server.
type OriginalFiles interface {
	// Keep stores content as a new file the owner keeps, and answers its id.
	Keep(ctx context.Context, ownerPrincipalID, sha256, mediaType string, content []byte) (string, error)
	// KeepCopyOf stores the file at path, which an import carries in.
	KeepCopyOf(ctx context.Context, ownerPrincipalID, sha256, mediaType, path string) (string, error)
	// Open reads a file Keep or KeepCopyOf answered.
	Open(ctx context.Context, fileID string) (io.ReadCloser, error)
	// Destroy removes a file whose row was never written.
	Destroy(ctx context.Context, fileID string) error
}

// Original is one kept original.
type Original struct {
	OwnerPrincipalID string `json:"owner_principal_id"`
	Sha256           string `json:"sha256"`
	Extension        string `json:"extension"`
	MediaType        string `json:"media_type"`
	FileID           string `json:"file_id"`
	ByteCount        int64  `json:"byte_count"`
	AddedBy          string `json:"added_by"`
	AddedAt          int64  `json:"added_at"`
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// KeepOriginal keeps content as the owner's original with that sha256, which
// must be the sha256 of content. An original the owner already keeps is
// answered as it is, with created false.
func (s *Store) KeepOriginal(ctx context.Context, sum, extension string, content []byte, actor string) (Original, bool, error) {
	owner, err := s.writeOwner()
	if err != nil {
		return Original{}, false, err
	}
	if actor == "" {
		return Original{}, false, fmt.Errorf("%w: added_by is required", ErrInvalid)
	}
	if !sha256Pattern.MatchString(sum) {
		return Original{}, false, fmt.Errorf("%w: %q is not a sha256 in lower-case hex", ErrInvalid, sum)
	}
	extension = strings.ToLower(extension)
	mediaType, known := OriginalMediaTypes[extension]
	if !known {
		return Original{}, false, fmt.Errorf("%w: %q originals are not kept; send JPEG, PNG or WebP", ErrInvalid, extension)
	}
	computed := sha256.Sum256(content)
	if hex.EncodeToString(computed[:]) != sum {
		return Original{}, false, fmt.Errorf("%w: the bytes sent do not have sha256 %s", ErrInvalid, sum)
	}
	if existing, err := s.original(sum); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Original{}, false, err
	}
	fileID, err := s.originals.Keep(ctx, owner, sum, mediaType, content)
	if err != nil {
		return Original{}, false, err
	}
	s.writeMutex.Lock()
	_, err = s.db.ExecContext(ctx, `INSERT INTO originals (sha256, extension, media_type, file_id, byte_count, added_by, added_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sum, extension, mediaType, fileID, len(content), actor, s.unixNow())
	s.writeMutex.Unlock()
	if err != nil {
		if destroyErr := s.originals.Destroy(ctx, fileID); destroyErr != nil {
			err = errors.Join(err, fmt.Errorf("destroying the file kept for it: %w", destroyErr))
		}
		if isUniqueViolation(err) {
			return Original{}, false, fmt.Errorf("%w: original %s was kept by another request at the same time", ErrConflict, sum)
		}
		return Original{}, false, err
	}
	kept, err := s.original(sum)
	return kept, true, err
}

func (s *Store) original(sum string) (Original, error) {
	var original Original
	err := s.db.QueryRow(`SELECT owner_principal_id, sha256, extension, media_type, file_id, byte_count, added_by, added_at FROM originals WHERE sha256 = ?`, sum).
		Scan(&original.OwnerPrincipalID, &original.Sha256, &original.Extension, &original.MediaType, &original.FileID, &original.ByteCount, &original.AddedBy, &original.AddedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Original{}, fmt.Errorf("%w: no original %s", ErrNotFound, sum)
	}
	return original, err
}

// OriginalBytes reads the original ownerPrincipalID keeps with that sha256,
// and false when they keep none.
func (s *Store) OriginalBytes(ctx context.Context, ownerPrincipalID, sum string) ([]byte, bool, error) {
	owned, err := s.NarrowedToOwner(ctx, ownerPrincipalID)
	if err != nil {
		return nil, false, err
	}
	original, err := owned.original(sum)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	file, err := s.originals.Open(ctx, original.FileID)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, false, fmt.Errorf("read original %s: %w", sum, err)
	}
	return content, true, nil
}
