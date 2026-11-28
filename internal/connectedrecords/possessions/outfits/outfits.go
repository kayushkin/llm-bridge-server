package outfits

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions/review"
)

// OutfitInput is the body of POST /outfits and PATCH /outfits/{id}. On a
// patch, a nil field is left as it is; possession_ids, when given, replaces
// the whole list.
type OutfitInput struct {
	Name             *string  `json:"name"`
	Occasion         *string  `json:"occasion"`
	Weather          *string  `json:"weather"`
	Status           *string  `json:"status"`
	Reasoning        *string  `json:"reasoning"`
	PreviewImagePath *string  `json:"preview_image_path"`
	PossessionIDs    []string `json:"possession_ids"`
	// Favorite marks an outfit the owner likes; tags are the owner's own
	// words for it (outfitTags tidies them). Tags, when given, replaces the
	// whole list; [] clears it.
	Favorite *bool     `json:"favorite"`
	Tags     *[]string `json:"tags"`
}

// Outfit is an outfit as the API answers it.
type Outfit struct {
	ID               string   `json:"id"`
	OwnerPrincipalID string   `json:"owner_principal_id"` // principal-store's id of who keeps it
	Name             string   `json:"name"`
	Occasion         string   `json:"occasion"`
	Weather          string   `json:"weather"`
	Status           string   `json:"status"`
	Reasoning        string   `json:"reasoning"`
	PreviewImagePath string   `json:"preview_image_path"`
	CreatedBy        string   `json:"created_by"`
	CreatedAt        int64    `json:"created_at"`
	UpdatedAt        int64    `json:"updated_at"`
	DeletedAt        int64    `json:"deleted_at"`
	PossessionIDs    []string `json:"possession_ids"`
	Favorite         bool     `json:"favorite"`
	Tags             []string `json:"tags"` // lower case, each once, in the order given
	WornOn           []string `json:"worn_on"`
	PhotoIDs         []string `json:"photo_ids"`      // photo-store photos it was read from, oldest first
	MergedIntoID     *string  `json:"merged_into_id"` // the outfit it was combined into; set only on a deleted one
	// The photo that shows the most of the outfit, and why (outfit_covers.go);
	// null with no photo. cover_box is the tall frame cut to the wearer,
	// filled by the routes that answer an outfit.
	CoverPhotoID *string            `json:"cover_photo_id"`
	CoverBox     *possessions.Box   `json:"cover_box"`
	CoverReason  *OutfitCoverReason `json:"cover_reason"`
	// CoverAspect is cover_box's width over its height in pixels, between
	// 1:2.4 and 3:4, so a gallery can lay the cover out before it loads.
	CoverAspect *float64 `json:"cover_aspect"`
}

// checkOutfitPossessions holds an outfit to live possessions of a wearable
// kind, each once.
func (s *Store) checkOutfitPossessions(transaction *possessions.DatabaseTransaction, possessionIDs []string) error {
	if len(possessionIDs) == 0 {
		return fmt.Errorf("%w: an outfit needs at least one thing in it", possessions.ErrInvalid)
	}
	seen := map[string]bool{}
	for _, possessionID := range possessionIDs {
		if seen[possessionID] {
			return fmt.Errorf("%w: %s is in the outfit twice", possessions.ErrInvalid, possessionID)
		}
		seen[possessionID] = true
		view, err := possessions.PossessionView(transaction, possessionID, false)
		if errors.Is(err, possessions.ErrNotFound) {
			return fmt.Errorf("%w: no possession %s", possessions.ErrInvalid, possessionID)
		}
		if err != nil {
			return err
		}
		if !slices.Contains(possessions.WearableKinds, view["kind"].(string)) {
			return fmt.Errorf("%w: %s is a %q; an outfit holds only %s", possessions.ErrInvalid, possessionID, view["kind"], strings.Join(possessions.WearableKinds, ", "))
		}
	}
	return nil
}

func replaceOutfitPossessions(transaction *possessions.DatabaseTransaction, outfitID string, possessionIDs []string) error {
	if _, err := transaction.Exec(`DELETE FROM outfit_possessions WHERE outfit_id = ?`, outfitID); err != nil {
		return err
	}
	for position, possessionID := range possessionIDs {
		if _, err := transaction.Exec(`INSERT INTO outfit_possessions (outfit_id, possession_id, position) VALUES (?, ?, ?)`, outfitID, possessionID, position); err != nil {
			return err
		}
	}
	return nil
}

func valueOr(pointer *string, fallback string) string {
	if pointer == nil {
		return fallback
	}
	return strings.TrimSpace(*pointer)
}

// CreateOutfit makes an outfit; status defaults to kept when a person makes
// it, and must be given otherwise.
func (s *Store) CreateOutfit(ctx context.Context, input OutfitInput, actor string) (Outfit, error) {
	if actor == "" {
		return Outfit{}, fmt.Errorf("%w: say who is writing (the actor)", possessions.ErrInvalid)
	}
	status := valueOr(input.Status, "")
	if !slices.Contains(possessions.OutfitStatuses, status) {
		return Outfit{}, fmt.Errorf("%w: status must be one of %s", possessions.ErrInvalid, strings.Join(possessions.OutfitStatuses, ", "))
	}
	s.WriteMutex.Lock()
	defer s.WriteMutex.Unlock()
	transaction, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Outfit{}, err
	}
	defer transaction.Rollback()
	if err := s.checkOutfitPossessions(transaction, input.PossessionIDs); err != nil {
		return Outfit{}, err
	}
	tags := []string{}
	if input.Tags != nil {
		if tags, err = outfitTags(*input.Tags); err != nil {
			return Outfit{}, err
		}
	}
	favorite := input.Favorite != nil && *input.Favorite
	id, sequence, err := possessions.NextID(transaction, "outfits", "outfit")
	if err != nil {
		return Outfit{}, err
	}
	now := s.UnixNow()
	if _, err := transaction.Exec(`INSERT INTO outfits (id, seq, name, occasion, weather, status, reasoning, preview_image_path, favorite, tags, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, sequence, valueOr(input.Name, ""), valueOr(input.Occasion, ""), valueOr(input.Weather, ""), status,
		valueOr(input.Reasoning, ""), valueOr(input.PreviewImagePath, ""), favorite, review.MustJSON(tags), actor, now, now); err != nil {
		return Outfit{}, err
	}
	if err := replaceOutfitPossessions(transaction, id, input.PossessionIDs); err != nil {
		return Outfit{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Outfit{}, err
	}
	return s.GetOutfit(id, false)
}

// PatchOutfit changes an outfit.
func (s *Store) PatchOutfit(ctx context.Context, id string, input OutfitInput, actor string) (Outfit, error) {
	if actor == "" {
		return Outfit{}, fmt.Errorf("%w: say who is writing (the actor)", possessions.ErrInvalid)
	}
	s.WriteMutex.Lock()
	defer s.WriteMutex.Unlock()
	transaction, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Outfit{}, err
	}
	defer transaction.Rollback()
	existing, err := outfitByID(transaction, id, false)
	if err != nil {
		return Outfit{}, err
	}
	status := valueOr(input.Status, existing.Status)
	if !slices.Contains(possessions.OutfitStatuses, status) {
		return Outfit{}, fmt.Errorf("%w: status must be one of %s", possessions.ErrInvalid, strings.Join(possessions.OutfitStatuses, ", "))
	}
	tags := existing.Tags
	if input.Tags != nil {
		if tags, err = outfitTags(*input.Tags); err != nil {
			return Outfit{}, err
		}
	}
	favorite := existing.Favorite
	if input.Favorite != nil {
		favorite = *input.Favorite
	}
	now := s.UnixNow()
	if err := possessions.SaveRevision(transaction, "outfits", "id", id, actor, now); err != nil {
		return Outfit{}, err
	}
	if _, err := transaction.Exec(`UPDATE outfits SET name = ?, occasion = ?, weather = ?, status = ?, reasoning = ?, preview_image_path = ?, favorite = ?, tags = ?, updated_at = ? WHERE id = ?`,
		valueOr(input.Name, existing.Name), valueOr(input.Occasion, existing.Occasion), valueOr(input.Weather, existing.Weather), status,
		valueOr(input.Reasoning, existing.Reasoning), valueOr(input.PreviewImagePath, existing.PreviewImagePath), favorite, review.MustJSON(tags), now, id); err != nil {
		return Outfit{}, err
	}
	if input.PossessionIDs != nil {
		if err := s.checkOutfitPossessions(transaction, input.PossessionIDs); err != nil {
			return Outfit{}, err
		}
		if err := replaceOutfitPossessions(transaction, id, input.PossessionIDs); err != nil {
			return Outfit{}, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return Outfit{}, err
	}
	return s.GetOutfit(id, false)
}

// DeleteOutfit stamps deleted_at.
func (s *Store) DeleteOutfit(id, actor string) error {
	s.WriteMutex.Lock()
	defer s.WriteMutex.Unlock()
	transaction, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if _, err := outfitByID(transaction, id, false); err != nil {
		return err
	}
	now := s.UnixNow()
	if err := possessions.SaveRevision(transaction, "outfits", "id", id, actor, now); err != nil {
		return err
	}
	if _, err := transaction.Exec(`UPDATE outfits SET deleted_at = ?, updated_at = ? WHERE id = ?`, now, now, id); err != nil {
		return err
	}
	return transaction.Commit()
}

var calendarDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// RecordWearing notes that an outfit was worn on a day (YYYY-MM-DD, the
// owner's time zone).
func (s *Store) RecordWearing(id, wornOn string) (Outfit, error) {
	if !calendarDate.MatchString(wornOn) {
		return Outfit{}, fmt.Errorf("%w: worn_on is YYYY-MM-DD", possessions.ErrInvalid)
	}
	s.WriteMutex.Lock()
	defer s.WriteMutex.Unlock()
	if _, err := outfitByID(s.DB, id, false); err != nil {
		return Outfit{}, err
	}
	if _, err := s.DB.Exec(`INSERT INTO outfit_wearings (outfit_id, worn_on) VALUES (?, ?) ON CONFLICT DO NOTHING`, id, wornOn); err != nil {
		return Outfit{}, err
	}
	return outfitByID(s.DB, id, false)
}

// GetOutfit answers one outfit.
func (s *Store) GetOutfit(id string, includeDeleted bool) (Outfit, error) {
	return outfitByID(s.DB, id, includeDeleted)
}

func outfitByID(q possessions.Querier, id string, includeDeleted bool) (Outfit, error) {
	var outfit Outfit
	var mergedInto sql.NullString
	var tags string
	err := q.QueryRow(`SELECT id, owner_principal_id, name, occasion, weather, status, reasoning, preview_image_path, favorite, tags, created_by, created_at, updated_at, deleted_at, merged_into_id FROM outfits WHERE id = ?`, id).Scan(
		&outfit.ID, &outfit.OwnerPrincipalID, &outfit.Name, &outfit.Occasion, &outfit.Weather, &outfit.Status, &outfit.Reasoning, &outfit.PreviewImagePath, &outfit.Favorite, &tags, &outfit.CreatedBy, &outfit.CreatedAt, &outfit.UpdatedAt, &outfit.DeletedAt, &mergedInto)
	if mergedInto.Valid {
		outfit.MergedIntoID = &mergedInto.String
	}
	if errors.Is(err, sql.ErrNoRows) || (err == nil && outfit.DeletedAt != 0 && !includeDeleted) {
		return Outfit{}, fmt.Errorf("%w: no outfit %s", possessions.ErrNotFound, id)
	}
	if err != nil {
		return Outfit{}, err
	}
	if err := json.Unmarshal([]byte(tags), &outfit.Tags); err != nil {
		return Outfit{}, fmt.Errorf("%s holds tags that are not a JSON list: %w", id, err)
	}
	if outfit.PossessionIDs, err = possessions.StringColumn(q, `SELECT possession_id FROM outfit_possessions WHERE outfit_id = ? ORDER BY position`, id); err != nil {
		return Outfit{}, err
	}
	if outfit.PhotoIDs, err = possessions.StringColumn(q, `SELECT photo_id FROM photo_reviews WHERE outfit_id = ? ORDER BY worn_on, seq`, id); err != nil {
		return Outfit{}, err
	}
	if outfit.WornOn, err = possessions.StringColumn(q, `SELECT worn_on FROM outfit_wearings WHERE outfit_id = ? ORDER BY worn_on`, id); err != nil {
		return Outfit{}, err
	}
	if err := chooseOutfitCover(q, &outfit); err != nil {
		return Outfit{}, fmt.Errorf("choose %s's cover: %w", id, err)
	}
	return outfit, nil
}

// OutfitFilter narrows a list of outfits; a zero field narrows nothing.
type OutfitFilter struct {
	Status       string
	PossessionID string
	FavoriteOnly bool
	Tag          string // one tag, matched after outfitTags tidies it
	Limit        int    // default 100
	Offset       int
}

// ListOutfits answers outfits, newest first, optionally only one status or
// only those holding one possession.
func (s *Store) ListOutfits(status, possessionID string, limit, offset int) ([]Outfit, error) {
	return s.FindOutfits(OutfitFilter{Status: status, PossessionID: possessionID, Limit: limit, Offset: offset})
}

// FindOutfits answers the live outfits the filter leaves, newest first.
func (s *Store) FindOutfits(filter OutfitFilter) ([]Outfit, error) {
	conditions := []string{"deleted_at = 0"}
	arguments := []any{}
	if filter.Status != "" {
		if !slices.Contains(possessions.OutfitStatuses, filter.Status) {
			return nil, fmt.Errorf("%w: status must be one of %s", possessions.ErrInvalid, strings.Join(possessions.OutfitStatuses, ", "))
		}
		conditions = append(conditions, "status = ?")
		arguments = append(arguments, filter.Status)
	}
	if filter.PossessionID != "" {
		conditions = append(conditions, "id IN (SELECT outfit_id FROM outfit_possessions WHERE possession_id = ?)")
		arguments = append(arguments, filter.PossessionID)
	}
	if filter.FavoriteOnly {
		conditions = append(conditions, "favorite = 1")
	}
	if filter.Tag != "" {
		tags, err := outfitTags([]string{filter.Tag})
		if err != nil {
			return nil, err
		}
		conditions = append(conditions, "EXISTS (SELECT 1 FROM json_array_texts(outfits.tags) AS tag WHERE tag = ?)")
		arguments = append(arguments, tags[0])
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	arguments = append(arguments, limit, filter.Offset)
	ids, err := possessions.StringColumn(s.DB, `SELECT id FROM outfits WHERE `+strings.Join(conditions, " AND ")+` ORDER BY seq DESC LIMIT ? OFFSET ?`, arguments...)
	if err != nil {
		return nil, err
	}
	outfits := make([]Outfit, 0, len(ids))
	for _, id := range ids {
		outfit, err := outfitByID(s.DB, id, false)
		if err != nil {
			return nil, err
		}
		outfits = append(outfits, outfit)
	}
	return outfits, nil
}

// outfitTagLongest is the most characters one tag may hold.
const outfitTagLongest = 40

// outfitTags tidies the owner's tags: each trimmed, its inner spaces made
// one, in lower case, and each once, in the order first given. An empty
// tag, or one longer than outfitTagLongest, is refused rather than dropped.
func outfitTags(given []string) ([]string, error) {
	tags := []string{}
	for _, tag := range given {
		tidy := strings.ToLower(strings.Join(strings.Fields(tag), " "))
		if tidy == "" {
			return nil, fmt.Errorf("%w: a tag is empty", possessions.ErrInvalid)
		}
		if len([]rune(tidy)) > outfitTagLongest {
			return nil, fmt.Errorf("%w: the tag %q is longer than %d characters", possessions.ErrInvalid, tidy, outfitTagLongest)
		}
		if !slices.Contains(tags, tidy) {
			tags = append(tags, tidy)
		}
	}
	return tags, nil
}

// OutfitTagCount is one tag and how many live outfits carry it.
type OutfitTagCount struct {
	Tag     string `json:"tag"`
	Outfits int    `json:"outfits"`
}

// OutfitTagCounts answers every tag on a live outfit, the most used first,
// then by name.
func (s *Store) OutfitTagCounts() ([]OutfitTagCount, error) {
	rows, err := s.DB.Query(`SELECT tag, COUNT(*) FROM outfits, json_array_texts(outfits.tags) AS tag WHERE outfits.deleted_at = 0
		GROUP BY tag ORDER BY COUNT(*) DESC, tag COLLATE "C"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := []OutfitTagCount{}
	for rows.Next() {
		var count OutfitTagCount
		if err := rows.Scan(&count.Tag, &count.Outfits); err != nil {
			return nil, err
		}
		counts = append(counts, count)
	}
	return counts, rows.Err()
}
