package photostore

import (
	"bytes"
	"net/http"
	"testing"
)

func TestAnImportKeepsOneFilePerPhotoAndReplacesItOnlyWhenTheBytesChange(t *testing.T) {
	service := newTestService(t)
	photo := photoOf("t", 100)
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photo, photoOf("u", 200)}}, nil)
	if service.thumbnails.count() != 2 {
		t.Fatalf("after importing two photos the fake keeps %d files, want 2", service.thumbnails.count())
	}
	keepsBefore := service.thumbnails.keeps
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photo}}, nil)
	if service.thumbnails.keeps != keepsBefore || service.thumbnails.count() != 2 {
		t.Errorf("the same bytes again kept %d new files, want none", service.thumbnails.keeps-keepsBefore)
	}

	photo.ThumbnailJPEG = realJPEG(t)
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photo}}, nil)
	files := service.thumbnails.filesOf(alicePrincipalID, "photo_000001")
	if len(files) != 1 || !bytes.Equal(files[0].content, photo.ThumbnailJPEG) || service.thumbnails.count() != 2 {
		t.Fatalf("after new bytes photo_000001 has %d files and the fake %d; want the one new file, the old destroyed", len(files), service.thumbnails.count())
	}
	if status, body := service.call(http.MethodGet, "/photos/photo_000001/thumbnail", nil, nil); status != http.StatusOK || !bytes.Equal(body, photo.ThumbnailJPEG) {
		t.Errorf("the thumbnail after new bytes = %d, %d bytes; want the new bytes", status, len(body))
	}
}

func TestAnImportThatFailsDestroysEveryFileItKept(t *testing.T) {
	service := newTestService(t)
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("v", 100)}}, nil)
	kept := service.thumbnails.count()

	// The file-store refuses the second file: the first is destroyed again.
	service.thumbnails.failKeepAfter = service.thumbnails.keeps + 1
	status, body := service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("w", 100), photoOf("x", 200)}}, nil)
	if status != http.StatusInternalServerError || service.thumbnails.count() != kept {
		t.Errorf("a refused file = %d %s with %d files kept, want 500 and %d", status, body, service.thumbnails.count(), kept)
	}
	service.thumbnails.failKeepAfter = 0

	// Another import takes the same original while this one keeps its
	// file: its transaction fails, and the file it kept is destroyed.
	contested := photoOf("y", 300)
	service.thumbnails.beforeKeep = func() {
		service.thumbnails.beforeKeep = nil
		if _, err := service.store.Import(testContext, alicePrincipalID, ImportRequest{Photos: []ImportPhoto{contested}}); err != nil {
			t.Errorf("the import that wins: %v", err)
		}
	}
	status, body = service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("z", 400), contested}}, nil)
	if status != http.StatusConflict {
		t.Errorf("the import that lost = %d %s, want 409", status, body)
	}
	var photos []Photo
	service.call(http.MethodGet, "/photos", nil, &photos)
	if len(photos) != 2 || service.thumbnails.count() != 2 {
		t.Errorf("after the lost import: %d photos and %d files, want the first photo and the winner's, each with one file", len(photos), service.thumbnails.count())
	}
}

func TestAPurgeDestroysTheThumbnailFileAndAMissingFileIsAnError(t *testing.T) {
	service := newTestService(t)
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("p1", 100), photoOf("p2", 200)}}, nil)
	service.call(http.MethodPost, "/photos/photo_000001/purge", nil, nil)
	if len(service.thumbnails.filesOf(alicePrincipalID, "photo_000001")) != 0 {
		t.Errorf("a purged photo's thumbnail file is still kept")
	}
	var fileID string
	if err := service.db.QueryRowContext(testContext, `SELECT thumbnail_file_id FROM photos WHERE seq = 1`).Scan(&fileID); err != nil || fileID != "" {
		t.Errorf("a purged photo's thumbnail_file_id = %q, %v; want it blank", fileID, err)
	}
	// A file file-store lost is a loud 500 naming the photo, not a 404.
	for id := range service.thumbnails.files {
		delete(service.thumbnails.files, id)
	}
	if status, body := service.call(http.MethodGet, "/photos/photo_000002/thumbnail", nil, nil); status != http.StatusInternalServerError || !bytes.Contains(body, []byte("photo_000002")) {
		t.Errorf("a thumbnail whose file is gone = %d %s, want 500 naming the photo", status, body)
	}
}
