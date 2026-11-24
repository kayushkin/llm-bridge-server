package mailstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/accounts"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/backend"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/localmail"
)

// ─── Pushed delivery and the event outbox ──────────────────────────────
//
// A sender that holds mail — Databricks' raw-mail custody, say — pushes it
// here with a credential bound to the accounts it may fill. Each delivery is
// stored with its receipt and its outbox event in one transaction and only
// then acknowledged, so an acknowledged delivery is durable and its event will
// be read. What the mail is for is not decided here: an event says a message
// revision is available, and ticketing (or anything else) reads the outbox,
// decides, and records its own decision.

// IngressTokenHeader carries an ingress credential's token. It is not the
// service bearer: an ingress credential can deliver into its accounts and do
// nothing else.
const IngressTokenHeader = "X-Mailstack-Ingress-Token"

// handleDeliver answers POST /api/ingress/deliveries. Each delivery gets its
// own result, in request order; a delivery that fails does not stop the
// others, and only status failed is worth sending again.
func (s *Server) handleDeliver(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get(IngressTokenHeader)
	if token == "" {
		writeError(w, http.StatusUnauthorized, fmt.Errorf("%s is required", IngressTokenHeader))
		return
	}
	store, err := s.localMailStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	credential, err := store.IngressCredentialByToken(r.Context(), token)
	if errors.Is(err, localmail.ErrCredentialNotFound) {
		writeError(w, http.StatusUnauthorized, errors.New("the ingress token names no live credential"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	var req struct {
		Deliveries []localmail.Delivery `json:"deliveries"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxImportRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("delivery request is over %d bytes; send fewer at once", maxImportRequestBytes))
			return
		}
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode deliveries: %w", err))
		return
	}
	if len(req.Deliveries) == 0 || len(req.Deliveries) > localmail.MaxDeliveryBatch {
		writeError(w, http.StatusBadRequest, fmt.Errorf("send 1 to %d deliveries at once, not %d", localmail.MaxDeliveryBatch, len(req.Deliveries)))
		return
	}

	started := time.Now()
	results := make([]localmail.DeliveryResult, len(req.Deliveries))
	for i := range req.Deliveries {
		results[i] = s.deliverOne(r, store, credential, &req.Deliveries[i])
	}
	setStoreTiming(w, started)
	counts := map[string]int{}
	for _, result := range results {
		counts[result.Status]++
	}
	writeJSON(w, http.StatusOK, map[string]any{"credential_id": credential.ID, "results": results, "counts": counts})
}

func (s *Server) deliverOne(r *http.Request, store *localmail.Store, credential localmail.IngressCredential, d *localmail.Delivery) localmail.DeliveryResult {
	rejected := func(detail string) localmail.DeliveryResult {
		return localmail.DeliveryResult{DeliveryID: d.DeliveryID, AccountID: d.AccountID, Status: localmail.DeliveryRejected, Detail: detail}
	}
	if err := localmail.ValidateDelivery(*d); err != nil {
		return rejected(err.Error())
	}
	if !credential.MayDeliverTo(d.AccountID) {
		return rejected(fmt.Sprintf("credential %s is not bound to account %s", credential.ID, d.AccountID))
	}
	acct, err := s.accounts.Get(d.AccountID)
	if err != nil {
		return rejected(fmt.Sprintf("account %s: %v", d.AccountID, err))
	}
	if acct.Provider != accounts.ProviderLocal {
		return rejected(fmt.Sprintf("account %s is a %s account; deliveries go to local accounts", d.AccountID, acct.Provider))
	}
	m := &d.Message
	var text, html string
	if m.BodyText != nil {
		text = *m.BodyText
	}
	if m.BodyHTML != nil {
		html = *m.BodyHTML
	}
	m.Snippet = localmail.Snippet(accounts.SearchableText(text, html))

	failed := func(err error) localmail.DeliveryResult {
		log.Printf("mailstack: delivery %s from %s into %s failed: %v", d.DeliveryID, credential.Source, d.AccountID, err)
		return localmail.DeliveryResult{DeliveryID: d.DeliveryID, AccountID: d.AccountID, Status: localmail.DeliveryFailed,
			Detail: err.Error(), Retry: true}
	}
	messages := []localmail.ImportedMessage{*m}
	uploaded, status, err := s.uploadChangedAttachments(r.Context(), store, d.AccountID, messages)
	if err != nil {
		if status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge {
			return rejected(err.Error())
		}
		return failed(err)
	}
	d.Message = messages[0]
	result, err := store.Deliver(r.Context(), credential, *d)
	if err != nil {
		if purgeErr := s.purgeAttachmentFiles(r.Context(), uploaded); purgeErr != nil {
			log.Printf("mailstack: delivery %s failed and its uploads could not all be purged: %v", d.DeliveryID, purgeErr)
		}
		return failed(err)
	}
	if err := s.purgeAttachmentFiles(r.Context(), result.UnusedFileIDs()); err != nil {
		log.Printf("mailstack: delivery %s left attachment files it could not purge: %v", d.DeliveryID, err)
	}
	if meta := result.ChangedMeta(); meta != nil {
		s.indexMessages(d.AccountID, []backend.MessageMeta{*meta})
	}
	return result
}

// handleCreateIngressCredential mints a credential bound to local accounts.
// The token is in this answer and nowhere else.
func (s *Server) handleCreateIngressCredential(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source     string   `json:"source"`
		Note       string   `json:"note"`
		AccountIDs []string `json:"account_ids"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode credential: %w", err))
		return
	}
	for _, accountID := range req.AccountIDs {
		if _, status, err := s.localAccountStore(accountID); err != nil {
			writeError(w, status, err)
			return
		}
	}
	store, err := s.localMailStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	credential, token, err := store.CreateIngressCredential(r.Context(), req.Source, req.Note, req.AccountIDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"credential": credential, "token": token,
		"header": IngressTokenHeader, "note": "the token is shown once; store it with the sender"})
}

func (s *Server) handleListIngressCredentials(w http.ResponseWriter, r *http.Request) {
	store, err := s.localMailStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	credentials, err := store.IngressCredentials(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": credentials})
}

func (s *Server) handleRevokeIngressCredential(w http.ResponseWriter, r *http.Request) {
	store, err := s.localMailStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	err = store.RevokeIngressCredential(r.Context(), r.PathValue("id"))
	if errors.Is(err, localmail.ErrCredentialNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": r.PathValue("id")})
}

// cursorOf reads a whole-number query parameter, 0 when absent.
func cursorOf(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s must be a whole number, got %q", name, raw))
		return 0, false
	}
	return n, true
}

// handleListReceipts reads the delivery journal after a receipt id.
func (s *Server) handleListReceipts(w http.ResponseWriter, r *http.Request) {
	after, ok := cursorOf(w, r, "after")
	if !ok {
		return
	}
	limit, ok := cursorOf(w, r, "limit")
	if !ok {
		return
	}
	store, err := s.localMailStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	receipts, err := store.Receipts(r.Context(), after, r.URL.Query().Get("status"), int(limit))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	next := after
	if len(receipts) > 0 {
		next = receipts[len(receipts)-1].ReceiptID
	}
	writeJSON(w, http.StatusOK, map[string]any{"receipts": receipts, "next_after": next})
}

// handleListEvents reads the outbox after a cursor. A consumer keeps the
// last seq it finished and asks from there; seq is also its idempotency key,
// so reading the same event twice must change nothing on its side.
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	after, ok := cursorOf(w, r, "after")
	if !ok {
		return
	}
	limit, ok := cursorOf(w, r, "limit")
	if !ok {
		return
	}
	store, err := s.localMailStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	started := time.Now()
	events, err := store.Events(r.Context(), after, r.URL.Query().Get("account"), int(limit))
	setStoreTiming(w, started)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	next := after
	if len(events) > 0 {
		next = events[len(events)-1].Seq
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "next_after": next})
}

// handlePurgeMessage destroys a message of a local account for retention:
// its attachment files first, then every revision, leaving a tombstone that
// refuses the same source message from any import or delivery again.
func (s *Server) handlePurgeMessage(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account")
	store, status, err := s.localAccountStore(accountID)
	if err != nil {
		writeError(w, status, err)
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, errors.New(`a purge needs {"reason": "…"}`))
		return
	}
	id := r.PathValue("id")
	fileIDs, err := store.AttachmentFileIDs(r.Context(), accountID, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.purgeAttachmentFiles(r.Context(), fileIDs); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("purge attachments of %s first: %w; the message is untouched", id, err))
		return
	}
	err = store.Purge(r.Context(), accountID, id, req.Reason)
	if errors.Is(err, localmail.ErrNotFound) {
		writeCodedError(w, http.StatusNotFound, errorCodeMessageNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"purged": accountID + ":" + id, "attachment_files_purged": len(fileIDs)})
}
