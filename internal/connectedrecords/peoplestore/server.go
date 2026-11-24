package peoplestore

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
)

// RegisterHandlers mounts the API on mux. Routes are rooted at /; a front door
// such as dash adds its own prefix and the auth this service has none of,
// which is why it binds 127.0.0.1 only.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	h := &handler{s: s}
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /identity-kinds", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, IdentityKinds) })
	mux.HandleFunc("GET /relationship-kinds", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, RelationshipKinds) })
	mux.HandleFunc("GET /link-sources", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, LinkSources) })

	mux.HandleFunc("GET /persons", h.listPersons)
	mux.HandleFunc("POST /persons", h.createPerson)
	mux.HandleFunc("GET /persons/{id}", h.getPerson)
	mux.HandleFunc("PATCH /persons/{id}", h.patchPerson)
	mux.HandleFunc("DELETE /persons/{id}", h.deletePerson)
	mux.HandleFunc("POST /persons/{id}/restore", h.restorePerson)
	mux.HandleFunc("GET /persons/{id}/revisions", h.listRevisions)
	mux.HandleFunc("POST /persons/{id}/identities", h.addIdentity)
	mux.HandleFunc("POST /persons/{id}/relationships", h.addRelationship)

	mux.HandleFunc("GET /identities", h.listIdentities)
	mux.HandleFunc("GET /identities/resolve", h.resolveIdentity)
	mux.HandleFunc("GET /refusals", h.listRefusals)
	mux.HandleFunc("DELETE /identities/{id}", h.removeIdentity)
	mux.HandleFunc("DELETE /relationships/{id}", h.removeRelationship)

	mux.HandleFunc("GET /suggestions", h.listSuggestions)
	mux.HandleFunc("POST /suggestions", h.proposeSuggestion)
	mux.HandleFunc("GET /suggestions/{id}", h.getSuggestion)
	mux.HandleFunc("POST /suggestions/{id}/accept", h.acceptSuggestion)
	mux.HandleFunc("POST /suggestions/{id}/reject", h.rejectSuggestion)

	mux.HandleFunc("PUT /handle-activity", h.reportHandleActivity)

	mux.HandleFunc("GET /reviews/pending", h.pendingReviews)
	mux.HandleFunc("POST /reviews/claim", h.claimReviews)
	mux.HandleFunc("POST /reviews", h.markReviewed)
}

func (h *handler) listIdentities(w http.ResponseWriter, r *http.Request) {
	limit, ok := intParameter(w, r, "limit")
	if !ok {
		return
	}
	offset, ok := intParameter(w, r, "offset")
	if !ok {
		return
	}
	identities, err := h.s.ListIdentities(IdentityFilter{Kind: r.URL.Query().Get("kind"), Limit: limit, Offset: offset})
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, identities)
}

func (h *handler) listRefusals(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	refusals, err := h.s.ListRefusals(RefusalFilter{PersonID: query.Get("person_id"), Kind: query.Get("kind"), Value: query.Get("value")})
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, refusals)
}

func (h *handler) pendingReviews(w http.ResponseWriter, r *http.Request) {
	limit, ok := intParameter(w, r, "limit")
	if !ok {
		return
	}
	queue, err := h.s.PendingReviews(limit)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, queue)
}

func (h *handler) claimReviews(w http.ResponseWriter, r *http.Request) {
	var input ReviewClaimInput
	if !decode(w, r, &input) {
		return
	}
	claim, err := h.s.ClaimReviews(input)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, claim)
}

func (h *handler) markReviewed(w http.ResponseWriter, r *http.Request) {
	var input ReviewInput
	if !decode(w, r, &input) {
		return
	}
	result, err := h.s.MarkReviewed(input)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type handler struct {
	s *Store
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	counts, err := h.s.Counts()
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "counts": counts})
}

func (h *handler) listPersons(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit, ok := intParameter(w, r, "limit")
	if !ok {
		return
	}
	offset, ok := intParameter(w, r, "offset")
	if !ok {
		return
	}
	includeDeleted, ok := boolParameter(w, r, "include_deleted")
	if !ok {
		return
	}
	persons, err := h.s.ListPersons(PersonFilter{Query: query.Get("q"), Tag: query.Get("tag"), PrincipalID: query.Get("principal_id"), IncludeDeleted: includeDeleted, Limit: limit, Offset: offset})
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, persons)
}

func (h *handler) createPerson(w http.ResponseWriter, r *http.Request) {
	var input PersonInput
	if !decode(w, r, &input) {
		return
	}
	person, err := h.s.CreatePerson(input)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, person)
}

func (h *handler) getPerson(w http.ResponseWriter, r *http.Request) {
	person, err := h.s.GetPerson(r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, person)
}

// patchableFields is every key PATCH /persons/{id} accepts, sorted, because
// the 400 for an unknown key lists them.
var patchableFields = []string{"birthday", "display_name", "how_met", "principal_id", "tags"}

// unpatchableFields answers a key PATCH refuses with what does change it.
var unpatchableFields = map[string]string{
	"id":            "id is assigned by this store and never changes; other records join on it.",
	"deleted_at":    "use DELETE /persons/{id} or POST /persons/{id}/restore, so that removal is always an explicit act.",
	"identities":    "use POST /persons/{id}/identities and DELETE /identities/{id}.",
	"relationships": "use POST /persons/{id}/relationships and DELETE /relationships/{id}.",
	"created_at":    "created_at is set by this store.",
	"updated_at":    "updated_at is set by this store.",
}

func (h *handler) patchPerson(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if !decode(w, r, &raw) {
		return
	}
	var patch PersonPatch
	for field, value := range raw {
		if explanation, refused := unpatchableFields[field]; refused {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s cannot be patched: %s", field, explanation))
			return
		}
		if !contains(patchableFields, field) {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown field %q: PATCH /persons/{id} takes %s", field, strings.Join(patchableFields, ", ")))
			return
		}
		isNull := bytes.Equal(bytes.TrimSpace(value), []byte("null"))
		var err error
		switch field {
		case "display_name":
			patch.DisplayName, err = stringField(field, value, isNull, false)
		case "birthday":
			patch.Birthday, err = stringField(field, value, isNull, true)
		case "how_met":
			patch.HowMet, err = stringField(field, value, isNull, true)
		case "principal_id":
			patch.PrincipalID, err = stringField(field, value, isNull, true)
		case "tags":
			tags := []string{}
			if !isNull {
				if err = json.Unmarshal(value, &tags); err != nil {
					err = fmt.Errorf("tags must be an array of strings: %v", err)
				}
			}
			patch.Tags = &tags
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	person, err := h.s.PatchPerson(r.PathValue("id"), patch)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, person)
}

// stringField reads one string from a patch. null clears the field when
// nullClears, and is refused otherwise.
func stringField(field string, value json.RawMessage, isNull, nullClears bool) (*string, error) {
	if isNull {
		if !nullClears {
			return nil, fmt.Errorf("%s cannot be null", field)
		}
		empty := ""
		return &empty, nil
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return nil, fmt.Errorf("%s must be a string: %v", field, err)
	}
	return &text, nil
}

func (h *handler) deletePerson(w http.ResponseWriter, r *http.Request) {
	person, err := h.s.DeletePerson(r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, person)
}

func (h *handler) restorePerson(w http.ResponseWriter, r *http.Request) {
	person, err := h.s.RestorePerson(r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, person)
}

func (h *handler) listRevisions(w http.ResponseWriter, r *http.Request) {
	revisions, err := h.s.Revisions(r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, revisions)
}

func (h *handler) addIdentity(w http.ResponseWriter, r *http.Request) {
	var input IdentityLinkInput
	if !decode(w, r, &input) {
		return
	}
	identity, added, err := h.s.AddIdentity(r.PathValue("id"), input)
	if respondStoreError(w, err) {
		return
	}
	status := http.StatusCreated
	if !added {
		status = http.StatusOK
	}
	writeJSON(w, status, identity)
}

func (h *handler) removeIdentity(w http.ResponseWriter, r *http.Request) {
	if respondStoreError(w, h.s.RemoveIdentity(r.PathValue("id"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) resolveIdentity(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	resolution, err := h.s.ResolveIdentity(query.Get("kind"), query.Get("value"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, resolution)
}

func (h *handler) addRelationship(w http.ResponseWriter, r *http.Request) {
	var input RelationshipInput
	if !decode(w, r, &input) {
		return
	}
	relationship, err := h.s.AddRelationship(r.PathValue("id"), input)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, relationship)
}

func (h *handler) removeRelationship(w http.ResponseWriter, r *http.Request) {
	if respondStoreError(w, h.s.RemoveRelationship(r.PathValue("id"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) listSuggestions(w http.ResponseWriter, r *http.Request) {
	limit, ok := intParameter(w, r, "limit")
	if !ok {
		return
	}
	offset, ok := intParameter(w, r, "offset")
	if !ok {
		return
	}
	query := r.URL.Query()
	suggestions, err := h.s.ListSuggestions(SuggestionFilter{Status: query.Get("status"), Query: query.Get("q"), Sort: query.Get("sort"), Limit: limit, Offset: offset})
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, suggestions)
}

func (h *handler) getSuggestion(w http.ResponseWriter, r *http.Request) {
	suggestion, err := h.s.GetSuggestion(r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, suggestion)
}

func (h *handler) proposeSuggestion(w http.ResponseWriter, r *http.Request) {
	var input SuggestionInput
	if !decode(w, r, &input) {
		return
	}
	proposal, err := h.s.ProposeSuggestion(input)
	if respondStoreError(w, err) {
		return
	}
	status := http.StatusCreated
	if !proposal.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, proposal)
}

func (h *handler) acceptSuggestion(w http.ResponseWriter, r *http.Request) {
	var input AcceptSuggestionInput
	if !decodeOptional(w, r, &input) {
		return
	}
	accepted, err := h.s.AcceptSuggestion(r.PathValue("id"), input.PersonID)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, accepted)
}

func (h *handler) rejectSuggestion(w http.ResponseWriter, r *http.Request) {
	suggestion, err := h.s.RejectSuggestion(r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, suggestion)
}

func (h *handler) reportHandleActivity(w http.ResponseWriter, r *http.Request) {
	var report HandleActivityReport
	if !decode(w, r, &report) {
		return
	}
	result, err := h.s.ReportHandleActivity(report)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// decode reads a JSON body strictly: an unknown key is a 400, so a misspelled
// field is never silently ignored.
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

// decodeOptional is decode for a body that may be absent altogether.
func decodeOptional(w http.ResponseWriter, r *http.Request, target any) bool {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func intParameter(w http.ResponseWriter, r *http.Request, name string) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return 0, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s must be a whole number, got %q", name, raw))
		return 0, false
	}
	return value, true
}

func boolParameter(w http.ResponseWriter, r *http.Request, name string) (bool, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return false, true
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s must be true or false, got %q", name, raw))
		return false, false
	}
	return value, true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("people-store: encode response: %v", err)
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
	case errors.Is(err, ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrPrincipalStoreUnavailable):
		writeErr(w, http.StatusBadGateway, err.Error())
	default:
		log.Printf("people-store: %v", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
	return true
}
