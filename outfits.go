package possessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// OutfitInput is the body of POST /outfits and PATCH /outfits/{id}. On a
// patch, a nil field is left as it is; garment_ids, when given, replaces the
// whole list.
type OutfitInput struct {
	Name             *string  `json:"name"`
	Occasion         *string  `json:"occasion"`
	Weather          *string  `json:"weather"`
	Status           *string  `json:"status"`
	Reasoning        *string  `json:"reasoning"`
	PreviewImagePath *string  `json:"preview_image_path"`
	GarmentIDs       []string `json:"garment_ids"`
}

// Outfit is an outfit as the API answers it.
type Outfit struct {
	ID               string   `json:"id"`
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
	GarmentIDs       []string `json:"garment_ids"`
	WornOn           []string `json:"worn_on"`
}

func (s *Store) checkOutfitGarments(transaction *sql.Tx, garmentIDs []string) error {
	if len(garmentIDs) == 0 {
		return fmt.Errorf("%w: an outfit needs at least one garment", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, garmentID := range garmentIDs {
		if seen[garmentID] {
			return fmt.Errorf("%w: %s is in the outfit twice", ErrInvalid, garmentID)
		}
		seen[garmentID] = true
		var deletedAt int64
		err := transaction.QueryRow(`SELECT possessions.deleted_at FROM garments JOIN possessions ON possessions.id = garments.possession_id WHERE garments.id = ?`, garmentID).Scan(&deletedAt)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && deletedAt != 0) {
			return fmt.Errorf("%w: no garment %s", ErrInvalid, garmentID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func replaceOutfitGarments(transaction *sql.Tx, outfitID string, garmentIDs []string) error {
	if _, err := transaction.Exec(`DELETE FROM outfit_garments WHERE outfit_id = ?`, outfitID); err != nil {
		return err
	}
	for position, garmentID := range garmentIDs {
		if _, err := transaction.Exec(`INSERT INTO outfit_garments (outfit_id, garment_id, position) VALUES (?, ?, ?)`, outfitID, garmentID, position); err != nil {
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
		return Outfit{}, fmt.Errorf("%w: say who is writing (the actor)", ErrInvalid)
	}
	status := valueOr(input.Status, "")
	if !contains(OutfitStatuses, status) {
		return Outfit{}, fmt.Errorf("%w: status must be one of %s", ErrInvalid, strings.Join(OutfitStatuses, ", "))
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Outfit{}, err
	}
	defer transaction.Rollback()
	if err := s.checkOutfitGarments(transaction, input.GarmentIDs); err != nil {
		return Outfit{}, err
	}
	id, sequence, err := nextID(transaction, "outfits", "outfit")
	if err != nil {
		return Outfit{}, err
	}
	now := s.unixNow()
	if _, err := transaction.Exec(`INSERT INTO outfits (id, seq, name, occasion, weather, status, reasoning, preview_image_path, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, sequence, valueOr(input.Name, ""), valueOr(input.Occasion, ""), valueOr(input.Weather, ""), status,
		valueOr(input.Reasoning, ""), valueOr(input.PreviewImagePath, ""), actor, now, now); err != nil {
		return Outfit{}, err
	}
	if err := replaceOutfitGarments(transaction, id, input.GarmentIDs); err != nil {
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
		return Outfit{}, fmt.Errorf("%w: say who is writing (the actor)", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Outfit{}, err
	}
	defer transaction.Rollback()
	existing, err := outfitByID(transaction, id, false)
	if err != nil {
		return Outfit{}, err
	}
	status := valueOr(input.Status, existing.Status)
	if !contains(OutfitStatuses, status) {
		return Outfit{}, fmt.Errorf("%w: status must be one of %s", ErrInvalid, strings.Join(OutfitStatuses, ", "))
	}
	now := s.unixNow()
	if err := saveRevision(transaction, "outfits", "id", id, actor, now); err != nil {
		return Outfit{}, err
	}
	if _, err := transaction.Exec(`UPDATE outfits SET name = ?, occasion = ?, weather = ?, status = ?, reasoning = ?, preview_image_path = ?, updated_at = ? WHERE id = ?`,
		valueOr(input.Name, existing.Name), valueOr(input.Occasion, existing.Occasion), valueOr(input.Weather, existing.Weather), status,
		valueOr(input.Reasoning, existing.Reasoning), valueOr(input.PreviewImagePath, existing.PreviewImagePath), now, id); err != nil {
		return Outfit{}, err
	}
	if input.GarmentIDs != nil {
		if err := s.checkOutfitGarments(transaction, input.GarmentIDs); err != nil {
			return Outfit{}, err
		}
		if err := replaceOutfitGarments(transaction, id, input.GarmentIDs); err != nil {
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
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if _, err := outfitByID(transaction, id, false); err != nil {
		return err
	}
	now := s.unixNow()
	if err := saveRevision(transaction, "outfits", "id", id, actor, now); err != nil {
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
		return Outfit{}, fmt.Errorf("%w: worn_on is YYYY-MM-DD", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	if _, err := outfitByID(s.db, id, false); err != nil {
		return Outfit{}, err
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO outfit_wearings (outfit_id, worn_on) VALUES (?, ?)`, id, wornOn); err != nil {
		return Outfit{}, err
	}
	return outfitByID(s.db, id, false)
}

// GetOutfit answers one outfit.
func (s *Store) GetOutfit(id string, includeDeleted bool) (Outfit, error) {
	return outfitByID(s.db, id, includeDeleted)
}

func outfitByID(q querier, id string, includeDeleted bool) (Outfit, error) {
	var outfit Outfit
	err := q.QueryRow(`SELECT id, name, occasion, weather, status, reasoning, preview_image_path, created_by, created_at, updated_at, deleted_at FROM outfits WHERE id = ?`, id).Scan(
		&outfit.ID, &outfit.Name, &outfit.Occasion, &outfit.Weather, &outfit.Status, &outfit.Reasoning, &outfit.PreviewImagePath, &outfit.CreatedBy, &outfit.CreatedAt, &outfit.UpdatedAt, &outfit.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && outfit.DeletedAt != 0 && !includeDeleted) {
		return Outfit{}, fmt.Errorf("%w: no outfit %s", ErrNotFound, id)
	}
	if err != nil {
		return Outfit{}, err
	}
	if outfit.GarmentIDs, err = stringColumn(q, `SELECT garment_id FROM outfit_garments WHERE outfit_id = ? ORDER BY position`, id); err != nil {
		return Outfit{}, err
	}
	if outfit.WornOn, err = stringColumn(q, `SELECT worn_on FROM outfit_wearings WHERE outfit_id = ? ORDER BY worn_on`, id); err != nil {
		return Outfit{}, err
	}
	return outfit, nil
}

// ListOutfits answers outfits, newest first, optionally only one status or
// only those holding one garment.
func (s *Store) ListOutfits(status, garmentID string, limit, offset int) ([]Outfit, error) {
	conditions := []string{"deleted_at = 0"}
	arguments := []any{}
	if status != "" {
		if !contains(OutfitStatuses, status) {
			return nil, fmt.Errorf("%w: status must be one of %s", ErrInvalid, strings.Join(OutfitStatuses, ", "))
		}
		conditions = append(conditions, "status = ?")
		arguments = append(arguments, status)
	}
	if garmentID != "" {
		conditions = append(conditions, "id IN (SELECT outfit_id FROM outfit_garments WHERE garment_id = ?)")
		arguments = append(arguments, garmentID)
	}
	if limit <= 0 {
		limit = 100
	}
	arguments = append(arguments, limit, offset)
	ids, err := stringColumn(s.db, `SELECT id FROM outfits WHERE `+strings.Join(conditions, " AND ")+` ORDER BY seq DESC LIMIT ? OFFSET ?`, arguments...)
	if err != nil {
		return nil, err
	}
	outfits := make([]Outfit, 0, len(ids))
	for _, id := range ids {
		outfit, err := outfitByID(s.db, id, false)
		if err != nil {
			return nil, err
		}
		outfits = append(outfits, outfit)
	}
	return outfits, nil
}
