package photostore

import (
	"bytes"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// Made-up originals of the standalone file's photos.
var (
	standaloneShaOne    = strings.Repeat("a", 64)
	standaloneShaThree  = strings.Repeat("b", 64)
	standaloneShaPurged = strings.Repeat("c", 64)
)

// standaloneThumbnailOf is the synthetic JPEG bytes a standalone photo's
// thumbnail holds.
func standaloneThumbnailOf(sha string) []byte {
	return append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("made-up "+sha[:4])...)
}

// newStandaloneDatabase writes a SQLite file the way the standalone store
// kept one, with a few made-up rows and gaps in every numbering, and a
// thumbnails directory holding each unpurged photo's thumbnail.
// withSightings false leaves photo_sightings empty, as a file made before
// sightings existed.
func newStandaloneDatabase(t *testing.T, withSightings bool) (sqlitePath, thumbnailsDirectory string) {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	sqlitePath = filepath.Join(directory, "photo-store.db")
	thumbnailsDirectory = filepath.Join(directory, "thumbnails")
	if err := os.Mkdir(thumbnailsDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, sha := range []string{standaloneShaOne, standaloneShaThree} {
		if err := os.WriteFile(filepath.Join(thumbnailsDirectory, sha+".jpg"), standaloneThumbnailOf(sha), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("standalone schema: %v", err)
	}
	statements := []string{
		`INSERT INTO photos (id, seq, sha256, source_machine, source_path, folder, media_type, taken_at, date_source, latitude, longitude,
			width, height, description, described_by, tags, created_at, updated_at, deleted_at, purged_at)
			VALUES ('photo_000001', 1, '` + standaloneShaOne + `', 'laptop', 'Pictures/Camera/one.jpg', 'Pictures/Camera', 'image/jpeg', 1600000000, 'exif',
			38.7, -9.1, 1024, 768, 'A tram on a hill', 'test', '["tram","street"]', 1700000000, 1700000100, 0, 0)`,
		`INSERT INTO photos (id, seq, sha256, source_machine, source_path, folder, media_type, taken_at, date_source, latitude, longitude,
			width, height, description, described_by, tags, created_at, updated_at, deleted_at, purged_at)
			VALUES ('photo_000003', 3, '` + standaloneShaThree + `', 'multichat', 'message_000007', 'chat/one', 'image/png', 0, 'unknown',
			NULL, NULL, 640, 480, '', '', '[]', 1700000000, 1700000000, 1700000500, 0)`,
		`INSERT INTO photos (id, seq, sha256, source_machine, source_path, folder, media_type, taken_at, date_source, latitude, longitude,
			width, height, description, described_by, tags, created_at, updated_at, deleted_at, purged_at)
			VALUES ('photo_000004', 4, '` + standaloneShaPurged + `', 'laptop', 'Pictures/Camera/card.jpg', 'Pictures/Camera', 'image/jpeg', 1600000100, 'exif',
			NULL, NULL, 800, 600, '', '', '[]', 1700000000, 1700000300, 1700000300, 1700000300)`,
		`INSERT INTO photo_revisions (revision_id, photo_id, reason, replaced_at, row_json)
			VALUES (2, 'photo_000001', 'import', 1700000100, '{"id":"photo_000001","source_machine":"laptop","source_path":"Old/one.jpg","folder":"Old","description":""}')`,
		`INSERT INTO face_scans VALUES ('photo_000001', 'test-model', 2, 1700000200)`,
		`INSERT INTO faces (id, seq, photo_id, model, box_left, box_top, box_right, box_bottom, detection_score, embedding, created_at, retired_at, retired_reason)
			VALUES ('face_000002', 2, 'photo_000001', 'test-model', 0.1, 0.1, 0.3, 0.4, 0.9, X'0000803F', 1700000200, 0, '')`,
		`INSERT INTO faces (id, seq, photo_id, model, box_left, box_top, box_right, box_bottom, detection_score, embedding, created_at, retired_at, retired_reason)
			VALUES ('face_000005', 5, 'photo_000001', 'test-model', 0.5, 0.5, 0.7, 0.8, 0.8, X'0000803F', 1700000200, 1700000250, 'a rescan')`,
		`INSERT INTO photo_people (id, seq, photo_id, person_id, face_id, status, confidence, evidence, proposed_by, decided_by, created_at, updated_at)
			VALUES ('photo_person_000003', 3, 'photo_000001', 'person_000012', 'face_000002', 'confirmed', 0.9, 'they said so', 'matcher', 'operator', 1700000300, 1700000300)`,
		`INSERT INTO photo_people (id, seq, photo_id, person_id, face_id, status, confidence, evidence, proposed_by, decided_by, created_at, updated_at)
			VALUES ('photo_person_000004', 4, 'photo_000003', 'person_000013', NULL, 'suggested', 0.4, '', 'matcher', '', 1700000300, 1700000300)`,
		`INSERT INTO photo_person_revisions (revision_id, photo_person_id, replaced_at, row_json)
			VALUES (6, 'photo_person_000003', 1700000300, '{"id":"photo_person_000003","status":"suggested"}')`,
		`INSERT INTO face_groups (id, seq, model, status, person_id, made_by, decided_by, decided_at, created_at, updated_at)
			VALUES ('face_group_000002', 2, 'test-model', 'named', 'person_000012', 'grouper', 'operator', 1700000300, 1700000250, 1700000300)`,
		`INSERT INTO face_group_members VALUES ('face_group_000002', 'face_000002', 0.95, 1700000250, 0, '')`,
		`INSERT INTO face_group_guesses (id, seq, group_id, person_id, confidence, evidence, proposed_by, created_at)
			VALUES ('face_group_guess_000007', 7, 'face_group_000002', 'person_000012', 0.7, 'posted by them', 'a model', 1700000260)`,
	}
	if withSightings {
		statements = append(statements,
			`INSERT INTO photo_sightings VALUES ('photo_000001', 'laptop', 'Pictures/Camera/one.jpg', 'Pictures/Camera', 1700000000)`,
			`INSERT INTO photo_sightings VALUES ('photo_000003', 'multichat', 'message_000007', 'chat/one', 1700000000)`,
			`INSERT INTO photo_sightings VALUES ('photo_000003', 'multichat', 'message_000002', 'chat/two', 1699999000)`)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return sqlitePath, thumbnailsDirectory
}

func TestImportCopiesEveryRowAsItWasCarriesTheThumbnailsAndNumbersOnAfterIt(t *testing.T) {
	service := newTestService(t)
	service.people.keep(alicePrincipalID, "person_000012")
	sqlitePath, thumbnailsDirectory := newStandaloneDatabase(t, true)
	counts, err := ImportStandaloneDatabase(testContext, service.db, service.thumbnails, sqlitePath, thumbnailsDirectory, alicePrincipalID)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want := ImportCounts{Photos: 3, PhotoRevisions: 1, Sightings: 3, FaceScans: 1, Faces: 2, PhotoPeople: 2, PhotoPersonRevisions: 1,
		FaceGroups: 1, FaceGroupMembers: 1, FaceGroupGuesses: 1, Thumbnails: 2}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}
	if got := counts.Summary(); got != "photos=3 photo_revisions=1 sightings=3 sightings_filled=0 face_scans=1 faces=2 photo_people=2 "+
		"photo_person_revisions=1 face_groups=1 face_group_members=1 face_group_guesses=1 thumbnails=2" {
		t.Errorf("summary = %q", got)
	}

	var photos []Photo
	service.call(http.MethodGet, "/photos?include_deleted=true", nil, &photos)
	if len(photos) != 3 || photos[0].ID != "photo_000003" || photos[1].ID != "photo_000001" || photos[2].ID != "photo_000004" {
		t.Fatalf("photos = %+v, want photo_000003 (undated), photo_000001, photo_000004", photos)
	}
	one := photos[1]
	if one.OwnerPrincipalID != alicePrincipalID || one.Description != "A tram on a hill" || strings.Join(one.Tags, ",") != "tram,street" ||
		one.Latitude == nil || *one.Latitude != 38.7 || one.UpdatedAt != 1700000100 || photos[2].PurgedAt != 1700000300 {
		t.Errorf("copied photos are not as they were: %+v", photos)
	}
	for _, sha := range []string{standaloneShaOne, standaloneShaThree} {
		id := map[string]string{standaloneShaOne: "photo_000001", standaloneShaThree: "photo_000003"}[sha]
		if status, body := service.call(http.MethodGet, "/photos/"+id+"/thumbnail", nil, nil); status != http.StatusOK || !bytes.Equal(body, standaloneThumbnailOf(sha)) {
			t.Errorf("%s's thumbnail = %d, %d bytes; want the carried bytes", id, status, len(body))
		}
	}
	if status, _ := service.call(http.MethodGet, "/photos/photo_000004/thumbnail", nil, nil); status != http.StatusGone {
		t.Errorf("the purged photo's thumbnail = %d, want 410", status)
	}
	var found []Photo
	service.call(http.MethodGet, "/photos?q=tram+HILL", nil, &found)
	if len(found) != 1 || found[0].ID != "photo_000001" {
		t.Errorf("a search of the copied text found %d photos, want photo_000001", len(found))
	}
	var revisions []Revision
	service.call(http.MethodGet, "/photos/photo_000001/revisions", nil, &revisions)
	if len(revisions) != 1 || revisions[0].RevisionID != 2 || revisions[0].Photo.SourcePath != "Old/one.jpg" || revisions[0].Photo.OwnerPrincipalID != alicePrincipalID {
		t.Errorf("revisions = %+v", revisions)
	}
	var faces []Face
	service.call(http.MethodGet, "/faces", nil, &faces)
	if len(faces) != 1 || faces[0].ID != "face_000002" || faces[0].OwnerPrincipalID != alicePrincipalID {
		t.Errorf("faces = %+v, want face_000002 alone: face_000005 is retired", faces)
	}
	var links []PhotoPerson
	service.as(administrator).call(http.MethodGet, "/photo-people", nil, &links)
	if len(links) != 1 || links[0].ID != "photo_person_000003" || links[0].FaceID != "face_000002" || links[0].OwnerPrincipalID != alicePrincipalID {
		t.Errorf("links on live photos = %+v, want photo_person_000003 on face_000002", links)
	}
	var group FaceGroup
	service.call(http.MethodGet, "/face-groups/face_group_000002", nil, &group)
	if group.Status != "named" || group.FaceCount != 1 || len(group.Guesses) != 1 || group.Guesses[0].ID != "face_group_guess_000007" {
		t.Errorf("face group = %+v", group)
	}
	var sightings []Sighting
	service.call(http.MethodGet, "/sightings?source_machine=multichat", nil, &sightings)
	if len(sightings) != 0 {
		t.Errorf("sightings of a deleted photo = %+v, want none listed", sightings)
	}

	// The next record of each kind takes the number after the highest copied.
	var result ImportResult
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("next", 100)}}, &result)
	var newFaces []Face
	service.call(http.MethodPost, "/face-scans", FaceScan{PhotoID: result.IDs[0], Model: "test-model", Faces: []ScannedFace{
		{Box: FaceBox{Left: 0.1, Top: 0.1, Right: 0.3, Bottom: 0.4}, DetectionScore: 0.9, EmbeddingFloat32Base64: embeddingOf(1, 0)}}}, &newFaces)
	var link PhotoPerson
	service.call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: result.IDs[0], PersonID: "person_000012", Status: "suggested", ProposedBy: "matcher"}, &link)
	var newGroup FaceGroup
	service.call(http.MethodPost, "/face-groups", NewFaceGroup{Model: "test-model", MadeBy: "grouper", Members: []NewFaceGroupMember{{FaceID: "face_000006", Similarity: 1}}}, &newGroup)
	var guess FaceGroupGuess
	service.call(http.MethodPost, "/face-groups/"+newGroup.ID+"/guesses", FaceGroupGuessProposal{Confidence: 0.5, ProposedBy: "a model"}, &guess)
	service.call(http.MethodPatch, "/photos/photo_000001", map[string]any{"tags": []string{"tram"}}, nil)
	service.call(http.MethodGet, "/photos/photo_000001/revisions", nil, &revisions)
	got := strings.Join([]string{result.IDs[0], newFaces[0].ID, link.ID, newGroup.ID, guess.ID}, ",")
	if got != "photo_000005,face_000006,photo_person_000005,face_group_000003,face_group_guess_000008" || revisions[0].RevisionID != 3 {
		t.Errorf("next ids = %s and revision %d, want each after the highest copied", got, revisions[0].RevisionID)
	}
}

func TestImportFillsSightingsWhenTheFileHasNone(t *testing.T) {
	service := newTestService(t)
	sqlitePath, thumbnailsDirectory := newStandaloneDatabase(t, false)
	counts, err := ImportStandaloneDatabase(testContext, service.db, service.thumbnails, sqlitePath, thumbnailsDirectory, alicePrincipalID)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// photo_000001's revision kept Old/one.jpg; every photo has its current
	// place.
	if counts.Sightings != 0 || counts.SightingsFilled != 4 {
		t.Fatalf("counts = %+v, want 4 sightings filled", counts)
	}
	var sightings []Sighting
	service.call(http.MethodGet, "/sightings?source_machine=laptop", nil, &sightings)
	if len(sightings) != 2 || sightings[0].SourcePath != "Old/one.jpg" || sightings[0].SeenAt != 1700000100 || sightings[1].SourcePath != "Pictures/Camera/one.jpg" {
		t.Errorf("laptop sightings = %+v, want the old place from the revision and the current one", sightings)
	}
}

func TestImportRefusesAMissingThumbnailABadOwnerAndAModuleWithRows(t *testing.T) {
	service := newTestService(t)
	sqlitePath, thumbnailsDirectory := newStandaloneDatabase(t, true)
	if _, err := ImportStandaloneDatabase(testContext, service.db, service.thumbnails, sqlitePath, thumbnailsDirectory, "alice"); err == nil {
		t.Error("an owner that is not a principal id was taken")
	}
	if err := os.Remove(filepath.Join(thumbnailsDirectory, standaloneShaThree+".jpg")); err != nil {
		t.Fatal(err)
	}
	_, err := ImportStandaloneDatabase(testContext, service.db, service.thumbnails, sqlitePath, thumbnailsDirectory, alicePrincipalID)
	if err == nil || !strings.Contains(err.Error(), "photo_000003") {
		t.Fatalf("a missing thumbnail = %v, want an error naming photo_000003", err)
	}
	var held int
	if err := service.db.QueryRowContext(testContext, `SELECT COUNT(*) FROM photos`).Scan(&held); err != nil || held != 0 || service.thumbnails.count() != 0 {
		t.Fatalf("after a refused import the module holds %d photos and the fake %d files, want none", held, service.thumbnails.count())
	}

	// A copy that fails part-way destroys every thumbnail it carried.
	if err := os.WriteFile(filepath.Join(thumbnailsDirectory, standaloneShaThree+".jpg"), standaloneThumbnailOf(standaloneShaThree), 0o600); err != nil {
		t.Fatal(err)
	}
	broken, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broken.Exec(`UPDATE photo_people SET face_id = 'face_0000x2' WHERE seq = 3`); err != nil {
		t.Fatal(err)
	}
	broken.Close()
	if _, err := ImportStandaloneDatabase(testContext, service.db, service.thumbnails, sqlitePath, thumbnailsDirectory, alicePrincipalID); err == nil {
		t.Fatal("a row pointing at a malformed face id was copied")
	}
	if err := service.db.QueryRowContext(testContext, `SELECT COUNT(*) FROM photos`).Scan(&held); err != nil || held != 0 || service.thumbnails.count() != 0 {
		t.Fatalf("after a failed copy the module holds %d photos and the fake %d files, want none", held, service.thumbnails.count())
	}

	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("held", 1)}}, nil)
	freshPath, freshThumbnails := newStandaloneDatabase(t, true)
	if _, err := ImportStandaloneDatabase(testContext, service.db, service.thumbnails, freshPath, freshThumbnails, alicePrincipalID); err == nil ||
		!strings.Contains(err.Error(), "already holds") {
		t.Errorf("an import into a module with rows = %v, want a refusal", err)
	}
	if service.thumbnails.count() != 1 {
		t.Errorf("a refused import left %d files, want only the photo already held", service.thumbnails.count())
	}
}
