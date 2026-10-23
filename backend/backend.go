package backend

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

type CanonicalKind string

const (
	KindInbox   CanonicalKind = "inbox"
	KindSent    CanonicalKind = "sent"
	KindDrafts  CanonicalKind = "drafts"
	KindTrash   CanonicalKind = "trash"
	KindSpam    CanonicalKind = "spam"
	KindArchive CanonicalKind = "archive"
)

type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type Folder struct {
	ID          string        `json:"id"`
	AccountID   string        `json:"account_id"`
	Name        string        `json:"name"`
	Path        string        `json:"path"`
	Canonical   CanonicalKind `json:"canonical,omitempty"`
	ParentID    string        `json:"parent_id,omitempty"`
	UnreadCount int           `json:"unread_count"`
}

type MessageMeta struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id"`
	FolderID  string    `json:"folder_id"`
	Subject   string    `json:"subject"`
	From      Address   `json:"from"`
	To        []Address `json:"to,omitempty"`
	Cc        []Address `json:"cc,omitempty"`
	Date      time.Time `json:"date"`
	Snippet   string    `json:"snippet"`
	Unread    bool      `json:"unread"`
	Flagged   bool      `json:"flagged"`
	HasAttach bool      `json:"has_attachments"`

	// Identity and threading. All four were previously decoded off the wire and
	// discarded, which left callers with no way to dedup one message that
	// reached two accounts, and no way to reconstruct a conversation.
	//
	// MessageID is the RFC 5322 Message-ID with its angle brackets stripped.
	// Assigned by the sender, so it is stable across folder moves, relabels and
	// accounts — but it is OPTIONAL per RFC 5322 §3.6.4, so it may be empty and
	// no caller may key on it blindly.
	MessageID string `json:"message_id,omitempty"`
	// ThreadID is the provider's own conversation key: Gmail threadId, Graph
	// conversationId. Authoritative within one account, absent on IMAP.
	ThreadID string `json:"thread_id,omitempty"`
	// InReplyTo is the parent's Message-ID, brackets stripped.
	InReplyTo string `json:"in_reply_to,omitempty"`
	// References is the ancestor chain, oldest first, brackets stripped. This is
	// the only thread signal that crosses providers and accounts.
	References []string `json:"references,omitempty"`
}

// StripAngleBrackets normalizes one RFC 5322 message identifier.
//
// Shared rather than reimplemented per backend because the three backends hand
// back three different spellings of the same header — Gmail preserves the
// sender's casing, Graph sets "Message-ID" explicitly, and IMAP's net/mail
// canonicalizes it to "Message-Id" — and a mismatch here silently costs the
// cross-account dedup this field exists to provide.
func StripAngleBrackets(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return strings.TrimSpace(s)
}

// SplitReferences parses a References or In-Reply-To header into bare ids.
func SplitReferences(raw string) []string {
	var out []string
	for _, tok := range strings.Fields(raw) {
		if id := StripAngleBrackets(tok); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// HeaderLookup reads a header case-insensitively. Mandatory here: the three
// backends disagree on capitalization, so an exact-case read works on one and
// silently returns nothing on another.
func HeaderLookup(headers map[string]string, name string) string {
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

type Attachment struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	MimeType  string `json:"mime_type"`
	Size      int64  `json:"size"`
	InlineCID string `json:"inline_cid,omitempty"`
}

// ErrAttachmentNotFound is wrapped by Attachment when the message has no
// attachment with the id asked for.
var ErrAttachmentNotFound = errors.New("attachment not found")

type Message struct {
	Meta        MessageMeta       `json:"meta"`
	BodyHTML    string            `json:"body_html"`
	BodyText    string            `json:"body_text"`
	Headers     map[string]string `json:"headers,omitempty"`
	Attachments []Attachment      `json:"attachments,omitempty"`
	InReplyTo   string            `json:"in_reply_to,omitempty"`
}

type Draft struct {
	AccountID   string            `json:"account_id"`
	To          []Address         `json:"to,omitempty"`
	Cc          []Address         `json:"cc,omitempty"`
	Bcc         []Address         `json:"bcc,omitempty"`
	Subject     string            `json:"subject"`
	BodyHTML    string            `json:"body_html"`
	InReplyTo   string            `json:"in_reply_to,omitempty"`
	References  []string          `json:"references,omitempty"`
	Attachments []DraftAttachment `json:"attachments,omitempty"`
}

type DraftAttachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Data     []byte `json:"data"`
}

// Searcher is a backend whose provider searches its mailbox itself. Search
// takes the provider's own query syntax and lists matches newest first.
// Gmail is one; a backend that is not answers GET /api/messages?q= with 501
// rather than an unfiltered list.
type Searcher interface {
	Search(ctx context.Context, query, cursor string, limit int) (msgs []MessageMeta, next string, err error)
}

// Backend is the provider-agnostic mail interface. Phase 2 implements
// only the read-side methods for Gmail; write-side and other providers
// come in later phases.
type Backend interface {
	Folders(ctx context.Context) ([]Folder, error)
	List(ctx context.Context, folderID, cursor string, limit int) (msgs []MessageMeta, next string, err error)
	// Get reads one message. folderID names the folder that holds it; a
	// backend whose ids are unique across the whole mailbox ignores it, and
	// IMAP, whose UIDs are unique only within one mailbox, reads INBOX when it
	// is empty.
	Get(ctx context.Context, folderID, id string) (Message, error)

	MarkRead(ctx context.Context, id string, read bool) error
	Flag(ctx context.Context, id string, flagged bool) error
	Move(ctx context.Context, id, folderID string) error
	Delete(ctx context.Context, id string) error
	PermDelete(ctx context.Context, id string) error

	Send(ctx context.Context, d Draft) (sentID string, err error)
	SaveDraft(ctx context.Context, d Draft, existingID string) (id string, err error)
	Attachment(ctx context.Context, msgID, attID string) (body io.ReadCloser, contentType string, err error)
}
