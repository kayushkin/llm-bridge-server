package files

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// principalIDShape is principal-store's id. See File.UploadedByPrincipalID for
// why only the shape is checked here.
var principalIDShape = regexp.MustCompile(`^principal_\d{6,}$`)

const (
	defaultListLimit = 100
	maximumListLimit = 500
	maximumNameBytes = 255
)

// Server is the HTTP API over a Store. It decides nothing about who may call
// it: the server it is mounted in lets only callers that check access
// themselves reach it, because whoever reaches it reads every file.
type Server struct {
	store *Store
	// limits reads the size limit and the inline list in force, at the time
	// of each request, so a change to either needs no restart.
	limits func() Limits
}

// NewServer wires the API. limits is read on every request that needs it.
func NewServer(store *Store, limits func() Limits) *Server {
	return &Server{store: store, limits: limits}
}

// Handler is every route, at the paths the standalone store answered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.Health)
	mux.HandleFunc("GET /limits", s.Limits)
	mux.HandleFunc("POST /files", s.Upload)
	mux.HandleFunc("GET /files", s.List)
	mux.HandleFunc("GET /files/{id}", s.Metadata)
	mux.HandleFunc("GET /files/{id}/content", s.Content)
	mux.HandleFunc("DELETE /files/{id}", s.Delete)
	mux.HandleFunc("POST /files/{id}/restore", s.Restore)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// WriteStoreError answers err as the routes answer it: 404 for a file this
// store does not hold, 500 for anything else.
func WriteStoreError(w http.ResponseWriter, err error) {
	writeStoreError(w, err)
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func (s *Server) Health(w http.ResponseWriter, r *http.Request) {
	health, err := s.store.Health()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, health)
}

func (s *Server) Limits(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.limits())
}

// validateFilename refuses anything that is not a plain name. The name is
// never used as a path here — blobs are named by their hash — but it is handed
// back in a Content-Disposition header and shown to people, and the next
// program to save the file will use it as one.
func validateFilename(name string) error {
	switch {
	case name == "":
		return errors.New("filename is required")
	case len(name) > maximumNameBytes:
		return fmt.Errorf("filename is %d bytes; at most %d", len(name), maximumNameBytes)
	case name != strings.TrimSpace(name):
		return errors.New("filename has surrounding whitespace")
	case name == "." || name == "..":
		return fmt.Errorf("filename %q is not a name", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("filename %q contains a path separator; send the name alone", name)
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return errors.New("filename contains a control character")
		}
	}
	return nil
}

func validateOwnerLabel(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required: a file belongs to the service that put it here, for a thing of that service's", field)
	}
	if len(value) > maximumNameBytes || value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must be at most %d bytes with no surrounding whitespace", field, maximumNameBytes)
	}
	return nil
}

// Upload is POST /files?filename=…&owner_service=…&owner_ref=…[&uploaded_by_principal_id=…].
// The body is the file's bytes and Content-Type is what they are declared to be.
func (s *Server) Upload(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	stored, err := s.Accept(r.Body, r.ContentLength, r.Header.Get("Content-Type"), NewFile{
		Filename: query.Get("filename"), OwnerService: query.Get("owner_service"), OwnerRef: query.Get("owner_ref"),
		UploadedByPrincipalID: query.Get("uploaded_by_principal_id"),
	})
	if err != nil {
		writeAcceptError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, stored)
}

func (s *Server) List(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := Filter{OwnerService: query.Get("owner_service"), OwnerRef: query.Get("owner_ref"), SHA256: query.Get("sha256"), Limit: defaultListLimit}
	for name, target := range map[string]*int{"limit": &filter.Limit, "offset": &filter.Offset} {
		if raw := query.Get(name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be a non-negative integer, got %q", name, raw))
				return
			}
			*target = value
		}
	}
	if filter.Limit == 0 || filter.Limit > maximumListLimit {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", maximumListLimit))
		return
	}
	switch raw := query.Get("include_deleted"); raw {
	case "", "false":
	case "true":
		filter.IncludeDeleted = true
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("include_deleted must be true or false, got %q", raw))
		return
	}
	files, err := s.store.List(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (s *Server) Metadata(w http.ResponseWriter, r *http.Request) {
	file, err := s.store.Get(r.PathValue("id"), r.URL.Query().Get("include_deleted") == "true")
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, file)
}

// Content serves a live file's bytes: GET /files/{id}/content[?inline=true].
func (s *Server) Content(w http.ResponseWriter, r *http.Request) {
	s.ServeContentOf(w, r, r.PathValue("id"), r.URL.Query().Get("inline"))
}

// Delete is reversible unless ?hard=true, which destroys the row and, when no
// other row names them, the bytes.
func (s *Server) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var err error
	switch raw := r.URL.Query().Get("hard"); raw {
	case "", "false":
		err = s.store.Delete(id)
	case "true":
		err = s.store.Purge(id)
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("hard must be true or false, got %q", raw))
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) Restore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.Restore(id); err != nil {
		writeStoreError(w, err)
		return
	}
	file, err := s.store.Get(id, false)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, file)
}
