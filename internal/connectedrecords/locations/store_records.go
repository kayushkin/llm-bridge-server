package locations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
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

const placeColumns = `id, owner_principal_id, name, latitude, longitude, radius_meters, address, kind, google_place_id, description,
	described_by, tags::text, source, import_id, locality, created_at, updated_at, deleted_at, merged_into_place_id`

func scanPlace(scan func(...any) error) (*Place, error) {
	var place Place
	var googlePlaceID, importID, locality, mergedInto sql.NullString
	var tags string
	if err := scan(&place.ID, &place.OwnerPrincipalID, &place.Name, &place.Latitude, &place.Longitude, &place.RadiusMeters, &place.Address, &place.Kind,
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

func getPlaceRow(ctx context.Context, q queryer, scope ownership.Scope, id string) (*Place, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	var args arguments
	place, err := scanPlace(q.QueryRowContext(ctx, `SELECT `+placeColumns+` FROM places WHERE id = `+args.add(id)+` AND `+args.within(scope, "owner_principal_id"),
		args.values...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no place %s", ErrNotFound, id)
	}
	return place, err
}

// GetPlace reads one place. A deleted place is not found unless asked for.
func (s *Store) GetPlace(ctx context.Context, scope ownership.Scope, id string, includeDeleted bool) (*Place, error) {
	place, err := getPlaceRow(ctx, s.db, scope, id)
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

// CreatePlace makes a place by hand, owned by owner.
func (s *Store) CreatePlace(ctx context.Context, owner string, create PlaceCreate) (*Place, error) {
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
	err = s.inOwnerTransaction(ctx, owner, func(tx *sql.Tx) error {
		var seq int64
		var err error
		if id, seq, err = nextID(ctx, tx, "places", "place"); err != nil {
			return err
		}
		at := now()
		_, err = tx.ExecContext(ctx, `INSERT INTO places (id, seq, owner_principal_id, name, latitude, longitude, radius_meters, address, kind, description,
			described_by, tags, source, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13, $14, $15)`,
			id, seq, owner, place.Name, place.Latitude, place.Longitude, place.RadiusMeters, place.Address, place.Kind,
			place.Description, place.DescribedBy, encodeStrings(place.Tags), SourceManual, at, at)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetPlace(ctx, ownership.OwnedBy(owner), id, false)
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

// ListPlaces answers GET /places: the places in scope.
func (s *Store) ListPlaces(ctx context.Context, scope ownership.Scope, filter PlaceFilter) ([]Place, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	var args arguments
	where := []string{args.within(scope, "owner_principal_id")}
	if !filter.IncludeDeleted {
		where = append(where, "deleted_at = 0")
	}
	for _, word := range strings.Fields(filter.Query) {
		where = append(where, `(`+containsText("name", &args, word)+` OR `+containsText("address", &args, word)+` OR `+
			containsText("description", &args, word)+` OR `+containsText("tags::text", &args, word)+`)`)
	}
	if filter.Kind != "" {
		if !knownWord(PlaceKinds, filter.Kind) {
			return nil, fmt.Errorf("%w: unknown kind %q: use one of %s", ErrInvalid, filter.Kind, strings.Join(PlaceKinds, ", "))
		}
		where = append(where, "kind = "+args.add(filter.Kind))
	}
	if filter.Tag != "" {
		where = append(where, "tags @> jsonb_build_array("+args.add(filter.Tag)+"::text)")
	}
	if filter.GooglePlaceID != "" {
		where = append(where, "google_place_id = "+args.add(filter.GooglePlaceID))
	}
	query := `SELECT ` + placeColumns + ` FROM places WHERE ` + strings.Join(where, " AND ") + ` ORDER BY seq LIMIT ` +
		args.add(clampLimit(filter.Limit, defaultLimit, maximumLimit)) + ` OFFSET ` + args.add(max(filter.Offset, 0))
	rows, err := s.db.QueryContext(ctx, query, args.values...)
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

// PatchPlace changes the fields the patch names on a live place in scope.
func (s *Store) PatchPlace(ctx context.Context, scope ownership.Scope, id string, patch PlacePatch) (*Place, error) {
	err := s.writeOwned(ctx, scope, "places", EntityPlace, id, func(tx *sql.Tx, owner string) error {
		place, err := getPlaceRow(ctx, tx, ownership.OwnedBy(owner), id)
		if err != nil {
			return err
		}
		if place.DeletedAt != 0 {
			return fmt.Errorf("%w: place %s is deleted; restore it first", ErrNotFound, id)
		}
		at := now()
		if err := snapshot(ctx, tx, EntityPlace, id, "update", at); err != nil {
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
		_, err = tx.ExecContext(ctx, `UPDATE places SET name = $1, latitude = $2, longitude = $3, radius_meters = $4, address = $5, kind = $6,
			description = $7, described_by = $8, tags = $9::jsonb, updated_at = $10 WHERE id = $11`, place.Name, place.Latitude, place.Longitude,
			place.RadiusMeters, place.Address, place.Kind, place.Description, place.DescribedBy, encodeStrings(place.Tags), at, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetPlace(ctx, scope, id, false)
}

// ---- visits ----

const visitSelect = `SELECT v.id, v.owner_principal_id, v.place_id, COALESCE(p.name, ''), v.started_at, v.ended_at, v.latitude, v.longitude,
	v.probability, v.source_semantic_type, v.description, v.described_by, v.tags::text, v.source, v.source_ref, v.import_id,
	v.superseded_by_import_id, v.locality, v.created_at, v.updated_at, v.deleted_at FROM visits v LEFT JOIN places p ON p.id = v.place_id`

func scanVisit(scan func(...any) error) (*Visit, error) {
	var visit Visit
	var placeID, importID, supersededBy, locality sql.NullString
	var latitude, longitude, probability sql.NullFloat64
	var tags string
	if err := scan(&visit.ID, &visit.OwnerPrincipalID, &placeID, &visit.PlaceName, &visit.StartedAt, &visit.EndedAt, &latitude, &longitude,
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

func getVisitRow(ctx context.Context, q queryer, scope ownership.Scope, id string) (*Visit, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	var args arguments
	visit, err := scanVisit(q.QueryRowContext(ctx, visitSelect+` WHERE v.id = `+args.add(id)+` AND `+args.within(scope, "v.owner_principal_id"),
		args.values...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no visit %s", ErrNotFound, id)
	}
	return visit, err
}

// GetVisit reads one visit. A deleted visit is not found unless asked for.
func (s *Store) GetVisit(ctx context.Context, scope ownership.Scope, id string, includeDeleted bool) (*Visit, error) {
	visit, err := getVisitRow(ctx, s.db, scope, id)
	if err != nil {
		return nil, err
	}
	if visit.DeletedAt != 0 && !includeDeleted {
		return nil, fmt.Errorf("%w: visit %s is deleted", ErrNotFound, id)
	}
	return visit, nil
}

// requirePlace refuses an id that names no place owner keeps: one that does
// not exist or is outside scope is a 400, as a missing place always was, and
// one of another owner's that scope reaches is a 400 too. A deleted place may
// still be linked: the link is history, not an endorsement.
func requirePlace(ctx context.Context, q queryer, scope ownership.Scope, owner string, id *string, field string) error {
	if id == nil {
		return nil
	}
	return requireOwner(ctx, q, scope, owner, "places", EntityPlace, *id, ErrInvalid, field)
}

// CreateVisit makes a visit by hand, owned by owner. Its place must be in
// scope and owner's.
func (s *Store) CreateVisit(ctx context.Context, scope ownership.Scope, owner string, create VisitCreate) (*Visit, error) {
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
	err = s.inOwnerTransaction(ctx, owner, func(tx *sql.Tx) error {
		if err := requirePlace(ctx, tx, scope, owner, create.PlaceID, "place_id"); err != nil {
			return err
		}
		var seq int64
		var err error
		if id, seq, err = nextID(ctx, tx, "visits", "visit"); err != nil {
			return err
		}
		at := now()
		_, err = tx.ExecContext(ctx, `INSERT INTO visits (id, seq, owner_principal_id, place_id, started_at, ended_at, latitude, longitude, description,
			described_by, tags, source, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12, $13, $14)`,
			id, seq, owner, nullableString(create.PlaceID), create.StartedAt, create.EndedAt, nullableFloat(create.Latitude),
			nullableFloat(create.Longitude), description, describedBy, encodeStrings(tags), SourceManual, at, at)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetVisit(ctx, ownership.OwnedBy(owner), id, false)
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

func (filter TimeFilter) clauses(alias string, args *arguments) []string {
	where := []string{}
	if !filter.IncludeDeleted {
		where = append(where, alias+".deleted_at = 0")
	}
	if filter.From > 0 {
		where = append(where, alias+".ended_at >= "+args.add(filter.From))
	}
	if filter.To > 0 {
		where = append(where, alias+".started_at < "+args.add(filter.To))
	}
	if filter.Source != "" {
		where = append(where, alias+".source = "+args.add(filter.Source))
	}
	if filter.Tag != "" {
		where = append(where, alias+".tags @> jsonb_build_array("+args.add(filter.Tag)+"::text)")
	}
	return where
}

// VisitFilter selects visits for GET /visits, earliest first.
type VisitFilter struct {
	TimeFilter
	PlaceID string
}

// ListVisits answers GET /visits: the visits in scope.
func (s *Store) ListVisits(ctx context.Context, scope ownership.Scope, filter VisitFilter) ([]Visit, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	var args arguments
	where := append([]string{args.within(scope, "v.owner_principal_id")}, filter.clauses("v", &args)...)
	if filter.PlaceID != "" {
		where = append(where, "v.place_id = "+args.add(filter.PlaceID))
	}
	query := visitSelect + " WHERE " + strings.Join(where, " AND ") + ` ORDER BY v.started_at, v.seq LIMIT ` +
		args.add(clampLimit(filter.Limit, defaultLimit, maximumLimit)) + ` OFFSET ` + args.add(max(filter.Offset, 0))
	return queryVisits(ctx, s.db, query, args.values...)
}

func queryVisits(ctx context.Context, q queryer, query string, args ...any) ([]Visit, error) {
	rows, err := q.QueryContext(ctx, query, args...)
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

// PatchVisit changes the fields the patch names on a live visit in scope.
func (s *Store) PatchVisit(ctx context.Context, scope ownership.Scope, id string, patch VisitPatch) (*Visit, error) {
	err := s.writeOwned(ctx, scope, "visits", EntityVisit, id, func(tx *sql.Tx, owner string) error {
		visit, err := getVisitRow(ctx, tx, ownership.OwnedBy(owner), id)
		if err != nil {
			return err
		}
		if visit.DeletedAt != 0 {
			return fmt.Errorf("%w: visit %s is deleted; restore it first", ErrNotFound, id)
		}
		at := now()
		if patch.PlaceID.Set {
			if err := requirePlace(ctx, tx, scope, owner, patch.PlaceID.Value, "place_id"); err != nil {
				return err
			}
		}
		if err := snapshot(ctx, tx, EntityVisit, id, "update", at); err != nil {
			return err
		}
		if patch.PlaceID.Set {
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
		_, err = tx.ExecContext(ctx, `UPDATE visits SET place_id = $1, description = $2, described_by = $3, tags = $4::jsonb, updated_at = $5 WHERE id = $6`,
			nullableString(visit.PlaceID), visit.Description, visit.DescribedBy, encodeStrings(visit.Tags), at, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetVisit(ctx, scope, id, false)
}

// ---- trips ----

const tripSelect = `SELECT t.id, t.owner_principal_id, t.kind, t.parent_trip_id, t.parent_set_by, t.started_at, t.ended_at, t.start_latitude, t.start_longitude,
	t.end_latitude, t.end_longitude, t.start_place_id, COALESCE(sp.name, ''), t.end_place_id, COALESCE(ep.name, ''),
	t.mode, t.source_mode, t.distance_meters, t.probability, t.title, t.title_by, t.description, t.described_by, t.tags::text,
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
	if err := scan(&trip.ID, &trip.OwnerPrincipalID, &trip.Kind, &parentTripID, &trip.ParentSetBy, &trip.StartedAt, &trip.EndedAt, &startLatitude, &startLongitude,
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

func getTripRow(ctx context.Context, q queryer, scope ownership.Scope, id string) (*Trip, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	var args arguments
	trip, err := scanTrip(q.QueryRowContext(ctx, tripSelect+` WHERE t.id = `+args.add(id)+` AND `+args.within(scope, "t.owner_principal_id"),
		args.values...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no trip %s", ErrNotFound, id)
	}
	return trip, err
}

func queryTrips(ctx context.Context, q queryer, query string, args ...any) ([]Trip, error) {
	rows, err := q.QueryContext(ctx, query, args...)
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

// GetTrip reads one trip. A deleted trip is not found unless asked for.
func (s *Store) GetTrip(ctx context.Context, scope ownership.Scope, id string, includeDeleted bool) (*Trip, error) {
	trip, err := getTripRow(ctx, s.db, scope, id)
	if err != nil {
		return nil, err
	}
	if trip.DeletedAt != 0 && !includeDeleted {
		return nil, fmt.Errorf("%w: trip %s is deleted", ErrNotFound, id)
	}
	return trip, nil
}

// checkParent refuses a parent that does not exist, is outside scope or
// another owner's, is deleted, or would make a loop: the child itself or one
// of its own descendants.
func checkParent(ctx context.Context, q queryer, scope ownership.Scope, owner, childID string, parentID *string) error {
	if parentID == nil {
		return nil
	}
	if err := requireOwner(ctx, q, scope, owner, "trips", EntityTrip, *parentID, ErrInvalid, "parent_trip_id"); err != nil {
		return err
	}
	owned := ownership.OwnedBy(owner)
	parent, err := getTripRow(ctx, q, owned, *parentID)
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
		if ancestor, err = getTripRow(ctx, q, owned, *ancestor.ParentTripID); err != nil {
			return err
		}
	}
}

// CreateTrip makes a trip by hand, owned by owner: usually a road trip or
// holiday that imported trips are then attached to. Its places and parent
// must be in scope and owner's.
func (s *Store) CreateTrip(ctx context.Context, scope ownership.Scope, owner string, create TripCreate) (*Trip, error) {
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
	err = s.inOwnerTransaction(ctx, owner, func(tx *sql.Tx) error {
		if err := requirePlace(ctx, tx, scope, owner, create.StartPlaceID, "start_place_id"); err != nil {
			return err
		}
		if err := requirePlace(ctx, tx, scope, owner, create.EndPlaceID, "end_place_id"); err != nil {
			return err
		}
		var seq int64
		var err error
		if id, seq, err = nextID(ctx, tx, "trips", "trip"); err != nil {
			return err
		}
		if err := checkParent(ctx, tx, scope, owner, id, create.ParentTripID); err != nil {
			return err
		}
		at := now()
		parentSetBy := ""
		if create.ParentTripID != nil {
			parentSetBy = SetByHand
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO trips (id, seq, owner_principal_id, kind, parent_trip_id, parent_set_by, title_by, started_at, ended_at,
			start_latitude, start_longitude, end_latitude, end_longitude, start_place_id, end_place_id, mode, distance_meters, title, description,
			described_by, tags, source, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21::jsonb, $22, $23, $24)`,
			id, seq, owner, kind, nullableString(create.ParentTripID), parentSetBy, SetByHand, create.StartedAt, create.EndedAt,
			nullableFloat(create.StartLatitude), nullableFloat(create.StartLongitude), nullableFloat(create.EndLatitude),
			nullableFloat(create.EndLongitude), nullableString(create.StartPlaceID), nullableString(create.EndPlaceID),
			create.Mode, nullableFloat(create.DistanceMeters), title, description, describedBy, encodeStrings(tags),
			SourceManual, at, at)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetTrip(ctx, ownership.OwnedBy(owner), id, false)
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

// ListTrips answers GET /trips: the trips in scope.
func (s *Store) ListTrips(ctx context.Context, scope ownership.Scope, filter TripFilter) ([]Trip, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	var args arguments
	where := append([]string{args.within(scope, "t.owner_principal_id")}, filter.clauses("t", &args)...)
	if filter.Mode != "" {
		if !knownWord(TripModes, filter.Mode) {
			return nil, fmt.Errorf("%w: unknown mode %q: use one of %s", ErrInvalid, filter.Mode, strings.Join(TripModes, ", "))
		}
		where = append(where, "t.mode = "+args.add(filter.Mode))
	}
	if filter.Kind != "" {
		if !knownWord(TripKinds, filter.Kind) {
			return nil, fmt.Errorf("%w: unknown kind %q: use one of %s", ErrInvalid, filter.Kind, strings.Join(TripKinds, ", "))
		}
		where = append(where, "t.kind = "+args.add(filter.Kind))
	}
	switch filter.ParentTripID {
	case "":
	case "none":
		where = append(where, "t.parent_trip_id IS NULL")
	default:
		where = append(where, "t.parent_trip_id = "+args.add(filter.ParentTripID))
	}
	if filter.MinimumDistanceMeters > 0 {
		where = append(where, "t.distance_meters >= "+args.add(filter.MinimumDistanceMeters))
	}
	if filter.ExcludeParents {
		where = append(where, "NOT EXISTS (SELECT 1 FROM trips c WHERE c.parent_trip_id = t.id AND c.deleted_at = 0)")
	}
	query := tripSelect + " WHERE " + strings.Join(where, " AND ") + ` ORDER BY t.started_at, t.seq LIMIT ` +
		args.add(clampLimit(filter.Limit, defaultLimit, maximumLimit)) + ` OFFSET ` + args.add(max(filter.Offset, 0))
	return queryTrips(ctx, s.db, query, args.values...)
}

// PatchTrip changes the fields the patch names on a live trip in scope.
func (s *Store) PatchTrip(ctx context.Context, scope ownership.Scope, id string, patch TripPatch) (*Trip, error) {
	err := s.writeOwned(ctx, scope, "trips", EntityTrip, id, func(tx *sql.Tx, owner string) error {
		trip, err := getTripRow(ctx, tx, ownership.OwnedBy(owner), id)
		if err != nil {
			return err
		}
		if trip.DeletedAt != 0 {
			return fmt.Errorf("%w: trip %s is deleted; restore it first", ErrNotFound, id)
		}
		at := now()
		if err := snapshot(ctx, tx, EntityTrip, id, "update", at); err != nil {
			return err
		}
		if patch.ParentTripID.Set {
			if err := checkParent(ctx, tx, scope, owner, id, patch.ParentTripID.Value); err != nil {
				return err
			}
			trip.ParentTripID = patch.ParentTripID.Value
			trip.ParentSetBy = SetByHand
		}
		if patch.StartPlaceID.Set {
			if err := requirePlace(ctx, tx, scope, owner, patch.StartPlaceID.Value, "start_place_id"); err != nil {
				return err
			}
			trip.StartPlaceID = patch.StartPlaceID.Value
		}
		if patch.EndPlaceID.Set {
			if err := requirePlace(ctx, tx, scope, owner, patch.EndPlaceID.Value, "end_place_id"); err != nil {
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
		_, err = tx.ExecContext(ctx, `UPDATE trips SET parent_trip_id = $1, parent_set_by = $2, start_place_id = $3, end_place_id = $4, mode = $5, title = $6,
			title_by = $7, description = $8, described_by = $9, tags = $10::jsonb, updated_at = $11 WHERE id = $12`, nullableString(trip.ParentTripID),
			trip.ParentSetBy, nullableString(trip.StartPlaceID), nullableString(trip.EndPlaceID), trip.Mode, trip.Title, trip.TitleBy,
			trip.Description, trip.DescribedBy, encodeStrings(trip.Tags), at, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetTrip(ctx, scope, id, false)
}

// AttachChildren makes each named trip part of parentID, in one transaction:
// one bad id attaches none. The parent must be in scope; each child must be
// the parent's owner's. The routes call it; a trip attached so is placed by
// hand, and location-group-journeys leaves it there.
func (s *Store) AttachChildren(ctx context.Context, scope ownership.Scope, parentID string, childIDs []string) (*Trip, error) {
	if len(childIDs) == 0 {
		return nil, fmt.Errorf("%w: trip_ids is empty", ErrInvalid)
	}
	err := s.writeOwned(ctx, scope, "trips", EntityTrip, parentID, func(tx *sql.Tx, owner string) error {
		at := now()
		for _, childID := range childIDs {
			if err := requireOwner(ctx, tx, scope, owner, "trips", EntityTrip, childID, ErrInvalid, ""); err != nil {
				return err
			}
			child, err := getTripRow(ctx, tx, ownership.OwnedBy(owner), childID)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrInvalid, err)
			}
			if child.DeletedAt != 0 {
				return fmt.Errorf("%w: trip %s is deleted", ErrInvalid, childID)
			}
			if child.ParentTripID != nil && *child.ParentTripID == parentID && child.ParentSetBy == SetByHand {
				continue
			}
			if err := checkParent(ctx, tx, scope, owner, childID, &parentID); err != nil {
				return err
			}
			if err := snapshot(ctx, tx, EntityTrip, childID, "attach", at); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE trips SET parent_trip_id = $1, parent_set_by = $2, updated_at = $3 WHERE id = $4`,
				parentID, SetByHand, at, childID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetTrip(ctx, scope, parentID, false)
}

// TripPath is the path of a trip in scope, in order.
func (s *Store) TripPath(ctx context.Context, scope ownership.Scope, id string) ([]PathPoint, error) {
	if _, err := ownerOfRecord(ctx, s.db, scope, "trips", EntityTrip, id); err != nil {
		return nil, err
	}
	path, err := readPath(ctx, s.db, id)
	if path == nil {
		path = []PathPoint{}
	}
	return path, err
}

// ---- delete and restore, shared ----

func deletedAtOf(ctx context.Context, tx *sql.Tx, entityType, id string) (int64, error) {
	var deletedAt int64
	err := tx.QueryRowContext(ctx, `SELECT deleted_at FROM `+entityTables[entityType]+` WHERE id = $1`, id).Scan(&deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: no %s %s", ErrNotFound, entityType, id)
	}
	return deletedAt, err
}

// Delete stamps deleted_at on a place, visit or trip in scope; again changes
// nothing.
func (s *Store) Delete(ctx context.Context, scope ownership.Scope, entityType, id string) error {
	table, known := entityTables[entityType]
	if !known {
		return fmt.Errorf("delete of unknown entity type %q", entityType)
	}
	return s.writeOwned(ctx, scope, table, entityType, id, func(tx *sql.Tx, owner string) error {
		deletedAt, err := deletedAtOf(ctx, tx, entityType, id)
		if err != nil || deletedAt != 0 {
			return err
		}
		at := now()
		if err := snapshot(ctx, tx, entityType, id, "delete", at); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE `+table+` SET deleted_at = $1, updated_at = $2 WHERE id = $3`, at, at, id)
		return err
	})
}

// Restore brings a deleted place, visit or trip in scope back as it was.
func (s *Store) Restore(ctx context.Context, scope ownership.Scope, entityType, id string) error {
	table, known := entityTables[entityType]
	if !known {
		return fmt.Errorf("restore of unknown entity type %q", entityType)
	}
	return s.writeOwned(ctx, scope, table, entityType, id, func(tx *sql.Tx, owner string) error {
		deletedAt, err := deletedAtOf(ctx, tx, entityType, id)
		if err != nil || deletedAt == 0 {
			return err
		}
		at := now()
		if err := snapshot(ctx, tx, entityType, id, "restore", at); err != nil {
			return err
		}
		if entityType == EntityPlace {
			_, err = tx.ExecContext(ctx, `UPDATE places SET deleted_at = 0, merged_into_place_id = NULL, updated_at = $1 WHERE id = $2`, at, id)
			return err
		}
		// Brought back by hand, a record a newer export set aside stays: the
		// "restore" revision marks it as someone's choice.
		_, err = tx.ExecContext(ctx, `UPDATE `+table+` SET deleted_at = 0, superseded_by_import_id = NULL, updated_at = $1 WHERE id = $2`, at, id)
		return err
	})
}

// ---- timeline and points ----

const (
	defaultTimelineLimit = 500
	maximumTimelineLimit = 5000
)

// Timeline is the visits and trips in scope overlapping [from, to),
// interleaved by start time. Trips that have children are left out unless
// includeParentTrips, so a road trip does not stand beside its own drives.
func (s *Store) Timeline(ctx context.Context, scope ownership.Scope, from, to int64, source string, includeParentTrips bool, limit int) ([]TimelineItem, error) {
	limit = clampLimit(limit, defaultTimelineLimit, maximumTimelineLimit)
	filter := TimeFilter{From: from, To: to, Source: source, Limit: limit}
	visits, err := s.listVisitsUncapped(ctx, scope, VisitFilter{TimeFilter: filter})
	if err != nil {
		return nil, err
	}
	trips, err := s.listTripsUncapped(ctx, scope, TripFilter{TimeFilter: filter, ExcludeParents: !includeParentTrips})
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
func (s *Store) listVisitsUncapped(ctx context.Context, scope ownership.Scope, filter VisitFilter) ([]Visit, error) {
	var all []Visit
	for offset := 0; offset < filter.Limit; offset += maximumLimit {
		page := filter
		page.Limit, page.Offset = min(maximumLimit, filter.Limit-offset), offset
		visits, err := s.ListVisits(ctx, scope, page)
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

func (s *Store) listTripsUncapped(ctx context.Context, scope ownership.Scope, filter TripFilter) ([]Trip, error) {
	var all []Trip
	for offset := 0; offset < filter.Limit; offset += maximumLimit {
		page := filter
		page.Limit, page.Offset = min(maximumLimit, filter.Limit-offset), offset
		trips, err := s.ListTrips(ctx, scope, page)
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

// ListPoints answers GET /points: the points in scope.
func (s *Store) ListPoints(ctx context.Context, scope ownership.Scope, filter PointFilter) ([]Point, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	var args arguments
	where := []string{args.within(scope, "owner_principal_id")}
	if filter.From > 0 {
		where = append(where, "timestamp >= "+args.add(filter.From))
	}
	if filter.To > 0 {
		where = append(where, "timestamp < "+args.add(filter.To))
	}
	if filter.Source != "" {
		if !knownWord(PointSources, filter.Source) {
			return nil, fmt.Errorf("%w: unknown point source %q: use one of %s", ErrInvalid, filter.Source, strings.Join(PointSources, ", "))
		}
		where = append(where, "source = "+args.add(filter.Source))
	}
	if filter.SourceRef != "" {
		where = append(where, "source_ref = "+args.add(filter.SourceRef))
	}
	query := `SELECT timestamp, latitude, longitude, accuracy_meters, source, source_ref, import_id FROM points WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY timestamp, seq LIMIT ` + args.add(clampLimit(filter.Limit, defaultPointLimit, maximumPointLimit)) + ` OFFSET ` + args.add(max(filter.Offset, 0))
	rows, err := s.db.QueryContext(ctx, query, args.values...)
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
