package messages

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// RegisterHandlers mounts the API on mux, rooted at /; the server mounts it
// under /messages/. Every request must carry the ownership.Caller the
// server verified; one without is 401. CONTRACT.md is the route table.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	h := &handler{s: s}
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /vocabulary", h.vocabulary)
	mux.HandleFunc("GET /summary", h.summary)

	mux.HandleFunc("GET /messages", h.listMessages)
	mux.HandleFunc("GET /messages/{id}", h.getMessage)
	mux.HandleFunc("GET /messages/{id}/revisions", h.messageRevisions)
	mux.HandleFunc("GET /superseded-messages", h.listSupersededMessages)

	mux.HandleFunc("GET /accounts", h.listAccounts)
	mux.HandleFunc("POST /accounts", h.putAccount)
	mux.HandleFunc("GET /accounts/{id}", h.getAccount)

	mux.HandleFunc("GET /conversations", h.listConversations)
	mux.HandleFunc("GET /conversations/{id}", h.getConversation)
	mux.HandleFunc("GET /conversation-merges", h.listConversationMerges)
	mux.HandleFunc("PATCH /conversations/{id}/participants/{handle}", h.patchParticipant)

	mux.HandleFunc("GET /conversation-days", h.listDays)
	mux.HandleFunc("GET /conversation-days/{id}", h.getDay)
	mux.HandleFunc("POST /conversation-days/journal-entries", h.linkJournalEntries)

	mux.HandleFunc("GET /activity", h.activity)

	mux.HandleFunc("GET /imports", h.listImports)
	mux.HandleFunc("POST /imports", h.startImport)
	mux.HandleFunc("GET /imports/{id}", h.getImport)
	mux.HandleFunc("POST /imports/{id}/batches", h.importBatch)
	mux.HandleFunc("POST /imports/{id}/finish", h.finishImport)
}

type handler struct {
	s *Store
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write response: %v", err)
	}
}

// writeError maps the store's errors to statuses; anything unexpected is a
// 500 and is logged.
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrInvalid):
		status = http.StatusBadRequest
	default:
		log.Printf("message-store: error: %v", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
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

// ownerOfNewRecord is the owner a record the request makes gets, or false
// once a refusal is answered.
func ownerOfNewRecord(w http.ResponseWriter, r *http.Request) (string, bool) {
	owner, refusal := ownership.OwnerOfNewRecord(r)
	if respondRefusal(w, refusal) {
		return "", false
	}
	return owner, true
}

func decode(r *http.Request, into any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<20))
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	return nil
}

func queryInt64(r *http.Request, name string) (int64, error) {
	text := r.URL.Query().Get(name)
	if text == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, errors.Join(ErrInvalid, errors.New(name+" must be unix milliseconds"))
	}
	return value, nil
}

func queryLimit(r *http.Request) (int, error) {
	text := r.URL.Query().Get("limit")
	if text == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(text)
	if err != nil {
		return 0, errors.Join(ErrInvalid, errors.New("limit must be a number"))
	}
	return value, nil
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	if _, refusal := ownership.CallerOfRequest(r); respondRefusal(w, refusal) {
		return
	}
	if err := h.s.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) vocabulary(w http.ResponseWriter, r *http.Request) {
	if _, refusal := ownership.CallerOfRequest(r); respondRefusal(w, refusal) {
		return
	}
	writeJSON(w, http.StatusOK, vocabulary())
}

func (h *handler) summary(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	summary, err := h.s.Summarize(r.Context(), scope)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (h *handler) listMessages(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	after, err := queryInt64(r, "after")
	if err != nil {
		writeError(w, err)
		return
	}
	before, err := queryInt64(r, "before")
	if err != nil {
		writeError(w, err)
		return
	}
	limit, err := queryLimit(r)
	if err != nil {
		writeError(w, err)
		return
	}
	order := query.Get("order")
	if order != "" && order != "oldest" && order != "newest" {
		writeError(w, errors.Join(ErrInvalid, errors.New("order is oldest or newest")))
		return
	}
	page, err := h.s.ListMessages(r.Context(), scope, MessageFilter{ConversationIDs: query["conversation"], PersonID: query.Get("person"), Platform: query.Get("platform"),
		Source: query.Get("source"), SourceRef: query.Get("source_ref"),
		After: after, Before: before, Query: query.Get("q"), Newest: order == "newest", Limit: limit, Cursor: query.Get("cursor")})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// messageRevisions answers every earlier version of a message, oldest first.
func (h *handler) messageRevisions(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	revisions, err := h.s.MessageRevisions(r.Context(), scope, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, revisions)
}

func (h *handler) getMessage(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	message, err := h.s.GetMessage(r.Context(), scope, r.PathValue("id"))
	var superseded ErrSuperseded
	if errors.As(err, &superseded) {
		// Relative, so it holds behind a proxy's prefix.
		w.Header().Set("Location", url.PathEscape(superseded.SupersededBy))
		writeJSON(w, http.StatusMovedPermanently, map[string]string{"error": superseded.Error(), "superseded_by": superseded.SupersededBy})
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, message)
}

func (h *handler) listSupersededMessages(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	limit, err := queryLimit(r)
	if err != nil {
		writeError(w, err)
		return
	}
	superseded, err := h.s.ListSupersededMessages(r.Context(), scope, r.URL.Query().Get("superseded_by"), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, superseded)
}

func (h *handler) listConversationMerges(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	limit, err := queryLimit(r)
	if err != nil {
		writeError(w, err)
		return
	}
	merges, err := h.s.ListConversationMerges(r.Context(), scope, r.URL.Query().Get("merged_into"), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, merges)
}

func (h *handler) listConversations(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	limit, err := queryLimit(r)
	if err != nil {
		writeError(w, err)
		return
	}
	query := r.URL.Query()
	conversations, err := h.s.ListConversations(r.Context(), scope, ConversationFilter{PersonID: query.Get("person"), AccountID: query.Get("account"), Platform: query.Get("platform"), Query: query.Get("q"), Limit: limit})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conversations)
}

func (h *handler) getConversation(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	conversation, err := h.s.GetConversation(r.Context(), scope, r.PathValue("id"))
	var merged ErrMerged
	if errors.As(err, &merged) {
		// Relative, so it holds behind a proxy's prefix.
		w.Header().Set("Location", url.PathEscape(merged.MergedInto))
		writeJSON(w, http.StatusMovedPermanently, map[string]string{"error": merged.Error(), "merged_into": merged.MergedInto})
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conversation)
}

func (h *handler) patchParticipant(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var patch ParticipantPatch
	if err := decode(r, &patch); err != nil {
		writeError(w, err)
		return
	}
	if patch.PersonID == nil {
		writeError(w, errors.Join(ErrInvalid, errors.New("person_id is required (\"\" clears it)")))
		return
	}
	participant, err := h.s.SetParticipantPerson(r.Context(), scope, r.PathValue("id"), r.PathValue("handle"), *patch.PersonID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, participant)
}

func (h *handler) listDays(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	limit, err := queryLimit(r)
	if err != nil {
		writeError(w, err)
		return
	}
	query := r.URL.Query()
	days, err := h.s.ListConversationDays(r.Context(), scope, DayFilter{ConversationID: query.Get("conversation"), JournalEntryID: query.Get("journal_entry"),
		PersonID: query.Get("person"), From: query.Get("from"), To: query.Get("to"), Involved: query.Get("involved") == "true", Limit: limit})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, days)
}

func (h *handler) getDay(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	day, err := h.s.GetConversationDay(r.Context(), scope, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, day)
}

func (h *handler) linkJournalEntries(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var links []JournalEntryLink
	if err := decode(r, &links); err != nil {
		writeError(w, err)
		return
	}
	result, err := h.s.LinkJournalEntries(r.Context(), scope, links)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *handler) activity(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	after, err := queryInt64(r, "after")
	if err != nil {
		writeError(w, err)
		return
	}
	before, err := queryInt64(r, "before")
	if err != nil {
		writeError(w, err)
		return
	}
	query := r.URL.Query()
	buckets, err := h.s.Activity(r.Context(), scope, ActivityFilter{GroupBy: query.Get("group_by"), Interval: query.Get("interval"), TimeZone: query.Get("time_zone"),
		PersonID: query.Get("person"), After: after, Before: before, Involved: query.Get("involved") == "true"})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, buckets)
}

func sortBuckets(buckets []ActivityBucket) {
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Start != buckets[j].Start {
			return buckets[i].Start < buckets[j].Start
		}
		return buckets[i].Key < buckets[j].Key
	})
}

func (h *handler) listImports(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	runs, err := h.s.ListImports(r.Context(), scope)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (h *handler) startImport(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerOfNewRecord(w, r)
	if !ok {
		return
	}
	var start ImportStart
	if err := decode(r, &start); err != nil {
		writeError(w, err)
		return
	}
	run, err := h.s.StartImport(r.Context(), owner, start)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (h *handler) getImport(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	run, err := h.s.GetImport(r.Context(), scope, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (h *handler) importBatch(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var batch ImportBatch
	if err := decode(r, &batch); err != nil {
		writeError(w, err)
		return
	}
	counts, err := h.s.ApplyBatch(r.Context(), scope, r.PathValue("id"), batch)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

func (h *handler) finishImport(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	run, err := h.s.FinishImport(r.Context(), scope, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (h *handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	accounts, err := h.s.ListAccounts(r.Context(), scope)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, accounts)
}

func (h *handler) putAccount(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerOfNewRecord(w, r)
	if !ok {
		return
	}
	var put AccountPut
	if err := decode(r, &put); err != nil {
		writeError(w, err)
		return
	}
	account, err := h.s.PutAccount(r.Context(), owner, put)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, account)
}

func (h *handler) getAccount(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	account, err := h.s.GetAccount(r.Context(), scope, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, account)
}
