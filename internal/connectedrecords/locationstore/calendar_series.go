package locationstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/kayushkin/location-store/internal/geonames"
)

// A recurring series with no location (a weekly class, a standing practice)
// says nothing about where by itself. But the user goes to most routines at
// one spot, so the times they went agree on it, and the times they skipped
// find them somewhere else or at home. matchUnlocatedSeries finds that spot
// from the visits alone and records what it found per series in
// calendar_series_places, linked or not, so a person can see how near a
// series came.

const (
	// seriesArrivalSeconds is how far from the start of a time of a series a
	// visit may begin and still count as going to it. A visit that began
	// hours before (a night somewhere, a day at work) shows where the user
	// was instead, so it counts against the spot, never for it.
	seriesArrivalSeconds = 45 * 60
	// seriesMinimumAgreement is the share of the times the user was out that
	// must find them at one spot before the series is taken to be held
	// there.
	seriesMinimumAgreement = 0.6
	// Confidence of an unlocated series' links runs from
	// unlocatedSeriesConfidenceBase, at no agreement, up by
	// unlocatedSeriesConfidencePerAgreement at full agreement: 0.64 at the
	// minimum share, 0.8 when every time out was at the spot.
	unlocatedSeriesConfidenceBase         = 0.4
	unlocatedSeriesConfidencePerAgreement = 0.4
)

// unlocatedSeries is one recurring series with no location, as the matching
// reads it, and what it found.
type unlocatedSeries struct {
	calendarID       string
	recurringEventID string
	summary          string
	instances        []*calendarEvent
	found            CalendarSeriesPlace
}

// unlocatedSeriesEvent says the event is a time of a recurring series with
// no location that the rule reads: timed, not canceled, a plan, not a call.
func unlocatedSeriesEvent(event *calendarEvent) bool {
	return event.recurring() && !event.physical && !event.online && event.flight == nil && !event.allDay &&
		event.event.Status != "cancelled" && !eventTypesThatAreNotPlans[event.event.EventType]
}

// outVisit says a visit can show where the user was for a time of a routine:
// not at home, and not away on a journey, where a routine at home is not
// held.
func (m *calendarMatcher) outVisit(visit *Visit) bool {
	if _, _, ok := m.visitCoordinate(visit); !ok {
		return false
	}
	return !m.atHome(visit) && !m.awayFromHome(visit)
}

// visitsForRoutineTime is every visit out that the user was at for the time:
// it began no later than seriesArrivalSeconds after the start and spans
// enough of it (enoughOverlap).
func (m *calendarMatcher) visitsForRoutineTime(event *calendarEvent) []*Visit {
	var found []*Visit
	for position := range m.visits {
		visit := &m.visits[position]
		if visit.StartedAt > event.startedAt+seriesArrivalSeconds || !enoughOverlap(event, visit.StartedAt, visit.EndedAt) || !m.outVisit(visit) {
			continue
		}
		found = append(found, visit)
	}
	return found
}

// arrivedFor says the visit began within seriesArrivalSeconds of the time's
// start: the user went there for it.
func arrivedFor(event *calendarEvent, visit *Visit) bool {
	return visit.StartedAt >= event.startedAt-seriesArrivalSeconds && visit.StartedAt <= event.startedAt+seriesArrivalSeconds
}

// spotAround is every place within placeReachMeters of the place, itself
// included, in id order: one building often has two Google place ids.
func (m *calendarMatcher) spotAround(center *Place) []*Place {
	var spot []*Place
	for _, place := range m.places {
		if place.DeletedAt == 0 && geonames.DistanceMeters(center.Latitude, center.Longitude, place.Latitude, place.Longitude) <= placeReachMeters {
			spot = append(spot, place)
		}
	}
	sort.Slice(spot, func(a, b int) bool { return spot[a].ID < spot[b].ID })
	return spot
}

// spotHoldsNoRoutine says a spot is home or work: a routine with no location
// that keeps finding the user there is one they do not go to.
func spotHoldsNoRoutine(spot []*Place) bool {
	for _, place := range spot {
		if place.Kind == PlaceKindHome || place.Kind == PlaceKindWork {
			return true
		}
	}
	return false
}

func (m *calendarMatcher) visitAtSpot(visit *Visit, center *Place, spot []*Place) bool {
	if visit.PlaceID != nil {
		for _, place := range spot {
			if place.ID == *visit.PlaceID {
				return true
			}
		}
	}
	latitude, longitude, ok := m.visitCoordinate(visit)
	return ok && geonames.DistanceMeters(center.Latitude, center.Longitude, latitude, longitude) <= placeReachMeters
}

func roundToHundredths(value float64) float64 { return math.Round(value*100) / 100 }

// matchUnlocatedSeries decides every recurring series with no location. Of
// the times the user was out, the spot they arrived at most (within
// seriesArrivalSeconds of the start) leads; the
// series is held there when it found them at least seriesMinimumInstances
// times and at least seriesMinimumAgreement of the times out, and is never
// home or work. Each time at the spot then links to the visit there that
// overlaps it most. It answers every series it read, linked or not.
func (m *calendarMatcher) matchUnlocatedSeries(events []*calendarEvent, windowFrom, windowTo int64) []*unlocatedSeries {
	bySeries := map[string]*unlocatedSeries{}
	var order []*unlocatedSeries
	for _, event := range events {
		if !unlocatedSeriesEvent(event) {
			continue
		}
		key := event.calendar.ID + "\x00" + event.event.RecurringEventID
		series := bySeries[key]
		if series == nil {
			series = &unlocatedSeries{calendarID: event.calendar.ID, recurringEventID: event.event.RecurringEventID}
			bySeries[key] = series
			order = append(order, series)
		}
		series.instances = append(series.instances, event)
	}
	for _, series := range order {
		sort.Slice(series.instances, func(a, b int) bool { return series.instances[a].startedAt < series.instances[b].startedAt })
		m.decideUnlocatedSeries(series, windowFrom, windowTo)
	}
	return order
}

func (m *calendarMatcher) decideUnlocatedSeries(series *unlocatedSeries, windowFrom, windowTo int64) {
	instances := series.instances
	last := instances[len(instances)-1]
	series.summary = last.event.Summary
	found := &series.found
	*found = CalendarSeriesPlace{CalendarID: series.calendarID, RecurringEventID: series.recurringEventID, Summary: series.summary,
		SpotPlaceIDs: []string{}, Instances: len(instances), FirstInstanceAt: instances[0].startedAt, LastInstanceAt: last.startedAt,
		WindowFrom: windowFrom, WindowTo: windowTo, MatchedBy: SetByCalendarMatching, Outcome: SeriesOutcomeNeverOut}

	visitsOut := make([][]*Visit, len(instances))
	candidateIDs := map[string]bool{}
	for position, event := range instances {
		visitsOut[position] = m.visitsForRoutineTime(event)
		if len(visitsOut[position]) > 0 {
			found.InstancesOut++
		}
		for _, visit := range visitsOut[position] {
			if visit.PlaceID != nil && m.places[*visit.PlaceID] != nil && arrivedFor(event, visit) {
				candidateIDs[*visit.PlaceID] = true
			}
		}
	}
	if found.InstancesOut == 0 {
		return
	}

	type candidate struct {
		center       *Place
		spot         []*Place
		instances    int // times with a visit at the spot
		visitsAtSelf int // arrivals naming the center itself: which of a spot's place ids leads
	}
	var best *candidate
	ids := make([]string, 0, len(candidateIDs))
	for id := range candidateIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		center := m.places[id]
		spot := m.spotAround(center)
		if spotHoldsNoRoutine(spot) {
			continue
		}
		current := &candidate{center: center, spot: spot}
		for position, event := range instances {
			atSpot := false
			for _, visit := range visitsOut[position] {
				if arrivedFor(event, visit) && m.visitAtSpot(visit, center, spot) {
					atSpot = true
				}
				if arrivedFor(event, visit) && visit.PlaceID != nil && *visit.PlaceID == id {
					current.visitsAtSelf++
				}
			}
			if atSpot {
				current.instances++
			}
		}
		if best == nil || current.instances > best.instances ||
			(current.instances == best.instances && current.visitsAtSelf > best.visitsAtSelf) {
			best = current
		}
	}
	if best == nil {
		// Out at its times, but never arriving for one where it could be held.
		found.Outcome = SeriesOutcomeTooFewTimes
		return
	}
	placeID := best.center.ID
	found.PlaceID, found.PlaceName = &placeID, best.center.Name
	for _, place := range best.spot {
		found.SpotPlaceIDs = append(found.SpotPlaceIDs, place.ID)
	}
	found.InstancesAtPlace = best.instances
	found.Agreement = roundToHundredths(float64(best.instances) / float64(found.InstancesOut))
	switch {
	case best.instances < seriesMinimumInstances:
		found.Outcome = SeriesOutcomeTooFewTimes
		return
	case float64(best.instances) < seriesMinimumAgreement*float64(found.InstancesOut):
		found.Outcome = SeriesOutcomeBelowAgreement
		return
	}
	found.Outcome = SeriesOutcomeLinked
	found.Confidence = roundToHundredths(unlocatedSeriesConfidenceBase + unlocatedSeriesConfidencePerAgreement*float64(best.instances)/float64(found.InstancesOut))
	for position, event := range instances {
		var chosen *Visit
		var chosenOverlap int64
		for _, visit := range visitsOut[position] {
			if !arrivedFor(event, visit) || !m.visitAtSpot(visit, best.center, best.spot) {
				continue
			}
			if overlap := overlapSeconds(event.startedAt, event.endedAt, visit.StartedAt, visit.EndedAt); chosen == nil || overlap > chosenOverlap {
				chosen, chosenOverlap = visit, overlap
			}
		}
		if chosen == nil {
			continue
		}
		event.unlocatedSeriesVisit = chosen
		event.unlocatedSeriesConfidence = found.Confidence
		event.unlocatedSeriesReason = fmt.Sprintf("one of a recurring series with no location; of the %d of its %d times the user was out (not home, not away), %d found them at %s (%s), %.0f%%; %s overlaps this one",
			found.InstancesOut, found.Instances, found.InstancesAtPlace, placeID, strings.Join(found.SpotPlaceIDs, ", "), found.Agreement*100, visitLabel(chosen))
	}
}

// ---- storage ----

const seriesPlaceColumns = `s.id, s.calendar_id, s.recurring_event_id, s.summary, s.place_id, COALESCE(p.name, ''), s.spot_place_ids,
	s.instances, s.instances_out, s.instances_at_place, s.agreement, s.confidence, s.outcome, s.first_instance_at, s.last_instance_at,
	s.window_from, s.window_to, s.matched_by, s.created_at, s.updated_at`

const seriesPlaceFrom = ` FROM calendar_series_places s LEFT JOIN places p ON p.id = s.place_id`

func scanSeriesPlace(scan func(...any) error) (*CalendarSeriesPlace, error) {
	var row CalendarSeriesPlace
	var placeID sql.NullString
	var spot string
	if err := scan(&row.ID, &row.CalendarID, &row.RecurringEventID, &row.Summary, &placeID, &row.PlaceName, &spot, &row.Instances,
		&row.InstancesOut, &row.InstancesAtPlace, &row.Agreement, &row.Confidence, &row.Outcome, &row.FirstInstanceAt, &row.LastInstanceAt,
		&row.WindowFrom, &row.WindowTo, &row.MatchedBy, &row.CreatedAt, &row.UpdatedAt); err != nil {
		return nil, err
	}
	row.PlaceID = stringPointer(placeID)
	if err := json.Unmarshal([]byte(spot), &row.SpotPlaceIDs); err != nil {
		return nil, fmt.Errorf("calendar series place %s has spot_place_ids that are not a JSON array: %w", row.ID, err)
	}
	return &row, nil
}

// saveSeriesPlaces writes what a run found for each series, over what an
// earlier run wrote for the same series, and answers the rows with their ids.
func saveSeriesPlaces(tx *sql.Tx, series []*unlocatedSeries, at int64) error {
	for _, one := range series {
		found := &one.found
		var id string
		err := tx.QueryRow(`SELECT id FROM calendar_series_places WHERE calendar_id = ? AND recurring_event_id = ?`,
			found.CalendarID, found.RecurringEventID).Scan(&id)
		values := []any{found.Summary, found.PlaceID, encodeStrings(found.SpotPlaceIDs), found.Instances, found.InstancesOut,
			found.InstancesAtPlace, found.Agreement, found.Confidence, found.Outcome, found.FirstInstanceAt, found.LastInstanceAt,
			found.WindowFrom, found.WindowTo, found.MatchedBy, at}
		switch {
		case errors.Is(err, sql.ErrNoRows):
			var seq int64
			if id, seq, err = nextID(tx, "calendar_series_places", "calendar_series_place"); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO calendar_series_places (summary, place_id, spot_place_ids, instances, instances_out,
				instances_at_place, agreement, confidence, outcome, first_instance_at, last_instance_at, window_from, window_to, matched_by,
				updated_at, id, seq, calendar_id, recurring_event_id, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				append(values, id, seq, found.CalendarID, found.RecurringEventID, at)...); err != nil {
				return err
			}
			found.CreatedAt = at
		case err != nil:
			return err
		default:
			if _, err := tx.Exec(`UPDATE calendar_series_places SET summary=?, place_id=?, spot_place_ids=?, instances=?, instances_out=?,
				instances_at_place=?, agreement=?, confidence=?, outcome=?, first_instance_at=?, last_instance_at=?, window_from=?, window_to=?,
				matched_by=?, updated_at=? WHERE id=?`, append(values, id)...); err != nil {
				return err
			}
		}
		found.ID, found.UpdatedAt = id, at
	}
	return nil
}

// CalendarSeriesPlaceFilter selects rows for GET /calendar-series-places,
// most times at the spot first.
type CalendarSeriesPlaceFilter struct {
	PlaceID       string
	CalendarID    string
	Outcome       string
	Limit, Offset int
}

// ListCalendarSeriesPlaces answers GET /calendar-series-places. PlaceID
// selects the series whose spot holds that place.
func (s *Store) ListCalendarSeriesPlaces(filter CalendarSeriesPlaceFilter) ([]CalendarSeriesPlace, error) {
	where, args := []string{}, []any{}
	if filter.PlaceID != "" {
		where = append(where, "EXISTS (SELECT 1 FROM json_each(s.spot_place_ids) WHERE value = ?)")
		args = append(args, filter.PlaceID)
	}
	if filter.CalendarID != "" {
		where, args = append(where, "s.calendar_id = ?"), append(args, filter.CalendarID)
	}
	if filter.Outcome != "" {
		if !knownWord(SeriesOutcomes, filter.Outcome) {
			return nil, fmt.Errorf("%w: unknown outcome %q: use one of %s", ErrInvalid, filter.Outcome, strings.Join(SeriesOutcomes, ", "))
		}
		where, args = append(where, "s.outcome = ?"), append(args, filter.Outcome)
	}
	query := `SELECT ` + seriesPlaceColumns + seriesPlaceFrom
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` ORDER BY s.instances_at_place DESC, s.seq LIMIT ? OFFSET ?`
	args = append(args, clampLimit(filter.Limit, defaultLimit, maximumLimit), max(filter.Offset, 0))
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := []CalendarSeriesPlace{}
	for rows.Next() {
		row, err := scanSeriesPlace(rows.Scan)
		if err != nil {
			return nil, err
		}
		found = append(found, *row)
	}
	return found, rows.Err()
}

// GetCalendarSeriesPlace reads one row.
func (s *Store) GetCalendarSeriesPlace(id string) (*CalendarSeriesPlace, error) {
	row, err := scanSeriesPlace(s.db.QueryRow(`SELECT `+seriesPlaceColumns+seriesPlaceFrom+` WHERE s.id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no calendar series place %s", ErrNotFound, id)
	}
	return row, err
}
