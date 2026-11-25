package mailstack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"

	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/accounts"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/backend"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/filestore"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/localmail"
)

// Server is mailstack's HTTP surface, mounted by llm-bridge-server, which
// says who each request is from in its context (ContextWithCaller).
type Server struct {
	accounts *accounts.Store
	// tokens fetches Gmail and Outlook access tokens from auth-store.
	tokens backend.TokenSource
	mux    *http.ServeMux

	// providerConnectionsEnabled is whether Gmail, Outlook and IMAP accounts
	// are reached at all. Off, every read or change on one answers 503 and
	// no connection to a mail provider is made; local accounts work either
	// way.
	providerConnectionsEnabled bool

	imapMu       sync.Mutex
	imapBackends map[string]*backend.Imap

	// localStore holds the mail of local accounts.
	localStore *localmail.Store

	// files holds imported attachments' bytes. Nil refuses attachments on
	// local accounts with 503.
	files *filestore.Client

	// people is where a person's access is read. Nil refuses every request
	// that names a principal; see principal_access.go.
	people PeopleDirectory
}

// ServerParts is what a Server is built from.
type ServerParts struct {
	// Accounts and LocalMail are the two halves of the mail module. Both
	// are required.
	Accounts  *accounts.Store
	LocalMail *localmail.Store
	// Tokens fetches provider access tokens. Required when provider
	// connections are on.
	Tokens backend.TokenSource
	// ProviderConnectionsEnabled lets Gmail, Outlook and IMAP accounts reach
	// their providers.
	ProviderConnectionsEnabled bool
	// Files keeps imported attachments' bytes; nil refuses attachments.
	Files *filestore.Client
	// People answers who a principal is and what it was granted; nil refuses
	// every request that names a person.
	People PeopleDirectory
}

// NewServer builds the server. It panics on a part that is required and
// missing, at boot, rather than at the first request that needs it.
func NewServer(parts ServerParts) *Server {
	if parts.Accounts == nil || parts.LocalMail == nil {
		panic("mailstack: NewServer needs both halves of the mail module")
	}
	if parts.ProviderConnectionsEnabled && parts.Tokens == nil {
		panic("mailstack: provider connections are on, and there is no token source to reach Gmail and Outlook with")
	}
	s := &Server{
		accounts:                   parts.Accounts,
		tokens:                     parts.Tokens,
		providerConnectionsEnabled: parts.ProviderConnectionsEnabled,
		localStore:                 parts.LocalMail,
		files:                      parts.Files,
		people:                     parts.People,
		mux:                        http.NewServeMux(),
		imapBackends:               map[string]*backend.Imap{},
	}
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.Handle("GET /api/accounts", s.routeFor(ruleHandlerFilters, 0, s.handleListAccounts))
	s.mux.Handle("POST /api/accounts", s.routeFor(ruleServicesOnly, 0, s.handleCreateAccount))
	s.mux.Handle("GET /api/accounts/{id}", s.routeFor(ruleReadAccount, accountInPath, s.handleGetAccount))
	s.mux.Handle("PATCH /api/accounts/{id}", s.routeFor(ruleServicesOnly, 0, s.handlePatchAccount))
	s.mux.Handle("DELETE /api/accounts/{id}", s.routeFor(ruleServicesOnly, 0, s.handleDeleteAccount))
	// Appending a message is only meaningful for an account this host owns. It
	// is scoped under the account rather than sitting at /api/messages because
	// it is not a mail operation that any provider could answer — see the
	// handler.
	s.mux.Handle("POST /api/accounts/{id}/messages", s.routeFor(ruleImportAccount, accountInPath, s.handleAppendLocalMessage))
	// Imported mail: an export's messages put into a local account by the id
	// the source gave them, idempotently. See imported_handlers.go.
	s.mux.Handle("POST /api/accounts/{id}/messages/import", s.routeFor(ruleImportAccount, accountInPath, s.handleImportMessages))
	s.mux.Handle("GET /api/accounts/{id}/import-sources", s.routeFor(ruleImportAccount, accountInPath, s.handleListImportSources))
	s.mux.Handle("GET /api/imap/creds", s.routeFor(ruleServicesOnly, 0, s.handleListImapCreds))
	s.mux.Handle("POST /api/imap/creds", s.routeFor(ruleServicesOnly, 0, s.handleSaveImapCred))
	s.mux.Handle("GET /api/imap/creds/{name}", s.routeFor(ruleServicesOnly, 0, s.handleGetImapCred))
	s.mux.Handle("PUT /api/imap/creds/{name}", s.routeFor(ruleServicesOnly, 0, s.handleSaveImapCred))
	s.mux.Handle("DELETE /api/imap/creds/{name}", s.routeFor(ruleServicesOnly, 0, s.handleDeleteImapCred))
	s.mux.Handle("GET /api/folders", s.routeFor(ruleReadAccount, accountInQuery, s.handleFolders))
	s.mux.Handle("GET /api/messages", s.routeFor(ruleReadAccount, accountInQuery, s.handleListMessages))
	// Resolution lives under its own /api/lookup/ namespace rather than under
	// /api/messages/. Both spellings are four segments, so
	// "/api/messages/by-message-id/{messageID}" and "/api/messages/{id}/thread"
	// are genuinely ambiguous for the path "/api/messages/by-message-id/thread"
	// and Go's mux panics at registration rather than guessing. Caught by the
	// e2e smoke, which exists for precisely this.
	s.mux.Handle("GET /api/lookup/message-id/{messageID}", s.routeFor(ruleServicesOnly, 0, s.handleLocatorsForMessageID))
	// Search runs over recorded payloads only (see accounts.MessageBody): a
	// message never fetched through GET /api/messages/{id} is invisible to it.
	// /api/index/stats reports the coverage gap as messages − bodies_stored.
	s.mux.Handle("GET /api/search", s.routeFor(ruleServicesOnly, 0, s.handleSearchMessages))
	s.mux.Handle("POST /api/search/similar", s.routeFor(ruleServicesOnly, 0, s.handleSimilarMessages))
	s.mux.Handle("GET /api/messages/{id}/thread", s.routeFor(ruleReadAccount, accountInQuery, s.handleMessageThread))
	s.mux.Handle("GET /api/index/stats", s.routeFor(ruleServicesOnly, 0, s.handleIndexStats))
	s.mux.Handle("GET /api/messages/{id}", s.routeFor(ruleReadMessage, accountInQuery, s.handleGetMessage))
	// Split reads for local accounts: metadata without bodies, one body format
	// at a time, and a bounded batch of metadata in one call.
	s.mux.Handle("GET /api/messages/{id}/metadata", s.routeFor(ruleReadMessage, accountInQuery, s.handleGetMessageMetadata))
	s.mux.Handle("GET /api/messages/{id}/body", s.routeFor(ruleReadMessage, accountInQuery, s.handleGetMessageBody))
	s.mux.Handle("GET /api/messages/{id}/revisions", s.routeFor(ruleReadMessage, accountInQuery, s.handleListRevisions))
	s.mux.Handle("GET /api/messages/{id}/revisions/{revision}", s.routeFor(ruleReadMessage, accountInQuery, s.handleGetRevisionMetadata))
	s.mux.Handle("GET /api/messages/{id}/revisions/{revision}/body", s.routeFor(ruleReadMessage, accountInQuery, s.handleGetRevisionBody))
	s.mux.Handle("GET /api/messages/{id}/revisions/{revision}/attachments/{attachment_id}", s.routeFor(ruleReadMessage, accountInQuery, s.handleGetRevisionAttachment))
	s.mux.Handle("POST /api/messages/resolve", s.routeFor(ruleHandlerFilters, 0, s.handleResolveMessages))
	s.mux.Handle("GET /api/messages/{id}/attachments/{attachment_id}", s.routeFor(ruleReadMessage, accountInQuery, s.handleGetAttachment))
	s.mux.Handle("POST /api/messages/{id}/read", s.routeFor(ruleImportAccount, accountInQuery, s.handleMarkRead))
	s.mux.Handle("POST /api/messages/{id}/flag", s.routeFor(ruleImportAccount, accountInQuery, s.handleFlag))
	s.mux.Handle("POST /api/messages/{id}/move", s.routeFor(ruleImportAccount, accountInQuery, s.handleMove))
	s.mux.Handle("DELETE /api/messages/{id}", s.routeFor(ruleImportAccount, accountInQuery, s.handleDelete))
	s.mux.Handle("POST /api/messages/{id}/perm-delete", s.routeFor(ruleServicesOnly, 0, s.handlePermDelete))
	s.mux.Handle("POST /api/messages/bulk", s.routeFor(ruleImportAccount, accountInQuery, s.handleBulk))
	// Pushed delivery authenticates with an ingress credential, not a caller
	// the mounting server verified, so it is the one /api/ route outside
	// routeFor: an ingress token can deliver into its accounts and do nothing
	// else.
	s.mux.HandleFunc("POST /api/ingress/deliveries", s.handleDeliver)
	s.mux.Handle("POST /api/ingress/credentials", s.routeFor(ruleServicesOnly, 0, s.handleCreateIngressCredential))
	s.mux.Handle("GET /api/ingress/credentials", s.routeFor(ruleServicesOnly, 0, s.handleListIngressCredentials))
	s.mux.Handle("POST /api/ingress/credentials/{id}/revoke", s.routeFor(ruleServicesOnly, 0, s.handleRevokeIngressCredential))
	s.mux.Handle("GET /api/ingress/receipts", s.routeFor(ruleServicesOnly, 0, s.handleListReceipts))
	s.mux.Handle("GET /api/events", s.routeFor(ruleServicesOnly, 0, s.handleListEvents))
	s.mux.Handle("POST /api/messages/{id}/purge", s.routeFor(ruleServicesOnly, 0, s.handlePurgeMessage))
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ─── Accounts ──────────────────────────────────────────────────────────

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := s.accounts.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// A person sees the accounts they hold a mail grant on. A message read
	// through a ticket does not show its account here: the ticket grants the
	// message, not the mailbox.
	if access := principalAccessOfRequest(r); access != nil && !access.administrator {
		visible := list[:0:0]
		for _, a := range list {
			if access.mayReadAccount(a.ID) {
				visible = append(visible, a)
			}
		}
		list = visible
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": list})
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var a accounts.Account
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if a.ID == "" || a.Label == "" || a.Provider == "" || a.Address == "" {
		writeError(w, http.StatusBadRequest, errors.New("id, label, provider, address are required"))
		return
	}
	if !a.Enabled {
		a.Enabled = true
	}
	if a.Color == "" {
		a.Color = "#888"
	}
	if err := s.accounts.Save(&a); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.accounts.Get(id)
	if errors.Is(err, accounts.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handlePatchAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.accounts.Get(id)
	if errors.Is(err, accounts.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var patch struct {
		Label     *string `json:"label"`
		Color     *string `json:"color"`
		Signature *string `json:"signature"`
		Enabled   *bool   `json:"enabled"`
		SortOrder *int    `json:"sort_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if patch.Label != nil {
		existing.Label = *patch.Label
	}
	if patch.Color != nil {
		existing.Color = *patch.Color
	}
	if patch.Signature != nil {
		existing.Signature = *patch.Signature
	}
	if patch.Enabled != nil {
		existing.Enabled = *patch.Enabled
	}
	if patch.SortOrder != nil {
		existing.SortOrder = *patch.SortOrder
	}
	if err := s.accounts.Save(existing); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.accounts.Delete(id)
	if errors.Is(err, accounts.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Tear down any cached IMAP backend bound to this account.
	s.imapMu.Lock()
	if b, ok := s.imapBackends[id]; ok {
		_ = b.Close()
		delete(s.imapBackends, id)
	}
	s.imapMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// ─── IMAP credentials ──────────────────────────────────────────────────

func (s *Server) handleListImapCreds(w http.ResponseWriter, r *http.Request) {
	list, err := s.accounts.ListImapCreds()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Strip passwords from list responses.
	type safeCred struct {
		Name     string           `json:"name"`
		Address  string           `json:"address"`
		ImapHost string           `json:"imap_host"`
		ImapPort int              `json:"imap_port"`
		SmtpHost string           `json:"smtp_host"`
		SmtpPort int              `json:"smtp_port"`
		Username string           `json:"username"`
		TLSMode  accounts.TLSMode `json:"tls_mode"`
	}
	out := make([]safeCred, 0, len(list))
	for _, c := range list {
		out = append(out, safeCred{c.Name, c.Address, c.ImapHost, c.ImapPort, c.SmtpHost, c.SmtpPort, c.Username, c.TLSMode})
	}
	writeJSON(w, http.StatusOK, map[string]any{"imap_creds": out})
}

func (s *Server) handleGetImapCred(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, err := s.accounts.GetImapCred(name)
	if errors.Is(err, accounts.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Do not expose password through GET.
	c.Password = ""
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleSaveImapCred(w http.ResponseWriter, r *http.Request) {
	var c accounts.ImapCred
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if name := r.PathValue("name"); name != "" {
		c.Name = name
	}
	if c.Name == "" || c.ImapHost == "" || c.ImapPort == 0 || c.Username == "" || c.Password == "" {
		writeError(w, http.StatusBadRequest, errors.New("name, imap_host, imap_port, username, password are required"))
		return
	}
	if c.TLSMode == "" {
		c.TLSMode = accounts.TLSModeTLS
	}
	if err := s.accounts.SaveImapCred(&c); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Drop any cached IMAP backends that might have used a stale cred copy.
	s.imapMu.Lock()
	for id, b := range s.imapBackends {
		_ = b.Close()
		delete(s.imapBackends, id)
	}
	s.imapMu.Unlock()
	c.Password = ""
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleDeleteImapCred(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.accounts.DeleteImapCred(name); err != nil {
		if errors.Is(err, accounts.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── Mail ops ──────────────────────────────────────────────────────────

func (s *Server) backendFor(accountID string) (backend.Backend, *accounts.Account, error) {
	a, err := s.accounts.Get(accountID)
	if err != nil {
		return nil, nil, err
	}
	if a.Provider != accounts.ProviderLocal && !s.providerConnectionsEnabled {
		return nil, a, fmt.Errorf("%w: account %s is %s", ErrProviderConnectionsOff, a.ID, a.Provider)
	}
	switch a.Provider {
	case accounts.ProviderGoogle:
		apiauthProvider := a.ApiauthProvider
		if apiauthProvider == "" {
			apiauthProvider = "google"
		}
		apiauthAccount := a.ApiauthAccount
		if apiauthAccount == "" {
			apiauthAccount = "default"
		}
		return backend.NewGmail(a.ID, apiauthProvider, apiauthAccount, s.tokens), a, nil
	case accounts.ProviderMicrosoft:
		apiauthProvider := a.ApiauthProvider
		if apiauthProvider == "" {
			apiauthProvider = "microsoft"
		}
		apiauthAccount := a.ApiauthAccount
		if apiauthAccount == "" {
			apiauthAccount = "default"
		}
		return backend.NewGraph(a.ID, apiauthProvider, apiauthAccount, s.tokens), a, nil
	case accounts.ProviderIMAP:
		b, err := s.imapBackendFor(a)
		if err != nil {
			return nil, a, err
		}
		return b, a, nil
	case accounts.ProviderLocal:
		store, err := s.localMailStore()
		if err != nil {
			return nil, a, err
		}
		return backend.NewLocal(a.ID, store, s.purgeAttachmentFiles), a, nil
	default:
		return nil, a, fmt.Errorf("unknown provider %q", a.Provider)
	}
}

// ErrProviderConnectionsOff is a read or change on a Gmail, Outlook or IMAP
// account while this server makes no connection to mail providers.
var ErrProviderConnectionsOff = errors.New("this server makes no connection to mail providers (mailstack.provider_connections.enabled is off), so only local accounts answer")

// errorCodeProviderConnectionsOff is the 503 code of ErrProviderConnectionsOff.
const errorCodeProviderConnectionsOff = "mail_provider_connections_off"

// writeBackendError answers a backendFor that failed: 503 for an account whose
// provider this server does not connect to, 400 for an unknown or misconfigured
// account, as before.
func writeBackendError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrProviderConnectionsOff) {
		writeCodedError(w, http.StatusServiceUnavailable, errorCodeProviderConnectionsOff, err)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

func (s *Server) imapBackendFor(a *accounts.Account) (*backend.Imap, error) {
	s.imapMu.Lock()
	defer s.imapMu.Unlock()
	if b, ok := s.imapBackends[a.ID]; ok {
		return b, nil
	}
	if a.ImapCredName == "" {
		return nil, fmt.Errorf("account %s has no imap_cred_name", a.ID)
	}
	cred, err := s.accounts.GetImapCred(a.ImapCredName)
	if err != nil {
		return nil, fmt.Errorf("imap cred %q: %w", a.ImapCredName, err)
	}
	cfg := backend.ImapConfig{
		Address:  cred.Address,
		ImapHost: cred.ImapHost,
		ImapPort: cred.ImapPort,
		Username: cred.Username,
		Password: cred.Password,
		TLSMode:  string(cred.TLSMode),
	}
	b := backend.NewImap(a.ID, cfg, s.accounts)
	s.imapBackends[a.ID] = b
	return b, nil
}

func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	acct := r.URL.Query().Get("account")
	if acct == "" {
		writeError(w, http.StatusBadRequest, errors.New("account query param required"))
		return
	}
	b, _, err := s.backendFor(acct)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	folders, err := b.Folders(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": folders})
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	acct := r.URL.Query().Get("account")
	folder := r.URL.Query().Get("folder")
	cursor := r.URL.Query().Get("cursor")
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	if acct == "" {
		writeError(w, http.StatusBadRequest, errors.New("account query param required"))
		return
	}
	b, _, err := s.backendFor(acct)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	var msgs []backend.MessageMeta
	var next string
	if query := r.URL.Query().Get("q"); query != "" {
		searcher, ok := b.(backend.Searcher)
		if !ok {
			writeError(w, http.StatusNotImplemented, fmt.Errorf("account %s's provider has no search here; q works on Gmail accounts", acct))
			return
		}
		if folder != "" {
			writeError(w, http.StatusBadRequest, errors.New("q searches every folder; send q or folder, not both (Gmail syntax: label:… inside q)"))
			return
		}
		msgs, next, err = searcher.Search(r.Context(), query, cursor, limit)
	} else {
		msgs, next, err = b.List(r.Context(), folder, cursor, limit)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.indexMessages(acct, msgs)
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "next_cursor": next})
}

func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	acct := r.URL.Query().Get("account")
	id := r.PathValue("id")
	if acct == "" {
		writeError(w, http.StatusBadRequest, errors.New("account query param required"))
		return
	}
	b, _, err := s.backendFor(acct)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	msg, err := b.Get(r.Context(), r.URL.Query().Get("folder"), id)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	// The get path carries headers the list path does not, so this is where a
	// message's References chain usually first becomes known — and the only
	// place its body passes through, so this is also where the payload is
	// recorded for search.
	s.recordFullMessage(acct, msg)
	writeJSON(w, http.StatusOK, msg)
}

// handleGetAttachment answers one attachment's bytes, with the content type
// the provider gave the part. attachment_id is an id from the message's
// attachments list. Only Gmail implements it so far; the other backends
// answer an error saying so.
func (s *Server) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	acct := r.URL.Query().Get("account")
	if acct == "" {
		writeError(w, http.StatusBadRequest, errors.New("account query param required"))
		return
	}
	b, a, err := s.backendFor(acct)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	if a.Provider == accounts.ProviderLocal {
		s.serveLocalAttachment(w, r, acct, r.PathValue("id"), 0, r.PathValue("attachment_id"))
		return
	}
	body, contentType, err := b.Attachment(r.Context(), r.PathValue("id"), r.PathValue("attachment_id"))
	if errors.Is(err, backend.ErrAttachmentNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", contentType)
	if _, err := io.Copy(w, body); err != nil {
		log.Printf("mailstack: writing attachment %s of message %s: %v", r.PathValue("attachment_id"), r.PathValue("id"), err)
	}
}

// ─── Write ops ─────────────────────────────────────────────────────────

func (s *Server) backendForReq(w http.ResponseWriter, r *http.Request) (backend.Backend, string, bool) {
	acct := r.URL.Query().Get("account")
	if acct == "" {
		writeError(w, http.StatusBadRequest, errors.New("account query param required"))
		return nil, "", false
	}
	b, _, err := s.backendFor(acct)
	if err != nil {
		writeBackendError(w, err)
		return nil, "", false
	}
	return b, acct, true
}

func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.backendForReq(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var body struct {
		Read bool `json:"read"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := b.MarkRead(r.Context(), id, body.Read); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleFlag(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.backendForReq(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var body struct {
		Flagged bool `json:"flagged"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := b.Flag(r.Context(), id, body.Flagged); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.backendForReq(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var body struct {
		Folder string `json:"folder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Folder == "" {
		writeError(w, http.StatusBadRequest, errors.New("folder is required"))
		return
	}
	if err := b.Move(r.Context(), id, body.Folder); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.backendForReq(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := b.Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePermDelete(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.backendForReq(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := b.PermDelete(r.Context(), id); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleBulk applies a single op to many messages on one account, fan-out
// concurrently. Per-message failures are collected into errors[]; the whole
// call does NOT fail unless the account/op is bad up front.
func (s *Server) handleBulk(w http.ResponseWriter, r *http.Request) {
	b, _, ok := s.backendForReq(w, r)
	if !ok {
		return
	}
	var body struct {
		IDs    []string `json:"ids"`
		Op     string   `json:"op"` // read|unread|flag|unflag|move|delete|perm-delete
		Folder string   `json:"folder,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(body.IDs) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("ids is required"))
		return
	}
	apply := bulkOpFunc(b, body.Op, body.Folder)
	if apply == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown op %q", body.Op))
		return
	}

	type result struct {
		ID    string `json:"id"`
		Error string `json:"error,omitempty"`
	}
	out := make([]result, len(body.IDs))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, id := range body.IDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i].ID = id
			if err := apply(r.Context(), id); err != nil {
				out[i].Error = err.Error()
			}
		}(i, id)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

func bulkOpFunc(b backend.Backend, op, folder string) func(context.Context, string) error {
	switch op {
	case "read":
		return func(ctx context.Context, id string) error { return b.MarkRead(ctx, id, true) }
	case "unread":
		return func(ctx context.Context, id string) error { return b.MarkRead(ctx, id, false) }
	case "flag":
		return func(ctx context.Context, id string) error { return b.Flag(ctx, id, true) }
	case "unflag":
		return func(ctx context.Context, id string) error { return b.Flag(ctx, id, false) }
	case "move":
		if folder == "" {
			return nil
		}
		return func(ctx context.Context, id string) error { return b.Move(ctx, id, folder) }
	case "delete":
		return func(ctx context.Context, id string) error { return b.Delete(ctx, id) }
	case "perm-delete":
		return func(ctx context.Context, id string) error { return b.PermDelete(ctx, id) }
	default:
		return nil
	}
}

// ─── Helpers ───────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Printf("json encode: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// Shutdown closes all persistent IMAP connections.
func (s *Server) Shutdown(ctx context.Context) error {
	s.imapMu.Lock()
	for id, b := range s.imapBackends {
		_ = b.Close()
		delete(s.imapBackends, id)
	}
	s.imapMu.Unlock()

	return nil
}

// ============================ message index ============================

// providerIDScope reports the folder a provider's message ids are scoped to.
//
// Gmail and Graph ids identify a message across the whole account, so they get
// an empty scope and one row per message. An IMAP UID is unique only within one
// mailbox and is invalidated by a UIDVALIDITY change, so its rows are keyed per
// folder. Getting this wrong would silently merge two different IMAP messages
// that happen to share a UID in different folders.
func providerIDScope(provider accounts.Provider, folderID string) string {
	if provider == accounts.ProviderIMAP {
		return folderID
	}
	return ""
}

// indexMessages records a page of list results.
//
// Index failures are logged and swallowed rather than returned. This is a
// deliberate exception to failing loudly, and the reason is narrow: the index is
// derived data that this request did not need in order to succeed. The caller
// asked for their mail and their mail is in hand; failing the read because a
// bookkeeping write went wrong would turn a degraded index into an outage. The
// log line is the alarm, and /api/index/stats is how coverage stays visible.
func (s *Server) indexMessages(accountID string, msgs []backend.MessageMeta) {
	if len(msgs) == 0 {
		return
	}
	a, err := s.accounts.Get(accountID)
	if err != nil {
		log.Printf("index: account %s: %v — page not indexed", accountID, err)
		return
	}
	rows := make([]accounts.IndexedMessage, 0, len(msgs))
	for _, m := range msgs {
		rows = append(rows, indexRow(a, m, nil))
	}
	if err := s.accounts.UpsertMessages(rows); err != nil {
		log.Printf("index: upsert %d message(s) for %s: %v", len(rows), accountID, err)
	}
}

// recordFullMessage stores everything a full fetch teaches: the identity row
// in the message index, and the payload in message_bodies so search can see
// the text. Named record rather than index because since migration 0002 it
// persists content, not just metadata.
func (s *Server) recordFullMessage(accountID string, msg backend.Message) {
	a, err := s.accounts.Get(accountID)
	if err != nil {
		log.Printf("index: account %s: %v — message not indexed", accountID, err)
		return
	}
	row := indexRow(a, msg.Meta, msg.Headers)
	if err := s.accounts.UpsertMessages([]accounts.IndexedMessage{row}); err != nil {
		log.Printf("index: upsert message %s for %s: %v", msg.Meta.ID, accountID, err)
	}
	if err := s.accounts.UpsertMessageBody(accounts.MessageBody{
		AccountID:   row.AccountID,
		FolderScope: row.FolderScope,
		ProviderID:  row.ProviderID,
		MessageID:   row.MessageID,
		Subject:     msg.Meta.Subject,
		FromName:    msg.Meta.From.Name,
		FromEmail:   msg.Meta.From.Email,
		DateMs:      row.DateMs,
		BodyText:    msg.BodyText,
		BodyHTML:    msg.BodyHTML,
		SearchText:  accounts.SearchableText(msg.BodyText, msg.BodyHTML),
	}); err != nil {
		log.Printf("index: store body %s for %s: %v", msg.Meta.ID, accountID, err)
	}
}

// indexRow converts one message into an index row, preferring header values
// when they are available because the get path carries more than the list path.
func indexRow(a *accounts.Account, m backend.MessageMeta, headers map[string]string) accounts.IndexedMessage {
	messageID := m.MessageID
	inReplyTo := m.InReplyTo
	refs := m.References
	if headers != nil {
		if v := backend.HeaderLookup(headers, "Message-ID"); v != "" {
			messageID = backend.StripAngleBrackets(v)
		}
		if v := backend.HeaderLookup(headers, "In-Reply-To"); v != "" {
			inReplyTo = backend.StripAngleBrackets(v)
		}
		if v := backend.HeaderLookup(headers, "References"); v != "" {
			refs = backend.SplitReferences(v)
		}
	}
	var dateMs int64
	if !m.Date.IsZero() {
		dateMs = m.Date.UnixMilli()
	}
	return accounts.IndexedMessage{
		AccountID:   a.ID,
		FolderScope: providerIDScope(a.Provider, m.FolderID),
		ProviderID:  m.ID,
		MessageID:   messageID,
		ThreadID:    m.ThreadID,
		RootID:      rootMessageID(messageID, inReplyTo, refs),
		InReplyTo:   inReplyTo,
		References:  refs,
		Subject:     m.Subject,
		FromName:    m.From.Name,
		FromEmail:   m.From.Email,
		DateMs:      dateMs,
		FolderID:    m.FolderID,
	}
}

// rootMessageID is the conversation's universal key: the oldest ancestor this
// message knows about.
//
// References is ordered oldest first per RFC 5322 §3.6.4, so its first entry is
// the thread root. A message with no ancestors is its own root, which is what
// makes a single message and a two-message thread answerable by the same query.
func rootMessageID(messageID, inReplyTo string, refs []string) string {
	if len(refs) > 0 {
		return refs[0]
	}
	if inReplyTo != "" {
		return inReplyTo
	}
	return messageID
}

func (s *Server) handleLocatorsForMessageID(w http.ResponseWriter, r *http.Request) {
	id := backend.StripAngleBrackets(r.PathValue("messageID"))
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("message id required"))
		return
	}
	locs, err := s.accounts.LocatorsForMessageID(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// An empty list is not the same claim as "no such message": this index is
	// filled by reads, so a message nobody has listed yet is simply absent. The
	// response says which it is rather than leaving the caller to guess.
	writeJSON(w, http.StatusOK, map[string]any{
		"message_id": id,
		"locators":   locs,
		"indexed":    len(locs) > 0,
	})
}

func (s *Server) handleMessageThread(w http.ResponseWriter, r *http.Request) {
	acct := r.URL.Query().Get("account")
	id := r.PathValue("id")
	folder := r.URL.Query().Get("folder")
	if acct == "" {
		writeError(w, http.StatusBadRequest, errors.New("account query param required"))
		return
	}
	a, err := s.accounts.Get(acct)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	seed, err := s.accounts.MessageByLocator(acct, providerIDScope(a.Provider, folder), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if seed == nil {
		writeJSON(w, http.StatusOK, map[string]any{"indexed": false, "messages": []any{}})
		return
	}
	msgs, err := s.accounts.ThreadMessages(acct, seed.ThreadID, seed.RootID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"indexed":         true,
		"thread_id":       seed.ThreadID,
		"root_message_id": seed.RootID,
		"messages":        msgs,
	})
}

// handleSearchMessages is full-text search over recorded message payloads,
// bm25-ranked. q is plain text, not FTS5 syntax. Optional: account narrows to
// one account, limit caps results (default 20, max 200).
func (s *Server) handleSearchMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, errors.New("q query param required"))
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	hits, err := s.accounts.SearchMessages(q, r.URL.Query().Get("account"), limit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if hits == nil {
		hits = []accounts.SearchHit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
}

// handleSimilarMessages finds recorded messages that read like the posted
// text. POST because the text is a whole message body, not a query string.
// exclude names a message to leave out of the results — normally the message
// the text came from, so it does not match itself.
func (s *Server) handleSimilarMessages(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text    string `json:"text"`
		Limit   int    `json:"limit"`
		Exclude struct {
			AccountID  string `json:"account_id"`
			ProviderID string `json:"provider_id"`
		} `json:"exclude"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Text == "" {
		writeError(w, http.StatusBadRequest, errors.New("text is required"))
		return
	}
	hits, err := s.accounts.SimilarMessages(req.Text, req.Exclude.AccountID, req.Exclude.ProviderID, req.Limit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if hits == nil {
		hits = []accounts.SearchHit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
}

func (s *Server) handleIndexStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.accounts.IndexStats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
