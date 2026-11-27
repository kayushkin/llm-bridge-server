package onlineactivity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// RegisterHandlers mounts the API on mux. Every request must carry the
// ownership.Caller the server verified; one without is 401. Reads and writes
// reach the caller's own records, or every owner's for a caller that sees
// them all; a record out of reach answers 404, as a missing one does.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	h := &handler{s: s}
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /vocabularies", h.vocabularies)
	mux.HandleFunc("GET /stats", h.stats)

	mux.HandleFunc("GET /activities", h.listActivities)
	mux.HandleFunc("GET /activities/{id}", h.getActivity)
	mux.HandleFunc("DELETE /activities/{id}", h.deleteActivity)
	mux.HandleFunc("POST /activities/{id}/restore", h.restoreActivity)
	mux.HandleFunc("POST /activities/{id}/purge", h.purgeActivity)
	mux.HandleFunc("GET /activities/{id}/revisions", h.revisions)
	mux.HandleFunc("GET /attachments/{id}", h.attachment)

	mux.HandleFunc("GET /days/{date}", h.day)

	mux.HandleFunc("GET /imports", h.listImports)
	mux.HandleFunc("POST /imports", h.startImport)
	mux.HandleFunc("GET /imports/{id}", h.getImport)
	mux.HandleFunc("POST /imports/{id}/batches", h.importBatch)
	mux.HandleFunc("POST /imports/{id}/finish", h.finishImport)
}

type handler struct {
	s *Store
}

// respondRefusal answers a request the ownership rules refuse, in the
// store's own error shape.
func respondRefusal(w http.ResponseWriter, refusal *ownership.RequestError) bool {
	if refusal == nil {
		return false
	}
	writeErr(w, refusal.Status, refusal.Message)
	return true
}

// readScope is the request's scope, or false once a refusal is answered.
func readScope(w http.ResponseWriter, r *http.Request) (ownership.Scope, bool) {
	scope, refusal := ownership.ReadScope(r)
	if respondRefusal(w, refusal) {
		return ownership.Scope{}, false
	}
	return scope, true
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	if !acceptOnly(w, r, ownership.OwnerParameter) {
		return
	}
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	counts, err := h.s.Counts(r.Context(), scope)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, Health{Status: "ok", Counts: counts})
}

func (h *handler) vocabularies(w http.ResponseWriter, r *http.Request) {
	if _, refusal := ownership.CallerOfRequest(r); respondRefusal(w, refusal) {
		return
	}
	writeJSON(w, http.StatusOK, Vocabularies{Actions: Actions, ActionPhrases: ActionPhrases, Sources: Sources,
		ImportKinds: ImportKinds, RevisionReasons: RevisionReasons})
}

func (h *handler) stats(w http.ResponseWriter, r *http.Request) {
	if !acceptOnly(w, r, ownership.OwnerParameter) {
		return
	}
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	stats, err := h.s.Stats(r.Context(), scope)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// ---- query parameters ----

// acceptOnly refuses a query parameter the route does not read, so a
// misspelt filter is a 400 and not a page that silently ignored it.
func acceptOnly(w http.ResponseWriter, r *http.Request, accepted ...string) bool {
	for name := range r.URL.Query() {
		if !knownWord(accepted, name) {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown query parameter %q; this route reads %s", name, strings.Join(accepted, ", ")))
			return false
		}
	}
	return true
}

func queryInteger(w http.ResponseWriter, r *http.Request, name string, fallback, max int64) (int64, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 || (max > 0 && value > max) {
		limit := ""
		if max > 0 {
			limit = fmt.Sprintf(" up to %d", max)
		}
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s must be a non-negative integer%s, got %q", name, limit, raw))
		return 0, false
	}
	return value, true
}

func queryBoolean(w http.ResponseWriter, r *http.Request, name string) (bool, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	switch raw {
	case "", "false":
		return false, true
	case "true":
		return true, true
	}
	writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s must be true or false, got %q", name, raw))
	return false, false
}

// ---- activities ----

const (
	defaultListLimit = 100
	maxListLimit     = 1000
)

func (h *handler) listActivities(w http.ResponseWriter, r *http.Request) {
	if !acceptOnly(w, r, "from", "to", "product", "action", "source", "q", "include_deleted", "order", "limit", "offset", ownership.OwnerParameter) {
		return
	}
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	filter := ActivityFilter{Product: query.Get("product"), Action: query.Get("action"), Source: query.Get("source"), Query: query.Get("q")}
	var value int64
	if filter.From, ok = queryInteger(w, r, "from", 0, 0); !ok {
		return
	}
	if filter.To, ok = queryInteger(w, r, "to", 0, 0); !ok {
		return
	}
	if value, ok = queryInteger(w, r, "limit", defaultListLimit, maxListLimit); !ok {
		return
	}
	filter.Limit = int(value)
	if value, ok = queryInteger(w, r, "offset", 0, 0); !ok {
		return
	}
	filter.Offset = int(value)
	if filter.IncludeDeleted, ok = queryBoolean(w, r, "include_deleted"); !ok {
		return
	}
	switch query.Get("order") {
	case "", "asc":
	case "desc":
		filter.Descending = true
	default:
		writeErr(w, http.StatusBadRequest, "order must be asc or desc")
		return
	}
	page, err := h.s.ListActivities(r.Context(), scope, filter)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *handler) getActivity(w http.ResponseWriter, r *http.Request) {
	if !acceptOnly(w, r, "include_deleted", ownership.OwnerParameter) {
		return
	}
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	includeDeleted, ok := queryBoolean(w, r, "include_deleted")
	if !ok {
		return
	}
	activity, err := h.s.GetActivity(r.Context(), scope, r.PathValue("id"), includeDeleted)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, activity)
}

func (h *handler) deleteActivity(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	if respondError(w, h.s.DeleteActivity(r.Context(), scope, r.PathValue("id"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) restoreActivity(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	activity, err := h.s.RestoreActivity(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, activity)
}

func (h *handler) purgeActivity(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	activity, err := h.s.PurgeActivity(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, activity)
}

func (h *handler) revisions(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	revisions, err := h.s.Revisions(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, revisions)
}

func (h *handler) attachment(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	attachment, content, err := h.s.OpenAttachment(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	defer content.Close()
	seeker, seekable := content.(io.ReadSeeker)
	if !seekable {
		// Range requests need a seeker; an attachment is at most
		// maxAttachmentBytes, so one that cannot seek is read whole.
		whole, err := io.ReadAll(io.LimitReader(content, maxAttachmentBytes+1))
		if err != nil {
			respondError(w, fmt.Errorf("attachment %s: reading its file from file-store: %w", attachment.ID, err))
			return
		}
		seeker = bytes.NewReader(whole)
	}
	w.Header().Set("Content-Type", attachment.ContentType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, "", time.Unix(attachment.CreatedAt, 0), seeker)
}

// ---- days ----

const defaultDayFirst = 5

func (h *handler) day(w http.ResponseWriter, r *http.Request) {
	if !acceptOnly(w, r, "tz", "first", ownership.OwnerParameter) {
		return
	}
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	first, ok := queryInteger(w, r, "first", defaultDayFirst, MaxDayFirst)
	if !ok {
		return
	}
	day, err := h.s.Day(r.Context(), scope, r.PathValue("date"), r.URL.Query().Get("tz"), int(first))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, day)
}

// ---- imports ----

func (h *handler) listImports(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	imports, err := h.s.ListImports(r.Context(), scope)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, imports)
}

func (h *handler) getImport(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	imported, err := h.s.GetImport(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, imported)
}

func (h *handler) startImport(w http.ResponseWriter, r *http.Request) {
	owner, refusal := ownership.OwnerOfNewRecord(r)
	if respondRefusal(w, refusal) {
		return
	}
	var start ImportStart
	if !decodeBody(w, r, 1<<16, &start) {
		return
	}
	started, err := h.s.StartImport(r.Context(), owner, start)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, started)
}

// maxBatchBytes bounds one batch: 2000 records and their attachments.
const maxBatchBytes = 256 << 20

func (h *handler) importBatch(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var batch ImportBatch
	if !decodeBody(w, r, maxBatchBytes, &batch) {
		return
	}
	counts, err := h.s.ImportBatch(r.Context(), scope, r.PathValue("id"), batch)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

func (h *handler) finishImport(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	imported, err := h.s.FinishImport(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, imported)
}

// ---- plumbing ----

func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("activity-store: encode response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func respondError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrGone):
		writeErr(w, http.StatusGone, err.Error())
	default:
		log.Printf("activity-store: %v", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
	return true
}
