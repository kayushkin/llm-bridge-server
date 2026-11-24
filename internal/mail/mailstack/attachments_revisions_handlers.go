package mailstack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/filestore"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/localmail"
)

// ─── Attachments and revisions of imported mail ────────────────────────
//
// An imported attachment's bytes live in file-store, which judges no caller.
// mailstack keeps which file hangs on which revision of which message, checks
// the caller on its own routes, and only then fetches the bytes with the
// service token. No route here takes a file-store id, and none returns one.

// uploadChangedAttachments puts in file-store the attachments of every message
// the import will create or change, and sets each one's FileID. It returns the
// ids uploaded, so a failed import can purge them, or the status to answer
// with. An unchanged message's attachments are not uploaded again.
func (s *Server) uploadChangedAttachments(ctx context.Context, store *localmail.Store, accountID string, msgs []localmail.ImportedMessage) ([]string, int, error) {
	plan, err := store.PlanImport(ctx, accountID, msgs)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	var uploaded []string
	fail := func(status int, err error) ([]string, int, error) {
		if purgeErr := s.purgeAttachmentFiles(ctx, uploaded); purgeErr != nil {
			log.Printf("mailstack: an import into %s stopped part way and %d uploaded attachment file(s) could not all be purged: %v",
				accountID, len(uploaded), purgeErr)
		}
		return nil, status, err
	}
	for i := range msgs {
		m := &msgs[i]
		if plan[i] == localmail.ImportUnchanged || len(m.Attachments) == 0 {
			continue
		}
		if s.files == nil {
			return fail(http.StatusServiceUnavailable, fmt.Errorf(
				"message %d (%s) has attachments and no file-store is configured (FILE_STORE_URL); nothing was imported", i, m.ProviderMessageID))
		}
		locator := accountID + ":" + localmail.ImportedID(accountID, m.ProviderMessageID)
		for j := range m.Attachments {
			a := &m.Attachments[j]
			file, err := s.files.Upload(a.Content, a.ContentType, a.Filename, locator)
			if err != nil {
				status := http.StatusBadGateway
				var refusal *filestore.RefusalError
				if errors.As(err, &refusal) && (refusal.Status == http.StatusBadRequest || refusal.Status == http.StatusRequestEntityTooLarge) {
					status = refusal.Status
				}
				return fail(status, fmt.Errorf("message %d (%s) attachment %d (%s): %w; nothing was imported",
					i, m.ProviderMessageID, j, a.Filename, err))
			}
			a.FileID = file.ID
			uploaded = append(uploaded, file.ID)
		}
	}
	return uploaded, 0, nil
}

// purgeAttachmentFiles destroys attachment files this service put in
// file-store. A file file-store no longer has counts as purged.
func (s *Server) purgeAttachmentFiles(ctx context.Context, fileIDs []string) error {
	if len(fileIDs) == 0 {
		return nil
	}
	if s.files == nil {
		return fmt.Errorf("%d attachment file(s) to purge and no file-store is configured (FILE_STORE_URL)", len(fileIDs))
	}
	var errs []error
	for _, id := range fileIDs {
		if err := s.files.Purge(id); err != nil && !errors.Is(err, filestore.ErrNotFound) {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// relayedFileHeaders are file-store's answer headers a download passes on
// unchanged. The safety ones — nosniff, the sandboxing CSP, and the
// Content-Disposition that makes a browser save rather than render — matter
// most: file-store's contract asks every relay to keep them.
var relayedFileHeaders = []string{
	"Content-Type", "Content-Length", "Content-Disposition", "Content-Range", "Accept-Ranges",
	"ETag", "Last-Modified", "Cache-Control", "X-Content-Type-Options", "Content-Security-Policy",
}

// serveLocalAttachment answers one attachment of one revision — 0 for the
// current — of a local account's message, after this service has found that
// the attachment belongs to that message.
func (s *Server) serveLocalAttachment(w http.ResponseWriter, r *http.Request, accountID, id string, revision int64, attachmentID string) {
	store, status, err := s.localAccountStore(accountID)
	if err != nil {
		writeError(w, status, err)
		return
	}
	started := time.Now()
	attachment, fileID, err := store.AttachmentFile(r.Context(), accountID, id, revision, attachmentID)
	setStoreTiming(w, started)
	switch {
	case errors.Is(err, localmail.ErrNotFound):
		writeCodedError(w, http.StatusNotFound, errorCodeMessageNotFound, err)
		return
	case errors.Is(err, localmail.ErrRevisionNotFound):
		writeCodedError(w, http.StatusNotFound, errorCodeRevisionNotFound, err)
		return
	case errors.Is(err, localmail.ErrAttachmentNotFound):
		writeCodedError(w, http.StatusNotFound, errorCodeAttachmentNotFound, err)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.files == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("no file-store is configured (FILE_STORE_URL), so no attachment can be read"))
		return
	}
	response, err := s.files.Content(fileID, r.URL.Query().Get("inline") == "true", r.Header)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("read attachment %s from file-store: %w", attachment.ID, err))
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		// mailstack names this file and file-store does not have it: data was
		// lost, which no caller should mistake for "no such attachment".
		log.Printf("mailstack: attachment %s of %s:%s names file-store file %s, which file-store does not have", attachment.ID, accountID, id, fileID)
		writeError(w, http.StatusBadGateway, fmt.Errorf("file-store has lost the bytes of attachment %s", attachment.ID))
		return
	}
	for _, name := range relayedFileHeaders {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		log.Printf("mailstack: writing attachment %s of %s:%s: %v", attachment.ID, accountID, id, err)
	}
}

// revisionOf reads {revision}, answering 400 itself unless it is a whole
// number of at least one.
func revisionOf(w http.ResponseWriter, r *http.Request) (int64, bool) {
	revision, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
	if err != nil || revision < 1 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("revision must be a whole number from 1, got %q", r.PathValue("revision")))
		return 0, false
	}
	return revision, true
}

func writeStoredMessageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, localmail.ErrNotFound):
		writeCodedError(w, http.StatusNotFound, errorCodeMessageNotFound, err)
	case errors.Is(err, localmail.ErrRevisionNotFound):
		writeCodedError(w, http.StatusNotFound, errorCodeRevisionNotFound, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

// handleListRevisions lists a message's history, newest first.
func (s *Server) handleListRevisions(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account")
	store, status, err := s.localAccountStore(accountID)
	if err != nil {
		writeError(w, status, err)
		return
	}
	started := time.Now()
	revisions, err := store.Revisions(r.Context(), accountID, r.PathValue("id"))
	setStoreTiming(w, started)
	if err != nil {
		writeStoredMessageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": revisions})
}

// handleGetRevisionMetadata reads one revision's metadata. A replaced revision
// never changes, so its ETag names only the revision; the current one's
// follows the mailbox state too, as GET .../metadata's does.
func (s *Server) handleGetRevisionMetadata(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account")
	store, status, err := s.localAccountStore(accountID)
	if err != nil {
		writeError(w, status, err)
		return
	}
	revision, ok := revisionOf(w, r)
	if !ok {
		return
	}
	started := time.Now()
	stored, err := store.Revision(r.Context(), accountID, r.PathValue("id"), revision)
	setStoreTiming(w, started)
	if err != nil {
		writeStoredMessageError(w, err)
		return
	}
	etag := stored.MetadataETag()
	if stored.ReplacedAt != nil {
		etag = fmt.Sprintf(`"%s.r%d.metadata"`, stored.ID, stored.Revision)
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if ifNoneMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

// handleGetRevisionBody serves one body of one revision, exactly as
// GET .../body serves the current one.
func (s *Server) handleGetRevisionBody(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account")
	store, status, err := s.localAccountStore(accountID)
	if err != nil {
		writeError(w, status, err)
		return
	}
	revision, ok := revisionOf(w, r)
	if !ok {
		return
	}
	format, ok := bodyFormatOf(w, r)
	if !ok {
		return
	}
	started := time.Now()
	body, stored, err := store.RevisionBody(r.Context(), accountID, r.PathValue("id"), revision, format)
	setStoreTiming(w, started)
	writeBody(w, r, format, body, stored, err)
}

func (s *Server) handleGetRevisionAttachment(w http.ResponseWriter, r *http.Request) {
	revision, ok := revisionOf(w, r)
	if !ok {
		return
	}
	s.serveLocalAttachment(w, r, r.URL.Query().Get("account"), r.PathValue("id"), revision, r.PathValue("attachment_id"))
}
