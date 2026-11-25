package locationstore

import (
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/kayushkin/location-store/internal/geonames"
)

// GeonamesDir is where location-name-localities downloads the GeoNames files:
// <data dir>/geonames.
func (s *Store) GeonamesDir() string { return GeonamesDirIn(s.dataDir) }

// GeonamesDirIn is the GeoNames directory of a data directory.
func GeonamesDirIn(dataDir string) string { return filepath.Join(dataDir, "geonames") }

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

// FillLocalities names the town of every place, visit and trip end whose
// locality is empty. It never changes one already set. Each record it fills
// is first copied to revisions, reason "locality".
func (s *Store) FillLocalities(dryRun bool) (*LocalityFill, error) {
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
	err = s.inTransaction(func(tx *sql.Tx) error {
		var work []pending
		var rows *sql.Rows
		var err error
		collect := func(entityType, column, query string, counter *int) error {
			if rows, err = tx.Query(query); err != nil {
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
		if err := collect(EntityPlace, "locality", `SELECT id, locality IS NOT NULL, latitude, longitude FROM places WHERE deleted_at = 0`, &fill.Places); err != nil {
			return err
		}
		if err := collect(EntityVisit, "locality", `SELECT v.id, v.locality IS NOT NULL, COALESCE(v.latitude, p.latitude), COALESCE(v.longitude, p.longitude)
			FROM visits v LEFT JOIN places p ON p.id = v.place_id WHERE v.deleted_at = 0`, &fill.Visits); err != nil {
			return err
		}
		if err := collect(EntityTrip, "start_locality", `SELECT id, start_locality IS NOT NULL, start_latitude, start_longitude FROM trips WHERE deleted_at = 0`, &fill.TripStarts); err != nil {
			return err
		}
		if err := collect(EntityTrip, "end_locality", `SELECT id, end_locality IS NOT NULL, end_latitude, end_longitude FROM trips WHERE deleted_at = 0`, &fill.TripEnds); err != nil {
			return err
		}
		if dryRun {
			return nil
		}
		at := now()
		for _, item := range work {
			if err := snapshot(tx, item.entityType, item.id, "locality", at); err != nil {
				return err
			}
			// The column is NULL-guarded again, so a value set since the read
			// above is never replaced.
			if _, err := tx.Exec(`UPDATE `+entityTables[item.entityType]+` SET `+item.column+`=?, updated_at=? WHERE id=? AND `+item.column+` IS NULL`,
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
