package thumbnail

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestMakeShrinksToTheLongestSideAndKeepsTheOriginalSize(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 3000, 1500))
	source.Set(10, 10, color.NRGBA{255, 0, 0, 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	thumb, width, height, err := Make(encoded.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if width != 3000 || height != 1500 {
		t.Errorf("original size = %dx%d", width, height)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(thumb))
	if err != nil {
		t.Fatalf("thumbnail is not a JPEG: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != 1024 || b.Dy() != 512 {
		t.Errorf("thumbnail = %dx%d, want 1024x512", b.Dx(), b.Dy())
	}
	if _, _, _, err := Make([]byte("not an image")); err == nil {
		t.Error("garbage must be an error")
	}
}
