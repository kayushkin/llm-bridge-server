package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/server/routeaccess"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// enablePossessionsOnTestDatabase serves possession-store from a Postgres
// database of the test's own, after the stores it asks.
func enablePossessionsOnTestDatabase(t *testing.T, gated *gatedTestServer) {
	t.Helper()
	enablePhotosAndLocationsOnTestDatabase(t, gated)
	enableBudgetStoreModuleOnTestDatabase(t, gated)
	module, err := possessions.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	if err := gated.server.EnablePossessions(PossessionsModule{
		Open: func(ctx context.Context, settings map[string]string) (*sql.DB, error) {
			return postgresstorage.OpenWithSessionSettings(ctx, database.URL(), module, settings)
		},
		FirstReaderRole: "possession-photo-reader", SecondReaderRole: "possession-photo-second-reader",
		OwnerLocation: time.UTC, ReviewImageLongestSide: 2048, CoverCacheDirectory: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gated.server.ClosePossessions)
}

// testPhotoJPEG is a small real JPEG, which the possession image routes cut.
func testPhotoJPEG(t *testing.T) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, 40, 30))
	for x := 0; x < 40; x++ {
		for y := 0; y < 30; y++ {
			picture.Set(x, y, color.RGBA{uint8(x * 6), uint8(y * 8), 90, 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, picture, nil); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestPossessionStoreAnswers503WithNoModule(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableConnectedRecordsOnTestDatabase(t, gated)
	if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", "/possessions/possessions", nil); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "possession_store_off") {
		t.Fatalf("GET /possessions/possessions = %d %s, want 503 possession_store_off", response.Code, response.Body)
	}
}

// Every route the store registers is mounted and classified, and nothing
// else is.
func TestEveryPossessionStoreRouteIsMountedAndDescribed(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enablePossessionsOnTestDatabase(t, gated)
	registered := gated.server.possessions.Load().patterns
	described := []string{}
	for pattern := range routeaccess.PossessionStoreRouteDescriptions() {
		method, path, _ := strings.Cut(pattern, " ")
		described = append(described, method+" "+strings.TrimPrefix(path, possessionsMountPrefix))
	}
	slices.Sort(registered)
	slices.Sort(described)
	if !slices.Equal(registered, described) {
		t.Fatalf("the store registers %d routes and %d are described:\nregistered %v\ndescribed  %v", len(registered), len(described), registered, described)
	}
}

// A possession, its picture and its chip are reached only by its owner and an
// administrator, and the photo it shows is checked in the owner's photos.
func TestAPossessionIsReachedOnlyByItsOwnerAndAnAdministrator(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enablePossessionsOnTestDatabase(t, gated)
	thumbnail := testPhotoJPEG(t)
	response := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/photos/imports", map[string]any{"photos": []map[string]any{{
		"sha256": strings.Repeat("c", 64), "source_machine": "laptop", "source_path": "Pictures/Camera/desk.jpg", "folder": "Pictures/Camera",
		"media_type": "image/jpeg", "taken_at": 1700000000, "date_source": "exif", "width": 40, "height": 30, "thumbnail_jpeg": thumbnail,
	}}})
	var imported struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &imported); err != nil || len(imported.IDs) != 1 {
		t.Fatalf("import = %d %s", response.Code, response.Body)
	}
	photoID := imported.IDs[0]
	possession := map[string]any{"actor": firstTestPrincipalID, "name": "desk lamp",
		"photos": []map[string]any{{"photo_id": photoID, "box_left": 0.1, "box_top": 0.1, "box_right": 0.9, "box_bottom": 0.9}}}
	if refused := gated.requestAsPrincipal(t, secondTestPrincipalID, "POST", "/possessions/possessions", possession); refused.Code != http.StatusBadRequest {
		t.Fatalf("a possession showing another owner's photo = %d %s, want 400", refused.Code, refused.Body)
	}
	created := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/possessions/possessions", possession)
	if created.Code != http.StatusCreated || decodeRecord(t, created)["owner_principal_id"] != firstTestPrincipalID {
		t.Fatalf("the owner's POST = %d %s", created.Code, created.Body)
	}
	id := decodeRecord(t, created)["id"].(string)
	path := "/possessions/possessions/" + id

	if picture := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", path+"/image", nil); picture.Code != http.StatusOK || picture.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("the owner's picture = %d %s", picture.Code, picture.Body)
	}
	for _, denied := range []string{path, path + "/image"} {
		if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", denied, nil); response.Code != http.StatusNotFound {
			t.Errorf("another principal's GET %s = %d, want 404", denied, response.Code)
		}
	}
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", "/possessions/possessions", nil); response.Code != http.StatusOK || strings.Contains(response.Body.String(), id) {
		t.Errorf("another principal's list = %d %s, want none of the owner's", response.Code, response.Body)
	}
	if response := gated.requestAsPrincipal(t, administratorTestPrincipalID, "GET", path, nil); response.Code != http.StatusOK {
		t.Errorf("the administrator's GET = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAsService(t, "GET", path+"/image"); response.Code != http.StatusOK {
		t.Errorf("the internal service's picture = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAsServiceWithBody(t, "POST", "/possessions/possessions", `{"actor":"x","name":"rug"}`); response.Code != http.StatusForbidden {
		t.Errorf("the internal service making a record for nobody = %d %s, want 403", response.Code, response.Body)
	}
	if response := gated.requestAs(t, nil, "GET", path, nil); response.Code != http.StatusUnauthorized {
		t.Errorf("nobody's GET = %d, want 401", response.Code)
	}
	if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/possessions/possessions",
		map[string]any{"actor": firstTestPrincipalID, "name": "laptop", "computer": map[string]any{"form_factor": "laptop", "harness_machine_id": "m_no_such_machine"}}); response.Code != http.StatusBadRequest {
		t.Errorf("a computer on a machine harness-store does not have = %d %s, want 400", response.Code, response.Body)
	}

	// reference-store asks possession-store in this process, as the caller.
	resolve := gated.requestAsPrincipal(t, administratorTestPrincipalID, "POST", "/references/resolve", map[string]any{"ids": []string{id}})
	if resolve.Code != http.StatusOK || !strings.Contains(resolve.Body.String(), `"type":"possession"`) {
		t.Errorf("the administrator's resolve = %d %s", resolve.Code, resolve.Body)
	}
	attachment := gated.requestAsPrincipal(t, administratorTestPrincipalID, "GET", "/references/attachments/"+id, nil)
	if attachment.Code != http.StatusOK || attachment.Body.Len() == 0 {
		t.Errorf("the administrator's attachment = %d, %d bytes", attachment.Code, attachment.Body.Len())
	}
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "POST", "/references/resolve", map[string]any{"ids": []string{id}}); response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"type":"possession"`) {
		t.Errorf("another principal's resolve = %d %s", response.Code, response.Body)
	}
}

// A possession's budget_transaction_id is checked in budget-store, in this
// process, in the books of the possession's owner: the owner's own
// transaction is taken, and another owner's is refused as one budget-store
// does not have, even when an administrator makes the possession for them.
func TestABudgetTransactionIsCheckedInTheOwnersBooks(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enablePossessionsOnTestDatabase(t, gated)
	transactionOf := func(principalID string) string {
		t.Helper()
		account := gated.requestAsPrincipal(t, principalID, "POST", "/budget/accounts", map[string]any{"name": "Checking", "kind": "checking"})
		category := gated.requestAsPrincipal(t, principalID, "POST", "/budget/categories", map[string]any{"name": "Furniture", "kind": "expense"})
		if account.Code != http.StatusCreated || category.Code != http.StatusCreated {
			t.Fatalf("%s's account = %d %s, category = %d %s", principalID, account.Code, account.Body, category.Code, category.Body)
		}
		created := gated.requestAsPrincipal(t, principalID, "POST", "/budget/transactions", map[string]any{"occurred_on": "2026-11-02", "description": "desk lamp",
			"postings": []map[string]any{{"account_id": decodeRecord(t, account)["id"], "amount_cents": -2500}, {"category_id": decodeRecord(t, category)["id"], "amount_cents": 2500}}})
		if created.Code != http.StatusCreated {
			t.Fatalf("%s's transaction = %d %s", principalID, created.Code, created.Body)
		}
		return decodeRecord(t, created)["id"].(string)
	}
	ownTransaction := transactionOf(firstTestPrincipalID)
	othersTransaction := transactionOf(secondTestPrincipalID)

	possession := func(transactionID string) map[string]any {
		return map[string]any{"actor": firstTestPrincipalID, "name": "desk lamp", "budget_transaction_id": transactionID}
	}
	if created := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/possessions/possessions", possession(ownTransaction)); created.Code != http.StatusCreated {
		t.Errorf("a possession paid by the owner's own transaction = %d %s, want 201", created.Code, created.Body)
	}
	if refused := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/possessions/possessions", possession(othersTransaction)); refused.Code != http.StatusBadRequest {
		t.Errorf("a possession paid by another owner's transaction = %d %s, want 400", refused.Code, refused.Body)
	}
	if refused := gated.requestAsPrincipal(t, administratorTestPrincipalID, "POST", "/possessions/possessions?owner_principal_id="+firstTestPrincipalID, possession(othersTransaction)); refused.Code != http.StatusBadRequest {
		t.Errorf("an administrator's possession for the owner, paid by another owner's transaction = %d %s, want 400", refused.Code, refused.Body)
	}
}

// An original goes into file-store as the owner's, and the store says where
// photo-store should record it: this server's local machine.
func TestAnOriginalIsKeptInFileStore(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enablePossessionsOnTestDatabase(t, gated)
	content := testPhotoJPEG(t)
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	machine, err := gated.server.possessionLocalMachine(context.Background())
	kept := gated.requestAsPrincipalWithBody(t, firstTestPrincipalID, "PUT", "/possessions/originals/"+hash+"?extension=.jpg&added_by="+firstTestPrincipalID, string(content))
	if err != nil {
		if kept.Code != http.StatusInternalServerError {
			t.Fatalf("with no single local machine (%v) the answer = %d %s", err, kept.Code, kept.Body)
		}
	} else if kept.Code != http.StatusCreated || !strings.Contains(kept.Body.String(), `"source_machine":"`+machine+`"`) {
		t.Fatalf("PUT original = %d %s", kept.Code, kept.Body)
	}
	read, held, err := gated.server.possessions.Load().store.OriginalBytes(context.Background(), firstTestPrincipalID, hash)
	if err != nil || !held || !bytes.Equal(read, content) {
		t.Fatalf("the kept original = %d bytes, %v, %v", len(read), held, err)
	}
	if _, held, err := gated.server.possessions.Load().store.OriginalBytes(context.Background(), secondTestPrincipalID, hash); err != nil || held {
		t.Fatalf("another owner holds the original: %v %v", held, err)
	}
}
