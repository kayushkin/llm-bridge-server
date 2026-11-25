package mailstackclient

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/backend"
)

// messageReader answers every read with one message, or one error, and keeps
// what it was asked.
type messageReader struct {
	message             backend.Message
	err                 error
	accountID, folderID string
	id                  string
}

func (reader *messageReader) ReadMessage(_ context.Context, accountID, folderID, id string) (backend.Message, error) {
	reader.accountID, reader.folderID, reader.id = accountID, folderID, id
	return reader.message, reader.err
}

// The headers of a message as mailstack reads it on this kind of account.
var helenaMessage = backend.Message{Meta: backend.MessageMeta{
	ID: "lmf8c5178e02571eae", AccountID: "demo-work", FolderID: "INBOX",
	Subject: "Small thing in internal/httpapi/server.go — expose the total in the health response?",
	From:    backend.Address{Name: "Helena Vos", Email: "helena.vos@northwind-eng.example"},
}, BodyText: "…"}

func TestGetMessageHeadersReadsMeta(t *testing.T) {
	reader := &messageReader{message: helenaMessage}
	h, err := New(reader).GetMessageHeaders(context.Background(), "demo-work", "lmf8c5178e02571eae")
	if err != nil {
		t.Fatal(err)
	}
	if h.From.Email != "helena.vos@northwind-eng.example" || h.From.Name != "Helena Vos" {
		t.Errorf("from = %+v", h.From)
	}
	if !strings.HasPrefix(h.Subject, "Small thing") {
		t.Errorf("subject = %q", h.Subject)
	}
	if reader.accountID != "demo-work" || reader.id != "lmf8c5178e02571eae" || reader.folderID != "" {
		t.Errorf("read account %q folder %q id %q", reader.accountID, reader.folderID, reader.id)
	}
}

func TestGetMessageHeadersRefusesAMessageWithNoSender(t *testing.T) {
	// A message with every header empty must be an error, not a reply drafted
	// to nobody.
	reader := &messageReader{message: backend.Message{Meta: backend.MessageMeta{ID: "x", Subject: "s"}}}
	if _, err := New(reader).GetMessageHeaders(context.Background(), "demo-work", "x"); err == nil {
		t.Fatal("no error for a message with no sender address")
	}
}

func TestGetMessageHeadersPassesMailstacksErrorOn(t *testing.T) {
	refused := errors.New("account demo-work is google")
	_, err := New(&messageReader{err: refused}).GetMessageHeaders(context.Background(), "demo-work", "x")
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want mailstack's", err)
	}
}

func TestParseLocator(t *testing.T) {
	acct, id, err := ParseLocator("demo-work:lmf8c5178e02571eae")
	if err != nil || acct != "demo-work" || id != "lmf8c5178e02571eae" {
		t.Errorf("got %q %q %v", acct, id, err)
	}
	if _, _, err := ParseLocator("no-colon"); err == nil {
		t.Error("a ref with no colon parsed")
	}
}
