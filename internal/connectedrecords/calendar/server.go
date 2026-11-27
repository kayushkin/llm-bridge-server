package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// MaxImportBytes is the largest body POST /calendars/{id}/imports and
// /google-event-imports take.
const MaxImportBytes = 64 << 20

// RegisterHandlers mounts the API on mux. Every request must carry the
// ownership.Caller the server verified; one without is 401.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	h := &handler{s: s}
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /vocabularies", h.vocabularies)
	mux.HandleFunc("GET /calendars", h.listCalendars)
	mux.HandleFunc("POST /calendars", h.ensureCalendar)
	mux.HandleFunc("POST /calendars/{id}/imports", h.importICS)
	mux.HandleFunc("POST /calendars/{id}/google-event-imports", h.importGoogleEvents)
	mux.HandleFunc("GET /imports", h.listImports)
	mux.HandleFunc("GET /events", h.listEvents)
	mux.HandleFunc("GET /events/{id}", h.getEvent)
	mux.HandleFunc("GET /events/{id}/revisions", h.revisions)
	mux.HandleFunc("POST /events/{id}/people", h.linkPerson)
	mux.HandleFunc("DELETE /events/{id}/people/{person_id}", h.unlinkPerson)
}

type handler struct{ s *Store }

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("calendar-store: write response: %v", err)
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
		log.Printf("calendar-store: error: %v", err)
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
func readScope(w http.ResponseWriter, r *http.Request) (ownership.Scope, bool) {
	scope, refusal := ownership.ReadScope(r)
	if respondRefusal(w, refusal) {
		return ownership.Scope{}, false
	}
	return scope, true
}

func decodeStrict(r *http.Request, into any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	return nil
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
	writeJSON(w, http.StatusOK, map[string]any{"link_sources": LinkSources, "revision_reasons": []string{"import"}})
}

func (h *handler) listCalendars(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	calendars, err := h.s.Calendars(r.Context(), scope)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, calendars)
}

func (h *handler) ensureCalendar(w http.ResponseWriter, r *http.Request) {
	owner, refusal := ownership.OwnerOfNewRecord(r)
	if respondRefusal(w, refusal) {
		return
	}
	var body struct {
		Name      string `json:"name"`
		Source    string `json:"source"`
		SourceRef string `json:"source_ref"`
	}
	if respondError(w, decodeStrict(r, &body)) {
		return
	}
	calendar, created, err := h.s.EnsureCalendar(r.Context(), owner, body.Name, body.Source, body.SourceRef)
	if respondError(w, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, calendar)
}

func (h *handler) importICS(w http.ResponseWriter, r *http.Request) {
	h.importBody(w, r, h.s.ImportICS)
}

func (h *handler) importGoogleEvents(w http.ResponseWriter, r *http.Request) {
	h.importBody(w, r, h.s.ImportGoogleEvents)
}

func (h *handler) importBody(w http.ResponseWriter, r *http.Request, importInto func(ctx context.Context, scope ownership.Scope, calendarID, fileName string, content []byte) (Import, error)) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	content, err := io.ReadAll(io.LimitReader(r.Body, MaxImportBytes+1))
	if respondError(w, err) {
		return
	}
	if len(content) > MaxImportBytes {
		respondError(w, errors.Join(ErrInvalid, errors.New("the body is larger than 64 MiB")))
		return
	}
	record, err := importInto(r.Context(), scope, r.PathValue("id"), r.URL.Query().Get("file_name"), content)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, record)
}

func (h *handler) listImports(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	imports, err := h.s.Imports(r.Context(), scope)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, imports)
}

func queryInt(r *http.Request, name string) (int64, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return 0, nil
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, errors.Join(ErrInvalid, errors.New(name+" must be an integer"))
	}
	return number, nil
}

func (h *handler) listEvents(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	query := EventQuery{CalendarID: r.URL.Query().Get("calendar_id"), Text: r.URL.Query().Get("q"), PersonID: r.URL.Query().Get("person_id")}
	var err error
	var limit, offset int64
	for _, field := range []struct {
		name string
		into *int64
	}{{"from", &query.From}, {"to", &query.To}, {"limit", &limit}, {"offset", &offset}} {
		if *field.into, err = queryInt(r, field.name); respondError(w, err) {
			return
		}
	}
	query.Limit, query.Offset = int(limit), int(offset)
	events, err := h.s.Events(r.Context(), scope, query)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (h *handler) getEvent(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	event, err := h.s.GetEvent(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, event)
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

func (h *handler) linkPerson(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var body struct {
		PersonID string `json:"person_id"`
		LinkedBy string `json:"linked_by"`
		Reason   string `json:"reason"`
	}
	if respondError(w, decodeStrict(r, &body)) {
		return
	}
	event, err := h.s.LinkPerson(r.Context(), scope, r.PathValue("id"), PersonLink{PersonID: body.PersonID, LinkedBy: body.LinkedBy, Reason: body.Reason})
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, event)
}

func (h *handler) unlinkPerson(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	if respondError(w, h.s.UnlinkPerson(r.Context(), scope, r.PathValue("id"), r.PathValue("person_id"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
