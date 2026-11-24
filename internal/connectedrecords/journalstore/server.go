package journalstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// RegisterHandlers mounts the API on mux. Routes are rooted at /; dash adds
// its own prefix and the auth this service has none of. That is why the
// service binds 127.0.0.1: with no auth, the front door has to be the only
// door.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	h := &handler{s: s}
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /entry-kinds", h.entryKinds)
	mux.HandleFunc("GET /visibilities", h.visibilities)

	mux.HandleFunc("GET /entries", h.listEntries)
	mux.HandleFunc("POST /entries", h.createEntry)
	mux.HandleFunc("GET /entries/{id}", h.getEntry)
	mux.HandleFunc("PATCH /entries/{id}", h.patchEntry)
	mux.HandleFunc("DELETE /entries/{id}", h.deleteEntry)
	mux.HandleFunc("POST /entries/{id}/restore", h.restoreEntry)
	mux.HandleFunc("GET /entries/{id}/revisions", h.listRevisions)
	mux.HandleFunc("POST /entries/{id}/promote-todo", h.promoteTodo)

	mux.HandleFunc("GET /mentions", h.backlinks)
	mux.HandleFunc("GET /published", h.listPublished)
	mux.HandleFunc("GET /published/{slug}", h.getPublished)
	mux.HandleFunc("POST /imports", h.importEntries)

	mux.HandleFunc("GET /time-precisions", h.timePrecisions)
	mux.HandleFunc("GET /timelines/{id}/items", h.listTimelineItems)
	mux.HandleFunc("POST /timelines/{id}/items", h.createTimelineItems)
	mux.HandleFunc("GET /timeline-items", h.overlayTimelineItems)
	mux.HandleFunc("GET /timeline-items/{id}", h.getTimelineItem)
	mux.HandleFunc("PATCH /timeline-items/{id}", h.patchTimelineItem)
	mux.HandleFunc("DELETE /timeline-items/{id}", h.deleteTimelineItem)
	mux.HandleFunc("POST /timeline-items/{id}/restore", h.restoreTimelineItem)
	mux.HandleFunc("GET /timeline-items/{id}/revisions", h.listTimelineItemRevisions)
}

type handler struct {
	s *Store
}

// patchableFields is every key PATCH /entries/{id} accepts, matching the json
// tags on EntryPatch.
var patchableFields = map[string]bool{
	"kind": true, "title": true, "body": true, "written_at": true,
	"visibility": true, "slug": true, "tags": true, "about_ref": true,
}

// unpatchableFields maps a key PATCH refuses to the explanation it answers with.
var unpatchableFields = map[string]string{
	"id":           "id is assigned by this store and never changes; other stores join on it.",
	"published_at": "published_at is set the first time the entry becomes published; send visibility instead.",
	"deleted_at":   "use DELETE /entries/{id} or POST /entries/{id}/restore, so removal is always an explicit act.",
	"created_at":   "created_at is set by this store.",
	"updated_at":   "updated_at is set by this store.",
	"source":       "source is written by POST /imports only.",
	"source_ref":   "source_ref is written by POST /imports only.",
	"source_hash":  "source_hash is written by POST /imports only.",
	"mentions":     "mentions are read from the body; edit the ref: links in body instead.",
}

// patchableTimelineItemFields is every key PATCH /timeline-items/{id}
// accepts, matching the json tags on TimelineItemPatch.
var patchableTimelineItemFields = map[string]bool{
	"occurred_at": true, "ends_at": true, "precision": true, "title": true, "body": true, "sources": true,
}

var unpatchableTimelineItemFields = map[string]string{
	"id":          "id is assigned by this store and never changes.",
	"timeline_id": "an item stays on its timeline; delete it and add it to the other one.",
	"deleted_at":  "use DELETE /timeline-items/{id} or POST /timeline-items/{id}/restore.",
	"created_at":  "created_at is set by this store.",
	"updated_at":  "updated_at is set by this store.",
}

func sortedFields(fields map[string]bool) []string {
	out := make([]string, 0, len(fields))
	for field := range fields {
		out = append(out, field)
	}
	sort.Strings(out)
	return out
}

// decodePatch decodes a PATCH body into target after checking every key.
// It goes through a map first, because that is the only way to tell a key
// that is absent from one that is misspelt: {"bdy":"…"} must be a 400, not a
// 200 that changed nothing.
func decodePatch(w http.ResponseWriter, r *http.Request, accepted map[string]bool, refused map[string]string, target any) bool {
	var raw map[string]json.RawMessage
	if !decode(w, r, &raw) {
		return false
	}
	for field := range raw {
		if explanation, isRefused := refused[field]; isRefused {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%q cannot be patched: %s", field, explanation))
			return false
		}
		if !accepted[field] {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown field %q: PATCH accepts %s", field, strings.Join(sortedFields(accepted), ", ")))
			return false
		}
		if string(raw[field]) == "null" {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%q is null; send a value, or leave the field out to keep it", field))
			return false
		}
	}
	body, err := json.Marshal(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	if err := json.Unmarshal(body, target); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	counts, err := h.s.Counts()
	if respondStoreError(w, err) {
		return
	}
	registry := h.s.Registry().Status()
	status := "ok"
	if registry.Error != "" {
		status = "degraded"
	}
	writeJSON(w, http.StatusOK, Health{Status: status, Counts: counts, EntityRegistry: registry})
}

func (h *handler) entryKinds(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, EntryKinds)
}

func (h *handler) visibilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Visibilities)
}

// integerParameter reads an optional integer query parameter. Present and not
// an integer is a 400: a caller who sent to=2026-09-11 must not be answered as
// if they had sent nothing.
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

func pageFrom(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	limit, ok := integerParameter(w, r, "limit")
	if !ok {
		return 0, 0, false
	}
	offset, ok := integerParameter(w, r, "offset")
	return int(limit), int(offset), ok
}

func (h *handler) listEntries(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := Filter{
		Query:          query.Get("q"),
		Kind:           query.Get("kind"),
		ExcludeKinds:   query["exclude_kind"],
		Tag:            query.Get("tag"),
		Visibility:     query.Get("visibility"),
		AboutRef:       query.Get("about_ref"),
		Source:         query.Get("source"),
		IncludeDeleted: isTrue(query.Get("include_deleted")),
	}
	var ok bool
	if filter.WrittenFrom, ok = integerParameter(w, r, "from"); !ok {
		return
	}
	if filter.WrittenTo, ok = integerParameter(w, r, "to"); !ok {
		return
	}
	if filter.Limit, filter.Offset, ok = pageFrom(w, r); !ok {
		return
	}
	entries, err := h.s.List(filter)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (h *handler) createEntry(w http.ResponseWriter, r *http.Request) {
	var input EntryInput
	if !decode(w, r, &input) {
		return
	}
	created, err := h.s.Create(input)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *handler) getEntry(w http.ResponseWriter, r *http.Request) {
	entry, err := h.s.Get(r.PathValue("id"), isTrue(r.URL.Query().Get("include_deleted")))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *handler) patchEntry(w http.ResponseWriter, r *http.Request) {
	var patch EntryPatch
	if !decodePatch(w, r, patchableFields, unpatchableFields, &patch) {
		return
	}
	updated, err := h.s.Patch(r.PathValue("id"), patch)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *handler) deleteEntry(w http.ResponseWriter, r *http.Request) {
	if respondStoreError(w, h.s.Delete(r.PathValue("id"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) restoreEntry(w http.ResponseWriter, r *http.Request) {
	entry, err := h.s.Restore(r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *handler) listRevisions(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := pageFrom(w, r)
	if !ok {
		return
	}
	revisions, err := h.s.ListRevisions(r.PathValue("id"), limit, offset)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, revisions)
}

func (h *handler) promoteTodo(w http.ResponseWriter, r *http.Request) {
	var request PromoteTodoRequest
	if !decode(w, r, &request) {
		return
	}
	result, err := h.s.PromoteTodo(r.PathValue("id"), request.Line)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (h *handler) backlinks(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		writeErr(w, http.StatusBadRequest, "ref is required: the id whose mentions to list, e.g. ?ref=person_000012")
		return
	}
	limit, offset, ok := pageFrom(w, r)
	if !ok {
		return
	}
	backlinks, err := h.s.Backlinks(ref, limit, offset)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, backlinks)
}

func (h *handler) listPublished(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := pageFrom(w, r)
	if !ok {
		return
	}
	entries, err := h.s.ListPublished(r.URL.Query().Get("tag"), limit, offset)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (h *handler) getPublished(w http.ResponseWriter, r *http.Request) {
	entry, err := h.s.GetPublished(r.PathValue("slug"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *handler) importEntries(w http.ResponseWriter, r *http.Request) {
	var request ImportRequest
	if !decode(w, r, &request) {
		return
	}
	result, err := h.s.Import(request, isTrue(r.URL.Query().Get("dry_run")))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// decode reads a JSON body, refusing unknown fields so a misspelt key is a
// 400 rather than a write that silently drops it.
func (h *handler) timePrecisions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, TimePrecisions)
}

func (h *handler) listTimelineItems(w http.ResponseWriter, r *http.Request) {
	items, err := h.s.ListTimelineItems(r.PathValue("id"), isTrue(r.URL.Query().Get("include_deleted")))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *handler) createTimelineItems(w http.ResponseWriter, r *http.Request) {
	var request TimelineItemsRequest
	if !decode(w, r, &request) {
		return
	}
	items, err := h.s.CreateTimelineItems(r.PathValue("id"), request.Items)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, TimelineItemsResult{Items: items})
}

// overlayTimelineItems answers GET /timeline-items?timeline=<id>&timeline=<id>:
// the items of every named timeline, merged in time order.
func (h *handler) overlayTimelineItems(w http.ResponseWriter, r *http.Request) {
	timelines := r.URL.Query()["timeline"]
	if len(timelines) == 0 {
		writeErr(w, http.StatusBadRequest, "name at least one timeline: ?timeline=entry_000001&timeline=entry_000002")
		return
	}
	from, ok := integerParameter(w, r, "from")
	if !ok {
		return
	}
	to, ok := integerParameter(w, r, "to")
	if !ok {
		return
	}
	limit, offset, ok := pageFrom(w, r)
	if !ok {
		return
	}
	items, err := h.s.OverlayTimelineItems(timelines, from, to, limit, offset)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *handler) getTimelineItem(w http.ResponseWriter, r *http.Request) {
	item, err := h.s.GetTimelineItem(r.PathValue("id"), isTrue(r.URL.Query().Get("include_deleted")))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h *handler) patchTimelineItem(w http.ResponseWriter, r *http.Request) {
	var patch TimelineItemPatch
	if !decodePatch(w, r, patchableTimelineItemFields, unpatchableTimelineItemFields, &patch) {
		return
	}
	item, err := h.s.PatchTimelineItem(r.PathValue("id"), patch)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h *handler) deleteTimelineItem(w http.ResponseWriter, r *http.Request) {
	if respondStoreError(w, h.s.DeleteTimelineItem(r.PathValue("id"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) restoreTimelineItem(w http.ResponseWriter, r *http.Request) {
	item, err := h.s.RestoreTimelineItem(r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h *handler) listTimelineItemRevisions(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := pageFrom(w, r)
	if !ok {
		return
	}
	revisions, err := h.s.ListTimelineItemRevisions(r.PathValue("id"), limit, offset)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, revisions)
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("journal-store: encode response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func respondStoreError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrInvalidEntry):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrUpstream):
		log.Printf("journal-store: %v", err)
		writeErr(w, http.StatusBadGateway, err.Error())
	default:
		log.Printf("journal-store: %v", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

func isTrue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
