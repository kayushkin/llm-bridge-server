package possessionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

const otherOwner = "principal_000002"

func twoOwners(t *testing.T) (*Store, *Store, *Store) {
	t.Helper()
	root, _ := newTestRoot(t, fakeReferences{known: map[string]bool{"photo_000001": true, "photo_000002": true, "m_test_machine": true, "place_000001": true}})
	ctx := context.Background()
	first, err := root.NarrowedToOwner(ctx, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	second, err := root.NarrowedToOwner(ctx, otherOwner)
	if err != nil {
		t.Fatal(err)
	}
	return root, first, second
}

func TestAnotherOwnersRecordsAreOutOfReach(t *testing.T) {
	root, first, second := twoOwners(t)
	ctx := context.Background()
	shirt, err := first.CreatePossession(ctx, fields(t, `{"name":"shirt","garment":{"category":"top","garment_type":"t_shirt"},
		"photos":[{"photo_id":"photo_000001"}]}`), testOwner)
	if err != nil {
		t.Fatal(err)
	}
	shirtID := shirt["id"].(string)
	if shirt["owner_principal_id"] != testOwner {
		t.Fatalf("owner = %v", shirt["owner_principal_id"])
	}
	if _, err := second.GetPossession(shirtID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner's possession read = %v, want not found", err)
	}
	if _, err := second.PatchPossession(ctx, shirtID, fields(t, `{"spot":"closet"}`), otherOwner); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner's possession changed = %v, want not found", err)
	}
	if _, err := second.CreateOutfit(ctx, OutfitInput{PossessionIDs: []string{shirtID}}, otherOwner); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an outfit of another owner's shirt = %v, want invalid", err)
	}
	listed, err := second.ListPossessions(PossessionFilter{})
	if err != nil || len(listed) != 0 {
		t.Fatalf("another owner lists %d possessions (%v), want none", len(listed), err)
	}
	theirs, err := second.CreatePossession(ctx, fields(t, `{"name":"watch","watch":{"movement":"quartz"}}`), otherOwner)
	if err != nil {
		t.Fatal(err)
	}
	if theirs["id"] == shirtID || theirs["id"] != "possession_000002" {
		t.Fatalf("the second owner's possession is %v: ids are numbered across owners", theirs["id"])
	}
	// One owner closing a group closes nothing of the other's.
	closed := true
	if _, err := first.ChangeWearableGroup(ctx, "watch", WearableGroupChange{Closed: &closed, Actor: testOwner}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.ChangeWearableGroup(ctx, "watch", WearableGroupChange{Closed: &closed, Actor: otherOwner}); err != nil {
		t.Fatalf("a second owner's group of the same name: %v", err)
	}

	every, err := root.Narrowed(ctx, ownership.EveryOwner())
	if err != nil {
		t.Fatal(err)
	}
	all, err := every.ListPossessions(PossessionFilter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("every owner's possessions = %d (%v), want 2", len(all), err)
	}
	if _, err := every.CreatePossession(ctx, fields(t, `{"name":"lamp"}`), testOwner); err == nil {
		t.Fatal("a store reading every owner's records must write none")
	}
}

func TestTheRoutesNarrowEachCallerToItsOwnRecords(t *testing.T) {
	root, _, _ := twoOwners(t)
	mux := http.NewServeMux()
	(&Server{Store: root, Reviewer: &Reviewer{Store: root, Photos: fakePhotos{}}, LocalMachine: func(context.Context) (string, error) { return "laptop", nil }}).RegisterHandlers(mux)
	call := func(caller ownership.Caller, method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		asCaller(caller, mux).ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
		return recorder
	}
	owner := ownership.Caller{PrincipalID: testOwner}
	other := ownership.Caller{PrincipalID: otherOwner}
	administrator := ownership.Caller{PrincipalID: "principal_000003", SeesEveryOwner: true}
	service := ownership.Caller{SeesEveryOwner: true}

	created := call(owner, "POST", "/possessions", `{"actor":"principal_000001","name":"lamp"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /possessions = %d %s", created.Code, created.Body)
	}
	if got := call(other, "GET", "/possessions/possession_000001", ""); got.Code != http.StatusNotFound {
		t.Errorf("another owner's possession = %d, want 404", got.Code)
	}
	if got := call(other, "DELETE", "/possessions/possession_000001?actor=principal_000002", ""); got.Code != http.StatusNotFound {
		t.Errorf("deleting another owner's possession = %d, want 404", got.Code)
	}
	if got := call(other, "GET", "/possessions?owner_principal_id=principal_000001", ""); got.Code != http.StatusForbidden {
		t.Errorf("naming another owner = %d, want 403", got.Code)
	}
	if got := call(other, "POST", "/possessions", `{"actor":"principal_000002","name":"chair"}`); got.Code != http.StatusCreated {
		t.Fatalf("the other owner's POST = %d %s", got.Code, got.Body)
	}
	count := func(caller ownership.Caller, path string) int {
		t.Helper()
		answer := call(caller, "GET", path, "")
		var listed []map[string]any
		if err := json.Unmarshal(answer.Body.Bytes(), &listed); err != nil {
			t.Fatalf("GET %s = %d %s", path, answer.Code, answer.Body)
		}
		return len(listed)
	}
	if got := count(owner, "/possessions"); got != 1 {
		t.Errorf("the owner lists %d possessions, want its own 1", got)
	}
	if got := count(administrator, "/possessions"); got != 2 {
		t.Errorf("an administrator lists %d possessions, want every owner's 2", got)
	}
	if got := count(administrator, "/possessions?owner_principal_id=principal_000002"); got != 1 {
		t.Errorf("an administrator naming one owner lists %d, want 1", got)
	}
	if got := call(service, "POST", "/possessions", `{"actor":"principal_000001","name":"rug"}`); got.Code != http.StatusForbidden {
		t.Errorf("the service acting as nobody making a record = %d, want 403", got.Code)
	}
	if got := call(service, "POST", "/possessions?owner_principal_id=principal_000001", `{"actor":"principal_000001","name":"rug"}`); got.Code != http.StatusCreated {
		t.Errorf("the service naming the owner = %d %s, want 201", got.Code, got.Body)
	}
	if got := call(administrator, "PATCH", "/possessions/possession_000001", `{"actor":"principal_000003","spot":"desk"}`); got.Code != http.StatusNotFound {
		t.Errorf("an administrator changing another owner's record without naming the owner = %d, want 404", got.Code)
	}
	if got := call(administrator, "PATCH", "/possessions/possession_000001?owner_principal_id=principal_000001", `{"actor":"principal_000003","spot":"desk"}`); got.Code != http.StatusOK {
		t.Errorf("an administrator naming the owner = %d %s, want 200", got.Code, got.Body)
	}
	if got := call(ownership.Caller{}, "GET", "/possessions", ""); got.Code != http.StatusUnauthorized {
		t.Errorf("a caller naming nobody = %d, want 401", got.Code)
	}

	// An original is kept for the owner the request acts for, and read by
	// that owner only.
	content := testJPEG()
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	put := func(caller ownership.Caller, path string, body []byte) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		asCaller(caller, mux).ServeHTTP(recorder, httptest.NewRequest("PUT", path, bytes.NewReader(body)))
		return recorder
	}
	kept := put(owner, "/originals/"+hash+"?extension=.jpg&added_by=principal_000001", content)
	if kept.Code != http.StatusCreated || !strings.Contains(kept.Body.String(), `"source_path":"possession-store/originals/`+hash+`.jpg"`) || !strings.Contains(kept.Body.String(), `"source_machine":"laptop"`) {
		t.Fatalf("PUT /originals = %d %s", kept.Code, kept.Body)
	}
	if again := put(owner, "/originals/"+hash+"?extension=.jpg&added_by=principal_000001", content); again.Code != http.StatusOK {
		t.Errorf("keeping it again = %d %s, want 200", again.Code, again.Body)
	}
	if wrong := put(owner, "/originals/"+strings.Repeat("0", 64)+"?extension=.jpg&added_by=principal_000001", content); wrong.Code != http.StatusBadRequest {
		t.Errorf("bytes whose sha256 is not the path's = %d, want 400", wrong.Code)
	}
	ctx := context.Background()
	if read, held, err := root.OriginalBytes(ctx, testOwner, hash); err != nil || !held || !bytes.Equal(read, content) {
		t.Fatalf("the owner's original = %d bytes, %v, %v", len(read), held, err)
	}
	if _, held, err := root.OriginalBytes(ctx, otherOwner, hash); err != nil || held {
		t.Fatalf("another owner holds the original: %v %v", held, err)
	}
}
