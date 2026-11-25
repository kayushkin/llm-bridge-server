package backend

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// newImapWithTwoMailboxes serves an in-memory IMAP account whose INBOX and
// Sent each hold one message. A UID is unique only within its mailbox, so
// both messages carry UID 1. The backend gets a plain-TCP client handed to
// it, because dial speaks only TLS.
func newImapWithTwoMailboxes(t *testing.T) *Imap {
	t.Helper()
	user := imapmemserver.NewUser("user", "pass")
	for mailbox, subject := range map[string]string{"INBOX": "inbox message", "Sent": "sent message"} {
		if err := user.Create(mailbox, nil); err != nil && mailbox != "INBOX" {
			t.Fatalf("create %s: %v", mailbox, err)
		}
		raw := "Subject: " + subject + "\r\nMessage-ID: <" + mailbox + "@test>\r\n\r\nbody of the " + subject + "\r\n"
		if _, err := user.Append(mailbox, strings.NewReader(raw), &imap.AppendOptions{}); err != nil {
			t.Fatalf("append to %s: %v", mailbox, err)
		}
	}
	memory := imapmemserver.New()
	memory.AddUser(user)
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memory.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	client, err := imapclient.DialInsecure(listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Fatal(err)
	}
	b := NewImap("acct-imap", ImapConfig{}, nil)
	b.client = client
	t.Cleanup(func() { b.Close() })
	return b
}

// Get reads the mailbox it is told to, whichever mailbox the connection last
// opened. It used to read the last-opened one, so opening message 1 of Sent
// after listing INBOX returned INBOX's message 1 without complaint.
func TestImapGetReadsTheNamedFolderNotTheLastOpenedOne(t *testing.T) {
	ctx := context.Background()
	b := newImapWithTwoMailboxes(t)

	if _, _, err := b.List(ctx, "INBOX", "", 10); err != nil {
		t.Fatal(err)
	}
	sent, err := b.Get(ctx, "Sent", "1")
	if err != nil {
		t.Fatal(err)
	}
	if sent.Meta.Subject != "sent message" || sent.Meta.FolderID != "Sent" {
		t.Errorf("Get(Sent, 1) after listing INBOX = %q in %q, want %q in Sent", sent.Meta.Subject, sent.Meta.FolderID, "sent message")
	}

	// With no folder named, Get reads INBOX, as List does — even when Sent
	// was the last mailbox opened.
	inbox, err := b.Get(ctx, "", "1")
	if err != nil {
		t.Fatal(err)
	}
	if inbox.Meta.Subject != "inbox message" || inbox.Meta.FolderID != "INBOX" {
		t.Errorf("Get(\"\", 1) after reading Sent = %q in %q, want %q in INBOX", inbox.Meta.Subject, inbox.Meta.FolderID, "inbox message")
	}
}
