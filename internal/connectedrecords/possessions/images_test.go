package possessions_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions/possessionstest"
)

func TestAnOriginalIsTurnedToStandAsItsThumbnail(t *testing.T) {
	raw := possessionstest.AsymmetricPicture(300, 200)
	thumbnail := possessions.Resized(possessions.Upright(raw, 6), 100) // the importer stood it upright by EXIF 6
	for _, claimed := range []int{6, 1, 8} {
		orientation, err := possessions.OrientationMatching(raw, thumbnail, claimed)
		if err != nil || orientation != 6 {
			t.Fatalf("EXIF says %d: matched %d, %v; want 6 whatever EXIF says", claimed, orientation, err)
		}
	}
	unturned := possessions.Resized(raw, 100) // a thumbnail made without turning it
	if orientation, err := possessions.OrientationMatching(raw, unturned, 6); err != nil || orientation != 1 {
		t.Fatalf("a thumbnail never turned: matched %d, %v; want 1", orientation, err)
	}
	other := image.NewRGBA(image.Rect(0, 0, 67, 100))
	for x := 0; x < 67; x++ {
		for y := 0; y < 100; y++ {
			other.Set(x, y, color.RGBA{uint8(255 * ((x / 8) % 2)), 0, 0, 255})
		}
	}
	if _, err := possessions.OrientationMatching(raw, other, 6); err == nil {
		t.Fatalf("a different picture should not match")
	}

	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, raw, nil); err != nil {
		t.Fatal(err)
	}
	aligned, orientation, err := possessions.AlignOriginal(encoded.Bytes(), thumbnail)
	if err != nil || orientation != 6 || aligned.Bounds().Dx() != 200 || aligned.Bounds().Dy() != 300 {
		t.Fatalf("aligned %v, orientation %d, %v", aligned.Bounds(), orientation, err)
	}
	// The bright block was top left of the raw picture; turned by 6 it is
	// top right, so a box read on the thumbnail's top right cuts it.
	crop, err := possessions.DetailCrop(aligned, possessions.Box{0.75, 0, 1, 0.33}, true)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, _ := image.Decode(bytes.NewReader(crop))
	if longest := max(decoded.Bounds().Dx(), decoded.Bounds().Dy()); longest < possessions.SmallPieceLongestSide {
		t.Fatalf("a small piece should be scaled up to %d, got %d", possessions.SmallPieceLongestSide, longest)
	}
	red, _, _, _ := decoded.At(decoded.Bounds().Dx()/2, decoded.Bounds().Dy()/3).RGBA()
	if red>>8 < 200 {
		t.Fatalf("the crop should show the bright block, got level %d", red>>8)
	}
}
