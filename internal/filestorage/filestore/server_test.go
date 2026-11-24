package filestore

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

type fixture struct {
	handler http.Handler
	store   *Store
	dataDir string
}

func defaultLimits(t *testing.T) Limits {
	t.Helper()
	maximum, err := strconv.ParseInt(DefaultMaximumFileBytes, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return Limits{MaximumFileBytes: maximum, InlineContentTypes: strings.Split(DefaultInlineContentTypes, ",")}
}

// newFixture serves a store on a Postgres database of its own and a data
// directory of its own, with limits in force (the defaults when nil).
func newFixture(t *testing.T, limits *Limits) *fixture {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	store, err := New(postgrestest.NewDatabase(t, module).Open(module), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	inForce := defaultLimits(t)
	if limits != nil {
		inForce = *limits
	}
	return &fixture{handler: NewServer(store, func() Limits { return inForce }).Handler(), store: store, dataDir: dataDir}
}

func (f *fixture) do(t *testing.T, method, target, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func (f *fixture) upload(t *testing.T, filename, contentType string, body []byte) File {
	t.Helper()
	target := "/files?owner_service=kanban-store&owner_ref=card-1&filename=" + url.QueryEscape(filename)
	w := f.do(t, "POST", target, contentType, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload %s: %d %s", filename, w.Code, w.Body.String())
	}
	var file File
	if err := json.Unmarshal(w.Body.Bytes(), &file); err != nil {
		t.Fatal(err)
	}
	return file
}

// blobsOnDisk counts the kept blobs, leaving out the incoming directory.
func (f *fixture) blobsOnDisk(t *testing.T) (kept, incoming int) {
	t.Helper()
	filepath.WalkDir(filepath.Join(f.dataDir, "blobs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if strings.Contains(path, string(filepath.Separator)+"incoming"+string(filepath.Separator)) {
			incoming++
		} else {
			kept++
		}
		return nil
	})
	return kept, incoming
}

func TestAnUploadComesBackByteForByteAsADownload(t *testing.T) {
	f := newFixture(t, nil)
	body := bytes.Repeat([]byte("carrier statement, line by line\n"), 4000)
	file := f.upload(t, "statement Q3 — final.txt", "text/plain; charset=utf-8", body)
	if file.ID != "file_000001" || file.SizeBytes != int64(len(body)) || file.ContentType != "text/plain" || file.OwnerRef != "card-1" {
		t.Fatalf("stored = %+v", file)
	}
	w := f.do(t, "GET", "/files/"+file.ID+"/content", "", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), body) {
		t.Fatalf("download: %d, %d bytes, want the %d uploaded", w.Code, w.Body.Len(), len(body))
	}
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") || !strings.Contains(got, "statement%20Q3") {
		t.Errorf("Content-Disposition = %q, want an attachment carrying the name", got)
	}
}

// An uploaded page shown from the dashboard's origin would run with the
// dashboard's cookies. So a file is a download unless its declared type is on
// the inline list, the browser is always told not to sniff, and a type a
// browser runs cannot be put on that list at all.
func TestOnlyListedTypesAreEverServedInline(t *testing.T) {
	f := newFixture(t, nil)
	page := f.upload(t, "invoice.html", "text/html", []byte("<script>fetch('/api/bridge/sessions')</script>"))
	disguised := f.upload(t, "photo.png", "image/png", []byte("<html><script>alert(1)</script></html>"))
	picture := f.upload(t, "real.png", "image/png", []byte("\x89PNG\r\n\x1a\n and the rest of a picture"))

	for what, c := range map[string]struct {
		target, wantType, wantDisposition string
	}{
		"a page asked for inline":           {"/files/" + page.ID + "/content?inline=true", "application/octet-stream", "attachment"},
		"a picture asked for inline":        {"/files/" + picture.ID + "/content?inline=true", "image/png", "inline"},
		"a picture not asked for inline":    {"/files/" + picture.ID + "/content", "application/octet-stream", "attachment"},
		"a page declared a picture, inline": {"/files/" + disguised.ID + "/content?inline=true", "image/png", "inline"},
	} {
		w := f.do(t, "GET", c.target, "", nil)
		if w.Code != 200 {
			t.Fatalf("%s: %d", what, w.Code)
		}
		if got := w.Header().Get("Content-Type"); got != c.wantType {
			t.Errorf("%s: Content-Type %q, want %q", what, got, c.wantType)
		}
		if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, c.wantDisposition+";") {
			t.Errorf("%s: Content-Disposition %q, want %s", what, got, c.wantDisposition)
		}
		// What makes the disguised page safe: served as image/png with sniffing
		// off, a browser draws a broken picture and runs nothing.
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
			t.Errorf("%s: missing nosniff or the sandbox policy: %v", what, w.Header())
		}
	}
	if disguised.DetectedContentType == "image/png" {
		t.Errorf("the disguised page was detected as %q; the record should show what the bytes look like", disguised.DetectedContentType)
	}
}

// The same bytes uploaded twice are two files and one blob. The blob goes
// when the last row naming it is purged, and not before.
func TestABlobLivesAsLongAsAnyRowNamesIt(t *testing.T) {
	f := newFixture(t, nil)
	body := []byte("the same signed contract, attached to two tickets")
	first := f.upload(t, "contract.pdf", "application/pdf", body)
	second := f.upload(t, "contract (copy).pdf", "application/pdf", body)
	if first.ID == second.ID || first.SHA256 != second.SHA256 {
		t.Fatalf("two uploads = %s and %s with hashes %s, %s; want two ids, one hash", first.ID, second.ID, first.SHA256, second.SHA256)
	}
	if kept, _ := f.blobsOnDisk(t); kept != 1 {
		t.Fatalf("%d blobs on disk, want 1", kept)
	}

	// A reversible delete hides the file and keeps everything.
	if w := f.do(t, "DELETE", "/files/"+first.ID, "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := f.do(t, "GET", "/files/"+first.ID+"/content", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("a deleted file's content: %d, want 404", w.Code)
	}
	if w := f.do(t, "POST", "/files/"+first.ID+"/restore", "", nil); w.Code != 200 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	if w := f.do(t, "GET", "/files/"+first.ID+"/content", "", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), body) {
		t.Errorf("a restored file's content: %d", w.Code)
	}

	if w := f.do(t, "DELETE", "/files/"+first.ID+"?hard=true", "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("purge the first: %d", w.Code)
	}
	if w := f.do(t, "GET", "/files/"+second.ID+"/content", "", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), body) {
		t.Fatalf("purging one file took the bytes of the other: %d", w.Code)
	}
	if w := f.do(t, "DELETE", "/files/"+second.ID+"?hard=true", "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("purge the second: %d", w.Code)
	}
	if kept, _ := f.blobsOnDisk(t); kept != 0 {
		t.Errorf("%d blobs left after the last row naming them was purged", kept)
	}
}

func TestAFileOverTheLimitIsRefusedAndNothingIsKept(t *testing.T) {
	limits := defaultLimits(t)
	limits.MaximumFileBytes = 1000
	f := newFixture(t, &limits)
	target := "/files?owner_service=kanban-store&owner_ref=card-1&filename=big.bin"
	if w := f.do(t, "POST", target, "application/octet-stream", bytes.Repeat([]byte("x"), 1001)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("1001 bytes against a limit of 1000: %d %s", w.Code, w.Body.String())
	}
	// A client that lies about, or does not send, its length is caught by the copy.
	request := httptest.NewRequest("POST", target, struct{ *bytes.Reader }{bytes.NewReader(bytes.Repeat([]byte("x"), 5000))})
	request.ContentLength = -1
	request.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, request)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("5000 bytes of unknown length: %d %s", w.Code, w.Body.String())
	}
	if kept, incoming := f.blobsOnDisk(t); kept != 0 || incoming != 0 {
		t.Errorf("refused uploads left %d blobs and %d partial copies", kept, incoming)
	}
	if w := f.do(t, "POST", target, "application/octet-stream", bytes.Repeat([]byte("x"), 1000)); w.Code != http.StatusCreated {
		t.Errorf("exactly the limit: %d, want 201", w.Code)
	}
	var answered Limits
	json.Unmarshal(f.do(t, "GET", "/limits", "", nil).Body.Bytes(), &answered)
	if answered.MaximumFileBytes != 1000 {
		t.Errorf("/limits says %d, want the 1000 in force", answered.MaximumFileBytes)
	}
}

func TestAnUploadThatDoesNotSayWhatItIsIsRefused(t *testing.T) {
	f := newFixture(t, nil)
	base := "owner_service=kanban-store&owner_ref=card-1"
	for what, c := range map[string]struct {
		query, contentType string
		body               []byte
	}{
		"no filename":            {base, "text/plain", []byte("x")},
		"a path for a filename":  {base + "&filename=" + url.QueryEscape("../../etc/passwd"), "text/plain", []byte("x")},
		"a backslash path":       {base + "&filename=" + url.QueryEscape(`C:\evil.exe`), "text/plain", []byte("x")},
		"a newline in the name":  {base + "&filename=" + url.QueryEscape("a\r\nSet-Cookie: x"), "text/plain", []byte("x")},
		"no owner":               {"filename=a.txt", "text/plain", []byte("x")},
		"no content type":        {base + "&filename=a.txt", "", []byte("x")},
		"no bytes":               {base + "&filename=a.txt", "text/plain", nil},
		"a name for an uploader": {base + "&filename=a.txt&uploaded_by_principal_id=alice", "text/plain", []byte("x")},
	} {
		if w := f.do(t, "POST", "/files?"+c.query, c.contentType, c.body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", what, w.Code, w.Body.String())
		}
	}
	if kept, incoming := f.blobsOnDisk(t); kept != 0 || incoming != 0 {
		t.Errorf("refused uploads left %d blobs and %d partial copies", kept, incoming)
	}
}

// Many uploads of the same bytes at once, while earlier ones are purged: every
// file that answers 201 must be readable afterwards. Without the store's
// mutex a purge can remove the blob a finishing upload is about to name.
func TestUploadsAndPurgesOfTheSameBytesNeverLoseAFile(t *testing.T) {
	f := newFixture(t, nil)
	body := []byte("the bytes everybody attaches")
	const rounds = 40
	var wg sync.WaitGroup
	ids := make(chan string, rounds)
	for i := 0; i < rounds; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			file := f.upload(t, "shared.txt", "text/plain", body)
			if i%2 == 0 {
				if w := f.do(t, "DELETE", "/files/"+file.ID+"?hard=true", "", nil); w.Code != http.StatusNoContent {
					t.Errorf("purge %s: %d", file.ID, w.Code)
				}
				return
			}
			ids <- file.ID
		}(i)
	}
	wg.Wait()
	close(ids)
	survivors := 0
	for id := range ids {
		survivors++
		if w := f.do(t, "GET", "/files/"+id+"/content", "", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), body) {
			t.Errorf("%s answered 201 and now reads %d %s", id, w.Code, w.Body.String())
		}
	}
	if survivors != rounds/2 {
		t.Fatalf("%d survivors, want %d", survivors, rounds/2)
	}
}

func TestAServiceListsItsOwnFilesForOneThing(t *testing.T) {
	f := newFixture(t, nil)
	f.upload(t, "a.txt", "text/plain", []byte("a"))
	f.upload(t, "b.txt", "text/plain", []byte("b"))
	other := f.do(t, "POST", "/files?owner_service=mailstack&owner_ref=message-9&filename=c.txt", "text/plain", []byte("c"))
	if other.Code != 201 {
		t.Fatal(other.Body.String())
	}
	var files []File
	json.Unmarshal(f.do(t, "GET", "/files?owner_service=kanban-store&owner_ref=card-1", "", nil).Body.Bytes(), &files)
	if len(files) != 2 || files[0].Filename != "a.txt" || files[1].Filename != "b.txt" {
		t.Errorf("kanban-store's files for card-1 = %+v, want a.txt then b.txt", files)
	}
	if w := f.do(t, "GET", "/files?limit=0", "", nil); w.Code != 400 {
		t.Errorf("limit=0: %d, want 400", w.Code)
	}
}

// The server the store is mounted in stores the two settings and must refuse
// what the API refused, at start as on a change.
func TestTheValidatorsRefuseWhatTheAPIRefused(t *testing.T) {
	for _, value := range []string{"0", "-1", "lots"} {
		if ValidateMaximumFileBytes(value) == nil {
			t.Errorf("a size limit of %q passed", value)
		}
	}
	for _, value := range []string{"image/png,text/html", "image/svg+xml", "text/plain; charset=utf-8", "Image/PNG"} {
		if ValidateInlineContentTypes(value) == nil {
			t.Errorf("an inline list of %q passed", value)
		}
	}
	if err := ValidateMaximumFileBytes(DefaultMaximumFileBytes); err != nil {
		t.Errorf("the default size limit: %v", err)
	}
	if err := ValidateInlineContentTypes(DefaultInlineContentTypes); err != nil {
		t.Errorf("the default inline list: %v", err)
	}
}

// A purged file's id must never be given to another file: a reference to the
// old one, left in any other store, would then open somebody else's upload.
func TestAPurgedFilesIdIsNeverGivenOutAgain(t *testing.T) {
	f := newFixture(t, nil)
	first := f.upload(t, "theirs.txt", "text/plain", []byte("the first person's file"))
	if w := f.do(t, "DELETE", "/files/"+first.ID+"?hard=true", "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("purge: %d", w.Code)
	}
	second := f.upload(t, "someone-elses.txt", "text/plain", []byte("a different person's file"))
	if second.ID == first.ID {
		t.Fatalf("the purged id %s was given to a new file", first.ID)
	}
	if w := f.do(t, "GET", "/files/"+first.ID+"/content", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("the purged id reads %d %s, want 404", w.Code, w.Body.String())
	}
}
