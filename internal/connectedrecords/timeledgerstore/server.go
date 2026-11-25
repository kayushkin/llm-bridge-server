package timeledgerstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
)

// RegisterHandlers mounts the API on mux. A front door such as dash adds the
// auth this service has none of, which is why it binds 127.0.0.1.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	h := &handler{s: s}
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /vocabularies", h.vocabularies)
	mux.HandleFunc("GET /kinds", h.listKinds)
	mux.HandleFunc("POST /kinds", h.createKind)
	mux.HandleFunc("GET /kinds/{id}", h.getKind)
	mux.HandleFunc("GET /time-entries", h.listEntries)
	mux.HandleFunc("POST /time-entries", h.createEntry)
	mux.HandleFunc("GET /time-entries/{id}", h.getEntry)
	mux.HandleFunc("PATCH /time-entries/{id}", h.updateEntry)
	mux.HandleFunc("DELETE /time-entries/{id}", h.deleteEntry)
	mux.HandleFunc("GET /time-entries/{id}/revisions", h.revisions)
	mux.HandleFunc("PUT /time-entries/{id}/kinds/{kind_id}", h.assignKind)
	mux.HandleFunc("DELETE /time-entries/{id}/kinds/{kind_id}", h.removeKind)
	mux.HandleFunc("POST /time-entries/{id}/links/{link_type}", h.addLink)
	mux.HandleFunc("DELETE /time-entries/{id}/links/{link_type}", h.removeLink)
	mux.HandleFunc("GET /links/{link_type}", h.linksTo)
	mux.HandleFunc("GET /time-entries/{id}/reviews", h.listReviews)
	mux.HandleFunc("POST /time-entries/{id}/reviews", h.addReview)
	mux.HandleFunc("GET /reviews/{id}", h.getReview)
}

type handler struct{ s *Store }

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write response: %v", err)
	}
}

func respondError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, ErrConflict):
		status = http.StatusConflict
	}
	if status == http.StatusInternalServerError {
		log.Printf("error: %v", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
	return true
}

func decodeStrict(r *http.Request, into any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	return nil
}

func queryInt(r *http.Request, name string) (int64, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return 0, nil
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a whole number", ErrInvalid, name)
	}
	return number, nil
}

func queryOptionalInt(r *http.Request, name string) (*int64, error) {
	if r.URL.Query().Get(name) == "" {
		return nil, nil
	}
	number, err := queryInt(r, name)
	return &number, err
}

func queryActor(r *http.Request) Actor {
	return Actor{By: r.URL.Query().Get("by"), Kind: r.URL.Query().Get("by_kind")}
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	counts, err := h.s.Counts()
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "counts": counts})
}

func (h *handler) vocabularies(w http.ResponseWriter, r *http.Request) {
	linkTypes := []LinkType{}
	for _, name := range LinkTypeNames() {
		linkTypes = append(linkTypes, LinkTypes[name])
	}
	writeJSON(w, http.StatusOK, map[string]any{"link_types": linkTypes, "bears_on": BearsOn, "actor_kinds": ActorKinds, "outcomes": Outcomes})
}

func (h *handler) listKinds(w http.ResponseWriter, r *http.Request) {
	kinds, err := h.s.Kinds()
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, kinds)
}

func (h *handler) createKind(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		By          string `json:"by"`
		ByKind      string `json:"by_kind"`
	}
	if respondError(w, decodeStrict(r, &body)) {
		return
	}
	kind, err := h.s.CreateKind(body.Name, body.Description, Actor{By: body.By, Kind: body.ByKind})
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, kind)
}

func (h *handler) getKind(w http.ResponseWriter, r *http.Request) {
	kind, err := h.s.Kind(r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, kind)
}

func (h *handler) listEntries(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := EntryFilter{Parent: query.Get("parent"), KindID: query.Get("kind_id"), Outcome: query.Get("outcome"), Query: query.Get("q"),
		NeedsReview: query.Get("needs_review") == "true", IncludeDeleted: query.Get("include_deleted") == "true"}
	var err error
	for name, field := range map[string]*int64{"from": &filter.From, "to": &filter.To, "ended_before": &filter.EndedBefore} {
		if *field, err = queryInt(r, name); respondError(w, err) {
			return
		}
	}
	for name, field := range map[string]*int{"limit": &filter.Limit, "offset": &filter.Offset} {
		number, err := queryInt(r, name)
		if respondError(w, err) {
			return
		}
		*field = int(number)
	}
	entries, err := h.s.Entries(filter)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (h *handler) createEntry(w http.ResponseWriter, r *http.Request) {
	var input EntryInput
	if respondError(w, decodeStrict(r, &input)) {
		return
	}
	entry, err := h.s.CreateEntry(input)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

func (h *handler) getEntry(w http.ResponseWriter, r *http.Request) {
	entry, err := h.s.Entry(r.PathValue("id"), true)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *handler) updateEntry(w http.ResponseWriter, r *http.Request) {
	var change EntryChange
	if respondError(w, decodeStrict(r, &change)) {
		return
	}
	entry, err := h.s.UpdateEntry(r.PathValue("id"), change)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *handler) deleteEntry(w http.ResponseWriter, r *http.Request) {
	if respondError(w, h.s.DeleteEntry(r.PathValue("id"), queryActor(r))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) revisions(w http.ResponseWriter, r *http.Request) {
	revisions, err := h.s.Revisions(r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, revisions)
}

func (h *handler) assignKind(w http.ResponseWriter, r *http.Request) {
	var body struct {
		By     string `json:"by"`
		ByKind string `json:"by_kind"`
		Reason string `json:"reason"`
	}
	if respondError(w, decodeStrict(r, &body)) {
		return
	}
	entry, err := h.s.AssignKind(r.PathValue("id"), r.PathValue("kind_id"), Actor{By: body.By, Kind: body.ByKind}, body.Reason)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *handler) removeKind(w http.ResponseWriter, r *http.Request) {
	if respondError(w, h.s.RemoveKind(r.PathValue("id"), r.PathValue("kind_id"), queryActor(r))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) addLink(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LinkInput
		By     string `json:"by"`
		ByKind string `json:"by_kind"`
	}
	if respondError(w, decodeStrict(r, &body)) {
		return
	}
	if body.LinkType != "" && body.LinkType != r.PathValue("link_type") {
		respondError(w, fmt.Errorf("%w: link_type in the body differs from the path", ErrInvalid))
		return
	}
	body.LinkType = r.PathValue("link_type")
	link, err := h.s.AddLink(r.PathValue("id"), body.LinkInput, Actor{By: body.By, Kind: body.ByKind})
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, link)
}

func (h *handler) removeLink(w http.ResponseWriter, r *http.Request) {
	occurrence, err := queryOptionalInt(r, "occurrence_starts_at")
	if respondError(w, err) {
		return
	}
	if respondError(w, h.s.RemoveLink(r.PathValue("id"), r.PathValue("link_type"), r.URL.Query().Get("ref"), occurrence, queryActor(r))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) linksTo(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		respondError(w, fmt.Errorf("%w: ref is required", ErrInvalid))
		return
	}
	occurrence, err := queryOptionalInt(r, "occurrence_starts_at")
	if respondError(w, err) {
		return
	}
	links, err := h.s.LinksTo(r.PathValue("link_type"), ref, occurrence, r.URL.Query().Get("include_removed") == "true")
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, links)
}

func (h *handler) listReviews(w http.ResponseWriter, r *http.Request) {
	reviews, err := h.s.Reviews(r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, reviews)
}

func (h *handler) addReview(w http.ResponseWriter, r *http.Request) {
	var input ReviewInput
	if respondError(w, decodeStrict(r, &input)) {
		return
	}
	review, err := h.s.AddReview(r.PathValue("id"), input)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, review)
}

func (h *handler) getReview(w http.ResponseWriter, r *http.Request) {
	review, err := h.s.Review(r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, review)
}
