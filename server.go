package calendarstore

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
)

// MaxImportBytes is the largest iCalendar file POST /calendars/{id}/imports
// takes.
const MaxImportBytes = 64 << 20

// RegisterHandlers mounts the API on mux. A front door such as dash adds the
// auth this service has none of, which is why it binds 127.0.0.1.
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

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	counts, err := h.s.Counts()
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "counts": counts})
}

func (h *handler) vocabularies(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"link_sources": LinkSources, "revision_reasons": []string{"import"}})
}

func (h *handler) listCalendars(w http.ResponseWriter, r *http.Request) {
	calendars, err := h.s.Calendars()
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, calendars)
}

func (h *handler) ensureCalendar(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string `json:"name"`
		Source    string `json:"source"`
		SourceRef string `json:"source_ref"`
	}
	if respondError(w, decodeStrict(r, &body)) {
		return
	}
	calendar, created, err := h.s.EnsureCalendar(body.Name, body.Source, body.SourceRef)
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

func (h *handler) importBody(w http.ResponseWriter, r *http.Request, importInto func(calendarID, fileName string, content []byte) (Import, error)) {
	content, err := io.ReadAll(io.LimitReader(r.Body, MaxImportBytes+1))
	if respondError(w, err) {
		return
	}
	if len(content) > MaxImportBytes {
		respondError(w, errors.Join(ErrInvalid, errors.New("the body is larger than 64 MiB")))
		return
	}
	record, err := importInto(r.PathValue("id"), r.URL.Query().Get("file_name"), content)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, record)
}

func (h *handler) listImports(w http.ResponseWriter, r *http.Request) {
	imports, err := h.s.Imports()
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
	events, err := h.s.Events(query)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (h *handler) getEvent(w http.ResponseWriter, r *http.Request) {
	event, err := h.s.GetEvent(r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, event)
}

func (h *handler) revisions(w http.ResponseWriter, r *http.Request) {
	revisions, err := h.s.Revisions(r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, revisions)
}

func (h *handler) linkPerson(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PersonID string `json:"person_id"`
		LinkedBy string `json:"linked_by"`
		Reason   string `json:"reason"`
	}
	if respondError(w, decodeStrict(r, &body)) {
		return
	}
	event, err := h.s.LinkPerson(r.PathValue("id"), PersonLink{PersonID: body.PersonID, LinkedBy: body.LinkedBy, Reason: body.Reason})
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, event)
}

func (h *handler) unlinkPerson(w http.ResponseWriter, r *http.Request) {
	if respondError(w, h.s.UnlinkPerson(r.PathValue("id"), r.PathValue("person_id"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
