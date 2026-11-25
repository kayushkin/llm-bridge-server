package locationstore

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/location-store/internal/geonames"
)

// Journey grouping: find the home base of each stretch of time from where
// the nights were spent, then make each span away from it into a journey, a
// trip of kind journey whose children are the movements inside the span.
// POST /journeys/group runs it; location-group-journeys is its command.

// Defaults of JourneyGroupRequest.
const (
	DefaultAwayDistanceMeters = 80000
	DefaultMinimumAwaySeconds = 6 * 3600
	DefaultHomeChangeDays     = 21
	DefaultReturnBreakSeconds = 12 * 3600
)

const (
	// sameSiteMeters is how close two night places must be to count as one
	// home: a building can carry more than one place id.
	sameSiteMeters = 300
	// atHomeMeters is how close a visit must be to the home to count as at
	// home, which stops a journey's edges from growing past it.
	atHomeMeters = 1000
	// edgeVisitSeconds is the longest stop near home a journey's start or end
	// takes in: coffee on the way out, fuel on the way back.
	edgeVisitSeconds = 3600
	// edgeGapSeconds is the longest silence between records a journey's edges
	// reach across.
	edgeGapSeconds = 2 * 3600
	// destinationSeconds is how long a journey must stay in a town for the
	// town to be named in its title.
	destinationSeconds = 3 * 3600
	// dominantModeShare is the share of a journey's distance one mode must
	// cover to be the journey's mode; below it the journey is mixed.
	dominantModeShare = 0.8
	// longJourneyDays is the length past which a journey is noted as doubtful.
	longJourneyDays = 21
)

// Actions of a PlannedJourney.
const (
	JourneyCreate    = "create"
	JourneyUpdate    = "update"
	JourneyUnchanged = "unchanged"
	JourneySkip      = "skip"
)

var errDryRunRollback = errors.New("dry run: roll back")

// position is somewhere at some time: a visit, or one end of a movement.
type groupingVisit struct {
	visit     Visit
	latitude  float64
	longitude float64
	hasPlace  bool
}

type groupingTrip struct {
	trip Trip
}

type nightSite struct {
	latitude, longitude float64
	nights              int
}

type grouper struct {
	tx       *sql.Tx
	index    *geonames.Index
	request  JourneyGroupRequest
	zones    map[string]*time.Location
	visits   []groupingVisit
	trips    []Trip // every live trip
	plan     *JourneyGrouping
	at       int64
	homeList []HomePeriod
}

func normalizeJourneyRequest(request JourneyGroupRequest) (JourneyGroupRequest, error) {
	if request.AwayDistanceMeters < 0 || request.MinimumAwaySeconds < 0 || request.HomeChangeDays < 0 || request.ReturnBreakSeconds < 0 {
		return request, fmt.Errorf("%w: thresholds cannot be negative", ErrInvalid)
	}
	if request.AwayDistanceMeters == 0 {
		request.AwayDistanceMeters = DefaultAwayDistanceMeters
	}
	if request.MinimumAwaySeconds == 0 {
		request.MinimumAwaySeconds = DefaultMinimumAwaySeconds
	}
	if request.HomeChangeDays == 0 {
		request.HomeChangeDays = DefaultHomeChangeDays
	}
	if request.ReturnBreakSeconds == 0 {
		request.ReturnBreakSeconds = DefaultReturnBreakSeconds
	}
	return request, nil
}

// GroupJourneys finds the home periods and the journeys away from them, and
// writes both, in one transaction; a dry run writes nothing and answers what
// it would have done. Running it again changes only what new data changed.
// It never touches a journey made by hand, never moves a trip someone placed
// by hand, and rewrites only titles it wrote itself.
func (s *Store) GroupJourneys(request JourneyGroupRequest) (*JourneyGrouping, error) {
	request, err := normalizeJourneyRequest(request)
	if err != nil {
		return nil, err
	}
	index, err := s.townIndex()
	if err != nil {
		return nil, err
	}
	plan := &JourneyGrouping{DryRun: request.DryRun, Request: request, HomePeriods: []HomePeriod{}, Journeys: []PlannedJourney{},
		DeletedJourneyIDs: []string{}, HomePlacesMarked: []string{}, Notes: []string{}}
	err = s.inTransaction(func(tx *sql.Tx) error {
		g := &grouper{tx: tx, index: index, request: request, zones: map[string]*time.Location{}, plan: plan, at: now()}
		if err := g.load(); err != nil {
			return err
		}
		if err := g.findHomes(); err != nil {
			return err
		}
		if len(g.homeList) == 0 {
			plan.Notes = append(plan.Notes, "no night is placed anywhere, so there is no home and no journey")
			return g.finish()
		}
		if err := g.groupJourneys(); err != nil {
			return err
		}
		return g.finish()
	})
	if err != nil && !errors.Is(err, errDryRunRollback) {
		return nil, err
	}
	return plan, nil
}

func (g *grouper) finish() error {
	if g.request.DryRun {
		return errDryRunRollback
	}
	return nil
}

func (g *grouper) load() error {
	rows, err := g.tx.Query(visitSelect + ` WHERE v.deleted_at = 0 ORDER BY v.started_at, v.seq`)
	if err != nil {
		return err
	}
	var visits []Visit
	for rows.Next() {
		visit, err := scanVisit(rows.Scan)
		if err != nil {
			rows.Close()
			return err
		}
		visits = append(visits, *visit)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	placeCoordinates := map[string][2]float64{}
	placeRows, err := g.tx.Query(`SELECT id, latitude, longitude FROM places`)
	if err != nil {
		return err
	}
	for placeRows.Next() {
		var id string
		var latitude, longitude float64
		if err := placeRows.Scan(&id, &latitude, &longitude); err != nil {
			placeRows.Close()
			return err
		}
		placeCoordinates[id] = [2]float64{latitude, longitude}
	}
	placeRows.Close()
	if err := placeRows.Err(); err != nil {
		return err
	}
	for _, visit := range visits {
		item := groupingVisit{visit: visit}
		switch {
		case visit.PlaceID != nil:
			coordinates, found := placeCoordinates[*visit.PlaceID]
			if !found {
				return fmt.Errorf("visit %s names place %s, which does not exist", visit.ID, *visit.PlaceID)
			}
			item.latitude, item.longitude, item.hasPlace = coordinates[0], coordinates[1], true
		case visit.Latitude != nil:
			item.latitude, item.longitude = *visit.Latitude, *visit.Longitude
		default:
			continue
		}
		g.visits = append(g.visits, item)
	}
	tripRows, err := g.tx.Query(tripSelect + ` WHERE t.deleted_at = 0 ORDER BY t.started_at, t.seq`)
	if err != nil {
		return err
	}
	defer tripRows.Close()
	for tripRows.Next() {
		trip, err := scanTrip(tripRows.Scan)
		if err != nil {
			return err
		}
		g.trips = append(g.trips, *trip)
	}
	return tripRows.Err()
}

// zoneAt is the time zone of the town nearest a coordinate.
func (g *grouper) zoneAt(latitude, longitude float64) (*time.Location, error) {
	match, found := g.index.Nearest(latitude, longitude)
	name := "UTC"
	if found && match.TimeZone != "" {
		name = match.TimeZone
	}
	if zone, cached := g.zones[name]; cached {
		return zone, nil
	}
	zone, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("time zone %q of the town nearest a visit: %w", name, err)
	}
	g.zones[name] = zone
	return zone, nil
}

// dayNumber is a calendar date as a count of days, so dates subtract.
func dayNumber(year int, month time.Month, day int) int {
	return int(time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

func dayOfNumber(number int) (int, time.Month, int) {
	date := time.Unix(int64(number)*86400, 0).UTC()
	return date.Year(), date.Month(), date.Day()
}

// nightInstant is 03:00 on the morning after the evening of day, in zone.
func nightInstant(day int, zone *time.Location) int64 {
	year, month, date := dayOfNumber(day)
	return time.Date(year, month, date+1, 3, 0, 0, 0, zone).Unix()
}

// nightsIn counts the 03:00s in zone between from and to.
func nightsIn(from, to int64, zone *time.Location) int {
	start := time.Unix(from, 0).In(zone)
	nights := 0
	for day := dayNumber(start.Year(), start.Month(), start.Day()) - 1; ; day++ {
		instant := nightInstant(day, zone)
		if instant >= to {
			return nights
		}
		if instant >= from {
			nights++
		}
	}
}

type nightRecord struct {
	site    int
	placeID string
	visit   *groupingVisit
}

// findHomes places each night (03:00 local) at the visit that covers it,
// takes the most common night site over a rolling window of HomeChangeDays,
// drops any lead shorter than that window, and writes what is left as home
// periods.
func (g *grouper) findHomes() error {
	var sites []nightSite
	siteOf := func(latitude, longitude float64) int {
		for index := range sites {
			if geonames.DistanceMeters(latitude, longitude, sites[index].latitude, sites[index].longitude) <= sameSiteMeters {
				return index
			}
		}
		sites = append(sites, nightSite{latitude: latitude, longitude: longitude})
		return len(sites) - 1
	}
	nights := map[int]nightRecord{}
	nightLength := map[int]int64{}
	for index := range g.visits {
		item := &g.visits[index]
		zone, err := g.zoneAt(item.latitude, item.longitude)
		if err != nil {
			return err
		}
		start := time.Unix(item.visit.StartedAt, 0).In(zone)
		for day := dayNumber(start.Year(), start.Month(), start.Day()) - 1; ; day++ {
			instant := nightInstant(day, zone)
			if instant >= item.visit.EndedAt {
				break
			}
			if instant < item.visit.StartedAt {
				continue
			}
			length := item.visit.EndedAt - item.visit.StartedAt
			if length <= nightLength[day] {
				continue
			}
			placeID := ""
			if item.visit.PlaceID != nil {
				placeID = *item.visit.PlaceID
			}
			nights[day] = nightRecord{site: siteOf(item.latitude, item.longitude), placeID: placeID, visit: item}
			nightLength[day] = length
		}
	}
	if len(nights) == 0 {
		return g.writeHomePeriods(nil)
	}
	firstDay, lastDay := math.MaxInt, math.MinInt
	for day, night := range nights {
		firstDay, lastDay = min(firstDay, day), max(lastDay, day)
		sites[night.site].nights++
	}

	// The rolling most-common site, or -1 when no site holds half the known
	// nights of the window.
	window := g.request.HomeChangeDays
	half := window / 2
	leading := make([]int, lastDay-firstDay+1)
	for day := firstDay; day <= lastDay; day++ {
		counts := map[int]int{}
		known := 0
		for other := day - half; other <= day+half; other++ {
			if night, found := nights[other]; found {
				counts[night.site]++
				known++
			}
		}
		best, bestCount := -1, 0
		for site, count := range counts {
			if count > bestCount || (count == bestCount && site < best) {
				best, bestCount = site, count
			}
		}
		if known < 3 || bestCount*2 < known {
			best = -1
		}
		leading[day-firstDay] = best
	}

	type segment struct{ site, from, to int }
	var segments []segment
	for offset, site := range leading {
		day := firstDay + offset
		switch {
		case len(segments) == 0 && site == -1:
			continue
		case len(segments) > 0 && (site == -1 || segments[len(segments)-1].site == site):
			segments[len(segments)-1].to = day
		default:
			segments = append(segments, segment{site: site, from: day, to: day})
		}
	}
	if len(segments) == 0 {
		// No site ever leads: take the site of the most nights for all of it.
		best := 0
		for index := range sites {
			if sites[index].nights > sites[best].nights {
				best = index
			}
		}
		segments = []segment{{site: best, from: firstDay, to: lastDay}}
	}
	segments[0].from = firstDay
	segments[len(segments)-1].to = lastDay
	// Drop every lead shorter than the window, the shortest first, giving
	// its days to the neighbors.
	for len(segments) > 1 {
		shortest := -1
		for index, candidate := range segments {
			if candidate.to-candidate.from+1 < window && (shortest == -1 || candidate.to-candidate.from < segments[shortest].to-segments[shortest].from) {
				shortest = index
			}
		}
		if shortest == -1 {
			break
		}
		dropped := segments[shortest]
		switch {
		case shortest == 0:
			segments[1].from = dropped.from
		case shortest == len(segments)-1:
			segments[shortest-1].to = dropped.to
		default:
			segments[shortest-1].to = dropped.to
		}
		segments = append(segments[:shortest], segments[shortest+1:]...)
		// Neighbors that are now the same site join.
		for index := 1; index < len(segments); {
			if segments[index].site == segments[index-1].site {
				segments[index-1].to = segments[index].to
				segments = append(segments[:index], segments[index+1:]...)
				continue
			}
			index++
		}
	}

	var periods []HomePeriod
	for position, part := range segments {
		site := sites[part.site]
		period := HomePeriod{Latitude: site.latitude, Longitude: site.longitude, DescribedBy: SetByJourneyGrouping}
		// The home's place is the place of the most of the period's nights at
		// its site.
		known, atHome := 0, 0
		placeNights := map[string]int{}
		for day := part.from; day <= part.to; day++ {
			night, found := nights[day]
			if !found {
				continue
			}
			known++
			if night.site == part.site {
				atHome++
				if night.placeID != "" {
					placeNights[night.placeID]++
				}
			}
		}
		bestPlace, bestNights := "", 0
		for placeID, count := range placeNights {
			if count > bestNights || (count == bestNights && placeID < bestPlace) {
				bestPlace, bestNights = placeID, count
			}
		}
		if bestPlace != "" {
			id := bestPlace
			period.PlaceID = &id
			place, err := getPlaceRow(g.tx, bestPlace)
			if err != nil {
				return err
			}
			period.Latitude, period.Longitude, period.PlaceName = place.Latitude, place.Longitude, place.Name
		}
		period.NightsKnown, period.NightsAtHome = known, atHome
		zone, err := g.zoneAt(period.Latitude, period.Longitude)
		if err != nil {
			return err
		}
		year, month, date := dayOfNumber(part.from)
		period.StartedAt = time.Date(year, month, date, 12, 0, 0, 0, zone).Unix()
		if position == 0 {
			period.StartedAt = min(period.StartedAt, g.earliest())
		}
		if position < len(segments)-1 {
			year, month, date = dayOfNumber(part.to)
			period.EndedAt = time.Date(year, month, date+1, 12, 0, 0, 0, zone).Unix()
		}
		period.Locality = localityFor(g.index, period.Latitude, period.Longitude)
		periods = append(periods, period)
	}
	// Each period starts where the one before it ends.
	for index := 1; index < len(periods); index++ {
		periods[index].StartedAt = periods[index-1].EndedAt
	}
	return g.writeHomePeriods(periods)
}

func (g *grouper) earliest() int64 {
	earliest := int64(math.MaxInt64)
	if len(g.visits) > 0 {
		earliest = g.visits[0].visit.StartedAt
	}
	for _, trip := range g.trips {
		earliest = min(earliest, trip.StartedAt)
	}
	return earliest
}

const homePeriodColumns = `h.id, h.place_id, COALESCE(p.name, ''), h.latitude, h.longitude, h.locality, h.started_at, h.ended_at,
	h.nights_known, h.nights_at_home, h.described_by, h.created_at, h.updated_at, h.deleted_at`

func scanHomePeriod(scan func(...any) error) (*HomePeriod, error) {
	var period HomePeriod
	var placeID, locality sql.NullString
	if err := scan(&period.ID, &placeID, &period.PlaceName, &period.Latitude, &period.Longitude, &locality, &period.StartedAt,
		&period.EndedAt, &period.NightsKnown, &period.NightsAtHome, &period.DescribedBy, &period.CreatedAt, &period.UpdatedAt,
		&period.DeletedAt); err != nil {
		return nil, err
	}
	period.PlaceID = stringPointer(placeID)
	var err error
	period.Locality, err = decodeLocality(locality, "home period "+period.ID)
	return &period, err
}

func listHomePeriods(q queryer, includeDeleted bool) ([]HomePeriod, error) {
	query := `SELECT ` + homePeriodColumns + ` FROM home_periods h LEFT JOIN places p ON p.id = h.place_id`
	if !includeDeleted {
		query += ` WHERE h.deleted_at = 0`
	}
	rows, err := q.Query(query + ` ORDER BY h.started_at, h.seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	periods := []HomePeriod{}
	for rows.Next() {
		period, err := scanHomePeriod(rows.Scan)
		if err != nil {
			return nil, err
		}
		periods = append(periods, *period)
	}
	return periods, rows.Err()
}

// ListHomePeriods answers GET /home-periods, earliest first.
func (s *Store) ListHomePeriods(includeDeleted bool) ([]HomePeriod, error) {
	return listHomePeriods(s.db, includeDeleted)
}

// GetHomePeriod answers GET /home-periods/{id}.
func (s *Store) GetHomePeriod(id string) (*HomePeriod, error) {
	period, err := scanHomePeriod(s.db.QueryRow(`SELECT `+homePeriodColumns+` FROM home_periods h LEFT JOIN places p ON p.id = h.place_id WHERE h.id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no home period %s", ErrNotFound, id)
	}
	return period, err
}

// writeHomePeriods keeps each existing period that matches a found one (the
// same site, overlapping in time), creates the rest, and soft-deletes the
// existing ones nothing matched. It marks each home place's kind home unless
// someone set the place's kind by hand.
func (g *grouper) writeHomePeriods(found []HomePeriod) error {
	existing, err := listHomePeriods(g.tx, false)
	if err != nil {
		return err
	}
	used := map[string]bool{}
	for index := range found {
		period := &found[index]
		for _, old := range existing {
			if used[old.ID] || geonames.DistanceMeters(old.Latitude, old.Longitude, period.Latitude, period.Longitude) > sameSiteMeters {
				continue
			}
			oldEnd, newEnd := old.EndedAt, period.EndedAt
			if oldEnd == 0 {
				oldEnd = math.MaxInt64
			}
			if newEnd == 0 {
				newEnd = math.MaxInt64
			}
			if old.StartedAt < newEnd && period.StartedAt < oldEnd {
				period.ID, period.CreatedAt = old.ID, old.CreatedAt
				used[old.ID] = true
				break
			}
		}
		if period.ID == "" {
			id, seq, err := nextID(g.tx, "home_periods", "home_period")
			if err != nil {
				return err
			}
			period.ID, period.CreatedAt = id, g.at
			if _, err := g.tx.Exec(`INSERT INTO home_periods (id, seq, latitude, longitude, started_at, described_by, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?)`, id, seq, period.Latitude, period.Longitude, period.StartedAt, period.DescribedBy, g.at, g.at); err != nil {
				return err
			}
		}
		period.UpdatedAt = g.at
		if _, err := g.tx.Exec(`UPDATE home_periods SET place_id=?, latitude=?, longitude=?, locality=?, started_at=?, ended_at=?,
			nights_known=?, nights_at_home=?, described_by=?, updated_at=? WHERE id=?`, nullableString(period.PlaceID), period.Latitude,
			period.Longitude, encodeLocality(period.Locality), period.StartedAt, period.EndedAt, period.NightsKnown, period.NightsAtHome,
			period.DescribedBy, g.at, period.ID); err != nil {
			return err
		}
		if err := g.markHome(period.PlaceID); err != nil {
			return err
		}
	}
	for _, old := range existing {
		if !used[old.ID] {
			if _, err := g.tx.Exec(`UPDATE home_periods SET deleted_at=?, updated_at=? WHERE id=?`, g.at, g.at, old.ID); err != nil {
				return err
			}
		}
	}
	if g.request.DryRun {
		for index := range found {
			if !used[found[index].ID] {
				found[index].ID = ""
			}
		}
	}
	g.homeList = found
	g.plan.HomePeriods = append(g.plan.HomePeriods, found...)
	return nil
}

func (g *grouper) markHome(placeID *string) error {
	if placeID == nil {
		return nil
	}
	place, err := getPlaceRow(g.tx, *placeID)
	if err != nil {
		return err
	}
	if place.Kind == PlaceKindHome {
		return nil
	}
	var editedByHand int
	if err := g.tx.QueryRow(`SELECT COUNT(*) FROM revisions WHERE entity_id = ? AND reason = 'update'`, place.ID).Scan(&editedByHand); err != nil {
		return err
	}
	if editedByHand > 0 {
		g.plan.Notes = append(g.plan.Notes, fmt.Sprintf("home place %s has kind %s, set by hand; left as it is", place.ID, place.Kind))
		return nil
	}
	if err := snapshot(g.tx, EntityPlace, place.ID, "home", g.at); err != nil {
		return err
	}
	if _, err := g.tx.Exec(`UPDATE places SET kind=?, updated_at=? WHERE id=?`, PlaceKindHome, g.at, place.ID); err != nil {
		return err
	}
	g.plan.HomePlacesMarked = append(g.plan.HomePlacesMarked, place.ID)
	return nil
}

// homeAt is the home period in force at a moment: the one that holds it, or
// before the first, the first.
func (g *grouper) homeAt(at int64) *HomePeriod {
	for index := len(g.homeList) - 1; index >= 0; index-- {
		if g.homeList[index].StartedAt <= at {
			return &g.homeList[index]
		}
	}
	return &g.homeList[0]
}

func (g *grouper) fromHome(at int64, latitude, longitude float64) float64 {
	home := g.homeAt(at)
	return geonames.DistanceMeters(home.Latitude, home.Longitude, latitude, longitude)
}

// timelineEntry is one visit or one movement, with how it stands to home.
type timelineEntry struct {
	startedAt, endedAt int64
	visit              *groupingVisit
	trip               *Trip
	far                bool
	atHome             bool
	// endsNear says the entry ends within the away distance of home.
	endsNear       bool
	farthestMeters float64
}

func (g *grouper) timeline() []timelineEntry {
	away := g.request.AwayDistanceMeters
	var entries []timelineEntry
	for index := range g.visits {
		item := &g.visits[index]
		distance := g.fromHome(item.visit.StartedAt, item.latitude, item.longitude)
		entries = append(entries, timelineEntry{startedAt: item.visit.StartedAt, endedAt: item.visit.EndedAt, visit: item,
			far: distance > away, atHome: distance <= atHomeMeters, endsNear: distance <= away, farthestMeters: distance})
	}
	for index := range g.trips {
		trip := &g.trips[index]
		if trip.Kind != TripKindMovement {
			continue
		}
		entry := timelineEntry{startedAt: trip.StartedAt, endedAt: trip.EndedAt, trip: trip, far: trip.Mode == ModeFlying, endsNear: true}
		if trip.StartLatitude != nil {
			distance := g.fromHome(trip.StartedAt, *trip.StartLatitude, *trip.StartLongitude)
			entry.far = entry.far || distance > away
			entry.farthestMeters = distance
		}
		if trip.EndLatitude != nil {
			distance := g.fromHome(trip.EndedAt, *trip.EndLatitude, *trip.EndLongitude)
			entry.far = entry.far || distance > away
			entry.endsNear = distance <= away
			entry.farthestMeters = max(entry.farthestMeters, distance)
		} else if trip.Mode == ModeFlying {
			entry.endsNear = false
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(a, b int) bool {
		if entries[a].startedAt != entries[b].startedAt {
			return entries[a].startedAt < entries[b].startedAt
		}
		return entries[a].endedAt < entries[b].endedAt
	})
	return entries
}

// journeySpan is one journey found: entries[first..last].
type journeySpan struct {
	first, last int
}

func (g *grouper) spans(entries []timelineEntry) []journeySpan {
	var runs []journeySpan
	for index, entry := range entries {
		if !entry.far {
			continue
		}
		if len(runs) > 0 && g.continues(entries, runs[len(runs)-1].last, index) {
			runs[len(runs)-1].last = index
			continue
		}
		runs = append(runs, journeySpan{first: index, last: index})
	}
	// Grow each run's edges over the short stops and movements that lead out
	// of home and back into it.
	for position := range runs {
		run := &runs[position]
		lowest := 0
		if position > 0 {
			lowest = runs[position-1].last + 1
		}
		for run.first > lowest && edgeEntry(entries[run.first-1], entries[run.first].startedAt) {
			run.first--
		}
		highest := len(entries) - 1
		if position < len(runs)-1 {
			highest = runs[position+1].first - 1
		}
		for run.last < highest && edgeEntry(entries[run.last+1], entries[run.last].endedAt) {
			// The step forward must start after the run ends, or it is a stop
			// the run already overlaps.
			run.last++
		}
	}
	return runs
}

// edgeEntry says whether a journey's edge may take in entry: a movement or a
// short stop away from home, close in time to the journey.
func edgeEntry(entry timelineEntry, journeyEdge int64) bool {
	gap := entry.startedAt - journeyEdge
	if entry.endedAt < journeyEdge {
		gap = journeyEdge - entry.endedAt
	}
	if gap > edgeGapSeconds || entry.atHome {
		return false
	}
	if entry.visit != nil {
		return entry.endedAt-entry.startedAt <= edgeVisitSeconds
	}
	return true
}

// continues says whether the far entry at next belongs to the same journey
// as the far entry at last: it does unless the record shows the user back
// near home overnight, or for ReturnBreakSeconds.
func (g *grouper) continues(entries []timelineEntry, last, next int) bool {
	returnedAt := int64(0)
	if entries[last].endsNear && entries[last].trip != nil {
		returnedAt = entries[last].endedAt
	}
	for index := last + 1; index < next; index++ {
		if !entries[index].far && returnedAt == 0 {
			returnedAt = entries[index].startedAt
		}
	}
	if returnedAt == 0 {
		// Nothing says the user came back: the record is silent, or still away.
		return true
	}
	leftAt := entries[next].startedAt
	if leftAt-returnedAt >= g.request.ReturnBreakSeconds {
		return false
	}
	home := g.homeAt(returnedAt)
	zone, err := g.zoneAt(home.Latitude, home.Longitude)
	if err != nil {
		return true
	}
	return nightsIn(returnedAt, leftAt, zone) == 0
}

// candidateJourney is a journey found, before it is matched to what is stored.
type candidateJourney struct {
	startedAt, endedAt int64
	children           []*Trip
	entries            []timelineEntry
	farthestMeters     float64
	nights             int
	hasFlight          bool
}

func (g *grouper) groupJourneys() error {
	entries := g.timeline()
	var candidates []candidateJourney
	for _, span := range g.spans(entries) {
		candidate := candidateJourney{startedAt: entries[span.first].startedAt, endedAt: entries[span.first].endedAt,
			entries: entries[span.first : span.last+1]}
		for _, entry := range candidate.entries {
			candidate.endedAt = max(candidate.endedAt, entry.endedAt)
			candidate.farthestMeters = max(candidate.farthestMeters, entry.farthestMeters)
			if entry.trip != nil {
				candidate.children = append(candidate.children, entry.trip)
				if entry.trip.Mode == ModeFlying {
					candidate.hasFlight = true
				}
			}
		}
		home := g.homeAt(candidate.startedAt)
		zone, err := g.zoneAt(home.Latitude, home.Longitude)
		if err != nil {
			return err
		}
		candidate.nights = nightsIn(candidate.startedAt, candidate.endedAt, zone)
		if candidate.nights == 0 && !candidate.hasFlight && candidate.endedAt-candidate.startedAt < g.request.MinimumAwaySeconds {
			continue
		}
		if len(candidate.children) == 0 {
			continue
		}
		candidates = append(candidates, candidate)
	}

	var generated, byHand []*Trip
	for index := range g.trips {
		trip := &g.trips[index]
		if trip.Kind != TripKindJourney {
			continue
		}
		if trip.Source == SourceGenerated {
			generated = append(generated, trip)
		} else {
			byHand = append(byHand, trip)
		}
	}
	matched := map[string]bool{}
	for _, candidate := range candidates {
		planned, err := g.writeJourney(candidate, generated, byHand, matched)
		if err != nil {
			return err
		}
		g.plan.Journeys = append(g.plan.Journeys, planned)
	}
	for _, old := range generated {
		if matched[old.ID] {
			continue
		}
		if err := g.retire(old); err != nil {
			return err
		}
	}
	return nil
}

func overlaps(startA, endA, startB, endB int64) int64 {
	return min(endA, endB) - max(startA, startB)
}

func (g *grouper) writeJourney(candidate candidateJourney, generated, byHand []*Trip, matched map[string]bool) (PlannedJourney, error) {
	planned := PlannedJourney{StartedAt: candidate.startedAt, EndedAt: candidate.endedAt, Nights: candidate.nights,
		FarthestMeters: math.Round(candidate.farthestMeters), ChildTripIDs: []string{}, Notes: []string{}}
	planned.Title = g.journeyTitle(candidate)
	planned.Mode = dominantMode(candidate.children)
	if days := (candidate.endedAt - candidate.startedAt) / 86400; days > longJourneyDays {
		planned.Notes = append(planned.Notes, fmt.Sprintf("%d days long: check the record is not silent through a return home", days))
	}
	for _, trip := range byHand {
		if overlaps(trip.StartedAt, trip.EndedAt, candidate.startedAt, candidate.endedAt) > 0 {
			planned.Action = JourneySkip
			planned.TripID = trip.ID
			planned.Notes = append(planned.Notes, fmt.Sprintf("overlaps %s, a journey made by hand; left to it", trip.ID))
			return planned, nil
		}
	}

	var existing *Trip
	var bestOverlap int64
	for _, old := range generated {
		if matched[old.ID] {
			continue
		}
		if overlap := overlaps(old.StartedAt, old.EndedAt, candidate.startedAt, candidate.endedAt); overlap > 0 && overlap > bestOverlap {
			existing, bestOverlap = old, overlap
		}
	}

	first, last := candidate.children[0], candidate.children[len(candidate.children)-1]
	distance := 0.0
	for _, child := range candidate.children {
		distance += tripDistance(child)
	}
	startLocality, endLocality := first.StartLocality, last.EndLocality
	if startLocality == nil && first.StartLatitude != nil {
		startLocality = localityFor(g.index, *first.StartLatitude, *first.StartLongitude)
	}
	if endLocality == nil && last.EndLatitude != nil {
		endLocality = localityFor(g.index, *last.EndLatitude, *last.EndLongitude)
	}

	var id string
	if existing == nil {
		planned.Action = JourneyCreate
		var seq int64
		var err error
		if id, seq, err = nextID(g.tx, "trips", "trip"); err != nil {
			return planned, err
		}
		if _, err := g.tx.Exec(`INSERT INTO trips (id, seq, kind, started_at, ended_at, mode, title, title_by, source, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, id, seq, TripKindJourney, candidate.startedAt, candidate.endedAt, planned.Mode,
			planned.Title, SetByJourneyGrouping, SourceGenerated, g.at, g.at); err != nil {
			return planned, err
		}
	} else {
		id = existing.ID
		matched[id] = true
		planned.Action = JourneyUnchanged
		if existing.TitleBy != SetByJourneyGrouping {
			planned.Title = existing.Title
			planned.Notes = append(planned.Notes, "title written by hand; kept")
		}
		if !(existing.StartedAt == candidate.startedAt && existing.EndedAt == candidate.endedAt && existing.Mode == planned.Mode &&
			existing.Title == planned.Title && sameString(existing.StartPlaceID, first.StartPlaceID) &&
			sameString(existing.EndPlaceID, last.EndPlaceID) && existing.DistanceMeters != nil && *existing.DistanceMeters == distance &&
			sameFloat(existing.StartLatitude, first.StartLatitude) && sameFloat(existing.EndLatitude, last.EndLatitude) &&
			sameLocality(existing.StartLocality, startLocality) && sameLocality(existing.EndLocality, endLocality)) {
			planned.Action = JourneyUpdate
			if err := snapshot(g.tx, EntityTrip, id, "journey", g.at); err != nil {
				return planned, err
			}
		}
	}
	if planned.Action != JourneyUnchanged {
		if _, err := g.tx.Exec(`UPDATE trips SET started_at=?, ended_at=?, start_latitude=?, start_longitude=?, end_latitude=?,
			end_longitude=?, start_place_id=?, end_place_id=?, mode=?, distance_meters=?, title=?, start_locality=?, end_locality=?,
			updated_at=? WHERE id=?`, candidate.startedAt, candidate.endedAt, nullableFloat(first.StartLatitude),
			nullableFloat(first.StartLongitude), nullableFloat(last.EndLatitude), nullableFloat(last.EndLongitude),
			nullableString(first.StartPlaceID), nullableString(last.EndPlaceID), planned.Mode, distance, planned.Title,
			encodeLocality(startLocality), encodeLocality(endLocality), g.at, id); err != nil {
			return planned, err
		}
	}

	inJourney := map[string]bool{}
	for _, child := range candidate.children {
		inJourney[child.ID] = true
		switch {
		case child.ParentTripID != nil && *child.ParentTripID == id:
			planned.ChildTripIDs = append(planned.ChildTripIDs, child.ID)
		case child.ParentSetBy == SetByHand:
			planned.ChildrenKeptElsewhere++
		case child.ParentTripID == nil || g.isGenerated(*child.ParentTripID):
			if err := g.setParent(child, &id); err != nil {
				return planned, err
			}
			planned.ChildTripIDs = append(planned.ChildTripIDs, child.ID)
			planned.ChildrenAttached++
		default:
			planned.ChildrenKeptElsewhere++
		}
	}
	if existing != nil {
		for index := range g.trips {
			child := &g.trips[index]
			if child.ParentTripID != nil && *child.ParentTripID == id && !inJourney[child.ID] && child.ParentSetBy == SetByJourneyGrouping {
				if err := g.setParent(child, nil); err != nil {
					return planned, err
				}
				planned.ChildrenDetached++
			}
		}
	}
	if planned.Action == JourneyUnchanged && (planned.ChildrenAttached > 0 || planned.ChildrenDetached > 0) {
		planned.Action = JourneyUpdate
	}
	if planned.ChildrenKeptElsewhere > 0 {
		planned.Notes = append(planned.Notes, fmt.Sprintf("%d trips in its span were placed by hand; left where they are", planned.ChildrenKeptElsewhere))
	}
	if !(g.request.DryRun && planned.Action == JourneyCreate) {
		planned.TripID = id
	}
	return planned, nil
}

func (g *grouper) isGenerated(tripID string) bool {
	for index := range g.trips {
		if g.trips[index].ID == tripID {
			return g.trips[index].Kind == TripKindJourney && g.trips[index].Source == SourceGenerated
		}
	}
	return false
}

// setParent places a movement in a journey, or out of one, as this command.
func (g *grouper) setParent(child *Trip, parentID *string) error {
	if err := snapshot(g.tx, EntityTrip, child.ID, "journey", g.at); err != nil {
		return err
	}
	setBy := SetByJourneyGrouping
	if parentID == nil {
		// Out of a journey this command made, the trip is nobody's again.
		setBy = ""
	}
	if _, err := g.tx.Exec(`UPDATE trips SET parent_trip_id=?, parent_set_by=?, updated_at=? WHERE id=?`, nullableString(parentID), setBy, g.at, child.ID); err != nil {
		return err
	}
	child.ParentTripID, child.ParentSetBy = parentID, setBy
	return nil
}

// retire handles a journey an earlier run made that no journey found now
// matches: its own children go back to nobody, and it is soft-deleted unless
// someone has written on it or placed a trip in it by hand.
func (g *grouper) retire(old *Trip) error {
	keptByHand := 0
	for index := range g.trips {
		child := &g.trips[index]
		if child.ParentTripID == nil || *child.ParentTripID != old.ID {
			continue
		}
		if child.ParentSetBy == SetByJourneyGrouping {
			if err := g.setParent(child, nil); err != nil {
				return err
			}
			continue
		}
		keptByHand++
	}
	if keptByHand > 0 || old.TitleBy != SetByJourneyGrouping || old.Description != "" || len(old.Tags) > 0 {
		g.plan.Notes = append(g.plan.Notes, fmt.Sprintf("%s no longer matches a journey, but someone wrote on it or placed trips in it by hand; kept", old.ID))
		return nil
	}
	if err := snapshot(g.tx, EntityTrip, old.ID, "delete", g.at); err != nil {
		return err
	}
	if _, err := g.tx.Exec(`UPDATE trips SET deleted_at=?, updated_at=? WHERE id=?`, g.at, g.at, old.ID); err != nil {
		return err
	}
	g.plan.DeletedJourneyIDs = append(g.plan.DeletedJourneyIDs, old.ID)
	return nil
}

func sameLocality(a, b *Locality) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func tripDistance(trip *Trip) float64 {
	if trip.DistanceMeters != nil {
		return *trip.DistanceMeters
	}
	if trip.StartLatitude != nil && trip.EndLatitude != nil {
		return math.Round(geonames.DistanceMeters(*trip.StartLatitude, *trip.StartLongitude, *trip.EndLatitude, *trip.EndLongitude))
	}
	return 0
}

// dominantMode is the mode that covers most of the distance, when it covers
// dominantModeShare of it; otherwise mixed.
func dominantMode(children []*Trip) string {
	byMode := map[string]float64{}
	total := 0.0
	for _, child := range children {
		distance := tripDistance(child)
		byMode[child.Mode] += distance
		total += distance
	}
	best, bestDistance := ModeMixed, 0.0
	for mode, distance := range byMode {
		if distance > bestDistance || (distance == bestDistance && mode < best) {
			best, bestDistance = mode, distance
		}
	}
	if total == 0 || bestDistance < dominantModeShare*total {
		return ModeMixed
	}
	return best
}

// LocalityLabel is a town as a title names it: "Fartown, Example Region" at home in
// the same country, "Fartown, Exampleland" abroad.
func LocalityLabel(locality *Locality, homeCountryCode string) string {
	if locality == nil {
		return ""
	}
	second := locality.Region
	if locality.CountryCode != homeCountryCode || second == "" {
		second = locality.Country
	}
	if second == "" || second == locality.City {
		return locality.City
	}
	return locality.City + ", " + second
}

// journeyTitle is "Home town → Town, Region → …, 26 Jun – 30 Jun 2026": the
// towns the journey slept in, in order of arrival, at most three (the three
// it spent most time in); with none, those it stayed in for
// destinationSeconds or more; with none of those, the farthest point.
func (g *grouper) journeyTitle(candidate candidateJourney) string {
	home := g.homeAt(candidate.startedAt)
	origin := "Home"
	homeCountry := ""
	if home.Locality != nil {
		origin, homeCountry = home.Locality.City, home.Locality.CountryCode
	}
	type stay struct {
		locality  *Locality
		seconds   int64
		nights    int
		arrivedAt int64
	}
	stays := map[int64]*stay{}
	var farthest *Locality
	farthestMeters := -1.0
	for _, entry := range candidate.entries {
		if entry.visit == nil || !entry.far {
			continue
		}
		locality := entry.visit.visit.Locality
		if locality == nil {
			locality = localityFor(g.index, entry.visit.latitude, entry.visit.longitude)
		}
		if locality == nil {
			continue
		}
		if entry.farthestMeters > farthestMeters {
			farthest, farthestMeters = locality, entry.farthestMeters
		}
		if stays[locality.GeonamesID] == nil {
			stays[locality.GeonamesID] = &stay{locality: locality, arrivedAt: entry.startedAt}
		}
		stays[locality.GeonamesID].seconds += entry.endedAt - entry.startedAt
		if zone, err := g.zoneAt(entry.visit.latitude, entry.visit.longitude); err == nil {
			stays[locality.GeonamesID].nights += nightsIn(entry.startedAt, entry.endedAt, zone)
		}
	}
	// The towns slept in, or on a journey with no night away, the towns of
	// long stops; a change of planes is neither.
	var kept []*stay
	for _, candidateStay := range stays {
		if candidateStay.nights > 0 {
			kept = append(kept, candidateStay)
		}
	}
	if len(kept) == 0 {
		for _, candidateStay := range stays {
			if candidateStay.seconds >= destinationSeconds {
				kept = append(kept, candidateStay)
			}
		}
	}
	sort.Slice(kept, func(a, b int) bool { return kept[a].seconds > kept[b].seconds })
	more := 0
	if len(kept) > 3 {
		more = len(kept) - 3
		kept = kept[:3]
	}
	sort.Slice(kept, func(a, b int) bool { return kept[a].arrivedAt < kept[b].arrivedAt })
	names := []string{origin}
	for _, kept := range kept {
		if kept.locality.City != origin {
			names = append(names, LocalityLabel(kept.locality, homeCountry))
		}
	}
	if len(names) == 1 {
		if farthest == nil {
			// No visit away has a town: the movements' ends do.
			for _, child := range candidate.children {
				if child.EndLocality != nil {
					farthest = child.EndLocality
				}
			}
		}
		if farthest != nil {
			names = append(names, LocalityLabel(farthest, homeCountry))
		} else {
			names = append(names, "away")
		}
	}
	route := strings.Join(names, " → ")
	if more > 0 {
		route += fmt.Sprintf(" and %d more", more)
	}
	zone, err := g.zoneAt(home.Latitude, home.Longitude)
	if err != nil {
		zone = time.UTC
	}
	return route + ", " + DateSpanLabel(candidate.startedAt, candidate.endedAt, zone)
}

// DateSpanLabel is "26 Jun – 30 Jun 2026", "26 Jun 2026" for one day, or
// "28 Dec 2025 – 3 Jan 2026" across a year's end.
func DateSpanLabel(startedAt, endedAt int64, zone *time.Location) string {
	start, end := time.Unix(startedAt, 0).In(zone), time.Unix(endedAt, 0).In(zone)
	switch {
	case start.Year() == end.Year() && start.YearDay() == end.YearDay():
		return start.Format("2 Jan 2006")
	case start.Year() == end.Year():
		return start.Format("2 Jan") + " – " + end.Format("2 Jan 2006")
	}
	return start.Format("2 Jan 2006") + " – " + end.Format("2 Jan 2006")
}

// Journey answers GET /journeys/{id}: the trip, its children, the visits in
// its span and the towns those visits were in.
func (s *Store) Journey(id string) (*JourneySummary, error) {
	trip, err := s.GetTrip(id, false)
	if err != nil {
		return nil, err
	}
	children, err := s.listTripsUncapped(TripFilter{TimeFilter: TimeFilter{Limit: math.MaxInt32}, ParentTripID: id})
	if err != nil {
		return nil, err
	}
	visits, err := s.listVisitsUncapped(VisitFilter{TimeFilter: TimeFilter{From: trip.StartedAt, To: trip.EndedAt, Limit: math.MaxInt32}})
	if err != nil {
		return nil, err
	}
	summary := &JourneySummary{Trip: *trip, Children: children, Visits: []Visit{}, Localities: []JourneyLocality{}}
	if summary.Children == nil {
		summary.Children = []Trip{}
	}
	byTown := map[int64]int{}
	for _, visit := range visits {
		// A visit that only touches the journey's edge is not part of it.
		seconds := min(visit.EndedAt, trip.EndedAt) - max(visit.StartedAt, trip.StartedAt)
		if seconds <= 0 {
			continue
		}
		summary.Visits = append(summary.Visits, visit)
		if visit.Locality == nil {
			continue
		}
		position, seen := byTown[visit.Locality.GeonamesID]
		if !seen {
			position = len(summary.Localities)
			byTown[visit.Locality.GeonamesID] = position
			summary.Localities = append(summary.Localities, JourneyLocality{Locality: *visit.Locality, ArrivedAt: visit.StartedAt})
		}
		summary.Localities[position].Seconds += seconds
	}
	targets := []string{trip.ID}
	for _, child := range summary.Children {
		targets = append(targets, child.ID)
	}
	for _, visit := range summary.Visits {
		targets = append(targets, visit.ID)
	}
	if summary.CalendarEventLinks, err = s.liveCalendarEventLinksTo(targets); err != nil {
		return nil, err
	}
	return summary, nil
}
