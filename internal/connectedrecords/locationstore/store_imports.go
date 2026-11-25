package locationstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const importColumns = `id, kind, sha256, source_machine, source_path, imported_at, finished_at, counts, snapshot, snapshot_changes, recorded_by, seq`

// importRow is an import with its seq, which orders snapshots that tie.
type importRow struct {
	Import
	seq int64
}

func scanImportRow(scan func(...any) error) (*importRow, error) {
	var record importRow
	var counts string
	var snapshot, changes sql.NullString
	if err := scan(&record.ID, &record.Kind, &record.Sha256, &record.SourceMachine, &record.SourcePath,
		&record.ImportedAt, &record.FinishedAt, &counts, &snapshot, &changes, &record.RecordedBy, &record.seq); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(counts), &record.Counts); err != nil {
		return nil, fmt.Errorf("import %s has counts that are not JSON: %w", record.ID, err)
	}
	if snapshot.Valid {
		record.Snapshot = &ImportSnapshot{}
		if err := json.Unmarshal([]byte(snapshot.String), record.Snapshot); err != nil {
			return nil, fmt.Errorf("import %s has a snapshot that is not JSON: %w", record.ID, err)
		}
	}
	if changes.Valid {
		record.SnapshotChanges = &SnapshotChanges{}
		if err := json.Unmarshal([]byte(changes.String), record.SnapshotChanges); err != nil {
			return nil, fmt.Errorf("import %s has snapshot changes that are not JSON: %w", record.ID, err)
		}
	}
	return &record, nil
}

func scanImport(scan func(...any) error) (*Import, error) {
	record, err := scanImportRow(scan)
	if err != nil {
		return nil, err
	}
	return &record.Import, nil
}

func getImportRow(q queryer, id string) (*Import, error) {
	record, err := scanImport(q.QueryRow(`SELECT `+importColumns+` FROM imports WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no import %s", ErrNotFound, id)
	}
	return record, err
}

// GetImport reads one import.
func (s *Store) GetImport(id string) (*Import, error) { return getImportRow(s.db, id) }

// ListImports is every import, newest first.
func (s *Store) ListImports(limit, offset int) ([]Import, error) {
	rows, err := s.db.Query(`SELECT `+importColumns+` FROM imports ORDER BY seq DESC LIMIT ? OFFSET ?`,
		clampLimit(limit, defaultLimit, maximumLimit), max(offset, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	imports := []Import{}
	for rows.Next() {
		record, err := scanImport(rows.Scan)
		if err != nil {
			return nil, err
		}
		imports = append(imports, *record)
	}
	return imports, rows.Err()
}

// StartImport begins an import of one file, found again by its hash: a file
// imported before answers AlreadyFinished, and one whose import stopped part
// way answers that import again so the importer can send it all again (every
// batch is idempotent).
func (s *Store) StartImport(start ImportStart) (*ImportStarted, error) {
	switch {
	case !knownWord(ImportKinds, start.Kind):
		return nil, fmt.Errorf("%w: unknown kind %q: use one of %s", ErrInvalid, start.Kind, strings.Join(ImportKinds, ", "))
	case !sha256Pattern.MatchString(start.Sha256):
		return nil, fmt.Errorf("%w: sha256 must be 64 lowercase hex characters", ErrInvalid)
	}
	var id string
	err := s.inTransaction(func(tx *sql.Tx) error {
		var kind string
		var finishedAt int64
		err := tx.QueryRow(`SELECT id, kind, finished_at FROM imports WHERE sha256 = ?`, start.Sha256).Scan(&id, &kind, &finishedAt)
		if err == nil {
			if kind != start.Kind {
				return fmt.Errorf("%w: this file was imported as %s (%s), not %s", ErrConflict, kind, id, start.Kind)
			}
			if start.Reapply && finishedAt != 0 {
				// Open it again; what it holds is recorded afresh as it is sent.
				if _, err := tx.Exec(`DELETE FROM import_records WHERE import_id = ?`, id); err != nil {
					return err
				}
				_, err = tx.Exec(`UPDATE imports SET finished_at = 0, counts = '{}', snapshot_changes = NULL WHERE id = ?`, id)
				return err
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var seq int64
		if id, seq, err = nextID(tx, "imports", "import"); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO imports (id, seq, kind, sha256, source_machine, source_path, imported_at) VALUES (?,?,?,?,?,?,?)`,
			id, seq, start.Kind, start.Sha256, strings.TrimSpace(start.SourceMachine), strings.TrimSpace(start.SourcePath), now())
		return err
	})
	if err != nil {
		return nil, err
	}
	record, err := s.GetImport(id)
	if err != nil {
		return nil, err
	}
	return &ImportStarted{Import: *record, AlreadyFinished: record.FinishedAt != 0}, nil
}

// FinishImport stamps finished_at, after which the same file is not imported
// again. Again changes nothing. With a snapshot, the import is a full
// snapshot of its span, and the snapshots are reconciled in the same
// transaction (reconcileSnapshots).
func (s *Store) FinishImport(id string, finish ImportFinish) (*Import, error) {
	if finish.Snapshot != nil {
		if err := checkTimeRange(finish.Snapshot.CoversFrom, finish.Snapshot.CoversTo); err != nil {
			return nil, fmt.Errorf("snapshot: %w", err)
		}
		if finish.Snapshot.RawSignalsNewestAt < finish.Snapshot.RawSignalsOldestAt {
			return nil, fmt.Errorf("%w: snapshot: raw_signals_newest_at is before raw_signals_oldest_at", ErrInvalid)
		}
	}
	err := s.inTransaction(func(tx *sql.Tx) error {
		record, err := getImportRow(tx, id)
		if err != nil || record.FinishedAt != 0 {
			return err
		}
		if finish.Snapshot != nil {
			if record.Kind != ImportKindGoogleTimeline {
				return fmt.Errorf("%w: only an on-device Timeline export (%s) is a snapshot; %s is %s", ErrInvalid, ImportKindGoogleTimeline, id, record.Kind)
			}
			encoded, err := json.Marshal(finish.Snapshot)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE imports SET snapshot=?, finished_at=? WHERE id=?`, string(encoded), now(), id); err != nil {
				return err
			}
			changes, err := reconcileSnapshots(tx, now())
			if err != nil {
				return err
			}
			encoded, err = json.Marshal(changes)
			if err != nil {
				return err
			}
			_, err = tx.Exec(`UPDATE imports SET snapshot_changes=? WHERE id=?`, string(encoded), id)
			return err
		}
		_, err = tx.Exec(`UPDATE imports SET finished_at=? WHERE id=?`, now(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetImport(id)
}

func importedSource(where, source string) error {
	if !knownWord(RecordSources, source) || source == SourceManual {
		return fmt.Errorf("%w: %s: source %q is not an import source: use one of %s", ErrInvalid, where, source, strings.Join(RecordSources, ", "))
	}
	return nil
}

func validateBatch(batch ImportBatch) error {
	if len(batch.Places)+len(batch.Visits)+len(batch.Trips)+len(batch.Points) == 0 {
		return fmt.Errorf("%w: the batch is empty", ErrInvalid)
	}
	for index, place := range batch.Places {
		where := fmt.Sprintf("place %d", index)
		switch {
		case strings.TrimSpace(place.GooglePlaceID) == "":
			return fmt.Errorf("%w: %s: google_place_id is required: an imported place is found again by it", ErrInvalid, where)
		case !validLatitudeLongitude(place.Latitude, place.Longitude):
			return fmt.Errorf("%w: %s: latitude or longitude out of range", ErrInvalid, where)
		case !knownWord(PlaceKinds, place.Kind):
			return fmt.Errorf("%w: %s: unknown kind %q: use one of %s", ErrInvalid, where, place.Kind, strings.Join(PlaceKinds, ", "))
		}
		if err := importedSource(where, place.Source); err != nil {
			return err
		}
	}
	for index, visit := range batch.Visits {
		where := fmt.Sprintf("visit %d", index)
		if err := importedSource(where, visit.Source); err != nil {
			return err
		}
		if strings.TrimSpace(visit.SourceRef) == "" {
			return fmt.Errorf("%w: %s: source_ref is required: an imported visit is found again by it", ErrInvalid, where)
		}
		for _, err := range []error{checkTimeRange(visit.StartedAt, visit.EndedAt), checkCoordinatePair("", visit.Latitude, visit.Longitude), checkProbability(visit.Probability)} {
			if err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		}
	}
	for index, trip := range batch.Trips {
		where := fmt.Sprintf("trip %d", index)
		if err := importedSource(where, trip.Source); err != nil {
			return err
		}
		if strings.TrimSpace(trip.SourceRef) == "" {
			return fmt.Errorf("%w: %s: source_ref is required: an imported trip is found again by it", ErrInvalid, where)
		}
		if !knownWord(TripModes, trip.Mode) {
			return fmt.Errorf("%w: %s: unknown mode %q: use one of %s", ErrInvalid, where, trip.Mode, strings.Join(TripModes, ", "))
		}
		if trip.DistanceMeters != nil && *trip.DistanceMeters < 0 {
			return fmt.Errorf("%w: %s: distance_meters cannot be negative", ErrInvalid, where)
		}
		for _, err := range []error{checkTimeRange(trip.StartedAt, trip.EndedAt), checkCoordinatePair("start", trip.StartLatitude, trip.StartLongitude),
			checkCoordinatePair("end", trip.EndLatitude, trip.EndLongitude), checkProbability(trip.Probability)} {
			if err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		}
		for pointIndex, point := range trip.Path {
			if !validLatitudeLongitude(point.Latitude, point.Longitude) {
				return fmt.Errorf("%w: %s: path point %d: latitude or longitude out of range", ErrInvalid, where, pointIndex)
			}
		}
	}
	for index, point := range batch.Points {
		where := fmt.Sprintf("point %d", index)
		switch {
		case point.Timestamp <= 0:
			return fmt.Errorf("%w: %s: timestamp must be set (epoch seconds)", ErrInvalid, where)
		case !validLatitudeLongitude(point.Latitude, point.Longitude):
			return fmt.Errorf("%w: %s: latitude or longitude out of range", ErrInvalid, where)
		case !knownWord(PointSources, point.Source):
			return fmt.Errorf("%w: %s: unknown source %q: use one of %s", ErrInvalid, where, point.Source, strings.Join(PointSources, ", "))
		case point.AccuracyMeters != nil && *point.AccuracyMeters < 0:
			return fmt.Errorf("%w: %s: accuracy_meters cannot be negative", ErrInvalid, where)
		}
	}
	return nil
}

func placeIDByGoogleID(tx *sql.Tx, googlePlaceID string) (*string, error) {
	if googlePlaceID == "" {
		return nil, nil
	}
	var id string
	var mergedInto sql.NullString
	err := tx.QueryRow(`SELECT id, merged_into_place_id FROM places WHERE google_place_id = ?`, googlePlaceID).Scan(&id, &mergedInto)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// A place merged into another is found as the one it went into, so a
	// later export's visits there land on the place kept.
	if mergedInto.Valid {
		return &mergedInto.String, nil
	}
	return &id, nil
}

// ApplyBatch writes one batch of an import, in one transaction: one bad record
// writes nothing. Each record is found again by its key (a place by
// google_place_id, a visit or trip by source and source_ref, a point by time,
// place and source), so sending a batch twice changes nothing. An import never
// touches a name, title, description, tags or parent someone wrote, never
// changes a place link already set, never changes a mode someone corrected,
// and leaves a deleted record deleted.
func (s *Store) ApplyBatch(importID string, batch ImportBatch) (*ImportCounts, error) {
	if err := validateBatch(batch); err != nil {
		return nil, err
	}
	counts := &ImportCounts{}
	err := s.inTransaction(func(tx *sql.Tx) error {
		record, err := getImportRow(tx, importID)
		if err != nil {
			return err
		}
		if record.FinishedAt != 0 {
			return fmt.Errorf("%w: import %s is finished; a file is imported once", ErrConflict, importID)
		}
		at := now()
		for _, place := range batch.Places {
			if err := applyPlace(tx, importID, place, at, counts); err != nil {
				return err
			}
		}
		for _, visit := range batch.Visits {
			if err := applyVisit(tx, importID, visit, at, counts); err != nil {
				return err
			}
		}
		for _, trip := range batch.Trips {
			if err := applyTrip(tx, importID, trip, at, counts); err != nil {
				return err
			}
		}
		if err := applyPoints(tx, importID, batch.Points, counts); err != nil {
			return err
		}
		total := record.Counts
		total.PointsAdded += counts.PointsAdded
		total.PointsUnchanged += counts.PointsUnchanged
		total.PlacesCreated += counts.PlacesCreated
		total.PlacesUpdated += counts.PlacesUpdated
		total.VisitsCreated += counts.VisitsCreated
		total.VisitsUpdated += counts.VisitsUpdated
		total.TripsCreated += counts.TripsCreated
		total.TripsUpdated += counts.TripsUpdated
		total.RecordsUnchanged += counts.RecordsUnchanged
		encoded, err := json.Marshal(total)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE imports SET counts=? WHERE id=?`, string(encoded), importID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return counts, nil
}

// applyPlace creates a place the first time its Google id is seen. Later it
// only fills a blank name or address and turns kind other into what the
// source says; it never moves a place or overwrites what someone wrote.
func applyPlace(tx *sql.Tx, importID string, place ImportPlace, at int64, counts *ImportCounts) error {
	existingID, err := placeIDByGoogleID(tx, place.GooglePlaceID)
	if err != nil {
		return err
	}
	if existingID == nil {
		id, seq, err := nextID(tx, "places", "place")
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO places (id, seq, name, latitude, longitude, address, kind, google_place_id, source, import_id,
			created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, id, seq, strings.TrimSpace(place.Name), place.Latitude,
			place.Longitude, strings.TrimSpace(place.Address), place.Kind, place.GooglePlaceID, place.Source, importID, at, at)
		if err == nil {
			counts.PlacesCreated++
		}
		return err
	}
	current, err := getPlaceRow(tx, *existingID)
	if err != nil {
		return err
	}
	name, address, kind := current.Name, current.Address, current.Kind
	if name == "" {
		name = strings.TrimSpace(place.Name)
	}
	if address == "" {
		address = strings.TrimSpace(place.Address)
	}
	if kind == PlaceKindOther {
		kind = place.Kind
	}
	if current.DeletedAt != 0 || (name == current.Name && address == current.Address && kind == current.Kind) {
		counts.RecordsUnchanged++
		return nil
	}
	if err := snapshot(tx, EntityPlace, current.ID, "import", at); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE places SET name=?, address=?, kind=?, updated_at=? WHERE id=?`, name, address, kind, at, current.ID); err != nil {
		return err
	}
	counts.PlacesUpdated++
	return nil
}

func existingByRef(tx *sql.Tx, table, source, sourceRef string) (string, error) {
	var id string
	err := tx.QueryRow(`SELECT id FROM `+table+` WHERE source = ? AND source_ref = ?`, source, sourceRef).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func applyVisit(tx *sql.Tx, importID string, visit ImportVisit, at int64, counts *ImportCounts) error {
	placeID, err := placeIDByGoogleID(tx, visit.GooglePlaceID)
	if err != nil {
		return err
	}
	existingID, err := existingByRef(tx, "visits", visit.Source, visit.SourceRef)
	if err != nil {
		return err
	}
	if existingID == "" {
		id, seq, err := nextID(tx, "visits", "visit")
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO visits (id, seq, place_id, started_at, ended_at, latitude, longitude, probability,
			source_semantic_type, source, source_ref, import_id, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, seq, nullableString(placeID), visit.StartedAt, visit.EndedAt, nullableFloat(visit.Latitude), nullableFloat(visit.Longitude),
			nullableFloat(visit.Probability), visit.SourceSemanticType, visit.Source, visit.SourceRef, importID, at, at)
		if err != nil {
			return err
		}
		counts.VisitsCreated++
		return recordHeld(tx, importID, EntityVisit, id)
	}
	if err := recordHeld(tx, importID, EntityVisit, existingID); err != nil {
		return err
	}
	current, err := getVisitRow(tx, existingID)
	if err != nil {
		return err
	}
	if current.PlaceID != nil {
		placeID = current.PlaceID
	}
	// A visit someone deleted stays as they left it; one a newer export set
	// aside is brought up to date, and reconcileSnapshots decides whether it
	// comes back.
	if (current.DeletedAt != 0 && current.SupersededByImportID == nil) || (sameString(current.PlaceID, placeID) && current.StartedAt == visit.StartedAt &&
		current.EndedAt == visit.EndedAt && sameFloat(current.Latitude, visit.Latitude) && sameFloat(current.Longitude, visit.Longitude) &&
		sameFloat(current.Probability, visit.Probability) && current.SourceSemanticType == visit.SourceSemanticType) {
		counts.RecordsUnchanged++
		return nil
	}
	if err := snapshot(tx, EntityVisit, existingID, "import", at); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE visits SET place_id=?, started_at=?, ended_at=?, latitude=?, longitude=?, probability=?,
		source_semantic_type=?, updated_at=? WHERE id=?`, nullableString(placeID), visit.StartedAt, visit.EndedAt,
		nullableFloat(visit.Latitude), nullableFloat(visit.Longitude), nullableFloat(visit.Probability), visit.SourceSemanticType,
		at, existingID); err != nil {
		return err
	}
	counts.VisitsUpdated++
	return nil
}

func samePath(a, b []PathPoint) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index].Latitude != b[index].Latitude || a[index].Longitude != b[index].Longitude {
			return false
		}
		if (a[index].Timestamp == nil) != (b[index].Timestamp == nil) || (a[index].Timestamp != nil && *a[index].Timestamp != *b[index].Timestamp) {
			return false
		}
	}
	return true
}

func writePath(tx *sql.Tx, tripID string, path []PathPoint) error {
	if _, err := tx.Exec(`DELETE FROM trip_path_points WHERE trip_id = ?`, tripID); err != nil {
		return err
	}
	statement, err := tx.Prepare(`INSERT INTO trip_path_points (trip_id, position, latitude, longitude, timestamp) VALUES (?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for position, point := range path {
		var timestamp any
		if point.Timestamp != nil {
			timestamp = *point.Timestamp
		}
		if _, err := statement.Exec(tripID, position, point.Latitude, point.Longitude, timestamp); err != nil {
			return err
		}
	}
	return nil
}

func readPath(tx *sql.Tx, tripID string) ([]PathPoint, error) {
	rows, err := tx.Query(`SELECT latitude, longitude, timestamp FROM trip_path_points WHERE trip_id = ? ORDER BY position`, tripID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var path []PathPoint
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

func applyTrip(tx *sql.Tx, importID string, trip ImportTrip, at int64, counts *ImportCounts) error {
	startPlaceID, err := placeIDByGoogleID(tx, trip.StartGooglePlaceID)
	if err != nil {
		return err
	}
	endPlaceID, err := placeIDByGoogleID(tx, trip.EndGooglePlaceID)
	if err != nil {
		return err
	}
	existingID, err := existingByRef(tx, "trips", trip.Source, trip.SourceRef)
	if err != nil {
		return err
	}
	if existingID == "" {
		id, seq, err := nextID(tx, "trips", "trip")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO trips (id, seq, started_at, ended_at, start_latitude, start_longitude, end_latitude,
			end_longitude, start_place_id, end_place_id, mode, source_mode, distance_meters, probability, source, source_ref,
			import_id, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, seq, trip.StartedAt, trip.EndedAt, nullableFloat(trip.StartLatitude), nullableFloat(trip.StartLongitude),
			nullableFloat(trip.EndLatitude), nullableFloat(trip.EndLongitude), nullableString(startPlaceID), nullableString(endPlaceID),
			trip.Mode, trip.SourceMode, nullableFloat(trip.DistanceMeters), nullableFloat(trip.Probability), trip.Source,
			trip.SourceRef, importID, at, at); err != nil {
			return err
		}
		counts.TripsCreated++
		if err := recordHeld(tx, importID, EntityTrip, id); err != nil {
			return err
		}
		return writePath(tx, id, trip.Path)
	}
	if err := recordHeld(tx, importID, EntityTrip, existingID); err != nil {
		return err
	}
	current, err := getTripRow(tx, existingID)
	if err != nil {
		return err
	}
	if current.StartPlaceID != nil {
		startPlaceID = current.StartPlaceID
	}
	if current.EndPlaceID != nil {
		endPlaceID = current.EndPlaceID
	}
	// A mode that no longer matches what the source said was corrected by
	// someone; keep it.
	mode := trip.Mode
	if current.Mode != ModeForGoogleActivityType(current.SourceMode) {
		mode = current.Mode
	}
	path, err := readPath(tx, existingID)
	if err != nil {
		return err
	}
	if current.DeletedAt != 0 && current.SupersededByImportID == nil {
		counts.RecordsUnchanged++
		return nil
	}
	if current.StartedAt == trip.StartedAt && current.EndedAt == trip.EndedAt && sameFloat(current.StartLatitude, trip.StartLatitude) &&
		sameFloat(current.StartLongitude, trip.StartLongitude) && sameFloat(current.EndLatitude, trip.EndLatitude) &&
		sameFloat(current.EndLongitude, trip.EndLongitude) && sameString(current.StartPlaceID, startPlaceID) &&
		sameString(current.EndPlaceID, endPlaceID) && current.Mode == mode && current.SourceMode == trip.SourceMode &&
		sameFloat(current.DistanceMeters, trip.DistanceMeters) && sameFloat(current.Probability, trip.Probability) && samePath(path, trip.Path) {
		counts.RecordsUnchanged++
		return nil
	}
	if err := snapshot(tx, EntityTrip, existingID, "import", at); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE trips SET started_at=?, ended_at=?, start_latitude=?, start_longitude=?, end_latitude=?,
		end_longitude=?, start_place_id=?, end_place_id=?, mode=?, source_mode=?, distance_meters=?, probability=?, updated_at=?
		WHERE id=?`, trip.StartedAt, trip.EndedAt, nullableFloat(trip.StartLatitude), nullableFloat(trip.StartLongitude),
		nullableFloat(trip.EndLatitude), nullableFloat(trip.EndLongitude), nullableString(startPlaceID), nullableString(endPlaceID),
		mode, trip.SourceMode, nullableFloat(trip.DistanceMeters), nullableFloat(trip.Probability), at, existingID); err != nil {
		return err
	}
	counts.TripsUpdated++
	if samePath(path, trip.Path) {
		return nil
	}
	return writePath(tx, existingID, trip.Path)
}

// applyPoints adds raw points. A point already there from the same source is
// left alone. A point with a source_ref replaces an earlier point with the
// same source and source_ref, so a photo whose GPS changed moves.
func applyPoints(tx *sql.Tx, importID string, points []ImportPoint, counts *ImportCounts) error {
	if len(points) == 0 {
		return nil
	}
	insert, err := tx.Prepare(`INSERT OR IGNORE INTO points (timestamp, latitude, longitude, accuracy_meters, source, source_ref, import_id)
		VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insert.Close()
	for _, point := range points {
		if point.SourceRef != "" {
			var same int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM points WHERE source = ? AND source_ref = ? AND timestamp = ? AND latitude = ? AND longitude = ?`,
				point.Source, point.SourceRef, point.Timestamp, point.Latitude, point.Longitude).Scan(&same); err != nil {
				return err
			}
			if same > 0 {
				counts.PointsUnchanged++
				continue
			}
			if _, err := tx.Exec(`DELETE FROM points WHERE source = ? AND source_ref = ?`, point.Source, point.SourceRef); err != nil {
				return err
			}
		}
		result, err := insert.Exec(point.Timestamp, point.Latitude, point.Longitude, nullableFloat(point.AccuracyMeters),
			point.Source, point.SourceRef, importID)
		if err != nil {
			return err
		}
		added, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if added == 1 {
			counts.PointsAdded++
		} else {
			counts.PointsUnchanged++
		}
	}
	return nil
}
