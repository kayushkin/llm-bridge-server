// Package thumbnail makes the JPEG thumbnails photo-store keeps: the longest
// side at most 1024 pixels, quality 80, from a JPEG, PNG, GIF or WebP.
package thumbnail

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// LongestSide is the thumbnail's largest dimension.
const LongestSide = 1024

// Make decodes an image and returns its thumbnail as JPEG, with the original's
// width and height. A GIF gives its first frame.
func Make(original []byte) (thumbnailJPEG []byte, width, height int, err error) {
	source, _, err := image.Decode(bytes.NewReader(original))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode: %w", err)
	}
	bounds := source.Bounds()
	width, height = bounds.Dx(), bounds.Dy()
	if width == 0 || height == 0 {
		return nil, 0, 0, fmt.Errorf("the image is %dx%d", width, height)
	}
	targetWidth, targetHeight := width, height
	if longest := max(width, height); longest > LongestSide {
		targetWidth = width * LongestSide / longest
		targetHeight = height * LongestSide / longest
	}
	target := image.NewRGBA(image.Rect(0, 0, max(targetWidth, 1), max(targetHeight, 1)))
	// A transparent PNG would turn black in a JPEG; paint white under it.
	draw.Draw(target, target.Bounds(), image.White, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(target, target.Bounds(), source, bounds, draw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, target, &jpeg.Options{Quality: 80}); err != nil {
		return nil, 0, 0, err
	}
	return out.Bytes(), width, height, nil
}
