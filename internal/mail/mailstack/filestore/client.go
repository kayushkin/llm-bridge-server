// Package filestore is file-store as mailstack uses it. file-store owns
// uploaded bytes; mailstack owns which file hangs on which message and
// revision, and who may read it. So file-store's answers come through
// unchanged, and mailstack re-describes neither.
//
// file-store runs in the same process; the server that mounts mailstack gives
// it the module as a Files. It is the same shape as kanban-store's
// internal/filestore, which cannot be imported from here.
package filestore

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// OwnerService is how mailstack names itself as the owner of a file.
const OwnerService = "mailstack"

// Files is file-store as mailstack calls it.
type Files interface {
	// Upload keeps content as a file of ownerRef's. A file file-store refuses
	// is a *RefusalError.
	Upload(content []byte, contentType, filename, ownerRef string) (*File, error)
	// ServeContent answers r with a file's bytes and file-store's own
	// headers. Only r's Range and conditional headers reach file-store. A
	// file it does not hold is ErrNotFound, with nothing written.
	ServeContent(w http.ResponseWriter, r *http.Request, fileID string, inline bool) error
	// Purge destroys a file. A file file-store does not hold is ErrNotFound,
	// which a caller purging what it owns may take as done.
	Purge(fileID string) error
}

// File is file-store's record. Only the fields mailstack reads are named.
type File struct {
	ID          string `json:"id"`
	SHA256      string `json:"sha256"`
	SizeBytes   int64  `json:"size_bytes"`
	ContentType string `json:"content_type"`
	Filename    string `json:"filename"`
}

// RefusalError is file-store saying no: its status and its body, to relay.
type RefusalError struct {
	Status int
	Body   []byte
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("file-store refused: %d %s", e.Status, strings.TrimSpace(string(e.Body)))
}

// ErrNotFound is file-store not having that file.
var ErrNotFound = errors.New("file-store has no such file")
