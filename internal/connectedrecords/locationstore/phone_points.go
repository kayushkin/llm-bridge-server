package locationstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/location-store/internal/geonames"
)

// A phone sends its fixes as it makes them (POST /points/live). They are
// points with source PointSourcePhone, kept in one import per device and UTC
// day. POST /phone-points/segment then makes them into visits (stays) and
// movements between them, source SourcePhonePoints, so journey grouping and
// calendar matching read them like an export's.

const (
	// livePointsMaximum is the most points one POST /points/live may carry.
	livePointsMaximum = 5000
	// livePointsFutureSeconds is how far past the server's clock a fix may
	// be, for a phone clock that runs a little fast.
	livePointsFutureSeconds = 10 * 60
	// livePointsOldest is 2000-01-01: an earlier fix is a broken clock.
	livePointsOldest = 946684800

	// phoneStayRadiusMeters is how far from a stay's center a fix may be and
	// still belong to it.
	phoneStayRadiusMeters = 150.0
	// phoneStayMinimumSeconds is the shortest stay that is a visit.
	phoneStayMinimumSeconds = 10 * 60
	// phoneStayMaximumGapSeconds is the longest silence inside one stay. A
	// phone lying still overnight reports seldom, so this is long: a night
	// must stay one visit for journey grouping to place it.
	phoneStayMaximumGapSeconds = 12 * 3600
	// phonePointMaximumAccuracyMeters drops fixes too rough to place a stay.
	phonePointMaximumAccuracyMeters = 250.0
	// phonePlaceMatchMeters is how near a stored place a stay's center must
	// be to count as a visit to it, when the place gives no larger radius.
	phonePlaceMatchMeters = 100.0
	// phoneSegmentMaximumSeconds is the longest span one run reads.
	phoneSegmentMaximumSeconds = 400 * 24 * 3600

	// reasonPhonePoints is the revision reason of every change a
	// segmenting run makes.
	reasonPhonePoints = "phone_points"
)

var devicePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// errDryRun rolls back a dry run's transaction: it does all the work a real
// run does, then writes none of it.
var errDryRun = errors.New("dry run")

// RecordLivePoints stores the fixes a phone sent. A fix already there is a
// no-op, so a phone may send a batch again after a lost answer.
func (s *Store) RecordLivePoints(live LivePoints) (*LivePointsRecorded, error) {
	device := strings.TrimSpace(live.Device)
	recordedBy := strings.TrimSpace(live.RecordedBy)
	switch {
	case !devicePattern.MatchString(device):
		return nil, fmt.Errorf("%w: device must be 1 to 100 letters, digits, dots, dashes or underscores", ErrInvalid)
	case recordedBy == "":
		return nil, fmt.Errorf("%w: recorded_by is required", ErrInvalid)
	case len(live.Points) == 0:
		return nil, fmt.Errorf("%w: points is empty", ErrInvalid)
	case len(live.Points) > livePointsMaximum:
		return nil, fmt.Errorf("%w: %d points; send at most %d at once", ErrInvalid, len(live.Points), livePointsMaximum)
	}
	at := now()
	byDay := map[string][]ImportPoint{}
	for index, point := range live.Points {
		switch {
		case !validLatitudeLongitude(point.Latitude, point.Longitude):
			return nil, fmt.Errorf("%w: point %d: latitude or longitude out of range", ErrInvalid, index)
		case point.Timestamp < livePointsOldest || point.Timestamp > at+livePointsFutureSeconds:
			return nil, fmt.Errorf("%w: point %d: timestamp %d is not a time the phone could have made it", ErrInvalid, index, point.Timestamp)
		case point.AccuracyMeters != nil && (*point.AccuracyMeters < 0 || math.IsNaN(*point.AccuracyMeters)):
			return nil, fmt.Errorf("%w: point %d: accuracy_meters is negative", ErrInvalid, index)
		}
		day := time.Unix(point.Timestamp, 0).UTC().Format(time.DateOnly)
		byDay[day] = append(byDay[day], ImportPoint{Timestamp: point.Timestamp, Latitude: point.Latitude,
			Longitude: point.Longitude, AccuracyMeters: point.AccuracyMeters, Source: PointSourcePhone})
	}
	days := make([]string, 0, len(byDay))
	for day := range byDay {
		days = append(days, day)
	}
	sort.Strings(days)
	recorded := &LivePointsRecorded{ImportIDs: []string{}}
	err := s.inTransaction(func(tx *sql.Tx) error {
		for _, day := range days {
			importID, counts, err := livePointsImport(tx, device, day, recordedBy, at)
			if err != nil {
				return err
			}
			before := *counts
			if err := applyPoints(tx, importID, byDay[day], counts); err != nil {
				return err
			}
			recorded.PointsAdded += counts.PointsAdded - before.PointsAdded
			recorded.PointsUnchanged += counts.PointsUnchanged - before.PointsUnchanged
			encoded, err := json.Marshal(counts)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE imports SET counts=?, finished_at=?, recorded_by=? WHERE id=?`,
				string(encoded), at, recordedBy, importID); err != nil {
				return err
			}
			recorded.ImportIDs = append(recorded.ImportIDs, importID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return recorded, nil
}

// livePointsImport finds or makes the import that holds one device's points
// of one UTC day. Its hash is of the device and day, so it is found again;
// finished_at is when it last took points.
func livePointsImport(tx *sql.Tx, device, day, recordedBy string, at int64) (string, *ImportCounts, error) {
	sum := sha256.Sum256([]byte(ImportKindPhoneLive + "\n" + device + "\n" + day))
	hash := hex.EncodeToString(sum[:])
	var id, kind, encoded string
	err := tx.QueryRow(`SELECT id, kind, counts FROM imports WHERE sha256 = ?`, hash).Scan(&id, &kind, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		var seq int64
		if id, seq, err = nextID(tx, "imports", "import"); err != nil {
			return "", nil, err
		}
		_, err = tx.Exec(`INSERT INTO imports (id, seq, kind, sha256, source_machine, source_path, recorded_by, imported_at) VALUES (?,?,?,?,?,?,?,?)`,
			id, seq, ImportKindPhoneLive, hash, device, "", recordedBy, at)
		return id, &ImportCounts{}, err
	}
	if err != nil {
		return "", nil, err
	}
	if kind != ImportKindPhoneLive {
		return "", nil, fmt.Errorf("%w: import %s has the hash of %s's points of %s but is %s", ErrConflict, id, device, day, kind)
	}
	counts := &ImportCounts{}
	if err := json.Unmarshal([]byte(encoded), counts); err != nil {
		return "", nil, fmt.Errorf("import %s has counts that are not JSON: %w", id, err)
	}
	return id, counts, nil
}

// phoneFix is one phone point the segmenter reads.
type phoneFix struct {
	timestamp           int64
	latitude, longitude float64
}

// phoneStay is a run of fixes within phoneStayRadiusMeters of their center.
type phoneStay struct {
	first, last         int // indexes into the fixes
	latitude, longitude float64
}

func (stay phoneStay) startedAt(fixes []phoneFix) int64 { return fixes[stay.first].timestamp }
func (stay phoneStay) endedAt(fixes []phoneFix) int64   { return fixes[stay.last].timestamp }

// findPhoneStays walks the fixes in time order and takes each longest run
// that stays within the radius of its own running center, with no silence
// longer than phoneStayMaximumGapSeconds, lasting phoneStayMinimumSeconds.
// Two stays in a row at one spot (a fix that strayed between them) are one.
func findPhoneStays(fixes []phoneFix) []phoneStay {
	stays := []phoneStay{}
	for first := 0; first < len(fixes); {
		latitudeSum, longitudeSum := fixes[first].latitude, fixes[first].longitude
		last := first
		for last+1 < len(fixes) {
			next := fixes[last+1]
			count := float64(last - first + 1)
			if geonames.DistanceMeters(latitudeSum/count, longitudeSum/count, next.latitude, next.longitude) > phoneStayRadiusMeters ||
				next.timestamp-fixes[last].timestamp > phoneStayMaximumGapSeconds {
				break
			}
			last++
			latitudeSum += next.latitude
			longitudeSum += next.longitude
		}
		if last > first && fixes[last].timestamp-fixes[first].timestamp >= phoneStayMinimumSeconds {
			count := float64(last - first + 1)
			stay := phoneStay{first: first, last: last, latitude: latitudeSum / count, longitude: longitudeSum / count}
			if previous := len(stays) - 1; previous >= 0 &&
				geonames.DistanceMeters(stays[previous].latitude, stays[previous].longitude, stay.latitude, stay.longitude) <= phoneStayRadiusMeters &&
				stay.startedAt(fixes)-stays[previous].endedAt(fixes) <= phoneStayMaximumGapSeconds {
				stays[previous] = mergePhoneStays(fixes, stays[previous], stay)
			} else {
				stays = append(stays, stay)
			}
			first = last + 1
			continue
		}
		first++
	}
	return stays
}

// mergePhoneStays is one stay from a to b, centered on all the fixes of
// both (the fixes between them that strayed are left out of the center).
func mergePhoneStays(fixes []phoneFix, a, b phoneStay) phoneStay {
	countA, countB := float64(a.last-a.first+1), float64(b.last-b.first+1)
	return phoneStay{first: a.first, last: b.last,
		latitude:  (a.latitude*countA + b.latitude*countB) / (countA + countB),
		longitude: (a.longitude*countA + b.longitude*countB) / (countA + countB)}
}

// phoneMovement is the way from one stay to the next.
type phoneMovement struct {
	startedAt, endedAt int64
	from, to           phoneStay
	path               []PathPoint
	distanceMeters     float64
	mode               string
}

// phoneMovementBetween is the movement from stay a to the next stay b: its
// path is a's last fix, every fix between, and b's first fix.
func phoneMovementBetween(fixes []phoneFix, a, b phoneStay) phoneMovement {
	movement := phoneMovement{startedAt: a.endedAt(fixes), endedAt: b.startedAt(fixes), from: a, to: b}
	for index := a.last; index <= b.first; index++ {
		timestamp := fixes[index].timestamp
		movement.path = append(movement.path, PathPoint{Latitude: fixes[index].latitude, Longitude: fixes[index].longitude, Timestamp: &timestamp})
		if index > a.last {
			movement.distanceMeters += geonames.DistanceMeters(fixes[index-1].latitude, fixes[index-1].longitude,
				fixes[index].latitude, fixes[index].longitude)
		}
	}
	straight := geonames.DistanceMeters(a.latitude, a.longitude, b.latitude, b.longitude)
	movement.mode = phoneMovementMode(straight, movement.distanceMeters, movement.endedAt-movement.startedAt)
	return movement
}

// phoneMovementMode guesses how the user moved from speed alone: a phone
// reporting a few times an hour cannot tell a bicycle from a slow bus, so
// what speed does not settle is unknown.
func phoneMovementMode(straightMeters, pathMeters float64, seconds int64) string {
	if seconds <= 0 {
		return ModeUnknown
	}
	straightKilometersPerHour := straightMeters / float64(seconds) * 3.6
	pathKilometersPerHour := math.Max(straightMeters, pathMeters) / float64(seconds) * 3.6
	switch {
	case straightMeters >= 100_000 && straightKilometersPerHour >= 200:
		return ModeFlying
	case pathKilometersPerHour >= 25:
		return ModeDriving
	case pathKilometersPerHour <= 7:
		return ModeWalking
	default:
		return ModeUnknown
	}
}

// storedPhoneRecord is a visit or movement an earlier run made.
type storedPhoneRecord struct {
	id                 string
	startedAt, endedAt int64
	deletedAt          int64
	editedByHand       bool
	deletedByHand      bool
	matched            bool
}

// SegmentPhonePoints makes the phone points of [from, to) into visits and
// movements. It rewrites only what earlier runs made: a record someone
// edited (a revision with reason update, attach or restore, or a description
// or tags) is left as it is, as is one someone deleted, and no segment is
// made where it would overlap one of those or any visit or movement from
// another source. A stored record no segment matches is soft-deleted.
func (s *Store) SegmentPhonePoints(request PhoneSegmentRequest) (*PhoneSegmentation, error) {
	if request.From <= 0 || request.To <= request.From {
		return nil, fmt.Errorf("%w: from and to are required, and to must be after from", ErrInvalid)
	}
	if request.To-request.From > phoneSegmentMaximumSeconds {
		return nil, fmt.Errorf("%w: read at most %d days at once", ErrInvalid, phoneSegmentMaximumSeconds/86400)
	}
	result := &PhoneSegmentation{DryRun: request.DryRun, To: request.To, PlacesCreated: []string{},
		Actions: []PhoneSegmentAction{}, ActionCounts: map[string]int{}}
	err := s.inTransaction(func(tx *sql.Tx) error {
		at := now()
		from, err := phoneSegmentStart(tx, request.From)
		if err != nil {
			return err
		}
		result.From = from
		fixes, tooRough, err := readPhoneFixes(tx, from, request.To)
		if err != nil {
			return err
		}
		result.PointsRead, result.PointsTooRough = len(fixes)+tooRough, tooRough
		stays := findPhoneStays(fixes)
		storedVisits, err := readStoredPhoneRecords(tx, "visits", "", from, request.To)
		if err != nil {
			return err
		}
		storedTrips, err := readStoredPhoneRecords(tx, "trips", "AND kind = 'movement'", from, request.To)
		if err != nil {
			return err
		}
		segmenter := phoneSegmenter{stayPlaces: map[int]string{}, tx: tx, at: at, fixes: fixes, result: result}
		// Visits first, so each movement starts and ends at its stays' places.
		for _, stay := range stays {
			if err := segmenter.visit(stay, storedVisits); err != nil {
				return err
			}
		}
		for index := 0; index+1 < len(stays); index++ {
			if err := segmenter.movement(phoneMovementBetween(fixes, stays[index], stays[index+1]), storedTrips); err != nil {
				return err
			}
		}
		if err := segmenter.deleteUnmatched(EntityVisit, storedVisits); err != nil {
			return err
		}
		if err := segmenter.deleteUnmatched(EntityTrip, storedTrips); err != nil {
			return err
		}
		if request.DryRun {
			return errDryRun
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDryRun) {
		return nil, err
	}
	for _, action := range result.Actions {
		result.ActionCounts[action.Action]++
	}
	return result, nil
}

// phoneSegmentStart moves from back to the start of a stored phone visit or
// movement that runs across it, so a run never cuts a stay in two.
func phoneSegmentStart(tx *sql.Tx, from int64) (int64, error) {
	var earliest sql.NullInt64
	err := tx.QueryRow(`SELECT MIN(started_at) FROM (
			SELECT started_at FROM visits WHERE source = ? AND deleted_at = 0 AND started_at < ? AND ended_at >= ?
			UNION ALL
			SELECT started_at FROM trips WHERE source = ? AND kind = 'movement' AND deleted_at = 0 AND started_at < ? AND ended_at >= ?)`,
		SourcePhonePoints, from, from, SourcePhonePoints, from, from).Scan(&earliest)
	if err != nil {
		return 0, err
	}
	if earliest.Valid {
		return earliest.Int64, nil
	}
	return from, nil
}

// readPhoneFixes is the phone points of [from, to) in time order, without
// the ones too rough to place a stay, which it counts.
func readPhoneFixes(tx *sql.Tx, from, to int64) ([]phoneFix, int, error) {
	rows, err := tx.Query(`SELECT timestamp, latitude, longitude, accuracy_meters FROM points
		WHERE source = ? AND timestamp >= ? AND timestamp < ? ORDER BY timestamp, latitude, longitude`, PointSourcePhone, from, to)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	fixes := []phoneFix{}
	tooRough := 0
	for rows.Next() {
		var fix phoneFix
		var accuracy sql.NullFloat64
		if err := rows.Scan(&fix.timestamp, &fix.latitude, &fix.longitude, &accuracy); err != nil {
			return nil, 0, err
		}
		if accuracy.Valid && accuracy.Float64 > phonePointMaximumAccuracyMeters {
			tooRough++
			continue
		}
		fixes = append(fixes, fix)
	}
	return fixes, tooRough, rows.Err()
}

// readStoredPhoneRecords is the visits or movements earlier runs made that
// start in [from, to), deleted ones too.
func readStoredPhoneRecords(tx *sql.Tx, table, extraWhere string, from, to int64) ([]*storedPhoneRecord, error) {
	rows, err := tx.Query(`SELECT r.id, r.started_at, r.ended_at, r.deleted_at,
			r.description != '' OR r.tags != '[]' OR EXISTS (SELECT 1 FROM revisions v WHERE v.entity_id = r.id AND v.reason IN ('update', 'attach', 'restore')),
			EXISTS (SELECT 1 FROM revisions v WHERE v.entity_id = r.id AND v.reason = 'delete')
		FROM `+table+` r WHERE r.source = ? `+extraWhere+` AND r.started_at >= ? AND r.started_at < ? ORDER BY r.started_at, r.seq`,
		SourcePhonePoints, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []*storedPhoneRecord{}
	for rows.Next() {
		record := &storedPhoneRecord{}
		if err := rows.Scan(&record.id, &record.startedAt, &record.endedAt, &record.deletedAt, &record.editedByHand, &record.deletedByHand); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

type phoneSegmenter struct {
	// stayPlaces is the place each stay's visit is at, by the stay's first
	// fix, so the movements between stays start and end at the same places.
	stayPlaces map[int]string
	tx         *sql.Tx
	at         int64
	fixes      []phoneFix
	result     *PhoneSegmentation
}

func (segmenter *phoneSegmenter) record(action PhoneSegmentAction) {
	segmenter.result.Actions = append(segmenter.result.Actions, action)
}

// claim finds the stored record a new segment is: of the records it overlaps,
// a kept one (edited or deleted by hand) stops it; otherwise the unmatched
// one it overlaps most is its own.
func claim(stored []*storedPhoneRecord, startedAt, endedAt int64) (own *storedPhoneRecord, keptAction string, kept *storedPhoneRecord) {
	var bestOverlap int64 = -1
	for _, record := range stored {
		overlap := overlapSeconds(startedAt, endedAt, record.startedAt, record.endedAt)
		if overlap < 0 || (overlap == 0 && !(startedAt == record.startedAt && endedAt == record.endedAt)) {
			continue
		}
		switch {
		case record.deletedAt != 0 && record.deletedByHand:
			record.matched = true
			return nil, "kept_deleted", record
		case record.deletedAt == 0 && record.editedByHand:
			record.matched = true
			return nil, "kept_edited", record
		case record.deletedAt != 0:
			continue // one an earlier run deleted is not brought back; a new one is made
		}
		if !record.matched && overlap > bestOverlap {
			own, bestOverlap = record, overlap
		}
	}
	if own != nil {
		own.matched = true
	}
	return own, "", nil
}

// otherSourceOverlaps is the id of a live visit (or movement) from another
// source that overlaps the span, or "".
func (segmenter *phoneSegmenter) otherSourceOverlaps(table, extraWhere string, startedAt, endedAt int64) (string, error) {
	var id string
	err := segmenter.tx.QueryRow(`SELECT id FROM `+table+` WHERE source != ? `+extraWhere+` AND deleted_at = 0
		AND started_at < ? AND ended_at > ? ORDER BY started_at LIMIT 1`, SourcePhonePoints, endedAt, startedAt).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// placeFor is the place a stay is at. A record that already names a place
// keeps it while the place is live and the stay's center within its reach
// (its radius, at least phonePlaceMatchMeters), so a merge, or a near
// duplicate a little nearer, never moves it. Otherwise it is the live place
// nearest the center within reach (the oldest of equally near ones), or a new
// unnamed one there.
func (segmenter *phoneSegmenter) placeFor(stay phoneStay, current *string) (string, error) {
	if current != nil {
		var latitude, longitude, radius float64
		err := segmenter.tx.QueryRow(`SELECT latitude, longitude, radius_meters FROM places WHERE id = ? AND deleted_at = 0`, *current).
			Scan(&latitude, &longitude, &radius)
		switch {
		case err == nil:
			if geonames.DistanceMeters(stay.latitude, stay.longitude, latitude, longitude) <= math.Max(radius, phonePlaceMatchMeters) {
				return *current, nil
			}
		case !errors.Is(err, sql.ErrNoRows):
			return "", err
		}
	}
	// A degree of latitude is about 111 km; the box is wide enough for any
	// radius a source gives, and the distance decides.
	const boxDegrees = 0.05
	longitudeBox := boxDegrees / math.Max(math.Cos(stay.latitude*math.Pi/180), 0.01)
	rows, err := segmenter.tx.Query(`SELECT id, latitude, longitude, radius_meters FROM places WHERE deleted_at = 0
		AND latitude BETWEEN ? AND ? AND longitude BETWEEN ? AND ? ORDER BY seq`,
		stay.latitude-boxDegrees, stay.latitude+boxDegrees, stay.longitude-longitudeBox, stay.longitude+longitudeBox)
	if err != nil {
		return "", err
	}
	nearestID, nearestMeters := "", math.Inf(1)
	for rows.Next() {
		var id string
		var latitude, longitude, radius float64
		if err := rows.Scan(&id, &latitude, &longitude, &radius); err != nil {
			rows.Close()
			return "", err
		}
		meters := geonames.DistanceMeters(stay.latitude, stay.longitude, latitude, longitude)
		if meters <= math.Max(radius, phonePlaceMatchMeters) && meters < nearestMeters {
			nearestID, nearestMeters = id, meters
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	if nearestID != "" {
		return nearestID, nil
	}
	id, seq, err := nextID(segmenter.tx, "places", "place")
	if err != nil {
		return "", err
	}
	if _, err := segmenter.tx.Exec(`INSERT INTO places (id, seq, latitude, longitude, radius_meters, kind, source, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, id, seq, stay.latitude, stay.longitude, phonePlaceMatchMeters, PlaceKindOther,
		SourcePhonePoints, segmenter.at, segmenter.at); err != nil {
		return "", err
	}
	segmenter.result.PlacesCreated = append(segmenter.result.PlacesCreated, id)
	return id, nil
}

// endPlace is the place a movement starts or ends at: its stay's visit's
// place, or for a stay with no visit of this run's (kept or another
// source's), the place found for the stay.
func (segmenter *phoneSegmenter) endPlace(stay phoneStay, current *string) (string, error) {
	if placeID, found := segmenter.stayPlaces[stay.first]; found {
		return placeID, nil
	}
	return segmenter.placeFor(stay, current)
}

func phoneSourceRef(startedAt, endedAt int64) string { return fmt.Sprintf("%d-%d", startedAt, endedAt) }

func (segmenter *phoneSegmenter) visit(stay phoneStay, stored []*storedPhoneRecord) error {
	startedAt, endedAt := stay.startedAt(segmenter.fixes), stay.endedAt(segmenter.fixes)
	action := PhoneSegmentAction{EntityType: EntityVisit, StartedAt: startedAt, EndedAt: endedAt, Points: stay.last - stay.first + 1}
	own, keptAction, kept := claim(stored, startedAt, endedAt)
	if kept != nil {
		action.Action, action.ID = keptAction, kept.id
		segmenter.record(action)
		return nil
	}
	other, err := segmenter.otherSourceOverlaps("visits", "", startedAt, endedAt)
	if err != nil {
		return err
	}
	if other != "" {
		if own != nil {
			own.matched = false // a stay another source now covers is not this run's to keep
		}
		action.Action, action.Note = "skip_other_source", "overlaps "+other
		segmenter.record(action)
		return nil
	}
	var current *Visit
	if own != nil {
		if current, err = getVisitRow(segmenter.tx, own.id); err != nil {
			return err
		}
	}
	var currentPlace *string
	if current != nil {
		currentPlace = current.PlaceID
	}
	placeID, err := segmenter.placeFor(stay, currentPlace)
	if err != nil {
		return err
	}
	segmenter.stayPlaces[stay.first] = placeID
	action.PlaceID = &placeID
	if own == nil {
		id, seq, err := nextID(segmenter.tx, "visits", "visit")
		if err != nil {
			return err
		}
		if _, err := segmenter.tx.Exec(`INSERT INTO visits (id, seq, place_id, started_at, ended_at, latitude, longitude,
			source, source_ref, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, id, seq, placeID, startedAt, endedAt,
			stay.latitude, stay.longitude, SourcePhonePoints, phoneSourceRef(startedAt, endedAt), segmenter.at, segmenter.at); err != nil {
			return err
		}
		action.Action, action.ID = "create", id
		segmenter.record(action)
		return nil
	}
	action.ID = own.id
	if current.StartedAt == startedAt && current.EndedAt == endedAt && sameString(current.PlaceID, &placeID) &&
		sameFloat(current.Latitude, &stay.latitude) && sameFloat(current.Longitude, &stay.longitude) {
		action.Action = "unchanged"
		segmenter.record(action)
		return nil
	}
	if err := snapshot(segmenter.tx, EntityVisit, own.id, reasonPhonePoints, segmenter.at); err != nil {
		return err
	}
	// A moved center may be in another town: clear it for
	// location-name-localities to name again.
	locality := ""
	if !sameFloat(current.Latitude, &stay.latitude) || !sameFloat(current.Longitude, &stay.longitude) {
		locality = "locality=NULL, "
	}
	if _, err := segmenter.tx.Exec(`UPDATE visits SET place_id=?, started_at=?, ended_at=?, latitude=?, longitude=?, source_ref=?,
		`+locality+`updated_at=? WHERE id=?`, placeID, startedAt, endedAt, stay.latitude, stay.longitude,
		phoneSourceRef(startedAt, endedAt), segmenter.at, own.id); err != nil {
		return err
	}
	action.Action = "update"
	segmenter.record(action)
	return nil
}

func (segmenter *phoneSegmenter) movement(movement phoneMovement, stored []*storedPhoneRecord) error {
	distance := movement.distanceMeters
	action := PhoneSegmentAction{EntityType: EntityTrip, StartedAt: movement.startedAt, EndedAt: movement.endedAt,
		Mode: movement.mode, DistanceMeters: &distance, Points: len(movement.path)}
	own, keptAction, kept := claim(stored, movement.startedAt, movement.endedAt)
	if kept != nil {
		action.Action, action.ID = keptAction, kept.id
		segmenter.record(action)
		return nil
	}
	other, err := segmenter.otherSourceOverlaps("trips", "AND kind = 'movement'", movement.startedAt, movement.endedAt)
	if err != nil {
		return err
	}
	if other != "" {
		if own != nil {
			own.matched = false
		}
		action.Action, action.Note = "skip_other_source", "overlaps "+other
		segmenter.record(action)
		return nil
	}
	from, to := movement.from, movement.to
	var current *Trip
	var currentStart, currentEnd *string
	if own != nil {
		if current, err = getTripRow(segmenter.tx, own.id); err != nil {
			return err
		}
		currentStart, currentEnd = current.StartPlaceID, current.EndPlaceID
	}
	startPlace, err := segmenter.endPlace(from, currentStart)
	if err != nil {
		return err
	}
	endPlace, err := segmenter.endPlace(to, currentEnd)
	if err != nil {
		return err
	}
	if own == nil {
		id, seq, err := nextID(segmenter.tx, "trips", "trip")
		if err != nil {
			return err
		}
		if _, err := segmenter.tx.Exec(`INSERT INTO trips (id, seq, kind, started_at, ended_at, start_latitude, start_longitude,
			end_latitude, end_longitude, start_place_id, end_place_id, mode, distance_meters, source, source_ref, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, seq, TripKindMovement, movement.startedAt, movement.endedAt,
			from.latitude, from.longitude, to.latitude, to.longitude, startPlace, endPlace, movement.mode, distance,
			SourcePhonePoints, phoneSourceRef(movement.startedAt, movement.endedAt), segmenter.at, segmenter.at); err != nil {
			return err
		}
		if err := writePath(segmenter.tx, id, movement.path); err != nil {
			return err
		}
		action.Action, action.ID = "create", id
		segmenter.record(action)
		return nil
	}
	action.ID = own.id
	path, err := readPath(segmenter.tx, own.id)
	if err != nil {
		return err
	}
	endsSame := sameFloat(current.StartLatitude, &from.latitude) && sameFloat(current.StartLongitude, &from.longitude) &&
		sameFloat(current.EndLatitude, &to.latitude) && sameFloat(current.EndLongitude, &to.longitude)
	if current.StartedAt == movement.startedAt && current.EndedAt == movement.endedAt && endsSame &&
		sameString(current.StartPlaceID, &startPlace) && sameString(current.EndPlaceID, &endPlace) &&
		current.Mode == movement.mode && sameFloat(current.DistanceMeters, &distance) && samePath(path, movement.path) {
		action.Action = "unchanged"
		segmenter.record(action)
		return nil
	}
	if err := snapshot(segmenter.tx, EntityTrip, own.id, reasonPhonePoints, segmenter.at); err != nil {
		return err
	}
	localities := ""
	if !endsSame {
		localities = "start_locality=NULL, end_locality=NULL, "
	}
	if _, err := segmenter.tx.Exec(`UPDATE trips SET started_at=?, ended_at=?, start_latitude=?, start_longitude=?, end_latitude=?,
		end_longitude=?, start_place_id=?, end_place_id=?, mode=?, distance_meters=?, source_ref=?, `+localities+`updated_at=? WHERE id=?`,
		movement.startedAt, movement.endedAt, from.latitude, from.longitude, to.latitude, to.longitude, startPlace, endPlace,
		movement.mode, distance, phoneSourceRef(movement.startedAt, movement.endedAt), segmenter.at, own.id); err != nil {
		return err
	}
	if err := writePath(segmenter.tx, own.id, movement.path); err != nil {
		return err
	}
	action.Action = "update"
	segmenter.record(action)
	return nil
}

// deleteUnmatched soft-deletes each live stored record this run found no
// segment for, unless someone edited it.
func (segmenter *phoneSegmenter) deleteUnmatched(entityType string, stored []*storedPhoneRecord) error {
	for _, record := range stored {
		if record.matched || record.deletedAt != 0 || record.editedByHand {
			continue
		}
		if err := snapshot(segmenter.tx, entityType, record.id, reasonPhonePoints, segmenter.at); err != nil {
			return err
		}
		if _, err := segmenter.tx.Exec(`UPDATE `+entityTables[entityType]+` SET deleted_at=?, updated_at=? WHERE id=?`,
			segmenter.at, segmenter.at, record.id); err != nil {
			return err
		}
		segmenter.record(PhoneSegmentAction{Action: "delete", EntityType: entityType, ID: record.id,
			StartedAt: record.startedAt, EndedAt: record.endedAt})
	}
	return nil
}
