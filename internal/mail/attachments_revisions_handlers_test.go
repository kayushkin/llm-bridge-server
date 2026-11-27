package mail

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/mail/accounts"
	"github.com/kayushkin/llm-bridge-server/internal/mail/filestore"
)

// fakeFileStore is file-store as mailstack calls it, with the headers
// file-store's contract promises on a download.
type fakeFileStore struct {
	mu       sync.Mutex
	next     int
	files    map[string][]byte
	purged   []string
	refuseAt int // the upload, counted from 1, to refuse with 413; 0 refuses none
	uploads  int
}

func newFakeFileStore(t *testing.T) (*fakeFileStore, filestore.Files) {
	fake := &fakeFileStore{files: map[string][]byte{}}
	return fake, fake
}

func (fake *fakeFileStore) Upload(content []byte, contentType, filename, ownerRef string) (*filestore.File, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.uploads++
	if fake.uploads == fake.refuseAt {
		return nil, &filestore.RefusalError{Status: http.StatusRequestEntityTooLarge, Body: []byte(`{"error":"too large"}`)}
	}
	fake.next++
	id := fmt.Sprintf("file_%06d", fake.next)
	fake.files[id] = append([]byte(nil), content...)
	return &filestore.File{ID: id, SizeBytes: int64(len(content)), Filename: filename, ContentType: contentType}, nil
}

func (fake *fakeFileStore) ServeContent(w http.ResponseWriter, _ *http.Request, fileID string, _ bool) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	body, ok := fake.files[fileID]
	if !ok {
		return filestore.ErrNotFound
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="x"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Write(body)
	return nil
}

func (fake *fakeFileStore) Purge(fileID string) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, ok := fake.files[fileID]; !ok {
		return filestore.ErrNotFound
	}
	delete(fake.files, fileID)
	fake.purged = append(fake.purged, fileID)
	return nil
}

func localTestServerWithFiles(t *testing.T, files filestore.Files) *Server {
	t.Helper()
	store, local := newTestStores(t)
	if err := store.Save(&accounts.Account{ID: "demo-local", Label: "Demo", Provider: accounts.ProviderLocal, Address: "work@demo.invalid", Enabled: true}); err != nil {
		t.Fatalf("save account: %v", err)
	}
	return newTestServer(t, store, local, ServerParts{Files: files})
}

func messageWithAttachments(subject string, attachments ...string) string {
	var parts []string
	for i, content := range attachments {
		parts = append(parts, fmt.Sprintf(`{"filename":"part-%d.pdf","content_type":"application/pdf","content_id":"<p%d@cid>","content_base64":%q}`,
			i, i, base64.StdEncoding.EncodeToString([]byte(content))))
	}
	return fmt.Sprintf(`{"messages":[{"provider_message_id":"WITH-ATT","subject":%q,"from":{"email":"a@x.example"},
		"received_at":"2026-10-01T09:30:00Z","body_html":"<p>see attached</p>","attachments":[%s]}]}`,
		subject, strings.Join(parts, ","))
}

func TestAnImportedAttachmentIsServedThroughMailstackWithFileStoresHeaders(t *testing.T) {
	fake, files := newFakeFileStore(t)
	s := localTestServerWithFiles(t, files)
	imported := importInto(t, s, "demo-local", messageWithAttachments("one", "%PDF-1 bytes"))
	id := imported.Outcomes[0].ID

	w := serve(t, s, http.MethodGet, "/api/messages/"+id+"/metadata?account=demo-local", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id": "a1"`) || !strings.Contains(w.Body.String(), `"has_attachments": true`) {
		t.Fatalf("metadata %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "file_0") {
		t.Fatalf("metadata hands out a file-store id: %s", w.Body)
	}

	w = serve(t, s, http.MethodGet, "/api/messages/"+id+"/attachments/a1?account=demo-local", "", nil)
	if w.Code != http.StatusOK || w.Body.String() != "%PDF-1 bytes" {
		t.Fatalf("attachment %d %q", w.Code, w.Body)
	}
	for _, header := range []string{"Content-Disposition", "X-Content-Type-Options", "Content-Security-Policy"} {
		if w.Header().Get(header) == "" {
			t.Fatalf("file-store's %s was not relayed", header)
		}
	}
	if w := serve(t, s, http.MethodGet, "/api/messages/"+id+"/attachments/a9?account=demo-local", "", nil); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "attachment_not_found") {
		t.Fatalf("unknown attachment %d: %s", w.Code, w.Body)
	}

	importInto(t, s, "demo-local", messageWithAttachments("one", "%PDF-1 bytes"))
	if fake.uploads != 1 {
		t.Fatalf("%d uploads after a replay, want the 1 from the first import", fake.uploads)
	}
}

func TestAChangedAttachmentKeepsTheOldRevisionsFile(t *testing.T) {
	fake, files := newFakeFileStore(t)
	s := localTestServerWithFiles(t, files)
	id := importInto(t, s, "demo-local", messageWithAttachments("one", "version one")).Outcomes[0].ID
	second := importInto(t, s, "demo-local", messageWithAttachments("two", "version two"))
	if second.Outcomes[0].Outcome != "updated" || second.Outcomes[0].Revision != 2 {
		t.Fatalf("second import %+v", second.Outcomes[0])
	}

	w := serve(t, s, http.MethodGet, "/api/messages/"+id+"/attachments/a1?account=demo-local", "", nil)
	if w.Body.String() != "version two" {
		t.Fatalf("current attachment %q", w.Body)
	}
	w = serve(t, s, http.MethodGet, "/api/messages/"+id+"/revisions/1/attachments/a1?account=demo-local", "", nil)
	if w.Code != http.StatusOK || w.Body.String() != "version one" {
		t.Fatalf("revision 1 attachment %d %q", w.Code, w.Body)
	}

	if w := serve(t, s, http.MethodPost, "/api/messages/"+id+"/perm-delete?account=demo-local", "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("perm-delete %d: %s", w.Code, w.Body)
	}
	if len(fake.files) != 0 || len(fake.purged) != 2 {
		t.Fatalf("after perm-delete file-store holds %d files and purged %v, want none left and both purged", len(fake.files), fake.purged)
	}
}

func TestARefusedUploadImportsNothingAndLeavesNoFiles(t *testing.T) {
	fake, files := newFakeFileStore(t)
	fake.refuseAt = 2
	s := localTestServerWithFiles(t, files)
	w := serve(t, s, http.MethodPost, "/api/accounts/demo-local/messages/import", messageWithAttachments("big", "small", "too big"), nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want file-store's 413: %s", w.Code, w.Body)
	}
	if len(fake.files) != 0 {
		t.Fatalf("file-store still holds %d file(s) from a refused import", len(fake.files))
	}
	r := serve(t, s, http.MethodPost, "/api/messages/resolve", `{"refs":[{"account_id":"demo-local","provider_message_id":"WITH-ATT"}]}`, nil)
	if !strings.Contains(r.Body.String(), `"not_found"`) {
		t.Fatalf("the message was stored from a refused import: %s", r.Body)
	}
}

func TestAttachmentsWithoutAFileStoreAreRefused(t *testing.T) {
	s := localTestServerWithFiles(t, nil)
	w := serve(t, s, http.MethodPost, "/api/accounts/demo-local/messages/import", messageWithAttachments("x", "bytes"), nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", w.Code, w.Body)
	}
	// Mail with no attachments still imports.
	importInto(t, s, "demo-local", twoExportedMessages)
}

func TestRevisionRoutes(t *testing.T) {
	s := localTestServer(t)
	id := importInto(t, s, "demo-local", twoExportedMessages).Outcomes[0].ID
	importInto(t, s, "demo-local", strings.Replace(twoExportedMessages, "<p>Bitte <b>prüfen</b>.</p>", "<p>neu</p>", 1))

	w := serve(t, s, http.MethodGet, "/api/messages/"+id+"/revisions?account=demo-local", "", nil)
	var list struct {
		Revisions []struct {
			Revision int64 `json:"revision"`
			Current  bool  `json:"current"`
		} `json:"revisions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Revisions) != 2 || !list.Revisions[0].Current || list.Revisions[1].Revision != 1 {
		t.Fatalf("revisions %d %s", w.Code, w.Body)
	}
	w = serve(t, s, http.MethodGet, "/api/messages/"+id+"/revisions/1/body?account=demo-local&format=html", "", nil)
	if w.Code != http.StatusOK || w.Body.String() != "<p>Bitte <b>prüfen</b>.</p>" || w.Header().Get("X-Mailstack-Revision") != "1" {
		t.Fatalf("revision 1 body %d %q", w.Code, w.Body)
	}
	w = serve(t, s, http.MethodGet, "/api/messages/"+id+"/revisions/1?account=demo-local", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"replaced_at"`) {
		t.Fatalf("revision 1 metadata %d %s", w.Code, w.Body)
	}
	if w := serve(t, s, http.MethodGet, "/api/messages/"+id+"/revisions/7?account=demo-local", "", nil); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "revision_not_found") {
		t.Fatalf("revision 7 %d %s", w.Code, w.Body)
	}
	if w := serve(t, s, http.MethodGet, "/api/messages/"+id+"/revisions/zero?account=demo-local", "", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("revision zero %d, want 400", w.Code)
	}
}
