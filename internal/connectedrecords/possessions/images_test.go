package possessions

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// asymmetricPicture has a bright block in its top left corner and a ramp,
// so every turn of it looks different.
func asymmetricPicture(width, height int) *image.RGBA {
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			level := uint8(40 + 100*x/width)
			if x < width/3 && y < height/4 {
				level = 250
			}
			picture.Set(x, y, color.RGBA{level, level, level, 255})
		}
	}
	return picture
}

func TestAnOriginalIsTurnedToStandAsItsThumbnail(t *testing.T) {
	raw := asymmetricPicture(300, 200)
	thumbnail := resized(Upright(raw, 6), 100) // the importer stood it upright by EXIF 6
	for _, claimed := range []int{6, 1, 8} {
		orientation, err := orientationMatching(raw, thumbnail, claimed)
		if err != nil || orientation != 6 {
			t.Fatalf("EXIF says %d: matched %d, %v; want 6 whatever EXIF says", claimed, orientation, err)
		}
	}
	unturned := resized(raw, 100) // a thumbnail made without turning it
	if orientation, err := orientationMatching(raw, unturned, 6); err != nil || orientation != 1 {
		t.Fatalf("a thumbnail never turned: matched %d, %v; want 1", orientation, err)
	}
	other := image.NewRGBA(image.Rect(0, 0, 67, 100))
	for x := 0; x < 67; x++ {
		for y := 0; y < 100; y++ {
			other.Set(x, y, color.RGBA{uint8(255 * ((x / 8) % 2)), 0, 0, 255})
		}
	}
	if _, err := orientationMatching(raw, other, 6); err == nil {
		t.Fatalf("a different picture should not match")
	}

	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, raw, nil); err != nil {
		t.Fatal(err)
	}
	aligned, orientation, err := alignOriginal(encoded.Bytes(), thumbnail)
	if err != nil || orientation != 6 || aligned.Bounds().Dx() != 200 || aligned.Bounds().Dy() != 300 {
		t.Fatalf("aligned %v, orientation %d, %v", aligned.Bounds(), orientation, err)
	}
	// The bright block was top left of the raw picture; turned by 6 it is
	// top right, so a box read on the thumbnail's top right cuts it.
	crop, err := detailCrop(aligned, Box{0.75, 0, 1, 0.33}, true)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, _ := image.Decode(bytes.NewReader(crop))
	if longest := max(decoded.Bounds().Dx(), decoded.Bounds().Dy()); longest < smallPieceLongestSide {
		t.Fatalf("a small piece should be scaled up to %d, got %d", smallPieceLongestSide, longest)
	}
	red, _, _, _ := decoded.At(decoded.Bounds().Dx()/2, decoded.Bounds().Dy()/3).RGBA()
	if red>>8 < 200 {
		t.Fatalf("the crop should show the bright block, got level %d", red>>8)
	}
}
