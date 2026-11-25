package locationstore

import (
	"database/sql"
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

// Calendar event links tie a Google Calendar event to the visit or trip it
// explains. Every decision is here; location-match-calendar only reads the
// events from dash and prints what POST /calendar/match did. Nothing here
// sends an address or a coordinate anywhere: an event's location is matched
// against the places already in the store and against the GeoNames towns on
// disk.

// Confidences of the rules, from strongest to weakest.
const (
	confidencePlace         = 0.9
	confidenceFlight        = 0.9
	confidenceFlightJourney = 0.8
	confidenceSeriesPlace   = 0.7
	confidenceNoFlightTrip  = 0.6
	confidenceTownVisit     = 0.5
	confidenceTownJourney   = 0.5
	confidenceJourneyTime   = 0.4
	confidenceHand          = 1
)

const (
	// placeReachMeters is how near a place a visit must be to count as there,
	// when the place gives no radius of its own.
	placeReachMeters = 150
	// townSlackMeters is added to a town's reach (never less than
	// townMinimumReachMeters) when asking whether a visit was in it.
	townSlackMeters        = 3000
	townMinimumReachMeters = 5000
	// arrivalWindowSeconds is how near an event's start a visit must begin
	// for a single event matched only by its town.
	arrivalWindowSeconds = 3600
	// seriesMinimumInstances is how many times of a recurring series must
	// find the user at one place before the series is taken to be held there.
	seriesMinimumInstances = 3
	// loadMarginSeconds widens the window the visits and trips are read for,
	// so an event at its edge still finds them.
	loadMarginSeconds = 2 * 86400
)

// Actions of a matching run on one link.
const (
	CalendarLinkCreate      = "create"
	CalendarLinkUpdate      = "update"
	CalendarLinkUnchanged   = "unchanged"
	CalendarLinkRestore     = "restore"
	CalendarLinkDelete      = "delete"
	CalendarLinkKeptDeleted = "kept_deleted"
)

// Why an event linked to nothing, as CalendarMatching.Unlinked counts it.
const (
	unlinkedCanceled            = "cancelled"
	unlinkedEventType           = "not_a_plan" // a birthday, working location, out of office or focus time
	unlinkedOnline              = "online_meeting"
	unlinkedFlightNotRecorded   = "flight_not_recorded"
	unlinkedPlaceNotVisited     = "place_named_not_visited"
	unlinkedLocationNotPlaced   = "location_names_no_known_place_or_town"
	unlinkedRoutineTownOnly     = "routine_not_at_one_place"
	unlinkedTownNotVisited      = "town_named_not_visited"
	unlinkedRoutineNoLocation   = "routine_without_location_not_at_its_spot"
	unlinkedReadOnlyNoLocation  = "no_location_on_calendar_user_only_reads"
	unlinkedNoLocationNoJourney = "no_location_no_journey"
)

// eventTypesThatAreNotPlans are Google event types that say nothing about
// where the user went.
var eventTypesThatAreNotPlans = map[string]bool{"birthday": true, "workingLocation": true, "outOfOffice": true, "focusTime": true}

// calendarRolesTheUserWrites are the access roles of a calendar that holds
// the user's own plans.
var calendarRolesTheUserWrites = map[string]bool{"owner": true, "writer": true}

var (
	// flightPattern is the summary Gmail gives a flight it finds in a
	// booking: "Flight to Dallas (AA 1361)".
	flightPattern = regexp.MustCompile(`(?i)^\s*flight to (.+?)\s*\(\s*([A-Z0-9]{2})\s*(\d{1,4})\s*\)\s*$`)
	// onlineMeetingPattern finds a location that is a video call.
	onlineMeetingPattern = regexp.MustCompile(`(?i)\b(microsoft teams|teams|zoom|google meet|webex|skype)\b|meet\.google\.com|zoom\.us|teams\.microsoft\.com`)
	// linkOnlyPattern is a location that is only a web address: it says
	// nothing about where.
	linkOnlyPattern = regexp.MustCompile(`(?i)^\s*https?://\S+\s*$`)
	// locationSeparators split a location into its parts: "Venue, 1 Main
	// St, Town, ST 00000" and "Room (Town, ST, US); Other room".
	locationSeparators = strings.NewReplacer(";", ",", "(", ",", ")", ",", "\n", ",")
)

// ---- storage ----

const calendarLinkColumns = `id, calendar_id, event_id, recurring_event_id, summary, location, started_at, ended_at, all_day,
	target_type, target_id, rule, confidence, reason, matched_by, created_at, updated_at, deleted_at, deleted_by`

func scanCalendarEventLink(scan func(...any) error) (*CalendarEventLink, error) {
	var link CalendarEventLink
	var allDay int
	err := scan(&link.ID, &link.CalendarID, &link.EventID, &link.RecurringEventID, &link.Summary, &link.Location, &link.StartedAt,
		&link.EndedAt, &allDay, &link.TargetType, &link.TargetID, &link.Rule, &link.Confidence, &link.Reason, &link.MatchedBy,
		&link.CreatedAt, &link.UpdatedAt, &link.DeletedAt, &link.DeletedBy)
	if err != nil {
		return nil, err
	}
	link.AllDay = allDay != 0
	return &link, nil
}

func queryCalendarEventLinks(q queryer, query string, args ...any) ([]CalendarEventLink, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []CalendarEventLink{}
	for rows.Next() {
		link, err := scanCalendarEventLink(rows.Scan)
		if err != nil {
			return nil, err
		}
		links = append(links, *link)
	}
	return links, rows.Err()
}

func getCalendarEventLinkRow(q queryer, id string) (*CalendarEventLink, error) {
	link, err := scanCalendarEventLink(q.QueryRow(`SELECT `+calendarLinkColumns+` FROM calendar_event_links WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no calendar event link %s", ErrNotFound, id)
	}
	return link, err
}

// GetCalendarEventLink reads one link. A deleted one is not found unless
// asked for.
func (s *Store) GetCalendarEventLink(id string, includeDeleted bool) (*CalendarEventLink, error) {
	link, err := getCalendarEventLinkRow(s.db, id)
	if err != nil {
		return nil, err
	}
	if link.DeletedAt != 0 && !includeDeleted {
		return nil, fmt.Errorf("%w: calendar event link %s is deleted", ErrNotFound, id)
	}
	return link, nil
}

// CalendarEventLinkFilter selects links for GET /calendar-event-links,
// earliest event first. From and To select events that overlap them.
type CalendarEventLinkFilter struct {
	From, To   int64
	VisitID    string
	TripID     string
	PlaceID    string
	CalendarID string
	EventID    string
	Rule       string
	MatchedBy  string
	// IncludeDeleted shows deleted links too.
	IncludeDeleted bool
	Limit, Offset  int
}

// ListCalendarEventLinks answers GET /calendar-event-links. PlaceID selects
// the links to the place and to every visit at it.
func (s *Store) ListCalendarEventLinks(filter CalendarEventLinkFilter) ([]CalendarEventLink, error) {
	where, args := []string{}, []any{}
	if !filter.IncludeDeleted {
		where = append(where, "deleted_at = 0")
	}
	if filter.From > 0 {
		where, args = append(where, "ended_at >= ?"), append(args, filter.From)
	}
	if filter.To > 0 {
		where, args = append(where, "started_at < ?"), append(args, filter.To)
	}
	if filter.VisitID != "" {
		where, args = append(where, "target_type = 'visit' AND target_id = ?"), append(args, filter.VisitID)
	}
	if filter.TripID != "" {
		where, args = append(where, "target_type = 'trip' AND target_id = ?"), append(args, filter.TripID)
	}
	if filter.PlaceID != "" {
		where = append(where, "((target_type = 'place' AND target_id = ?) OR (target_type = 'visit' AND target_id IN (SELECT id FROM visits WHERE place_id = ?)))")
		args = append(args, filter.PlaceID, filter.PlaceID)
	}
	for column, value := range map[string]string{"calendar_id": filter.CalendarID, "event_id": filter.EventID, "matched_by": filter.MatchedBy} {
		if value != "" {
			where, args = append(where, column+" = ?"), append(args, value)
		}
	}
	if filter.Rule != "" {
		if !knownWord(CalendarMatchRules, filter.Rule) {
			return nil, fmt.Errorf("%w: unknown rule %q: use one of %s", ErrInvalid, filter.Rule, strings.Join(CalendarMatchRules, ", "))
		}
		where, args = append(where, "rule = ?"), append(args, filter.Rule)
	}
	query := `SELECT ` + calendarLinkColumns + ` FROM calendar_event_links`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` ORDER BY started_at, seq LIMIT ? OFFSET ?`
	args = append(args, clampLimit(filter.Limit, defaultLimit, maximumLimit), max(filter.Offset, 0))
	return queryCalendarEventLinks(s.db, query, args...)
}

// liveCalendarEventLinksTo is every live link to any of the records named,
// earliest event first.
func (s *Store) liveCalendarEventLinksTo(targetIDs []string) ([]CalendarEventLink, error) {
	encoded, err := json.Marshal(targetIDs)
	if err != nil {
		return nil, err
	}
	return queryCalendarEventLinks(s.db, `SELECT `+calendarLinkColumns+` FROM calendar_event_links
		WHERE deleted_at = 0 AND target_id IN (SELECT value FROM json_each(?)) ORDER BY started_at, seq`, string(encoded))
}

// requireLinkTarget refuses a target that is not a live visit, trip or place.
func requireLinkTarget(q queryer, targetType, targetID string) error {
	var deletedAt int64
	var err error
	switch targetType {
	case EntityVisit:
		var visit *Visit
		if visit, err = getVisitRow(q, targetID); err == nil {
			deletedAt = visit.DeletedAt
		}
	case EntityTrip:
		var trip *Trip
		if trip, err = getTripRow(q, targetID); err == nil {
			deletedAt = trip.DeletedAt
		}
	case EntityPlace:
		var place *Place
		if place, err = getPlaceRow(q, targetID); err == nil {
			deletedAt = place.DeletedAt
		}
	default:
		return fmt.Errorf("%w: target_type %q: use one of %s", ErrInvalid, targetType, strings.Join(CalendarLinkTargetTypes, ", "))
	}
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: target %s %s does not exist", ErrInvalid, targetType, targetID)
	}
	if err != nil {
		return err
	}
	if deletedAt != 0 {
		return fmt.Errorf("%w: target %s %s is deleted", ErrInvalid, targetType, targetID)
	}
	return nil
}

// CreateCalendarEventLink makes a link by hand. A link between the same
// event and record already there, made by the matcher or deleted, becomes
// this one: live, rule hand, confidence 1, so no matching run changes it.
func (s *Store) CreateCalendarEventLink(create CalendarEventLinkCreate) (*CalendarEventLink, error) {
	if strings.TrimSpace(create.CalendarID) == "" || strings.TrimSpace(create.EventID) == "" {
		return nil, fmt.Errorf("%w: calendar_id and event_id are required: the link is found again by them", ErrInvalid)
	}
	if err := checkTimeRange(create.StartedAt, create.EndedAt); err != nil {
		return nil, err
	}
	var id string
	err := s.inTransaction(func(tx *sql.Tx) error {
		if err := requireLinkTarget(tx, create.TargetType, create.TargetID); err != nil {
			return err
		}
		link := CalendarEventLink{CalendarID: create.CalendarID, EventID: create.EventID, RecurringEventID: create.RecurringEventID,
			Summary: create.Summary, Location: create.Location, StartedAt: create.StartedAt, EndedAt: create.EndedAt, AllDay: create.AllDay,
			TargetType: create.TargetType, TargetID: create.TargetID, Rule: CalendarRuleHand, Confidence: confidenceHand,
			Reason: create.Reason, MatchedBy: SetByHand}
		existing, err := findCalendarEventLink(tx, link)
		if err != nil {
			return err
		}
		at := now()
		if existing == nil {
			id, err = insertCalendarEventLink(tx, link, at)
			return err
		}
		id = existing.ID
		return updateCalendarEventLink(tx, existing.ID, link, at)
	})
	if err != nil {
		return nil, err
	}
	return getCalendarEventLinkRow(s.db, id)
}

// DeleteCalendarEventLink soft-deletes a link, as a person's judgment: a
// matching run never makes it again. Deleting a deleted link is a no-op.
func (s *Store) DeleteCalendarEventLink(id string) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		link, err := getCalendarEventLinkRow(tx, id)
		if err != nil {
			return err
		}
		if link.DeletedAt != 0 {
			return nil
		}
		_, err = tx.Exec(`UPDATE calendar_event_links SET deleted_at = ?, deleted_by = ?, updated_at = ? WHERE id = ?`, now(), SetByHand, now(), id)
		return err
	})
}

func findCalendarEventLink(q queryer, link CalendarEventLink) (*CalendarEventLink, error) {
	found, err := scanCalendarEventLink(q.QueryRow(`SELECT `+calendarLinkColumns+` FROM calendar_event_links
		WHERE calendar_id = ? AND event_id = ? AND target_type = ? AND target_id = ?`,
		link.CalendarID, link.EventID, link.TargetType, link.TargetID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return found, err
}

func boolInteger(value bool) int {
	if value {
		return 1
	}
	return 0
}

func insertCalendarEventLink(tx *sql.Tx, link CalendarEventLink, at int64) (string, error) {
	id, seq, err := nextID(tx, "calendar_event_links", "calendar_event_link")
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(`INSERT INTO calendar_event_links (id, seq, calendar_id, event_id, recurring_event_id, summary, location, started_at,
		ended_at, all_day, target_type, target_id, rule, confidence, reason, matched_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, seq, link.CalendarID, link.EventID, link.RecurringEventID, link.Summary, link.Location, link.StartedAt, link.EndedAt,
		boolInteger(link.AllDay), link.TargetType, link.TargetID, link.Rule, link.Confidence, link.Reason, link.MatchedBy, at, at)
	return id, err
}

// updateCalendarEventLink writes every field of link onto the row, live.
func updateCalendarEventLink(tx *sql.Tx, id string, link CalendarEventLink, at int64) error {
	_, err := tx.Exec(`UPDATE calendar_event_links SET recurring_event_id = ?, summary = ?, location = ?, started_at = ?, ended_at = ?,
		all_day = ?, rule = ?, confidence = ?, reason = ?, matched_by = ?, updated_at = ?, deleted_at = 0, deleted_by = '' WHERE id = ?`,
		link.RecurringEventID, link.Summary, link.Location, link.StartedAt, link.EndedAt, boolInteger(link.AllDay), link.Rule,
		link.Confidence, link.Reason, link.MatchedBy, at, id)
	return err
}

// sameFoundLink says whether a stored link already says what a run found.
func sameFoundLink(stored, found CalendarEventLink) bool {
	return stored.RecurringEventID == found.RecurringEventID && stored.Summary == found.Summary && stored.Location == found.Location &&
		stored.StartedAt == found.StartedAt && stored.EndedAt == found.EndedAt && stored.AllDay == found.AllDay &&
		stored.Rule == found.Rule && stored.Confidence == found.Confidence && stored.Reason == found.Reason
}

func calendarLinkIdentity(link CalendarEventLink) string {
	return link.CalendarID + "\x00" + link.EventID + "\x00" + link.TargetType + "\x00" + link.TargetID
}

// ---- matching ----

// calendarEvent is one event as the matching reads it.
type calendarEvent struct {
	calendar  *GoogleCalendar
	event     GoogleCalendarEvent
	startedAt int64
	endedAt   int64
	allDay    bool
	// physical says the location names somewhere: it is set, is not a video
	// call, and is not only a web address.
	physical bool
	online   bool
	flight   []string // destination, airline, number
	places   []*Place // the known places the location names
	towns    []geonames.City
	townName string
	// seriesVisit is the visit at the place its recurring series is held,
	// when matchSeries found one; seriesReason says why.
	seriesVisit  *Visit
	seriesReason string
	// unlocatedSeriesVisit is the visit at the spot its recurring series
	// with no location is held, when matchUnlocatedSeries found one.
	unlocatedSeriesVisit      *Visit
	unlocatedSeriesReason     string
	unlocatedSeriesConfidence float64
}

func (event *calendarEvent) recurring() bool { return event.event.RecurringEventID != "" }

func (event *calendarEvent) link(targetType, targetID, rule string, confidence float64, reason string) CalendarEventLink {
	return CalendarEventLink{CalendarID: event.calendar.ID, EventID: event.event.ID, RecurringEventID: event.event.RecurringEventID,
		Summary: event.event.Summary, Location: event.event.Location, StartedAt: event.startedAt, EndedAt: event.endedAt,
		AllDay: event.allDay, TargetType: targetType, TargetID: targetID, Rule: rule, Confidence: confidence, Reason: reason,
		MatchedBy: SetByCalendarMatching}
}

type calendarMatcher struct {
	index    *geonames.Index
	places   map[string]*Place
	named    []*Place // places with a name or an address
	visits   []Visit
	trips    []Trip
	tripByID map[string]*Trip
	homes    []HomePeriod
	found    []CalendarEventLink
	unlinked map[string]int
}

// MatchCalendar ties the request's events to the visits and trips they
// explain, and settles the run's own links of those calendars in the
// window: new ones made, changed ones rewritten, ones no longer found
// deleted. A link made by hand, or one a person deleted, is never changed.
func (s *Store) MatchCalendar(request CalendarMatchRequest) (*CalendarMatching, error) {
	if request.From <= 0 || request.To <= request.From {
		return nil, fmt.Errorf("%w: from and to (epoch seconds) must be set, from before to; got %d to %d", ErrInvalid, request.From, request.To)
	}
	index, err := s.townIndex()
	if err != nil {
		return nil, err
	}
	matcher, err := s.loadCalendarMatcher(index, request.From-loadMarginSeconds, request.To+loadMarginSeconds)
	if err != nil {
		return nil, err
	}
	events, err := matcher.readEvents(request)
	if err != nil {
		return nil, err
	}
	matcher.matchSeries(events)
	unlocated := matcher.matchUnlocatedSeries(events, request.From, request.To)
	for _, event := range events {
		matcher.match(event)
	}
	matching := &CalendarMatching{DryRun: request.DryRun, From: request.From, To: request.To, EventsRead: len(events),
		LinksByRule: map[string]int{}, Unlinked: matcher.unlinked, Actions: map[string]int{}, Links: []PlannedCalendarEventLink{},
		SeriesPlaces: []CalendarSeriesPlace{}, Notes: []string{}}
	for _, link := range matcher.found {
		matching.LinksByRule[link.Rule]++
	}
	calendarIDs := []string{}
	for _, calendar := range request.Calendars {
		calendarIDs = append(calendarIDs, calendar.Calendar.ID)
	}
	err = s.inTransaction(func(tx *sql.Tx) error {
		if err := settleCalendarLinks(tx, matching, matcher.found, calendarIDs, request); err != nil {
			return err
		}
		if request.DryRun {
			return nil
		}
		return saveSeriesPlaces(tx, unlocated, now())
	})
	if err != nil {
		return nil, err
	}
	for _, series := range unlocated {
		matching.SeriesPlaces = append(matching.SeriesPlaces, series.found)
	}
	if len(matcher.homes) == 0 {
		matching.Notes = append(matching.Notes, "no home periods: no visit counts as home, so a routine at home could match by town; run location-group-journeys first")
	}
	return matching, nil
}

func (s *Store) loadCalendarMatcher(index *geonames.Index, from, to int64) (*calendarMatcher, error) {
	matcher := &calendarMatcher{index: index, places: map[string]*Place{}, tripByID: map[string]*Trip{}, unlinked: map[string]int{}}
	places, err := s.listPlacesUncapped()
	if err != nil {
		return nil, err
	}
	for position := range places {
		place := &places[position]
		matcher.places[place.ID] = place
		if strings.TrimSpace(place.Name) != "" || strings.TrimSpace(place.Address) != "" {
			matcher.named = append(matcher.named, place)
		}
	}
	if matcher.visits, err = s.listVisitsUncapped(VisitFilter{TimeFilter: TimeFilter{From: from, To: to, Limit: math.MaxInt32}}); err != nil {
		return nil, err
	}
	if matcher.trips, err = s.listTripsUncapped(TripFilter{TimeFilter: TimeFilter{From: from, To: to, Limit: math.MaxInt32}}); err != nil {
		return nil, err
	}
	for position := range matcher.trips {
		matcher.tripByID[matcher.trips[position].ID] = &matcher.trips[position]
	}
	// A journey's parent may start before the window; read the ones named.
	for _, trip := range matcher.trips {
		if trip.ParentTripID == nil || matcher.tripByID[*trip.ParentTripID] != nil {
			continue
		}
		parent, err := s.GetTrip(*trip.ParentTripID, false)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		matcher.tripByID[parent.ID] = parent
	}
	if matcher.homes, err = s.ListHomePeriods(false); err != nil {
		return nil, err
	}
	sort.Slice(matcher.homes, func(a, b int) bool { return matcher.homes[a].StartedAt < matcher.homes[b].StartedAt })
	return matcher, nil
}

func (s *Store) listPlacesUncapped() ([]Place, error) {
	var all []Place
	for offset := 0; ; offset += maximumLimit {
		places, err := s.ListPlaces(PlaceFilter{Limit: maximumLimit, Offset: offset})
		if err != nil {
			return nil, err
		}
		all = append(all, places...)
		if len(places) < maximumLimit {
			return all, nil
		}
	}
}

// eventTime reads a Google event time: an instant, or a local day in the
// time's own zone, else the calendar's, else UTC.
func eventTime(value GoogleEventTime, calendar *GoogleCalendar) (int64, bool, error) {
	if value.DateTime != "" {
		at, err := time.Parse(time.RFC3339, value.DateTime)
		if err != nil {
			return 0, false, fmt.Errorf("dateTime %q: %w", value.DateTime, err)
		}
		return at.Unix(), false, nil
	}
	if value.Date == "" {
		return 0, false, errors.New("neither dateTime nor date is set")
	}
	zoneName := value.TimeZone
	if zoneName == "" {
		zoneName = calendar.TimeZone
	}
	zone := time.UTC
	if zoneName != "" {
		loaded, err := time.LoadLocation(zoneName)
		if err != nil {
			return 0, false, fmt.Errorf("time zone %q: %w", zoneName, err)
		}
		zone = loaded
	}
	day, err := time.ParseInLocation("2006-01-02", value.Date, zone)
	if err != nil {
		return 0, false, fmt.Errorf("date %q: %w", value.Date, err)
	}
	return day.Unix(), true, nil
}

func (m *calendarMatcher) readEvents(request CalendarMatchRequest) ([]*calendarEvent, error) {
	var events []*calendarEvent
	for position := range request.Calendars {
		calendar := &request.Calendars[position].Calendar
		if strings.TrimSpace(calendar.ID) == "" {
			return nil, fmt.Errorf("%w: calendar %d has no id", ErrInvalid, position)
		}
		for _, raw := range request.Calendars[position].Events {
			if strings.TrimSpace(raw.ID) == "" {
				return nil, fmt.Errorf("%w: an event of calendar %s has no id", ErrInvalid, calendar.ID)
			}
			startedAt, allDay, err := eventTime(raw.Start, calendar)
			if err != nil {
				return nil, fmt.Errorf("%w: event %s of calendar %s: start: %v", ErrInvalid, raw.ID, calendar.ID, err)
			}
			endedAt, _, err := eventTime(raw.End, calendar)
			if err != nil {
				return nil, fmt.Errorf("%w: event %s of calendar %s: end: %v", ErrInvalid, raw.ID, calendar.ID, err)
			}
			if endedAt < startedAt {
				return nil, fmt.Errorf("%w: event %s of calendar %s ends before it starts", ErrInvalid, raw.ID, calendar.ID)
			}
			event := &calendarEvent{calendar: calendar, event: raw, startedAt: startedAt, endedAt: endedAt, allDay: allDay}
			location := strings.TrimSpace(raw.Location)
			event.online = onlineMeetingPattern.MatchString(location) || (location == "" && raw.HangoutLink != "")
			event.physical = location != "" && !event.online && !linkOnlyPattern.MatchString(location)
			if match := flightPattern.FindStringSubmatch(raw.Summary); match != nil {
				event.flight = match[1:]
			}
			if event.physical {
				event.places = m.placesNamedIn(location)
				event.towns, event.townName = m.townsNamedIn(location)
			}
			events = append(events, event)
		}
	}
	return events, nil
}

// normalizedWords lowercases text and keeps only its letters and digits,
// one space between words, with a space at each end so a phrase is found
// only as whole words.
func normalizedWords(text string) string {
	var builder strings.Builder
	for _, character := range strings.ToLower(text) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character > 127 {
			builder.WriteRune(character)
		} else {
			builder.WriteRune(' ')
		}
	}
	return " " + strings.Join(strings.Fields(builder.String()), " ") + " "
}

func locationParts(location string) []string {
	var parts []string
	for _, part := range strings.Split(locationSeparators.Replace(location), ",") {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

// placesNamedIn is every known place the location names: by the whole of
// one of its parts being the place's name, or by holding the street line of
// the place's address (and its town, when the address has one).
func (m *calendarMatcher) placesNamedIn(location string) []*Place {
	whole := normalizedWords(location)
	parts := map[string]bool{}
	for _, part := range locationParts(location) {
		parts[normalizedWords(part)] = true
	}
	var found []*Place
	for _, place := range m.named {
		name := normalizedWords(place.Name)
		if len(strings.TrimSpace(name)) >= 3 && parts[name] {
			found = append(found, place)
			continue
		}
		addressParts := locationParts(place.Address)
		if len(addressParts) == 0 {
			continue
		}
		street := normalizedWords(addressParts[0])
		if len(strings.TrimSpace(street)) < 6 || !strings.ContainsAny(street, "0123456789") || !strings.Contains(whole, street) {
			continue
		}
		if len(addressParts) > 1 && !strings.Contains(whole, normalizedWords(addressParts[1])) {
			continue
		}
		found = append(found, place)
	}
	return found
}

// withoutNumbers drops every word holding a digit: "NV 89502" is "NV".
func withoutNumbers(part string) string {
	var kept []string
	for _, word := range strings.Fields(part) {
		if !strings.ContainsAny(word, "0123456789") {
			kept = append(kept, word)
		}
	}
	return strings.Join(kept, " ")
}

// townsNamedIn finds the town a location names, offline: the first part
// after the first (which names the venue) that is a GeoNames town's whole
// name, narrowed by the part after it when that names the town's region
// (NV, Nevada). It answers every town of that name: which one is meant is
// settled by where the user was.
func (m *calendarMatcher) townsNamedIn(location string) ([]geonames.City, string) {
	parts := locationParts(location)
	for position := 1; position < len(parts); position++ {
		name := withoutNumbers(parts[position])
		if name == "" {
			continue
		}
		towns := m.index.Named(name)
		if len(towns) == 0 {
			continue
		}
		if position+1 < len(parts) {
			region := strings.ToLower(withoutNumbers(parts[position+1]))
			var inRegion []geonames.City
			for _, town := range towns {
				if region != "" && (strings.ToLower(town.RegionCode) == region || strings.ToLower(m.index.RegionName(town)) == region) {
					inRegion = append(inRegion, town)
				}
			}
			if len(inRegion) > 0 {
				towns = inRegion
			}
		}
		return towns, name
	}
	return nil, ""
}

func overlapSeconds(startA, endA, startB, endB int64) int64 {
	return min(endA, endB) - max(startA, startB)
}

// enoughOverlap says a record spans enough of an event to have been there
// for it: half the event, or half an hour of a longer one. An event with no
// length must fall inside the record.
func enoughOverlap(event *calendarEvent, startedAt, endedAt int64) bool {
	length := event.endedAt - event.startedAt
	if length <= 0 {
		return startedAt <= event.startedAt && event.startedAt < endedAt
	}
	overlap := overlapSeconds(event.startedAt, event.endedAt, startedAt, endedAt)
	return overlap > 0 && overlap >= min(1800, length/2)
}

func (m *calendarMatcher) visitCoordinate(visit *Visit) (float64, float64, bool) {
	if visit.Latitude != nil && visit.Longitude != nil {
		return *visit.Latitude, *visit.Longitude, true
	}
	if visit.PlaceID != nil {
		if place := m.places[*visit.PlaceID]; place != nil {
			return place.Latitude, place.Longitude, true
		}
	}
	return 0, 0, false
}

// atHome says a visit was at the home: a place of kind home, or within the
// grouping's own site distance of the home period in force.
func (m *calendarMatcher) atHome(visit *Visit) bool {
	if visit.PlaceID != nil {
		if place := m.places[*visit.PlaceID]; place != nil && place.Kind == PlaceKindHome {
			return true
		}
	}
	if len(m.homes) == 0 {
		return false
	}
	latitude, longitude, ok := m.visitCoordinate(visit)
	if !ok {
		return false
	}
	home := m.homeAt(visit.StartedAt)
	return geonames.DistanceMeters(home.Latitude, home.Longitude, latitude, longitude) <= sameSiteMeters
}

// awayFromHome says a visit was farther from the home in force than the
// grouping counts as away: a journey passing through the home town on its
// way out says nothing about an event there.
func (m *calendarMatcher) awayFromHome(visit *Visit) bool {
	latitude, longitude, ok := m.visitCoordinate(visit)
	if !ok || len(m.homes) == 0 {
		return ok
	}
	home := m.homeAt(visit.StartedAt)
	return geonames.DistanceMeters(home.Latitude, home.Longitude, latitude, longitude) > DefaultAwayDistanceMeters
}

func (m *calendarMatcher) homeAt(at int64) *HomePeriod {
	for position := len(m.homes) - 1; position >= 0; position-- {
		if m.homes[position].StartedAt <= at {
			return &m.homes[position]
		}
	}
	return &m.homes[0]
}

func (m *calendarMatcher) visitNearPlaces(visit *Visit, places []*Place) bool {
	latitude, longitude, ok := m.visitCoordinate(visit)
	for _, place := range places {
		if visit.PlaceID != nil && *visit.PlaceID == place.ID {
			return true
		}
		if ok && geonames.DistanceMeters(place.Latitude, place.Longitude, latitude, longitude) <= max(place.RadiusMeters, placeReachMeters) {
			return true
		}
	}
	return false
}

func (m *calendarMatcher) visitInTown(visit *Visit, towns []geonames.City, townName string) bool {
	if visit.Locality != nil && strings.EqualFold(visit.Locality.City, townName) {
		return true
	}
	latitude, longitude, ok := m.visitCoordinate(visit)
	if !ok {
		return false
	}
	for _, town := range towns {
		reach := max(geonames.ReachMeters(town), townMinimumReachMeters) + townSlackMeters
		if geonames.DistanceMeters(town.Latitude, town.Longitude, latitude, longitude) <= reach {
			return true
		}
	}
	return false
}

// bestVisit is the visit that overlaps the event most, of those accept
// takes; nil when none does.
func (m *calendarMatcher) bestVisit(event *calendarEvent, accept func(*Visit) bool) *Visit {
	var best *Visit
	var bestOverlap int64
	for position := range m.visits {
		visit := &m.visits[position]
		if !enoughOverlap(event, visit.StartedAt, visit.EndedAt) || !accept(visit) {
			continue
		}
		if overlap := overlapSeconds(event.startedAt, event.endedAt, visit.StartedAt, visit.EndedAt); best == nil || overlap > bestOverlap {
			best, bestOverlap = visit, overlap
		}
	}
	return best
}

// bestTrip is the trip that overlaps the event most, of those accept takes.
// strict asks for enoughOverlap; otherwise any overlap will do.
func (m *calendarMatcher) bestTrip(event *calendarEvent, strict bool, accept func(*Trip) bool) *Trip {
	var best *Trip
	var bestOverlap int64
	for position := range m.trips {
		trip := &m.trips[position]
		overlap := overlapSeconds(event.startedAt, event.endedAt, trip.StartedAt, trip.EndedAt)
		if event.endedAt == event.startedAt {
			overlap = boolSeconds(trip.StartedAt <= event.startedAt && event.startedAt < trip.EndedAt)
		}
		if overlap <= 0 || (strict && !enoughOverlap(event, trip.StartedAt, trip.EndedAt)) || !accept(trip) {
			continue
		}
		if best == nil || overlap > bestOverlap {
			best, bestOverlap = trip, overlap
		}
	}
	return best
}

func boolSeconds(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func isJourney(trip *Trip) bool { return trip.Kind == TripKindJourney }

func describeTime(at int64) string { return time.Unix(at, 0).UTC().Format("2006-01-02 15:04Z") }

func visitLabel(visit *Visit) string {
	label := visit.ID
	if visit.PlaceID != nil {
		label += " at " + *visit.PlaceID
		if visit.PlaceName != "" {
			label += " (" + visit.PlaceName + ")"
		}
	}
	return label
}

// matchSeries finds, for each recurring series whose location names a town
// and no known place, the one place the user was at for at least
// seriesMinimumInstances of its times, and at least half the times they
// were anywhere in that town but home. Its times there link to those
// visits: place and time agree. A series without such a place links nothing
// by town alone.
func (m *calendarMatcher) matchSeries(events []*calendarEvent) {
	series := map[string][]*calendarEvent{}
	var order []string
	for _, event := range events {
		if !event.recurring() || !event.physical || event.flight != nil || len(event.places) > 0 || len(event.towns) == 0 ||
			event.event.Status == "cancelled" || eventTypesThatAreNotPlans[event.event.EventType] {
			continue
		}
		key := event.calendar.ID + "\x00" + event.event.RecurringEventID
		if series[key] == nil {
			order = append(order, key)
		}
		series[key] = append(series[key], event)
	}
	for _, key := range order {
		instances := series[key]
		visitsAt := make([]map[string]*Visit, len(instances))
		counts := map[string]int{}
		instancesSomewhere := 0
		for position, event := range instances {
			visitsAt[position] = map[string]*Visit{}
			for index := range m.visits {
				visit := &m.visits[index]
				if visit.PlaceID == nil || !enoughOverlap(event, visit.StartedAt, visit.EndedAt) || m.atHome(visit) ||
					!m.visitInTown(visit, event.towns, event.townName) {
					continue
				}
				if previous := visitsAt[position][*visit.PlaceID]; previous == nil ||
					overlapSeconds(event.startedAt, event.endedAt, visit.StartedAt, visit.EndedAt) >
						overlapSeconds(event.startedAt, event.endedAt, previous.StartedAt, previous.EndedAt) {
					visitsAt[position][*visit.PlaceID] = visit
				}
			}
			if len(visitsAt[position]) > 0 {
				instancesSomewhere++
			}
			for placeID := range visitsAt[position] {
				counts[placeID]++
			}
		}
		bestPlace, bestCount := "", 0
		for placeID, count := range counts {
			if count > bestCount || (count == bestCount && placeID < bestPlace) {
				bestPlace, bestCount = placeID, count
			}
		}
		if bestCount < seriesMinimumInstances || bestCount*2 < instancesSomewhere {
			continue
		}
		for position, event := range instances {
			visit := visitsAt[position][bestPlace]
			if visit == nil {
				continue
			}
			event.seriesVisit = visit
			event.seriesReason = fmt.Sprintf("one of a recurring series in %s; the user was at %s for %d of its %d times (in that town, not home, for %d)",
				event.townName, bestPlace, bestCount, len(instances), instancesSomewhere)
		}
	}
}

// match decides one event. The rules run in order and the first that
// applies decides; see CONTRACT.md "Calendar events".
func (m *calendarMatcher) match(event *calendarEvent) {
	switch {
	case event.event.Status == "cancelled":
		m.unlinked[unlinkedCanceled]++
	case eventTypesThatAreNotPlans[event.event.EventType]:
		m.unlinked[unlinkedEventType]++
	case event.flight != nil:
		m.matchFlight(event)
	case event.online:
		m.unlinked[unlinkedOnline]++
	case event.physical:
		m.matchLocation(event)
	default:
		m.matchNoLocation(event)
	}
}

func (m *calendarMatcher) add(link CalendarEventLink) { m.found = append(m.found, link) }

// matchFlight links a flight event to the recorded flight at its time, and
// to the journey that flight is part of; with no flight recorded, to the
// journey the event falls in.
func (m *calendarMatcher) matchFlight(event *calendarEvent) {
	destination, airline, number := event.flight[0], strings.ToUpper(event.flight[1]), event.flight[2]
	described := fmt.Sprintf("flight %s %s to %s, from %q", airline, number, destination, strings.TrimSpace(event.event.Location))
	flight := m.bestTrip(event, true, func(trip *Trip) bool { return trip.Kind == TripKindMovement && trip.Mode == ModeFlying })
	if flight != nil {
		recorded := fmt.Sprintf("recorded flight %s %s to %s", flight.ID, localityLabelOrUnknown(flight.StartLocality), localityLabelOrUnknown(flight.EndLocality))
		m.add(event.link(EntityTrip, flight.ID, CalendarRuleFlight, confidenceFlight, described+"; "+recorded+" overlaps it"))
		if flight.ParentTripID != nil {
			if parent := m.tripByID[*flight.ParentTripID]; parent != nil && isJourney(parent) {
				m.add(event.link(EntityTrip, parent.ID, CalendarRuleFlightJourney, confidenceFlightJourney,
					described+"; the journey its recorded flight "+flight.ID+" is part of"))
			}
		}
		return
	}
	journey := m.bestTrip(event, false, isJourney)
	if journey == nil {
		m.unlinked[unlinkedFlightNotRecorded]++
		return
	}
	m.add(event.link(EntityTrip, journey.ID, CalendarRuleFlightJourney, confidenceNoFlightTrip,
		described+"; no flight recorded at its time, but it falls in this journey"))
}

func localityLabelOrUnknown(locality *Locality) string {
	if locality == nil {
		return "(town not named)"
	}
	return locality.City
}

// matchLocation links an event whose location names somewhere.
func (m *calendarMatcher) matchLocation(event *calendarEvent) {
	if len(event.places) > 0 {
		visit := m.bestVisit(event, func(visit *Visit) bool { return m.visitNearPlaces(visit, event.places) })
		if visit == nil {
			m.unlinked[unlinkedPlaceNotVisited]++
			return
		}
		var named []string
		for _, place := range event.places {
			named = append(named, place.ID)
		}
		m.add(event.link(EntityVisit, visit.ID, CalendarRulePlace, confidencePlace,
			fmt.Sprintf("location names %s; %s overlaps the event", strings.Join(named, ", "), visitLabel(visit))))
		return
	}
	if len(event.towns) == 0 {
		m.unlinked[unlinkedLocationNotPlaced]++
		return
	}
	if event.recurring() {
		if event.seriesVisit == nil {
			m.unlinked[unlinkedRoutineTownOnly]++
			return
		}
		m.add(event.link(EntityVisit, event.seriesVisit.ID, CalendarRuleSeriesPlace, confidenceSeriesPlace,
			event.seriesReason+"; "+visitLabel(event.seriesVisit)+" overlaps this one"))
		return
	}
	if !event.allDay {
		visit := m.bestVisit(event, func(visit *Visit) bool {
			arrivedInTime := visit.StartedAt >= event.startedAt-arrivalWindowSeconds && visit.StartedAt <= event.startedAt+arrivalWindowSeconds
			return arrivedInTime && !m.atHome(visit) && m.visitInTown(visit, event.towns, event.townName)
		})
		if visit != nil {
			m.add(event.link(EntityVisit, visit.ID, CalendarRuleTownVisit, confidenceTownVisit,
				fmt.Sprintf("location names the town %s and no known place; the user arrived at %s, not home, in that town, at %s for an event at %s",
					event.townName, visitLabel(visit), describeTime(visit.StartedAt), describeTime(event.startedAt))))
			return
		}
	}
	journey := m.bestTrip(event, false, func(trip *Trip) bool {
		if !isJourney(trip) {
			return false
		}
		for position := range m.visits {
			visit := &m.visits[position]
			if overlapSeconds(event.startedAt, event.endedAt, visit.StartedAt, visit.EndedAt) > 0 &&
				overlapSeconds(trip.StartedAt, trip.EndedAt, visit.StartedAt, visit.EndedAt) > 0 && m.visitInTown(visit, event.towns, event.townName) &&
				m.awayFromHome(visit) {
				return true
			}
		}
		return false
	})
	if journey == nil {
		m.unlinked[unlinkedTownNotVisited]++
		return
	}
	m.add(event.link(EntityTrip, journey.ID, CalendarRuleTownJourney, confidenceTownJourney,
		fmt.Sprintf("location names the town %s; this journey was in that town while the event ran", event.townName)))
}

// matchNoLocation links a time of a recurring series with no location to
// the visit at the spot the series is held, when matchUnlocatedSeries found
// one and the user was there; and a single event with no location, on a
// calendar the user writes, to the journey it falls in: time alone, so the
// weakest rule. A routine never links by time alone.
func (m *calendarMatcher) matchNoLocation(event *calendarEvent) {
	if event.recurring() {
		if event.unlocatedSeriesVisit == nil {
			m.unlinked[unlinkedRoutineNoLocation]++
			return
		}
		m.add(event.link(EntityVisit, event.unlocatedSeriesVisit.ID, CalendarRuleUnlocatedSeriesPlace, event.unlocatedSeriesConfidence,
			event.unlocatedSeriesReason))
		return
	}
	if !calendarRolesTheUserWrites[event.calendar.AccessRole] {
		m.unlinked[unlinkedReadOnlyNoLocation]++
		return
	}
	journey := m.bestTrip(event, false, isJourney)
	if journey == nil {
		m.unlinked[unlinkedNoLocationNoJourney]++
		return
	}
	m.add(event.link(EntityTrip, journey.ID, CalendarRuleJourneyTime, confidenceJourneyTime,
		"a single event with no location that falls in this journey: time alone"))
}

// settleCalendarLinks writes what a run found against what earlier runs
// wrote. A dry run decides the same and writes nothing.
func settleCalendarLinks(tx *sql.Tx, matching *CalendarMatching, found []CalendarEventLink, calendarIDs []string, request CalendarMatchRequest) error {
	encoded, err := json.Marshal(calendarIDs)
	if err != nil {
		return err
	}
	stored, err := queryCalendarEventLinks(tx, `SELECT `+calendarLinkColumns+` FROM calendar_event_links
		WHERE calendar_id IN (SELECT value FROM json_each(?)) AND ended_at >= ? AND started_at < ? ORDER BY started_at, seq`,
		string(encoded), request.From, request.To)
	if err != nil {
		return err
	}
	byIdentity := map[string]*CalendarEventLink{}
	for position := range stored {
		byIdentity[calendarLinkIdentity(stored[position])] = &stored[position]
	}
	at := now()
	plan := func(action string, link CalendarEventLink) {
		matching.Actions[action]++
		matching.Links = append(matching.Links, PlannedCalendarEventLink{Action: action, Link: link})
	}
	seen := map[string]bool{}
	for _, link := range found {
		identity := calendarLinkIdentity(link)
		if seen[identity] {
			continue
		}
		seen[identity] = true
		existing := byIdentity[identity]
		if existing == nil {
			// One outside the window (an event that moved) is found by its
			// identity too.
			if existing, err = findCalendarEventLink(tx, link); err != nil {
				return err
			}
		}
		switch {
		case existing == nil:
			if !request.DryRun {
				if link.ID, err = insertCalendarEventLink(tx, link, at); err != nil {
					return err
				}
				link.CreatedAt, link.UpdatedAt = at, at
			}
			plan(CalendarLinkCreate, link)
		case existing.DeletedAt != 0 && existing.DeletedBy != SetByCalendarMatching:
			plan(CalendarLinkKeptDeleted, *existing)
		case existing.MatchedBy != SetByCalendarMatching:
			plan(CalendarLinkUnchanged, *existing)
		case existing.DeletedAt != 0 || !sameFoundLink(*existing, link):
			action := CalendarLinkUpdate
			if existing.DeletedAt != 0 {
				action = CalendarLinkRestore
			}
			link.ID, link.CreatedAt, link.UpdatedAt = existing.ID, existing.CreatedAt, at
			if !request.DryRun {
				if err := updateCalendarEventLink(tx, existing.ID, link, at); err != nil {
					return err
				}
			}
			plan(action, link)
		default:
			plan(CalendarLinkUnchanged, *existing)
		}
	}
	for _, existing := range stored {
		if seen[calendarLinkIdentity(existing)] || existing.MatchedBy != SetByCalendarMatching || existing.DeletedAt != 0 {
			continue
		}
		if !request.DryRun {
			if _, err := tx.Exec(`UPDATE calendar_event_links SET deleted_at = ?, deleted_by = ?, updated_at = ? WHERE id = ?`,
				at, SetByCalendarMatching, at, existing.ID); err != nil {
				return err
			}
			existing.DeletedAt, existing.DeletedBy = at, SetByCalendarMatching
		}
		plan(CalendarLinkDelete, existing)
	}
	return nil
}

// GoogleEventTimes reads an event's start and end, as the matching does:
// epoch seconds, and whether it is an all-day event.
func GoogleEventTimes(event GoogleCalendarEvent, calendar *GoogleCalendar) (int64, int64, bool, error) {
	startedAt, allDay, err := eventTime(event.Start, calendar)
	if err != nil {
		return 0, 0, false, fmt.Errorf("event %s start: %w", event.ID, err)
	}
	endedAt, _, err := eventTime(event.End, calendar)
	if err != nil {
		return 0, 0, false, fmt.Errorf("event %s end: %w", event.ID, err)
	}
	return startedAt, endedAt, allDay, nil
}

// EventLocationNamesSomewhere says an event's location names a place: it is
// set, is not a video call, and is not only a web address.
func EventLocationNamesSomewhere(event GoogleCalendarEvent) bool {
	location := strings.TrimSpace(event.Location)
	return location != "" && !onlineMeetingPattern.MatchString(location) && !linkOnlyPattern.MatchString(location)
}
