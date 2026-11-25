package photostore

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	jpegcodec "image/jpeg"
	"math"
	"net/http"
	"strings"
	"testing"
)

func embeddingOf(values ...float32) string {
	raw := make([]byte, 4*len(values))
	for index, value := range values {
		binary.LittleEndian.PutUint32(raw[index*4:], math.Float32bits(value))
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// realJPEG is a small grey picture a face can be cut from, unlike the
// placeholder bytes most tests import as a thumbnail.
func realJPEG(t *testing.T) []byte {
	t.Helper()
	picture := image.NewGray(image.Rect(0, 0, 200, 150))
	var encoded bytes.Buffer
	if err := jpegcodec.Encode(&encoded, picture, nil); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestAFaceScanIsKeptOnceAndItsEmbeddingNormalized(t *testing.T) {
	service := newTestService(t)
	withPicture := photoOf("n", 100)
	withPicture.ThumbnailJPEG = realJPEG(t)
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{withPicture, photoOf("o", 200)}}, nil)

	var pending []string
	service.call(http.MethodGet, "/face-scans/pending?model=test-model", nil, &pending)
	if len(pending) != 2 {
		t.Fatalf("pending before any scan = %v, want both photos", pending)
	}
	scan := FaceScan{PhotoID: "photo_000001", Model: "test-model", Faces: []ScannedFace{
		{Box: FaceBox{Left: 0.1, Top: 0.1, Right: 0.3, Bottom: 0.4}, DetectionScore: 0.9, EmbeddingFloat32Base64: embeddingOf(3, 4)},
	}}
	var faces []Face
	if status, body := service.call(http.MethodPost, "/face-scans", scan, &faces); status != http.StatusCreated || faces[0].ID != "face_000001" {
		t.Fatalf("POST /face-scans = %d %s", status, body)
	}
	if status, _ := service.call(http.MethodPost, "/face-scans", scan, nil); status != http.StatusConflict {
		t.Errorf("a second scan by the same model = %d, want 409", status)
	}
	if status, _ := service.call(http.MethodPost, "/face-scans", FaceScan{PhotoID: "photo_000002", Model: "test-model"}, nil); status != http.StatusCreated {
		t.Errorf("a scan that found no face = %d, want 201", status)
	}
	service.call(http.MethodGet, "/face-scans/pending?model=test-model", nil, &pending)
	if len(pending) != 0 {
		t.Errorf("pending after both scans = %v, want none: a scan with no face still counts", pending)
	}
	for _, bad := range []FaceScan{
		{PhotoID: "photo_000002", Model: "other", Faces: []ScannedFace{{Box: FaceBox{Left: 0.5, Top: 0, Right: 0.2, Bottom: 1}, EmbeddingFloat32Base64: embeddingOf(1)}}},
		{PhotoID: "photo_000002", Model: "other", Faces: []ScannedFace{{Box: FaceBox{Left: 0, Top: 0, Right: 1, Bottom: 1}, EmbeddingFloat32Base64: embeddingOf(0, 0)}}},
		{PhotoID: "photo_000002", Model: ""},
	} {
		if status, _ := service.call(http.MethodPost, "/face-scans", bad, nil); status != http.StatusBadRequest {
			t.Errorf("POST /face-scans %+v = %d, want 400", bad, status)
		}
	}

	var withEmbeddings []Face
	service.call(http.MethodGet, "/faces?include_embeddings=true", nil, &withEmbeddings)
	raw, _ := base64.StdEncoding.DecodeString(withEmbeddings[0].EmbeddingFloat32Base64)
	first := math.Float32frombits(binary.LittleEndian.Uint32(raw))
	if math.Abs(float64(first)-0.6) > 1e-6 {
		t.Errorf("the stored embedding starts %v, want 0.6: (3, 4) normalized", first)
	}
	if status, image := service.call(http.MethodGet, "/faces/face_000001/image", nil, nil); status != http.StatusOK || len(image) < 2 || image[0] != 0xFF || image[1] != 0xD8 {
		t.Errorf("GET /faces/face_000001/image = %d, %d bytes; want a JPEG", status, len(image))
	}
	if status, _ := service.call(http.MethodGet, "/faces/face_000099/image", nil, nil); status != http.StatusNotFound {
		t.Errorf("an unknown face's image = %d, want 404", status)
	}
	var withoutEmbeddings []Face
	service.call(http.MethodGet, "/photos/photo_000001/faces", nil, &withoutEmbeddings)
	if len(withoutEmbeddings) != 1 || withoutEmbeddings[0].EmbeddingFloat32Base64 != "" {
		t.Errorf("a photo's faces = %+v, want one face and no embedding", withoutEmbeddings)
	}
}

func TestPeopleOnAPhotoMoveOnlyUpwardAndARejectionHolds(t *testing.T) {
	service := newTestService(t)
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("p", 100), photoOf("q", 200)}}, nil)
	service.call(http.MethodPost, "/face-scans", FaceScan{PhotoID: "photo_000001", Model: "test-model", Faces: []ScannedFace{
		{Box: FaceBox{Left: 0.1, Top: 0.1, Right: 0.3, Bottom: 0.4}, DetectionScore: 0.9, EmbeddingFloat32Base64: embeddingOf(1, 0)}}}, nil)

	suggestion := PhotoPersonProposal{PhotoID: "photo_000001", PersonID: "person_000001", FaceID: "face_000001", Status: "suggested",
		Confidence: 0.6, Evidence: "a matcher's guess", ProposedBy: "matcher"}
	var link PhotoPerson
	if status, body := service.call(http.MethodPost, "/photo-people", suggestion, &link); status != http.StatusCreated || link.ID != "photo_person_000001" {
		t.Fatalf("POST /photo-people = %d %s", status, body)
	}
	confirmation := suggestion
	confirmation.Status, confirmation.Confidence, confirmation.DecidedBy = "confirmed", 0.97, "matcher, face and model agree"
	service.call(http.MethodPost, "/photo-people", confirmation, &link)
	if link.Status != "confirmed" || link.DecidedBy == "" {
		t.Fatalf("a confirming proposal left %+v, want it confirmed", link)
	}
	service.call(http.MethodPost, "/photo-people", suggestion, &link)
	if link.Status != "confirmed" {
		t.Errorf("a later suggestion turned a confirmed link into %q", link.Status)
	}

	var photos []Photo
	service.call(http.MethodGet, "/photos?person=person_000001", nil, &photos)
	if len(photos) != 1 || photos[0].ID != "photo_000001" {
		t.Errorf("photos of person_000001 = %v, want only photo_000001", photos)
	}

	rejected, decidedBy := "rejected", "operator"
	service.call(http.MethodPatch, "/photo-people/photo_person_000001", PhotoPersonDecision{Status: &rejected, DecidedBy: &decidedBy}, &link)
	service.call(http.MethodPost, "/photo-people", confirmation, &link)
	if link.Status != "rejected" {
		t.Errorf("a matcher's confirmation undid a rejection: %+v", link)
	}

	for _, bad := range []PhotoPersonProposal{
		{PhotoID: "photo_000002", PersonID: "Mary", Status: "suggested", ProposedBy: "matcher"},
		{PhotoID: "photo_000002", PersonID: "person_000002", Status: "confirmed", ProposedBy: "matcher"},
		{PhotoID: "photo_000002", PersonID: "person_000002", FaceID: "face_000001", Status: "suggested", ProposedBy: "matcher"},
		{PhotoID: "photo_000002", PersonID: "person_000002", Status: "suggested"},
	} {
		if status, _ := service.call(http.MethodPost, "/photo-people", bad, nil); status != http.StatusBadRequest {
			t.Errorf("POST /photo-people %+v = %d, want 400", bad, status)
		}
	}
	if status, _ := service.call(http.MethodPatch, "/photo-people/photo_person_000001", `{"status":"confirmed"}`, nil); status != http.StatusBadRequest {
		t.Errorf("a status change with no decided_by = %d, want 400", status)
	}

	service.call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000002", PersonID: "person_000002", Status: "suggested", ProposedBy: "matcher"}, nil)
	var counts []PersonPhotoCount
	service.call(http.MethodGet, "/people-counts", nil, &counts)
	if len(counts) != 1 || counts[0] != (PersonPhotoCount{PersonID: "person_000002", Suggested: 1}) {
		t.Errorf("people counts = %+v, want only person_000002 suggested once: a rejected link counts for nobody", counts)
	}
}

func TestARescanRetiresTheOldFacesAndALinkKeepsItsStatusButNotItsFace(t *testing.T) {
	service := newTestService(t)
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photoOf("r", 100)}}, nil)
	firstScan := FaceScan{PhotoID: "photo_000001", Model: "test-model", Faces: []ScannedFace{
		{Box: FaceBox{Left: 0.5, Top: 0.4, Right: 0.9, Bottom: 0.8}, DetectionScore: 0.8, EmbeddingFloat32Base64: embeddingOf(1, 0)}}}
	service.call(http.MethodPost, "/face-scans", firstScan, nil)
	confirmed := PhotoPersonProposal{PhotoID: "photo_000001", PersonID: "person_000001", FaceID: "face_000001", Status: "confirmed",
		Evidence: "they said so", ProposedBy: "operator", DecidedBy: "operator"}
	service.call(http.MethodPost, "/photo-people", confirmed, nil)

	secondScan := firstScan
	secondScan.Faces = []ScannedFace{
		{Box: FaceBox{Left: 0.3, Top: 0.05, Right: 0.7, Bottom: 0.45}, DetectionScore: 0.95, EmbeddingFloat32Base64: embeddingOf(0, 1)},
		{Box: FaceBox{Left: 0, Top: 0.3, Right: 0.3, Bottom: 0.7}, DetectionScore: 0.9, EmbeddingFloat32Base64: embeddingOf(1, 1)}}
	secondScan.Replaces = true
	var faces []Face
	if status, body := service.call(http.MethodPost, "/face-scans", secondScan, &faces); status != http.StatusCreated || len(faces) != 2 || faces[0].ID != "face_000002" {
		t.Fatalf("a replacing scan = %d %s, want 201 with face_000002 and face_000003", status, body)
	}
	service.call(http.MethodGet, "/photos/photo_000001/faces", nil, &faces)
	if len(faces) != 2 || faces[0].ID != "face_000002" {
		t.Errorf("faces after the rescan = %+v, want only the new two", faces)
	}
	var links []PhotoPerson
	service.call(http.MethodGet, "/photo-people?photo_id=photo_000001", nil, &links)
	if len(links) != 1 || links[0].Status != "confirmed" || links[0].FaceID != "" || !strings.Contains(links[0].Evidence, "face_000001") {
		t.Fatalf("the link after the rescan = %+v, want it confirmed with no face and a note naming the retired one", links)
	}
	if status, _ := service.call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000001", PersonID: "person_000002",
		FaceID: "face_000001", Status: "suggested", ProposedBy: "matcher"}, nil); status != http.StatusBadRequest {
		t.Errorf("a link naming a retired face = %d, want 400", status)
	}
	var link PhotoPerson
	service.call(http.MethodPost, "/photo-people", PhotoPersonProposal{PhotoID: "photo_000001", PersonID: "person_000001",
		FaceID: "face_000002", Status: "suggested", ProposedBy: "matcher"}, &link)
	if link.Status != "confirmed" || link.FaceID != "face_000002" || link.DecidedBy != "operator" {
		t.Errorf("a matcher naming the face of a confirmed link with none = %+v, want the face taken and the decision kept", link)
	}
	if status, _ := service.call(http.MethodPost, "/face-scans", firstScan, nil); status != http.StatusConflict {
		t.Errorf("a second scan without replaces = %d, want 409", status)
	}
}

func TestAnImportThatChangesTheThumbnailRetiresItsFacesAndTheSameThumbnailDoesNot(t *testing.T) {
	service := newTestService(t)
	photo := photoOf("s", 100)
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photo}}, nil)
	service.call(http.MethodPost, "/face-scans", FaceScan{PhotoID: "photo_000001", Model: "test-model", Faces: []ScannedFace{
		{Box: FaceBox{Left: 0.1, Top: 0.1, Right: 0.3, Bottom: 0.4}, DetectionScore: 0.9, EmbeddingFloat32Base64: embeddingOf(1, 0)}}}, nil)

	var result ImportResult
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photo}}, &result)
	var pending []string
	service.call(http.MethodGet, "/face-scans/pending?model=test-model", nil, &pending)
	if result.FacesRetired != 0 || len(pending) != 0 {
		t.Fatalf("the same thumbnail again retired %d faces and left pending %v, want nothing", result.FacesRetired, pending)
	}

	photo.ThumbnailJPEG = realJPEG(t)
	service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{photo}}, &result)
	service.call(http.MethodGet, "/face-scans/pending?model=test-model", nil, &pending)
	var faces []Face
	service.call(http.MethodGet, "/photos/photo_000001/faces", nil, &faces)
	if result.FacesRetired != 1 || len(pending) != 1 || len(faces) != 0 {
		t.Errorf("a new thumbnail retired %d faces, left pending %v and faces %+v; want 1 retired, the photo pending and no faces", result.FacesRetired, pending, faces)
	}
}
