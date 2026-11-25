package filestore

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"time"
)

// The calls a module in the same process makes, with no request between. The
// routes answer through the same methods, so a refusal is the same whichever
// way the file came.

// Refusal is file-store saying no to a file, with the status its route
// answers and the message it gives.
type Refusal struct {
	Status  int
	Message string
}

func (refusal *Refusal) Error() string { return refusal.Message }

// Write answers the refusal as the routes do: its status and
// {"error": message}.
func (refusal *Refusal) Write(w http.ResponseWriter) {
	writeError(w, refusal.Status, refusal.Message)
}

// writeAcceptError answers a refusal with its own status and any other
// failure as 500.
func writeAcceptError(w http.ResponseWriter, err error) {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		refusal.Write(w)
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

// CurrentLimits is the size limit and inline list in force now (GET /limits).
func (s *Server) CurrentLimits() Limits {
	return s.limits()
}

// Accept checks a new file and keeps it, as POST /files does. contentType is
// the declared Content-Type, parsed here; declaredLength is the content's
// length when the caller knows it, else -1, and a length over the limit is
// refused before a byte is read. A file the checks refuse is a *Refusal
// (400 or 413); any other error is the store failing.
func (s *Server) Accept(content io.Reader, declaredLength int64, contentType string, file NewFile) (*File, error) {
	if err := validateFilename(file.Filename); err != nil {
		return nil, &Refusal{http.StatusBadRequest, err.Error()}
	}
	for _, owner := range []struct{ field, value string }{{"owner_service", file.OwnerService}, {"owner_ref", file.OwnerRef}} {
		if err := validateOwnerLabel(owner.field, owner.value); err != nil {
			return nil, &Refusal{http.StatusBadRequest, err.Error()}
		}
	}
	if file.UploadedByPrincipalID != "" && !principalIDShape.MatchString(file.UploadedByPrincipalID) {
		return nil, &Refusal{http.StatusBadRequest, fmt.Sprintf("uploaded_by_principal_id %q is not a principal-store id (principal_000001)", file.UploadedByPrincipalID)}
	}
	// The declared type is part of the record and decides how the file may be
	// served, so it is not guessed when it is missing.
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, &Refusal{http.StatusBadRequest, "Content-Type must say what the file is (application/octet-stream when nothing better is known): " + err.Error()}
	}
	file.ContentType = mediaType

	limits := s.limits()
	if declaredLength > limits.MaximumFileBytes {
		return nil, &Refusal{http.StatusRequestEntityTooLarge, fmt.Sprintf("the file is %d bytes and the limit is %d (GET /limits)", declaredLength, limits.MaximumFileBytes)}
	}
	stored, err := s.store.Put(content, limits.MaximumFileBytes, file)
	switch {
	case errors.Is(err, ErrFileTooLarge):
		return nil, &Refusal{http.StatusRequestEntityTooLarge, err.Error() + " (GET /limits)"}
	case errors.Is(err, ErrEmptyFile):
		return nil, &Refusal{http.StatusBadRequest, "the file is empty"}
	case err != nil:
		return nil, err
	}
	return stored, nil
}

// ServeContentOf answers r with a live file's bytes, as
// GET /files/{id}/content?inline=<inline> does: 404 for a file this store
// does not hold or has deleted, 400 for an inline that is not true or false,
// and Range and the conditional headers r carries honored.
//
// Every answer is a download unless inline is "true" AND the file's declared
// type is one this store serves for display. Whatever the answer, the browser
// is told not to sniff the type and to run nothing: a person's upload shown
// from the dashboard's origin must never execute as it.
func (s *Server) ServeContentOf(w http.ResponseWriter, r *http.Request, id, inline string) {
	file, err := s.store.Get(id, false)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	served := false
	switch inline {
	case "", "false":
	case "true":
		for _, allowed := range s.limits().InlineContentTypes {
			if allowed == file.ContentType {
				served = true
			}
		}
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("inline must be true or false, got %q", inline))
		return
	}
	blob, err := s.store.OpenContent(file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer blob.Close()

	header := w.Header()
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	header.Set("Cache-Control", "private, no-store")
	header.Set("ETag", `"`+file.SHA256+`"`)
	disposition := "attachment"
	header.Set("Content-Type", "application/octet-stream")
	if served {
		disposition = "inline"
		header.Set("Content-Type", file.ContentType)
	}
	header.Set("Content-Disposition", disposition+"; filename*=UTF-8''"+url.PathEscape(file.Filename))
	http.ServeContent(w, r, "", time.Unix(file.CreatedAt, 0), blob)
}

// OpenLiveContent opens a live file's bytes for reading in this process. A
// file this store does not hold, or has deleted, is ErrNotFound. The caller
// closes the file.
func (s *Server) OpenLiveContent(id string) (*File, *os.File, error) {
	file, err := s.store.Get(id, false)
	if err != nil {
		return nil, nil, err
	}
	blob, err := s.store.OpenContent(file)
	if err != nil {
		return nil, nil, err
	}
	return file, blob, nil
}

// PurgeFile destroys a file's row and, when no other row names them, its
// bytes, as DELETE /files/{id}?hard=true does. An unknown file is ErrNotFound.
func (s *Server) PurgeFile(id string) error {
	return s.store.Purge(id)
}
