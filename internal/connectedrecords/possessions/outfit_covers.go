package possessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	"golang.org/x/image/draw"
)

// An outfit's cover is the one photo that shows the most of it, cut to the
// wearer in a tall frame for a gallery. The choice is made on every read
// from boxes already stored, with no model call: the photo where the most
// of the outfit's pieces have a box (possession_photos, which slot links
// and readings both write), then the one where those boxes span the most of
// the photo, then the earliest. The owner can pick another photo
// (outfit_cover_choices), which holds while it is one of the outfit's
// photos. The frame is the wearer's outfit: the first reader is asked once
// per cover photo for a tight box around the wearer, from the top of the
// head to the lowest clothing or shoe that shows (outfit_cover_wearer_boxes),
// and the cover is that box with a small margin, in its own shape between
// coverNarrowest and coverWidest (coverFrameAroundWearer). Until that box is
// read, the frame is the union of the wearer's confirmed face and every
// piece box, with a wider margin (coverFrameAroundPieces), and the reason
// says so.

const (
	// coverNarrowest and coverWidest bound a cover's width over its height:
	// a frame keeps the wearer's own shape between 1:2.4 and 3:4, and is
	// widened or heightened only to stay in that range.
	coverNarrowest = 1 / 2.4
	coverWidest    = 3.0 / 4.0
	// coverMiddleAspect is the shape of the cut from the middle of a photo
	// with neither a face nor a box.
	coverMiddleAspect = 2.0 / 3.0
	// coverWearerMargin is the room left around the wearer's box, as a
	// share of its height, on every side.
	coverWearerMargin = 0.03
	// coverMargin is the room left around the face and piece boxes, as a
	// share of their span's width and height on each side: model piece
	// boxes are loose and miss what is between them.
	coverMargin = 0.08
	// coverImageHeight is the tallest a cover image is served; a smaller
	// cut keeps its own size.
	coverImageHeight = 900
)

// What a cover's frame was worked out from.
const (
	CoverFrameFromWearerBox  = "wearer_box"  // the reader's box around the wearer
	CoverFrameFromPieceBoxes = "piece_boxes" // the face and piece boxes, while the wearer box is not read yet
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
	// FrameFrom says what the frame was cut to: CoverFrameFromWearerBox or
	// CoverFrameFromPieceBoxes. Set with cover_box.
	FrameFrom string `json:"frame_from,omitempty"`
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
	if ownerPhotoID != "" && slices.Contains(outfit.PhotoIDs, ownerPhotoID) {
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
	if photoID != "" && !slices.Contains(outfit.PhotoIDs, photoID) {
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

// coverFrameAroundWearer cuts a photo to the reader's box around the
// wearer, with coverWearerMargin of its height on every side, in its own
// shape kept between coverNarrowest and coverWidest, inside the photo.
// photoAspect is the photo's width over its height.
func coverFrameAroundWearer(wearer Box, photoAspect float64) Box {
	margin := coverWearerMargin * (wearer.Bottom - wearer.Top)
	return shapedFrame(wearer, margin, margin, photoAspect)
}

// coverFrameAroundPieces cuts a photo to the union of the face (when known)
// and the piece boxes, with coverMargin of the union's width and height on
// each side, shaped as coverFrameAroundWearer. With neither a face nor a
// box the frame is the tallest coverMiddleAspect cut from the middle.
func coverFrameAroundPieces(face *Box, pieces []Box, photoAspect float64) Box {
	all := append([]Box{}, pieces...)
	if face != nil {
		all = append(all, *face)
	}
	span, found := spanOf(all)
	if !found {
		width := min(coverMiddleAspect, photoAspect)
		height := width / coverMiddleAspect
		left, top := (photoAspect-width)/2, (1-height)/2
		return Box{left / photoAspect, top, (left + width) / photoAspect, top + height}.clamped()
	}
	return shapedFrame(span, coverMargin*(span.Right-span.Left)*photoAspect, coverMargin*(span.Bottom-span.Top), photoAspect)
}

// shapedFrame grows a box by the margins (in units of the photo's height,
// across and down), widens or heightens it about its center only as far as
// the shape range needs, and keeps it inside the photo. When the box is
// taller or wider than the photo allows in that range, the frame is the
// largest that fits, centered on the box, and cuts its sides.
func shapedFrame(span Box, marginAcross, marginDown, photoAspect float64) Box {
	// Work in units where the photo is photoAspect wide and 1 tall, so a
	// unit is the same length across and down.
	width := (span.Right-span.Left)*photoAspect + 2*marginAcross
	height := span.Bottom - span.Top + 2*marginDown
	centerX := (span.Left + span.Right) / 2 * photoAspect
	centerY := (span.Top + span.Bottom) / 2
	height = max(height, 1e-6)
	if width < height*coverNarrowest {
		width = height * coverNarrowest
	} else if width > height*coverWidest {
		height = width / coverWidest
	}
	if height > 1 {
		height = 1
		width = min(width, coverWidest)
	}
	if width > photoAspect {
		width = photoAspect
		height = min(height, width/coverNarrowest)
	}
	left := min(max(centerX-width/2, 0), photoAspect-width)
	top := min(max(centerY-height/2, 0), 1-height)
	return Box{left / photoAspect, top, (left + width) / photoAspect, top + height}.clamped()
}

// frameAspect is a frame's width over its height in pixels.
func frameAspect(frame Box, photoAspect float64) float64 {
	return (frame.Right - frame.Left) * photoAspect / max(frame.Bottom-frame.Top, 1e-9)
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
	// The background reads of wearer boxes (QueueCoverWearerReads).
	wearerReading  bool
	wearerQueue    []coverWearerRead
	wearerQueued   map[string]bool
	wearerFailedAt map[string]time.Time
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

func newOutfitCovers() *outfitCovers {
	return &outfitCovers{shapes: map[string]float64{}, faces: map[string]cachedFace{},
		wearerQueued: map[string]bool{}, wearerFailedAt: map[string]time.Time{},
		renderSlots: make(chan struct{}, coverRendersAtOnce)}
}

func (reviewer *Reviewer) covers() *outfitCovers { return reviewer.sharedState().covers }

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

// wearerOnPhoto answers the person an outfit reading of the photo says is
// the wearer, "" when no review names one.
func (reviewer *Reviewer) wearerOnPhoto(photoID string) (string, error) {
	var personID string
	err := reviewer.Store.db.QueryRow(`SELECT wearer_person_id FROM photo_reviews WHERE photo_id = ?`, photoID).Scan(&personID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return personID, nil
}

// storedWearerBox answers the reader's box around the wearer on a photo,
// or nil when it has not been read.
func (s *Store) storedWearerBox(photoID, personID string) (*Box, error) {
	var box Box
	err := s.db.QueryRow(`SELECT box_left, box_top, box_right, box_bottom FROM outfit_cover_wearer_boxes WHERE photo_id = ? AND person_id = ?`, photoID, personID).Scan(
		&box.Left, &box.Top, &box.Right, &box.Bottom)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &box, nil
}

// FillOutfitCoverBox works out the frame of an outfit's cover and sets
// cover_box, cover_aspect and the reason's wearer_face_found and
// frame_from. An outfit with no cover is left as it is.
func (reviewer *Reviewer) FillOutfitCoverBox(ctx context.Context, outfit *Outfit) error {
	if outfit.CoverPhotoID == nil {
		return nil
	}
	photoID := *outfit.CoverPhotoID
	personID, err := reviewer.wearerOnPhoto(photoID)
	if err != nil {
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
	wearer, err := reviewer.Store.storedWearerBox(photoID, personID)
	if err != nil {
		return err
	}
	var frame Box
	if wearer != nil {
		frame = coverFrameAroundWearer(*wearer, aspect)
		outfit.CoverReason.FrameFrom = CoverFrameFromWearerBox
	} else {
		boxes, err := photoPieceBoxes(reviewer.Store.db, []string{photoID}, outfit.PossessionIDs)
		if err != nil {
			return err
		}
		frame = coverFrameAroundPieces(face, boxesOf(boxes[photoID]), aspect)
		outfit.CoverReason.FrameFrom = CoverFrameFromPieceBoxes
	}
	rounded := Box{roundTo(frame.Left), roundTo(frame.Top), roundTo(frame.Right), roundTo(frame.Bottom)}
	outfit.CoverBox = &rounded
	shape := math.Round(frameAspect(rounded, aspect)*1000) / 1000
	outfit.CoverAspect = &shape
	faceFound := face != nil
	outfit.CoverReason.WearerFaceFound = &faceFound
	return nil
}

// coverWearerSchema is the answer the reader gives about the wearer.
var coverWearerSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["wearer_found","box"],
"properties":{"wearer_found":{"type":"boolean"},
"box":{"type":"object","additionalProperties":false,"required":["left","top","right","bottom"],
"properties":{"left":{"type":"number"},"top":{"type":"number"},"right":{"type":"number"},"bottom":{"type":"number"}}}}}`)

func coverWearerPrompt(face *Box) string {
	who := "The owner is the person whose photo this is: the one taking the selfie or standing in the mirror, or the only person, or the largest one in front."
	if face != nil {
		who = fmt.Sprintf("The owner's face is in the box left %.3f, top %.3f, right %.3f, bottom %.3f.", face.Left, face.Top, face.Right, face.Bottom)
	}
	return who + ` This photo will be cut to show the owner's outfit, so give one tight box around the owner as they appear in it:
- top: the top of their head, or of their hat;
- bottom: the lowest point of their clothing or shoes that can be seen (the bottom edge of the photo when they run past it);
- left and right: the outermost points of their body, clothes and what they carry, arms included.
Leave out other people, furniture, mirrors, doorways and the background. Do not add any room around the box; the cut adds its own.
Sides are fractions of the photo, 0 to 1, measured from its top left. Set wearer_found to false, with any box, only when the owner cannot be seen at all.`
}

// readCoverWearerBox asks the first reader for the box around the wearer
// on a photo and stores it, with what it cost. readBy says who asked.
func (reviewer *Reviewer) readCoverWearerBox(ctx context.Context, photoID, readBy string) (Box, ModelAnswer, error) {
	personID, err := reviewer.wearerOnPhoto(photoID)
	if err != nil {
		return Box{}, ModelAnswer{}, err
	}
	face, err := reviewer.wearerFaceOn(ctx, photoID, personID)
	if err != nil {
		return Box{}, ModelAnswer{}, fmt.Errorf("the wearer's face on %s: %w", photoID, err)
	}
	photo, _, err := reviewer.Photos.ReviewImage(ctx, photoID)
	if err != nil {
		return Box{}, ModelAnswer{}, err
	}
	answer, err := reviewer.Models.Ask(ctx, reviewer.FirstReaderRole, "You find people in photos and give exact boxes around them.", coverWearerPrompt(face),
		coverWearerSchema, []msg.OneShotImage{{MediaType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(photo)}}, 4000)
	if err != nil {
		return Box{}, ModelAnswer{}, fmt.Errorf("reader %s on %s: %w", reviewer.FirstReaderRole, photoID, err)
	}
	var reading struct {
		WearerFound bool `json:"wearer_found"`
		Box         Box  `json:"box"`
	}
	if err := json.Unmarshal(answer.Parsed, &reading); err != nil {
		return Box{}, answer, fmt.Errorf("%s's answer about %s is not a wearer box: %w", answer.Model, photoID, err)
	}
	box := reading.Box.clamped()
	if !reading.WearerFound {
		return Box{}, answer, fmt.Errorf("%w: %s found no wearer on %s", ErrInvalid, answer.Model, photoID)
	}
	if !box.valid() {
		return Box{}, answer, fmt.Errorf("%w: %s gave %s a box with no area: %+v", ErrInvalid, answer.Model, photoID, reading.Box)
	}
	store := reviewer.Store
	store.writeMutex.Lock()
	defer store.writeMutex.Unlock()
	if _, err := store.db.Exec(`INSERT INTO outfit_cover_wearer_boxes (photo_id, person_id, box_left, box_top, box_right, box_bottom, face_given, model, input_tokens, output_tokens, cost_usd, read_by, read_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner_principal_id, photo_id, person_id) DO UPDATE SET box_left = excluded.box_left, box_top = excluded.box_top, box_right = excluded.box_right,
			box_bottom = excluded.box_bottom, face_given = excluded.face_given, model = excluded.model, input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens, cost_usd = excluded.cost_usd, read_by = excluded.read_by, read_at = excluded.read_at`, photoID, personID, box.Left, box.Top, box.Right, box.Bottom, face != nil, answer.Model,
		answer.InputTokens, answer.OutputTokens, answer.CostUSD, readBy, store.unixNow()); err != nil {
		return Box{}, answer, err
	}
	return box, answer, nil
}

// EnsureCoverWearerBox reads the box around the wearer on the outfit's
// cover photo when it has not been read, so a cover the owner just picked
// is cut tight at once.
func (reviewer *Reviewer) EnsureCoverWearerBox(ctx context.Context, outfit Outfit, actor string) error {
	if outfit.CoverPhotoID == nil {
		return nil
	}
	personID, err := reviewer.wearerOnPhoto(*outfit.CoverPhotoID)
	if err != nil {
		return err
	}
	stored, err := reviewer.Store.storedWearerBox(*outfit.CoverPhotoID, personID)
	if err != nil || stored != nil {
		return err
	}
	_, _, err = reviewer.readCoverWearerBox(ctx, *outfit.CoverPhotoID, actor)
	return err
}

// CoverWearerReading is what POST /outfit-covers/read-wearer-boxes did.
type CoverWearerReading struct {
	CoverPhotos int                  `json:"cover_photos"` // distinct cover photos of live outfits
	AlreadyRead int                  `json:"already_read"`
	Read        int                  `json:"read"`
	Failed      []CoverWearerFailure `json:"failed"`
	CostUSD     float64              `json:"cost_usd"`
}

// CoverWearerFailure is a cover photo whose wearer box could not be read.
type CoverWearerFailure struct {
	PhotoID string `json:"photo_id"`
	Why     string `json:"why"`
}

// coverWearerReadsAtOnce is how many wearer boxes are read together.
const coverWearerReadsAtOnce = 4

// ReadMissingCoverWearerBoxes reads the wearer box of every live outfit's
// cover photo that has none, coverWearerReadsAtOnce at a time. A photo the
// reader fails on is named in failed with why; the rest go on.
func (reviewer *Reviewer) ReadMissingCoverWearerBoxes(ctx context.Context, actor string) (CoverWearerReading, error) {
	if actor == "" {
		return CoverWearerReading{}, fmt.Errorf("%w: say who is asking (the actor)", ErrInvalid)
	}
	outfits, err := reviewer.Store.FindOutfits(OutfitFilter{Limit: math.MaxInt32})
	if err != nil {
		return CoverWearerReading{}, err
	}
	result := CoverWearerReading{Failed: []CoverWearerFailure{}}
	seen := map[string]bool{}
	missing := []string{}
	for _, outfit := range outfits {
		if outfit.CoverPhotoID == nil || seen[*outfit.CoverPhotoID] {
			continue
		}
		photoID := *outfit.CoverPhotoID
		seen[photoID] = true
		result.CoverPhotos++
		personID, err := reviewer.wearerOnPhoto(photoID)
		if err != nil {
			return result, err
		}
		stored, err := reviewer.Store.storedWearerBox(photoID, personID)
		if err != nil {
			return result, err
		}
		if stored != nil {
			result.AlreadyRead++
			continue
		}
		missing = append(missing, photoID)
	}
	var mutex sync.Mutex
	var group sync.WaitGroup
	slots := make(chan struct{}, coverWearerReadsAtOnce)
	for _, photoID := range missing {
		group.Add(1)
		slots <- struct{}{}
		go func(photoID string) {
			defer group.Done()
			defer func() { <-slots }()
			_, answer, err := reviewer.readCoverWearerBox(ctx, photoID, actor)
			mutex.Lock()
			defer mutex.Unlock()
			result.CostUSD += answer.CostUSD
			if err != nil {
				result.Failed = append(result.Failed, CoverWearerFailure{PhotoID: photoID, Why: err.Error()})
				return
			}
			result.Read++
		}(photoID)
	}
	group.Wait()
	result.CostUSD = math.Round(result.CostUSD*10000) / 10000
	return result, ctx.Err()
}

// QueueCoverWearerReads reads, in the background and one at a time, the
// wearer box of every listed outfit whose cover is still cut to its piece
// boxes, so a cover that changed (a rebuild, a combine) is cut tight soon
// after the gallery lists it. Only for owners ReadCoverWearersInBackgroundFor
// names. A
// photo that failed is not tried again for coverWearerRetryAfter; failures
// are logged.
func (reviewer *Reviewer) QueueCoverWearerReads(outfits []Outfit) {
	if reviewer.ReadCoverWearersInBackgroundFor == nil {
		return
	}
	cache := reviewer.covers()
	cache.mutex.Lock()
	for _, outfit := range outfits {
		if outfit.CoverPhotoID == nil || outfit.CoverReason == nil || outfit.CoverReason.FrameFrom != CoverFrameFromPieceBoxes ||
			!reviewer.ReadCoverWearersInBackgroundFor(outfit.OwnerPrincipalID) {
			continue
		}
		photoID := *outfit.CoverPhotoID
		if cache.wearerQueued[photoID] || time.Since(cache.wearerFailedAt[photoID]) < coverWearerRetryAfter {
			continue
		}
		cache.wearerQueued[photoID] = true
		cache.wearerQueue = append(cache.wearerQueue, coverWearerRead{photoID: photoID, ownerPrincipalID: outfit.OwnerPrincipalID})
	}
	start := !cache.wearerReading && len(cache.wearerQueue) > 0
	cache.wearerReading = cache.wearerReading || start
	cache.mutex.Unlock()
	if !start {
		return
	}
	go func() {
		for {
			cache.mutex.Lock()
			if len(cache.wearerQueue) == 0 {
				cache.wearerReading = false
				cache.mutex.Unlock()
				return
			}
			next := cache.wearerQueue[0]
			photoID := next.photoID
			cache.wearerQueue = cache.wearerQueue[1:]
			cache.mutex.Unlock()
			// Each read acts as the owner of the outfit listed, whoever
			// listed it: the box is that owner's record.
			ctx := reviewer.backgroundContext(next.ownerPrincipalID)
			var answer ModelAnswer
			owned, err := reviewer.NarrowedToOwner(ctx, next.ownerPrincipalID)
			if err == nil {
				_, answer, err = owned.readCoverWearerBox(ctx, photoID, coverWearerReadByList)
			}
			cache.mutex.Lock()
			delete(cache.wearerQueued, photoID)
			if err != nil {
				cache.wearerFailedAt[photoID] = time.Now()
			}
			cache.mutex.Unlock()
			if err != nil {
				log.Printf("read the wearer box on cover photo %s: %v", photoID, err)
			} else {
				log.Printf("read the wearer box on cover photo %s ($%.4f)", photoID, answer.CostUSD)
			}
		}
	}()
}

const (
	// coverWearerReadByList is read_by on a wearer box the outfit list
	// started reading.
	coverWearerReadByList = "cover"
	coverWearerRetryAfter = time.Hour
)

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
	return filepath.Join(reviewer.CoverImagesDirectory, hex.EncodeToString(sum[:])+".jpg")
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
			cache.warmQueue = append(cache.warmQueue, coverCut{photoID: *outfit.CoverPhotoID, frame: *outfit.CoverBox, outfitID: outfit.ID, ownerPrincipalID: outfit.OwnerPrincipalID})
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
			if _, err := reviewer.coverImage(reviewer.backgroundContext(next.ownerPrincipalID), next.photoID, next.frame); err != nil {
				log.Printf("warm %s's cover: %v", next.outfitID, err)
			}
		}
	}()
}

// coverCut is one cover waiting to be cut, as the outfit's owner.
type coverCut struct {
	photoID          string
	frame            Box
	outfitID         string
	ownerPrincipalID string
}

// coverWearerRead is one cover photo whose wearer box waits to be read, as
// the outfit's owner.
type coverWearerRead struct {
	photoID          string
	ownerPrincipalID string
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
