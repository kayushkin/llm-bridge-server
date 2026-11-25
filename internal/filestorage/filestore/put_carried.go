package filestore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// PutCarried records the file at sourcePath as a new file, carrying its bytes
// into this store as carry says: a copy, or a hard link that takes no room
// when both directories are on one file system. It is for another module's
// import of bytes a standalone service kept on disk, which would otherwise be
// read whole into memory and written again. The source is only read; a blob
// already here with the same bytes is kept, and the bytes are checked against
// their hash after they are carried. An empty source is refused, as an empty
// upload is.
func (s *Store) PutCarried(sourcePath string, carry BlobCarry, file NewFile) (*File, error) {
	if carry != CopyBlobs && carry != LinkBlobs {
		return nil, fmt.Errorf("carry %q is neither %q nor %q", carry, CopyBlobs, LinkBlobs)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("open the file to carry: %w", err)
	}
	hasher := sha256.New()
	head := &headCapture{limit: 512}
	size, err := io.Copy(io.MultiWriter(hasher, head), source)
	closeErr := source.Close()
	if err != nil {
		return nil, fmt.Errorf("read the file to carry: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close the file to carry: %w", closeErr)
	}
	if size == 0 {
		return nil, ErrEmptyFile
	}
	sha := hex.EncodeToString(hasher.Sum(nil))

	s.blobMutex.Lock()
	defer s.blobMutex.Unlock()
	if _, err := s.carryBlob(sourcePath, sha, size, carry); err != nil {
		return nil, err
	}
	stored := &File{
		SHA256: sha, SizeBytes: size,
		ContentType: file.ContentType, DetectedContentType: http.DetectContentType(head.bytes),
		Filename: file.Filename, OwnerService: file.OwnerService, OwnerRef: file.OwnerRef,
		UploadedByPrincipalID: file.UploadedByPrincipalID, CreatedAt: time.Now().Unix(),
	}
	var seq int64
	if err := s.db.QueryRow(`INSERT INTO files
		(sha256, size_bytes, content_type, detected_content_type, filename, owner_service, owner_ref, uploaded_by_principal_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING seq`,
		stored.SHA256, stored.SizeBytes, stored.ContentType, stored.DetectedContentType, stored.Filename,
		stored.OwnerService, stored.OwnerRef, stored.UploadedByPrincipalID, stored.CreatedAt).Scan(&seq); err != nil {
		// As in Put: a blob no row names is harmless, and removing it here could
		// take the bytes of an older row that names the same hash.
		return nil, fmt.Errorf("record the file: %w", err)
	}
	stored.ID = formatID(seq)
	return stored, nil
}
