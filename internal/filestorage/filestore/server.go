package filestore

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceTokenHeader is what every caller but /health must send.
const ServiceTokenHeader = "X-File-Store-Service-Token"

// principalIDShape is principal-store's id. See File.UploadedByPrincipalID for
// why only the shape is checked here.
var principalIDShape = regexp.MustCompile(`^principal_\d{6,}$`)

const (
	defaultListLimit = 100
	maximumListLimit = 500
	maximumNameBytes = 255
)

// Server is the HTTP API over a Store.
type Server struct {
	store    *Store
	settings *servicesettings.Registry
}

// NewServer wires the API. It refuses a token too short to be one, at start,
// because serving with it would serve every file to anyone who guessed little.
func NewServer(store *Store, settings *servicesettings.Registry) (*Server, error) {
	if len(settings.String(SettingServiceToken)) < MinimumServiceTokenLength {
		return nil, fmt.Errorf("%s must be at least %d characters: whoever holds it reads every file", SettingServiceToken, MinimumServiceTokenLength)
	}
	return &Server{store: store, settings: settings}, nil
}

// Handler is every route. /health is open; the rest need the service token.
func (s *Server) Handler() http.Handler {
	gated := http.NewServeMux()
	gated.HandleFunc("GET /limits", s.limits)
	gated.HandleFunc("POST /files", s.upload)
	gated.HandleFunc("GET /files", s.list)
	gated.HandleFunc("GET /files/{id}", s.metadata)
	gated.HandleFunc("GET /files/{id}/content", s.content)
	gated.HandleFunc("DELETE /files/{id}", s.delete)
	gated.HandleFunc("POST /files/{id}/restore", s.restore)
	settingsHandler := servicesettings.Handler(s.settings, "/settings")
	gated.Handle("/settings", settingsHandler)
	gated.Handle("/settings/", settingsHandler)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.Handle("/", s.requireServiceToken(gated))
	return mux
}

func (s *Server) requireServiceToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := r.Header.Get(ServiceTokenHeader)
		expected := s.settings.String(SettingServiceToken)
		if presented == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
			writeError(w, http.StatusUnauthorized, "send "+ServiceTokenHeader+": this store serves only services that check who may read a file")
			return
		}
		next.ServeHTTP(w, r)
	})
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

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	health, err := s.store.Health()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, health)
}

func (s *Server) currentLimits() Limits {
	return Limits{
		MaximumFileBytes:   int64(s.settings.Integer(SettingMaximumFileBytes)),
		InlineContentTypes: s.settings.StringList(SettingInlineContentTypes),
	}
}

func (s *Server) limits(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.currentLimits())
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

// upload is POST /files?filename=…&owner_service=…&owner_ref=…[&uploaded_by_principal_id=…].
// The body is the file's bytes and Content-Type is what they are declared to be.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	file := NewFile{
		Filename: query.Get("filename"), OwnerService: query.Get("owner_service"), OwnerRef: query.Get("owner_ref"),
		UploadedByPrincipalID: query.Get("uploaded_by_principal_id"),
	}
	if err := validateFilename(file.Filename); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for field, value := range map[string]string{"owner_service": file.OwnerService, "owner_ref": file.OwnerRef} {
		if err := validateOwnerLabel(field, value); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if file.UploadedByPrincipalID != "" && !principalIDShape.MatchString(file.UploadedByPrincipalID) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("uploaded_by_principal_id %q is not a principal-store id (principal_000001)", file.UploadedByPrincipalID))
		return
	}
	// The declared type is part of the record and decides how the file may be
	// served, so it is not guessed when it is missing.
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Content-Type must say what the file is (application/octet-stream when nothing better is known): "+err.Error())
		return
	}
	file.ContentType = mediaType

	limits := s.currentLimits()
	if r.ContentLength > limits.MaximumFileBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the file is %d bytes and the limit is %d (GET /limits)", r.ContentLength, limits.MaximumFileBytes))
		return
	}
	stored, err := s.store.Put(r.Body, limits.MaximumFileBytes, file)
	switch {
	case errors.Is(err, ErrFileTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error()+" (GET /limits)")
	case errors.Is(err, ErrEmptyFile):
		writeError(w, http.StatusBadRequest, "the file is empty")
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusCreated, stored)
	}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) metadata(w http.ResponseWriter, r *http.Request) {
	file, err := s.store.Get(r.PathValue("id"), r.URL.Query().Get("include_deleted") == "true")
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, file)
}

// content serves a live file's bytes.
//
// Every answer is a download unless the caller asks ?inline=true AND the
// file's declared type is one this store serves for display. Whatever the
// answer, the browser is told not to sniff the type and to run nothing: a
// person's upload shown from the dashboard's origin must never execute as it.
func (s *Server) content(w http.ResponseWriter, r *http.Request) {
	file, err := s.store.Get(r.PathValue("id"), false)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	inline := false
	switch raw := r.URL.Query().Get("inline"); raw {
	case "", "false":
	case "true":
		for _, allowed := range s.currentLimits().InlineContentTypes {
			if allowed == file.ContentType {
				inline = true
			}
		}
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("inline must be true or false, got %q", raw))
		return
	}
	blob, err := s.store.OpenContent(file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer blob.Close()

	header := w.Header()
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	header.Set("Cache-Control", "private, no-store")
	header.Set("ETag", `"`+file.SHA256+`"`)
	disposition := "attachment"
	header.Set("Content-Type", "application/octet-stream")
	if inline {
		disposition = "inline"
		header.Set("Content-Type", file.ContentType)
	}
	header.Set("Content-Disposition", disposition+"; filename*=UTF-8''"+url.PathEscape(file.Filename))
	http.ServeContent(w, r, "", time.Unix(file.CreatedAt, 0), blob)
}

// delete is reversible unless ?hard=true, which destroys the row and, when no
// other row names them, the bytes.
func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
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
