package mailstack

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/accounts"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/backend"
)

// localTestServer has one local account and one microsoft account, so both the
// accepted and the refused path are reachable with no network and no credential.
func localTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	store, err := accounts.Open(filepath.Join(dir, "accounts.db"))
	if err != nil {
		t.Fatalf("open accounts store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	for _, a := range []*accounts.Account{
		{ID: "demo-local", Label: "Demo", Provider: accounts.ProviderLocal, Address: "work@demo.invalid", Enabled: true},
		{ID: "acct-ms", Label: "Real", Provider: accounts.ProviderMicrosoft, Address: "user@example.com", Enabled: true},
	} {
		if err := store.Save(a); err != nil {
			t.Fatalf("save account %s: %v", a.ID, err)
		}
	}
	s := NewServer(store, nil, testSettings(t, ""), WithLocalMailDB(filepath.Join(dir, "local-mail.db")))
	t.Cleanup(func() { s.Shutdown(nil) })
	return s
}

func appendTo(t *testing.T, s *Server, accountID, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/accounts/"+accountID+"/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w
}

const oneMessage = `{"subject":"Can you do X task",
	"from":{"name":"Priya Raman","email":"priya@demo.invalid"},
	"to":[{"email":"work@demo.invalid"}],
	"body_text":"Please pick this up before the release."}`

func TestAppendingToALocalAccountReturnsTheLocator(t *testing.T) {
	s := localTestServer(t)
	w := appendTo(t, s, "demo-local", oneMessage)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", w.Code, w.Body)
	}
	var got struct {
		Message backend.MessageMeta `json:"message"`
		Locator string              `json:"locator"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Message.ID == "" {
		t.Fatal("no id assigned")
	}
	want := "demo-local:" + got.Message.ID
	if got.Locator != want {
		t.Fatalf("locator %q, want %q", got.Locator, want)
	}
}

// The refusal is the feature. "Append" against a real provider would mean
// either sending mail or forging a message into somebody's real mailbox.
func TestAppendingToARealProviderIsRefused(t *testing.T) {
	s := localTestServer(t)
	w := appendTo(t, s, "acct-ms", oneMessage)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "local account") {
		t.Fatalf("refusal does not explain itself: %s", w.Body)
	}
}

func TestAppendingToAnUnknownAccountIsNotFound(t *testing.T) {
	if w := appendTo(t, localTestServer(t), "nope", oneMessage); w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", w.Code)
	}
}

func TestAppendRequiresASenderAndABody(t *testing.T) {
	s := localTestServer(t)
	for name, body := range map[string]string{
		"no sender": `{"subject":"x","body_text":"y"}`,
		"no body":   `{"subject":"x","from":{"email":"a@demo.invalid"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if w := appendTo(t, s, "demo-local", body); w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", w.Code)
			}
		})
	}
}

// An appended message must be readable through the same routes as real mail —
// that is the whole reason for a local backend rather than a fixture file.
func TestAnAppendedMessageIsReadableThroughTheOrdinaryRoutes(t *testing.T) {
	s := localTestServer(t)
	w := appendTo(t, s, "demo-local", oneMessage)
	if w.Code != http.StatusCreated {
		t.Fatalf("append: %d %s", w.Code, w.Body)
	}
	var created struct {
		Message backend.MessageMeta `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	get := func(target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, r)
		return rec
	}

	if rec := get("/api/folders?account=demo-local"); rec.Code != http.StatusOK {
		t.Fatalf("folders: %d %s", rec.Code, rec.Body)
	}

	rec := get("/api/messages?account=demo-local")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Messages []backend.MessageMeta `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Messages) != 1 || list.Messages[0].ID != created.Message.ID {
		t.Fatalf("list returned %+v, want the appended message", list.Messages)
	}

	rec = get(fmt.Sprintf("/api/messages/%s?account=demo-local", created.Message.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	var full backend.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if full.BodyText != "Please pick this up before the release." {
		t.Fatalf("body %q", full.BodyText)
	}
}

// Appending indexes the message, so it is answerable by RFC identity without
// ever having been listed. The thread route reads only the index, so this is
// what makes a synthetic conversation hang together.
func TestAnAppendedMessageIsIndexedForThreadingImmediately(t *testing.T) {
	s := localTestServer(t)
	w := appendTo(t, s, "demo-local", `{"subject":"Original",
		"from":{"email":"priya@demo.invalid"},
		"message_id":"root@demo.invalid",
		"body_text":"first"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("append root: %d %s", w.Code, w.Body)
	}
	w = appendTo(t, s, "demo-local", `{"subject":"Re: Original",
		"from":{"email":"priya@demo.invalid"},
		"message_id":"reply@demo.invalid",
		"in_reply_to":"root@demo.invalid",
		"references":["root@demo.invalid"],
		"body_text":"second"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("append reply: %d %s", w.Code, w.Body)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/lookup/message-id/root@demo.invalid", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup: %d %s", rec.Code, rec.Body)
	}
	var lookup struct {
		Indexed  bool `json:"indexed"`
		Locators []struct {
			AccountID  string `json:"account_id"`
			ProviderID string `json:"provider_id"`
		} `json:"locators"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &lookup); err != nil {
		t.Fatalf("decode lookup: %v", err)
	}
	if !lookup.Indexed || len(lookup.Locators) == 0 {
		t.Fatalf("appended message was not indexed: %s", rec.Body)
	}
	if lookup.Locators[0].AccountID != "demo-local" {
		t.Fatalf("locator account %q", lookup.Locators[0].AccountID)
	}
}
