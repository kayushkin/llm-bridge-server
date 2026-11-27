package photos

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// RegisterHandlers mounts the API on mux. Routes are rooted at /; the server
// mounts them under its own prefix, and puts the verified caller on each
// request's context (ownership.ContextWithCaller) before it gets here. A
// request with no caller is 401.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	h := &handler{s: s}
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /date-sources", vocabulary(DateSources))
	mux.HandleFunc("GET /tags", h.tagCounts)
	mux.HandleFunc("GET /photos", h.list)
	mux.HandleFunc("GET /sightings", h.sightings)
	mux.HandleFunc("GET /taken-activity", h.takenActivity)
	mux.HandleFunc("POST /imports", h.importPhotos)
	mux.HandleFunc("GET /photos/{id}", h.get)
	mux.HandleFunc("PATCH /photos/{id}", h.patch)
	mux.HandleFunc("DELETE /photos/{id}", h.remove)
	mux.HandleFunc("POST /photos/{id}/restore", h.restore)
	mux.HandleFunc("POST /photos/{id}/purge", h.purge)
	mux.HandleFunc("GET /photos/{id}/revisions", h.revisions)
	mux.HandleFunc("GET /photos/{id}/thumbnail", h.thumbnail)
	registerFaceHandlers(mux, h)
	registerFaceGroupHandlers(mux, h)
}

type handler struct {
	s *Store
}

// vocabulary serves a fixed list to any caller the server verified.
func vocabulary(list any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, refusal := ownership.CallerOfRequest(r); refusal != nil {
			writeErr(w, refusal.Status, refusal.Message)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}

// readScope is whose records the request reaches, or false after answering
// its refusal.
func readScope(w http.ResponseWriter, r *http.Request) (ownership.Scope, bool) {
	scope, refusal := ownership.ReadScope(r)
	if refusal != nil {
		writeErr(w, refusal.Status, refusal.Message)
		return ownership.Scope{}, false
	}
	return scope, true
}

// ownerOfNewRecord is who owns what the request creates, or false after
// answering its refusal.
func ownerOfNewRecord(w http.ResponseWriter, r *http.Request) (string, bool) {
	owner, refusal := ownership.OwnerOfNewRecord(r)
	if refusal != nil {
		writeErr(w, refusal.Status, refusal.Message)
		return "", false
	}
	return owner, true
}

// patchableFields is every key PATCH /photos/{id} accepts.
var patchableFields = map[string]bool{"description": true, "described_by": true, "tags": true, "taken_at": true, "date_source": true}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
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

func (h *handler) tagCounts(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	counts, err := h.s.TagCounts(r.Context(), scope)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

func integerParameter(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return 0, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s must be a non-negative integer (epoch seconds for from and to), got %q", name, raw))
		return 0, false
	}
	return value, true
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	filter := Filter{Folder: query.Get("folder"), Query: query.Get("q"), Tag: query.Get("tag"), Person: query.Get("person"), Described: query.Get("described"),
		HasGPS: query.Get("has_gps") == "true", Sha256: query.Get("sha256"), IncludeDeleted: query.Get("include_deleted") == "true",
		ExcludeTags: query["exclude_tag"]}
	if filter.From, ok = integerParameter(w, r, "from"); !ok {
		return
	}
	if filter.To, ok = integerParameter(w, r, "to"); !ok {
		return
	}
	limit, ok := integerParameter(w, r, "limit")
	if !ok {
		return
	}
	offset, ok := integerParameter(w, r, "offset")
	if !ok {
		return
	}
	filter.Limit, filter.Offset = int(limit), int(offset)
	photos, err := h.s.List(r.Context(), scope, filter)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, photos)
}

func (h *handler) importPhotos(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerOfNewRecord(w, r)
	if !ok {
		return
	}
	var request ImportRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	result, err := h.s.Import(r.Context(), owner, request)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	photo, err := h.s.Get(r.Context(), scope, r.PathValue("id"), r.URL.Query().Get("include_deleted") == "true")
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, photo)
}

func (h *handler) patch(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	// Decoded into a map first: a misspelt key must be a 400, not a 200 that
	// changed nothing.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&raw); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	accepted := make([]string, 0, len(patchableFields))
	for field := range patchableFields {
		accepted = append(accepted, field)
	}
	sort.Strings(accepted)
	for field, value := range raw {
		if !patchableFields[field] {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("field %q cannot be patched: PATCH accepts %s; an import sets the rest", field, strings.Join(accepted, ", ")))
			return
		}
		if string(value) == "null" {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%q is null; send a value, or leave the field out to keep it", field))
			return
		}
	}
	body, err := json.Marshal(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	var patch PhotoPatch
	if err := json.Unmarshal(body, &patch); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	photo, err := h.s.Patch(r.Context(), scope, r.PathValue("id"), patch)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, photo)
}

func (h *handler) remove(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	if respondError(w, h.s.Delete(r.Context(), scope, r.PathValue("id"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) purge(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	if respondError(w, h.s.Purge(r.Context(), scope, r.PathValue("id"))) {
		return
	}
	photo, err := h.s.Get(r.Context(), scope, r.PathValue("id"), true)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, photo)
}

func (h *handler) restore(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	photo, err := h.s.Restore(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, photo)
}

func (h *handler) revisions(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	limit, ok := integerParameter(w, r, "limit")
	if !ok {
		return
	}
	offset, ok := integerParameter(w, r, "offset")
	if !ok {
		return
	}
	revisions, err := h.s.Revisions(r.Context(), scope, r.PathValue("id"), int(limit), int(offset))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, revisions)
}

// thumbnail serves the JPEG. A deleted photo's thumbnail is still served, so a
// citation of it keeps drawing; the photo's own record says it is deleted.
func (h *handler) thumbnail(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	content, err := h.s.OpenThumbnail(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	defer content.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	if _, err := io.Copy(w, content); err != nil {
		log.Printf("photo-store: sending the thumbnail of %s: %v", r.PathValue("id"), err)
	}
}

// sightings answers GET /sightings?source_machine=&limit=&offset=: every
// place a live photo in scope was found.
func (h *handler) sightings(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	limit, ok := integerParameter(w, r, "limit")
	if !ok {
		return
	}
	offset, ok := integerParameter(w, r, "offset")
	if !ok {
		return
	}
	sightings, err := h.s.Sightings(r.Context(), scope, r.URL.Query().Get("source_machine"), int(limit), int(offset))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, sightings)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("photo-store: encode response: %v", err)
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
	case errors.Is(err, ErrConflict), isUniqueViolation(err):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrGone):
		writeErr(w, http.StatusGone, err.Error())
	case errors.Is(err, ErrUpstream):
		writeErr(w, http.StatusBadGateway, err.Error())
	default:
		log.Printf("photo-store: %v", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
	return true
}
