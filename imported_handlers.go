package mailstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/mailstack/accounts"
	"github.com/kayushkin/mailstack/localmail"
)

// ─── Imported mail and split reads ─────────────────────────────────────
//
// These routes serve a caller that keeps its own records pointing at messages
// — a ticket desk linking tickets to the mail they came from — and needs to
// show those messages fast without reading a whole mailbox. They answer only
// for local accounts, where mailstack holds the content itself; for a provider
// account the content lives upstream and GET /api/messages/{id} stays the read.
//
// Each response that reads the store sets Server-Timing with how long the store
// took, so a caller measuring end to end can see how much of it was mailstack.

// maxImportRequestBytes bounds one import request. A batch is at most
// localmail.MaxImportBatch messages; this leaves room for large HTML bodies and
// still refuses a request that would hold the server's memory hostage.
const maxImportRequestBytes = 256 << 20

// maxResolveRefs bounds one resolve call: a page of a conversation, not a
// mailbox.
const maxResolveRefs = 200

// Error codes a caller can branch on without parsing the message.
const (
	errorCodeMessageNotFound = "message_not_found"
	errorCodeBodyAbsent      = "body_absent"
)

func writeCodedError(w http.ResponseWriter, status int, code string, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error(), "code": code})
}

func setStoreTiming(w http.ResponseWriter, started time.Time) {
	w.Header().Set("Server-Timing", fmt.Sprintf("store;dur=%.2f", float64(time.Since(started).Microseconds())/1000))
}

// localAccountStore checks that accountID names a local account and returns
// the store that holds its mail, or the status to answer with.
func (s *Server) localAccountStore(accountID string) (*localmail.Store, int, error) {
	if accountID == "" {
		return nil, http.StatusBadRequest, errors.New("account is required")
	}
	acct, err := s.accounts.Get(accountID)
	if errors.Is(err, accounts.ErrNotFound) {
		return nil, http.StatusNotFound, err
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if acct.Provider != accounts.ProviderLocal {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"account %s is a %s account; imported mail and split reads exist only for local accounts — read it with GET /api/messages/{id}",
			accountID, acct.Provider)
	}
	store, err := s.localMailStore()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return store, 0, nil
}

type importRequest struct {
	Messages []localmail.ImportedMessage `json:"messages"`
	Source   *localmail.ImportSource     `json:"source,omitempty"`
}

// handleImportMessages stores an export's messages in a local account.
//
// The batch is all or nothing: one invalid message refuses the whole request
// with every problem listed, and nothing is stored or checkpointed. Replaying
// a batch is safe, and each message reports whether it was created, updated
// (with the revision it replaced) or unchanged.
func (s *Server) handleImportMessages(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	store, status, err := s.localAccountStore(accountID)
	if err != nil {
		writeError(w, status, err)
		return
	}
	var req importRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxImportRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf(
				"import request is over %d bytes; split it into smaller batches", maxImportRequestBytes))
			return
		}
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode import: %w", err))
		return
	}
	for i := range req.Messages {
		m := &req.Messages[i]
		var text, html string
		if m.BodyText != nil {
			text = *m.BodyText
		}
		if m.BodyHTML != nil {
			html = *m.BodyHTML
		}
		m.Snippet = localmail.Snippet(accounts.SearchableText(text, html))
	}

	started := time.Now()
	outcomes, changed, err := store.Import(r.Context(), accountID, req.Messages, req.Source)
	setStoreTiming(w, started)
	var invalid *localmail.ImportValidationError
	if errors.As(err, &invalid) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": invalid.Error(), "invalid": invalid.Items})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Index what changed exactly as an append does, so an imported message is
	// answerable by Message-ID and joins its thread at once.
	s.indexMessages(accountID, changed)

	counts := map[string]int{localmail.ImportCreated: 0, localmail.ImportUpdated: 0, localmail.ImportUnchanged: 0}
	for _, o := range outcomes {
		counts[o.Outcome]++
	}
	writeJSON(w, http.StatusOK, map[string]any{"outcomes": outcomes, "counts": counts, "source": req.Source})
}

// handleListImportSources lists the sources imported into an account, so an
// importer can skip every source whose sha256 it already sees here.
func (s *Server) handleListImportSources(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	store, status, err := s.localAccountStore(accountID)
	if err != nil {
		writeError(w, status, err)
		return
	}
	started := time.Now()
	sources, err := store.ImportSources(r.Context(), accountID)
	setStoreTiming(w, started)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}

// resolveRef names one message, by locator ("<account_id>:<id>", the form
// kanban-store's email entity ref holds) or by the id its source gave it.
type resolveRef struct {
	Locator           string `json:"locator,omitempty"`
	AccountID         string `json:"account_id,omitempty"`
	ProviderMessageID string `json:"provider_message_id,omitempty"`
}

// Resolve statuses. Every ref gets exactly one.
const (
	resolveFound           = "found"
	resolveNotFound        = "not_found"
	resolveInvalid         = "invalid"
	resolveUnknownAccount  = "unknown_account"
	resolveNotLocalAccount = "not_local_account"
	resolveError           = "error"
)

type resolveResult struct {
	Ref     resolveRef               `json:"ref"`
	Status  string                   `json:"status"`
	Error   string                   `json:"error,omitempty"`
	ETag    string                   `json:"etag,omitempty"`
	Message *localmail.StoredMessage `json:"message,omitempty"`
}

// handleResolveMessages reads the metadata of a bounded batch of messages in
// one call, with an outcome per ref in request order. It never reads a body.
// The request succeeds when the batch is well formed; a ref that cannot be
// answered says why in its own result.
func (s *Server) handleResolveMessages(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Refs []resolveRef `json:"refs"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode resolve: %w", err))
		return
	}
	if len(req.Refs) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("refs is required"))
		return
	}
	if len(req.Refs) > maxResolveRefs {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%d refs in one resolve; the most is %d", len(req.Refs), maxResolveRefs))
		return
	}

	type accountAnswer struct {
		status string
		err    error
	}
	accountChecked := map[string]accountAnswer{}
	checkAccount := func(accountID string) accountAnswer {
		if answer, ok := accountChecked[accountID]; ok {
			return answer
		}
		var answer accountAnswer
		_, code, err := s.localAccountStore(accountID)
		switch {
		case err == nil:
		case code == http.StatusNotFound:
			answer = accountAnswer{resolveUnknownAccount, err}
		case code == http.StatusBadRequest:
			answer = accountAnswer{resolveNotLocalAccount, err}
		default:
			answer = accountAnswer{resolveError, err}
		}
		accountChecked[accountID] = answer
		return answer
	}

	started := time.Now()
	results := make([]resolveResult, len(req.Refs))
	for i, ref := range req.Refs {
		results[i] = resolveResult{Ref: ref}
		accountID, id, providerID, err := ref.parts()
		if err != nil {
			results[i].Status, results[i].Error = resolveInvalid, err.Error()
			continue
		}
		if answer := checkAccount(accountID); answer.err != nil {
			results[i].Status, results[i].Error = answer.status, answer.err.Error()
			continue
		}
		store, err := s.localMailStore()
		if err != nil {
			results[i].Status, results[i].Error = resolveError, err.Error()
			continue
		}
		var stored localmail.StoredMessage
		if id != "" {
			stored, err = store.Metadata(r.Context(), accountID, id)
		} else {
			stored, err = store.MetadataByProviderID(r.Context(), accountID, providerID)
		}
		switch {
		case errors.Is(err, localmail.ErrNotFound):
			results[i].Status = resolveNotFound
		case err != nil:
			results[i].Status, results[i].Error = resolveError, err.Error()
		default:
			results[i].Status, results[i].ETag, results[i].Message = resolveFound, stored.MetadataETag(), &stored
		}
	}
	setStoreTiming(w, started)
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// parts splits a ref into the account and exactly one of the store id or the
// provider id.
func (ref resolveRef) parts() (accountID, id, providerID string, err error) {
	switch {
	case ref.Locator != "" && (ref.AccountID != "" || ref.ProviderMessageID != ""):
		return "", "", "", errors.New("give either locator or account_id with provider_message_id, not both")
	case ref.Locator != "":
		accountID, id, ok := strings.Cut(ref.Locator, ":")
		if !ok || accountID == "" || id == "" {
			return "", "", "", fmt.Errorf("locator %q is not <account_id>:<id>", ref.Locator)
		}
		return accountID, id, "", nil
	case ref.AccountID != "" && ref.ProviderMessageID != "":
		return ref.AccountID, "", ref.ProviderMessageID, nil
	default:
		return "", "", "", errors.New("a ref needs locator, or account_id and provider_message_id")
	}
}

// ifNoneMatch reports whether the request's If-None-Match already names etag.
func ifNoneMatch(r *http.Request, etag string) bool {
	header := r.Header.Get("If-None-Match")
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

// handleGetMessageMetadata reads one message without its bodies: the fields,
// participants, source location, revision, and which bodies exist and how big
// they are.
func (s *Server) handleGetMessageMetadata(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account")
	store, status, err := s.localAccountStore(accountID)
	if err != nil {
		writeError(w, status, err)
		return
	}
	started := time.Now()
	stored, err := store.Metadata(r.Context(), accountID, r.PathValue("id"))
	setStoreTiming(w, started)
	if errors.Is(err, localmail.ErrNotFound) {
		writeCodedError(w, http.StatusNotFound, errorCodeMessageNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	etag := stored.MetadataETag()
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if ifNoneMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

// handleGetMessageBody serves one body representation whole, as its own media
// type rather than inside JSON, so a 250 KB newsletter costs its own bytes and
// nothing more.
//
// The HTML is the message's own, unsanitized: sanitizing belongs to whatever
// renders it, which knows its own rules. So that opening this URL directly in a
// browser cannot run the message's scripts or load its trackers, the response
// carries a sandboxing Content-Security-Policy and nosniff.
//
// 404 has two meanings, told apart by "code": message_not_found, and
// body_absent for a message that has no body in this format. A present body
// that is empty is a 200 with no bytes.
func (s *Server) handleGetMessageBody(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account")
	store, status, err := s.localAccountStore(accountID)
	if err != nil {
		writeError(w, status, err)
		return
	}
	format := localmail.BodyFormat(r.URL.Query().Get("format"))
	var contentType string
	switch format {
	case localmail.BodyFormatText:
		contentType = "text/plain; charset=utf-8"
	case localmail.BodyFormatHTML:
		contentType = "text/html; charset=utf-8"
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("format must be text or html, got %q", format))
		return
	}

	started := time.Now()
	body, stored, err := store.Body(r.Context(), accountID, r.PathValue("id"), format)
	setStoreTiming(w, started)
	switch {
	case errors.Is(err, localmail.ErrNotFound):
		writeCodedError(w, http.StatusNotFound, errorCodeMessageNotFound, err)
		return
	case errors.Is(err, localmail.ErrBodyAbsent):
		writeCodedError(w, http.StatusNotFound, errorCodeBodyAbsent, fmt.Errorf(
			"message %s has no %s body", stored.ID, format))
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	etag := stored.BodyETag(format)
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "private, no-cache")
	h.Set("X-Mailstack-Revision", strconv.FormatInt(stored.Revision, 10))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	if ifNoneMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.WriteString(w, body); err != nil {
		log.Printf("mailstack: writing %s body of message %s: %v", format, stored.ID, err)
	}
}
