package locationstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Place name suggestions. location-suggest-place-names reads OpenStreetMap
// extracts on disk and the calendar, offline, and sends what it found near
// each unnamed place to POST /place-suggestions/proposals. Everything that
// decides is here: which places are ranked first, when the user goes, which
// findings become suggestions. Nothing is ever named by a run: a person or an
// agent accepts a suggestion, and only then does the place get a name.

const (
	// suggestionPOIReachMeters is how near a place a named feature must be
	// to be suggested; suggestionMaximumPOIs of the nearest are.
	suggestionPOIReachMeters = 100
	suggestionMaximumPOIs    = 5
	// suggestionAddressReachMeters is how near the nearest address must be
	// to be suggested as the place's name.
	suggestionAddressReachMeters = 60
	// suggestionCalendarReachMeters is how near a calendar event's address
	// must be.
	suggestionCalendarReachMeters = placeReachMeters
	// suggestionCloseMeters is the distance at which a lone feature is a
	// strong suggestion.
	suggestionCloseMeters = 25
)

// PlaceSuggestion actions, as PlaceSuggesting.Actions counts them.
const (
	suggestionCreate      = "create"
	suggestionUpdate      = "update"
	suggestionUnchanged   = "unchanged"
	suggestionKeptDecided = "kept_decided"
	suggestionDelete      = "delete"
)

// ---- visit patterns ----

func zoneOf(place *Place) (*time.Location, string, error) {
	if place.Locality == nil || place.Locality.TimeZone == "" {
		return time.UTC, "UTC", nil
	}
	zone, err := time.LoadLocation(place.Locality.TimeZone)
	if err != nil {
		return nil, "", fmt.Errorf("place %s: time zone %q: %w", place.ID, place.Locality.TimeZone, err)
	}
	return zone, place.Locality.TimeZone, nil
}

// visitPattern is when the user goes to the place, read from its visits.
func visitPattern(place *Place, visits []Visit) (PlaceVisitPattern, error) {
	zone, zoneName, err := zoneOf(place)
	if err != nil {
		return PlaceVisitPattern{}, err
	}
	pattern := PlaceVisitPattern{TimeZone: zoneName, ArrivalsByWeekday: make([]int, 7), ArrivalsByHour: make([]int, 24)}
	days := map[string]bool{}
	var minutes []float64
	for _, visit := range visits {
		pattern.Visits++
		seconds := float64(visit.EndedAt - visit.StartedAt)
		pattern.Hours += seconds / 3600
		minutes = append(minutes, seconds/60)
		if pattern.FirstVisitAt == 0 || visit.StartedAt < pattern.FirstVisitAt {
			pattern.FirstVisitAt = visit.StartedAt
		}
		pattern.LastVisitAt = max(pattern.LastVisitAt, visit.EndedAt)
		arrived := time.Unix(visit.StartedAt, 0).In(zone)
		pattern.ArrivalsByWeekday[arrived.Weekday()]++
		pattern.ArrivalsByHour[arrived.Hour()]++
		for day := arrived; !day.After(time.Unix(visit.EndedAt, 0).In(zone)); day = day.AddDate(0, 0, 1) {
			days[day.Format(time.DateOnly)] = true
		}
	}
	pattern.Days = len(days)
	pattern.Hours = math.Round(pattern.Hours*10) / 10
	for weekday, count := range pattern.ArrivalsByWeekday {
		if count > pattern.UsualWeekdayVisits {
			pattern.UsualWeekday, pattern.UsualWeekdayVisits = time.Weekday(weekday).String(), count
		}
	}
	for hour, count := range pattern.ArrivalsByHour {
		if count > pattern.UsualArrivalHourVisits {
			pattern.UsualArrivalHour, pattern.UsualArrivalHourVisits = hour, count
		}
	}
	if len(minutes) > 0 {
		sort.Float64s(minutes)
		pattern.MedianVisitMinutes = math.Round(minutes[len(minutes)/2])
	}
	return pattern, nil
}

// visitsByPlace is every live visit, by the place it names.
func (s *Store) visitsByPlace() (map[string][]Visit, error) {
	visits, err := s.listVisitsUncapped(VisitFilter{TimeFilter: TimeFilter{Limit: math.MaxInt32}})
	if err != nil {
		return nil, err
	}
	byPlace := map[string][]Visit{}
	for _, visit := range visits {
		if visit.PlaceID != nil {
			byPlace[*visit.PlaceID] = append(byPlace[*visit.PlaceID], visit)
		}
	}
	return byPlace, nil
}

// homePlaceIDs is every place of kind home and every place a home period
// names.
func (s *Store) homePlaceIDs(places []Place) (map[string]bool, error) {
	homes := map[string]bool{}
	for _, place := range places {
		if place.Kind == PlaceKindHome {
			homes[place.ID] = true
		}
	}
	periods, err := s.ListHomePeriods(false)
	if err != nil {
		return nil, err
	}
	for _, period := range periods {
		if period.PlaceID != nil {
			homes[*period.PlaceID] = true
		}
	}
	return homes, nil
}

// ListUnnamedPlaces answers GET /unnamed-places: every live place with no
// name that the user has visited, home left out, most days visited first,
// then most hours.
func (s *Store) ListUnnamedPlaces(limit, offset int) ([]UnnamedPlace, error) {
	places, err := s.listPlacesUncapped()
	if err != nil {
		return nil, err
	}
	homes, err := s.homePlaceIDs(places)
	if err != nil {
		return nil, err
	}
	byPlace, err := s.visitsByPlace()
	if err != nil {
		return nil, err
	}
	unnamed := []UnnamedPlace{}
	for position := range places {
		place := &places[position]
		if strings.TrimSpace(place.Name) != "" || homes[place.ID] || len(byPlace[place.ID]) == 0 {
			continue
		}
		pattern, err := visitPattern(place, byPlace[place.ID])
		if err != nil {
			return nil, err
		}
		unnamed = append(unnamed, UnnamedPlace{Place: *place, Pattern: pattern})
	}
	sort.SliceStable(unnamed, func(a, b int) bool {
		if unnamed[a].Pattern.Days != unnamed[b].Pattern.Days {
			return unnamed[a].Pattern.Days > unnamed[b].Pattern.Days
		}
		return unnamed[a].Pattern.Hours > unnamed[b].Pattern.Hours
	})
	offset = min(max(offset, 0), len(unnamed))
	return unnamed[offset:min(len(unnamed), offset+clampLimit(limit, defaultLimit, maximumLimit))], nil
}

// ---- deciding suggestions ----

// osmKindWords are the OpenStreetMap keys that make a named feature a place
// someone goes to: a venue.
var osmKindWords = []string{"amenity", "shop", "leisure", "tourism", "office", "craft", "healthcare", "sport", "club", "historic", "building"}

// featureType is the feature's first type tag, key=value, for the reason.
func featureType(feature OSMFeature) string {
	for _, key := range osmKindWords {
		if value := feature.Tags[key]; value != "" {
			return key + "=" + value
		}
	}
	return "no type tag"
}

// suggestionsFound turns what was found near one place into suggestions.
func suggestionsFound(place *Place, evidence PlaceSuggestionEvidence, proposedBy string) []PlaceSuggestion {
	var found []PlaceSuggestion
	suggest := func(source, ref, name, address, kind string, distance float64, reason string) {
		found = append(found, PlaceSuggestion{PlaceID: place.ID, Source: source, SourceRef: ref, Name: name, Address: address, Kind: kind,
			DistanceMeters: math.Round(distance), Reason: reason, Evidence: evidence, Status: PlaceSuggestionProposed, ProposedBy: proposedBy})
	}
	addressFor := func(own string) string {
		if own != "" {
			return own
		}
		if evidence.NearestAddress != nil && evidence.NearestAddress.DistanceMeters <= suggestionAddressReachMeters {
			return evidence.NearestAddress.Address
		}
		return ""
	}
	pois := 0
	for _, feature := range evidence.NearbyPOIs {
		if feature.DistanceMeters > suggestionPOIReachMeters || pois == suggestionMaximumPOIs {
			continue
		}
		pois++
		rivals := "the only named feature within 25 m"
		if evidence.POIsWithin25Meters != 1 || feature.DistanceMeters > suggestionCloseMeters {
			rivals = fmt.Sprintf("named features within 25 m of the place: %d", evidence.POIsWithin25Meters)
		}
		suggest(PlaceSuggestionSourcePOI, feature.OSMRef, feature.Name, addressFor(feature.Address), PlaceKindVenue, feature.DistanceMeters,
			fmt.Sprintf("OpenStreetMap %s %q (%s) is %.0f m away; %s", feature.OSMRef, feature.Name, featureType(feature), feature.DistanceMeters, rivals))
	}
	byAddress := map[string][]GeocodedCalendarEvent{}
	var order []string
	for _, event := range evidence.CalendarEvents {
		if event.DistanceMeters > suggestionCalendarReachMeters {
			continue
		}
		if byAddress[event.Address.OSMRef] == nil {
			order = append(order, event.Address.OSMRef)
		}
		byAddress[event.Address.OSMRef] = append(byAddress[event.Address.OSMRef], event)
	}
	for _, ref := range order {
		events := byAddress[ref]
		venues := map[string]int{}
		overlapped := 0
		for _, event := range events {
			if event.Venue != "" {
				venues[event.Venue]++
			}
			if event.VisitOverlapped {
				overlapped++
			}
		}
		name, best := events[0].Address.Address, 0
		kind := ""
		for venue, count := range venues {
			if count > best || (count == best && venue < name) {
				name, best, kind = venue, count, PlaceKindVenue
			}
		}
		suggest(PlaceSuggestionSourceCalendar, "calendar:"+ref, name, events[0].Address.Address, kind, events[0].DistanceMeters,
			fmt.Sprintf("calendar events at %s: %d (OpenStreetMap %s, %.0f m away), first %q; a visit here overlapped %d of them",
				events[0].Address.Address, len(events), ref, events[0].DistanceMeters, events[0].Summary, overlapped))
	}
	if nearest := evidence.NearestAddress; nearest != nil && nearest.DistanceMeters <= suggestionAddressReachMeters {
		suggest(PlaceSuggestionSourceAddress, nearest.OSMRef, nearest.Address, nearest.Address, "", nearest.DistanceMeters,
			fmt.Sprintf("the nearest OpenStreetMap address, %s, is %.0f m away", nearest.OSMRef, nearest.DistanceMeters))
	}
	return found
}

// ---- storage ----

const suggestionColumns = `id, place_id, source, source_ref, name, address, kind, distance_meters, reason, evidence, status, proposed_by,
	decided_by, decided_at, decision_note, created_at, updated_at, deleted_at`

func scanSuggestion(scan func(...any) error) (*PlaceSuggestion, error) {
	var suggestion PlaceSuggestion
	var evidence string
	if err := scan(&suggestion.ID, &suggestion.PlaceID, &suggestion.Source, &suggestion.SourceRef, &suggestion.Name, &suggestion.Address,
		&suggestion.Kind, &suggestion.DistanceMeters, &suggestion.Reason, &evidence, &suggestion.Status, &suggestion.ProposedBy,
		&suggestion.DecidedBy, &suggestion.DecidedAt, &suggestion.DecisionNote, &suggestion.CreatedAt, &suggestion.UpdatedAt,
		&suggestion.DeletedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(evidence), &suggestion.Evidence); err != nil {
		return nil, fmt.Errorf("place suggestion %s has evidence that is not JSON: %w", suggestion.ID, err)
	}
	return &suggestion, nil
}

func querySuggestions(q queryer, query string, args ...any) ([]PlaceSuggestion, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := []PlaceSuggestion{}
	for rows.Next() {
		suggestion, err := scanSuggestion(rows.Scan)
		if err != nil {
			return nil, err
		}
		found = append(found, *suggestion)
	}
	return found, rows.Err()
}

func getSuggestionRow(q queryer, id string) (*PlaceSuggestion, error) {
	suggestion, err := scanSuggestion(q.QueryRow(`SELECT `+suggestionColumns+` FROM place_suggestions WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no place suggestion %s", ErrNotFound, id)
	}
	return suggestion, err
}

// GetPlaceSuggestion reads one suggestion, deleted or not.
func (s *Store) GetPlaceSuggestion(id string) (*PlaceSuggestion, error) {
	return getSuggestionRow(s.db, id)
}

// PlaceSuggestionFilter selects suggestions for GET /place-suggestions.
type PlaceSuggestionFilter struct {
	PlaceID        string
	Status         string
	Source         string
	IncludeDeleted bool
	Limit, Offset  int
}

// ListPlaceSuggestions answers GET /place-suggestions, oldest first.
func (s *Store) ListPlaceSuggestions(filter PlaceSuggestionFilter) ([]PlaceSuggestion, error) {
	where, args := []string{}, []any{}
	if !filter.IncludeDeleted {
		where = append(where, "deleted_at = 0")
	}
	if filter.PlaceID != "" {
		where, args = append(where, "place_id = ?"), append(args, filter.PlaceID)
	}
	if filter.Status != "" {
		if !knownWord(PlaceSuggestionStatuses, filter.Status) {
			return nil, fmt.Errorf("%w: unknown status %q: use one of %s", ErrInvalid, filter.Status, strings.Join(PlaceSuggestionStatuses, ", "))
		}
		where, args = append(where, "status = ?"), append(args, filter.Status)
	}
	if filter.Source != "" {
		if !knownWord(PlaceSuggestionSources, filter.Source) {
			return nil, fmt.Errorf("%w: unknown source %q: use one of %s", ErrInvalid, filter.Source, strings.Join(PlaceSuggestionSources, ", "))
		}
		where, args = append(where, "source = ?"), append(args, filter.Source)
	}
	query := `SELECT ` + suggestionColumns + ` FROM place_suggestions`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` ORDER BY seq LIMIT ? OFFSET ?`
	args = append(args, clampLimit(filter.Limit, defaultLimit, maximumLimit), max(filter.Offset, 0))
	return querySuggestions(s.db, query, args...)
}

func encodeEvidence(evidence PlaceSuggestionEvidence) (string, error) {
	encoded, err := json.Marshal(evidence)
	return string(encoded), err
}

// sameSuggestion says a stored suggestion already says what a run found.
func sameSuggestion(stored, found PlaceSuggestion) bool {
	storedEvidence, _ := json.Marshal(stored.Evidence)
	foundEvidence, _ := json.Marshal(found.Evidence)
	return stored.Name == found.Name && stored.Address == found.Address && stored.Kind == found.Kind &&
		stored.DistanceMeters == found.DistanceMeters && stored.Reason == found.Reason && string(storedEvidence) == string(foundEvidence)
}

// ProposePlaceSuggestions decides the suggestions for each place sent and
// settles them against earlier runs': new ones made, changed ones rewritten,
// proposed ones no longer found deleted. One accepted or rejected is never
// changed. Proposed suggestions for a place that has since been named, by
// any means, are deleted too.
func (s *Store) ProposePlaceSuggestions(request PlaceSuggestionProposals) (*PlaceSuggesting, error) {
	if strings.TrimSpace(request.ProposedBy) == "" {
		return nil, fmt.Errorf("%w: proposed_by is required: who found these", ErrInvalid)
	}
	byPlace, err := s.visitsByPlace()
	if err != nil {
		return nil, err
	}
	answer := &PlaceSuggesting{DryRun: request.DryRun, Places: []PlaceSuggestionsForPlace{}, Actions: map[string]int{}}
	type ranked struct {
		forPlace PlaceSuggestionsForPlace
		days     int
	}
	var all []ranked
	err = s.inTransaction(func(tx *sql.Tx) error {
		at := now()
		seen := map[string]bool{}
		for _, sent := range request.Places {
			if seen[sent.PlaceID] {
				return fmt.Errorf("%w: place %s is sent twice", ErrInvalid, sent.PlaceID)
			}
			seen[sent.PlaceID] = true
			place, err := getPlaceRow(tx, sent.PlaceID)
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("%w: %v", ErrInvalid, err)
			}
			if err != nil {
				return err
			}
			if place.DeletedAt != 0 || strings.TrimSpace(place.Name) != "" {
				return fmt.Errorf("%w: place %s is deleted or already named %q: suggest names only for unnamed places", ErrConflict, place.ID, place.Name)
			}
			pattern, err := visitPattern(place, byPlace[place.ID])
			if err != nil {
				return err
			}
			evidence := PlaceSuggestionEvidence{Pattern: pattern, NearbyPOIs: sent.NearbyPOIs, NearestAddress: sent.NearestAddress,
				CalendarEvents: sent.CalendarEvents, Sources: request.Sources}
			if evidence.NearbyPOIs == nil {
				evidence.NearbyPOIs = []OSMFeature{}
			}
			if evidence.CalendarEvents == nil {
				evidence.CalendarEvents = []GeocodedCalendarEvent{}
			}
			if evidence.Sources == nil {
				evidence.Sources = []string{}
			}
			sort.SliceStable(evidence.NearbyPOIs, func(a, b int) bool {
				return evidence.NearbyPOIs[a].DistanceMeters < evidence.NearbyPOIs[b].DistanceMeters
			})
			for _, feature := range evidence.NearbyPOIs {
				if feature.DistanceMeters <= suggestionCloseMeters {
					evidence.POIsWithin25Meters++
				}
			}
			for position := range evidence.CalendarEvents {
				event := &evidence.CalendarEvents[position]
				event.VisitOverlapped = false
				for _, visit := range byPlace[place.ID] {
					if overlapSeconds(event.StartedAt, event.EndedAt, visit.StartedAt, visit.EndedAt) > 0 {
						event.VisitOverlapped = true
						break
					}
				}
			}
			found := suggestionsFound(place, evidence, request.ProposedBy)
			stored, err := querySuggestions(tx, `SELECT `+suggestionColumns+` FROM place_suggestions WHERE place_id = ? ORDER BY seq`, place.ID)
			if err != nil {
				return err
			}
			byKey := map[string]*PlaceSuggestion{}
			for position := range stored {
				byKey[stored[position].Source+"\x00"+stored[position].SourceRef] = &stored[position]
			}
			live := []PlaceSuggestion{}
			kept := map[string]bool{}
			for _, suggestion := range found {
				existing := byKey[suggestion.Source+"\x00"+suggestion.SourceRef]
				var action string
				switch {
				case existing == nil:
					action = suggestionCreate
					if !request.DryRun {
						if suggestion.ID, err = insertSuggestion(tx, suggestion, at); err != nil {
							return err
						}
						suggestion.CreatedAt, suggestion.UpdatedAt = at, at
					}
				case existing.Status != PlaceSuggestionProposed:
					action, suggestion = suggestionKeptDecided, *existing
				case existing.DeletedAt == 0 && sameSuggestion(*existing, suggestion):
					action, suggestion = suggestionUnchanged, *existing
				default:
					action = suggestionUpdate
					suggestion.ID, suggestion.CreatedAt, suggestion.UpdatedAt = existing.ID, existing.CreatedAt, at
					if !request.DryRun {
						if err := updateSuggestion(tx, suggestion, at); err != nil {
							return err
						}
					}
				}
				if existing != nil {
					kept[existing.ID] = true
				}
				answer.Actions[action]++
				if suggestion.DeletedAt == 0 {
					live = append(live, suggestion)
				}
			}
			for _, existing := range stored {
				if kept[existing.ID] || existing.Status != PlaceSuggestionProposed || existing.DeletedAt != 0 {
					continue
				}
				answer.Actions[suggestionDelete]++
				if !request.DryRun {
					if _, err := tx.Exec(`UPDATE place_suggestions SET deleted_at = ?, updated_at = ? WHERE id = ?`, at, at, existing.ID); err != nil {
						return err
					}
				}
			}
			all = append(all, ranked{forPlace: PlaceSuggestionsForPlace{Place: *place, Suggestions: live}, days: pattern.Days})
		}
		// Proposed suggestions for a place named since, by anyone, are moot.
		named, err := querySuggestions(tx, `SELECT `+suggestionColumns+` FROM place_suggestions WHERE status = ? AND deleted_at = 0
			AND place_id IN (SELECT id FROM places WHERE TRIM(name) != '' OR deleted_at != 0)`, PlaceSuggestionProposed)
		if err != nil {
			return err
		}
		for _, suggestion := range named {
			answer.Actions[suggestionDelete]++
			if !request.DryRun {
				if _, err := tx.Exec(`UPDATE place_suggestions SET deleted_at = ?, updated_at = ? WHERE id = ?`, at, at, suggestion.ID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].days > all[b].days })
	for _, one := range all {
		answer.Places = append(answer.Places, one.forPlace)
	}
	return answer, nil
}

func insertSuggestion(tx *sql.Tx, suggestion PlaceSuggestion, at int64) (string, error) {
	id, seq, err := nextID(tx, "place_suggestions", "place_suggestion")
	if err != nil {
		return "", err
	}
	evidence, err := encodeEvidence(suggestion.Evidence)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(`INSERT INTO place_suggestions (id, seq, place_id, source, source_ref, name, address, kind, distance_meters, reason,
		evidence, status, proposed_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, seq, suggestion.PlaceID,
		suggestion.Source, suggestion.SourceRef, suggestion.Name, suggestion.Address, suggestion.Kind, suggestion.DistanceMeters,
		suggestion.Reason, evidence, PlaceSuggestionProposed, suggestion.ProposedBy, at, at)
	return id, err
}

func updateSuggestion(tx *sql.Tx, suggestion PlaceSuggestion, at int64) error {
	evidence, err := encodeEvidence(suggestion.Evidence)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE place_suggestions SET name = ?, address = ?, kind = ?, distance_meters = ?, reason = ?, evidence = ?,
		proposed_by = ?, updated_at = ?, deleted_at = 0 WHERE id = ?`, suggestion.Name, suggestion.Address, suggestion.Kind,
		suggestion.DistanceMeters, suggestion.Reason, evidence, suggestion.ProposedBy, at, suggestion.ID)
	return err
}

// ---- deciding ----

func (s *Store) liveProposedSuggestion(tx *sql.Tx, id string, decision PlaceSuggestionDecision) (*PlaceSuggestion, error) {
	if strings.TrimSpace(decision.DecidedBy) == "" {
		return nil, fmt.Errorf("%w: decided_by is required: who decided, and on what evidence belongs in note", ErrInvalid)
	}
	suggestion, err := getSuggestionRow(tx, id)
	if err != nil {
		return nil, err
	}
	if suggestion.DeletedAt != 0 {
		return nil, fmt.Errorf("%w: place suggestion %s is deleted: a later run no longer found it", ErrConflict, id)
	}
	if suggestion.Status != PlaceSuggestionProposed {
		return nil, fmt.Errorf("%w: place suggestion %s is already %s by %s", ErrConflict, id, suggestion.Status, suggestion.DecidedBy)
	}
	return suggestion, nil
}

func decideSuggestion(tx *sql.Tx, id, status, decidedBy, note string, at int64) error {
	_, err := tx.Exec(`UPDATE place_suggestions SET status = ?, decided_by = ?, decided_at = ?, decision_note = ?, updated_at = ? WHERE id = ?`,
		status, strings.TrimSpace(decidedBy), at, strings.TrimSpace(note), at, id)
	return err
}

// AcceptPlaceSuggestion names the place from the suggestion: its name,
// and its address and kind where the place has none (a kind only replaces
// other), with a description saying where the name came from unless the
// place has one. A place someone already named is never renamed. The
// place's other proposed suggestions are rejected, naming this one.
func (s *Store) AcceptPlaceSuggestion(id string, decision PlaceSuggestionDecision) (*PlaceSuggestion, error) {
	err := s.inTransaction(func(tx *sql.Tx) error {
		suggestion, err := s.liveProposedSuggestion(tx, id, decision)
		if err != nil {
			return err
		}
		place, err := getPlaceRow(tx, suggestion.PlaceID)
		if err != nil {
			return err
		}
		if place.DeletedAt != 0 {
			return fmt.Errorf("%w: place %s is deleted", ErrConflict, place.ID)
		}
		if strings.TrimSpace(place.Name) != "" {
			return fmt.Errorf("%w: place %s is already named %q; a suggestion never renames a place", ErrConflict, place.ID, place.Name)
		}
		name, address, kind := firstNonBlank(decision.Name, suggestion.Name), firstNonBlank(decision.Address, suggestion.Address),
			firstNonBlank(decision.Kind, suggestion.Kind)
		if name == "" {
			return fmt.Errorf("%w: the suggestion has no name; send one", ErrInvalid)
		}
		if kind != "" && !knownWord(PlaceKinds, kind) {
			return fmt.Errorf("%w: kind %q: use one of %s", ErrInvalid, kind, strings.Join(PlaceKinds, ", "))
		}
		at := now()
		if err := snapshot(tx, EntityPlace, place.ID, "suggestion", at); err != nil {
			return err
		}
		place.Name = name
		if strings.TrimSpace(place.Address) == "" {
			place.Address = address
		}
		if kind != "" && place.Kind == PlaceKindOther {
			place.Kind = kind
		}
		if strings.TrimSpace(place.Description) == "" {
			place.Description = fmt.Sprintf("Named from %s: %s.", suggestion.ID, suggestion.Reason)
			if note := strings.TrimSpace(decision.Note); note != "" {
				place.Description += " " + note
			}
			place.DescribedBy = strings.TrimSpace(decision.DecidedBy)
		}
		if err := validatePlace(place); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE places SET name = ?, address = ?, kind = ?, description = ?, described_by = ?, updated_at = ? WHERE id = ?`,
			place.Name, place.Address, place.Kind, place.Description, place.DescribedBy, at, place.ID); err != nil {
			return err
		}
		note := strings.TrimSpace(decision.Note)
		if name != suggestion.Name {
			note = strings.TrimSpace(fmt.Sprintf("named %q. %s", name, note))
		}
		if err := decideSuggestion(tx, id, PlaceSuggestionAccepted, decision.DecidedBy, note, at); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE place_suggestions SET status = ?, decided_by = ?, decided_at = ?, decision_note = ?, updated_at = ?
			WHERE place_id = ? AND id != ? AND status = ? AND deleted_at = 0`, PlaceSuggestionRejected, strings.TrimSpace(decision.DecidedBy), at,
			id+" was accepted for this place", at, place.ID, id, PlaceSuggestionProposed)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetPlaceSuggestion(id)
}

// RejectPlaceSuggestion records that the suggestion is wrong; no run
// proposes it again.
func (s *Store) RejectPlaceSuggestion(id string, decision PlaceSuggestionDecision) (*PlaceSuggestion, error) {
	err := s.inTransaction(func(tx *sql.Tx) error {
		if _, err := s.liveProposedSuggestion(tx, id, decision); err != nil {
			return err
		}
		return decideSuggestion(tx, id, PlaceSuggestionRejected, decision.DecidedBy, decision.Note, now())
	})
	if err != nil {
		return nil, err
	}
	return s.GetPlaceSuggestion(id)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
