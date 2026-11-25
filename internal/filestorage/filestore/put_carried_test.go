package filestore

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPutCarriedLinksAFileInAndPurgeKeepsTheSource(t *testing.T) {
	f := newFixture(t, nil)
	sourceDirectory := t.TempDir()
	source := filepath.Join(sourceDirectory, "thumbnail.jpg")
	content := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'm', 'a', 'd', 'e', ' ', 'u', 'p'}
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, carry := range []BlobCarry{LinkBlobs, CopyBlobs} {
		file, err := f.store.PutCarried(source, carry, NewFile{ContentType: "image/jpeg", Filename: "thumbnail.jpg", OwnerService: "photo-store", OwnerRef: "photo_000001"})
		if err != nil {
			t.Fatalf("%s: %v", carry, err)
		}
		if file.SizeBytes != int64(len(content)) || file.DetectedContentType != "image/jpeg" || file.OwnerRef != "photo_000001" {
			t.Fatalf("%s: %+v", carry, file)
		}
		got, err := f.store.Get(file.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := f.store.OpenContent(got)
		if err != nil {
			t.Fatal(err)
		}
		read, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || string(read) != string(content) {
			t.Fatalf("%s: read %q, %v", carry, read, err)
		}
		if err := f.store.Purge(file.ID); err != nil {
			t.Fatal(err)
		}
	}
	if kept, err := os.ReadFile(source); err != nil || string(kept) != string(content) {
		t.Fatalf("the source after both purges: %q, %v", kept, err)
	}
	if _, err := f.store.PutCarried(source, "move", NewFile{}); err == nil {
		t.Fatal("an unknown carry was taken")
	}
	empty := filepath.Join(sourceDirectory, "empty.jpg")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PutCarried(empty, CopyBlobs, NewFile{}); err != ErrEmptyFile {
		t.Fatalf("an empty file: %v", err)
	}
}
