package locationstore

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every town, coordinate and place id here is invented: a grid near 10°N
// 20°E, in a made-up country.

func writeTownFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	town := func(id int, name string, latitude, longitude float64, featureCode string, population int) string {
		return fmt.Sprintf("%d\t%s\t%s\t\t%v\t%v\tP\t%s\tXX\t\t01\t\t\t\t%d\t\t300\tAfrica/Ndjamena\t2026-01-01", id, name, name,
			latitude, longitude, featureCode, population)
	}
	cities := strings.Join([]string{
		town(1, "Hometown", 10.0, 20.0, "PPLA", 50000),
		town(2, "Old Quarter", 10.002, 20.002, "PPLX", 9000), // part of Hometown: never named
		town(3, "Suburbia", 10.06, 20.06, "PPL", 2000),
		town(4, "Fartown", 11.5, 20.0, "PPL", 20000),
	}, "\n") + "\n"
	files := map[string]string{
		"cities1000.txt":       cities,
		"admin1CodesASCII.txt": "XX.01\tExample Region\tExample Region\t100\n",
		"countryInfo.txt":      "#ISO\tISO3\tISO-Numeric\tfips\tCountry\n" + "XX\tXXX\t999\tXX\tExampleland\tCapital\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

const journeyBase = int64(1767225600) // 2026-01-01 00:00 UTC

func hours(h float64) int64 { return journeyBase + int64(h*3600) }

// journeyBatch is 20 nights at home, three nights in Fartown (out by car,
// back by car), then 10 more at home.
func journeyBatch() ImportBatch {
	batch := ImportBatch{Places: []ImportPlace{
		{GooglePlaceID: "example-home", Latitude: 10.001, Longitude: 20.001, Kind: PlaceKindOther, Source: SourceGoogleTimeline},
		{GooglePlaceID: "example-hotel", Latitude: 11.501, Longitude: 20.001, Kind: PlaceKindOther, Source: SourceGoogleTimeline},
		{GooglePlaceID: "example-cafe", Latitude: 10.06, Longitude: 20.06, Kind: PlaceKindOther, Source: SourceGoogleTimeline},
	}}
	visit := func(place string, latitude, longitude float64, from, to float64) {
		startedAt, endedAt := hours(from), hours(to)
		batch.Visits = append(batch.Visits, ImportVisit{Source: SourceGoogleTimeline, SourceRef: fmt.Sprintf("%d-%d", startedAt, endedAt),
			StartedAt: startedAt, EndedAt: endedAt, Latitude: float(latitude), Longitude: float(longitude), GooglePlaceID: place})
	}
	trip := func(from, to float64, startLatitude, endLatitude float64) {
		startedAt, endedAt := hours(from), hours(to)
		batch.Trips = append(batch.Trips, ImportTrip{Source: SourceGoogleTimeline, SourceRef: fmt.Sprintf("%d-%d", startedAt, endedAt),
			StartedAt: startedAt, EndedAt: endedAt, StartLatitude: float(startLatitude), StartLongitude: float(20.001),
			EndLatitude: float(endLatitude), EndLongitude: float(20.001), Mode: ModeDriving, SourceMode: "IN_PASSENGER_VEHICLE",
			DistanceMeters: float(170000)})
	}
	// Each day: home overnight until 08:00, out to the cafe and back.
	homeDay := func(day int) {
		d := float64(day * 24)
		visit("example-home", 10.001, 20.001, d-6, d+8)
		trip(d+8, d+8.5, 10.001, 10.06)
		visit("example-cafe", 10.06, 20.06, d+8.5, d+17.5)
		trip(d+17.5, d+18, 10.06, 10.001)
	}
	for day := 0; day < 20; day++ {
		homeDay(day)
	}
	// Day 20: drive to Fartown, sleep three nights at the hotel, drive back on
	// day 23.
	visit("example-home", 10.001, 20.001, 20*24-6, 20*24+8)
	trip(20*24+8, 20*24+11, 10.001, 11.501)
	visit("example-hotel", 11.501, 20.001, 20*24+11, 23*24+9)
	trip(23*24+9, 23*24+12, 11.501, 10.001)
	visit("example-home", 10.001, 20.001, 23*24+12, 23*24+18)
	for day := 24; day < 34; day++ {
		homeDay(day)
	}
	return batch
}

func newJourneyService(t *testing.T) *testService {
	t.Helper()
	service := newTestService(t)
	writeTownFixture(t, service.store.GeonamesDir())
	service.startImport(ImportKindGoogleTimeline, hashA)
	service.must(http.MethodPost, "/imports/import_000001/batches", journeyBatch(), nil, http.StatusOK)
	service.must(http.MethodPost, "/imports/import_000001/finish", nil, nil, http.StatusOK)
	return service
}

func TestLocalitiesNameTheTownAndNeverReplaceOne(t *testing.T) {
	service := newJourneyService(t)
	var fill LocalityFill
	service.must(http.MethodPost, "/localities/fill", LocalityFillRequest{DryRun: true}, &fill, http.StatusOK)
	var place Place
	service.must(http.MethodGet, "/places/place_000001", nil, &place, http.StatusOK)
	if place.Locality != nil || fill.Places != 3 {
		t.Fatalf("dry run: fill = %+v, place locality = %+v; want counts and nothing written", fill, place.Locality)
	}
	service.must(http.MethodPost, "/localities/fill", LocalityFillRequest{}, &fill, http.StatusOK)
	service.must(http.MethodGet, "/places/place_000001", nil, &place, http.StatusOK)
	if place.Locality == nil || place.Locality.City != "Hometown" || place.Locality.Region != "Example Region" ||
		place.Locality.Country != "Exampleland" || place.Locality.TimeZone != "Africa/Ndjamena" ||
		!strings.HasPrefix(place.Locality.DescribedBy, SetByLocalityNaming) {
		t.Errorf("home's locality = %+v: the part of town (PPLX) is never named; Hometown is", place.Locality)
	}
	var cafe Place
	service.must(http.MethodGet, "/places/place_000003", nil, &cafe, http.StatusOK)
	if cafe.Locality == nil || cafe.Locality.City != "Suburbia" {
		t.Errorf("cafe's locality = %+v, want Suburbia: Hometown does not reach that far", cafe.Locality)
	}
	var trip Trip
	service.must(http.MethodGet, "/trips/trip_000001", nil, &trip, http.StatusOK)
	if trip.StartLocality == nil || trip.EndLocality == nil || trip.EndLocality.City != "Suburbia" {
		t.Errorf("trip localities = %+v → %+v", trip.StartLocality, trip.EndLocality)
	}
	service.must(http.MethodPost, "/localities/fill", LocalityFillRequest{}, &fill, http.StatusOK)
	if fill.Places+fill.Visits+fill.TripStarts+fill.TripEnds != 0 || fill.AlreadyNamed == 0 {
		t.Errorf("second fill = %+v, want nothing filled", fill)
	}
}

func TestLocalitiesAskForTheTownFilesWhenMissing(t *testing.T) {
	service := newTestService(t)
	if status, body := service.call(http.MethodPost, "/localities/fill", LocalityFillRequest{}, nil); status != http.StatusConflict ||
		!strings.Contains(string(body), "location-name-localities") {
		t.Errorf("fill with no GeoNames files = %d %s, want 409 naming the command", status, body)
	}
}

func TestJourneysGroupTheSpanAwayAndARerunChangesNothing(t *testing.T) {
	service := newJourneyService(t)
	service.must(http.MethodPost, "/localities/fill", LocalityFillRequest{}, nil, http.StatusOK)

	var dry JourneyGrouping
	service.must(http.MethodPost, "/journeys/group", JourneyGroupRequest{DryRun: true}, &dry, http.StatusOK)
	if len(dry.Journeys) != 1 || dry.Journeys[0].Action != JourneyCreate || dry.Journeys[0].TripID != "" {
		t.Fatalf("dry run = %+v", dry)
	}
	var journeys []Trip
	service.must(http.MethodGet, "/journeys", nil, &journeys, http.StatusOK)
	if len(journeys) != 0 {
		t.Fatalf("a dry run wrote %d journeys", len(journeys))
	}

	var grouping JourneyGrouping
	service.must(http.MethodPost, "/journeys/group", JourneyGroupRequest{}, &grouping, http.StatusOK)
	if len(grouping.HomePeriods) != 1 || grouping.HomePeriods[0].PlaceID == nil || *grouping.HomePeriods[0].PlaceID != "place_000001" ||
		grouping.HomePeriods[0].EndedAt != 0 {
		t.Fatalf("home periods = %+v, want one, open, at the home", grouping.HomePeriods)
	}
	if len(grouping.HomePlacesMarked) != 1 || grouping.HomePlacesMarked[0] != "place_000001" {
		t.Errorf("home places marked = %v", grouping.HomePlacesMarked)
	}
	if len(grouping.Journeys) != 1 {
		t.Fatalf("journeys = %+v, want one", grouping.Journeys)
	}
	journey := grouping.Journeys[0]
	zone, _ := time.LoadLocation("Africa/Ndjamena")
	wantTitle := "Hometown → Fartown, Example Region, " + DateSpanLabel(hours(20*24+8), hours(23*24+12), zone)
	if journey.Title != wantTitle || journey.Nights != 3 || len(journey.ChildTripIDs) != 2 || journey.Mode != ModeDriving ||
		journey.StartedAt != hours(20*24+8) || journey.EndedAt != hours(23*24+12) {
		t.Fatalf("journey = %+v, want %q, 3 nights, the two drives", journey, wantTitle)
	}
	var summary JourneySummary
	service.must(http.MethodGet, "/journeys/"+journey.TripID, nil, &summary, http.StatusOK)
	if summary.Trip.Kind != TripKindJourney || summary.Trip.Source != SourceGenerated || summary.Trip.TitleBy != SetByJourneyGrouping ||
		summary.Trip.ChildTrips != 2 || len(summary.Children) != 2 || len(summary.Localities) != 1 || summary.Localities[0].Locality.City != "Fartown" {
		t.Errorf("summary = %+v", summary)
	}
	service.must(http.MethodGet, "/trips?kind=journey", nil, &journeys, http.StatusOK)
	if len(journeys) != 1 || journeys[0].ID != journey.TripID {
		t.Errorf("trips?kind=journey = %+v", journeys)
	}
	var periods []HomePeriod
	service.must(http.MethodGet, "/home-periods", nil, &periods, http.StatusOK)
	if len(periods) != 1 || periods[0].Locality == nil || periods[0].Locality.City != "Hometown" {
		t.Errorf("home periods = %+v", periods)
	}

	service.must(http.MethodPost, "/journeys/group", JourneyGroupRequest{}, &grouping, http.StatusOK)
	if len(grouping.Journeys) != 1 || grouping.Journeys[0].Action != JourneyUnchanged || grouping.Journeys[0].TripID != journey.TripID ||
		len(grouping.HomePeriods) != 1 || grouping.HomePeriods[0].ID != periods[0].ID {
		t.Errorf("a re-run = %+v, want everything unchanged", grouping)
	}

	// A person renames the journey and takes the drive home out of it: a
	// re-run keeps both.
	child := journey.ChildTripIDs[1]
	service.must(http.MethodPatch, "/trips/"+journey.TripID, map[string]any{"title": "Hotel weekend"}, nil, http.StatusOK)
	service.must(http.MethodPatch, "/trips/"+child, map[string]any{"parent_trip_id": nil}, nil, http.StatusOK)
	service.must(http.MethodPost, "/journeys/group", JourneyGroupRequest{}, &grouping, http.StatusOK)
	var trip Trip
	service.must(http.MethodGet, "/trips/"+journey.TripID, nil, &trip, http.StatusOK)
	if trip.Title != "Hotel weekend" || trip.ChildTrips != 1 || grouping.Journeys[0].ChildrenKeptElsewhere != 1 {
		t.Errorf("after hand edits: journey = %+v, grouping = %+v", trip, grouping.Journeys[0])
	}
}

func TestJourneysLeaveAJourneyMadeByHandAlone(t *testing.T) {
	service := newJourneyService(t)
	var made Trip
	service.must(http.MethodPost, "/trips", TripCreate{Title: "Our trip", StartedAt: hours(20 * 24), EndedAt: hours(24 * 24), Mode: ModeDriving},
		&made, http.StatusCreated)
	if made.Kind != TripKindJourney || made.TitleBy != SetByHand {
		t.Fatalf("a trip made by hand = %+v, want kind journey, title by hand", made)
	}
	var grouping JourneyGrouping
	service.must(http.MethodPost, "/journeys/group", JourneyGroupRequest{}, &grouping, http.StatusOK)
	if len(grouping.Journeys) != 1 || grouping.Journeys[0].Action != JourneySkip || grouping.Journeys[0].TripID != made.ID {
		t.Errorf("grouping = %+v, want the journey skipped for the one made by hand", grouping.Journeys)
	}
	var trips []Trip
	service.must(http.MethodGet, "/trips?parent_trip_id="+made.ID, nil, &trips, http.StatusOK)
	if len(trips) != 0 {
		t.Errorf("the command attached %d trips to a journey made by hand", len(trips))
	}
}

func TestAHomeThatChangesForWeeksStartsANewHomePeriod(t *testing.T) {
	service := newTestService(t)
	writeTownFixture(t, service.store.GeonamesDir())
	batch := ImportBatch{Places: []ImportPlace{
		{GooglePlaceID: "example-home", Latitude: 10.001, Longitude: 20.001, Kind: PlaceKindOther, Source: SourceGoogleTimeline},
		{GooglePlaceID: "example-new-home", Latitude: 10.03, Longitude: 20.03, Kind: PlaceKindOther, Source: SourceGoogleTimeline},
	}}
	for day := 0; day < 70; day++ {
		place, latitude := "example-home", 10.001
		if day >= 30 {
			place, latitude = "example-new-home", 10.03
		}
		startedAt, endedAt := hours(float64(day*24-6)), hours(float64(day*24+8))
		batch.Visits = append(batch.Visits, ImportVisit{Source: SourceGoogleTimeline, SourceRef: fmt.Sprintf("%d-%d", startedAt, endedAt),
			StartedAt: startedAt, EndedAt: endedAt, Latitude: float(latitude), Longitude: float(latitude + 10), GooglePlaceID: place})
	}
	service.startImport(ImportKindGoogleTimeline, hashA)
	service.must(http.MethodPost, "/imports/import_000001/batches", batch, nil, http.StatusOK)
	var grouping JourneyGrouping
	service.must(http.MethodPost, "/journeys/group", JourneyGroupRequest{}, &grouping, http.StatusOK)
	periods := grouping.HomePeriods
	if len(periods) != 2 || *periods[0].PlaceID != "place_000001" || *periods[1].PlaceID != "place_000002" ||
		periods[0].EndedAt != periods[1].StartedAt || periods[1].EndedAt != 0 {
		t.Fatalf("home periods = %+v, want the first home then the new one", periods)
	}
	// The move falls on the first night at the new home, within a day.
	if moved := periods[1].StartedAt; moved < hours(29*24) || moved > hours(30*24) {
		t.Errorf("the new home starts at %d, want during day 29", moved)
	}
}
