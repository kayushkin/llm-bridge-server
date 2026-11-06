package possessionstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// PhotoSource reads photos from photo-store, and the originals
// possession-add-photos copied into the data directory.
type PhotoSource interface {
	// ReviewImage answers the JPEG the readers are shown and what it is:
	// "original" or "thumbnail".
	ReviewImage(ctx context.Context, photoID string) ([]byte, string, error)
	// Thumbnail answers photo-store's thumbnail of a photo.
	Thumbnail(ctx context.Context, photoID string) ([]byte, error)
}

// PhotoStoreSource is the PhotoSource over photo-store and originals/.
type PhotoStoreSource struct {
	PhotoStoreURL string
	OriginalsDir  string
	LongestSide   int
	HTTPClient    *http.Client
}

func (source PhotoStoreSource) get(ctx context.Context, path string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.PhotoStoreURL+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := source.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("photo-store GET %s: %w", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("photo-store GET %s: %d %s", path, response.StatusCode, bytes.TrimSpace(body))
	}
	return body, nil
}

// Thumbnail answers photo-store's thumbnail.
func (source PhotoStoreSource) Thumbnail(ctx context.Context, photoID string) ([]byte, error) {
	return source.get(ctx, "/photos/"+url.PathEscape(photoID)+"/thumbnail")
}

// ReviewImage answers the original scaled to LongestSide when originals/
// holds it (found by the photo's sha256), and the thumbnail otherwise.
func (source PhotoStoreSource) ReviewImage(ctx context.Context, photoID string) ([]byte, string, error) {
	body, err := source.get(ctx, "/photos/"+url.PathEscape(photoID))
	if err != nil {
		return nil, "", err
	}
	var photo struct {
		Sha256 string `json:"sha256"`
	}
	if err := json.Unmarshal(body, &photo); err != nil {
		return nil, "", fmt.Errorf("photo-store's %s is not a photo: %w", photoID, err)
	}
	matches, err := filepath.Glob(filepath.Join(source.OriginalsDir, photo.Sha256+".*"))
	if err != nil {
		return nil, "", err
	}
	if len(matches) > 0 {
		original, err := os.ReadFile(matches[0])
		if err != nil {
			return nil, "", err
		}
		scaled, err := scaleJPEG(original, source.LongestSide)
		if err != nil {
			return nil, "", fmt.Errorf("scale %s: %w", matches[0], err)
		}
		return scaled, "original", nil
	}
	thumbnail, err := source.Thumbnail(ctx, photoID)
	if err != nil {
		return nil, "", err
	}
	return thumbnail, "thumbnail", nil
}

// scaleJPEG decodes an image, stands it upright by its EXIF orientation, and
// re-encodes it as a JPEG whose longest side is at most longestSide.
func scaleJPEG(original []byte, longestSide int) ([]byte, error) {
	facts, err := ReadExif(original)
	if err != nil {
		return nil, fmt.Errorf("read EXIF: %w", err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(original))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	decoded = Upright(decoded, facts.Orientation)
	bounds := decoded.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if longest := max(width, height); longest > longestSide {
		width = width * longestSide / longest
		height = height * longestSide / longest
	}
	target := image.NewRGBA(image.Rect(0, 0, max(width, 1), max(height, 1)))
	draw.Draw(target, target.Bounds(), image.White, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(target, target.Bounds(), decoded, bounds, draw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, target, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// UprightJPEG answers a JPEG standing upright: the file itself when its
// EXIF says it already is, a re-encoded copy otherwise.
func UprightJPEG(original []byte) ([]byte, error) {
	facts, err := ReadExif(original)
	if err != nil {
		return nil, fmt.Errorf("read EXIF: %w", err)
	}
	if facts.Orientation == 1 {
		return original, nil
	}
	decoded, _, err := image.Decode(bytes.NewReader(original))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, Upright(decoded, facts.Orientation), &jpeg.Options{Quality: 92}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Box is a region of a photo in fractions of its width and height, from the
// top left.
type Box struct {
	Left   float64 `json:"left"`
	Top    float64 `json:"top"`
	Right  float64 `json:"right"`
	Bottom float64 `json:"bottom"`
}

func (box Box) valid() bool {
	return box.Left >= 0 && box.Top >= 0 && box.Right <= 1 && box.Bottom <= 1 && box.Left < box.Right && box.Top < box.Bottom
}

// clamped pulls a box a model gave into the photo.
func (box Box) clamped() Box {
	clamp := func(value float64) float64 { return min(max(value, 0), 1) }
	box = Box{clamp(box.Left), clamp(box.Top), clamp(box.Right), clamp(box.Bottom)}
	if box.Left > box.Right {
		box.Left, box.Right = box.Right, box.Left
	}
	if box.Top > box.Bottom {
		box.Top, box.Bottom = box.Bottom, box.Top
	}
	return box
}

func (box Box) area() float64 { return max(box.Right-box.Left, 0) * max(box.Bottom-box.Top, 0) }

// overlap is the intersection over union of two boxes, 0 to 1.
func (box Box) overlap(other Box) float64 {
	intersection := Box{max(box.Left, other.Left), max(box.Top, other.Top), min(box.Right, other.Right), min(box.Bottom, other.Bottom)}
	if intersection.Left >= intersection.Right || intersection.Top >= intersection.Bottom {
		return 0
	}
	union := box.area() + other.area() - intersection.area()
	if union <= 0 {
		return 0
	}
	return intersection.area() / union
}

// cropJPEG cuts a box, with a margin of a tenth of its size on each side,
// out of a JPEG.
func cropJPEG(photo []byte, box Box) ([]byte, error) {
	decoded, _, err := image.Decode(bytes.NewReader(photo))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	bounds := decoded.Bounds()
	marginX := (box.Right - box.Left) * 0.1
	marginY := (box.Bottom - box.Top) * 0.1
	padded := Box{box.Left - marginX, box.Top - marginY, box.Right + marginX, box.Bottom + marginY}.clamped()
	region := image.Rect(
		bounds.Min.X+int(padded.Left*float64(bounds.Dx())), bounds.Min.Y+int(padded.Top*float64(bounds.Dy())),
		bounds.Min.X+int(padded.Right*float64(bounds.Dx())), bounds.Min.Y+int(padded.Bottom*float64(bounds.Dy())))
	if region.Dx() < 1 || region.Dy() < 1 {
		return nil, fmt.Errorf("the box is smaller than a pixel")
	}
	target := image.NewRGBA(image.Rect(0, 0, region.Dx(), region.Dy()))
	draw.Draw(target, target.Bounds(), decoded, region.Min, draw.Src)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, target, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
