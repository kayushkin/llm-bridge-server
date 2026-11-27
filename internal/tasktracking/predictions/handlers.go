package predictions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Handlers answers the store's HTTP routes. The server that mounts the store
// registers each method under its own pattern, behind its own prefix and
// access rules; the paths in the method comments are relative to that prefix.
type Handlers struct{ s *Store }

// NewHandlers returns the route handlers of store.
func NewHandlers(store *Store) *Handlers { return &Handlers{s: store} }

// patchableFields is every key PATCH /predictions/{id} accepts, matching the
// json tags on Patch. probability, outcome and status are deliberately absent
// and are refused by name above, pointing at the route that does move them.
var patchableFields = map[string]bool{
	"claim":               true,
	"resolution_criteria": true,
	"category":            true,
	"tags":                true,
	"provenance":          true,
	"author":              true,
	"due_at":              true,
	// The note explains an outcome it cannot change, so a wrong one is worth
	// correcting. Store.Patch refuses it on a row that has not resolved yet.
	"resolution_note": true,
}

func sortedPatchableFields() []string {
	out := make([]string, 0, len(patchableFields))
	for field := range patchableFields {
		out = append(out, field)
	}
	sort.Strings(out)
	return out
}

// Health answers GET /health.
func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	total, err := h.s.Count(r.Context(), Filter{})
	if respondStoreError(w, err) {
		return
	}
	open, err := h.s.Count(r.Context(), Filter{Status: "open"})
	if respondStoreError(w, err) {
		return
	}
	overdue, err := h.s.Count(r.Context(), Filter{Overdue: true})
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"counts": map[string]int{"predictions": total, "open": open, "overdue": overdue},
	})
}

// Vocabulary answers GET /vocabulary and GET /categories.
func (h *Handlers) Vocabulary(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"categories":         Categories,
		"provenances":        Provenances,
		"statuses":           Statuses,
		"outcomes":           Outcomes,
		"group_by":           GroupByFields,
		"probability_ladder": ProbabilityLadder,
	})
}

// ListTags answers GET /tags.
func (h *Handlers) ListTags(w http.ResponseWriter, r *http.Request) {
	tags, err := h.s.ListTags(r.Context())
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
}

func filterFrom(r *http.Request) Filter {
	q := r.URL.Query()
	return Filter{
		Status:         q.Get("status"),
		Category:       q.Get("category"),
		Tag:            q.Get("tag"),
		Author:         q.Get("author"),
		Provenance:     q.Get("provenance"),
		Outcome:        q.Get("outcome"),
		EntityType:     q.Get("entity_type"),
		EntityRef:      q.Get("entity_ref"),
		Query:          q.Get("q"),
		Overdue:        isTrue(q.Get("overdue")) || isTrue(q.Get("due")),
		Since:          atoi64(q.Get("since")),
		Until:          atoi64(q.Get("until")),
		IncludeDeleted: isTrue(q.Get("include_deleted")),
		Expand:         isTrue(q.Get("expand")),
		Limit:          int(atoi64(q.Get("limit"))),
		Offset:         int(atoi64(q.Get("offset"))),
	}
}

// ListPredictions answers GET /predictions.
func (h *Handlers) ListPredictions(w http.ResponseWriter, r *http.Request) {
	list, err := h.s.ListWithTotal(r.Context(), filterFrom(r))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// PredictionList is GET /predictions's answer: one page of predictions, and
// how many match the filter in all.
type PredictionList struct {
	Predictions []*Prediction `json:"predictions"`
	Total       int           `json:"total"`
}

// ListWithTotal is the page of predictions f names and the count of all that
// match it, as GET /predictions answers them.
func (s *Store) ListWithTotal(ctx context.Context, f Filter) (PredictionList, error) {
	predictions, err := s.List(ctx, f)
	if err != nil {
		return PredictionList{}, err
	}
	total, err := s.Count(ctx, f)
	if err != nil {
		return PredictionList{}, err
	}
	return PredictionList{Predictions: predictions, Total: total}, nil
}

// CreatePrediction answers POST /predictions.
func (h *Handlers) CreatePrediction(w http.ResponseWriter, r *http.Request) {
	var p Prediction
	if !decode(w, r, &p) {
		return
	}
	// A note about why a row resolved cannot exist before it is stated, and the
	// INSERT writes '' over this field regardless. Left alone that is a silent
	// discard: the caller sends prose and never learns it was thrown away.
	if strings.TrimSpace(p.ResolutionNote) != "" {
		writeErr(w, http.StatusBadRequest,
			`"resolution_note" cannot be set at creation: a prediction has no outcome yet. `+
				`Send it with the outcome to POST /predictions/{id}/resolve.`)
		return
	}
	created, err := h.s.Create(r.Context(), &p)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// GetPrediction answers GET /predictions/{id}.
func (h *Handlers) GetPrediction(w http.ResponseWriter, r *http.Request) {
	p, err := h.s.Get(r.Context(), r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// PatchPrediction answers PATCH /predictions/{id}.
func (h *Handlers) PatchPrediction(w http.ResponseWriter, r *http.Request) {
	// Decoded strictly so that PATCHing "probability" or "outcome" is a loud
	// 400 naming the right route, not a silent no-op that leaves the caller
	// believing the number moved.
	var raw map[string]json.RawMessage
	if !decode(w, r, &raw) {
		return
	}
	for field, route := range map[string]string{
		"probability": "POST /predictions/{id}/estimates",
		"outcome":     "POST /predictions/{id}/resolve",
		"status":      "POST /predictions/{id}/resolve",
	} {
		if _, present := raw[field]; present {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf(
				"%q cannot be patched: use %s. Overwriting it in place would destroy the record of what you believed before.", field, route))
			return
		}
	}
	// Decoding into a map defeats DisallowUnknownFields, so the allowed keys
	// are checked by hand. Without this a PATCH of {"clam":"…"} answers 200
	// having changed nothing, and the caller believes the edit landed.
	for field := range raw {
		if !patchableFields[field] {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf(
				"unknown field %q: PATCH accepts %s", field, strings.Join(sortedPatchableFields(), ", ")))
			return
		}
	}
	body, err := json.Marshal(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	var patch Patch
	if err := json.Unmarshal(body, &patch); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	updated, err := h.s.Patch(r.Context(), r.PathValue("id"), patch)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// DeletePrediction answers DELETE /predictions/{id}.
func (h *Handlers) DeletePrediction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if isTrue(r.URL.Query().Get("hard")) {
		if respondStoreError(w, h.s.Purge(r.Context(), id)) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"purged": id})
		return
	}
	if respondStoreError(w, h.s.SoftDelete(r.Context(), id)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// RestorePrediction answers POST /predictions/{id}/restore.
func (h *Handlers) RestorePrediction(w http.ResponseWriter, r *http.Request) {
	p, err := h.s.Restore(r.Context(), r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ListEstimates answers GET /predictions/{id}/estimates.
func (h *Handlers) ListEstimates(w http.ResponseWriter, r *http.Request) {
	estimates, err := h.s.ListEstimates(r.Context(), r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"estimates": estimates})
}

// AddEstimate answers POST /predictions/{id}/estimates.
func (h *Handlers) AddEstimate(w http.ResponseWriter, r *http.Request) {
	var e Estimate
	if !decode(w, r, &e) {
		return
	}
	p, err := h.s.AddEstimate(r.Context(), r.PathValue("id"), &e)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// ResolvePrediction answers POST /predictions/{id}/resolve.
func (h *Handlers) ResolvePrediction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Outcome string `json:"outcome"`
		Note    string `json:"note"`
	}
	if !decode(w, r, &body) {
		return
	}
	p, err := h.s.Resolve(r.Context(), r.PathValue("id"), body.Outcome, body.Note)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ListLinks answers GET /predictions/{id}/links.
func (h *Handlers) ListLinks(w http.ResponseWriter, r *http.Request) {
	links, err := h.s.ListLinks(r.Context(), r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": links})
}

// AddLink answers POST /predictions/{id}/links.
func (h *Handlers) AddLink(w http.ResponseWriter, r *http.Request) {
	var l Link
	if !decode(w, r, &l) {
		return
	}
	created, err := h.s.AddLink(r.Context(), r.PathValue("id"), &l)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// DeleteLink answers DELETE /links/{id}.
func (h *Handlers) DeleteLink(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "link id must be a number: "+err.Error())
		return
	}
	if respondStoreError(w, h.s.DeleteLink(r.Context(), id)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// Calibration answers GET /calibration.
func (h *Handlers) Calibration(w http.ResponseWriter, r *http.Request) {
	report, err := h.s.Calibration(r.Context(), filterFrom(r), r.URL.Query().Get("group_by"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// BaseRates answers GET /base-rates.
func (h *Handlers) BaseRates(w http.ResponseWriter, r *http.Request) {
	report, err := h.s.BaseRates(r.Context(), filterFrom(r), r.URL.Query().Get("group_by"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// decode reads a JSON body, rejecting unknown fields so a misspelled key is a
// 400 rather than a write that silently drops it.
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("prediction-store: ERROR: writing a response: %v", err)
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
	case errors.Is(err, ErrAlreadyResolved):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrInvalidPrediction), errors.Is(err, ErrInvalidEstimate), errors.Is(err, ErrInvalidLink):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

func atoi64(raw string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func isTrue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
