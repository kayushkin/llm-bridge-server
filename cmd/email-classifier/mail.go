package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ============================ mailstack client ============================
//
// mailstack (:8195) caches nothing: every list is a live provider round trip,
// and a Gmail page of 50 costs 51 upstream API calls. So every function here is
// treated as expensive, and the caller is expected to bound how many it makes
// per tick rather than sweeping the mailbox.
//
// It also has no search route and no since-filter, which is why this classifier
// cannot look a message up by its RFC 5322 Message-ID and must carry the
// provider locator (account_id:message_id) as the addressable reference.

type mailClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func newMailClient(baseURL, token string) *mailClient {
	return &mailClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// mailAccount is one configured mailbox. Provider matters: only google
// implements mutations, and imap ids are not trustworthy (see locatorOf).
type mailAccount struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider"`
	Address  string `json:"address"`
	Enabled  bool   `json:"enabled"`
}

// mailAddress is a From/To entry. Name is frequently empty on bulk mail.
type mailAddress struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// messageMeta is the list-row shape. Note the field is Unread, not Read.
type messageMeta struct {
	ID             string        `json:"id"`
	AccountID      string        `json:"account_id"`
	FolderID       string        `json:"folder_id"`
	Subject        string        `json:"subject"`
	From           mailAddress   `json:"from"`
	To             []mailAddress `json:"to"`
	Date           time.Time     `json:"date"`
	Snippet        string        `json:"snippet"`
	Unread         bool          `json:"unread"`
	Flagged        bool          `json:"flagged"`
	HasAttachments bool          `json:"has_attachments"`
}

// fullMessage is the single-message shape. Headers is where Message-ID,
// References and In-Reply-To live; mailstack drops the provider thread id, so
// those headers are the only route to conversation grouping.
type fullMessage struct {
	Meta      messageMeta       `json:"meta"`
	BodyText  string            `json:"body_text"`
	BodyHTML  string            `json:"body_html"`
	Headers   map[string]string `json:"headers"`
	InReplyTo string            `json:"in_reply_to"`
}

type mailFolder struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Canonical string `json:"canonical"`
}

func (m *mailClient) do(path string, out any) error {
	req, err := http.NewRequest("GET", m.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.Token)
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mailstack GET %s: %s — %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func (m *mailClient) listAccounts() ([]mailAccount, error) {
	var r struct {
		Accounts []mailAccount `json:"accounts"`
	}
	if err := m.do("/api/accounts", &r); err != nil {
		return nil, err
	}
	return r.Accounts, nil
}

// inboxFolder returns the account's canonical inbox. It is resolved rather than
// assumed because the canonical name differs per provider ("INBOX" on Gmail and
// on this host's IMAP account, but the mapping is the server's to make).
func (m *mailClient) inboxFolder(accountID string) (string, error) {
	var r struct {
		Folders []mailFolder `json:"folders"`
	}
	if err := m.do("/api/folders?account="+url.QueryEscape(accountID), &r); err != nil {
		return "", err
	}
	for _, f := range r.Folders {
		if f.Canonical == "inbox" {
			return f.ID, nil
		}
	}
	return "", fmt.Errorf("account %s has no folder with canonical=inbox", accountID)
}

// listMessages returns one page newest-first plus the cursor for the next page.
// An empty returned cursor means the folder is exhausted.
func (m *mailClient) listMessages(accountID, folderID, cursor string, limit int) ([]messageMeta, string, error) {
	q := url.Values{}
	q.Set("account", accountID)
	q.Set("folder", folderID)
	q.Set("limit", fmt.Sprint(limit))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var r struct {
		Messages   []messageMeta `json:"messages"`
		NextCursor string        `json:"next_cursor"`
	}
	if err := m.do("/api/messages?"+q.Encode(), &r); err != nil {
		return nil, "", err
	}
	return r.Messages, r.NextCursor, nil
}

func (m *mailClient) getMessage(accountID, messageID string) (*fullMessage, error) {
	var msg fullMessage
	path := "/api/messages/" + url.PathEscape(messageID) + "?account=" + url.QueryEscape(accountID)
	if err := m.do(path, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// ============================ identity ============================

// locatorOf builds the addressable reference for a message:
//
//	<account_id>:<provider message id>
//
// This is the entity_ref stored on card links, because it is the only id
// mailstack can resolve back to a message — the service has no search route, so
// a Message-ID alone is a dead end.
//
// It is account-qualified deliberately. Gmail ids are permanent per account, but
// an IMAP id here is a bare UID with no mailbox qualifier and UIDs are unique
// only within a mailbox, so an unqualified id can collide across accounts and
// across folders of the same account.
func locatorOf(accountID, messageID string) string {
	return accountID + ":" + messageID
}

// headerLookup reads a header case-insensitively. Providers disagree on the
// casing of Message-ID ("Message-Id" on some relays), and a case-sensitive miss
// here would silently cost us cross-account dedup.
func headerLookup(headers map[string]string, name string) string {
	if v, ok := headers[name]; ok {
		return v
	}
	want := strings.ToLower(name)
	for k, v := range headers {
		if strings.ToLower(k) == want {
			return v
		}
	}
	return ""
}

// messageIDOf returns the RFC 5322 Message-ID, angle brackets stripped.
//
// This is the durable identity: assigned by the sender, immutable across folder
// moves and relabels, and identical when one message reaches two of our accounts
// (this host's Gmail is fed by a forward, so that case is real). It is also the
// only key that can rebuild a thread, since References and In-Reply-To point at
// it.
//
// The header is optional per RFC 5322 §3.6.4. It was present on every message
// sampled from this host, but "almost always" is not "always", so the caller
// must handle an empty return rather than key on it blindly.
func messageIDOf(msg *fullMessage) string {
	raw := headerLookup(msg.Headers, "Message-ID")
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "<")
	raw = strings.TrimSuffix(raw, ">")
	return strings.TrimSpace(raw)
}

// referencedMessageIDs returns the Message-IDs this message replies to, newest
// intent first: In-Reply-To (the direct parent) then References (the chain).
// Used to attach a reply onto the card its parent already sits on, without
// spending a model call to rediscover the connection.
func referencedMessageIDs(msg *fullMessage) []string {
	var out []string
	seen := map[string]bool{}
	add := func(field string) {
		for _, tok := range strings.Fields(field) {
			tok = strings.TrimSpace(tok)
			tok = strings.TrimPrefix(tok, "<")
			tok = strings.TrimSuffix(tok, ">")
			if tok == "" || seen[tok] {
				continue
			}
			seen[tok] = true
			out = append(out, tok)
		}
	}
	add(msg.InReplyTo)
	add(headerLookup(msg.Headers, "In-Reply-To"))
	add(headerLookup(msg.Headers, "References"))
	return out
}

// senderKey is the address a sender-affinity rule is keyed on, lowercased.
// The full address rather than the domain: google.com sends both a security
// alert and a Play Store receipt, and those do not belong on one card.
func senderKey(from mailAddress) string {
	return strings.ToLower(strings.TrimSpace(from.Email))
}
