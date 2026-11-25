package locationstore

import (
	"fmt"
	"net/http"
	"testing"
)

// A weekly class with no location at 09:00, when the user arrives at the
// cafe half an hour early each time; a daily one at 12:00, when they have
// been at the cafe since 08:30 (there for something else, not for it).
func unlocatedSeriesEvents() []GoogleCalendarEvent {
	var events []GoogleCalendarEvent
	for _, day := range []int{1, 8, 15, 26, 33} {
		events = append(events, recurringEvent("weekly", day, "Weekly class", "", 9, 10))
	}
	for _, day := range []int{2, 3, 4} {
		events = append(events, recurringEvent("noon", day, "Noon thing", "", 12, 13))
	}
	// The same weekly class during the Fartown journey: away, so it neither
	// counts for the spot nor against it.
	events = append(events, recurringEvent("weekly", 22, "Weekly class", "", 9, 10))
	return events
}

func seriesPlaceOf(t *testing.T, matching CalendarMatching, recurringEventID string) CalendarSeriesPlace {
	t.Helper()
	for _, series := range matching.SeriesPlaces {
		if series.RecurringEventID == recurringEventID {
			return series
		}
	}
	t.Fatalf("no series place for %s in %+v", recurringEventID, matching.SeriesPlaces)
	return CalendarSeriesPlace{}
}

func TestARoutineWithNoLocationLinksWhereTheUserArrivesForIt(t *testing.T) {
	service, _ := newCalendarService(t)
	owned, readOnly := calendarEvents()
	owned = append(owned, unlocatedSeriesEvents()...)

	var matching CalendarMatching
	service.must(http.MethodPost, "/calendar/match", calendarRequest(false, owned, readOnly), &matching, http.StatusOK)
	byEvent := linksOf(matching)
	for _, day := range []int{1, 8, 15, 26, 33} {
		links := byEvent[fmt.Sprintf("weekly_%d", day)]
		if len(links) != 1 || links[0].Rule != CalendarRuleUnlocatedSeriesPlace || links[0].TargetType != EntityVisit || links[0].Confidence != 0.8 {
			t.Fatalf("weekly_%d links = %+v, want one unlocated_series_place link at 0.8", day, links)
		}
		var visit Visit
		service.must(http.MethodGet, "/visits/"+links[0].TargetID, nil, &visit, http.StatusOK)
		if visit.PlaceID == nil || *visit.PlaceID != "place_000003" {
			t.Errorf("weekly_%d linked to %+v, want the cafe's visit", day, visit)
		}
	}
	for _, eventID := range []string{"weekly_22", "noon_2", "noon_3", "noon_4"} {
		if len(byEvent[eventID]) != 0 {
			t.Errorf("%s linked to %+v; it must link nothing", eventID, byEvent[eventID])
		}
	}
	weekly := seriesPlaceOf(t, matching, "weekly")
	if weekly.ID == "" || weekly.Outcome != SeriesOutcomeLinked || weekly.PlaceID == nil || *weekly.PlaceID != "place_000003" ||
		weekly.Instances != 6 || weekly.InstancesOut != 5 || weekly.InstancesAtPlace != 5 || weekly.Agreement != 1 {
		t.Errorf("weekly series place = %+v", weekly)
	}
	// The hall named by hand lies beside the cafe: one spot.
	if len(weekly.SpotPlaceIDs) != 2 || weekly.SpotPlaceIDs[1] != "place_000004" {
		t.Errorf("weekly spot = %v, want the cafe and the hall beside it", weekly.SpotPlaceIDs)
	}
	if noon := seriesPlaceOf(t, matching, "noon"); noon.Outcome != SeriesOutcomeTooFewTimes || noon.PlaceID != nil || noon.InstancesOut != 3 {
		t.Errorf("noon series place = %+v, want out three times and arriving for none", noon)
	}
	if skating := seriesPlaceOf(t, matching, "skating"); skating.Outcome != SeriesOutcomeNeverOut {
		t.Errorf("skating series place = %+v, want never out: the user was home at 06:00", skating)
	}

	var stored []CalendarSeriesPlace
	service.must(http.MethodGet, "/calendar-series-places?place_id=place_000004&outcome=linked", nil, &stored, http.StatusOK)
	if len(stored) != 1 || stored[0].RecurringEventID != "weekly" || stored[0].PlaceName != "" {
		t.Errorf("series places at the hall = %+v", stored)
	}
	service.must(http.MethodGet, "/calendar-series-places/"+weekly.ID, nil, &stored[0], http.StatusOK)
	if status, _ := service.call(http.MethodGet, "/calendar-series-places?outcome=maybe", nil, nil); status != http.StatusBadRequest {
		t.Errorf("an unknown outcome = %d, want 400", status)
	}

	// Once the cafe is known to be work, a routine there is not held there:
	// the links go, and the run deletes them.
	service.must(http.MethodPatch, "/places/place_000003", map[string]any{"kind": PlaceKindWork}, nil, http.StatusOK)
	matching = CalendarMatching{}
	service.must(http.MethodPost, "/calendar/match", calendarRequest(false, owned, readOnly), &matching, http.StatusOK)
	if links := linksOf(matching)["weekly_1"]; len(links) != 1 || links[0].DeletedAt == 0 {
		t.Errorf("weekly_1 after the cafe became work = %+v, want its link deleted", links)
	}
	if weekly := seriesPlaceOf(t, matching, "weekly"); weekly.Outcome == SeriesOutcomeLinked || weekly.ID != stored[0].ID {
		t.Errorf("weekly series place after the cafe became work = %+v, want the same row, not linked", weekly)
	}
}
