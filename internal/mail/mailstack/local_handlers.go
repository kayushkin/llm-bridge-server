package mailstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/kayushkin/mailstack/accounts"
	"github.com/kayushkin/mailstack/backend"
	"github.com/kayushkin/mailstack/localmail"
)

// localMailStore opens the synthetic mailbox on first use.
//
// Lazy rather than opened at start-up so that a host with no local account
// never creates the file. The e2e smoke asserts that a run touches nothing
// under the real config directory, and an eagerly-created database would fail
// it on every host whether or not the feature is used.
func (s *Server) localMailStore() (*localmail.Store, error) {
	s.localMu.Lock()
	defer s.localMu.Unlock()
	if s.localStore != nil {
		return s.localStore, nil
	}
	path := s.localDBPath
	if path == "" {
		path = localmail.DefaultDBPath()
	}
	store, err := localmail.Open(path)
	if err != nil {
		return nil, err
	}
	s.localStore = store
	return store, nil
}

// appendMessageRequest is one message being put into a local account, as a
// sender would have written it.
//
// It carries what a sender controls and nothing a provider would assign. The
// opaque id, the Message-ID when omitted, the snippet and the thread key are
// all filled in by the store, exactly as a real provider fills them in — see
// localmail.Append.
type appendMessageRequest struct {
	Subject string            `json:"subject"`
	From    backend.Address   `json:"from"`
	To      []backend.Address `json:"to,omitempty"`
	Cc      []backend.Address `json:"cc,omitempty"`
	// Date is when the message was sent. Empty means now. It is settable
	// precisely because synthetic mail is usually backfilled: a corpus that all
	// arrived in the same second exercises no ordering, no threading over time
	// and no paging.
	Date     time.Time `json:"date,omitempty"`
	BodyText string    `json:"body_text"`
	BodyHTML string    `json:"body_html,omitempty"`
	FolderID string    `json:"folder_id,omitempty"`
	Unread   *bool     `json:"unread,omitempty"`
	Flagged  bool      `json:"flagged,omitempty"`

	// Threading. MessageID is optional and minted when absent. InReplyTo and
	// References are what make a message a reply rather than a new thread, and
	// they are the caller's to set because only the caller knows the corpus.
	MessageID  string   `json:"message_id,omitempty"`
	InReplyTo  string   `json:"in_reply_to,omitempty"`
	References []string `json:"references,omitempty"`
	// ThreadID overrides the derived conversation key. Rarely wanted: the store
	// derives it from References the same way mailstack answers a thread query,
	// so setting it by hand is how a thread ends up split.
	ThreadID string `json:"thread_id,omitempty"`
}

// handleAppendLocalMessage puts a message into an account this host owns.
//
// It is refused for google, microsoft and imap accounts, and the refusal is the
// point rather than a missing feature: mailstack is a proxy for those, and
// "append" against a real provider would either mean sending mail or forging a
// message into somebody's real mailbox. Neither is something this service
// should be able to do by accident. A local account has no such ambiguity —
// there is no upstream, so the store is the only place the message could live.
func (s *Server) handleAppendLocalMessage(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	acct, err := s.accounts.Get(accountID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if acct.Provider != accounts.ProviderLocal {
		writeError(w, http.StatusBadRequest, fmt.Errorf(
			"account %s is a %s account; messages can only be appended to a local account",
			accountID, acct.Provider))
		return
	}

	var req appendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.From.Email == "" {
		writeError(w, http.StatusBadRequest, errors.New("from.email is required"))
		return
	}
	if req.BodyText == "" && req.BodyHTML == "" {
		writeError(w, http.StatusBadRequest, errors.New("body_text or body_html is required"))
		return
	}

	// Unread defaults to true: a message that has just arrived has not been
	// read. A plain bool would make "false" and "unspecified" the same request.
	unread := true
	if req.Unread != nil {
		unread = *req.Unread
	}

	store, err := s.localMailStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	meta, err := store.Append(r.Context(), accountID, backend.Message{
		Meta: backend.MessageMeta{
			Subject: req.Subject, From: req.From, To: req.To, Cc: req.Cc,
			Date: req.Date, FolderID: req.FolderID, Unread: unread, Flagged: req.Flagged,
			MessageID: req.MessageID, ThreadID: req.ThreadID,
			InReplyTo: req.InReplyTo, References: req.References,
		},
		BodyText: req.BodyText,
		BodyHTML: req.BodyHTML,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Index it exactly as a read would, so a message that has never been listed
	// is still answerable by Message-ID and still joins a thread.
	s.indexMessages(accountID, []backend.MessageMeta{meta})

	// The locator is returned assembled. Every consumer builds it by hand today
	// — kanban-store's entity ref, the classifier's locatorOf — and each of them
	// is a place the two-part format can be spelled wrong.
	writeJSON(w, http.StatusCreated, map[string]any{
		"message": meta,
		"locator": accountID + ":" + meta.ID,
	})
}
