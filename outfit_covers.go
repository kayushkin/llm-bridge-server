package possessionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
)

// An outfit's cover is the one photo that shows the most of it, cut to the
// wearer in a tall frame for a gallery. The choice is made on every read
// from boxes already stored, with no model call: the photo where the most
// of the outfit's pieces have a box (possession_photos, which slot links
// and readings both write), then the one where those boxes span the most of
// the photo, then the earliest. The owner can pick another photo
// (outfit_cover_choices), which holds while it is one of the outfit's
// photos. The frame (coverFrame) is the union of the wearer's confirmed
// face and every piece box on the cover photo, with a margin, widened or
// heightened to coverAspect without leaving the photo.

const (
	// coverAspect is the cover's width over its height: 2:3, portrait.
	coverAspect = 2.0 / 3.0
	// coverMargin is the room left around the wearer, as a share of the
	// wearer's width and height on each side.
	coverMargin = 0.08
	// coverImageHeight is the tallest a cover image is served; a smaller
	// cut keeps its own size.
	coverImageHeight = 900
)

// Cover rules: how the cover photo was chosen.
const (
	CoverRuleMostPiecesVisible = "most_pieces_visible"
	CoverRuleOwnerChoice       = "owner_choice"
)

// OutfitCoverReason says why a photo is an outfit's cover, in counts that
// can be checked against the boxes.
type OutfitCoverReason struct {
	Rule           string  `json:"rule"`            // most_pieces_visible or owner_choice
	PiecesVisible  int     `json:"pieces_visible"`  // the outfit's pieces with a box on the cover photo
	PiecesTotal    int     `json:"pieces_total"`    // the outfit's pieces
	WearerArea     float64 `json:"wearer_area"`     // share of the photo the piece boxes span, 0 to 1
	PhotosCompared int     `json:"photos_compared"` // the outfit's photos
	ChosenBy       string  `json:"chosen_by"`       // who picked it, for owner_choice; "" otherwise
	ChosenAt       int64   `json:"chosen_at"`       // when, for owner_choice; 0 otherwise
	// IgnoredOwnerPhotoID is a photo the owner picked that is no longer one
	// of the outfit's photos, so the rule chose instead; "" when none.
	IgnoredOwnerPhotoID string `json:"ignored_owner_photo_id"`
	// WearerFaceFound says whether photo-store has the wearer's face
	// confirmed on the cover photo; without it the frame holds the pieces
	// only. Set only where the frame is worked out (cover_box present).
	WearerFaceFound *bool `json:"wearer_face_found,omitempty"`
}

// photoPieceBoxes answers, for each of the photos, the boxes of the given
// pieces on it, piece by piece.
func photoPieceBoxes(q querier, photoIDs, possessionIDs []string) (map[string]map[string]Box, error) {
	boxes := map[string]map[string]Box{}
	if len(photoIDs) == 0 || len(possessionIDs) == 0 {
		return boxes, nil
	}
	arguments := []any{}
	for _, photoID := range photoIDs {
		arguments = append(arguments, photoID)
	}
	for _, possessionID := range possessionIDs {
		arguments = append(arguments, possessionID)
	}
	rows, err := q.Query(`SELECT photo_id, possession_id, box_left, box_top, box_right, box_bottom FROM possession_photos
		WHERE photo_id IN (`+placeholders(len(photoIDs))+`) AND possession_id IN (`+placeholders(len(possessionIDs))+`) AND box_left IS NOT NULL`, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var photoID, possessionID string
		var box Box
		if err := rows.Scan(&photoID, &possessionID, &box.Left, &box.Top, &box.Right, &box.Bottom); err != nil {
			return nil, err
		}
		if boxes[photoID] == nil {
			boxes[photoID] = map[string]Box{}
		}
		boxes[photoID][possessionID] = box.clamped()
	}
	return boxes, rows.Err()
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", count), ", ")
}

// spanOf is the smallest box holding every box given.
func spanOf(boxes []Box) (Box, bool) {
	if len(boxes) == 0 {
		return Box{}, false
	}
	span := boxes[0]
	for _, box := range boxes[1:] {
		span = Box{min(span.Left, box.Left), min(span.Top, box.Top), max(span.Right, box.Right), max(span.Bottom, box.Bottom)}
	}
	return span, true
}

func boxesOf(byPiece map[string]Box) []Box {
	boxes := make([]Box, 0, len(byPiece))
	for _, box := range byPiece {
		boxes = append(boxes, box)
	}
	return boxes
}

// chooseOutfitCover fills the outfit's cover photo and reason from its
// photos, its pieces' boxes and the owner's choice. An outfit with no photo
// has no cover.
func chooseOutfitCover(q querier, outfit *Outfit) error {
	if len(outfit.PhotoIDs) == 0 {
		return nil
	}
	boxes, err := photoPieceBoxes(q, outfit.PhotoIDs, outfit.PossessionIDs)
	if err != nil {
		return err
	}
	reasonFor := func(photoID string) OutfitCoverReason {
		reason := OutfitCoverReason{PiecesVisible: len(boxes[photoID]), PiecesTotal: len(outfit.PossessionIDs), PhotosCompared: len(outfit.PhotoIDs)}
		if span, found := spanOf(boxesOf(boxes[photoID])); found {
			reason.WearerArea = math.Round(span.area()*1000) / 1000
		}
		return reason
	}
	var ownerPhotoID, chosenBy string
	var chosenAt int64
	err = q.QueryRow(`SELECT photo_id, chosen_by, chosen_at FROM outfit_cover_choices WHERE outfit_id = ?`, outfit.ID).Scan(&ownerPhotoID, &chosenBy, &chosenAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if ownerPhotoID != "" && contains(outfit.PhotoIDs, ownerPhotoID) {
		reason := reasonFor(ownerPhotoID)
		reason.Rule, reason.ChosenBy, reason.ChosenAt = CoverRuleOwnerChoice, chosenBy, chosenAt
		outfit.CoverPhotoID, outfit.CoverReason = &ownerPhotoID, &reason
		return nil
	}
	best := outfit.PhotoIDs[0]
	bestReason := reasonFor(best)
	for _, photoID := range outfit.PhotoIDs[1:] {
		reason := reasonFor(photoID)
		if reason.PiecesVisible > bestReason.PiecesVisible ||
			(reason.PiecesVisible == bestReason.PiecesVisible && reason.WearerArea > bestReason.WearerArea) {
			best, bestReason = photoID, reason
		}
	}
	bestReason.Rule = CoverRuleMostPiecesVisible
	bestReason.IgnoredOwnerPhotoID = ownerPhotoID
	outfit.CoverPhotoID, outfit.CoverReason = &best, &bestReason
	return nil
}

// SetOutfitCover records the owner's pick of an outfit's cover. An empty
// photo id takes the pick away, so the rule chooses again. The photo must
// be one of the outfit's.
func (s *Store) SetOutfitCover(ctx context.Context, outfitID, photoID, actor string) (Outfit, error) {
	if actor == "" {
		return Outfit{}, fmt.Errorf("%w: say who is writing (the actor)", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Outfit{}, err
	}
	defer transaction.Rollback()
	outfit, err := outfitByID(transaction, outfitID, false)
	if err != nil {
		return Outfit{}, err
	}
	if photoID != "" && !contains(outfit.PhotoIDs, photoID) {
		return Outfit{}, fmt.Errorf("%w: %s is not one of %s's photos (%s)", ErrInvalid, photoID, outfitID, strings.Join(outfit.PhotoIDs, ", "))
	}
	now := s.unixNow()
	var existing int
	if err := transaction.QueryRow(`SELECT COUNT(*) FROM outfit_cover_choices WHERE outfit_id = ?`, outfitID).Scan(&existing); err != nil {
		return Outfit{}, err
	}
	if existing > 0 {
		if err := saveRevision(transaction, "outfit_cover_choices", "outfit_id", outfitID, actor, now); err != nil {
			return Outfit{}, err
		}
		if _, err := transaction.Exec(`DELETE FROM outfit_cover_choices WHERE outfit_id = ?`, outfitID); err != nil {
			return Outfit{}, err
		}
	}
	if photoID != "" {
		if _, err := transaction.Exec(`INSERT INTO outfit_cover_choices (outfit_id, photo_id, chosen_by, chosen_at) VALUES (?, ?, ?, ?)`, outfitID, photoID, actor, now); err != nil {
			return Outfit{}, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return Outfit{}, err
	}
	return s.GetOutfit(outfitID, false)
}

// coverFrame cuts a photo to the wearer: the union of the face (when known)
// and the piece boxes, with coverMargin around it, made coverAspect wide
// over tall in pixels and kept inside the photo. photoAspect is the photo's
// width over its height. With neither a face nor a box the frame is the
// tallest coverAspect cut from the middle of the photo. When the wearer is
// wider or taller than the photo allows at coverAspect, the frame is the
// largest that fits, centered on the wearer, and cuts its sides.
func coverFrame(face *Box, pieces []Box, photoAspect float64) Box {
	all := append([]Box{}, pieces...)
	if face != nil {
		all = append(all, *face)
	}
	wearer, found := spanOf(all)
	if !found {
		wearer = Box{0.5, 0.5, 0.5, 0.5}
	}
	// Work in units where the photo is photoAspect wide and 1 tall, so a
	// unit is the same length across and down.
	width := (wearer.Right - wearer.Left) * photoAspect
	height := wearer.Bottom - wearer.Top
	width += 2 * coverMargin * width
	height += 2 * coverMargin * height
	centerX := (wearer.Left + wearer.Right) / 2 * photoAspect
	centerY := (wearer.Top + wearer.Bottom) / 2
	if width/max(height, 1e-9) > coverAspect {
		height = width / coverAspect
	} else {
		width = height * coverAspect
	}
	if height > 1 {
		width, height = width/height, 1
	}
	if width > photoAspect {
		width, height = photoAspect, height*photoAspect/width
	}
	if !found {
		width, height = min(coverAspect, photoAspect), min(coverAspect, photoAspect)/coverAspect
	}
	left := min(max(centerX-width/2, 0), photoAspect-width)
	top := min(max(centerY-height/2, 0), 1-height)
	return Box{left / photoAspect, top, (left + width) / photoAspect, top + height}.clamped()
}

// outfitCovers caches what a cover's frame costs to work out: each photo's
// shape (it never changes) and each wearer's face on a photo (for a while,
// since a face can be confirmed later). The cut images are kept on disk
// (coverImagePath). renderSlots bounds how many full-size photos are
// decoded at once, since a gallery asks for every cover together.
type outfitCovers struct {
	mutex       sync.Mutex
	shapes      map[string]float64
	faces       map[string]cachedFace
	renderSlots chan struct{}
	warming     bool // a goroutine is cutting warmQueue
	warmQueue   []coverCut
}

type cachedFace struct {
	box    *Box // nil: photo-store has no confirmed face of the wearer there
	readAt time.Time
}

const (
	coverFaceReadEvery = 10 * time.Minute
	coverImagesKept    = 400
	coverRendersAtOnce = 2
)

func (reviewer *Reviewer) covers() *outfitCovers {
	reviewer.coversOnce.Do(func() {
		reviewer.coverCache = &outfitCovers{shapes: map[string]float64{}, faces: map[string]cachedFace{},
			renderSlots: make(chan struct{}, coverRendersAtOnce)}
	})
	return reviewer.coverCache
}

// photoAspect answers a photo's width over its height, read from
// photo-store's thumbnail, which stands as every box was read.
func (reviewer *Reviewer) photoAspect(ctx context.Context, photoID string) (float64, error) {
	cache := reviewer.covers()
	cache.mutex.Lock()
	aspect, known := cache.shapes[photoID]
	cache.mutex.Unlock()
	if known {
		return aspect, nil
	}
	thumbnail, err := reviewer.Photos.Thumbnail(ctx, photoID)
	if err != nil {
		return 0, err
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(thumbnail))
	if err != nil {
		return 0, fmt.Errorf("read the size of %s's thumbnail: %w", photoID, err)
	}
	if config.Width < 1 || config.Height < 1 {
		return 0, fmt.Errorf("%s's thumbnail is %dx%d", photoID, config.Width, config.Height)
	}
	aspect = float64(config.Width) / float64(config.Height)
	cache.mutex.Lock()
	cache.shapes[photoID] = aspect
	cache.mutex.Unlock()
	return aspect, nil
}

// wearerFaceOn answers the wearer's confirmed face on a photo, or nil when
// photo-store has none confirmed (ErrInvalid from WearerFace). Any other
// error is returned.
func (reviewer *Reviewer) wearerFaceOn(ctx context.Context, photoID, personID string) (*Box, error) {
	if personID == "" {
		return nil, nil
	}
	cache := reviewer.covers()
	key := photoID + "|" + personID
	cache.mutex.Lock()
	cached, known := cache.faces[key]
	cache.mutex.Unlock()
	if known && time.Since(cached.readAt) < coverFaceReadEvery {
		return cached.box, nil
	}
	face, err := reviewer.Photos.WearerFace(ctx, photoID, personID)
	var box *Box
	switch {
	case errors.Is(err, ErrInvalid):
	case err != nil:
		return nil, err
	default:
		box = &face
	}
	cache.mutex.Lock()
	cache.faces[key] = cachedFace{box: box, readAt: time.Now()}
	cache.mutex.Unlock()
	return box, nil
}

// FillOutfitCoverBox works out the frame of an outfit's cover and sets
// cover_box and the reason's wearer_face_found. An outfit with no cover is
// left as it is.
func (reviewer *Reviewer) FillOutfitCoverBox(ctx context.Context, outfit *Outfit) error {
	if outfit.CoverPhotoID == nil {
		return nil
	}
	photoID := *outfit.CoverPhotoID
	var personID string
	err := reviewer.Store.db.QueryRow(`SELECT wearer_person_id FROM photo_reviews WHERE photo_id = ?`, photoID).Scan(&personID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	face, err := reviewer.wearerFaceOn(ctx, photoID, personID)
	if err != nil {
		return fmt.Errorf("the wearer's face on %s: %w", photoID, err)
	}
	aspect, err := reviewer.photoAspect(ctx, photoID)
	if err != nil {
		return err
	}
	boxes, err := photoPieceBoxes(reviewer.Store.db, []string{photoID}, outfit.PossessionIDs)
	if err != nil {
		return err
	}
	frame := coverFrame(face, boxesOf(boxes[photoID]), aspect)
	rounded := Box{roundTo(frame.Left), roundTo(frame.Top), roundTo(frame.Right), roundTo(frame.Bottom)}
	outfit.CoverBox = &rounded
	faceFound := face != nil
	outfit.CoverReason.WearerFaceFound = &faceFound
	return nil
}

func roundTo(value float64) float64 { return math.Round(value*10000) / 10000 }

// FillOutfitCoverBoxes fills every outfit's cover frame.
func (reviewer *Reviewer) FillOutfitCoverBoxes(ctx context.Context, outfits []Outfit) error {
	for index := range outfits {
		if err := reviewer.FillOutfitCoverBox(ctx, &outfits[index]); err != nil {
			return fmt.Errorf("%s: %w", outfits[index].ID, err)
		}
	}
	return nil
}

// OutfitCoverImage answers the outfit's cover cut to its frame, as a JPEG
// at most coverImageHeight tall: from the original when originals/ holds
// it, otherwise from photo-store's thumbnail. It is worked out on each read
// (and kept by photo and frame), so a change to the outfit's pieces or
// photos shows at once.
func (reviewer *Reviewer) OutfitCoverImage(ctx context.Context, outfitID string) ([]byte, error) {
	outfit, err := reviewer.Store.GetOutfit(outfitID, false)
	if err != nil {
		return nil, err
	}
	if outfit.CoverPhotoID == nil {
		return nil, fmt.Errorf("%w: %s has no photo", ErrNotFound, outfitID)
	}
	if err := reviewer.FillOutfitCoverBox(ctx, &outfit); err != nil {
		return nil, err
	}
	return reviewer.coverImage(ctx, *outfit.CoverPhotoID, *outfit.CoverBox)
}

// coverImagePath is where the cut of one photo to one frame is kept:
// cutting a full-size original takes about a second, and a gallery asks for
// every cover at once. A changed frame is a new file.
func (reviewer *Reviewer) coverImagePath(photoID string, frame Box) string {
	key := fmt.Sprintf("%s|%.4f|%.4f|%.4f|%.4f|%d", photoID, frame.Left, frame.Top, frame.Right, frame.Bottom, coverImageHeight)
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(reviewer.Store.CoverImagesDir(), hex.EncodeToString(sum[:])+".jpg")
}

func (reviewer *Reviewer) coverImage(ctx context.Context, photoID string, frame Box) ([]byte, error) {
	path := reviewer.coverImagePath(photoID, frame)
	if kept, err := os.ReadFile(path); err == nil {
		return kept, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	cache := reviewer.covers()
	select {
	case cache.renderSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-cache.renderSlots }()
	// Another request may have cut it while this one waited.
	if kept, err := os.ReadFile(path); err == nil {
		return kept, nil
	}
	picture, _, err := reviewer.Photos.DetailImage(ctx, photoID)
	if err != nil {
		return nil, err
	}
	encoded, err := cutCover(picture, frame)
	if err != nil {
		return nil, fmt.Errorf("cut %s to the cover frame: %w", photoID, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	temporary := path + ".writing"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(temporary, path); err != nil {
		return nil, err
	}
	return encoded, nil
}

// WarmOutfitCovers cuts, in the background, every listed outfit's cover
// not cut yet, so a gallery's images are ready soon after the list is
// read. Outfits listed while a warm runs join its queue.
func (reviewer *Reviewer) WarmOutfitCovers(outfits []Outfit) {
	cache := reviewer.covers()
	cache.mutex.Lock()
	for _, outfit := range outfits {
		if outfit.CoverPhotoID != nil && outfit.CoverBox != nil {
			cache.warmQueue = append(cache.warmQueue, coverCut{photoID: *outfit.CoverPhotoID, frame: *outfit.CoverBox, outfitID: outfit.ID})
		}
	}
	start := !cache.warming && len(cache.warmQueue) > 0
	cache.warming = cache.warming || start
	cache.mutex.Unlock()
	if !start {
		return
	}
	go func() {
		for {
			cache.mutex.Lock()
			if len(cache.warmQueue) == 0 {
				cache.warming = false
				cache.mutex.Unlock()
				return
			}
			next := cache.warmQueue[0]
			cache.warmQueue = cache.warmQueue[1:]
			cache.mutex.Unlock()
			if _, err := reviewer.coverImage(context.Background(), next.photoID, next.frame); err != nil {
				log.Printf("warm %s's cover: %v", next.outfitID, err)
			}
		}
	}()
}

// coverCut is one cover waiting to be cut.
type coverCut struct {
	photoID  string
	frame    Box
	outfitID string
}

// cutCover cuts a frame out of a picture and scales it to at most
// coverImageHeight tall.
func cutCover(picture image.Image, frame Box) ([]byte, error) {
	bounds := picture.Bounds()
	region := image.Rect(
		bounds.Min.X+int(math.Round(frame.Left*float64(bounds.Dx()))), bounds.Min.Y+int(math.Round(frame.Top*float64(bounds.Dy()))),
		bounds.Min.X+int(math.Round(frame.Right*float64(bounds.Dx()))), bounds.Min.Y+int(math.Round(frame.Bottom*float64(bounds.Dy())))).Intersect(bounds)
	if region.Dx() < 1 || region.Dy() < 1 {
		return nil, fmt.Errorf("the frame is smaller than a pixel")
	}
	width, height := region.Dx(), region.Dy()
	if height > coverImageHeight {
		width, height = max(width*coverImageHeight/height, 1), coverImageHeight
	}
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(out, out.Bounds(), picture, region, draw.Src, nil)
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, out, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}
