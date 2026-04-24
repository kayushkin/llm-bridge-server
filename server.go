package mailstack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/kayushkin/mailstack/accounts"
	"github.com/kayushkin/mailstack/apiauth"
	"github.com/kayushkin/mailstack/backend"
)

type Server struct {
	accounts *accounts.Store
	apiauth  *apiauth.Client
	token    string
	mux      *http.ServeMux

	imapMu       sync.Mutex
	imapBackends map[string]*backend.Imap
}

func NewServer(store *accounts.Store, auth *apiauth.Client, bearerToken string) *Server {
	s := &Server{
		accounts:     store,
		apiauth:      auth,
		token:        bearerToken,
		mux:          http.NewServeMux(),
		imapBackends: map[string]*backend.Imap{},
	}
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.Handle("GET /api/accounts", s.auth(s.handleListAccounts))
	s.mux.Handle("POST /api/accounts", s.auth(s.handleCreateAccount))
	s.mux.Handle("GET /api/accounts/{id}", s.auth(s.handleGetAccount))
	s.mux.Handle("PATCH /api/accounts/{id}", s.auth(s.handlePatchAccount))
	s.mux.Handle("DELETE /api/accounts/{id}", s.auth(s.handleDeleteAccount))
	s.mux.Handle("GET /api/imap/creds", s.auth(s.handleListImapCreds))
	s.mux.Handle("POST /api/imap/creds", s.auth(s.handleSaveImapCred))
	s.mux.Handle("GET /api/imap/creds/{name}", s.auth(s.handleGetImapCred))
	s.mux.Handle("PUT /api/imap/creds/{name}", s.auth(s.handleSaveImapCred))
	s.mux.Handle("DELETE /api/imap/creds/{name}", s.auth(s.handleDeleteImapCred))
	s.mux.Handle("GET /api/folders", s.auth(s.handleFolders))
	s.mux.Handle("GET /api/messages", s.auth(s.handleListMessages))
	s.mux.Handle("GET /api/messages/{id}", s.auth(s.handleGetMessage))
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			next.ServeHTTP(w, r)
			return
		}
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") || h[7:] != s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

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
		return backend.NewGmail(a.ID, apiauthProvider, apiauthAccount, s.apiauth), a, nil
	case accounts.ProviderMicrosoft:
		apiauthProvider := a.ApiauthProvider
		if apiauthProvider == "" {
			apiauthProvider = "microsoft"
		}
		apiauthAccount := a.ApiauthAccount
		if apiauthAccount == "" {
			apiauthAccount = "default"
		}
		return backend.NewGraph(a.ID, apiauthProvider, apiauthAccount, s.apiauth), a, nil
	case accounts.ProviderIMAP:
		b, err := s.imapBackendFor(a)
		if err != nil {
			return nil, a, err
		}
		return b, a, nil
	default:
		return nil, a, fmt.Errorf("unknown provider %q", a.Provider)
	}
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
		writeError(w, http.StatusBadRequest, err)
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
		writeError(w, http.StatusBadRequest, err)
		return
	}
	msgs, next, err := b.List(r.Context(), folder, cursor, limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
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
		writeError(w, http.StatusBadRequest, err)
		return
	}
	msg, err := b.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
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

var _ backend.TokenSource = (*apiauth.Client)(nil)

// Shutdown closes all persistent IMAP connections.
func (s *Server) Shutdown(ctx context.Context) error {
	s.imapMu.Lock()
	defer s.imapMu.Unlock()
	for id, b := range s.imapBackends {
		_ = b.Close()
		delete(s.imapBackends, id)
	}
	return nil
}
