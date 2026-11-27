package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"

	_ "modernc.org/sqlite"
)

// standaloneStore is a standalone file-store's data directory: its SQLite
// file and its blobs, holding contents as files of seq 1, 2, ….
func standaloneStore(t *testing.T, contents ...string) (sqlitePath, blobsDirectory string) {
	t.Helper()
	directory := t.TempDir()
	sqlitePath = filepath.Join(directory, "file-store.db")
	blobsDirectory = filepath.Join(directory, "blobs")
	source, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	schema, err := os.ReadFile("testdata/standalone_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	for index, content := range contents {
		sum := sha256.Sum256([]byte(content))
		sha := hex.EncodeToString(sum[:])
		path := filepath.Join(blobsDirectory, sha[0:2], sha[2:4], sha)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		seq := index + 1
		if _, err := source.Exec(`INSERT INTO files VALUES (?, ?, ?, ?, 'text/plain', 'text/plain; charset=utf-8', ?, 'kanban-store', 'card-1', '', ?, ?)`,
			fmt.Sprintf("file_%06d", seq), seq, sha, len(content), fmt.Sprintf("note-%d.txt", seq), 100+seq, 0); err != nil {
			t.Fatal(err)
		}
	}
	// A file purged after the last upload: its number is used up.
	if _, err := source.Exec(`UPDATE file_sequence SET last_seq = ?`, len(contents)+1); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(`INSERT INTO service_settings VALUES ('inline_content_types', 'image/png,video/mp4')`); err != nil {
		t.Fatal(err)
	}
	return sqlitePath, blobsDirectory
}

func newImportTarget(t *testing.T) (*sql.DB, *Store) {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	db := postgrestest.NewDatabase(t, module).Open(module)
	store, err := New(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return db, store
}

func TestImportCarriesEveryRecordAndItsBytes(t *testing.T) {
	for _, carry := range []BlobCarry{CopyBlobs, LinkBlobs} {
		t.Run(string(carry), func(t *testing.T) {
			sqlitePath, blobs := standaloneStore(t, "the first file", "the second file", "the first file")
			db, store := newImportTarget(t)
			counts, err := ImportStandaloneDatabase(context.Background(), db, store, sqlitePath, blobs, carry)
			if err != nil {
				t.Fatal(err)
			}
			if counts.Files != 3 || counts.Blobs != 2 || counts.LastFileNumber != 4 || counts.StoredSettings["inline_content_types"] != "image/png,video/mp4" {
				t.Fatalf("counts %+v", counts)
			}
			file, err := store.Get("file_000002", false)
			if err != nil || file.Filename != "note-2.txt" || file.CreatedAt != 102 || file.DetectedContentType != "text/plain; charset=utf-8" {
				t.Fatalf("%v %+v", err, file)
			}
			content, err := store.OpenContent(file)
			if err != nil {
				t.Fatal(err)
			}
			read, _ := io.ReadAll(content)
			content.Close()
			if string(read) != "the second file" {
				t.Fatalf("the bytes came across as %q", read)
			}
			next, err := store.Put(bytes.NewReader([]byte("new")), 100, NewFile{ContentType: "text/plain", Filename: "new.txt", OwnerService: "a", OwnerRef: "b"})
			if err != nil || next.ID != "file_000005" {
				t.Fatalf("the next upload: %v %+v", err, next)
			}
		})
	}
}

func TestImportRefusesABlobThatIsMissingOrDamaged(t *testing.T) {
	sqlitePath, blobs := standaloneStore(t, "the first file", "the second file")
	sum := sha256.Sum256([]byte("the second file"))
	sha := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(blobs, sha[0:2], sha[2:4], sha), []byte("something else"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, store := newImportTarget(t)
	if _, err := ImportStandaloneDatabase(context.Background(), db, store, sqlitePath, blobs, CopyBlobs); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("a damaged blob: %v", err)
	}
	if files, err := store.List(Filter{Limit: 10, IncludeDeleted: true}); err != nil || len(files) != 0 {
		t.Fatalf("a refused import left rows: %v %+v", err, files)
	}
}
