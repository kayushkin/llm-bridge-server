package possessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// querier is a *sql.DB or a *sql.Tx.
type querier interface {
	QueryRow(query string, arguments ...any) *sql.Row
	Query(query string, arguments ...any) (*sql.Rows, error)
}

// PhotoLink is one photo of a possession, and where in it the possession is.
type PhotoLink struct {
	PhotoID    string   `json:"photo_id"`
	BoxLeft    *float64 `json:"box_left"`
	BoxTop     *float64 `json:"box_top"`
	BoxRight   *float64 `json:"box_right"`
	BoxBottom  *float64 `json:"box_bottom"`
	IsCover    bool     `json:"is_cover"`
	SightingID *string  `json:"sighting_id"`
	LinkedBy   string   `json:"linked_by"`
	LinkedAt   int64    `json:"linked_at"`
}

func (link PhotoLink) validate() error {
	if link.PhotoID == "" {
		return fmt.Errorf("%w: photo_id is required", ErrInvalid)
	}
	box := []*float64{link.BoxLeft, link.BoxTop, link.BoxRight, link.BoxBottom}
	given := 0
	for _, side := range box {
		if side != nil {
			given++
			if *side < 0 || *side > 1 {
				return fmt.Errorf("%w: a box side is a fraction of the photo, 0 to 1", ErrInvalid)
			}
		}
	}
	if given != 0 && given != 4 {
		return fmt.Errorf("%w: give all four box sides or none", ErrInvalid)
	}
	if given == 4 && (*link.BoxLeft >= *link.BoxRight || *link.BoxTop >= *link.BoxBottom) {
		return fmt.Errorf("%w: a box's left must be left of its right and its top above its bottom", ErrInvalid)
	}
	return nil
}

// splitPossessionFields separates a possession request into its own columns,
// at most one kind's fields (under the kind's name) and its photos.
func splitPossessionFields(fields map[string]json.RawMessage) (own map[string]json.RawMessage, kind *RecordType, kindFields map[string]json.RawMessage, photos []PhotoLink, err error) {
	own = map[string]json.RawMessage{}
	for name, raw := range fields {
		if candidate, isKind := KindByName(name); isKind {
			if kind != nil {
				return nil, nil, nil, nil, fmt.Errorf("%w: a possession has one kind; got %s and %s", ErrInvalid, kind.Name, name)
			}
			kindCopy := candidate
			kind = &kindCopy
			if err := json.Unmarshal(raw, &kindFields); err != nil || kindFields == nil {
				return nil, nil, nil, nil, fmt.Errorf("%w: %s must be an object of its fields", ErrInvalid, name)
			}
			continue
		}
		if name == "photos" {
			if err := json.Unmarshal(raw, &photos); err != nil {
				return nil, nil, nil, nil, fmt.Errorf("%w: photos must be an array of {photo_id, box_left…, is_cover}", ErrInvalid)
			}
			continue
		}
		own[name] = raw
	}
	return own, kind, kindFields, photos, nil
}

func (s *Store) checkValues(ctx context.Context, transaction *sql.Tx, values map[string]decodedValue) error {
	for _, decoded := range values {
		if decoded.column.References == "" || decoded.value == nil {
			continue
		}
		if err := s.checkReference(ctx, transaction, decoded.column.References, decoded.value.(string)); err != nil {
			return err
		}
	}
	return nil
}

// insertRecord writes a new row of recordType with the given values; the
// caller has minted id and sequence.
func insertRecord(transaction *sql.Tx, recordType RecordType, id string, sequence int64, extra map[string]any, values map[string]decodedValue) error {
	names := []string{"id", "seq"}
	arguments := []any{id, sequence}
	extraNames := make([]string, 0, len(extra))
	for name := range extra {
		extraNames = append(extraNames, name)
	}
	sort.Strings(extraNames)
	for _, name := range extraNames {
		names = append(names, name)
		arguments = append(arguments, extra[name])
	}
	for _, column := range recordType.Columns {
		if decoded, present := values[column.Name]; present {
			names = append(names, column.Name)
			arguments = append(arguments, decoded.value)
		}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ")
	_, err := transaction.Exec(`INSERT INTO `+recordType.Table+` (`+strings.Join(names, ", ")+`) VALUES (`+placeholders+`)`, arguments...)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

func updateRecord(transaction *sql.Tx, recordType RecordType, idColumn, id string, extra map[string]any, values map[string]decodedValue) error {
	assignments := []string{}
	arguments := []any{}
	for name, value := range extra {
		assignments = append(assignments, name+" = ?")
		arguments = append(arguments, value)
	}
	for _, column := range recordType.Columns {
		if decoded, present := values[column.Name]; present {
			assignments = append(assignments, column.Name+" = ?")
			arguments = append(arguments, decoded.value)
		}
	}
	if len(assignments) == 0 {
		return nil
	}
	arguments = append(arguments, id)
	_, err := transaction.Exec(`UPDATE `+recordType.Table+` SET `+strings.Join(assignments, ", ")+` WHERE `+idColumn+` = ?`, arguments...)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

// CreatePossession makes a possession from a client's JSON object: its own
// fields, optionally one kind's fields under the kind's name, and optionally
// photos. It answers the possession as GetPossession does.
func (s *Store) CreatePossession(ctx context.Context, fields map[string]json.RawMessage, actor string) (map[string]any, error) {
	if actor == "" {
		return nil, fmt.Errorf("%w: say who is writing (the actor)", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	id, err := s.createPossessionInTransaction(ctx, transaction, fields, actor)
	if err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return s.GetPossession(id, false)
}

func (s *Store) createPossessionInTransaction(ctx context.Context, transaction *sql.Tx, fields map[string]json.RawMessage, actor string) (string, error) {
	own, kind, kindFields, photos, err := splitPossessionFields(fields)
	if err != nil {
		return "", err
	}
	if _, given := own["status"]; !given {
		own["status"] = json.RawMessage(`"owned"`)
	}
	values, err := possessionType.decodeFields(own, true)
	if err != nil {
		return "", err
	}
	if err := checkPrice(values, nil); err != nil {
		return "", err
	}
	if err := s.checkValues(ctx, transaction, values); err != nil {
		return "", err
	}
	id, sequence, err := nextID(transaction, possessionType.Table, possessionType.Prefix)
	if err != nil {
		return "", err
	}
	now := s.unixNow()
	if err := insertRecord(transaction, possessionType, id, sequence, map[string]any{"created_by": actor, "created_at": now, "updated_at": now}, values); err != nil {
		return "", err
	}
	if kind != nil {
		if err := s.insertKind(ctx, transaction, *kind, id, kindFields); err != nil {
			return "", err
		}
	}
	for _, photo := range photos {
		photo.LinkedBy = actor
		if err := s.linkPhotoInTransaction(ctx, transaction, id, photo, now); err != nil {
			return "", err
		}
	}
	return id, nil
}

// checkPrice holds price_cents and price_currency together. existing is the
// stored possession on a patch.
func checkPrice(values map[string]decodedValue, existing map[string]any) error {
	price, priceGiven := values["price_cents"]
	currency, currencyGiven := values["price_currency"]
	var priceValue any
	var currencyValue string
	if existing != nil {
		priceValue = existing["price_cents"]
		currencyValue, _ = existing["price_currency"].(string)
	}
	if priceGiven {
		priceValue = price.value
	}
	if currencyGiven {
		currencyValue, _ = currency.value.(string)
	}
	if priceValue != nil && len(currencyValue) != 3 {
		return fmt.Errorf("%w: price_cents needs price_currency, a three-letter ISO 4217 code such as USD", ErrInvalid)
	}
	return nil
}

func (s *Store) insertKind(ctx context.Context, transaction *sql.Tx, kind RecordType, possessionID string, kindFields map[string]json.RawMessage) error {
	values, err := kind.decodeFields(kindFields, true)
	if err != nil {
		return err
	}
	if err := s.checkValues(ctx, transaction, values); err != nil {
		return err
	}
	kindID, sequence, err := nextID(transaction, kind.Table, kind.Prefix)
	if err != nil {
		return err
	}
	return insertRecord(transaction, kind, kindID, sequence, map[string]any{"possession_id": possessionID}, values)
}

func (s *Store) linkPhotoInTransaction(ctx context.Context, transaction *sql.Tx, possessionID string, link PhotoLink, now int64) error {
	if err := link.validate(); err != nil {
		return err
	}
	if link.LinkedBy == "" {
		return fmt.Errorf("%w: say who links the photo", ErrInvalid)
	}
	if err := s.checkReference(ctx, transaction, ReferencePhoto, link.PhotoID); err != nil {
		return err
	}
	var coverCount int
	if err := transaction.QueryRow(`SELECT COUNT(*) FROM possession_photos WHERE possession_id = ? AND is_cover = 1`, possessionID).Scan(&coverCount); err != nil {
		return err
	}
	// The first photo is the cover until another is chosen, so a chip
	// always has a picture to draw.
	isCover := link.IsCover || coverCount == 0
	if isCover {
		if _, err := transaction.Exec(`UPDATE possession_photos SET is_cover = 0 WHERE possession_id = ?`, possessionID); err != nil {
			return err
		}
	}
	_, err := transaction.Exec(`INSERT INTO possession_photos (possession_id, photo_id, box_left, box_top, box_right, box_bottom, is_cover, sighting_id, linked_by, linked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (possession_id, photo_id) DO UPDATE SET box_left = excluded.box_left, box_top = excluded.box_top, box_right = excluded.box_right,
			box_bottom = excluded.box_bottom, is_cover = excluded.is_cover, sighting_id = COALESCE(excluded.sighting_id, sighting_id), linked_by = excluded.linked_by, linked_at = excluded.linked_at`,
		possessionID, link.PhotoID, link.BoxLeft, link.BoxTop, link.BoxRight, link.BoxBottom, boolToInteger(isCover), link.SightingID, link.LinkedBy, now)
	return err
}

func boolToInteger(value bool) int {
	if value {
		return 1
	}
	return 0
}

// LinkPhoto adds a photo to a possession, or changes its box or cover flag.
func (s *Store) LinkPhoto(ctx context.Context, possessionID string, link PhotoLink) (map[string]any, error) {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	if err := requireLivePossession(transaction, possessionID); err != nil {
		return nil, err
	}
	if err := s.linkPhotoInTransaction(ctx, transaction, possessionID, link, s.unixNow()); err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return s.GetPossession(possessionID, false)
}

// UnlinkPhoto removes a photo from a possession. The photo itself stays in
// photo-store. When the cover goes, the earliest linked photo left becomes it.
func (s *Store) UnlinkPhoto(ctx context.Context, possessionID, photoID string) (map[string]any, error) {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	result, err := transaction.Exec(`DELETE FROM possession_photos WHERE possession_id = ? AND photo_id = ?`, possessionID, photoID)
	if err != nil {
		return nil, err
	}
	if removed, _ := result.RowsAffected(); removed == 0 {
		return nil, fmt.Errorf("%w: %s has no photo %s", ErrNotFound, possessionID, photoID)
	}
	if _, err := transaction.Exec(`UPDATE possession_photos SET is_cover = 1 WHERE possession_id = ?1 AND NOT EXISTS (SELECT 1 FROM possession_photos WHERE possession_id = ?1 AND is_cover = 1)
		AND photo_id = (SELECT photo_id FROM possession_photos WHERE possession_id = ?1 ORDER BY linked_at, photo_id LIMIT 1)`, possessionID); err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return s.GetPossession(possessionID, false)
}

func requireLivePossession(q querier, id string) error {
	var deletedAt int64
	err := q.QueryRow(`SELECT deleted_at FROM possessions WHERE id = ?`, id).Scan(&deletedAt)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && deletedAt != 0) {
		return fmt.Errorf("%w: no possession %s", ErrNotFound, id)
	}
	return err
}

// GetPossession answers a possession with its kind, photos and, for a
// garment, the outfits it is in.
func (s *Store) GetPossession(id string, includeDeleted bool) (map[string]any, error) {
	return possessionView(s.db, id, includeDeleted)
}

func possessionView(q querier, id string, includeDeleted bool) (map[string]any, error) {
	columns := possessionType.readColumns()
	row := q.QueryRow(`SELECT `+strings.Join(columnNames(columns), ", ")+` FROM possessions WHERE id = ?`, id)
	view, err := scanRow(row, columns)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no possession %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	if view["deleted_at"].(int64) != 0 && !includeDeleted {
		return nil, fmt.Errorf("%w: possession %s is deleted", ErrNotFound, id)
	}
	view["kind"] = ""
	for _, kind := range KindTypes {
		kindColumns := kind.readColumns()
		record, err := scanRow(q.QueryRow(`SELECT `+strings.Join(columnNames(kindColumns), ", ")+` FROM `+kind.Table+` WHERE possession_id = ?`, id), kindColumns)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		delete(record, "possession_id")
		view["kind"] = kind.Name
		view[kind.Name] = record
		if kind.Name == "garment" {
			outfitIDs, err := stringColumn(q, `SELECT outfit_garments.outfit_id FROM outfit_garments JOIN outfits ON outfits.id = outfit_garments.outfit_id
				WHERE outfit_garments.garment_id = ? AND outfits.deleted_at = 0 ORDER BY outfits.seq`, record["id"])
			if err != nil {
				return nil, err
			}
			view["outfit_ids"] = outfitIDs
		}
		if kind.Name == "computer" {
			partIDs, err := stringColumn(q, `SELECT computer_parts.id FROM computer_parts JOIN possessions ON possessions.id = computer_parts.possession_id
				WHERE computer_parts.installed_in_computer_id = ? AND possessions.deleted_at = 0 ORDER BY computer_parts.seq`, record["id"])
			if err != nil {
				return nil, err
			}
			view["installed_part_ids"] = partIDs
		}
		if kind.Name == "vehicle" {
			readings, err := odometerReadings(q, record["id"].(string))
			if err != nil {
				return nil, err
			}
			view["odometer_readings"] = readings
		}
	}
	photos, err := possessionPhotos(q, id)
	if err != nil {
		return nil, err
	}
	view["photos"] = photos
	view["cover_photo_id"] = nil
	for _, photo := range photos {
		if photo.IsCover {
			view["cover_photo_id"] = photo.PhotoID
		}
	}
	return view, nil
}

func stringColumn(q querier, query string, arguments ...any) ([]string, error) {
	rows, err := q.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func possessionPhotos(q querier, possessionID string) ([]PhotoLink, error) {
	rows, err := q.Query(`SELECT photo_id, box_left, box_top, box_right, box_bottom, is_cover, sighting_id, linked_by, linked_at
		FROM possession_photos WHERE possession_id = ? ORDER BY is_cover DESC, linked_at, photo_id`, possessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	photos := []PhotoLink{}
	for rows.Next() {
		var photo PhotoLink
		var left, top, right, bottom sql.NullFloat64
		var sighting sql.NullString
		var isCover int
		if err := rows.Scan(&photo.PhotoID, &left, &top, &right, &bottom, &isCover, &sighting, &photo.LinkedBy, &photo.LinkedAt); err != nil {
			return nil, err
		}
		if left.Valid {
			photo.BoxLeft, photo.BoxTop, photo.BoxRight, photo.BoxBottom = &left.Float64, &top.Float64, &right.Float64, &bottom.Float64
		}
		if sighting.Valid {
			photo.SightingID = &sighting.String
		}
		photo.IsCover = isCover == 1
		photos = append(photos, photo)
	}
	return photos, rows.Err()
}

// PossessionFilter narrows ListPossessions.
type PossessionFilter struct {
	Query          string // every word in name, brand, model, description, spot or tags
	Status         string
	Kind           string // a kind name, or "none" for possessions with no kind
	PlaceID        string
	IncludeDeleted bool
	Limit          int
	Offset         int
}

// ListPossessions answers possessions, newest first.
func (s *Store) ListPossessions(filter PossessionFilter) ([]map[string]any, error) {
	conditions := []string{}
	arguments := []any{}
	if !filter.IncludeDeleted {
		conditions = append(conditions, "deleted_at = 0")
	}
	if filter.Status != "" {
		if !contains(PossessionStatuses, filter.Status) {
			return nil, fmt.Errorf("%w: status %q is not one of %s", ErrInvalid, filter.Status, strings.Join(PossessionStatuses, ", "))
		}
		conditions = append(conditions, "status = ?")
		arguments = append(arguments, filter.Status)
	}
	if filter.PlaceID != "" {
		conditions = append(conditions, "place_id = ?")
		arguments = append(arguments, filter.PlaceID)
	}
	if filter.Kind != "" {
		if filter.Kind == "none" {
			for _, kind := range KindTypes {
				conditions = append(conditions, "id NOT IN (SELECT possession_id FROM "+kind.Table+")")
			}
		} else {
			kind, known := KindByName(filter.Kind)
			if !known {
				return nil, fmt.Errorf("%w: kind %q is not one of %s, or none", ErrInvalid, filter.Kind, strings.Join(KindNames(), ", "))
			}
			conditions = append(conditions, "id IN (SELECT possession_id FROM "+kind.Table+")")
		}
	}
	for _, word := range strings.Fields(strings.ToLower(filter.Query)) {
		conditions = append(conditions, "LOWER(name || ' ' || brand || ' ' || model || ' ' || description || ' ' || spot || ' ' || tags) LIKE ?")
		arguments = append(arguments, "%"+word+"%")
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		return nil, fmt.Errorf("%w: limit is at most 1000", ErrInvalid)
	}
	query := `SELECT id FROM possessions`
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, " AND ")
	}
	query += ` ORDER BY seq DESC LIMIT ? OFFSET ?`
	arguments = append(arguments, limit, filter.Offset)
	ids, err := stringColumn(s.db, query, arguments...)
	if err != nil {
		return nil, err
	}
	views := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		view, err := possessionView(s.db, id, true)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// PatchPossession changes any of a possession's fields and its kind's fields.
// A possession with no kind takes one; one with a kind cannot take another.
func (s *Store) PatchPossession(ctx context.Context, id string, fields map[string]json.RawMessage, actor string) (map[string]any, error) {
	if actor == "" {
		return nil, fmt.Errorf("%w: say who is writing (the actor)", ErrInvalid)
	}
	own, kind, kindFields, photos, err := splitPossessionFields(fields)
	if err != nil {
		return nil, err
	}
	if photos != nil {
		return nil, fmt.Errorf("%w: change photos with POST and DELETE /possessions/{id}/photos", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	existing, err := possessionView(transaction, id, false)
	if err != nil {
		return nil, err
	}
	values, err := possessionType.decodeFields(own, false)
	if err != nil {
		return nil, err
	}
	if err := checkPrice(values, existing); err != nil {
		return nil, err
	}
	if err := s.checkValues(ctx, transaction, values); err != nil {
		return nil, err
	}
	now := s.unixNow()
	if err := saveRevision(transaction, "possessions", "id", id, actor, now); err != nil {
		return nil, err
	}
	if err := updateRecord(transaction, possessionType, "id", id, map[string]any{"updated_at": now}, values); err != nil {
		return nil, err
	}
	if kind != nil {
		currentKind := existing["kind"].(string)
		switch currentKind {
		case "":
			if err := s.insertKind(ctx, transaction, *kind, id, kindFields); err != nil {
				return nil, err
			}
		case kind.Name:
			kindValues, err := kind.decodeFields(kindFields, false)
			if err != nil {
				return nil, err
			}
			if err := s.checkValues(ctx, transaction, kindValues); err != nil {
				return nil, err
			}
			if err := saveRevision(transaction, kind.Table, "possession_id", id, actor, now); err != nil {
				return nil, err
			}
			if err := updateRecord(transaction, *kind, "possession_id", id, nil, kindValues); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("%w: %s is a %s, not a %s", ErrConflict, id, currentKind, kind.Name)
		}
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return s.GetPossession(id, false)
}

// DeletePossession stamps deleted_at; RestorePossession clears it.
func (s *Store) DeletePossession(id, actor string) error {
	return s.setPossessionDeleted(id, actor, true)
}

// RestorePossession brings back a deleted possession.
func (s *Store) RestorePossession(id, actor string) (map[string]any, error) {
	if err := s.setPossessionDeleted(id, actor, false); err != nil {
		return nil, err
	}
	return s.GetPossession(id, false)
}

func (s *Store) setPossessionDeleted(id, actor string, deleted bool) error {
	if actor == "" {
		return fmt.Errorf("%w: say who is writing (the actor)", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if _, err := possessionView(transaction, id, true); err != nil {
		return err
	}
	now := s.unixNow()
	if err := saveRevision(transaction, "possessions", "id", id, actor, now); err != nil {
		return err
	}
	deletedAt := int64(0)
	if deleted {
		deletedAt = now
	}
	if _, err := transaction.Exec(`UPDATE possessions SET deleted_at = ?, updated_at = ? WHERE id = ?`, deletedAt, now, id); err != nil {
		return err
	}
	return transaction.Commit()
}

// PossessionIDForKindRecord finds the possession a kind record extends.
func (s *Store) PossessionIDForKindRecord(kind RecordType, kindID string) (string, error) {
	var possessionID string
	err := s.db.QueryRow(`SELECT possession_id FROM `+kind.Table+` WHERE id = ?`, kindID).Scan(&possessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: no %s %s", ErrNotFound, kind.Name, kindID)
	}
	return possessionID, err
}

// ComputerForHarnessMachine finds the computer tied to a harness-store machine.
func (s *Store) ComputerForHarnessMachine(machineID string) (map[string]any, error) {
	var possessionID string
	err := s.db.QueryRow(`SELECT possession_id FROM computers WHERE harness_machine_id = ?`, machineID).Scan(&possessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no computer is tied to harness machine %s", ErrNotFound, machineID)
	}
	if err != nil {
		return nil, err
	}
	return s.GetPossession(possessionID, false)
}

// OdometerReading is one vehicle mileage reading.
type OdometerReading struct {
	ReadAt int64  `json:"read_at"`
	Miles  int64  `json:"miles"`
	Source string `json:"source"`
}

func odometerReadings(q querier, vehicleID string) ([]OdometerReading, error) {
	rows, err := q.Query(`SELECT read_at, miles, source FROM vehicle_odometer_readings WHERE vehicle_id = ? ORDER BY read_at`, vehicleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	readings := []OdometerReading{}
	for rows.Next() {
		var reading OdometerReading
		if err := rows.Scan(&reading.ReadAt, &reading.Miles, &reading.Source); err != nil {
			return nil, err
		}
		readings = append(readings, reading)
	}
	return readings, rows.Err()
}

// AddOdometerReading records a vehicle's mileage at a time.
func (s *Store) AddOdometerReading(vehicleID string, reading OdometerReading) error {
	if reading.ReadAt <= 0 || reading.Miles < 0 || reading.Source == "" {
		return fmt.Errorf("%w: a reading needs read_at, miles of 0 or more, and its source", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	var found int
	if err := s.db.QueryRow(`SELECT 1 FROM vehicles WHERE id = ?`, vehicleID).Scan(&found); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no vehicle %s", ErrNotFound, vehicleID)
	} else if err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO vehicle_odometer_readings (vehicle_id, read_at, miles, source) VALUES (?, ?, ?, ?)`, vehicleID, reading.ReadAt, reading.Miles, reading.Source)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("%w: %s already has a reading at %d", ErrConflict, vehicleID, reading.ReadAt)
	}
	return err
}
