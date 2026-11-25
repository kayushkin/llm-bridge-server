// Package filestore is file-store as kanban-store uses it. file-store owns
// uploaded bytes; kanban-store owns which files hang on which card and who may
// see them. So file-store's records and answers come through unchanged, and
// kanban-store re-describes neither.
//
// file-store runs in the same process; the server that mounts kanban-store
// gives it the module as a Files.
package filestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OwnerService is how kanban-store names itself as the owner of a file.
const OwnerService = "kanban-store"

// Files is file-store as kanban-store calls it.
type Files interface {
	// Upload keeps content as a file of cardID's. contentLength is -1 when
	// it is not known. A file file-store refuses is a *RefusalError.
	Upload(content io.Reader, contentLength int64, contentType, filename, cardID, uploadedByPrincipalID string) (*File, error)
	// FilesOfCard is every live file file-store holds for a card, by id.
	FilesOfCard(cardID string) (map[string]*File, error)
	// EveryFileOfCard is FilesOfCard with the files file-store holds as
	// deleted: the ones that were taken off the card, which file-store keeps
	// so that a removal can be undone. Purging a card has to reach those too.
	EveryFileOfCard(cardID string) (map[string]*File, error)
	// ServeContent answers r with a file's bytes and file-store's own headers,
	// which are what keep somebody's upload from running in the reader's
	// browser. Only r's Range and conditional headers reach file-store. A file
	// it does not hold is ErrNotFound, with nothing written.
	ServeContent(w http.ResponseWriter, r *http.Request, fileID string, inline bool) error
	// Delete removes a file. Reversible unless purge, which destroys it. A
	// file file-store does not hold is ErrNotFound.
	Delete(fileID string, purge bool) error
}

// File is file-store's record, passed through as file-store gave it. Only the
// fields kanban-store itself reads are named; the rest ride along in Raw.
type File struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	SizeBytes   int64  `json:"size_bytes"`
	ContentType string `json:"content_type"`
	// Raw is the whole record, which is what goes out on kanban-store's wire.
	Raw json.RawMessage `json:"-"`
}

// FileOfRecord reads the fields kanban-store names out of record, file-store's
// record encoded as file-store's routes answer it, and keeps the whole of it
// in Raw.
func FileOfRecord(record json.RawMessage) (*File, error) {
	var file File
	if err := json.Unmarshal(record, &file); err != nil {
		return nil, fmt.Errorf("file-store's record does not parse: %w", err)
	}
	file.Raw = append(json.RawMessage(nil), record...)
	return &file, nil
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
