package locationstore

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// OptionalID is a nullable id field of a patch: Set says the patch names the
// field, and a nil Value then clears it.
type OptionalID struct {
	Set   bool
	Value *string
}

// PlacePatch is the body of PATCH /places/{id}. A nil field is left alone.
type PlacePatch struct {
	Name         *string   `json:"name,omitempty"`
	Latitude     *float64  `json:"latitude,omitempty"`
	Longitude    *float64  `json:"longitude,omitempty"`
	RadiusMeters *float64  `json:"radius_meters,omitempty"`
	Address      *string   `json:"address,omitempty"`
	Kind         *string   `json:"kind,omitempty"`
	Description  *string   `json:"description,omitempty"`
	DescribedBy  *string   `json:"described_by,omitempty"`
	Tags         *[]string `json:"tags,omitempty"`
}

// VisitPatch is the body of PATCH /visits/{id}.
type VisitPatch struct {
	PlaceID     OptionalID `json:"-"`
	Description *string    `json:"description,omitempty"`
	DescribedBy *string    `json:"described_by,omitempty"`
	Tags        *[]string  `json:"tags,omitempty"`
}

// TripPatch is the body of PATCH /trips/{id}.
type TripPatch struct {
	ParentTripID OptionalID `json:"-"`
	StartPlaceID OptionalID `json:"-"`
	EndPlaceID   OptionalID `json:"-"`
	Mode         *string    `json:"mode,omitempty"`
	Title        *string    `json:"title,omitempty"`
	Description  *string    `json:"description,omitempty"`
	DescribedBy  *string    `json:"described_by,omitempty"`
	Tags         *[]string  `json:"tags,omitempty"`
}

func describedByRequired(description, describedBy string) error {
	if description != "" && describedBy == "" {
		return fmt.Errorf("%w: a description needs described_by: who or what wrote it", ErrInvalid)
	}
	return nil
}

// ---- places ----

const placeColumns = `id, name, latitude, longitude, radius_meters, address, kind, google_place_id, description,
	described_by, tags, source, import_id, locality, created_at, updated_at, deleted_at, merged_into_place_id`

func scanPlace(scan func(...any) error) (*Place, error) {
	var place Place
	var googlePlaceID, importID, locality, mergedInto sql.NullString
	var tags string
	if err := scan(&place.ID, &place.Name, &place.Latitude, &place.Longitude, &place.RadiusMeters, &place.Address, &place.Kind,
		&googlePlaceID, &place.Description, &place.DescribedBy, &tags, &place.Source, &importID, &locality,
		&place.CreatedAt, &place.UpdatedAt, &place.DeletedAt, &mergedInto); err != nil {
		return nil, err
	}
	place.GooglePlaceID, place.ImportID = stringPointer(googlePlaceID), stringPointer(importID)
	place.MergedIntoPlaceID = stringPointer(mergedInto)
	var err error
	if place.Locality, err = decodeLocality(locality, "place "+place.ID); err != nil {
		return nil, err
	}
	if place.Tags, err = decodeStrings(tags, "place "+place.ID); err != nil {
		return nil, err
	}
	return &place, nil
}

func getPlaceRow(q queryer, id string) (*Place, error) {
	place, err := scanPlace(q.QueryRow(`SELECT `+placeColumns+` FROM places WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no place %s", ErrNotFound, id)
	}
	return place, err
}

// GetPlace reads one place. A deleted place is not found unless asked for.
func (s *Store) GetPlace(id string, includeDeleted bool) (*Place, error) {
	place, err := getPlaceRow(s.db, id)
	if err != nil {
		return nil, err
	}
	if place.DeletedAt != 0 && !includeDeleted {
		return nil, fmt.Errorf("%w: place %s is deleted", ErrNotFound, id)
	}
	return place, nil
}

func validatePlace(place *Place) error {
	switch {
	case !validLatitudeLongitude(place.Latitude, place.Longitude):
		return fmt.Errorf("%w: latitude or longitude out of range", ErrInvalid)
	case place.RadiusMeters < 0:
		return fmt.Errorf("%w: radius_meters cannot be negative", ErrInvalid)
	case !knownWord(PlaceKinds, place.Kind):
		return fmt.Errorf("%w: unknown kind %q: use one of %s", ErrInvalid, place.Kind, strings.Join(PlaceKinds, ", "))
	}
	return describedByRequired(place.Description, place.DescribedBy)
}

// CreatePlace makes a place by hand.
func (s *Store) CreatePlace(create PlaceCreate) (*Place, error) {
	if create.Latitude == nil || create.Longitude == nil {
		return nil, fmt.Errorf("%w: latitude and longitude are required", ErrInvalid)
	}
	tags, err := normalizeTags(create.Tags)
	if err != nil {
		return nil, err
	}
	place := &Place{Name: strings.TrimSpace(create.Name), Latitude: *create.Latitude, Longitude: *create.Longitude,
		RadiusMeters: create.RadiusMeters, Address: strings.TrimSpace(create.Address), Kind: create.Kind,
		Description: strings.TrimSpace(create.Description), DescribedBy: strings.TrimSpace(create.DescribedBy), Tags: tags}
	if place.Name == "" {
		return nil, fmt.Errorf("%w: a place made by hand needs a name", ErrInvalid)
	}
	if err := validatePlace(place); err != nil {
		return nil, err
	}
	var id string
	err = s.inTransaction(func(tx *sql.Tx) error {
		var seq int64
		var err error
		if id, seq, err = nextID(tx, "places", "place"); err != nil {
			return err
		}
		at := now()
		_, err = tx.Exec(`INSERT INTO places (id, seq, name, latitude, longitude, radius_meters, address, kind, description,
			described_by, tags, source, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, seq, place.Name, place.Latitude, place.Longitude, place.RadiusMeters, place.Address, place.Kind,
			place.Description, place.DescribedBy, encodeStrings(place.Tags), SourceManual, at, at)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetPlace(id, false)
}

// PlaceFilter selects places for GET /places, oldest first.
type PlaceFilter struct {
	Query          string // words that must all appear in the name, address, description or tags
	Kind           string
	Tag            string
	GooglePlaceID  string
	IncludeDeleted bool
	Limit, Offset  int
}

// ListPlaces answers GET /places.
func (s *Store) ListPlaces(filter PlaceFilter) ([]Place, error) {
	where, args := []string{}, []any{}
	if !filter.IncludeDeleted {
		where = append(where, "deleted_at = 0")
	}
	for _, word := range strings.Fields(filter.Query) {
		where = append(where, `(name LIKE ? ESCAPE '\' OR address LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\' OR tags LIKE ? ESCAPE '\')`)
		pattern := likePattern(word)
		args = append(args, pattern, pattern, pattern, pattern)
	}
	if filter.Kind != "" {
		if !knownWord(PlaceKinds, filter.Kind) {
			return nil, fmt.Errorf("%w: unknown kind %q: use one of %s", ErrInvalid, filter.Kind, strings.Join(PlaceKinds, ", "))
		}
		where, args = append(where, "kind = ?"), append(args, filter.Kind)
	}
	if filter.Tag != "" {
		where, args = append(where, "EXISTS (SELECT 1 FROM json_each(places.tags) WHERE value = ?)"), append(args, filter.Tag)
	}
	if filter.GooglePlaceID != "" {
		where, args = append(where, "google_place_id = ?"), append(args, filter.GooglePlaceID)
	}
	query := `SELECT ` + placeColumns + ` FROM places`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` ORDER BY seq LIMIT ? OFFSET ?`
	args = append(args, clampLimit(filter.Limit, defaultLimit, maximumLimit), max(filter.Offset, 0))
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	places := []Place{}
	for rows.Next() {
		place, err := scanPlace(rows.Scan)
		if err != nil {
			return nil, err
		}
		places = append(places, *place)
	}
	return places, rows.Err()
}

// PatchPlace changes the fields the patch names on a live place.
func (s *Store) PatchPlace(id string, patch PlacePatch) (*Place, error) {
	err := s.inTransaction(func(tx *sql.Tx) error {
		place, err := getPlaceRow(tx, id)
		if err != nil {
			return err
		}
		if place.DeletedAt != 0 {
			return fmt.Errorf("%w: place %s is deleted; restore it first", ErrNotFound, id)
		}
		at := now()
		if err := snapshot(tx, EntityPlace, id, "update", at); err != nil {
			return err
		}
		if patch.Name != nil {
			place.Name = strings.TrimSpace(*patch.Name)
		}
		if patch.Latitude != nil {
			place.Latitude = *patch.Latitude
		}
		if patch.Longitude != nil {
			place.Longitude = *patch.Longitude
		}
		if patch.RadiusMeters != nil {
			place.RadiusMeters = *patch.RadiusMeters
		}
		if patch.Address != nil {
			place.Address = strings.TrimSpace(*patch.Address)
		}
		if patch.Kind != nil {
			place.Kind = *patch.Kind
		}
		if patch.Description != nil {
			place.Description = strings.TrimSpace(*patch.Description)
		}
		if patch.DescribedBy != nil {
			place.DescribedBy = strings.TrimSpace(*patch.DescribedBy)
		}
		if patch.Tags != nil {
			if place.Tags, err = normalizeTags(*patch.Tags); err != nil {
				return err
			}
		}
		if err := validatePlace(place); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE places SET name=?, latitude=?, longitude=?, radius_meters=?, address=?, kind=?, description=?,
			described_by=?, tags=?, updated_at=? WHERE id=?`, place.Name, place.Latitude, place.Longitude, place.RadiusMeters,
			place.Address, place.Kind, place.Description, place.DescribedBy, encodeStrings(place.Tags), at, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetPlace(id, false)
}

// ---- visits ----

const visitSelect = `SELECT v.id, v.place_id, COALESCE(p.name, ''), v.started_at, v.ended_at, v.latitude, v.longitude,
	v.probability, v.source_semantic_type, v.description, v.described_by, v.tags, v.source, v.source_ref, v.import_id,
	v.superseded_by_import_id, v.locality, v.created_at, v.updated_at, v.deleted_at FROM visits v LEFT JOIN places p ON p.id = v.place_id`

func scanVisit(scan func(...any) error) (*Visit, error) {
	var visit Visit
	var placeID, importID, supersededBy, locality sql.NullString
	var latitude, longitude, probability sql.NullFloat64
	var tags string
	if err := scan(&visit.ID, &placeID, &visit.PlaceName, &visit.StartedAt, &visit.EndedAt, &latitude, &longitude,
		&probability, &visit.SourceSemanticType, &visit.Description, &visit.DescribedBy, &tags, &visit.Source,
		&visit.SourceRef, &importID, &supersededBy, &locality, &visit.CreatedAt, &visit.UpdatedAt, &visit.DeletedAt); err != nil {
		return nil, err
	}
	visit.PlaceID, visit.ImportID, visit.SupersededByImportID = stringPointer(placeID), stringPointer(importID), stringPointer(supersededBy)
	visit.Latitude, visit.Longitude, visit.Probability = floatPointer(latitude), floatPointer(longitude), floatPointer(probability)
	var err error
	if visit.Tags, err = decodeStrings(tags, "visit "+visit.ID); err != nil {
		return nil, err
	}
	if visit.Locality, err = decodeLocality(locality, "visit "+visit.ID); err != nil {
		return nil, err
	}
	return &visit, nil
}

func getVisitRow(q queryer, id string) (*Visit, error) {
	visit, err := scanVisit(q.QueryRow(visitSelect+` WHERE v.id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no visit %s", ErrNotFound, id)
	}
	return visit, err
}

// GetVisit reads one visit. A deleted visit is not found unless asked for.
func (s *Store) GetVisit(id string, includeDeleted bool) (*Visit, error) {
	visit, err := getVisitRow(s.db, id)
	if err != nil {
		return nil, err
	}
	if visit.DeletedAt != 0 && !includeDeleted {
		return nil, fmt.Errorf("%w: visit %s is deleted", ErrNotFound, id)
	}
	return visit, nil
}

// requirePlace refuses an id that names no place. A deleted place may still be
// linked: the link is history, not an endorsement.
func requirePlace(q queryer, id *string, field string) error {
	if id == nil {
		return nil
	}
	if _, err := getPlaceRow(q, *id); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalid, field, err)
	}
	return nil
}

// CreateVisit makes a visit by hand.
func (s *Store) CreateVisit(create VisitCreate) (*Visit, error) {
	if err := checkTimeRange(create.StartedAt, create.EndedAt); err != nil {
		return nil, err
	}
	if err := checkCoordinatePair("the visit's", create.Latitude, create.Longitude); err != nil {
		return nil, err
	}
	if create.PlaceID == nil && create.Latitude == nil {
		return nil, fmt.Errorf("%w: a visit needs a place_id or a latitude and longitude", ErrInvalid)
	}
	tags, err := normalizeTags(create.Tags)
	if err != nil {
		return nil, err
	}
	description, describedBy := strings.TrimSpace(create.Description), strings.TrimSpace(create.DescribedBy)
	if err := describedByRequired(description, describedBy); err != nil {
		return nil, err
	}
	var id string
	err = s.inTransaction(func(tx *sql.Tx) error {
		if err := requirePlace(tx, create.PlaceID, "place_id"); err != nil {
			return err
		}
		var seq int64
		var err error
		if id, seq, err = nextID(tx, "visits", "visit"); err != nil {
			return err
		}
		at := now()
		_, err = tx.Exec(`INSERT INTO visits (id, seq, place_id, started_at, ended_at, latitude, longitude, description,
			described_by, tags, source, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, seq, nullableString(create.PlaceID), create.StartedAt, create.EndedAt, nullableFloat(create.Latitude),
			nullableFloat(create.Longitude), description, describedBy, encodeStrings(tags), SourceManual, at, at)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetVisit(id, false)
}

// TimeFilter is the time range and paging every time-ordered list shares. A
// record is in the range when it overlaps it: it ends at or after From and
// starts before To. 0 leaves that side open.
type TimeFilter struct {
	From, To       int64
	Source         string
	Tag            string
	IncludeDeleted bool
	Limit, Offset  int
}

func (filter TimeFilter) clauses(alias string) ([]string, []any) {
	where, args := []string{}, []any{}
	if !filter.IncludeDeleted {
		where = append(where, alias+".deleted_at = 0")
	}
	if filter.From > 0 {
		where, args = append(where, alias+".ended_at >= ?"), append(args, filter.From)
	}
	if filter.To > 0 {
		where, args = append(where, alias+".started_at < ?"), append(args, filter.To)
	}
	if filter.Source != "" {
		where, args = append(where, alias+".source = ?"), append(args, filter.Source)
	}
	if filter.Tag != "" {
		where, args = append(where, "EXISTS (SELECT 1 FROM json_each("+alias+".tags) WHERE value = ?)"), append(args, filter.Tag)
	}
	return where, args
}

// VisitFilter selects visits for GET /visits, earliest first.
type VisitFilter struct {
	TimeFilter
	PlaceID string
}

// ListVisits answers GET /visits.
func (s *Store) ListVisits(filter VisitFilter) ([]Visit, error) {
	where, args := filter.clauses("v")
	if filter.PlaceID != "" {
		where, args = append(where, "v.place_id = ?"), append(args, filter.PlaceID)
	}
	query := visitSelect
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` ORDER BY v.started_at, v.seq LIMIT ? OFFSET ?`
	args = append(args, clampLimit(filter.Limit, defaultLimit, maximumLimit), max(filter.Offset, 0))
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	visits := []Visit{}
	for rows.Next() {
		visit, err := scanVisit(rows.Scan)
		if err != nil {
			return nil, err
		}
		visits = append(visits, *visit)
	}
	return visits, rows.Err()
}

// PatchVisit changes the fields the patch names on a live visit.
func (s *Store) PatchVisit(id string, patch VisitPatch) (*Visit, error) {
	err := s.inTransaction(func(tx *sql.Tx) error {
		visit, err := getVisitRow(tx, id)
		if err != nil {
			return err
		}
		if visit.DeletedAt != 0 {
			return fmt.Errorf("%w: visit %s is deleted; restore it first", ErrNotFound, id)
		}
		at := now()
		if err := snapshot(tx, EntityVisit, id, "update", at); err != nil {
			return err
		}
		if patch.PlaceID.Set {
			if err := requirePlace(tx, patch.PlaceID.Value, "place_id"); err != nil {
				return err
			}
			visit.PlaceID = patch.PlaceID.Value
		}
		if patch.Description != nil {
			visit.Description = strings.TrimSpace(*patch.Description)
		}
		if patch.DescribedBy != nil {
			visit.DescribedBy = strings.TrimSpace(*patch.DescribedBy)
		}
		if patch.Tags != nil {
			if visit.Tags, err = normalizeTags(*patch.Tags); err != nil {
				return err
			}
		}
		if err := describedByRequired(visit.Description, visit.DescribedBy); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE visits SET place_id=?, description=?, described_by=?, tags=?, updated_at=? WHERE id=?`,
			nullableString(visit.PlaceID), visit.Description, visit.DescribedBy, encodeStrings(visit.Tags), at, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetVisit(id, false)
}

// ---- trips ----

const tripSelect = `SELECT t.id, t.kind, t.parent_trip_id, t.parent_set_by, t.started_at, t.ended_at, t.start_latitude, t.start_longitude,
	t.end_latitude, t.end_longitude, t.start_place_id, COALESCE(sp.name, ''), t.end_place_id, COALESCE(ep.name, ''),
	t.mode, t.source_mode, t.distance_meters, t.probability, t.title, t.title_by, t.description, t.described_by, t.tags,
	t.source, t.source_ref, t.import_id, t.superseded_by_import_id, t.start_locality, t.end_locality,
	(SELECT COUNT(*) FROM trip_path_points pp WHERE pp.trip_id = t.id),
	(SELECT COUNT(*) FROM trips c WHERE c.parent_trip_id = t.id AND c.deleted_at = 0),
	t.created_at, t.updated_at, t.deleted_at
	FROM trips t LEFT JOIN places sp ON sp.id = t.start_place_id LEFT JOIN places ep ON ep.id = t.end_place_id`

func scanTrip(scan func(...any) error) (*Trip, error) {
	var trip Trip
	var parentTripID, startPlaceID, endPlaceID, importID, supersededBy, startLocality, endLocality sql.NullString
	var startLatitude, startLongitude, endLatitude, endLongitude, distance, probability sql.NullFloat64
	var tags string
	if err := scan(&trip.ID, &trip.Kind, &parentTripID, &trip.ParentSetBy, &trip.StartedAt, &trip.EndedAt, &startLatitude, &startLongitude,
		&endLatitude, &endLongitude, &startPlaceID, &trip.StartPlaceName, &endPlaceID, &trip.EndPlaceName,
		&trip.Mode, &trip.SourceMode, &distance, &probability, &trip.Title, &trip.TitleBy, &trip.Description, &trip.DescribedBy, &tags,
		&trip.Source, &trip.SourceRef, &importID, &supersededBy, &startLocality, &endLocality, &trip.PathPoints, &trip.ChildTrips,
		&trip.CreatedAt, &trip.UpdatedAt, &trip.DeletedAt); err != nil {
		return nil, err
	}
	trip.ParentTripID, trip.StartPlaceID, trip.EndPlaceID = stringPointer(parentTripID), stringPointer(startPlaceID), stringPointer(endPlaceID)
	trip.ImportID, trip.SupersededByImportID = stringPointer(importID), stringPointer(supersededBy)
	trip.StartLatitude, trip.StartLongitude = floatPointer(startLatitude), floatPointer(startLongitude)
	trip.EndLatitude, trip.EndLongitude = floatPointer(endLatitude), floatPointer(endLongitude)
	trip.DistanceMeters, trip.Probability = floatPointer(distance), floatPointer(probability)
	var err error
	if trip.Tags, err = decodeStrings(tags, "trip "+trip.ID); err != nil {
		return nil, err
	}
	if trip.StartLocality, err = decodeLocality(startLocality, "trip "+trip.ID+" start"); err != nil {
		return nil, err
	}
	if trip.EndLocality, err = decodeLocality(endLocality, "trip "+trip.ID+" end"); err != nil {
		return nil, err
	}
	return &trip, nil
}

func getTripRow(q queryer, id string) (*Trip, error) {
	trip, err := scanTrip(q.QueryRow(tripSelect+` WHERE t.id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no trip %s", ErrNotFound, id)
	}
	return trip, err
}

// GetTrip reads one trip. A deleted trip is not found unless asked for.
func (s *Store) GetTrip(id string, includeDeleted bool) (*Trip, error) {
	trip, err := getTripRow(s.db, id)
	if err != nil {
		return nil, err
	}
	if trip.DeletedAt != 0 && !includeDeleted {
		return nil, fmt.Errorf("%w: trip %s is deleted", ErrNotFound, id)
	}
	return trip, nil
}

// checkParent refuses a parent that does not exist, is deleted, or would make
// a loop: the child itself or one of its own descendants.
func checkParent(q queryer, childID string, parentID *string) error {
	if parentID == nil {
		return nil
	}
	parent, err := getTripRow(q, *parentID)
	if err != nil {
		return fmt.Errorf("%w: parent_trip_id: %v", ErrInvalid, err)
	}
	if parent.DeletedAt != 0 {
		return fmt.Errorf("%w: parent trip %s is deleted", ErrInvalid, parent.ID)
	}
	for ancestor := parent; ; {
		if ancestor.ID == childID {
			return fmt.Errorf("%w: trip %s cannot be part of %s: that would make a loop", ErrInvalid, childID, *parentID)
		}
		if ancestor.ParentTripID == nil {
			return nil
		}
		if ancestor, err = getTripRow(q, *ancestor.ParentTripID); err != nil {
			return err
		}
	}
}

// CreateTrip makes a trip by hand: usually a road trip or holiday that
// imported trips are then attached to.
func (s *Store) CreateTrip(create TripCreate) (*Trip, error) {
	if err := checkTimeRange(create.StartedAt, create.EndedAt); err != nil {
		return nil, err
	}
	if err := checkCoordinatePair("start", create.StartLatitude, create.StartLongitude); err != nil {
		return nil, err
	}
	if err := checkCoordinatePair("end", create.EndLatitude, create.EndLongitude); err != nil {
		return nil, err
	}
	if !knownWord(TripModes, create.Mode) {
		return nil, fmt.Errorf("%w: unknown mode %q: use one of %s", ErrInvalid, create.Mode, strings.Join(TripModes, ", "))
	}
	kind := create.Kind
	if kind == "" {
		kind = TripKindJourney
	}
	if !knownWord(TripKinds, kind) {
		return nil, fmt.Errorf("%w: unknown kind %q: use one of %s", ErrInvalid, kind, strings.Join(TripKinds, ", "))
	}
	if create.DistanceMeters != nil && *create.DistanceMeters < 0 {
		return nil, fmt.Errorf("%w: distance_meters cannot be negative", ErrInvalid)
	}
	title := strings.TrimSpace(create.Title)
	if title == "" {
		return nil, fmt.Errorf("%w: a trip made by hand needs a title", ErrInvalid)
	}
	tags, err := normalizeTags(create.Tags)
	if err != nil {
		return nil, err
	}
	description, describedBy := strings.TrimSpace(create.Description), strings.TrimSpace(create.DescribedBy)
	if err := describedByRequired(description, describedBy); err != nil {
		return nil, err
	}
	var id string
	err = s.inTransaction(func(tx *sql.Tx) error {
		if err := requirePlace(tx, create.StartPlaceID, "start_place_id"); err != nil {
			return err
		}
		if err := requirePlace(tx, create.EndPlaceID, "end_place_id"); err != nil {
			return err
		}
		var seq int64
		var err error
		if id, seq, err = nextID(tx, "trips", "trip"); err != nil {
			return err
		}
		if err := checkParent(tx, id, create.ParentTripID); err != nil {
			return err
		}
		at := now()
		parentSetBy := ""
		if create.ParentTripID != nil {
			parentSetBy = SetByHand
		}
		_, err = tx.Exec(`INSERT INTO trips (id, seq, kind, parent_trip_id, parent_set_by, title_by, started_at, ended_at, start_latitude, start_longitude,
			end_latitude, end_longitude, start_place_id, end_place_id, mode, distance_meters, title, description,
			described_by, tags, source, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, seq, kind, nullableString(create.ParentTripID), parentSetBy, SetByHand, create.StartedAt, create.EndedAt,
			nullableFloat(create.StartLatitude), nullableFloat(create.StartLongitude), nullableFloat(create.EndLatitude),
			nullableFloat(create.EndLongitude), nullableString(create.StartPlaceID), nullableString(create.EndPlaceID),
			create.Mode, nullableFloat(create.DistanceMeters), title, description, describedBy, encodeStrings(tags),
			SourceManual, at, at)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetTrip(id, false)
}

// TripFilter selects trips for GET /trips, earliest first.
type TripFilter struct {
	TimeFilter
	Mode string
	Kind string
	// ParentTripID lists the children of one trip; "none" lists only trips
	// that are part of no other.
	ParentTripID          string
	MinimumDistanceMeters float64
	// ExcludeParents leaves out trips that have live children, so a road trip
	// does not stand beside its own drives.
	ExcludeParents bool
}

// ListTrips answers GET /trips.
func (s *Store) ListTrips(filter TripFilter) ([]Trip, error) {
	where, args := filter.clauses("t")
	if filter.Mode != "" {
		if !knownWord(TripModes, filter.Mode) {
			return nil, fmt.Errorf("%w: unknown mode %q: use one of %s", ErrInvalid, filter.Mode, strings.Join(TripModes, ", "))
		}
		where, args = append(where, "t.mode = ?"), append(args, filter.Mode)
	}
	if filter.Kind != "" {
		if !knownWord(TripKinds, filter.Kind) {
			return nil, fmt.Errorf("%w: unknown kind %q: use one of %s", ErrInvalid, filter.Kind, strings.Join(TripKinds, ", "))
		}
		where, args = append(where, "t.kind = ?"), append(args, filter.Kind)
	}
	switch filter.ParentTripID {
	case "":
	case "none":
		where = append(where, "t.parent_trip_id IS NULL")
	default:
		where, args = append(where, "t.parent_trip_id = ?"), append(args, filter.ParentTripID)
	}
	if filter.MinimumDistanceMeters > 0 {
		where, args = append(where, "t.distance_meters >= ?"), append(args, filter.MinimumDistanceMeters)
	}
	if filter.ExcludeParents {
		where = append(where, "NOT EXISTS (SELECT 1 FROM trips c WHERE c.parent_trip_id = t.id AND c.deleted_at = 0)")
	}
	query := tripSelect
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` ORDER BY t.started_at, t.seq LIMIT ? OFFSET ?`
	args = append(args, clampLimit(filter.Limit, defaultLimit, maximumLimit), max(filter.Offset, 0))
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	trips := []Trip{}
	for rows.Next() {
		trip, err := scanTrip(rows.Scan)
		if err != nil {
			return nil, err
		}
		trips = append(trips, *trip)
	}
	return trips, rows.Err()
}

// PatchTrip changes the fields the patch names on a live trip.
func (s *Store) PatchTrip(id string, patch TripPatch) (*Trip, error) {
	err := s.inTransaction(func(tx *sql.Tx) error {
		trip, err := getTripRow(tx, id)
		if err != nil {
			return err
		}
		if trip.DeletedAt != 0 {
			return fmt.Errorf("%w: trip %s is deleted; restore it first", ErrNotFound, id)
		}
		at := now()
		if err := snapshot(tx, EntityTrip, id, "update", at); err != nil {
			return err
		}
		if patch.ParentTripID.Set {
			if err := checkParent(tx, id, patch.ParentTripID.Value); err != nil {
				return err
			}
			trip.ParentTripID = patch.ParentTripID.Value
			trip.ParentSetBy = SetByHand
		}
		if patch.StartPlaceID.Set {
			if err := requirePlace(tx, patch.StartPlaceID.Value, "start_place_id"); err != nil {
				return err
			}
			trip.StartPlaceID = patch.StartPlaceID.Value
		}
		if patch.EndPlaceID.Set {
			if err := requirePlace(tx, patch.EndPlaceID.Value, "end_place_id"); err != nil {
				return err
			}
			trip.EndPlaceID = patch.EndPlaceID.Value
		}
		if patch.Mode != nil {
			if !knownWord(TripModes, *patch.Mode) {
				return fmt.Errorf("%w: unknown mode %q: use one of %s", ErrInvalid, *patch.Mode, strings.Join(TripModes, ", "))
			}
			trip.Mode = *patch.Mode
		}
		if patch.Title != nil {
			trip.Title = strings.TrimSpace(*patch.Title)
			trip.TitleBy = SetByHand
		}
		if patch.Description != nil {
			trip.Description = strings.TrimSpace(*patch.Description)
		}
		if patch.DescribedBy != nil {
			trip.DescribedBy = strings.TrimSpace(*patch.DescribedBy)
		}
		if patch.Tags != nil {
			if trip.Tags, err = normalizeTags(*patch.Tags); err != nil {
				return err
			}
		}
		if err := describedByRequired(trip.Description, trip.DescribedBy); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE trips SET parent_trip_id=?, parent_set_by=?, start_place_id=?, end_place_id=?, mode=?, title=?,
			title_by=?, description=?, described_by=?, tags=?, updated_at=? WHERE id=?`, nullableString(trip.ParentTripID),
			trip.ParentSetBy, nullableString(trip.StartPlaceID), nullableString(trip.EndPlaceID), trip.Mode, trip.Title, trip.TitleBy,
			trip.Description, trip.DescribedBy, encodeStrings(trip.Tags), at, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetTrip(id, false)
}

// AttachChildren makes each named trip part of parentID, in one transaction:
// one bad id attaches none. The routes call it; a trip attached so is placed
// by hand, and location-group-journeys leaves it there.
func (s *Store) AttachChildren(parentID string, childIDs []string) (*Trip, error) {
	if len(childIDs) == 0 {
		return nil, fmt.Errorf("%w: trip_ids is empty", ErrInvalid)
	}
	err := s.inTransaction(func(tx *sql.Tx) error {
		at := now()
		for _, childID := range childIDs {
			child, err := getTripRow(tx, childID)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrInvalid, err)
			}
			if child.DeletedAt != 0 {
				return fmt.Errorf("%w: trip %s is deleted", ErrInvalid, childID)
			}
			if child.ParentTripID != nil && *child.ParentTripID == parentID && child.ParentSetBy == SetByHand {
				continue
			}
			if err := checkParent(tx, childID, &parentID); err != nil {
				return err
			}
			if err := snapshot(tx, EntityTrip, childID, "attach", at); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE trips SET parent_trip_id=?, parent_set_by=?, updated_at=? WHERE id=?`, parentID, SetByHand, at, childID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetTrip(parentID, false)
}

// TripPath is a trip's path, in order.
func (s *Store) TripPath(id string) ([]PathPoint, error) {
	if _, err := getTripRow(s.db, id); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT latitude, longitude, timestamp FROM trip_path_points WHERE trip_id = ? ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	path := []PathPoint{}
	for rows.Next() {
		var point PathPoint
		var timestamp sql.NullInt64
		if err := rows.Scan(&point.Latitude, &point.Longitude, &timestamp); err != nil {
			return nil, err
		}
		if timestamp.Valid {
			point.Timestamp = &timestamp.Int64
		}
		path = append(path, point)
	}
	return path, rows.Err()
}

// ---- delete and restore, shared ----

var entityTables = map[string]string{EntityPlace: "places", EntityVisit: "visits", EntityTrip: "trips"}

func deletedAtOf(tx *sql.Tx, entityType, id string) (int64, error) {
	var deletedAt int64
	err := tx.QueryRow(`SELECT deleted_at FROM `+entityTables[entityType]+` WHERE id = ?`, id).Scan(&deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: no %s %s", ErrNotFound, entityType, id)
	}
	return deletedAt, err
}

// Delete stamps deleted_at on a place, visit or trip; again changes nothing.
func (s *Store) Delete(entityType, id string) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		deletedAt, err := deletedAtOf(tx, entityType, id)
		if err != nil || deletedAt != 0 {
			return err
		}
		at := now()
		if err := snapshot(tx, entityType, id, "delete", at); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE `+entityTables[entityType]+` SET deleted_at=?, updated_at=? WHERE id=?`, at, at, id)
		return err
	})
}

// Restore brings a deleted place, visit or trip back as it was.
func (s *Store) Restore(entityType, id string) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		deletedAt, err := deletedAtOf(tx, entityType, id)
		if err != nil || deletedAt == 0 {
			return err
		}
		at := now()
		if err := snapshot(tx, entityType, id, "restore", at); err != nil {
			return err
		}
		if entityType == EntityPlace {
			_, err = tx.Exec(`UPDATE places SET deleted_at=0, merged_into_place_id=NULL, updated_at=? WHERE id=?`, at, id)
			return err
		}
		// Brought back by hand, a record a newer export set aside stays: the
		// "restore" revision marks it as someone's choice.
		_, err = tx.Exec(`UPDATE `+entityTables[entityType]+` SET deleted_at=0, superseded_by_import_id=NULL, updated_at=? WHERE id=?`, at, id)
		return err
	})
}

// ---- timeline and points ----

const (
	defaultTimelineLimit = 500
	maximumTimelineLimit = 5000
)

// Timeline is the visits and trips overlapping [from, to), interleaved by
// start time. Trips that have children are left out unless includeParentTrips,
// so a road trip does not stand beside its own drives.
func (s *Store) Timeline(from, to int64, source string, includeParentTrips bool, limit int) ([]TimelineItem, error) {
	limit = clampLimit(limit, defaultTimelineLimit, maximumTimelineLimit)
	filter := TimeFilter{From: from, To: to, Source: source, Limit: limit}
	visits, err := s.listVisitsUncapped(VisitFilter{TimeFilter: filter})
	if err != nil {
		return nil, err
	}
	trips, err := s.listTripsUncapped(TripFilter{TimeFilter: filter, ExcludeParents: !includeParentTrips})
	if err != nil {
		return nil, err
	}
	items := make([]TimelineItem, 0, len(visits)+len(trips))
	for index := range visits {
		items = append(items, TimelineItem{Kind: EntityVisit, Visit: &visits[index]})
	}
	for index := range trips {
		items = append(items, TimelineItem{Kind: EntityTrip, Trip: &trips[index]})
	}
	startOf := func(item TimelineItem) (int64, int64) {
		if item.Visit != nil {
			return item.Visit.StartedAt, item.Visit.EndedAt
		}
		return item.Trip.StartedAt, item.Trip.EndedAt
	}
	sort.SliceStable(items, func(a, b int) bool {
		startA, endA := startOf(items[a])
		startB, endB := startOf(items[b])
		if startA != startB {
			return startA < startB
		}
		return endA < endB
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

// The timeline asks each list for up to its own limit, which may be above the
// lists' usual maximum.
func (s *Store) listVisitsUncapped(filter VisitFilter) ([]Visit, error) {
	var all []Visit
	for offset := 0; offset < filter.Limit; offset += maximumLimit {
		page := filter
		page.Limit, page.Offset = min(maximumLimit, filter.Limit-offset), offset
		visits, err := s.ListVisits(page)
		if err != nil {
			return nil, err
		}
		all = append(all, visits...)
		if len(visits) < page.Limit {
			break
		}
	}
	return all, nil
}

func (s *Store) listTripsUncapped(filter TripFilter) ([]Trip, error) {
	var all []Trip
	for offset := 0; offset < filter.Limit; offset += maximumLimit {
		page := filter
		page.Limit, page.Offset = min(maximumLimit, filter.Limit-offset), offset
		trips, err := s.ListTrips(page)
		if err != nil {
			return nil, err
		}
		all = append(all, trips...)
		if len(trips) < page.Limit {
			break
		}
	}
	return all, nil
}

const (
	defaultPointLimit = 1000
	maximumPointLimit = 10000
)

// PointFilter selects points for GET /points, earliest first. From is
// inclusive, To exclusive; 0 leaves that side open.
type PointFilter struct {
	From, To      int64
	Source        string
	SourceRef     string
	Limit, Offset int
}

// ListPoints answers GET /points.
func (s *Store) ListPoints(filter PointFilter) ([]Point, error) {
	where, args := []string{}, []any{}
	if filter.From > 0 {
		where, args = append(where, "timestamp >= ?"), append(args, filter.From)
	}
	if filter.To > 0 {
		where, args = append(where, "timestamp < ?"), append(args, filter.To)
	}
	if filter.Source != "" {
		if !knownWord(PointSources, filter.Source) {
			return nil, fmt.Errorf("%w: unknown point source %q: use one of %s", ErrInvalid, filter.Source, strings.Join(PointSources, ", "))
		}
		where, args = append(where, "source = ?"), append(args, filter.Source)
	}
	if filter.SourceRef != "" {
		where, args = append(where, "source_ref = ?"), append(args, filter.SourceRef)
	}
	query := `SELECT timestamp, latitude, longitude, accuracy_meters, source, source_ref, import_id FROM points`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` ORDER BY timestamp, rowid LIMIT ? OFFSET ?`
	args = append(args, clampLimit(filter.Limit, defaultPointLimit, maximumPointLimit), max(filter.Offset, 0))
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []Point{}
	for rows.Next() {
		var point Point
		var accuracy sql.NullFloat64
		if err := rows.Scan(&point.Timestamp, &point.Latitude, &point.Longitude, &accuracy, &point.Source, &point.SourceRef, &point.ImportID); err != nil {
			return nil, err
		}
		point.AccuracyMeters = floatPointer(accuracy)
		points = append(points, point)
	}
	return points, rows.Err()
}
