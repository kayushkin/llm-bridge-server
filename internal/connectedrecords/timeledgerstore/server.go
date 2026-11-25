package timeledgerstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// RegisterHandlers mounts the API on mux. Every request must carry the
// ownership.Caller the server verified; one without is 401.
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
		log.Printf("time-ledger-store: write response: %v", err)
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
		log.Printf("time-ledger-store: error: %v", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
	return true
}

// respondRefusal answers a request the ownership rules refuse, in the
// store's own error shape.
func respondRefusal(w http.ResponseWriter, refusal *ownership.RequestError) bool {
	if refusal == nil {
		return false
	}
	writeJSON(w, refusal.Status, map[string]string{"error": refusal.Message})
	return true
}

// readScope is the request's scope, or false once a refusal is answered.
// Writes to a record that exists take it too: a caller changes only what it
// may read.
func readScope(w http.ResponseWriter, r *http.Request) (ownership.Scope, bool) {
	scope, refusal := ownership.ReadScope(r)
	if respondRefusal(w, refusal) {
		return ownership.Scope{}, false
	}
	return scope, true
}

// ownerOfNewRecord is the owner a record the request makes gets, or false
// once a refusal is answered.
func ownerOfNewRecord(w http.ResponseWriter, r *http.Request) (string, bool) {
	owner, refusal := ownership.OwnerOfNewRecord(r)
	if respondRefusal(w, refusal) {
		return "", false
	}
	return owner, true
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
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	counts, err := h.s.Counts(r.Context(), scope)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "counts": counts})
}

func (h *handler) vocabularies(w http.ResponseWriter, r *http.Request) {
	if _, refusal := ownership.CallerOfRequest(r); respondRefusal(w, refusal) {
		return
	}
	linkTypes := []LinkType{}
	for _, name := range LinkTypeNames() {
		linkTypes = append(linkTypes, LinkTypes[name])
	}
	writeJSON(w, http.StatusOK, map[string]any{"link_types": linkTypes, "bears_on": BearsOn, "actor_kinds": ActorKinds, "outcomes": Outcomes})
}

func (h *handler) listKinds(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	kinds, err := h.s.Kinds(r.Context(), scope)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, kinds)
}

func (h *handler) createKind(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerOfNewRecord(w, r)
	if !ok {
		return
	}
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		By          string `json:"by"`
		ByKind      string `json:"by_kind"`
	}
	if respondError(w, decodeStrict(r, &body)) {
		return
	}
	kind, err := h.s.CreateKind(r.Context(), owner, body.Name, body.Description, Actor{By: body.By, Kind: body.ByKind})
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, kind)
}

func (h *handler) getKind(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	kind, err := h.s.Kind(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, kind)
}

func (h *handler) listEntries(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
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
	entries, err := h.s.Entries(r.Context(), scope, filter)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (h *handler) createEntry(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerOfNewRecord(w, r)
	if !ok {
		return
	}
	var input EntryInput
	if respondError(w, decodeStrict(r, &input)) {
		return
	}
	entry, err := h.s.CreateEntry(r.Context(), owner, input)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

func (h *handler) getEntry(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	entry, err := h.s.Entry(r.Context(), scope, r.PathValue("id"), true)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *handler) updateEntry(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var change EntryChange
	if respondError(w, decodeStrict(r, &change)) {
		return
	}
	entry, err := h.s.UpdateEntry(r.Context(), scope, r.PathValue("id"), change)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *handler) deleteEntry(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	if respondError(w, h.s.DeleteEntry(r.Context(), scope, r.PathValue("id"), queryActor(r))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

func (h *handler) assignKind(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var body struct {
		By     string `json:"by"`
		ByKind string `json:"by_kind"`
		Reason string `json:"reason"`
	}
	if respondError(w, decodeStrict(r, &body)) {
		return
	}
	entry, err := h.s.AssignKind(r.Context(), scope, r.PathValue("id"), r.PathValue("kind_id"), Actor{By: body.By, Kind: body.ByKind}, body.Reason)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *handler) removeKind(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	if respondError(w, h.s.RemoveKind(r.Context(), scope, r.PathValue("id"), r.PathValue("kind_id"), queryActor(r))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) addLink(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
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
	link, err := h.s.AddLink(r.Context(), scope, r.PathValue("id"), body.LinkInput, Actor{By: body.By, Kind: body.ByKind})
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, link)
}

func (h *handler) removeLink(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	occurrence, err := queryOptionalInt(r, "occurrence_starts_at")
	if respondError(w, err) {
		return
	}
	if respondError(w, h.s.RemoveLink(r.Context(), scope, r.PathValue("id"), r.PathValue("link_type"), r.URL.Query().Get("ref"), occurrence, queryActor(r))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) linksTo(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		respondError(w, fmt.Errorf("%w: ref is required", ErrInvalid))
		return
	}
	occurrence, err := queryOptionalInt(r, "occurrence_starts_at")
	if respondError(w, err) {
		return
	}
	links, err := h.s.LinksTo(r.Context(), scope, r.PathValue("link_type"), ref, occurrence, r.URL.Query().Get("include_removed") == "true")
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, links)
}

func (h *handler) listReviews(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	reviews, err := h.s.Reviews(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, reviews)
}

func (h *handler) addReview(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var input ReviewInput
	if respondError(w, decodeStrict(r, &input)) {
		return
	}
	review, err := h.s.AddReview(r.Context(), scope, r.PathValue("id"), input)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, review)
}

func (h *handler) getReview(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	review, err := h.s.Review(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, review)
}
