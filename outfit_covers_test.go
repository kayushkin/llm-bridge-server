package possessionstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// portraitPhotos is a photo source whose photos are 600×800 pictures (a
// 300×400 thumbnail), with the wearer's face confirmed only on photo_000002.
type portraitPhotos struct{ fakePhotos }

func (portraitPhotos) picture(width, height int) image.Image { return asymmetricPicture(width, height) }

func (photos portraitPhotos) Thumbnail(ctx context.Context, photoID string) ([]byte, error) {
	var out bytes.Buffer
	err := jpeg.Encode(&out, photos.picture(300, 400), nil)
	return out.Bytes(), err
}
func (photos portraitPhotos) DetailImage(ctx context.Context, photoID string) (image.Image, string, error) {
	return photos.picture(600, 800), "original", nil
}
func (portraitPhotos) WearerFace(ctx context.Context, photoID, personID string) (Box, error) {
	if photoID == "photo_000002" && personID == "person_000001" {
		return Box{0.4, 0.05, 0.6, 0.2}, nil
	}
	return Box{}, ErrInvalid
}

func pixelAspect(frame Box, photoAspect float64) float64 {
	return (frame.Right - frame.Left) * photoAspect / (frame.Bottom - frame.Top)
}

func holds(outer, inner Box) bool {
	const slack = 1e-9
	return outer.Left <= inner.Left+slack && outer.Top <= inner.Top+slack && outer.Right >= inner.Right-slack && outer.Bottom >= inner.Bottom-slack
}

func TestACoverFrameHoldsTheWearerInATallFrameInsideThePhoto(t *testing.T) {
	face := Box{0.4, 0.1, 0.55, 0.25}
	jacket := Box{0.3, 0.25, 0.7, 0.6}
	watch := Box{0.65, 0.5, 0.7, 0.55}
	for _, photoAspect := range []float64{0.75, 1, 4.0 / 3} {
		frame := coverFrame(&face, []Box{jacket, watch}, photoAspect)
		if aspect := pixelAspect(frame, photoAspect); math.Abs(aspect-coverAspect) > 0.01 {
			t.Fatalf("photo %.2f: frame %v is %.3f wide over tall, want %.3f", photoAspect, frame, aspect, coverAspect)
		}
		if !holds(Box{0, 0, 1, 1}, frame) || !holds(frame, Box{0.3 - 0.4*coverMargin, 0.1 - 0.5*coverMargin, 0.7 + 0.4*coverMargin, 0.6 + 0.5*coverMargin}) {
			t.Fatalf("photo %.2f: frame %v should be inside the photo and hold the face and every piece, with the margin", photoAspect, frame)
		}
	}

	// A wearer as wide as a landscape photo cannot fit at 2:3: the frame is
	// the tallest that fits, centered on the wearer, and stays inside.
	frame := coverFrame(nil, []Box{{0, 0.2, 1, 0.9}}, 16.0/9)
	if aspect := pixelAspect(frame, 16.0/9); math.Abs(aspect-coverAspect) > 0.01 || frame.Top != 0 || frame.Bottom != 1 {
		t.Fatalf("a wide wearer: frame %v (aspect %.3f), want full height at 2:3", frame, aspect)
	}
	if center := (frame.Left + frame.Right) / 2; math.Abs(center-0.5) > 0.01 {
		t.Fatalf("a wide wearer: frame %v should be centered on the wearer", frame)
	}

	// Nothing known: the tallest 2:3 cut from the middle.
	frame = coverFrame(nil, nil, 1)
	if math.Abs(pixelAspect(frame, 1)-coverAspect) > 0.01 || frame.Top != 0 || frame.Bottom != 1 {
		t.Fatalf("no boxes: frame %v, want the middle of the photo, full height", frame)
	}
}

// outfitWithTwoPhotos records a jacket on both test photos and a watch on
// photo_000002 only, an outfit of both, and an outfit reading of each photo
// (the wearer person_000001).
func outfitWithTwoPhotos(t *testing.T, store *Store, jacketBoxes [2]Box) Outfit {
	t.Helper()
	ctx := context.Background()
	photoJSON := func(photoID string, box Box) string {
		encoded, _ := json.Marshal(map[string]any{"photo_id": photoID, "box_left": box.Left, "box_top": box.Top, "box_right": box.Right, "box_bottom": box.Bottom})
		return string(encoded)
	}
	jacket, err := store.CreatePossession(ctx, fields(t, `{"name":"denim jacket","garment":{"category":"outerwear","garment_type":"jacket","primary_color":"blue"},
		"photos":[`+photoJSON("photo_000001", jacketBoxes[0])+`,`+photoJSON("photo_000002", jacketBoxes[1])+`]}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	watch, err := store.CreatePossession(ctx, fields(t, `{"name":"steel watch","watch":{},
		"photos":[`+photoJSON("photo_000002", Box{0.6, 0.5, 0.65, 0.55})+`]}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	status := "kept"
	outfit, err := store.CreateOutfit(ctx, OutfitInput{Status: &status, PossessionIDs: []string{jacket["id"].(string), watch["id"].(string)}}, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	for sequence, photoID := range []string{"photo_000001", "photo_000002"} {
		if _, err := store.db.Exec(`INSERT INTO photo_reviews (id, seq, photo_id, status, queued_by, queued_at, reading, wearer_person_id, outfit_id, worn_on)
			VALUES (?, ?, ?, 'done', 'principal_000001', 0, 'outfit', 'person_000001', ?, '2026-11-01')`,
			"photo_review_00000"+string(rune('1'+sequence)), sequence+1, photoID, outfit.ID); err != nil {
			t.Fatal(err)
		}
	}
	outfit, err = store.GetOutfit(outfit.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	return outfit
}

func TestAnOutfitsCoverIsThePhotoShowingTheMostOfIt(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	// The jacket is larger on photo_000001, but photo_000002 also shows the
	// watch: more pieces wins over more area.
	outfit := outfitWithTwoPhotos(t, store, [2]Box{{0.1, 0.1, 0.9, 0.9}, {0.3, 0.3, 0.6, 0.6}})
	if outfit.CoverPhotoID == nil || *outfit.CoverPhotoID != "photo_000002" {
		t.Fatalf("cover = %v, want photo_000002, which shows both pieces", outfit.CoverPhotoID)
	}
	reason := *outfit.CoverReason
	if reason.Rule != CoverRuleMostPiecesVisible || reason.PiecesVisible != 2 || reason.PiecesTotal != 2 || reason.PhotosCompared != 2 ||
		math.Abs(reason.WearerArea-0.35*0.3) > 0.001 {
		t.Fatalf("reason = %+v", reason)
	}

	// The owner picks the other photo; it holds, and says who picked it.
	picked, err := store.SetOutfitCover(ctx, outfit.ID, "photo_000001", "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if *picked.CoverPhotoID != "photo_000001" || picked.CoverReason.Rule != CoverRuleOwnerChoice || picked.CoverReason.ChosenBy != "principal_000001" ||
		picked.CoverReason.PiecesVisible != 1 {
		t.Fatalf("after the owner's pick: %v %+v", *picked.CoverPhotoID, picked.CoverReason)
	}
	if _, err := store.SetOutfitCover(ctx, outfit.ID, "photo_000009", "principal_000001"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a photo not of the outfit: %v, want ErrInvalid", err)
	}
	// A pick of a photo the outfit no longer has is named and set aside.
	if _, err := store.db.Exec(`UPDATE photo_reviews SET outfit_id = NULL WHERE photo_id = 'photo_000001'`); err != nil {
		t.Fatal(err)
	}
	moved, err := store.GetOutfit(outfit.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if *moved.CoverPhotoID != "photo_000002" || moved.CoverReason.Rule != CoverRuleMostPiecesVisible || moved.CoverReason.IgnoredOwnerPhotoID != "photo_000001" {
		t.Fatalf("after the picked photo left: %v %+v", *moved.CoverPhotoID, moved.CoverReason)
	}
	cleared, err := store.SetOutfitCover(ctx, outfit.ID, "", "principal_000001")
	if err != nil || cleared.CoverReason.IgnoredOwnerPhotoID != "" {
		t.Fatalf("clearing the pick: %+v %v", cleared.CoverReason, err)
	}
	revisions, err := store.Revisions(outfit.ID)
	if err != nil || len(revisions) == 0 {
		t.Fatalf("the cleared pick should be kept in revisions: %d %v", len(revisions), err)
	}
}

func TestATieOnPiecesGoesToTheLargerWearer(t *testing.T) {
	store := openTestStore(t)
	outfit := outfitWithTwoPhotos(t, store, [2]Box{{0.1, 0.1, 0.9, 0.9}, {0.3, 0.3, 0.6, 0.6}})
	// Take the watch out: both photos now show one piece, the jacket.
	if _, err := store.PatchOutfit(context.Background(), outfit.ID, OutfitInput{PossessionIDs: outfit.PossessionIDs[:1]}, "principal_000001"); err != nil {
		t.Fatal(err)
	}
	changed, err := store.GetOutfit(outfit.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if *changed.CoverPhotoID != "photo_000001" || changed.CoverReason.PiecesVisible != 1 || changed.CoverReason.WearerArea != 0.64 {
		t.Fatalf("cover %v %+v, want photo_000001 where the jacket spans more", *changed.CoverPhotoID, changed.CoverReason)
	}
}

func TestTheCoverRoutesCutATallImageAndTakeTheOwnersPick(t *testing.T) {
	store := openTestStore(t)
	outfit := outfitWithTwoPhotos(t, store, [2]Box{{0.1, 0.1, 0.9, 0.9}, {0.3, 0.3, 0.6, 0.6}})
	mux := http.NewServeMux()
	reviewer := &Reviewer{Store: store, Photos: portraitPhotos{}}
	(&Server{Store: store, Reviewer: reviewer}).RegisterHandlers(mux)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
		return recorder
	}

	got := call("GET", "/outfits/"+outfit.ID, "")
	var answered Outfit
	if err := json.Unmarshal(got.Body.Bytes(), &answered); err != nil || got.Code != 200 {
		t.Fatalf("GET outfit = %d %s", got.Code, got.Body)
	}
	if answered.CoverBox == nil || answered.CoverReason.WearerFaceFound == nil || !*answered.CoverReason.WearerFaceFound {
		t.Fatalf("the outfit should answer its frame, with the face found: %s", got.Body)
	}
	// The frame holds the face (0.4–0.6 across, from 0.05 down) and both pieces.
	if !holds(*answered.CoverBox, Box{0.3, 0.05, 0.65, 0.6}) {
		t.Fatalf("frame %v should hold the face and the pieces", *answered.CoverBox)
	}
	listed := call("GET", "/outfits", "")
	if listed.Code != 200 || !strings.Contains(listed.Body.String(), `"cover_box":{`) {
		t.Fatalf("GET /outfits = %d %s", listed.Code, listed.Body)
	}
	// The list starts cutting the covers in the background; wait for it, so
	// the image below is read from the kept cut.
	deadline := time.Now().Add(10 * time.Second)
	for reviewer.covers().warming.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if kept, _ := filepath.Glob(filepath.Join(store.CoverImagesDir(), "*.jpg")); len(kept) != 1 {
		t.Fatalf("listing should have cut the one cover, kept %v", kept)
	}

	image1 := call("GET", "/outfits/"+outfit.ID+"/image", "")
	if image1.Code != 200 || image1.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("image = %d %s", image1.Code, image1.Body)
	}
	decoded, _, err := image.Decode(image1.Body)
	if err != nil {
		t.Fatal(err)
	}
	if bounds := decoded.Bounds(); math.Abs(float64(bounds.Dx())/float64(bounds.Dy())-coverAspect) > 0.02 || bounds.Dy() > coverImageHeight {
		t.Fatalf("the cover image is %dx%d, want 2:3 and at most %d tall", bounds.Dx(), bounds.Dy(), coverImageHeight)
	}

	// The owner's pick of photo_000001, which has no confirmed face.
	put := call("PUT", "/outfits/"+outfit.ID+"/cover", `{"photo_id":"photo_000001","actor":"principal_000001"}`)
	if put.Code != 200 || !strings.Contains(put.Body.String(), `"rule":"owner_choice"`) || !strings.Contains(put.Body.String(), `"wearer_face_found":false`) {
		t.Fatalf("PUT cover = %d %s", put.Code, put.Body)
	}
	for body, want := range map[string]int{
		`{"photo_id":"photo_000009","actor":"principal_000001"}`: 400,
		`{"actor":"principal_000001"}`:                           400,
		`{"photo_id":"photo_000001"}`:                            400,
	} {
		if response := call("PUT", "/outfits/"+outfit.ID+"/cover", body); response.Code != want {
			t.Fatalf("PUT cover %s = %d %s, want %d", body, response.Code, response.Body, want)
		}
	}
	if response := call("GET", "/outfits/outfit_000099/image", ""); response.Code != 404 {
		t.Fatalf("image of no outfit = %d", response.Code)
	}
}
