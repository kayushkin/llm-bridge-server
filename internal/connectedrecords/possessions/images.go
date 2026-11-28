package possessions

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"strings"

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
	// WearerFace answers where a person's face is in a photo, from the face
	// photo-store confirmed as theirs; an error when none is confirmed.
	WearerFace(ctx context.Context, photoID, personID string) (Box, error)
	// TakenAt answers when photo-store says the photo was taken, epoch
	// seconds; 0 when it does not know.
	TakenAt(ctx context.Context, photoID string) (int64, error)
	// WhyACopy answers why photo-store's photo is a copy of another photo
	// rather than a photo of its own day (a screenshot), "" when it is not.
	WhyACopy(ctx context.Context, photoID string) (string, error)
	// DetailImage answers the photo at full size, turned to stand as its
	// thumbnail stands, so a box read on the thumbnail cuts the same place:
	// the original from originals/ ("original"), or the thumbnail itself
	// when there is none ("thumbnail").
	DetailImage(ctx context.Context, photoID string) (image.Image, string, error)
}

// OriginalReader reads the original an owner keeps (originals.go).
type OriginalReader interface {
	OriginalBytes(ctx context.Context, ownerPrincipalID, sha256 string) ([]byte, bool, error)
}

// PhotoStoreReads is what possession-store reads from photo-store, as the
// owner the context's ownership.Caller names. A photo outside that owner's
// records is not found, as a missing one.
type PhotoStoreReads interface {
	// Photo is the photo's record.
	Photo(ctx context.Context, photoID string) (PhotoFacts, error)
	// Thumbnail is the photo's thumbnail JPEG.
	Thumbnail(ctx context.Context, photoID string) ([]byte, error)
	// ConfirmedFacesOfPerson is the face ids photo-store confirmed as the
	// person's on the photo, in photo-store's order; a link to the person
	// with no face gives no id.
	ConfirmedFacesOfPerson(ctx context.Context, photoID, personID string) ([]string, error)
	// Faces is every face found on the photo.
	Faces(ctx context.Context, photoID string) ([]PhotoFace, error)
}

// PhotoFacts is the part of photo-store's photo possession-store reads.
type PhotoFacts struct {
	OwnerPrincipalID string   `json:"owner_principal_id"`
	Sha256           string   `json:"sha256"`
	TakenAt          int64    `json:"taken_at"`
	SourcePath       string   `json:"source_path"`
	Tags             []string `json:"tags"`
}

// PhotoFace is one face on a photo.
type PhotoFace struct {
	ID  string `json:"id"`
	Box Box    `json:"box"`
}

// PhotoStoreSource is the PhotoSource over photo-store and the kept
// originals.
type PhotoStoreSource struct {
	PhotoStore  PhotoStoreReads
	Originals   OriginalReader
	LongestSide int
}

// WearerFace finds the person's confirmed face on the photo.
func (source PhotoStoreSource) WearerFace(ctx context.Context, photoID, personID string) (Box, error) {
	faceIDs, err := source.PhotoStore.ConfirmedFacesOfPerson(ctx, photoID, personID)
	if err != nil {
		return Box{}, err
	}
	faceID := ""
	for _, id := range faceIDs {
		if id != "" {
			faceID = id
		}
	}
	if faceID == "" {
		return Box{}, fmt.Errorf("%w: photo-store has no confirmed face of %s on %s", ErrInvalid, personID, photoID)
	}
	faces, err := source.PhotoStore.Faces(ctx, photoID)
	if err != nil {
		return Box{}, err
	}
	for _, face := range faces {
		if face.ID == faceID {
			return face.Box.clamped(), nil
		}
	}
	return Box{}, fmt.Errorf("photo-store links %s to %s, but the photo has no such face", faceID, photoID)
}

// TakenAt reads the photo's taken_at.
func (source PhotoStoreSource) TakenAt(ctx context.Context, photoID string) (int64, error) {
	photo, err := source.PhotoStore.Photo(ctx, photoID)
	if err != nil {
		return 0, err
	}
	return photo.TakenAt, nil
}

// WhyACopy reads photo-store's source path and tags: a file whose name
// begins "Screenshot", or a photo tagged screenshot, is a copy.
func (source PhotoStoreSource) WhyACopy(ctx context.Context, photoID string) (string, error) {
	photo, err := source.PhotoStore.Photo(ctx, photoID)
	if err != nil {
		return "", err
	}
	return whyACopy(photo.SourcePath, photo.Tags), nil
}

func whyACopy(sourcePath string, tags []string) string {
	name := sourcePath[strings.LastIndexAny(sourcePath, `/\`)+1:]
	if strings.HasPrefix(strings.ToLower(name), "screenshot") {
		return "a screenshot (" + name + "), not a photo of its own day"
	}
	for _, tag := range tags {
		if strings.EqualFold(tag, "screenshot") {
			return "tagged screenshot in photo-store, not a photo of its own day"
		}
	}
	return ""
}

// Thumbnail answers photo-store's thumbnail.
func (source PhotoStoreSource) Thumbnail(ctx context.Context, photoID string) ([]byte, error) {
	return source.PhotoStore.Thumbnail(ctx, photoID)
}

// ReviewImage answers the photo the readers are shown: the original,
// turned as its thumbnail stands and scaled to LongestSide, when the owner
// keeps one, and the thumbnail otherwise.
func (source PhotoStoreSource) ReviewImage(ctx context.Context, photoID string) ([]byte, string, error) {
	picture, kind, err := source.DetailImage(ctx, photoID)
	if err != nil {
		return nil, "", err
	}
	if kind == "thumbnail" {
		thumbnail, err := source.Thumbnail(ctx, photoID)
		return thumbnail, kind, err
	}
	scaled, err := encodeScaled(picture, source.LongestSide)
	if err != nil {
		return nil, "", fmt.Errorf("scale the original of %s: %w", photoID, err)
	}
	return scaled, kind, nil
}

// DetailImage finds the original the photo's owner keeps by the photo's
// sha256 and turns it to match photo-store's thumbnail (alignToThumbnail).
func (source PhotoStoreSource) DetailImage(ctx context.Context, photoID string) (image.Image, string, error) {
	photo, err := source.PhotoStore.Photo(ctx, photoID)
	if err != nil {
		return nil, "", err
	}
	thumbnailBytes, err := source.Thumbnail(ctx, photoID)
	if err != nil {
		return nil, "", err
	}
	thumbnail, _, err := image.Decode(bytes.NewReader(thumbnailBytes))
	if err != nil {
		return nil, "", fmt.Errorf("decode the thumbnail of %s: %w", photoID, err)
	}
	original, kept, err := source.Originals.OriginalBytes(ctx, photo.OwnerPrincipalID, photo.Sha256)
	if err != nil {
		return nil, "", err
	}
	if !kept {
		return thumbnail, "thumbnail", nil
	}
	aligned, _, err := alignOriginal(original, thumbnail)
	if err != nil {
		return nil, "", fmt.Errorf("%s (original %s): %w", photoID, photo.Sha256, err)
	}
	return aligned, "original", nil
}

// alignOriginal decodes an original and turns it to stand as its thumbnail
// does. Boxes are read on photo-store's thumbnails, which the importer made
// upright by EXIF; an original is turned by its EXIF orientation too, and
// this checks that against the thumbnail itself, trying every orientation
// on a small copy and keeping the one whose shape and pixels match best
// (EXIF's on a near tie). It answers the orientation used. A picture that
// matches the thumbnail in no orientation is an error, never a guess.
func alignOriginal(original []byte, thumbnail image.Image) (image.Image, int, error) {
	facts, err := ReadExif(original)
	if err != nil {
		return nil, 0, fmt.Errorf("read EXIF: %w", err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(original))
	if err != nil {
		return nil, 0, fmt.Errorf("decode: %w", err)
	}
	orientation, err := orientationMatching(decoded, thumbnail, facts.Orientation)
	if err != nil {
		return nil, 0, err
	}
	return Upright(decoded, orientation), orientation, nil
}

// orientationMatching answers the orientation (1 to 8, as EXIF numbers
// them) that turns picture to look like reference.
func orientationMatching(picture, reference image.Image, exifOrientation int) (int, error) {
	small := resized(picture, 96)
	referenceBounds := reference.Bounds()
	referenceAspect := float64(referenceBounds.Dx()) / float64(referenceBounds.Dy())
	referenceGray := grayGrid(reference)
	best, bestDifference := 0, 1e9
	differences := map[int]float64{}
	for orientation := 1; orientation <= 8; orientation++ {
		turned := Upright(small, orientation)
		bounds := turned.Bounds()
		aspect := float64(bounds.Dx()) / float64(bounds.Dy())
		if aspect/referenceAspect > 1.04 || referenceAspect/aspect > 1.04 {
			continue
		}
		difference := grayDifference(grayGrid(turned), referenceGray)
		differences[orientation] = difference
		if difference < bestDifference {
			best, bestDifference = orientation, difference
		}
	}
	if best == 0 {
		return 0, fmt.Errorf("the original is %dx%d and its thumbnail %dx%d: no orientation gives the thumbnail's shape",
			picture.Bounds().Dx(), picture.Bounds().Dy(), referenceBounds.Dx(), referenceBounds.Dy())
	}
	if bestDifference > 40 {
		return 0, fmt.Errorf("the original does not look like its thumbnail in any orientation (closest differs by %.0f of 255)", bestDifference)
	}
	if exif, measured := differences[exifOrientation]; measured && exif <= bestDifference+2 {
		return exifOrientation, nil
	}
	return best, nil
}

// resized scales a picture so its longest side is longest.
func resized(picture image.Image, longest int) image.Image {
	bounds := picture.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	scale := float64(longest) / float64(max(width, height))
	target := image.NewRGBA(image.Rect(0, 0, max(int(float64(width)*scale), 1), max(int(float64(height)*scale), 1)))
	draw.ApproxBiLinear.Scale(target, target.Bounds(), picture, bounds, draw.Src, nil)
	return target
}

const grayGridSide = 24

// grayGrid is a picture as a 24×24 grid of gray levels.
func grayGrid(picture image.Image) []float64 {
	target := image.NewRGBA(image.Rect(0, 0, grayGridSide, grayGridSide))
	draw.ApproxBiLinear.Scale(target, target.Bounds(), picture, picture.Bounds(), draw.Src, nil)
	grid := make([]float64, 0, grayGridSide*grayGridSide)
	for y := 0; y < grayGridSide; y++ {
		for x := 0; x < grayGridSide; x++ {
			red, green, blue, _ := target.At(x, y).RGBA()
			grid = append(grid, (0.299*float64(red)+0.587*float64(green)+0.114*float64(blue))/257)
		}
	}
	return grid
}

func grayDifference(first, second []float64) float64 {
	total := 0.0
	for index := range first {
		difference := first[index] - second[index]
		if difference < 0 {
			difference = -difference
		}
		total += difference
	}
	return total / float64(len(first))
}

// encodeScaled encodes a picture as a JPEG whose longest side is at most
// longestSide.
func encodeScaled(picture image.Image, longestSide int) ([]byte, error) {
	bounds := picture.Bounds()
	if max(bounds.Dx(), bounds.Dy()) > longestSide {
		picture = resizedSmooth(picture, longestSide)
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, picture, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func resizedSmooth(picture image.Image, longest int) image.Image {
	bounds := picture.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	scale := float64(longest) / float64(max(width, height))
	target := image.NewRGBA(image.Rect(0, 0, max(int(float64(width)*scale), 1), max(int(float64(height)*scale), 1)))
	draw.CatmullRom.Scale(target, target.Bounds(), picture, bounds, draw.Src, nil)
	return target
}

// Small pieces (jewelry, watches, eyewear) are cut with more room around
// them and scaled up to at least smallPieceLongestSide, so a reader sees
// the detail a ring or a dial needs; other pieces are cut tight and kept
// at most otherPieceLongestSide.
const (
	smallPieceLongestSide = 768
	otherPieceLongestSide = 768
)

// smallPieceGroup says whether pieces of a group are small enough to cut
// with room and scale up.
func smallPieceGroup(group string) bool {
	return group == "watch" || group == "eyewear" || strings.HasPrefix(group, "jewelry:")
}

// detailCrop cuts a box out of a full-size picture for identifying the
// piece in it.
func detailCrop(picture image.Image, box Box, small bool) ([]byte, error) {
	bounds := picture.Bounds()
	margin := 0.08
	if small {
		margin = 0.3
	}
	marginX, marginY := (box.Right-box.Left)*margin, (box.Bottom-box.Top)*margin
	padded := Box{box.Left - marginX, box.Top - marginY, box.Right + marginX, box.Bottom + marginY}.clamped()
	region := image.Rect(
		bounds.Min.X+int(padded.Left*float64(bounds.Dx())), bounds.Min.Y+int(padded.Top*float64(bounds.Dy())),
		bounds.Min.X+int(padded.Right*float64(bounds.Dx())), bounds.Min.Y+int(padded.Bottom*float64(bounds.Dy())))
	if region.Dx() < 1 || region.Dy() < 1 {
		return nil, fmt.Errorf("the box is smaller than a pixel")
	}
	longest := max(region.Dx(), region.Dy())
	target := longest
	switch {
	case small && longest < smallPieceLongestSide:
		target = smallPieceLongestSide
	case longest > otherPieceLongestSide:
		target = otherPieceLongestSide
	}
	width, height := region.Dx()*target/longest, region.Dy()*target/longest
	out := image.NewRGBA(image.Rect(0, 0, max(width, 1), max(height, 1)))
	draw.CatmullRom.Scale(out, out.Bounds(), picture, region, draw.Src, nil)
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, out, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
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

// PossessionImage cuts a possession out of its cover photo, read from
// photos; with no box it is the whole thumbnail.
func (s *Store) PossessionImage(ctx context.Context, photos PhotoSource, possessionID string) ([]byte, error) {
	links, err := possessionPhotos(s.db, possessionID)
	if err != nil {
		return nil, err
	}
	if err := requireLivePossession(s.db, possessionID); err != nil {
		var deletedAt int64
		if s.db.QueryRow(`SELECT deleted_at FROM possessions WHERE id = ?`, possessionID).Scan(&deletedAt) != nil {
			return nil, err
		}
	}
	for _, photo := range links {
		if !photo.IsCover {
			continue
		}
		thumbnail, err := photos.Thumbnail(ctx, photo.PhotoID)
		if err != nil {
			return nil, err
		}
		if photo.BoxLeft == nil {
			return thumbnail, nil
		}
		return cropJPEG(thumbnail, Box{*photo.BoxLeft, *photo.BoxTop, *photo.BoxRight, *photo.BoxBottom})
	}
	return nil, fmt.Errorf("%w: %s has no photo", ErrNotFound, possessionID)
}
