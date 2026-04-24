package backend

import (
	"context"
	"io"
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
}

type Attachment struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	MimeType  string `json:"mime_type"`
	Size      int64  `json:"size"`
	InlineCID string `json:"inline_cid,omitempty"`
}

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

// Backend is the provider-agnostic mail interface. Phase 2 implements
// only the read-side methods for Gmail; write-side and other providers
// come in later phases.
type Backend interface {
	Folders(ctx context.Context) ([]Folder, error)
	List(ctx context.Context, folderID, cursor string, limit int) (msgs []MessageMeta, next string, err error)
	Get(ctx context.Context, id string) (Message, error)

	MarkRead(ctx context.Context, id string, read bool) error
	Flag(ctx context.Context, id string, flagged bool) error
	Move(ctx context.Context, id, folderID string) error
	Delete(ctx context.Context, id string) error
	PermDelete(ctx context.Context, id string) error

	Send(ctx context.Context, d Draft) (sentID string, err error)
	SaveDraft(ctx context.Context, d Draft, existingID string) (id string, err error)
	Attachment(ctx context.Context, msgID, attID string) (body io.ReadCloser, contentType string, err error)
}
