package possessions_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions/possessionstest"
)

func TestAGarmentIsMadeReadAndChangedWithItsKind(t *testing.T) {
	store := possessionstest.OpenTestStore(t)
	ctx := context.Background()
	created, err := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"black hoodie","brand":"Carhartt","place_id":"place_000001",
		"garment":{"category":"top","garment_type":"hoodie","primary_color":"black","seasons":["fall","winter"],"warmth":4},
		"photos":[{"photo_id":"photo_000001","box_left":0.1,"box_top":0.1,"box_right":0.5,"box_bottom":0.6}]}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if created["id"] != "possession_000001" || created["kind"] != "garment" || created["status"] != "owned" {
		t.Fatalf("created = %v", created)
	}
	garment := created["garment"].(map[string]any)
	if garment["id"] != "garment_000001" || garment["warmth"] != int64(4) || len(garment["seasons"].([]string)) != 2 {
		t.Fatalf("garment = %v", garment)
	}
	if created["cover_photo_id"] != "photo_000001" {
		t.Fatalf("the first photo should be the cover, got %v", created["cover_photo_id"])
	}
	patched, err := store.PatchPossession(ctx, "possession_000001", possessionstest.Fields(t, `{"spot":"closet","garment":{"fit":"relaxed"}}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if patched["spot"] != "closet" || patched["garment"].(map[string]any)["fit"] != "relaxed" || patched["garment"].(map[string]any)["primary_color"] != "black" {
		t.Fatalf("patched = %v", patched)
	}
	revisions, err := store.Revisions("possession_000001")
	if err != nil || len(revisions) != 2 {
		t.Fatalf("revisions = %d %v, want the possession row and the garment row", len(revisions), err)
	}
	if _, err := store.PatchPossession(ctx, "possession_000001", possessionstest.Fields(t, `{"computer":{"form_factor":"laptop"}}`), "principal_000001"); !errors.Is(err, possessions.ErrConflict) {
		t.Fatalf("giving a garment a second kind = %v, want a conflict", err)
	}
}

func TestAWriteRefusesWhatItCannotStore(t *testing.T) {
	store := possessionstest.OpenTestStore(t)
	ctx := context.Background()
	for _, body := range []string{
		`{"brand":"no name"}`,
		`{"name":"x","colour":"red"}`,
		`{"name":"x","garment":{"category":"top","primary_color":"maroon"}}`,
		`{"name":"x","garment":{"garment_type":"hoodie"}}`,
		`{"name":"x","garment":{"category":"top"},"computer":{"form_factor":"laptop"}}`,
		`{"name":"x","place_id":"place_999999"}`,
		`{"name":"x","price_cents":1000}`,
		`{"name":"x","computer":{"form_factor":"laptop","harness_machine_id":"m_unknown"}}`,
		`{"name":"x","photos":[{"photo_id":"photo_000001","box_left":0.5}]}`,
	} {
		if _, err := store.CreatePossession(ctx, possessionstest.Fields(t, body), "principal_000001"); !errors.Is(err, possessions.ErrInvalid) {
			t.Errorf("%s = %v, want invalid", body, err)
		}
	}
	if _, err := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"x"}`), ""); !errors.Is(err, possessions.ErrInvalid) {
		t.Errorf("a write with no actor = %v, want invalid", err)
	}
}

func TestAComputerIsFoundByItsHarnessMachineAndHoldsItsParts(t *testing.T) {
	store := possessionstest.OpenTestStore(t)
	ctx := context.Background()
	computer, err := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"desktop","computer":{"form_factor":"desktop","harness_machine_id":"m_test_machine","memory_gigabytes":64}}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	computerID := computer["computer"].(map[string]any)["id"].(string)
	if _, err := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"2 TB drive","computer_part":{"part_type":"storage_drive","installed_in_computer_id":"`+computerID+`","capacity_gigabytes":2000}}`), "principal_000001"); err != nil {
		t.Fatal(err)
	}
	found, err := store.ComputerForHarnessMachine("m_test_machine")
	if err != nil {
		t.Fatal(err)
	}
	if found["id"] != computer["id"] || len(found["installed_part_ids"].([]string)) != 1 {
		t.Fatalf("found = %v", found)
	}
	if _, err := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"another","computer":{"form_factor":"laptop","harness_machine_id":"m_test_machine"}}`), "principal_000001"); !errors.Is(err, possessions.ErrConflict) {
		t.Fatalf("a second computer on one machine = %v, want a conflict", err)
	}
}

func TestUprightTurnsASidewaysPhoto(t *testing.T) {
	picture := image.NewRGBA(image.Rect(0, 0, 2, 1))
	picture.Set(0, 0, color.RGBA{255, 0, 0, 255})
	picture.Set(1, 0, color.RGBA{0, 0, 255, 255})
	turned := possessions.Upright(picture, 6)
	if turned.Bounds().Dx() != 1 || turned.Bounds().Dy() != 2 {
		t.Fatalf("bounds = %v", turned.Bounds())
	}
	if red, _, _, _ := turned.At(0, 0).RGBA(); red == 0 {
		t.Fatalf("orientation 6 should put the left pixel on top")
	}
}

func TestReadExifFindsTheDateOrientationAndPlace(t *testing.T) {
	file := jpegWithExif(t)
	facts, err := possessions.ReadExif(file)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Orientation != 6 || facts.TakenLocal != "2026:11:06 12:30:00" || facts.TakenOffset != "-08:00" {
		t.Fatalf("facts = %+v", facts)
	}
	if facts.Latitude == nil || *facts.Latitude != 12.5 || *facts.Longitude != -45.25 {
		t.Fatalf("place = %v %v", facts.Latitude, facts.Longitude)
	}
	taken, ok, err := facts.TakenAt(nil)
	if err != nil || !ok || taken.UTC().Hour() != 20 {
		t.Fatalf("taken = %v %v %v", taken, ok, err)
	}
}

// jpegWithExif builds a little-endian EXIF block by hand: IFD0 with the
// orientation and pointers to the EXIF and GPS IFDs.
func jpegWithExif(t *testing.T) []byte {
	t.Helper()
	var tiff bytes.Buffer
	le := func(values ...any) {
		for _, value := range values {
			switch typed := value.(type) {
			case uint16:
				tiff.Write([]byte{byte(typed), byte(typed >> 8)})
			case uint32:
				tiff.Write([]byte{byte(typed), byte(typed >> 8), byte(typed >> 16), byte(typed >> 24)})
			}
		}
	}
	tiff.WriteString("II")
	le(uint16(42), uint32(8))
	// IFD0 at 8: three entries, then next-IFD 0. Ends at 8+2+36+4 = 50.
	le(uint16(3))
	le(uint16(0x0112), uint16(3), uint32(1), uint16(6), uint16(0))
	le(uint16(0x8769), uint16(4), uint32(1), uint32(50))
	le(uint16(0x8825), uint16(4), uint32(1), uint32(80))
	le(uint32(0))
	// EXIF IFD at 50: two entries; ends at 50+2+24+4 = 80; values after the GPS IFD.
	le(uint16(2))
	le(uint16(0x9003), uint16(2), uint32(20), uint32(200))
	le(uint16(0x9011), uint16(2), uint32(7), uint32(220))
	le(uint32(0))
	// GPS IFD at 80: four entries; ends at 80+2+48+4 = 134.
	le(uint16(4))
	le(uint16(1), uint16(2), uint32(2), uint32('N'))
	le(uint16(2), uint16(5), uint32(3), uint32(140))
	le(uint16(3), uint16(2), uint32(2), uint32('W'))
	le(uint16(4), uint16(5), uint32(3), uint32(164))
	le(uint32(0))
	for tiff.Len() < 140 {
		tiff.WriteByte(0)
	}
	le(uint32(12), uint32(1), uint32(30), uint32(1), uint32(0), uint32(1)) // 12°30'0" N
	le(uint32(45), uint32(1), uint32(15), uint32(1), uint32(0), uint32(1)) // 45°15'0" W
	for tiff.Len() < 200 {
		tiff.WriteByte(0)
	}
	tiff.WriteString("2026:11:06 12:30:00\x00")
	tiff.WriteString("-08:00\x00")
	segment := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	file := []byte{0xFF, 0xD8, 0xFF, 0xE1, byte((len(segment) + 2) >> 8), byte(len(segment) + 2)}
	file = append(file, segment...)
	return append(file, possessionstest.JPEG()[2:]...)
}

func TestAMergeMovesPhotosKindAndLinksOntoTheKeptPossession(t *testing.T) {
	store := possessionstest.OpenTestStore(t)
	ctx := context.Background()
	kept, err := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"GoXLR Mini","photos":[{"photo_id":"photo_000001"}]}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"streaming mixer","brand":"TC-Helicon","audio_device":{"device_type":"mixer"},
		"photos":[{"photo_id":"photo_000002","box_left":0.1,"box_top":0.1,"box_right":0.3,"box_bottom":0.3}]}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	keptID, duplicateID := kept["id"].(string), duplicate["id"].(string)
	merged, err := store.MergePossession(ctx, duplicateID, keptID, "principal_000001", "")
	if err != nil {
		t.Fatal(err)
	}
	if merged["name"] != "GoXLR Mini" || merged["brand"] != "TC-Helicon" || merged["kind"] != "audio_device" || len(merged["photos"].([]possessions.PhotoLink)) != 2 || merged["cover_photo_id"] != "photo_000001" {
		t.Fatalf("merged = %v", merged)
	}
	if _, err := store.GetPossession(duplicateID, false); !errors.Is(err, possessions.ErrNotFound) {
		t.Fatalf("the duplicate should be gone from reads, got %v", err)
	}
	gone, _ := store.GetPossession(duplicateID, true)
	if gone["merged_into_id"] != keptID {
		t.Fatalf("the duplicate should say where it went: %v", gone["merged_into_id"])
	}
	if _, err := store.MergePossession(ctx, keptID, keptID, "principal_000001", ""); !errors.Is(err, possessions.ErrInvalid) {
		t.Fatalf("merging into itself = %v", err)
	}
	laptop, _ := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"laptop","computer":{"form_factor":"laptop"}}`), "principal_000001")
	if _, err := store.MergePossession(ctx, laptop["id"].(string), keptID, "principal_000001", ""); !errors.Is(err, possessions.ErrConflict) {
		t.Fatalf("merging a computer into an audio device = %v, want a conflict", err)
	}
}

func TestEveryKindCanBeMade(t *testing.T) {
	store := possessionstest.OpenTestStore(t)
	ctx := context.Background()
	for _, kind := range possessions.KindTypes {
		required := ""
		for _, column := range kind.Columns {
			if !column.Required {
				continue
			}
			if column.Required {
				required = fmt.Sprintf(`"%s":%q`, column.Name, column.Vocabulary[0])
			}
		}
		made, err := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"x","room":"office","`+kind.Name+`":{`+required+`}}`), "principal_000001")
		if err != nil {
			t.Fatalf("%s: %v", kind.Name, err)
		}
		if made["kind"] != kind.Name || made["room"] != "office" {
			t.Fatalf("%s made as %v", kind.Name, made["kind"])
		}
	}
}

func TestAPossessionMovesToAnotherKind(t *testing.T) {
	store := possessionstest.OpenTestStore(t)
	ctx := context.Background()
	watch, _ := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"Seiko","garment":{"category":"accessory"}}`), "principal_000001")
	id := watch["id"].(string)
	moved, err := store.ChangeKind(ctx, id, "watch", possessionstest.Fields(t, `{"movement":"quartz"}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if moved["kind"] != "watch" || moved["garment"] != nil || moved["watch"].(map[string]any)["movement"] != "quartz" {
		t.Fatalf("moved = %v", moved)
	}
	if _, err := store.ChangeKind(ctx, id, "watch", nil, "principal_000001"); !errors.Is(err, possessions.ErrConflict) {
		t.Fatalf("moving to the kind it has = %v", err)
	}
	if _, err := store.ChangeKind(ctx, id, "jewelry", possessionstest.Fields(t, `{}`), "principal_000001"); !errors.Is(err, possessions.ErrInvalid) {
		t.Fatalf("a kind with its required field missing = %v", err)
	}
	if still, _ := store.GetPossession(id, false); still["kind"] != "watch" {
		t.Fatalf("a refused move should change nothing: %v", still["kind"])
	}
}
