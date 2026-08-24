package backend

import (
	"context"
	"fmt"
	"io"
)

// LocalStore is the storage a local account reads and writes.
//
// Declared here rather than in the storage package, and in terms of this
// package's own types, so that the dependency points one way: localmail imports
// backend, backend imports nothing of localmail. It is the same shape as
// FolderMapStore, which lets Imap take the accounts store without importing it.
type LocalStore interface {
	Folders(ctx context.Context, accountID string) ([]Folder, error)
	List(ctx context.Context, accountID, folderID, cursor string, limit int) ([]MessageMeta, string, error)
	Get(ctx context.Context, accountID, id string) (Message, error)
	SetUnread(ctx context.Context, accountID, id string, unread bool) error
	SetFlagged(ctx context.Context, accountID, id string, flagged bool) error
	MoveTo(ctx context.Context, accountID, id, folderID string) error
	Delete(ctx context.Context, accountID, id string) error
	PermDelete(ctx context.Context, accountID, id string) error
}

// Local is a mail account this host owns outright: no provider, no network, no
// credentials. It exists so that synthetic mail — demo data, fixtures, a corpus
// for exercising a classifier — can be read through exactly the same interface,
// routes and consumers as real mail, instead of every consumer growing a second
// code path for fake messages.
//
// Unlike the three provider backends, the write operations here are real. There
// is no upstream to be unimplemented against: marking a local message read is a
// row update, and refusing it would be inventing a limitation.
type Local struct {
	accountID string
	store     LocalStore
}

func NewLocal(accountID string, store LocalStore) *Local {
	return &Local{accountID: accountID, store: store}
}

func (l *Local) Folders(ctx context.Context) ([]Folder, error) {
	return l.store.Folders(ctx, l.accountID)
}

func (l *Local) List(ctx context.Context, folderID, cursor string, limit int) ([]MessageMeta, string, error) {
	return l.store.List(ctx, l.accountID, folderID, cursor, limit)
}

func (l *Local) Get(ctx context.Context, id string) (Message, error) {
	return l.store.Get(ctx, l.accountID, id)
}

func (l *Local) MarkRead(ctx context.Context, id string, read bool) error {
	return l.store.SetUnread(ctx, l.accountID, id, !read)
}

func (l *Local) Flag(ctx context.Context, id string, flagged bool) error {
	return l.store.SetFlagged(ctx, l.accountID, id, flagged)
}

func (l *Local) Move(ctx context.Context, id, folderID string) error {
	return l.store.MoveTo(ctx, l.accountID, id, folderID)
}

func (l *Local) Delete(ctx context.Context, id string) error {
	return l.store.Delete(ctx, l.accountID, id)
}

func (l *Local) PermDelete(ctx context.Context, id string) error {
	return l.store.PermDelete(ctx, l.accountID, id)
}

// Send and SaveDraft are refused rather than approximated.
//
// A local account has nowhere to send to: there is no MTA behind it, and
// writing the message into a Sent folder would report success for mail that
// never left the building. Appending a message to a local account is a
// different operation with different meaning, and it has its own route.
func (l *Local) Send(ctx context.Context, d Draft) (string, error) {
	return "", fmt.Errorf("local accounts cannot send: there is no mail transport behind them")
}

func (l *Local) SaveDraft(ctx context.Context, d Draft, existingID string) (string, error) {
	return "", errNotImplemented
}

// Attachment is refused because a local message has none: the append route
// carries a body and no parts, so there is no attachment id that could ever
// resolve.
func (l *Local) Attachment(ctx context.Context, msgID, attID string) (io.ReadCloser, string, error) {
	return nil, "", fmt.Errorf("local messages carry no attachments")
}
