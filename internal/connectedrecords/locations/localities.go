package locations

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/locations/internal/geonames"
)

// GeonamesDir is the directory the store reads the GeoNames files from, the
// one New was given, where location-name-localities downloads them.
func (s *Store) GeonamesDir() string { return s.geonamesDirectory }

// townIndex reads the GeoNames files once. Missing files are a conflict the
// caller fixes by running location-name-localities, which downloads them.
func (s *Store) townIndex() (*geonames.Index, error) {
	s.townsMutex.Lock()
	defer s.townsMutex.Unlock()
	if s.towns != nil {
		return s.towns, nil
	}
	dir := s.GeonamesDir()
	if !geonames.Present(dir) {
		return nil, fmt.Errorf("%w: the GeoNames files are not in %s: run location-name-localities, which downloads them", ErrConflict, dir)
	}
	index, err := geonames.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("read GeoNames files: %w", err)
	}
	s.towns = index
	return index, nil
}

// localityFor names the town a coordinate is in, or nil when none is near.
func localityFor(index *geonames.Index, latitude, longitude float64) *Locality {
	match, found := index.Nearest(latitude, longitude)
	if !found {
		return nil
	}
	return &Locality{City: match.Name, Region: match.Region, Country: match.Country, CountryCode: match.CountryCode,
		TimeZone: match.TimeZone, GeonamesID: match.GeonamesID, DistanceMeters: match.DistanceMeters,
		DescribedBy: SetByLocalityNaming + " (" + geonames.Source + ")"}
}

// FillLocalities names the town of every place, visit and trip end of
// owner's whose locality is empty. It never changes one already set. Each
// record it fills is first copied to revisions, reason "locality".
func (s *Store) FillLocalities(ctx context.Context, owner string, dryRun bool) (*LocalityFill, error) {
	index, err := s.townIndex()
	if err != nil {
		return nil, err
	}
	fill := &LocalityFill{DryRun: dryRun, GeonamesTownsIn: index.Cities}
	towns := map[int64]bool{}
	name := func(latitude, longitude sql.NullFloat64) *Locality {
		if !latitude.Valid || !longitude.Valid {
			fill.NoCoordinate++
			return nil
		}
		locality := localityFor(index, latitude.Float64, longitude.Float64)
		if locality == nil {
			fill.NoTownNearby++
			return nil
		}
		towns[locality.GeonamesID] = true
		return locality
	}
	type pending struct {
		entityType, id, column string
		locality               *Locality
	}
	err = s.inOwnerTransaction(ctx, owner, func(tx *sql.Tx) error {
		var work []pending
		var rows *sql.Rows
		var err error
		collect := func(entityType, column, query string, counter *int) error {
			if rows, err = tx.QueryContext(ctx, query, owner); err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				var named bool
				var latitude, longitude sql.NullFloat64
				if err := rows.Scan(&id, &named, &latitude, &longitude); err != nil {
					return err
				}
				if named {
					fill.AlreadyNamed++
					continue
				}
				if locality := name(latitude, longitude); locality != nil {
					work = append(work, pending{entityType, id, column, locality})
					*counter++
				}
			}
			return rows.Err()
		}
		if err := collect(EntityPlace, "locality", `SELECT id, locality IS NOT NULL, latitude, longitude FROM places WHERE owner_principal_id = $1 AND deleted_at = 0 ORDER BY seq`, &fill.Places); err != nil {
			return err
		}
		if err := collect(EntityVisit, "locality", `SELECT v.id, v.locality IS NOT NULL, COALESCE(v.latitude, p.latitude), COALESCE(v.longitude, p.longitude)
			FROM visits v LEFT JOIN places p ON p.id = v.place_id WHERE v.owner_principal_id = $1 AND v.deleted_at = 0 ORDER BY v.seq`, &fill.Visits); err != nil {
			return err
		}
		if err := collect(EntityTrip, "start_locality", `SELECT id, start_locality IS NOT NULL, start_latitude, start_longitude FROM trips WHERE owner_principal_id = $1 AND deleted_at = 0 ORDER BY seq`, &fill.TripStarts); err != nil {
			return err
		}
		if err := collect(EntityTrip, "end_locality", `SELECT id, end_locality IS NOT NULL, end_latitude, end_longitude FROM trips WHERE owner_principal_id = $1 AND deleted_at = 0 ORDER BY seq`, &fill.TripEnds); err != nil {
			return err
		}
		if dryRun {
			return nil
		}
		at := now()
		for _, item := range work {
			if err := snapshot(ctx, tx, item.entityType, item.id, "locality", at); err != nil {
				return err
			}
			// The column is NULL-guarded again, so a value set since the read
			// above is never replaced.
			if _, err := tx.ExecContext(ctx, `UPDATE `+entityTables[item.entityType]+` SET `+item.column+` = $1, updated_at = $2 WHERE id = $3 AND `+item.column+` IS NULL`,
				encodeLocality(item.locality), at, item.id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	fill.DistinctTowns = len(towns)
	return fill, nil
}
