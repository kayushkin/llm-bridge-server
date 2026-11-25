package googletimeline_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	locationstore "github.com/kayushkin/location-store"
	"github.com/kayushkin/location-store/internal/googletimeline"
	"github.com/kayushkin/location-store/internal/importclient"
)

// Every file here is invented: the shapes are Google's, the places and times
// are made up, on a grid near 10°N 20°E.

const onDeviceTimeline = `{
  "semanticSegments": [
    {"startTime": "2024-03-01T08:00:00.000+01:00", "endTime": "2024-03-01T09:00:00.000+01:00",
     "startTimeTimezoneUtcOffsetMinutes": 60, "endTimeTimezoneUtcOffsetMinutes": 60,
     "visit": {"hierarchyLevel": 0, "probability": 0.95,
       "topCandidate": {"placeId": "example-home", "semanticType": "HOME", "probability": 0.9,
         "placeLocation": {"latLng": "10.0010000°, 20.0010000°"}}}},
    {"startTime": "2024-03-01T08:10:00.000+01:00", "endTime": "2024-03-01T08:20:00.000+01:00",
     "visit": {"hierarchyLevel": 1, "probability": 0.5,
       "topCandidate": {"placeId": "example-inner", "semanticType": "UNKNOWN", "probability": 0.4,
         "placeLocation": {"latLng": "10.0011°, 20.0011°"}}}},
    {"startTime": "2024-03-01T09:00:00.000+01:00", "endTime": "2024-03-01T09:30:00.000+01:00",
     "activity": {"start": {"latLng": "10.0010000°, 20.0010000°"}, "end": {"latLng": "10.0500000°, 20.0500000°"},
       "distanceMeters": 7123.5, "probability": 0.99,
       "topCandidate": {"type": "IN_PASSENGER_VEHICLE", "probability": 0.87}}},
    {"startTime": "2024-03-01T09:00:00.000+01:00", "endTime": "2024-03-01T11:00:00.000+01:00",
     "timelinePath": [
       {"point": "10.0100000°, 20.0100000°", "time": "2024-03-01T09:10:00.000+01:00"},
       {"point": "10.0300000°, 20.0300000°", "time": "2024-03-01T09:20:00.000+01:00"},
       {"point": "10.0600000°, 20.0600000°", "time": "2024-03-01T10:15:00.000+01:00"}]},
    {"startTime": "2024-03-01T09:30:00.000+01:00", "endTime": "2024-03-01T12:00:00.000+01:00",
     "visit": {"hierarchyLevel": 0, "probability": 0.7,
       "topCandidate": {"placeId": "example-cafe", "semanticType": "UNKNOWN", "probability": 0.6,
         "placeLocation": {"latLng": "10.0500000°, 20.0500000°"}}}},
    {"startTime": "2024-03-01T00:00:00.000+01:00", "endTime": "2024-03-02T00:00:00.000+01:00",
     "timelineMemory": {"destinations": []}}
  ],
  "rawSignals": [
    {"position": {"LatLng": "10.0020000°, 20.0020000°", "accuracyMeters": 15, "altitudeMeters": 100.0,
      "source": "WIFI", "timestamp": "2024-03-01T08:30:00.000+01:00", "speedMetersPerSecond": 0.0}},
    {"wifiScan": {"deliveryTime": "2024-03-01T08:31:00.000+01:00", "devicesRecords": []}},
    {"activityRecord": {"probableActivities": [], "timestamp": "2024-03-01T08:32:00.000+01:00"}}
  ],
  "userLocationProfile": {
    "frequentPlaces": [
      {"placeId": "example-cafe", "placeLocation": "10.0500000°, 20.0500000°", "label": "WORK"}],
    "persona": {"travelModeAffinities": []}
  }
}`

const legacyRecords = `{"locations": [
  {"latitudeE7": 100010000, "longitudeE7": 200010000, "accuracy": 20, "source": "WIFI", "timestampMs": "1500000000000"},
  {"latitudeE7": 100020000, "longitudeE7": 200020000, "accuracy": 8, "timestamp": "2019-05-01T10:00:00.123Z",
   "activity": [{"activity": [{"type": "STILL", "confidence": 100}], "timestamp": "2019-05-01T10:00:00Z"}]},
  {"latitudeE7": 4194967296, "longitudeE7": 200030000, "timestamp": "2019-05-01T11:00:00Z"},
  {"longitudeE7": 200040000, "timestamp": "2019-05-01T12:00:00Z"}
]}`

const legacySemantic = `{"timelineObjects": [
  {"placeVisit": {
    "location": {"latitudeE7": 100500000, "longitudeE7": 200500000, "placeId": "example-library",
      "address": "1 Example Road", "name": "Example Library", "semanticType": "TYPE_SEARCHED_ADDRESS",
      "sourceInfo": {"deviceTag": 1}, "locationConfidence": 85.5},
    "duration": {"startTimestamp": "2019-06-01T08:00:00Z", "endTimestamp": "2019-06-01T10:00:00.500Z"},
    "placeConfidence": "HIGH_CONFIDENCE", "centerLatE7": 100501000, "centerLngE7": 200501000, "visitConfidence": 90}},
  {"activitySegment": {
    "startLocation": {"latitudeE7": 100500000, "longitudeE7": 200500000, "placeId": "example-library"},
    "endLocation": {"latitudeE7": 101000000, "longitudeE7": 201000000},
    "duration": {"startTimestampMs": "1559383200000", "endTimestampMs": "1559385000000"},
    "distance": 9000, "activityType": "IN_BUS", "confidence": "HIGH",
    "activities": [{"activityType": "IN_BUS", "probability": 72.5}, {"activityType": "WALKING", "probability": 10.0}],
    "waypointPath": {"waypoints": [{"latE7": 100600000, "lngE7": 200600000}, {"latE7": 100800000, "lngE7": 200800000}],
      "source": "INFERRED", "travelMode": "DRIVE"},
    "simplifiedRawPath": {"points": [{"latE7": 100700000, "lngE7": 200700000, "accuracyMeters": 10, "timestamp": "2019-06-01T10:10:00Z"}]}}},
  {"activitySegment": {
    "startLocation": {"latitudeE7": 101000000, "longitudeE7": 201000000},
    "endLocation": {"latitudeE7": 101100000, "longitudeE7": 201100000},
    "duration": {"startTimestamp": "2019-06-01T10:30:00Z", "endTimestamp": "2019-06-01T10:45:00Z"},
    "activityType": "SKATEBOARDING"}}
]}`

// memorySink keeps what a parse hands it.
type memorySink struct {
	kind   string
	places []locationstore.ImportPlace
	visits []locationstore.ImportVisit
	trips  []locationstore.ImportTrip
	points []locationstore.ImportPoint
}

func (sink *memorySink) Begin(kind string) error { sink.kind = kind; return nil }
func (sink *memorySink) Place(place locationstore.ImportPlace) error {
	sink.places = append(sink.places, place)
	return nil
}
func (sink *memorySink) Visit(visit locationstore.ImportVisit) error {
	sink.visits = append(sink.visits, visit)
	return nil
}
func (sink *memorySink) Trip(trip locationstore.ImportTrip) error {
	sink.trips = append(sink.trips, trip)
	return nil
}
func (sink *memorySink) Point(point locationstore.ImportPoint) error {
	sink.points = append(sink.points, point)
	return nil
}

func TestTheOnDeviceExport(t *testing.T) {
	sink := &memorySink{}
	report, err := googletimeline.Parse(strings.NewReader(onDeviceTimeline), sink)
	if err != nil {
		t.Fatal(err)
	}
	if sink.kind != locationstore.ImportKindGoogleTimeline {
		t.Fatalf("kind = %q", sink.kind)
	}
	if len(sink.visits) != 2 {
		t.Fatalf("visits = %+v, want the two at hierarchy level 0", sink.visits)
	}
	home := sink.visits[0]
	if home.StartedAt != 1709276400 || home.EndedAt != 1709280000 || home.GooglePlaceID != "example-home" ||
		*home.Probability != 0.9 || home.SourceSemanticType != "HOME" || *home.Latitude != 10.001 || home.SourceRef != "1709276400-1709280000" {
		t.Errorf("home visit = %+v", home)
	}
	if len(sink.places) != 3 || sink.places[0].Kind != locationstore.PlaceKindHome || sink.places[1].Kind != locationstore.PlaceKindOther ||
		sink.places[2].Kind != locationstore.PlaceKindWork || sink.places[2].GooglePlaceID != "example-cafe" {
		t.Errorf("places = %+v: home, the cafe as other from its visit, then as work from frequentPlaces", sink.places)
	}
	if len(sink.trips) != 1 {
		t.Fatalf("trips = %+v", sink.trips)
	}
	trip := sink.trips[0]
	if trip.Mode != locationstore.ModeDriving || trip.SourceMode != "IN_PASSENGER_VEHICLE" || *trip.DistanceMeters != 7123.5 ||
		*trip.Probability != 0.87 || *trip.EndLatitude != 10.05 {
		t.Errorf("trip = %+v", trip)
	}
	if len(trip.Path) != 2 || trip.Path[0].Latitude != 10.01 || *trip.Path[1].Timestamp != 1709281200 {
		t.Errorf("trip path = %+v, want the two timelinePath points inside the activity", trip.Path)
	}
	var pathPoints, rawPoints int
	for _, point := range sink.points {
		switch point.Source {
		case locationstore.PointSourceGoogleTimelinePath:
			pathPoints++
		case locationstore.PointSourceGoogleTimelineRaw:
			rawPoints++
			if *point.AccuracyMeters != 15 || point.Timestamp != 1709278200 {
				t.Errorf("raw point = %+v", point)
			}
		}
	}
	if pathPoints != 3 || rawPoints != 1 {
		t.Errorf("points: %d path, %d raw; want 3 and 1", pathPoints, rawPoints)
	}
	if report["raw signals that are not positions (wifi scans, activity records)"] != 2 ||
		report["visits inside another visit (hierarchyLevel above 0) not read"] != 1 ||
		report["timelineMemory segments (trip summaries) not read"] != 1 {
		t.Errorf("report = %v", report)
	}
}

func TestLegacyRecords(t *testing.T) {
	sink := &memorySink{}
	report, err := googletimeline.Parse(strings.NewReader(legacyRecords), sink)
	if err != nil {
		t.Fatal(err)
	}
	if sink.kind != locationstore.ImportKindGoogleRecords || len(sink.points) != 3 {
		t.Fatalf("kind %q, points %+v", sink.kind, sink.points)
	}
	first, second, wrapped := sink.points[0], sink.points[1], sink.points[2]
	if first.Timestamp != 1500000000 || first.Latitude != 10.001 || *first.AccuracyMeters != 20 || first.Source != locationstore.PointSourceGoogleRecords {
		t.Errorf("timestampMs point = %+v", first)
	}
	if second.Timestamp != 1556704800 || second.Longitude != 20.002 {
		t.Errorf("timestamp point = %+v", second)
	}
	if wrapped.Latitude != -10 {
		t.Errorf("a latitude written as an unsigned 32-bit number = %v, want -10", wrapped.Latitude)
	}
	if report["locations with no latitudeE7 or longitudeE7"] != 1 {
		t.Errorf("report = %v", report)
	}
}

func TestLegacySemanticLocationHistory(t *testing.T) {
	sink := &memorySink{}
	report, err := googletimeline.Parse(strings.NewReader(legacySemantic), sink)
	if err != nil {
		t.Fatal(err)
	}
	if sink.kind != locationstore.ImportKindGoogleSemantic {
		t.Fatalf("kind = %q", sink.kind)
	}
	if len(sink.places) != 1 || sink.places[0].Name != "Example Library" || sink.places[0].Address != "1 Example Road" ||
		sink.places[0].Kind != locationstore.PlaceKindSaved || sink.places[0].Latitude != 10.05 {
		t.Errorf("places = %+v", sink.places)
	}
	if len(sink.visits) != 1 {
		t.Fatalf("visits = %+v", sink.visits)
	}
	visit := sink.visits[0]
	if visit.StartedAt != 1559376000 || visit.EndedAt != 1559383200 || *visit.Latitude != 10.0501 || *visit.Probability != 0.855 ||
		visit.GooglePlaceID != "example-library" {
		t.Errorf("visit = %+v: times, the visit's center, confidence as 0..1", visit)
	}
	if len(sink.trips) != 2 {
		t.Fatalf("trips = %+v", sink.trips)
	}
	bus, skate := sink.trips[0], sink.trips[1]
	if bus.Mode != locationstore.ModeBus || *bus.Probability != 0.725 || *bus.DistanceMeters != 9000 || bus.StartGooglePlaceID != "example-library" ||
		bus.StartedAt != 1559383200 || len(bus.Path) != 2 || bus.Path[0].Timestamp != nil {
		t.Errorf("bus trip = %+v: mode, probability, distance, start place, the waypoint path", bus)
	}
	if skate.Mode != locationstore.ModeUnknown || skate.SourceMode != "SKATEBOARDING" || skate.DistanceMeters != nil {
		t.Errorf("an activity type with no mode = %+v", skate)
	}
	if len(sink.points) != 1 || sink.points[0].Source != locationstore.PointSourceGoogleSemanticPath {
		t.Errorf("points = %+v, want the one simplifiedRawPath point", sink.points)
	}
	if len(report) != 0 {
		t.Errorf("report = %v, want nothing left out", report)
	}
}

func TestFilesThatAreNotLocationHistory(t *testing.T) {
	for name, content := range map[string]string{
		"settings file": `{"createdTime": "2020-01-01T00:00:00Z", "deviceSettings": []}`,
		"empty object":  `{}`,
	} {
		if _, err := googletimeline.Parse(strings.NewReader(content), &memorySink{}); !errors.Is(err, googletimeline.ErrNotLocationHistory) {
			t.Errorf("%s = %v, want ErrNotLocationHistory", name, err)
		}
	}
	if _, err := googletimeline.Parse(strings.NewReader(`[{"startTime": "2024-01-01T00:00:00Z"}]`), &memorySink{}); err == nil ||
		!strings.Contains(err.Error(), "iPhone") {
		t.Errorf("an array = %v, want a refusal naming the iPhone export", err)
	}
	if _, err := googletimeline.Parse(strings.NewReader(`{"locations": [{"latitudeE7": 1,`), &memorySink{}); err == nil {
		t.Error("a cut-off file parsed")
	}
}

// The whole path: parse, post in batches to a real store, finish; the same
// file again is a no-op.
func TestAnImportThroughTheStoreAndAgain(t *testing.T) {
	store, err := locationstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mux := http.NewServeMux()
	locationstore.RegisterHandlers(mux, store)
	server := httptest.NewServer(mux)
	defer server.Close()

	const hash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	client := importclient.New(server.URL, hash, "laptop", "exports/Timeline.json", false)
	if _, err := googletimeline.Parse(strings.NewReader(onDeviceTimeline), client); err != nil {
		t.Fatal(err)
	}
	record, err := client.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != "import_000001" || record.Counts.VisitsCreated != 2 || record.Counts.TripsCreated != 1 ||
		record.Counts.PlacesCreated != 2 || record.Counts.PlacesUpdated != 1 || record.Counts.PointsAdded != 4 {
		t.Errorf("import = %+v", record)
	}
	cafe, err := store.ListPlaces(locationstore.PlaceFilter{GooglePlaceID: "example-cafe"})
	if err != nil || len(cafe) != 1 || cafe[0].Kind != locationstore.PlaceKindWork {
		t.Errorf("the cafe = %+v %v, want kind work from frequentPlaces", cafe, err)
	}
	trip, err := store.GetTrip("trip_000001", false)
	if err != nil || trip.PathPoints != 2 {
		t.Errorf("trip = %+v %v", trip, err)
	}

	again := importclient.New(server.URL, hash, "laptop", "exports/Timeline.json", false)
	if err := again.Begin(locationstore.ImportKindGoogleTimeline); !errors.Is(err, importclient.ErrAlreadyImported) {
		t.Errorf("the same file again = %v, want ErrAlreadyImported", err)
	}
}
