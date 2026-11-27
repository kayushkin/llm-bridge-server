package possessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

//go:embed testdata/standalone_schema.sql
var standaloneSchema string

// standaloneFile writes a standalone store's file holding a few made-up
// records, two of them pointing forward: a possession merged into a later
// one.
func standaloneFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "possession-store.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range append([]string{standaloneSchema}, []string{
		`INSERT INTO possessions (id, seq, name, status, created_by, created_at, updated_at, deleted_at, merged_into_id) VALUES ('possession_000004', 4, 'old shirt', 'owned', 'principal_000001', 10, 10, 20, 'possession_000007')`,
		`INSERT INTO possessions (id, seq, name, status, created_by, created_at, updated_at) VALUES ('possession_000007', 7, 'shirt', 'owned', 'principal_000001', 11, 11)`,
		`INSERT INTO garments (id, seq, possession_id, category) VALUES ('garment_000003', 3, 'possession_000007', 'top')`,
		`INSERT INTO possession_photos (possession_id, photo_id, is_cover, linked_by, linked_at) VALUES ('possession_000007', 'photo_000009', 1, 'principal_000001', 12)`,
		`INSERT INTO outfits (id, seq, status, created_by, created_at, updated_at, favorite, tags) VALUES ('outfit_000002', 2, 'kept', 'principal_000001', 13, 13, 1, '["work"]')`,
		`INSERT INTO outfit_possessions (outfit_id, possession_id, position) VALUES ('outfit_000002', 'possession_000007', 0)`,
		`INSERT INTO revisions (id, record_id, record, changed_by, changed_at) VALUES (5, 'possession_000004', '{"table":"possessions","id":"possession_000004"}', 'principal_000001', 20)`,
	}...) {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

// carryingOriginals is memoryOriginals that carries files too.
type carryingOriginals struct{ *memoryOriginals }

func (originals carryingOriginals) KeepCopyOf(ctx context.Context, owner, sum, mediaType, path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return originals.Keep(ctx, owner, sum, mediaType, content)
}

func TestAStandaloneFileIsImportedWholeWithItsIdsAndOriginals(t *testing.T) {
	root, memory := newTestRoot(t, fakeReferences{})
	root.originals = carryingOriginals{memory}
	ctx := context.Background()
	originalsDirectory := t.TempDir()
	content := testJPEG()
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(originalsDirectory, hash+".jpg"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	counts, err := ImportStandaloneDatabase(ctx, root, standaloneFile(t), originalsDirectory, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Rows["possessions"] != 2 || counts.Rows["garments"] != 1 || counts.Rows["revisions"] != 1 || counts.Originals != 1 {
		t.Fatalf("counts = %s", counts.Summary())
	}
	store, err := root.NarrowedToOwner(ctx, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	shirt, err := store.GetPossession("possession_000007", false)
	if err != nil || shirt["kind"] != "garment" || shirt["cover_photo_id"] != "photo_000009" || shirt["owner_principal_id"] != testOwner {
		t.Fatalf("shirt = %v %v", shirt, err)
	}
	outfit, err := store.GetOutfit("outfit_000002", false)
	if err != nil || !outfit.Favorite || len(outfit.Tags) != 1 || outfit.PossessionIDs[0] != "possession_000007" {
		t.Fatalf("outfit = %+v %v", outfit, err)
	}
	if read, held, err := root.OriginalBytes(ctx, testOwner, hash); err != nil || !held || !bytes.Equal(read, content) {
		t.Fatalf("original: %d bytes %v %v", len(read), held, err)
	}
	created, err := store.CreatePossession(ctx, fields(t, `{"name":"hat"}`), testOwner)
	if err != nil || created["id"] != "possession_000008" {
		t.Fatalf("the next possession = %v %v, want possession_000008", created["id"], err)
	}
	if _, err := ImportStandaloneDatabase(ctx, root, standaloneFile(t), originalsDirectory, otherOwner); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("a second import = %v, want a refusal of a module that holds rows", err)
	}
}

func TestAnImportThatFailsLeavesNothing(t *testing.T) {
	root, memory := newTestRoot(t, fakeReferences{})
	root.originals = carryingOriginals{memory}
	ctx := context.Background()
	path := standaloneFile(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// A row pointing at a possession the file does not hold.
	if _, err := db.Exec(`INSERT INTO outfit_possessions (outfit_id, possession_id, position) VALUES ('outfit_000002', 'possession_000099', 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	originalsDirectory := t.TempDir()
	content := testJPEG()
	sum := sha256.Sum256(content)
	if err := os.WriteFile(filepath.Join(originalsDirectory, hex.EncodeToString(sum[:])+".jpg"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportStandaloneDatabase(ctx, root, path, originalsDirectory, testOwner); err == nil {
		t.Fatal("an import with a dangling reference must fail")
	}
	if len(memory.files) != 0 {
		t.Fatalf("a failed import left %d carried files", len(memory.files))
	}
	store, err := root.NarrowedToOwner(ctx, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPossession("possession_000007", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a failed import left rows: %v", err)
	}
}
