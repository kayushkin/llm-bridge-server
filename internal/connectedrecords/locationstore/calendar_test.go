package locationstore

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Every event, calendar, place and town here is invented; the towns and the
// days are journeyBatch's (home in Hometown, a cafe in Suburbia each day, a
// hotel in Fartown on days 20 to 23).

func at(h float64) string { return time.Unix(hours(h), 0).UTC().Format(time.RFC3339) }

func timedEvent(id, summary, location string, from, to float64) GoogleCalendarEvent {
	return GoogleCalendarEvent{ID: id, Status: "confirmed", EventType: "default", Summary: summary, Location: location,
		Start: GoogleEventTime{DateTime: at(from)}, End: GoogleEventTime{DateTime: at(to)}}
}

func recurringEvent(series string, day int, summary, location string, fromHour, toHour float64) GoogleCalendarEvent {
	event := timedEvent(fmt.Sprintf("%s_%d", series, day), summary, location, float64(day*24)+fromHour, float64(day*24)+toHour)
	event.RecurringEventID = series
	return event
}

// newCalendarService is journeyBatch with a flight inside the Fartown stay,
// named towns, grouped journeys, and one place named by hand at the cafe.
func newCalendarService(t *testing.T) (*testService, string) {
	t.Helper()
	service := newJourneyService(t)
	startedAt, endedAt := hours(21*24+10), hours(21*24+12)
	service.startImport(ImportKindGoogleTimeline, strings.Repeat("b", 64))
	service.must(http.MethodPost, "/imports/import_000002/batches", ImportBatch{Trips: []ImportTrip{{Source: SourceGoogleTimeline,
		SourceRef: fmt.Sprintf("%d-%d", startedAt, endedAt), StartedAt: startedAt, EndedAt: endedAt, StartLatitude: float(11.501),
		StartLongitude: float(20.001), EndLatitude: float(11.9), EndLongitude: float(20.001), Mode: ModeFlying, SourceMode: "FLYING"}}}, nil, http.StatusOK)
	service.must(http.MethodPost, "/imports/import_000002/finish", nil, nil, http.StatusOK)
	service.must(http.MethodPost, "/localities/fill", LocalityFillRequest{}, nil, http.StatusOK)
	var grouping JourneyGrouping
	service.must(http.MethodPost, "/journeys/group", JourneyGroupRequest{}, &grouping, http.StatusOK)
	if len(grouping.Journeys) != 1 {
		t.Fatalf("journeys = %+v, want the Fartown one", grouping.Journeys)
	}
	service.must(http.MethodPost, "/places", PlaceCreate{Name: "Example Hall", Address: "1 Fake Road, Suburbia", Latitude: float(10.0601),
		Longitude: float(20.0601), Kind: PlaceKindVenue}, nil, http.StatusCreated)
	return service, grouping.Journeys[0].TripID
}

func calendarRequest(dryRun bool, owned []GoogleCalendarEvent, readOnly []GoogleCalendarEvent) CalendarMatchRequest {
	return CalendarMatchRequest{DryRun: dryRun, From: hours(-24), To: hours(40 * 24), Calendars: []CalendarEvents{
		{Calendar: GoogleCalendar{ID: "own@example.invalid", AccessRole: "owner", TimeZone: "Africa/Ndjamena"}, Events: owned},
		{Calendar: GoogleCalendar{ID: "shared@example.invalid", AccessRole: "reader", TimeZone: "UTC"}, Events: readOnly},
	}}
}

func calendarEvents() ([]GoogleCalendarEvent, []GoogleCalendarEvent) {
	owned := []GoogleCalendarEvent{
		// 1: the location names a known place, and the user was at the cafe
		// beside it.
		timedEvent("hall", "Reading group", "Example Hall, 1 Fake Road, Suburbia", 5*24+10, 5*24+11),
		// 2: a flight, recorded, inside the Fartown journey.
		timedEvent("flight", "Flight to Elsewhere (XX 123)", "Fartown FAR", 21*24+10, 21*24+12),
		// 3: a single event with no location during the journey.
		timedEvent("wedding", "A wedding", "", 22*24+12, 22*24+18),
		// A single event in Suburbia; the user arrived at the cafe 15
		// minutes before.
		timedEvent("talk", "A talk", "Some Venue, 2 Fake Street, Suburbia", 6*24+8.75, 6*24+10),
		// A video call never links.
		timedEvent("call", "Standup", "Microsoft Teams Meeting", 7*24+9, 7*24+9.5),
		// A canceled event never links.
		{ID: "canceled", Status: "cancelled", Summary: "Gone", Location: "Example Hall, Suburbia",
			Start: GoogleEventTime{DateTime: at(5*24 + 10)}, End: GoogleEventTime{DateTime: at(5*24 + 11)}},
		// An all-day birthday never links.
		{ID: "birthday", Status: "confirmed", EventType: "birthday", Summary: "A birthday",
			Start: GoogleEventTime{Date: "2026-01-22"}, End: GoogleEventTime{Date: "2026-01-23"}},
	}
	// 4: a weekly routine with no location at 06:00 links nothing, even
	// during the journey.
	for _, day := range []int{0, 7, 14, 21, 28} {
		owned = append(owned, recurringEvent("skating", day, "Skating", "", 6, 7.5))
	}
	// A daily routine whose location names only a town, where the user was
	// at the same place (the cafe) each time: place and time agree.
	for _, day := range []int{1, 2, 3, 4} {
		owned = append(owned, recurringEvent("class", day, "Class", "A Studio, Suburbia", 9, 10))
	}
	// A weekly routine in Suburbia at an hour the user was at home links
	// nothing.
	for _, day := range []int{1, 8, 15} {
		owned = append(owned, recurringEvent("evening", day, "Evening class", "A Studio, Suburbia", 20, 21))
	}
	// On a calendar the user only reads, an event with no location during
	// the journey is someone else's day off.
	readOnly := []GoogleCalendarEvent{timedEvent("pto", "Colleague away", "", 22*24, 22*24+8)}
	return owned, readOnly
}

func linksOf(matching CalendarMatching) map[string][]CalendarEventLink {
	byEvent := map[string][]CalendarEventLink{}
	for _, planned := range matching.Links {
		byEvent[planned.Link.EventID] = append(byEvent[planned.Link.EventID], planned.Link)
	}
	return byEvent
}

func TestCalendarEventsLinkByPlaceFlightJourneyAndNeverByRoutineTime(t *testing.T) {
	service, journeyID := newCalendarService(t)
	owned, readOnly := calendarEvents()

	var dry CalendarMatching
	service.must(http.MethodPost, "/calendar/match", calendarRequest(true, owned, readOnly), &dry, http.StatusOK)
	var stored []CalendarEventLink
	service.must(http.MethodGet, "/calendar-event-links", nil, &stored, http.StatusOK)
	if len(stored) != 0 || dry.Actions[CalendarLinkCreate] == 0 {
		t.Fatalf("dry run wrote %d links, planned %v", len(stored), dry.Actions)
	}

	var matching CalendarMatching
	service.must(http.MethodPost, "/calendar/match", calendarRequest(false, owned, readOnly), &matching, http.StatusOK)
	byEvent := linksOf(matching)
	expect := func(eventID, targetType, rule string) CalendarEventLink {
		t.Helper()
		for _, link := range byEvent[eventID] {
			if link.TargetType == targetType && link.Rule == rule {
				return link
			}
		}
		t.Fatalf("event %s: links %+v, want a %s link by %s", eventID, byEvent[eventID], targetType, rule)
		return CalendarEventLink{}
	}
	hall := expect("hall", EntityVisit, CalendarRulePlace)
	var visit Visit
	service.must(http.MethodGet, "/visits/"+hall.TargetID, nil, &visit, http.StatusOK)
	if visit.PlaceID == nil || *visit.PlaceID != "place_000003" || len(visit.CalendarEventLinks) != 1 {
		t.Errorf("the hall's visit = %+v, want the cafe visit carrying its link", visit)
	}
	flight := expect("flight", EntityTrip, CalendarRuleFlight)
	if flight.TargetID == journeyID {
		t.Errorf("the flight linked to the journey, not the flight")
	}
	if link := expect("flight", EntityTrip, CalendarRuleFlightJourney); link.TargetID != journeyID {
		t.Errorf("flight journey link = %+v, want %s", link, journeyID)
	}
	if link := expect("wedding", EntityTrip, CalendarRuleJourneyTime); link.TargetID != journeyID || link.Confidence >= confidencePlace {
		t.Errorf("wedding link = %+v", link)
	}
	expect("talk", EntityVisit, CalendarRuleTownVisit)
	for _, day := range []int{1, 2, 3, 4} {
		expect(fmt.Sprintf("class_%d", day), EntityVisit, CalendarRuleSeriesPlace)
	}
	for _, eventID := range []string{"call", "canceled", "birthday", "pto", "skating_0", "skating_21", "evening_1", "evening_8"} {
		if len(byEvent[eventID]) != 0 {
			t.Errorf("event %s linked to %+v; it must link nothing", eventID, byEvent[eventID])
		}
	}
	if matching.Unlinked[unlinkedRoutineNoLocation] != 5 || matching.Unlinked[unlinkedOnline] != 1 || matching.Unlinked[unlinkedReadOnlyNoLocation] != 1 {
		t.Errorf("unlinked = %v", matching.Unlinked)
	}

	var summary JourneySummary
	service.must(http.MethodGet, "/journeys/"+journeyID, nil, &summary, http.StatusOK)
	if len(summary.CalendarEventLinks) != 3 {
		t.Errorf("journey summary carries %d links, want the flight's two and the wedding's", len(summary.CalendarEventLinks))
	}
	var atCafe []CalendarEventLink
	service.must(http.MethodGet, "/calendar-event-links?place_id=place_000003", nil, &atCafe, http.StatusOK)
	if len(atCafe) != 6 {
		t.Errorf("links at the cafe = %d, want the hall, the talk and four classes", len(atCafe))
	}

	// A re-run changes nothing. (Each answer is read into a fresh value:
	// decoding JSON into a map merges.)
	matching = CalendarMatching{}
	service.must(http.MethodPost, "/calendar/match", calendarRequest(false, owned, readOnly), &matching, http.StatusOK)
	if len(matching.Actions) != 1 || matching.Actions[CalendarLinkUnchanged] != len(matching.Links) {
		t.Fatalf("a re-run = %v, want everything unchanged", matching.Actions)
	}

	// A person deletes the talk's link; a re-run leaves it deleted. The
	// wedding leaves the calendar; a re-run deletes its link.
	talk := linksOf(matching)["talk"][0]
	service.must(http.MethodDelete, "/calendar-event-links/"+talk.ID, nil, nil, http.StatusNoContent)
	var withoutWedding []GoogleCalendarEvent
	for _, event := range owned {
		if event.ID != "wedding" {
			withoutWedding = append(withoutWedding, event)
		}
	}
	matching = CalendarMatching{}
	service.must(http.MethodPost, "/calendar/match", calendarRequest(false, withoutWedding, readOnly), &matching, http.StatusOK)
	if matching.Actions[CalendarLinkKeptDeleted] != 1 || matching.Actions[CalendarLinkDelete] != 1 {
		t.Errorf("after a hand delete and a removed event: %v", matching.Actions)
	}
	var trip Trip
	service.must(http.MethodGet, "/trips/"+journeyID, nil, &trip, http.StatusOK)
	for _, link := range trip.CalendarEventLinks {
		if link.EventID == "wedding" {
			t.Errorf("the removed event still links: %+v", link)
		}
	}

	// A link made by hand is the person's: a run neither changes nor deletes it.
	var made CalendarEventLink
	service.must(http.MethodPost, "/calendar-event-links", CalendarEventLinkCreate{CalendarID: "own@example.invalid", EventID: "skating_7",
		StartedAt: hours(7*24 + 6), EndedAt: hours(7*24 + 7.5), TargetType: EntityPlace, TargetID: "place_000004", Reason: "the rink"}, &made, http.StatusCreated)
	service.must(http.MethodPost, "/calendar/match", calendarRequest(false, withoutWedding, readOnly), &matching, http.StatusOK)
	service.must(http.MethodGet, "/calendar-event-links/"+made.ID, nil, &made, http.StatusOK)
	if made.MatchedBy != SetByHand || made.Rule != CalendarRuleHand || made.DeletedAt != 0 {
		t.Errorf("hand link after a run = %+v", made)
	}
	if status, body := service.call(http.MethodPost, "/calendar-event-links", CalendarEventLinkCreate{CalendarID: "c", EventID: "e",
		StartedAt: 1, EndedAt: 2, TargetType: EntityVisit, TargetID: "visit_999999"}, nil); status != http.StatusBadRequest {
		t.Errorf("a link to no visit = %d %s, want 400", status, body)
	}
}

func TestTownsAreReadFromALocationOffline(t *testing.T) {
	service := newTestService(t)
	writeTownFixture(t, service.store.GeonamesDir())
	index, err := service.store.townIndex()
	if err != nil {
		t.Fatal(err)
	}
	matcher := &calendarMatcher{index: index}
	for location, want := range map[string]string{
		"A Venue, 3 Fake Street, Suburbia, 01 12345, XX": "Suburbia",
		"Room 4 (Fartown, Example Region, XX)":           "Fartown",
		"Hometown":                                       "", // the first part names the venue, never the town
		"A Venue, Nowhere":                               "",
	} {
		if _, name := matcher.townsNamedIn(location); name != want {
			t.Errorf("town of %q = %q, want %q", location, name, want)
		}
	}
}
