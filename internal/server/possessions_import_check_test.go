package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/files"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
	_ "modernc.org/sqlite"
)

// The check of possession-store's importer against a copy of a live
// standalone store, run by hand:
//
//	go test ./internal/server -run TestAnImportedPossessionStoreAnswersAsTheStandaloneOne \
//	  -standalone-possession-store-copy=<copy.db> -standalone-possession-store-originals=<originals dir> \
//	  -possession-file-store-directory=<scratch dir on the originals' file system> \
//	  -live-possession-store-url=<standalone base URL> -live-photo-store-url=<standalone photo-store base URL> \
//	  -possession-owner=<principal id>
//
// It imports the copy, linking the originals into a scratch file-store, and
// asks the same GET routes of the import and of the live service, which it
// only reads. It reports counts and the paths that differ, never a body.
var (
	standalonePossessionStoreCopy      = flag.String("standalone-possession-store-copy", "", "a copy of a standalone possession-store database to import")
	standalonePossessionStoreOriginals = flag.String("standalone-possession-store-originals", "", "the standalone store's originals directory, only read")
	possessionFileStoreDirectory       = flag.String("possession-file-store-directory", "", "a scratch file-store directory on the originals' file system")
	livePossessionStoreURL             = flag.String("live-possession-store-url", "", "the standalone possession-store to compare with, read only")
	livePhotoStoreURL                  = flag.String("live-photo-store-url", "", "the standalone photo-store the covers are read from, read only")
	possessionImportOwner              = flag.String("possession-owner", "", "the owner every imported record is stamped with")
)

// withoutOwners is a decoded answer with every owner_principal_id taken out:
// the import adds it, and the standalone store has none.
func withoutOwners(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		delete(typed, "owner_principal_id")
		for key, inner := range typed {
			typed[key] = withoutOwners(inner)
		}
	case []any:
		for index, inner := range typed {
			typed[index] = withoutOwners(inner)
		}
	}
	return value
}

// equalButForRounding says whether two decoded answers are the same but for
// floats that differ in their last digits, as a sum added up in another
// order does.
func equalButForRounding(first, second any) bool {
	switch typed := first.(type) {
	case map[string]any:
		other, ok := second.(map[string]any)
		if !ok || len(other) != len(typed) {
			return false
		}
		for key, value := range typed {
			if !equalButForRounding(value, other[key]) {
				return false
			}
		}
		return true
	case []any:
		other, ok := second.([]any)
		if !ok || len(other) != len(typed) {
			return false
		}
		for index := range typed {
			if !equalButForRounding(typed[index], other[index]) {
				return false
			}
		}
		return true
	case float64:
		other, ok := second.(float64)
		return ok && math.Abs(typed-other) <= 1e-12*math.Max(1, math.Abs(typed))
	}
	return reflect.DeepEqual(first, second)
}

func TestAnImportedPossessionStoreAnswersAsTheStandaloneOne(t *testing.T) {
	if *standalonePossessionStoreCopy == "" || *livePossessionStoreURL == "" {
		t.Skip("no -standalone-possession-store-copy and -live-possession-store-url given")
	}
	ctx := context.Background()
	possessionModule, err := possessions.Module()
	if err != nil {
		t.Fatal(err)
	}
	fileModule, err := files.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, possessionModule, fileModule)
	fileStore, err := files.New(database.Open(fileModule), *possessionFileStoreDirectory)
	if err != nil {
		t.Fatal(err)
	}
	limits := func() files.Limits { return files.Limits{MaximumFileBytes: 1 << 30} }
	root, err := possessions.New(func(ctx context.Context, settings map[string]string) (*sql.DB, error) {
		return postgresstorage.OpenWithSessionSettings(ctx, database.URL(), possessionModule, settings)
	}, possessions.ServiceReferences{}, PossessionOriginalFilesCarrying(fileStore, limits, files.LinkBlobs))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	started := time.Now()
	counts, err := possessions.ImportStandaloneDatabase(ctx, root, *standalonePossessionStoreCopy, *standalonePossessionStoreOriginals, *possessionImportOwner)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	t.Logf("imported in %s: %s", time.Since(started).Round(time.Millisecond), counts.Summary())

	plain := &http.Client{Timeout: time.Minute}
	reviewer := &possessions.Reviewer{Store: root, CoverImagesDirectory: t.TempDir(),
		Photos: possessions.PhotoStoreSource{PhotoStore: possessions.PhotoStoreOverHTTP{URL: *livePhotoStoreURL, HTTPClient: plain}, Originals: root, LongestSide: 2048}}
	mux := http.NewServeMux()
	(&possessions.Server{Store: root, Reviewer: reviewer}).RegisterHandlers(mux)
	caller := ownership.Caller{PrincipalID: *possessionImportOwner}

	source, err := sql.Open("sqlite", "file:"+*standalonePossessionStoreCopy+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ids := func(query string) []string {
		rows, err := source.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var found []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			found = append(found, id)
		}
		return found
	}
	paths := []string{"/health", "/vocabulary", "/kinds", "/outfit-tags", "/wearable-groups", "/products", "/purchases?limit=1000",
		"/possession-merges?limit=1000", "/outfit-merges?limit=1000", "/outfit-looks?limit=1000", "/photo-reviews?limit=1000",
		"/owner-questions?limit=1000", "/owner-questions?include_answered=true&limit=1000", "/owner-questions?include_answered=true&limit=1000&offset=1000"}
	for offset := 0; offset < 800; offset += 200 {
		paths = append(paths, fmt.Sprintf("/possessions?include_deleted=true&limit=200&offset=%d", offset))
	}
	for _, kind := range possessions.KindTypes {
		paths = append(paths, "/"+kind.Route+"?limit=1000&include_deleted=true")
	}
	for _, id := range ids(`SELECT id FROM possessions ORDER BY seq`) {
		paths = append(paths, "/possessions/"+id+"?include_deleted=true", "/possessions/"+id+"/revisions")
	}
	for query, route := range map[string]string{
		`SELECT id FROM outfits ORDER BY seq`:                                                        "/outfits/%s?include_deleted=true",
		`SELECT id FROM outfit_looks ORDER BY seq`:                                                   "/outfit-looks/%s",
		`SELECT id FROM outfit_slots ORDER BY seq`:                                                   "/outfit-slots/%s",
		`SELECT id FROM outfit_rebuilds ORDER BY seq`:                                                "/outfit-rebuilds/%s",
		`SELECT id FROM possession_merges ORDER BY seq`:                                              "/possession-merges/%s",
		`SELECT id FROM outfit_merges ORDER BY seq`:                                                  "/outfit-merges/%s",
		`SELECT id FROM purchases ORDER BY seq`:                                                      "/purchases/%s",
		`SELECT id FROM products ORDER BY seq`:                                                       "/products/%s",
		`SELECT id FROM photo_reviews ORDER BY seq`:                                                  "/photo-reviews/%s",
		`SELECT id FROM sightings ORDER BY seq`:                                                      "/sightings/%s",
		`SELECT id FROM owner_questions ORDER BY seq`:                                                "/owner-questions/%s",
		`SELECT harness_machine_id FROM computers WHERE harness_machine_id IS NOT NULL ORDER BY seq`: "/harness-machines/%s/computer",
	} {
		for _, id := range ids(query) {
			paths = append(paths, fmt.Sprintf(route, id))
		}
	}

	same, rounded, different := 0, []string{}, []string{}
	for _, path := range paths {
		response, err := plain.Get(*livePossessionStoreURL + path)
		if err != nil {
			t.Fatal(err)
		}
		liveBody, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		mux.ServeHTTP(recorder, request.WithContext(ownership.ContextWithCaller(request.Context(), caller)))
		var live, imported any
		liveErr := json.Unmarshal(liveBody, &live)
		importedErr := json.Unmarshal(recorder.Body.Bytes(), &imported)
		if response.StatusCode == recorder.Code && liveErr == nil && importedErr == nil {
			live, imported = withoutOwners(live), withoutOwners(imported)
			if reflect.DeepEqual(live, imported) {
				same++
				continue
			}
			if equalButForRounding(live, imported) {
				rounded = append(rounded, path)
				continue
			}
		}
		different = append(different, fmt.Sprintf("%s (live %d, imported %d)", path, response.StatusCode, recorder.Code))
	}
	sort.Strings(different)
	t.Logf("%d of %d GET paths answered the same, %d the same but for a float's last digits", same, len(paths), len(rounded))
	for _, path := range rounded {
		t.Logf("same but for rounding: %s", path)
	}
	for _, path := range different {
		t.Logf("differs: %s", path)
	}
	if len(different) > 0 {
		t.Fail()
	}
}
