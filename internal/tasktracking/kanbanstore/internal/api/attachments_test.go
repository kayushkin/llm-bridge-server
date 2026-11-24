package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kayushkin/kanban-store/internal/api"
	"github.com/kayushkin/kanban-store/internal/bundlestore"
	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/filestore"
	"github.com/kayushkin/kanban-store/internal/grantstore"
	"github.com/kayushkin/kanban-store/internal/llmbridge"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/noteboard"
	"github.com/kayushkin/kanban-store/internal/principalstore"
)

const fakeFileStoreToken = "file-store-token-for-the-tests-0123456789abcdef"

// fakeFileStore stands in for file-store, written from its CONTRACT.md and
// server.go (2026-09-19): the token on every route, raw-body upload with the
// record's fields in the query, 413 over the limit, a list by owner, content
// served as an attachment with nosniff, a reversible DELETE and ?hard=true.
type fakeFileStore struct {
	mu      sync.Mutex
	seq     int
	files   map[string]map[string]any
	bytes   map[string][]byte
	deleted map[string]bool
	purged  []string
	limit   int
	down    bool
}

func newFakeFileStore() *fakeFileStore {
	return &fakeFileStore{files: map[string]map[string]any{}, bytes: map[string][]byte{}, deleted: map[string]bool{}, limit: 1 << 20}
}

func (f *fakeFileStore) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /files", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		query := r.URL.Query()
		switch {
		case query.Get("filename") == "":
			writeJSON(w, 400, map[string]string{"error": "filename is required"})
			return
		case len(body) > f.limit:
			writeJSON(w, 413, map[string]string{"error": fmt.Sprintf("file is larger than this store accepts: the limit is %d bytes (GET /limits)", f.limit)})
			return
		}
		f.mu.Lock()
		f.seq++
		id := fmt.Sprintf("file_%06d", f.seq)
		hash := sha256.Sum256(body)
		record := map[string]any{
			"id": id, "sha256": hex.EncodeToString(hash[:]), "size_bytes": len(body), "content_type": r.Header.Get("Content-Type"),
			"detected_content_type": http.DetectContentType(body), "filename": query.Get("filename"),
			"owner_service": query.Get("owner_service"), "owner_ref": query.Get("owner_ref"),
			"uploaded_by_principal_id": query.Get("uploaded_by_principal_id"), "created_at": 1789770000,
		}
		f.files[id], f.bytes[id] = record, body
		f.mu.Unlock()
		writeJSON(w, 201, record)
	})
	mux.HandleFunc("GET /files", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []map[string]any{}
		for i := 1; i <= f.seq; i++ {
			id := fmt.Sprintf("file_%06d", i)
			record, held := f.files[id]
			if held && (!f.deleted[id] || r.URL.Query().Get("include_deleted") == "true") && record["owner_service"] == r.URL.Query().Get("owner_service") && record["owner_ref"] == r.URL.Query().Get("owner_ref") {
				out = append(out, record)
			}
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("GET /files/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body, held := f.bytes[r.PathValue("id")]
		gone := f.deleted[r.PathValue("id")]
		f.mu.Unlock()
		if !held || gone {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''served-by-file-store")
		w.Write(body)
	})
	mux.HandleFunc("DELETE /files/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, held := f.files[id]; !held {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		if r.URL.Query().Get("hard") == "true" {
			delete(f.files, id)
			delete(f.bytes, id)
			f.purged = append(f.purged, id)
		} else {
			f.deleted[id] = true
		}
		w.WriteHeader(204)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.down {
			writeJSON(w, 500, map[string]string{"error": "disk is gone"})
			return
		}
		if r.Header.Get(filestore.ServiceTokenHeader) != fakeFileStoreToken {
			writeJSON(w, 401, map[string]string{"error": "send the token"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func setupWithAttachments(t *testing.T) (http.Handler, *fakeGrantStore, *fakeFileStore) {
	t.Helper()
	grants := &fakeGrantStore{grantsByPrincipal: map[string][]grantstore.BoardGrant{}}
	servers := map[string]http.Handler{
		"grants": grants.handler(), "principals": newFakePrincipalStore().handler(), "bridge": newFakeLLMBridgeServer().handler(),
		"bundles": newFakeBundleStore().handler(), "notes": newFakeNoteboard().handler(),
	}
	files := newFakeFileStore()
	servers["files"] = files.handler()
	urls := map[string]string{}
	for name, handler := range servers {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		urls[name] = server.URL
	}
	store, err := db.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	a := api.New(store, noteboard.New(urls["notes"]), principalstore.New(urls["principals"]), llmbridge.New(urls["bridge"]), bundlestore.New(urls["bundles"]), settingsRegistryForTests(t))
	a.SetReferenceStore(fakeReferenceStore(t))
	// Cleanups run last-registered first, so this waits for trigger
	// dispatches before the store above is closed.
	t.Cleanup(a.WaitForMessageTriggers)
	a.SetPrincipalEnforcement(api.PrincipalEnforcement{ServiceToken: testServiceToken, Grants: grantstore.New(urls["grants"], "grant-store-token")})
	a.SetFileStore(filestore.New(urls["files"], fakeFileStoreToken))
	return a.Handler(), grants, files
}

func sendBytes(t *testing.T, h http.Handler, who map[string]string, target, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("POST", target, bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	for name, value := range who {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder
}

// file-store hands a file to whoever holds its token, so the only thing between
// a person and somebody else's attachment is this store. A file is reached
// through the card it hangs on, under that card's rules, and no other way.
func TestAFileIsReachedOnlyThroughTheCardItHangsOn(t *testing.T) {
	h, grants, _ := setupWithAttachments(t)
	f := buildTwoBoards(t, h, grants)
	invoice := []byte("%PDF-1.7 the disputed invoice")

	w := sendBytes(t, h, asPrincipal(alice), "/api/cards/"+f.supportCardID+"/attachments?filename=invoice.pdf", "application/pdf", invoice)
	mustStatus(t, w, 201, "alice, who edits Support, attaches a file")
	var attached model.CardAttachment
	decodeSuccessfulResponse(t, w, &attached)
	var record map[string]any
	json.Unmarshal(attached.File, &record)
	if attached.Visibility != model.NoteVisibilityInternal || attached.AttachedBy != alice ||
		record["filename"] != "invoice.pdf" || record["owner_ref"] != f.supportCardID || record["uploaded_by_principal_id"] != alice {
		t.Fatalf("attachment = %+v with file %v; want internal, by alice, file-store's own record for this card", attached, record)
	}
	contentPath := "/api/cards/" + f.supportCardID + "/attachments/" + attached.FileID + "/content"

	w = requestAs(t, h, asPrincipal(alice), "GET", contentPath, nil)
	mustStatus(t, w, 200, "alice downloads it")
	if !bytes.Equal(w.Body.Bytes(), invoice) {
		t.Errorf("downloaded %q", w.Body.String())
	}
	// file-store's headers are what keep an upload from running in a browser;
	// they must arrive as it sent them.
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Disposition") != "attachment; filename*=UTF-8''served-by-file-store" {
		t.Errorf("file-store's headers did not come through: %v", w.Header())
	}

	// bob views Finance only. Support's card is 404 to him, files and all.
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "GET", contentPath, nil), 404, "bob downloads a file on a card he cannot see")
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "GET", "/api/cards/"+f.supportCardID+"/attachments", nil), 404, "bob lists them")
	// And he cannot reach it by naming it under a card he CAN see.
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "GET", "/api/cards/"+f.financeCardID+"/attachments/"+attached.FileID+"/content", nil),
		404, "bob asks for Support's file through a Finance card")
	// A viewer reads and does not write.
	mustStatus(t, sendBytes(t, h, asPrincipal(bob), "/api/cards/"+f.financeCardID+"/attachments?filename=x.txt", "text/plain", []byte("x")), 403, "a viewer attaches")
}

func TestAnAttachmentSaysWhoItIsForAndSitsOnTheTimeline(t *testing.T) {
	h, grants, files := setupWithAttachments(t)
	f := buildTwoBoards(t, h, grants)
	base := "/api/cards/" + f.supportCardID + "/attachments"
	mustStatus(t, sendBytes(t, h, asPrincipal(alice), base+"?filename=internal-notes.txt", "text/plain", []byte("do not concede")), 201, "an internal file")
	w := sendBytes(t, h, asPrincipal(alice), base+"?filename=reply.pdf&visibility=requester", "application/pdf", []byte("%PDF our reply"))
	mustStatus(t, w, 201, "a file for the requester")
	var forRequester model.CardAttachment
	decodeSuccessfulResponse(t, w, &forRequester)
	mustStatus(t, sendBytes(t, h, asPrincipal(alice), base+"?filename=x.txt&visibility=everyone", "text/plain", []byte("x")), 400, "an unknown visibility")

	namesOf := func(query string) []string {
		w := requestAs(t, h, asPrincipal(alice), "GET", base+query, nil)
		mustStatus(t, w, 200, "list"+query)
		var attachments []model.CardAttachment
		decodeSuccessfulResponse(t, w, &attachments)
		names := []string{}
		for _, attachment := range attachments {
			var record map[string]any
			json.Unmarshal(attachment.File, &record)
			names = append(names, fmt.Sprint(record["filename"]))
		}
		return names
	}
	if got := namesOf("?visibility=requester"); len(got) != 1 || got[0] != "reply.pdf" {
		t.Errorf("requester attachments = %v, want only reply.pdf", got)
	}
	if got := namesOf(""); len(got) != 2 {
		t.Errorf("all attachments = %v, want both", got)
	}

	mustStatus(t, requestAs(t, h, asPrincipal(alice), "DELETE", base+"/"+forRequester.FileID, nil), 204, "alice removes one")
	if !files.deleted[forRequester.FileID] || len(files.purged) != 0 {
		t.Errorf("removing an attachment must delete it in file-store reversibly; deleted=%v purged=%v", files.deleted, files.purged)
	}
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "GET", base+"/"+forRequester.FileID+"/content", nil), 404, "a removed attachment")

	w = requestAs(t, h, asService, "GET", "/api/cards/"+f.supportCardID+"/events", nil)
	var events []model.CardEvent
	decodeSuccessfulResponse(t, w, &events)
	seen := []string{}
	for _, event := range events {
		if event.Kind == model.EventAttachmentAdded || event.Kind == model.EventAttachmentRemoved {
			var detail model.AttachmentEventDetail
			json.Unmarshal(event.Detail, &detail)
			seen = append(seen, string(event.Kind)+" "+detail.Filename+" "+string(detail.Visibility)+" by "+event.Actor)
		}
	}
	want := []string{"attachment_added internal-notes.txt internal by " + alice, "attachment_added reply.pdf requester by " + alice, "attachment_removed reply.pdf requester by " + alice}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Errorf("timeline = %v\nwant     %v", seen, want)
	}
}

// file-store's refusals are the uploader's to read, as it wrote them. Its
// failures are this store's upstream failing, and leave nothing behind here.
func TestFileStoresRefusalsComeThroughAndItsFailuresLeaveNothing(t *testing.T) {
	h, grants, files := setupWithAttachments(t)
	f := buildTwoBoards(t, h, grants)
	base := "/api/cards/" + f.supportCardID + "/attachments"
	files.limit = 10
	w := sendBytes(t, h, asPrincipal(alice), base+"?filename=big.bin", "application/octet-stream", bytes.Repeat([]byte("x"), 11))
	mustStatus(t, w, 413, "a file over file-store's limit")
	if !bytes.Contains(w.Body.Bytes(), []byte("the limit is 10 bytes")) {
		t.Errorf("413 body = %s, want file-store's own words", w.Body.String())
	}
	mustStatus(t, sendBytes(t, h, asPrincipal(alice), base, "text/plain", []byte("x")), 400, "no filename, as file-store refuses it")

	files.down = true
	mustStatus(t, sendBytes(t, h, asPrincipal(alice), base+"?filename=a.txt", "text/plain", []byte("x")), 502, "file-store failing")
	files.down = false
	w = requestAs(t, h, asPrincipal(alice), "GET", base, nil)
	mustStatus(t, w, 200, "list")
	var attachments []model.CardAttachment
	decodeSuccessfulResponse(t, w, &attachments)
	if len(attachments) != 0 {
		t.Errorf("refused and failed uploads left %d attachments", len(attachments))
	}
}

// A purged card can no longer say who may read its files, so they are
// destroyed with it — and if they cannot be, the card stays.
func TestPurgingACardDestroysItsFilesOrDoesNotHappen(t *testing.T) {
	h, grants, files := setupWithAttachments(t)
	f := buildTwoBoards(t, h, grants)
	w := sendBytes(t, h, asService, "/api/cards/"+f.supportCardID+"/attachments?filename=a.txt", "text/plain", []byte("a"))
	mustStatus(t, w, 201, "attach")
	var attached model.CardAttachment
	decodeSuccessfulResponse(t, w, &attached)

	files.down = true
	mustStatus(t, requestAs(t, h, asService, "DELETE", "/api/cards/"+f.supportCardID+"?hard=true", nil), 502, "purge while file-store cannot destroy the files")
	files.down = false
	mustStatus(t, requestAs(t, h, asService, "GET", "/api/cards/"+f.supportCardID, nil), 200, "the card is still there")

	// A file taken off the card earlier has no row here and is still in
	// file-store, deleted and restorable. The purge has to reach it too.
	w = sendBytes(t, h, asService, "/api/cards/"+f.supportCardID+"/attachments?filename=removed-earlier.txt", "text/plain", []byte("b"))
	mustStatus(t, w, 201, "attach a second")
	var removedEarlier model.CardAttachment
	decodeSuccessfulResponse(t, w, &removedEarlier)
	mustStatus(t, requestAs(t, h, asService, "DELETE", "/api/cards/"+f.supportCardID+"/attachments/"+removedEarlier.FileID, nil), 204, "take it off the card")

	mustStatus(t, requestAs(t, h, asService, "DELETE", "/api/cards/"+f.supportCardID+"?hard=true", nil), 200, "purge")
	purged := map[string]bool{}
	for _, id := range files.purged {
		purged[id] = true
	}
	if len(purged) != 2 || !purged[attached.FileID] || !purged[removedEarlier.FileID] {
		t.Errorf("file-store purged %v, want the live file %s and the one removed earlier, %s", files.purged, attached.FileID, removedEarlier.FileID)
	}
}

func TestWithoutAFileStoreAttachmentsAreUnknownNotEmpty(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "GET", "/api/cards/"+f.supportCardID+"/attachments", nil), 503, "list with no file-store configured")
}
