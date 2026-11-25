package mailstack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/localmail"
)

func serve(t *testing.T, s *Server, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	testCallerHandler(s).ServeHTTP(w, r)
	return w
}

const twoExportedMessages = `{"source":{"ref":"export-001.json","sha256":"aa11"},"messages":[
	{"provider_message_id":"AAMk-1","message_id":"<one@outlook.example>","conversation_id":"conv-1",
	 "subject":"Läuft nicht — 至急","from":{"name":"Zoë","email":"zoe@customer.example"},
	 "to":[{"email":"support@desk.example"}],"received_at":"2026-10-01T09:30:00Z",
	 "body_text":"Bitte prüfen.","body_html":"<p>Bitte <b>prüfen</b>.</p>",
	 "outlook":{"store_id":"S1","folder_id":"F1","folder_path":"\\\\Support\\\\Inbox"}},
	{"provider_message_id":"AAMk-2","subject":"no html","from":{"email":"ops@desk.example"},
	 "received_at":"2026-10-01T10:00:00Z","body_text":""}]}`

type importResponse struct {
	Outcomes []localmail.ImportOutcome `json:"outcomes"`
	Counts   map[string]int            `json:"counts"`
}

func importInto(t *testing.T, s *Server, accountID, body string) importResponse {
	t.Helper()
	w := serve(t, s, http.MethodPost, "/api/accounts/"+accountID+"/messages/import", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("import status %d: %s", w.Code, w.Body)
	}
	var got importResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode import: %v", err)
	}
	return got
}

func TestImportReportsEachMessageAndReplaysAsUnchanged(t *testing.T) {
	s := localTestServer(t)
	first := importInto(t, s, "demo-local", twoExportedMessages)
	if first.Counts["created"] != 2 {
		t.Fatalf("counts %v, want 2 created", first.Counts)
	}
	again := importInto(t, s, "demo-local", twoExportedMessages)
	if again.Counts["unchanged"] != 2 || again.Outcomes[0].ID != first.Outcomes[0].ID {
		t.Fatalf("replay %+v, want 2 unchanged on the same ids", again)
	}

	w := serve(t, s, http.MethodGet, "/api/accounts/demo-local/import-sources", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"sha256": "aa11"`) {
		t.Fatalf("import sources %d: %s", w.Code, w.Body)
	}
}

func TestImportIsRefusedForARealProviderAndForMalformedInput(t *testing.T) {
	s := localTestServer(t)
	if w := serve(t, s, http.MethodPost, "/api/accounts/acct-ms/messages/import", twoExportedMessages, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("microsoft account: status %d, want 400", w.Code)
	}
	if w := serve(t, s, http.MethodPost, "/api/accounts/nope/messages/import", twoExportedMessages, nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown account: status %d, want 404", w.Code)
	}
	// A misspelled field is a 400, not a silently dropped value.
	w := serve(t, s, http.MethodPost, "/api/accounts/demo-local/messages/import",
		`{"messages":[{"provider_message_id":"x","received_at":"2026-10-01T09:30:00Z","body_txt":"typo"}]}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: status %d, want 400: %s", w.Code, w.Body)
	}
	w = serve(t, s, http.MethodPost, "/api/accounts/demo-local/messages/import",
		`{"messages":[{"provider_message_id":"x","body_text":"no date"}]}`, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "received_at is required") {
		t.Fatalf("missing date: status %d: %s", w.Code, w.Body)
	}
}

func TestResolveAnswersEveryRefInOrder(t *testing.T) {
	s := localTestServer(t)
	imported := importInto(t, s, "demo-local", twoExportedMessages)
	refs := fmt.Sprintf(`{"refs":[
		{"locator":%q},
		{"account_id":"demo-local","provider_message_id":"AAMk-2"},
		{"locator":"demo-local:lm-not-there"},
		{"locator":"no-colon"},
		{"locator":"ghost:lm1"},
		{"locator":"acct-ms:AAMk-1"}]}`, imported.Outcomes[0].Locator)
	w := serve(t, s, http.MethodPost, "/api/messages/resolve", refs, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve status %d: %s", w.Code, w.Body)
	}
	if !strings.HasPrefix(w.Header().Get("Server-Timing"), "store;dur=") {
		t.Fatalf("Server-Timing %q", w.Header().Get("Server-Timing"))
	}
	var got struct {
		Results []struct {
			Status  string                   `json:"status"`
			ETag    string                   `json:"etag"`
			Message *localmail.StoredMessage `json:"message"`
		} `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"found", "found", "not_found", "invalid", "unknown_account", "not_local_account"}
	if len(got.Results) != len(want) {
		t.Fatalf("%d results, want %d", len(got.Results), len(want))
	}
	for i, status := range want {
		if got.Results[i].Status != status {
			t.Fatalf("result %d status %q, want %q", i, got.Results[i].Status, status)
		}
	}
	first := got.Results[0].Message
	if first.Subject != "Läuft nicht — 至急" || first.Outlook == nil || first.Outlook.FolderPath != `\\Support\\Inbox` {
		t.Fatalf("first message %+v", first)
	}
	if got.Results[1].Message.Bodies.HTML != nil || got.Results[1].Message.Bodies.Text == nil {
		t.Fatalf("second message bodies %+v, want text present and html absent", got.Results[1].Message.Bodies)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("Bitte <b>")) {
		t.Fatal("resolve returned a body; it must return metadata only")
	}
}

func TestBodyIsServedRawWithItsOwnTypeAndRevalidates(t *testing.T) {
	s := localTestServer(t)
	imported := importInto(t, s, "demo-local", twoExportedMessages)
	id := imported.Outcomes[0].ID
	path := "/api/messages/" + id + "/body?account=demo-local&format=html"

	w := serve(t, s, http.MethodGet, path, "", nil)
	if w.Code != http.StatusOK || w.Body.String() != "<p>Bitte <b>prüfen</b>.</p>" {
		t.Fatalf("html body %d %q", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Fatalf("no sandbox CSP on an unsanitized HTML body: %q", csp)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	if w := serve(t, s, http.MethodGet, path, "", map[string]string{"If-None-Match": etag}); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("conditional read status %d with %d bytes, want 304 and none", w.Code, w.Body.Len())
	}

	// A changed import invalidates the body's ETag.
	changed := strings.Replace(twoExportedMessages, "<p>Bitte <b>prüfen</b>.</p>", "<p>neu</p>", 1)
	importInto(t, s, "demo-local", changed)
	if w := serve(t, s, http.MethodGet, path, "", map[string]string{"If-None-Match": etag}); w.Code != http.StatusOK || w.Body.String() != "<p>neu</p>" {
		t.Fatalf("after change status %d body %q, want 200 with the new body", w.Code, w.Body)
	}
}

func TestBodyNotFoundSaysWhichKind(t *testing.T) {
	s := localTestServer(t)
	imported := importInto(t, s, "demo-local", twoExportedMessages)
	textOnly := imported.Outcomes[1].ID

	w := serve(t, s, http.MethodGet, "/api/messages/"+textOnly+"/body?account=demo-local&format=html", "", nil)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"body_absent"`) {
		t.Fatalf("absent html: %d %s", w.Code, w.Body)
	}
	w = serve(t, s, http.MethodGet, "/api/messages/"+textOnly+"/body?account=demo-local&format=text", "", nil)
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("empty text: %d %q, want 200 with no bytes", w.Code, w.Body)
	}
	w = serve(t, s, http.MethodGet, "/api/messages/lm-nope/body?account=demo-local&format=text", "", nil)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"message_not_found"`) {
		t.Fatalf("missing message: %d %s", w.Code, w.Body)
	}
	w = serve(t, s, http.MethodGet, "/api/messages/"+textOnly+"/body?account=demo-local&format=rtf", "", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown format: %d, want 400", w.Code)
	}
}

func TestMetadataETagFollowsMailboxState(t *testing.T) {
	s := localTestServer(t)
	imported := importInto(t, s, "demo-local", twoExportedMessages)
	id := imported.Outcomes[0].ID
	path := "/api/messages/" + id + "/metadata?account=demo-local"
	w := serve(t, s, http.MethodGet, path, "", nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "prüfen</b>") {
		t.Fatalf("metadata %d: %s", w.Code, w.Body)
	}
	etag := w.Header().Get("ETag")

	if w := serve(t, s, http.MethodPost, "/api/messages/"+id+"/read?account=demo-local", `{"read":true}`, nil); w.Code != http.StatusOK {
		t.Fatalf("mark read %d: %s", w.Code, w.Body)
	}
	w = serve(t, s, http.MethodGet, path, "", map[string]string{"If-None-Match": etag})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"unread": false`) {
		t.Fatalf("after mark read: %d %s, want a fresh 200 — the old ETag must not match", w.Code, w.Body)
	}
}

func TestAnImportedMessageIsFoundByMessageID(t *testing.T) {
	s := localTestServer(t)
	importInto(t, s, "demo-local", twoExportedMessages)
	w := serve(t, s, http.MethodGet, "/api/lookup/message-id/one@outlook.example", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"indexed": true`) {
		t.Fatalf("lookup %d: %s", w.Code, w.Body)
	}
}
