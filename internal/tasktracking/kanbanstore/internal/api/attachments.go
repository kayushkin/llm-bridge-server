package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/filestore"
	"github.com/kayushkin/kanban-store/internal/model"
)

// Files on a card.
//
// file-store owns a file and decides nothing about who may read it. This store
// decides: the routes below sit under /api/cards/{id}/, so the gate has already
// held the caller to the card — view to list and download, edit to attach and
// remove — before any of them runs. A file is reached only through the card it
// hangs on, never by its id alone, which is what makes "who may read this
// file?" the same question as "who may view this card?".

// SetFileStore gives the API its file-store client. Without it this store has
// no attachments, and says so.
func (a *API) SetFileStore(files *filestore.Client) { a.files = files }

// requireFileStore answers 503 when no file-store is configured: a card's
// attachments are then unknown, which is not the same as none.
func (a *API) requireFileStore(w http.ResponseWriter) bool {
	if a.files == nil {
		writeError(w, http.StatusServiceUnavailable, "attachments are not configured: set FILE_STORE_URL and FILE_STORE_SERVICE_TOKEN")
		return false
	}
	return true
}

// writeFileStoreFailure relays file-store's refusal as it was sent, and
// reports anything else as the upstream failure it is.
func writeFileStoreFailure(w http.ResponseWriter, err error) {
	var refusal *filestore.RefusalError
	if errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500 && refusal.Status != http.StatusUnauthorized {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(refusal.Status)
		_, _ = w.Write(refusal.Body)
		return
	}
	// file-store's 401 is this store's token being wrong, not the caller's.
	writeError(w, http.StatusBadGateway, "file-store: "+err.Error())
}

func attachmentVisibilityFrom(raw string) (model.NoteVisibility, error) {
	visibility := model.NoteVisibility(raw)
	if visibility != "" && !model.ValidNoteVisibility(visibility) {
		return "", fmt.Errorf("visibility %q is not one of %v (GET /api/note-visibilities)", raw, model.NoteVisibilities)
	}
	return visibility, nil
}

func (a *API) cardAttachments(w http.ResponseWriter, r *http.Request, cardID string) {
	if !a.requireFileStore(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		visibility, err := attachmentVisibilityFrom(r.URL.Query().Get("visibility"))
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		attachments, err := a.store.ListCardAttachments(cardID, visibility)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if len(attachments) > 0 {
			files, err := a.files.FilesOfCard(cardID)
			if err != nil {
				writeFileStoreFailure(w, err)
				return
			}
			for i := range attachments {
				if file, held := files[attachments[i].FileID]; held {
					attachments[i].File = file.Raw
				}
			}
		}
		writeJSON(w, 200, attachments)
	case http.MethodPost:
		a.attachFileToCard(w, r, cardID)
	default:
		writeError(w, 405, "method not allowed")
	}
}

// attachFileToCard is POST /api/cards/{id}/attachments?filename=…[&visibility=…].
// The body is the file's bytes and Content-Type is what they are.
func (a *API) attachFileToCard(w http.ResponseWriter, r *http.Request, cardID string) {
	visibility, err := attachmentVisibilityFrom(r.URL.Query().Get("visibility"))
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if visibility == "" {
		// As for a note: nothing is shown to a requester because a field was
		// left out.
		visibility = model.DefaultNoteVisibility
	}
	placements, err := a.store.ListPlacementsByCard(cardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if len(placements) == 0 {
		writeError(w, 404, "not found")
		return
	}
	uploadedBy := r.Header.Get(PrincipalIDHeader) // empty for an internal service
	file, err := a.files.Upload(r.Body, r.ContentLength, r.Header.Get("Content-Type"), r.URL.Query().Get("filename"), cardID, uploadedBy)
	if err != nil {
		writeFileStoreFailure(w, err)
		return
	}
	attachment := model.CardAttachment{CardID: cardID, FileID: file.ID, Visibility: visibility, AttachedBy: actorFrom(r), File: file.Raw}
	if err := a.store.CreateCardAttachment(&attachment); err != nil {
		// The file is in file-store and nothing here names it. Take it back
		// rather than leave bytes nobody can reach or remove.
		if undoErr := a.files.Delete(file.ID, true); undoErr != nil {
			log.Printf("attachments: %s was uploaded for card %s, could not be recorded (%v) and could not be removed again: %v", file.ID, cardID, err, undoErr)
		}
		writeError(w, 500, err.Error())
		return
	}
	if err := a.recordAttachmentEvent(r, model.EventAttachmentAdded, &attachment, file); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, attachment)
}

func (a *API) recordAttachmentEvent(r *http.Request, kind model.EventKind, attachment *model.CardAttachment, file *filestore.File) error {
	state, err := a.clockStateCarriedForward(attachment.CardID)
	if err != nil {
		return err
	}
	detail, err := json.Marshal(model.AttachmentEventDetail{
		FileID: file.ID, Filename: file.Filename, SizeBytes: file.SizeBytes, ContentType: file.ContentType, Visibility: attachment.Visibility,
	})
	if err != nil {
		return err
	}
	return a.recordEventAndFireMessageTriggers(&model.CardEvent{
		CardID: attachment.CardID, Kind: kind, ClockState: state, Actor: actorFrom(r), Summary: file.Filename, Detail: detail,
	})
}

// cardAttachmentByFile is DELETE /api/cards/{id}/attachments/{file_id}: the
// file comes off the card and is deleted in file-store, reversibly there.
func (a *API) cardAttachmentByFile(w http.ResponseWriter, r *http.Request, cardID, fileID string) {
	if !a.requireFileStore(w) {
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, 405, "method not allowed")
		return
	}
	attachment, err := a.store.GetCardAttachment(cardID, fileID)
	if err != nil {
		mapDBErr(w, err)
		return
	}
	files, err := a.files.FilesOfCard(cardID)
	if err != nil {
		writeFileStoreFailure(w, err)
		return
	}
	// file-store first: if it cannot take the file away, nothing has changed.
	// A file it no longer has is already away.
	if err := a.files.Delete(fileID, false); err != nil && !errors.Is(err, filestore.ErrNotFound) {
		writeFileStoreFailure(w, err)
		return
	}
	if err := a.store.DeleteCardAttachment(cardID, fileID); err != nil {
		mapDBErr(w, err)
		return
	}
	file := files[fileID]
	if file == nil {
		file = &filestore.File{ID: fileID}
	}
	if err := a.recordAttachmentEvent(r, model.EventAttachmentRemoved, attachment, file); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cardAttachmentContent streams a file on this card. file-store's answer goes
// through as it was sent — status, headers and bytes — because the headers are
// what keep somebody's upload from running in the reader's browser.
func (a *API) cardAttachmentContent(w http.ResponseWriter, r *http.Request, cardID, fileID string) {
	if !a.requireFileStore(w) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, 405, "method not allowed")
		return
	}
	// The file must hang on the card in the path, which is the card the gate
	// held the caller to. A file id from another card is not found here.
	if _, err := a.store.GetCardAttachment(cardID, fileID); err != nil {
		mapDBErr(w, err)
		return
	}
	inline := false
	switch raw := r.URL.Query().Get("inline"); raw {
	case "", "false":
	case "true":
		inline = true
	default:
		writeError(w, 400, fmt.Sprintf("inline must be true or false, got %q", raw))
		return
	}
	response, err := a.files.Content(fileID, inline, r.Header)
	if err != nil {
		writeError(w, http.StatusBadGateway, "file-store: "+err.Error())
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		writeError(w, http.StatusBadGateway, "file-store refused this store's service token")
		return
	}
	for name, values := range response.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		log.Printf("attachments: streaming %s of card %s: %v", fileID, cardID, err)
	}
}

// purgeAttachmentsOfCard destroys every file of a card that is about to be
// purged, and answers the status to refuse with when it cannot.
//
// "Every file" is file-store's list for the card, deleted ones included, and
// not this store's rows: a file taken off the card has no row here any more,
// and file-store still holds it so the removal can be undone. Going by the rows
// left those behind for good, owned by a card that no longer existed — found on
// the first live run.
func (a *API) purgeAttachmentsOfCard(cardID string) (int, error) {
	attachments, err := a.store.ListCardAttachments(cardID, "")
	if err != nil {
		return 500, err
	}
	if a.files == nil {
		if len(attachments) > 0 {
			return http.StatusServiceUnavailable, fmt.Errorf("card %s has %d attachments and no file-store is configured to destroy them, so the card is not purged", cardID, len(attachments))
		}
		return 0, nil
	}
	files, err := a.files.EveryFileOfCard(cardID)
	if err != nil {
		return http.StatusBadGateway, fmt.Errorf("file-store could not list the files of card %s, so the card is not purged: %w", cardID, err)
	}
	for fileID := range files {
		if err := a.files.Delete(fileID, true); err != nil && !errors.Is(err, filestore.ErrNotFound) {
			return http.StatusBadGateway, fmt.Errorf("file-store could not destroy %s, so card %s is not purged: %w", fileID, cardID, err)
		}
	}
	for _, attachment := range attachments {
		if err := a.store.DeleteCardAttachment(cardID, attachment.FileID); err != nil && !errors.Is(err, db.ErrNotFound) {
			return 500, err
		}
	}
	return 0, nil
}
