package mailstackclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The body is what mailstack's GET /api/messages/{id} actually returns on this
// host (read 2026-09-09 against demo-work:lmf8c5178e02571eae), trimmed: the
// headers live under "meta", not at the top level.
const liveMessageShape = `{
  "meta": {
    "id": "lmf8c5178e02571eae",
    "account_id": "demo-work",
    "folder_id": "INBOX",
    "subject": "Small thing in internal/httpapi/server.go — expose the total in the health response?",
    "from": {"name": "Helena Vos", "email": "helena.vos@northwind-eng.example"},
    "date": "2026-09-08T16:02:00Z"
  },
  "body": {"text": "…"}
}`

func stub(t *testing.T, status int, body string) (*Client, *http.Request) {
	t.Helper()
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	return c, got
}

func TestGetMessageHeadersReadsMeta(t *testing.T) {
	c, _ := stub(t, http.StatusOK, liveMessageShape)
	h, err := c.GetMessageHeaders(context.Background(), "demo-work", "lmf8c5178e02571eae")
	if err != nil {
		t.Fatal(err)
	}
	if h.From.Email != "helena.vos@northwind-eng.example" || h.From.Name != "Helena Vos" {
		t.Errorf("from = %+v", h.From)
	}
	if !strings.HasPrefix(h.Subject, "Small thing") {
		t.Errorf("subject = %q", h.Subject)
	}
}

func TestGetMessageHeadersRefusesAMessageWithNoSender(t *testing.T) {
	// The top-level shape the client used to decode: every field empty, no
	// error. That must be an error now, not a reply drafted to nobody.
	c, _ := stub(t, http.StatusOK, `{"id":"x","subject":"s","from":{"name":"","email":""}}`)
	if _, err := c.GetMessageHeaders(context.Background(), "demo-work", "x"); err == nil {
		t.Fatal("no error for a message with no sender address")
	}
}

func TestGetMessageHeadersSendsTokenAndAccount(t *testing.T) {
	var auth, account string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		account = r.URL.Query().Get("account")
		_, _ = w.Write([]byte(liveMessageShape))
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetMessageHeaders(context.Background(), "demo-work", "lmf8c5178e02571eae"); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || account != "demo-work" {
		t.Errorf("auth=%q account=%q", auth, account)
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

func TestNewRefusesAnEmptyToken(t *testing.T) {
	if _, err := New("http://localhost:1", ""); err == nil {
		t.Error("a client with no token was built; mailstack answers 401 to it")
	}
}
