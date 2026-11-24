package referencestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// ServiceName is how the store names itself to auth-store when it asks for a
// service's credential.
const ServiceName = "reference-store"

// PrincipalIDHeader is who an owner-decided lookup is for, sent to the
// owning service beside its credential.
const PrincipalIDHeader = "X-Principal-Id"

// Server answers reference-store's routes. Who a request is for comes from
// its context (ContextWithCaller), set by the server this store is mounted in
// from the credential it verified; a client cannot set it.
type Server struct {
	Resolver *Resolver
}

// RegisterHandlers mounts every route on mux at the paths the standalone
// store answered.
//
//	GET  /health
//	GET  /types            every reference type
//	GET  /types/{type}     one
//	GET  /vocabulary       the formats, access rules and grant names rows use
//	POST /resolve          {"ids":[…]} → what each id names, for the caller
//	GET  /search?q=        records of every mentionable type the caller may see
//	GET  /attachments/{id} a picture of the record, when the caller may see it
func RegisterHandlers(mux *http.ServeMux, server *Server) {
	mux.HandleFunc("GET /health", server.Health)
	mux.HandleFunc("GET /types", server.ListTypes)
	mux.HandleFunc("GET /types/{type}", server.GetType)
	mux.HandleFunc("GET /vocabulary", server.Vocabulary)
	mux.HandleFunc("POST /resolve", server.ResolveIDs)
	mux.HandleFunc("GET /search", server.Search)
	mux.HandleFunc("GET /attachments/{id}", server.AttachmentOfRecord)
}

type callerContextKey struct{}

// ContextWithCaller says who the request is for.
func ContextWithCaller(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, callerContextKey{}, caller)
}

// CallerFromContext is who the request is for, and false when nobody said.
func CallerFromContext(ctx context.Context) (Caller, bool) {
	caller, ok := ctx.Value(callerContextKey{}).(Caller)
	if !ok || (!caller.Unchecked && caller.PrincipalID == "") {
		return Caller{}, false
	}
	return caller, true
}

// Health answers that the store is up.
func (s *Server) Health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	// Store bodies pass through as they came: without this an & in a name
	// would arrive as &.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		log.Printf("[reference-store] write answer: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// ListTypes is every reference type.
func (s *Server) ListTypes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"types": s.Resolver.Types})
}

// GetType is one reference type.
func (s *Server) GetType(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("type")
	for i := range s.Resolver.Types {
		if s.Resolver.Types[i].Type == name {
			writeJSON(w, http.StatusOK, s.Resolver.Types[i])
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Sprintf("no reference type %q", name))
}

// Vocabulary is the formats, access rules and grant names rows use.
func (s *Server) Vocabulary(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"formats":  Formats,
		"access":   []Access{AccessByGrant, AccessByOwner, AccessByGrantAndOwner},
		"relation": RelationCanSeeReferences,
		"resource_types": map[string]string{
			ResourceTypeReferenceType: "a reference type's name, or * for every type",
			ResourceTypeReference:     "one record's id, as it is written",
		},
	})
}

const callerRequired = "sign in, or call as this server's internal service: every route but /health, /types and /vocabulary looks records up for someone"

// ResolveIDs is POST /resolve: what each id names, for the caller.
func (s *Server) ResolveIDs(w http.ResponseWriter, r *http.Request) {
	caller, ok := CallerFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, callerRequired)
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "body: "+err.Error())
		return
	}
	if len(body.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required and must not be empty")
		return
	}
	if len(body.IDs) > MaxIDsPerAsk {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d ids per ask", MaxIDsPerAsk))
		return
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(body.IDs))
	for _, id := range body.IDs {
		if id == "" || len(id) > MaxIDLength {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("an id is empty or longer than %d bytes", MaxIDLength))
			return
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	results, errs := s.Resolver.Resolve(r.Context(), caller, ids)
	answer := map[string]any{"results": results}
	if len(errs) > 0 {
		answer["errors"] = errs
	}
	writeJSON(w, http.StatusOK, answer)
}

const maxSearchQueryLength = 200

// searchAnswer is one type's search answer, passed through, beside the row
// fields that say how to read it.
type searchAnswer struct {
	Type          string          `json:"type"`
	Service       string          `json:"service"`
	SearchResults string          `json:"search_results"`
	LabelField    string          `json:"label_field"`
	IDPatterns    []string        `json:"id_patterns"`
	Data          json.RawMessage `json:"data"`
}

type searchError struct {
	Type  string `json:"type"`
	Error string `json:"error"`
}

// Search asks every mentionable type the caller may see whole. A type the
// caller sees only some records of is not searched: a search would list the
// others.
func (s *Server) Search(w http.ResponseWriter, r *http.Request) {
	caller, ok := CallerFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, callerRequired)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}
	if len(query) > maxSearchQueryLength {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("q is longer than %d bytes", maxSearchQueryLength))
		return
	}
	var rows []*ReferenceType
	for i := range s.Resolver.Types {
		row := &s.Resolver.Types[i]
		if !row.Mentionable() {
			continue
		}
		if !caller.Unchecked {
			allowed, err := s.mayAskTypeWhole(r.Context(), caller, row)
			if err != nil {
				writeError(w, http.StatusBadGateway, err.Error())
				return
			}
			if !allowed {
				continue
			}
		}
		rows = append(rows, row)
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		answers = make([]*searchAnswer, len(rows))
		errs    = []searchError{}
	)
	for i, row := range rows {
		wg.Add(1)
		go func(i int, row *ReferenceType) {
			defer wg.Done()
			data, err := s.searchOne(r.Context(), caller, row, query)
			if err != nil {
				mu.Lock()
				errs = append(errs, searchError{Type: row.Type, Error: err.Error()})
				mu.Unlock()
				log.Printf("[reference-store] search %s: %v", row.Type, err)
				return
			}
			answers[i] = &searchAnswer{Type: row.Type, Service: row.Service, SearchResults: row.SearchResults,
				LabelField: row.LabelField, IDPatterns: row.IDPatterns, Data: data}
		}(i, row)
	}
	wg.Wait()
	results := []searchAnswer{}
	for _, answer := range answers {
		if answer != nil {
			results = append(results, *answer)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "errors": errs})
}

// mayAskTypeWhole: an owner-decided type is searched as the caller, so the
// owner narrows the answer; a grant-decided one only when the caller sees the
// whole type, and one decided by both only then, searched as the caller.
func (s *Server) mayAskTypeWhole(ctx context.Context, caller Caller, row *ReferenceType) (bool, error) {
	if row.Access == AccessByOwner {
		return true, nil
	}
	return s.Resolver.Access.MaySeeType(ctx, caller.PrincipalID, row.Type)
}

func (s *Server) searchOne(ctx context.Context, caller Caller, row *ReferenceType, query string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Resolver.LookupTimeout)
	defer cancel()
	request, client, err := s.Resolver.newRequest(ctx, row, row.Search+url.QueryEscape(query))
	if err != nil {
		return nil, err
	}
	if row.Access.ownerDecides() && !caller.Unchecked {
		request.Header.Set(PrincipalIDHeader, caller.PrincipalID)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d: %s", row.Service, response.StatusCode, firstLine(body))
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("%s answered something that is not JSON", row.Service)
	}
	return body, nil
}

// AttachmentOfRecord is a picture of the record an id names, when the caller
// may see it.
func (s *Server) AttachmentOfRecord(w http.ResponseWriter, r *http.Request) {
	caller, ok := CallerFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, callerRequired)
		return
	}
	id := r.PathValue("id")
	contentType, data, err := s.Resolver.Attachment(r.Context(), caller, id)
	if errors.Is(err, ErrNoAttachment) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s names no record with a picture that you may see", id))
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Write(data)
}
