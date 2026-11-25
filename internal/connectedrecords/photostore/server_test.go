package photostore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// The made-up people the tests act as.
const (
	alicePrincipalID = "principal_000001"
	bobPrincipalID   = "principal_000002"
)

var (
	alice         = ownership.Caller{PrincipalID: alicePrincipalID}
	bob           = ownership.Caller{PrincipalID: bobPrincipalID}
	administrator = ownership.Caller{PrincipalID: "principal_000009", SeesEveryOwner: true}
	testContext   = context.Background()
	aliceScope    = ownership.OwnedBy(alicePrincipalID)
)

// The test server reads who the request acts as from these headers, as the
// real server reads it from the credential it verified. A request with
// noCallerHeader carries no caller at all.
const (
	callerPrincipalHeader     = "X-Test-Caller-Principal"
	callerAdministratorHeader = "X-Test-Caller-Administrator"
	noCallerHeader            = "X-Test-No-Caller"
)

func withTestCaller(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(noCallerHeader) == "" {
			caller := ownership.Caller{PrincipalID: r.Header.Get(callerPrincipalHeader), SeesEveryOwner: r.Header.Get(callerAdministratorHeader) != ""}
			r = r.WithContext(ownership.ContextWithCaller(r.Context(), caller))
		}
		next.ServeHTTP(w, r)
	})
}

type testService struct {
	t          *testing.T
	store      *Store
	db         *sql.DB
	server     *httptest.Server
	thumbnails *fakeThumbnailFiles
	people     *fakePeople
	// caller is who call acts as; nil sends no caller.
	caller *ownership.Caller
}

// as is the same service called as caller.
func (s *testService) as(caller ownership.Caller) *testService {
	copied := *s
	copied.caller = &caller
	return &copied
}

// withoutCaller is the same service called with no caller on the request.
func (s *testService) withoutCaller() *testService {
	copied := *s
	copied.caller = nil
	return &copied
}

// newTestDatabase gives the test a Postgres database of its own holding the
// module, migrated (postgresstorage/postgrestest), and the module's pool on it.
func newTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	database := postgrestest.NewDatabase(t, module)
	return database.Open(module)
}

// newTestService serves a store on a database of its own, called as Alice,
// who keeps person_000001 to person_000003 in people-store.
func newTestService(t *testing.T) *testService {
	t.Helper()
	db := newTestDatabase(t)
	thumbnails := newFakeThumbnailFiles()
	people := &fakePeople{persons: map[string]map[string]bool{}}
	people.keep(alicePrincipalID, "person_000001", "person_000002", "person_000003")
	store := New(db, thumbnails, people)
	mux := http.NewServeMux()
	RegisterHandlers(mux, store)
	server := httptest.NewServer(withTestCaller(mux))
	t.Cleanup(server.Close)
	return &testService{t: t, store: store, db: db, server: server, thumbnails: thumbnails, people: people, caller: &alice}
}

func (s *testService) call(method, path string, body any, out any) (int, []byte) {
	s.t.Helper()
	var reader io.Reader
	switch typed := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(typed)
	default:
		encoded, _ := json.Marshal(typed)
		reader = bytes.NewReader(encoded)
	}
	request, _ := http.NewRequest(method, s.server.URL+path, reader)
	request.Header.Set("Content-Type", "application/json")
	if s.caller == nil {
		request.Header.Set(noCallerHeader, "true")
	} else {
		request.Header.Set(callerPrincipalHeader, s.caller.PrincipalID)
		if s.caller.SeesEveryOwner {
			request.Header.Set(callerAdministratorHeader, "true")
		}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	if out != nil && len(answer) > 0 && strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(answer, out); err != nil {
			s.t.Fatalf("%s %s: %v: %s", method, path, err, answer)
		}
	}
	return response.StatusCode, answer
}

var jpeg = []byte{0xFF, 0xD8, 0xFF, 0xE0, 'f', 'a', 'k', 'e'}

func photoOf(hash string, takenAt int64) ImportPhoto {
	digest := sha256.Sum256([]byte(hash))
	return ImportPhoto{Sha256: hex.EncodeToString(digest[:]), SourceMachine: "laptop", SourcePath: "C:/Pictures/Camera/" + hash + ".jpg",
		Folder: "Pictures/Camera", MediaType: "image/jpeg", TakenAt: takenAt, DateSource: DateFromExif, Width: 1024, Height: 768, ThumbnailJPEG: jpeg}
}

func TestImportCreatesSkipsAndUpdatesByHashAndKeepsTheDescription(t *testing.T) {
	service := newTestService(t)
	var result ImportResult
	if status, body := service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("a", 1300000000), photoOf("b", 1200000000)}}, &result); status != http.StatusOK {
		t.Fatalf("import = %d: %s", status, body)
	}
	if result.Created != 2 || result.IDs[0] != "photo_000001" {
		t.Fatalf("result = %+v", result)
	}
	service.call(http.MethodPatch, "/photos/photo_000001", map[string]any{"description": "On the beach", "described_by": "test"}, nil)

	moved := photoOf("a", 1300000000)
	moved.SourcePath = "D:/Pictures/Camera/a.jpg"
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{moved, photoOf("b", 1200000000)}}, &result)
	if result.Updated != 1 || result.Skipped != 1 || result.Created != 0 {
		t.Fatalf("second import = %+v, want one updated and one skipped", result)
	}
	var photo Photo
	service.call(http.MethodGet, "/photos/photo_000001", nil, &photo)
	if photo.SourcePath != "D:/Pictures/Camera/a.jpg" || photo.Description != "On the beach" {
		t.Errorf("after a re-import = %+v: the path moves, the description stays", photo)
	}
	var revisions []Revision
	service.call(http.MethodGet, "/photos/photo_000001/revisions", nil, &revisions)
	if len(revisions) != 2 || revisions[0].Reason != "import" || revisions[1].Reason != "update" {
		t.Errorf("revisions = %+v, want import then update, newest first", revisions)
	}
}

func TestAnImportWithOneBadPhotoWritesNothing(t *testing.T) {
	service := newTestService(t)
	latitudeOnly := photoOf("c", 1)
	latitude := 39.5
	latitudeOnly.Latitude = &latitude
	for name, bad := range map[string]ImportPhoto{
		"short hash":    func() ImportPhoto { p := photoOf("d", 1); p.Sha256 = "abc"; return p }(),
		"no path":       func() ImportPhoto { p := photoOf("d", 1); p.SourcePath = ""; return p }(),
		"unknown date":  func() ImportPhoto { p := photoOf("d", 1); p.DateSource = "guess"; return p }(),
		"date but zero": func() ImportPhoto { p := photoOf("d", 0); return p }(),
		"half a place":  latitudeOnly,
		"not a jpeg":    func() ImportPhoto { p := photoOf("d", 1); p.ThumbnailJPEG = []byte("png"); return p }(),
	} {
		status, body := service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("e", 1), bad}}, nil)
		if status != http.StatusBadRequest || !strings.Contains(string(body), "photo 1") {
			t.Errorf("%s = %d %s, want 400 naming photo 1", name, status, body)
		}
	}
	var health Health
	service.call(http.MethodGet, "/health", nil, &health)
	if health.Counts.Photos != 0 {
		t.Errorf("refused imports wrote %d photos", health.Counts.Photos)
	}
}

func TestListFiltersByTimeFolderWordsAndPlace(t *testing.T) {
	service := newTestService(t)
	place := photoOf("g", 300)
	latitude, longitude := 39.5, -119.8
	place.Latitude, place.Longitude = &latitude, &longitude
	other := photoOf("h", 200)
	other.Folder = "Pictures/Phone"
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("f", 100), other, place}}, nil)
	service.call(http.MethodPatch, "/photos/photo_000003", map[string]any{"description": "Skating at the 100% park", "described_by": "test", "tags": []string{"skating"}}, nil)

	titles := func(path string) string {
		var photos []Photo
		if status, body := service.call(http.MethodGet, path, nil, &photos); status != http.StatusOK {
			t.Fatalf("%s = %d %s", path, status, body)
		}
		var ids []string
		for _, photo := range photos {
			ids = append(ids, photo.ID)
		}
		return strings.Join(ids, ",")
	}
	for path, want := range map[string]string{
		"/photos":                                         "photo_000001,photo_000002,photo_000003",
		"/photos?from=150&to=300":                         "photo_000002",
		"/photos?folder=Pictures/Phone":                   "photo_000002",
		"/photos?q=skating+100%25":                        "photo_000003",
		"/photos?q=50%25":                                 "",
		"/photos?tag=skating":                             "photo_000003",
		"/photos?described=false":                         "photo_000001,photo_000002",
		"/photos?has_gps=true":                            "photo_000003",
		"/photos?sha256=" + place.Sha256:                  "photo_000003",
		"/photos?sha256=" + strings.ToUpper(other.Sha256): "photo_000002",
		"/photos?sha256=" + strings.Repeat("0", 64):       "",
	} {
		if got := titles(path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if status, _ := service.call(http.MethodGet, "/photos?described=maybe", nil, nil); status != http.StatusBadRequest {
		t.Errorf("described=maybe = %d, want 400", status)
	}
}

func TestTagsAreCountedOverLivePhotosMostUsedFirst(t *testing.T) {
	service := newTestService(t)
	var none []TagCount
	if service.call(http.MethodGet, "/tags", nil, &none); none == nil || len(none) != 0 {
		t.Fatalf("an empty store's tags = %#v, want an empty list, not null", none)
	}
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("k", 100), photoOf("l", 200), photoOf("m", 300)}}, nil)
	service.call(http.MethodPatch, "/photos/photo_000001", map[string]any{"tags": []string{"dog", "beach"}}, nil)
	service.call(http.MethodPatch, "/photos/photo_000002", map[string]any{"tags": []string{"dog"}}, nil)
	service.call(http.MethodPatch, "/photos/photo_000003", map[string]any{"tags": []string{"dog", "snow"}}, nil)
	service.call(http.MethodDelete, "/photos/photo_000003", nil, nil)

	var counts []TagCount
	if status, body := service.call(http.MethodGet, "/tags", nil, &counts); status != http.StatusOK {
		t.Fatalf("GET /tags = %d %s", status, body)
	}
	want := []TagCount{{Tag: "dog", Photos: 2}, {Tag: "beach", Photos: 1}}
	if fmt.Sprint(counts) != fmt.Sprint(want) {
		t.Errorf("tags = %v, want %v: a deleted photo's tags do not count", counts, want)
	}
}

func TestTheDateSourcesAreServed(t *testing.T) {
	service := newTestService(t)
	var sources []string
	service.call(http.MethodGet, "/date-sources", nil, &sources)
	if strings.Join(sources, ",") != "exif,filename,mtime,message,upload,google_photos,unknown" {
		t.Errorf("date sources = %v", sources)
	}
}

func TestPatchRefusesWhatItDoesNotTakeAndADescriptionWithNoAuthor(t *testing.T) {
	service := newTestService(t)
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("i", 100)}}, nil)
	// In order: the description with no author must come before one with an
	// author, which the photo then keeps.
	for _, step := range []struct {
		body string
		want int
	}{
		{`{"descripton":"x"}`, 400},
		{`{"source_path":"x"}`, 400},
		{`{"description":null}`, 400},
		{`{"description":"x"}`, 400},
		{`{"date_source":"guess"}`, 400},
		{`{"date_source":"unknown"}`, 400},
		{`{"date_source":"unknown","taken_at":0}`, 200},
		{`{"description":"A dog","described_by":"me"}`, 200},
		{`{"tags":["",""]}`, 400},
	} {
		if status, answer := service.call(http.MethodPatch, "/photos/photo_000001", step.body, nil); status != step.want {
			t.Errorf("PATCH %s = %d %s, want %d", step.body, status, answer, step.want)
		}
	}
}

func TestDeleteIsSoftAndTheThumbnailStillServes(t *testing.T) {
	service := newTestService(t)
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("j", 100)}}, nil)
	if status, body := service.call(http.MethodGet, "/photos/photo_000001/thumbnail", nil, nil); status != http.StatusOK || !bytes.Equal(body, jpeg) {
		t.Fatalf("thumbnail = %d %q", status, body)
	}
	service.call(http.MethodDelete, "/photos/photo_000001", nil, nil)
	if status, _ := service.call(http.MethodGet, "/photos/photo_000001", nil, nil); status != http.StatusNotFound {
		t.Errorf("a deleted photo = %d, want 404", status)
	}
	if status, _ := service.call(http.MethodGet, "/photos/photo_000001/thumbnail", nil, nil); status != http.StatusOK {
		t.Errorf("a deleted photo's thumbnail = %d, want 200 so a citation keeps drawing", status)
	}
	var restored Photo
	service.call(http.MethodPost, "/photos/photo_000001/restore", nil, &restored)
	if restored.DeletedAt != 0 {
		t.Errorf("restored = %+v", restored)
	}
	if status, _ := service.call(http.MethodGet, "/photos/photo_999999/thumbnail", nil, nil); status != http.StatusNotFound {
		t.Errorf("an unknown photo's thumbnail = %d, want 404", status)
	}
}

func TestPurgeRemovesTheThumbnailAndDescriptionForGood(t *testing.T) {
	service := newTestService(t)
	photo := photoOf("k", 100)
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photo}}, nil)
	description, by := "A driver's license", "test"
	service.call(http.MethodPatch, "/photos/photo_000001", PhotoPatch{Description: &description, DescribedBy: &by}, nil)
	var purged Photo
	if status, body := service.call(http.MethodPost, "/photos/photo_000001/purge", nil, &purged); status != http.StatusOK {
		t.Fatalf("purge = %d %s", status, body)
	}
	if purged.PurgedAt == 0 || purged.DeletedAt == 0 || purged.Description != "" || purged.DescribedBy != "" {
		t.Errorf("purged = %+v", purged)
	}
	if status, _ := service.call(http.MethodGet, "/photos/photo_000001/thumbnail", nil, nil); status != http.StatusGone {
		t.Errorf("a purged photo's thumbnail = %d, want 410", status)
	}
	var revisions []Revision
	service.call(http.MethodGet, "/photos/photo_000001/revisions", nil, &revisions)
	for _, revision := range revisions {
		if revision.Photo.Description != "" {
			t.Errorf("revision %d keeps the description %q", revision.RevisionID, revision.Photo.Description)
		}
	}
	if status, _ := service.call(http.MethodPost, "/photos/photo_000001/restore", nil, nil); status != http.StatusConflict {
		t.Errorf("restoring a purged photo = %d, want 409", status)
	}
	var result ImportResult
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photo}}, &result)
	if result.Skipped != 1 {
		t.Errorf("re-import = %+v, want it skipped", result)
	}
	if status, _ := service.call(http.MethodGet, "/photos/photo_000001/thumbnail", nil, nil); status != http.StatusGone {
		t.Errorf("after a re-import the thumbnail = %d, want 410: an import must not bring it back", status)
	}
	if status, _ := service.call(http.MethodPost, "/photos/photo_000001/purge", nil, nil); status != http.StatusOK {
		t.Errorf("purging again = %d, want 200", status)
	}
}

func TestAPhotoKeepsEveryPlaceItWasFound(t *testing.T) {
	service := newTestService(t)
	first := photoOf("a", 1300000000)
	first.SourceMachine, first.SourcePath, first.Folder = "multichat", "message_000001", "chat/one"
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{first}}, nil)
	again := first
	again.SourcePath, again.Folder = "message_000009", "chat/two"
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{again}}, nil)
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{again}}, nil)

	var sightings []Sighting
	if status, body := service.call(http.MethodGet, "/sightings?source_machine=multichat", nil, &sightings); status != http.StatusOK {
		t.Fatalf("sightings = %d: %s", status, body)
	}
	if len(sightings) != 2 || sightings[0].SourcePath != "message_000001" || sightings[1].SourcePath != "message_000009" ||
		sightings[0].PhotoID != "photo_000001" || sightings[0].MediaType != "image/jpeg" {
		t.Fatalf("sightings = %+v, want the first message and the second, once each", sightings)
	}
	var none []Sighting
	service.call(http.MethodGet, "/sightings?source_machine=laptop", nil, &none)
	if len(none) != 0 {
		t.Fatalf("another machine's sightings = %+v", none)
	}
}
