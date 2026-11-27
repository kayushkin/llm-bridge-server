package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/locations"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photos"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/files"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// enablePhotosAndLocationsOnTestDatabase serves photo-store and
// location-store from a Postgres database of the test's own, after the
// file-store and people-store they use.
func enablePhotosAndLocationsOnTestDatabase(t *testing.T, gated *gatedTestServer) {
	t.Helper()
	enableConnectedRecordsOnTestDatabase(t, gated)
	photoModule, err := photos.Module()
	if err != nil {
		t.Fatal(err)
	}
	locationModule, err := locations.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, photoModule, locationModule)
	if err := gated.server.EnablePhotosAndLocations(PhotosAndLocationsModules{
		Photos: database.Open(photoModule), Locations: database.Open(locationModule), GeonamesDirectory: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
}

// testThumbnail is made-up JPEG bytes: the store checks only the marker.
var testThumbnail = []byte{0xFF, 0xD8, 0xFF, 0xE0, 't', 'e', 's', 't', ' ', 't', 'h', 'u', 'm', 'b'}

// importTestPhoto imports one made-up photo as principalID and answers its id.
func importTestPhoto(t *testing.T, gated *gatedTestServer, principalID, sha string) string {
	t.Helper()
	response := gated.requestAsPrincipal(t, principalID, "POST", "/photo-store/imports", map[string]any{"photos": []map[string]any{{
		"sha256": sha, "source_machine": "laptop", "source_path": "Pictures/Camera/one.jpg", "folder": "Pictures/Camera",
		"media_type": "image/jpeg", "taken_at": 1700000000, "date_source": "exif", "width": 4000, "height": 3000,
		"thumbnail_jpeg": testThumbnail,
	}}})
	if response.Code != http.StatusOK && response.Code != http.StatusCreated {
		t.Fatalf("import as %s = %d %s", principalID, response.Code, response.Body)
	}
	var result struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.IDs) != 1 {
		t.Fatalf("import answer %s: %v", response.Body, err)
	}
	return result.IDs[0]
}

func TestPhotoStoreAndLocationStoreAnswer503WithNoModule(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableConnectedRecordsOnTestDatabase(t, gated)
	for path, code := range map[string]string{"/photo-store/photos": "photo_store_off", "/location-store/places": "location_store_off"} {
		if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", path, nil); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), code) {
			t.Errorf("GET %s = %d %s, want 503 %s", path, response.Code, response.Body, code)
		}
	}
}

// A photo and its thumbnail, kept in file-store, are reached only by the
// photo's owner, an administrator and the internal service.
func TestAPhotoAndItsThumbnailAreReachedOnlyByItsOwnerAndAnAdministrator(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enablePhotosAndLocationsOnTestDatabase(t, gated)
	id := importTestPhoto(t, gated, firstTestPrincipalID, strings.Repeat("a", 64))
	path := "/photo-store/photos/" + id

	owner := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", path, nil)
	if owner.Code != http.StatusOK || decodeRecord(t, owner)["owner_principal_id"] != firstTestPrincipalID {
		t.Fatalf("the owner's GET = %d %s", owner.Code, owner.Body)
	}
	thumbnail := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", path+"/thumbnail", nil)
	if thumbnail.Code != http.StatusOK || !bytes.Equal(thumbnail.Body.Bytes(), testThumbnail) {
		t.Fatalf("the owner's thumbnail = %d, %d bytes", thumbnail.Code, thumbnail.Body.Len())
	}
	storedFiles, err := gated.server.fileStoreModule.Load().List(files.Filter{OwnerService: photoThumbnailOwnerService, Limit: 100})
	if err != nil || len(storedFiles) != 1 || storedFiles[0].OwnerRef != id || storedFiles[0].UploadedByPrincipalID != firstTestPrincipalID {
		t.Fatalf("file-store holds %d thumbnail files (%v), want one for %s", len(storedFiles), err, id)
	}

	for _, denied := range []string{path, path + "/thumbnail"} {
		if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", denied, nil); response.Code != http.StatusNotFound {
			t.Errorf("another principal's GET %s = %d, want 404", denied, response.Code)
		}
	}
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", "/photo-store/photos", nil); response.Code != http.StatusOK || strings.Contains(response.Body.String(), id) {
		t.Errorf("another principal's list = %d %s, want none of the owner's photos", response.Code, response.Body)
	}
	if response := gated.requestAsPrincipal(t, administratorTestPrincipalID, "GET", path, nil); response.Code != http.StatusOK {
		t.Errorf("the administrator's GET = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAsService(t, "GET", path+"/thumbnail"); response.Code != http.StatusOK {
		t.Errorf("the internal service's thumbnail = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAs(t, nil, "GET", path, nil); response.Code != http.StatusUnauthorized {
		t.Errorf("nobody's GET = %d, want 401", response.Code)
	}

	// reference-store asks photo-store in this process: the administrator's
	// chip resolves and its picture is the thumbnail; another principal is
	// told of nothing.
	resolve := gated.requestAsPrincipal(t, administratorTestPrincipalID, "POST", "/reference-store/resolve", map[string]any{"ids": []string{id}})
	if resolve.Code != http.StatusOK || !strings.Contains(resolve.Body.String(), `"type":"photo"`) {
		t.Errorf("the administrator's resolve = %d %s", resolve.Code, resolve.Body)
	}
	attachment := gated.requestAsPrincipal(t, administratorTestPrincipalID, "GET", "/reference-store/attachments/"+id, nil)
	if attachment.Code != http.StatusOK || !bytes.Equal(attachment.Body.Bytes(), testThumbnail) {
		t.Errorf("the administrator's attachment = %d, %d bytes", attachment.Code, attachment.Body.Len())
	}
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "POST", "/reference-store/resolve", map[string]any{"ids": []string{id}}); response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"type":"photo"`) {
		t.Errorf("another principal's resolve = %d %s", response.Code, response.Body)
	}

	// A person on a photo must be one the photo's owner keeps.
	someoneElses := gated.requestAsPrincipal(t, secondTestPrincipalID, "POST", "/people-store/persons", map[string]any{"display_name": "Bob"})
	bobID, _ := decodeRecord(t, someoneElses)["id"].(string)
	proposal := map[string]any{"photo_id": id, "person_id": bobID, "status": "suggested", "confidence": 0.5, "evidence": "test", "proposed_by": "test"}
	if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/photo-store/photo-people", proposal); response.Code != http.StatusBadRequest {
		t.Errorf("naming another owner's person on a photo = %d %s, want 400", response.Code, response.Body)
	}
	ownPerson := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/people-store/persons", map[string]any{"display_name": "Alice"})
	proposal["person_id"], _ = decodeRecord(t, ownPerson)["id"].(string)
	if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/photo-store/photo-people", proposal); response.Code != http.StatusCreated {
		t.Errorf("naming the owner's own person on a photo = %d %s, want 201", response.Code, response.Body)
	}

	// A purge destroys the thumbnail's file.
	if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", path+"/purge", nil); response.Code != http.StatusOK {
		t.Fatalf("purge = %d %s", response.Code, response.Body)
	}
	if files, err := gated.server.fileStoreModule.Load().List(files.Filter{OwnerService: photoThumbnailOwnerService, Limit: 100}); err != nil || len(files) != 0 {
		t.Errorf("after the purge file-store holds %d thumbnail files (%v), want none", len(files), err)
	}
}

// A place is reached only by its owner and an administrator.
func TestAPlaceIsReachedOnlyByItsOwnerAndAnAdministrator(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enablePhotosAndLocationsOnTestDatabase(t, gated)
	created := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/location-store/places",
		map[string]any{"name": "Example home", "latitude": 10.0, "longitude": 20.0, "kind": "home"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body)
	}
	place := decodeRecord(t, created)
	id, _ := place["id"].(string)
	if place["owner_principal_id"] != firstTestPrincipalID || id == "" {
		t.Fatalf("the new place's id %q and owner %v", id, place["owner_principal_id"])
	}
	path := "/location-store/places/" + id
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", path, nil); response.Code != http.StatusNotFound {
		t.Errorf("another principal's GET = %d, want 404", response.Code)
	}
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "PATCH", path, map[string]any{"name": "Elsewhere"}); response.Code != http.StatusNotFound {
		t.Errorf("another principal's PATCH = %d, want 404", response.Code)
	}
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", "/location-store/places", nil); response.Code != http.StatusOK || strings.Contains(response.Body.String(), id) {
		t.Errorf("another principal's list = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAsPrincipal(t, administratorTestPrincipalID, "GET", path, nil); response.Code != http.StatusOK {
		t.Errorf("the administrator's GET = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAsService(t, "GET", path); response.Code != http.StatusOK {
		t.Errorf("the internal service's GET = %d %s", response.Code, response.Body)
	}
}

// The face passes are off by default, a pass that names people refuses to
// start without multichat, and one with every service it reads starts.
func TestPhotoFacePassesAreOffByDefaultAndRefuseSettingsThatCannotRun(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	settings, err := gated.server.readPhotoFacePassSettings()
	if err != nil || settings.grouping || settings.matching || settings.guessing {
		t.Fatalf("default settings: %+v %v", settings, err)
	}
	if err := gated.server.StartPhotoFacePasses(context.Background()); err != nil {
		t.Fatalf("starting with every pass off: %v", err)
	}
	settings.matching = true
	if err := checkPhotoFacePassSettings(settings); err == nil || !strings.Contains(err.Error(), "multichat.url") {
		t.Errorf("people matching with no multichat: %v", err)
	}
	settings.multichatURL = "http://multichat.example"
	if err := checkPhotoFacePassSettings(settings); err != nil {
		t.Errorf("people matching with every service named: %v", err)
	}
	settings.guessing = true
	settings.contextOnlyModels = append(settings.contextOnlyModels, "a-model-not-asked")
	if err := checkPhotoFacePassSettings(settings); err == nil || !strings.Contains(err.Error(), "a-model-not-asked") {
		t.Errorf("a context-only model not in the guessing models: %v", err)
	}
}

// A pass's client reaches photo-store as its owner only, even for an owner
// who is an administrator.
func TestAFacePassReachesOnlyItsOwnersPhotos(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enablePhotosAndLocationsOnTestDatabase(t, gated)
	first := importTestPhoto(t, gated, firstTestPrincipalID, strings.Repeat("b", 64))
	administrators := importTestPhoto(t, gated, administratorTestPrincipalID, strings.Repeat("c", 64))
	list := func(owner string) string {
		client, err := gated.server.photoFacePassClient(owner)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Get("http://" + facePassPhotoStoreHost + "/photos")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("list as %s = %d %s %v", owner, response.StatusCode, body, err)
		}
		return string(body)
	}
	if body := list(firstTestPrincipalID); !strings.Contains(body, first) || strings.Contains(body, administrators) {
		t.Errorf("the first owner's pass sees %s", body)
	}
	if body := list(administratorTestPrincipalID); !strings.Contains(body, administrators) || strings.Contains(body, first) {
		t.Errorf("the administrator's pass sees %s", body)
	}
}

// A pass's client reaches message-store in this process as its owner only:
// the owner's message is there, and another owner's pass sees none.
func TestAFacePassReadsOnlyItsOwnersMessages(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enablePhotosAndLocationsOnTestDatabase(t, gated)
	enableMessageActivityAndTimeLedgerModulesOnTestDatabase(t, gated)
	started := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/message-store/imports", map[string]any{"importer": "test", "description": "made up"})
	if started.Code != http.StatusCreated && started.Code != http.StatusOK {
		t.Fatalf("start = %d %s", started.Code, started.Body)
	}
	importID, _ := decodeRecord(t, started)["id"].(string)
	batch := map[string]any{
		"conversations": []map[string]any{{"platform": "discord", "source": "discord", "source_ref": "channel-1", "is_group": false,
			"participants": []map[string]any{{"handle": "carol", "display_name": "Carol"}, {"handle": "me", "display_name": "Me Self", "is_self": true}}}},
		"messages": []map[string]any{{"conversation_source": "discord", "conversation_source_ref": "channel-1", "sent_at": 1710100800000,
			"sender_handle": "carol", "kind": "text", "body": "hello there", "body_source": "platform", "source": "discord", "source_ref": "m-1", "source_hash": "h1"}},
	}
	if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/message-store/imports/"+importID+"/batches", batch); response.Code != http.StatusOK && response.Code != http.StatusCreated {
		t.Fatalf("batch = %d %s", response.Code, response.Body)
	}
	messagesSeenBy := func(owner string) int {
		client, err := gated.server.photoFacePassClient(owner)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Get("http://" + facePassMessageStoreHost + "/messages")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var page struct {
			Messages []any `json:"messages"`
		}
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("messages as %s = %d %v", owner, response.StatusCode, err)
		}
		return len(page.Messages)
	}
	if seen := messagesSeenBy(firstTestPrincipalID); seen != 1 {
		t.Errorf("the owner's pass sees %d messages, want 1", seen)
	}
	if seen := messagesSeenBy(administratorTestPrincipalID); seen != 0 {
		t.Errorf("an administrator's pass sees %d of another owner's messages, want 0", seen)
	}
}
