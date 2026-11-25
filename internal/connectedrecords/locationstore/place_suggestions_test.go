package locationstore

import (
	"net/http"
	"strings"
	"testing"
)

// Every feature and address here is invented, near the made-up cafe at
// 10.06, 20.06.
func cafeEvidence() PlaceProposalEvidence {
	return PlaceProposalEvidence{PlaceID: "place_000003",
		NearbyPOIs: []OSMFeature{
			{OSMRef: "node/2", Name: "Corner Shop", Tags: map[string]string{"shop": "convenience"}, Latitude: 10.0607, Longitude: 20.06, DistanceMeters: 78},
			{OSMRef: "node/1", Name: "Example Cafe", Tags: map[string]string{"amenity": "cafe"}, Latitude: 10.0601, Longitude: 20.06, DistanceMeters: 11,
				Address: "1 Fake Road, Suburbia"},
			{OSMRef: "node/3", Name: "Far Away", Tags: map[string]string{"shop": "bakery"}, Latitude: 10.062, Longitude: 20.06, DistanceMeters: 140},
		},
		NearestAddress: &OSMAddress{OSMRef: "way/9", Address: "3 Fake Road, Suburbia", Latitude: 10.0601, Longitude: 20.0601, DistanceMeters: 15},
		CalendarEvents: []GeocodedCalendarEvent{{CalendarID: "own@example.invalid", EventID: "meetup", Summary: "Meetup",
			Location: "Example Cafe, 1 Fake Road, Suburbia", StartedAt: hours(5*24 + 10), EndedAt: hours(5*24 + 11), Venue: "Example Cafe",
			Address: OSMAddress{OSMRef: "node/1", Address: "1 Fake Road, Suburbia", Latitude: 10.0601, Longitude: 20.06}, DistanceMeters: 11}},
	}
}

func TestPlaceNameSuggestionsAreProposedAndNameAPlaceOnlyWhenAccepted(t *testing.T) {
	service, _ := newCalendarService(t)
	var unnamed []UnnamedPlace
	service.must(http.MethodGet, "/unnamed-places", nil, &unnamed, http.StatusOK)
	if len(unnamed) != 2 || unnamed[0].Place.ID != "place_000003" || unnamed[1].Place.ID != "place_000002" {
		t.Fatalf("unnamed places = %+v, want the cafe then the hotel, home left out", unnamed)
	}
	cafe := unnamed[0].Pattern
	if cafe.Days != 30 || cafe.Visits != 30 || cafe.UsualArrivalHour != 9 || cafe.TimeZone != "Africa/Ndjamena" || cafe.MedianVisitMinutes != 540 {
		t.Errorf("the cafe's pattern = %+v", cafe)
	}

	request := PlaceSuggestionProposals{DryRun: true, ProposedBy: SetByPlaceNameSuggesting, Sources: []string{"OpenStreetMap example.osm.pbf"},
		Places: []PlaceProposalEvidence{cafeEvidence(), {PlaceID: "place_000002"}}}
	var answer PlaceSuggesting
	service.must(http.MethodPost, "/place-suggestions/proposals", request, &answer, http.StatusOK)
	var stored []PlaceSuggestion
	service.must(http.MethodGet, "/place-suggestions", nil, &stored, http.StatusOK)
	if len(stored) != 0 || answer.Actions[suggestionCreate] != 4 {
		t.Fatalf("dry run stored %d, planned %v", len(stored), answer.Actions)
	}

	request.DryRun = false
	answer = PlaceSuggesting{}
	service.must(http.MethodPost, "/place-suggestions/proposals", request, &answer, http.StatusOK)
	if len(answer.Places) != 2 || answer.Places[0].Place.ID != "place_000003" || len(answer.Places[0].Suggestions) != 4 || len(answer.Places[1].Suggestions) != 0 {
		t.Fatalf("answer = %+v", answer.Places)
	}
	bySource := map[string]PlaceSuggestion{}
	for _, suggestion := range answer.Places[0].Suggestions {
		bySource[suggestion.Source+" "+suggestion.SourceRef] = suggestion
	}
	poi, calendar := bySource["osm_poi node/1"], bySource["calendar_address calendar:node/1"]
	if poi.ID == "" || poi.Name != "Example Cafe" || poi.Kind != PlaceKindVenue || !strings.Contains(poi.Reason, "the only named feature within 25 m") ||
		poi.Evidence.POIsWithin25Meters != 1 || poi.Evidence.Pattern.Days != 30 {
		t.Errorf("the cafe's POI suggestion = %+v", poi)
	}
	if calendar.Name != "Example Cafe" || !calendar.Evidence.CalendarEvents[0].VisitOverlapped || !strings.Contains(calendar.Reason, "overlapped 1") {
		t.Errorf("the cafe's calendar suggestion = %+v", calendar)
	}
	if _, found := bySource["osm_poi node/3"]; found {
		t.Errorf("a feature 140 m away was suggested")
	}
	if address := bySource["osm_address way/9"]; address.Name != "3 Fake Road, Suburbia" || address.Kind != "" {
		t.Errorf("the address suggestion = %+v", address)
	}

	// A re-run finding the same changes nothing.
	answer = PlaceSuggesting{}
	service.must(http.MethodPost, "/place-suggestions/proposals", request, &answer, http.StatusOK)
	if len(answer.Actions) != 1 || answer.Actions[suggestionUnchanged] != 4 {
		t.Errorf("a re-run = %v, want everything unchanged", answer.Actions)
	}

	// Accepting needs who decided; then names the place, once.
	if status, _ := service.call(http.MethodPost, "/place-suggestions/"+poi.ID+"/accept", PlaceSuggestionDecision{}, nil); status != http.StatusBadRequest {
		t.Errorf("accept without decided_by = %d, want 400", status)
	}
	var accepted PlaceSuggestion
	service.must(http.MethodPost, "/place-suggestions/"+poi.ID+"/accept", PlaceSuggestionDecision{DecidedBy: "weekly job",
		Note: "one cafe within 25 m; weekday mornings fit"}, &accepted, http.StatusOK)
	var place Place
	service.must(http.MethodGet, "/places/place_000003", nil, &place, http.StatusOK)
	if accepted.Status != PlaceSuggestionAccepted || place.Name != "Example Cafe" || place.Kind != PlaceKindVenue || place.Address != "1 Fake Road, Suburbia" ||
		place.DescribedBy != "weekly job" || !strings.Contains(place.Description, poi.ID) {
		t.Fatalf("after accepting: suggestion %+v, place %+v", accepted, place)
	}
	var revisions []Revision
	service.must(http.MethodGet, "/places/place_000003/revisions", nil, &revisions, http.StatusOK)
	if len(revisions) == 0 || revisions[0].Reason != "suggestion" {
		t.Errorf("revisions = %+v, want the unnamed place kept", revisions)
	}
	var sibling PlaceSuggestion
	service.must(http.MethodGet, "/place-suggestions/"+calendar.ID, nil, &sibling, http.StatusOK)
	if sibling.Status != PlaceSuggestionRejected || !strings.Contains(sibling.DecisionNote, poi.ID) {
		t.Errorf("the other suggestion after accepting = %+v", sibling)
	}
	if status, _ := service.call(http.MethodPost, "/place-suggestions/"+poi.ID+"/accept", PlaceSuggestionDecision{DecidedBy: "x"}, nil); status != http.StatusConflict {
		t.Errorf("accepting twice = %d, want 409", status)
	}
	// The place is named now: no run suggests for it again.
	if status, _ := service.call(http.MethodPost, "/place-suggestions/proposals", request, nil); status != http.StatusConflict {
		t.Errorf("proposing for a named place = %d, want 409", status)
	}
	if status, _ := service.call(http.MethodPost, "/place-suggestions/"+poi.ID+"/reject", PlaceSuggestionDecision{DecidedBy: "x", Name: "y"}, nil); status != http.StatusBadRequest {
		t.Errorf("reject with a name = %d, want 400", status)
	}
	var proposed []PlaceSuggestion
	service.must(http.MethodGet, "/place-suggestions?status=proposed", nil, &proposed, http.StatusOK)
	if len(proposed) != 0 {
		t.Errorf("proposed after accepting = %+v", proposed)
	}
}
