// Package googletimeline reads Google's location history exports and hands
// what it finds to a Sink as location-store import records. It reads three
// formats, told apart by their first known top-level key:
//
//   - the on-device Timeline export (Timeline.json, 2024 on): semanticSegments
//     (visits, activities and timelinePath), rawSignals (positions) and
//     userLocationProfile (frequentPlaces);
//   - legacy Takeout Records.json: locations[] with latitudeE7, longitudeE7,
//     timestamp or timestampMs, accuracy;
//   - legacy Takeout Semantic Location History month files: timelineObjects[]
//     of placeVisit and activitySegment.
//
// The arrays that can hold millions of entries (rawSignals, locations,
// timelineObjects) are read one entry at a time. semanticSegments is held in
// memory, because a trip's path is in timelinePath segments of its own.
package googletimeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	locationstore "github.com/kayushkin/location-store"
)

// Sink receives what a file holds. Begin is called once, with the file's
// import kind, before any record.
type Sink interface {
	Begin(kind string) error
	Place(locationstore.ImportPlace) error
	Visit(locationstore.ImportVisit) error
	Trip(locationstore.ImportTrip) error
	Point(locationstore.ImportPoint) error
}

// SnapshotSink is a Sink told, once the whole file is read, the span an
// on-device Timeline export covers and how new it is. Such an export holds
// every segment of its span, so it is a full snapshot of it.
type SnapshotSink interface {
	Snapshot(locationstore.ImportSnapshot) error
}

// ErrNotLocationHistory is a JSON file none of whose top-level keys is one of
// the three formats'.
var ErrNotLocationHistory = errors.New("not a Google location history file: no semanticSegments, rawSignals, userLocationProfile, locations or timelineObjects")

// ErrStop, returned by a Sink, ends the read early with no error: the store
// already has this file.
var ErrStop = errors.New("stop reading")

// Report counts what was left out, by reason, so nothing is dropped unseen.
type Report map[string]int

// Parse reads one export file.
func Parse(reader io.Reader, sink Sink) (Report, error) {
	report := Report{}
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return report, fmt.Errorf("read the first token: %w", err)
	}
	if token == json.Delim('[') {
		return report, errors.New("the file is a JSON array: that is the iPhone Timeline export, which this importer does not read")
	}
	if token != json.Delim('{') {
		return report, ErrNotLocationHistory
	}
	parser := &parser{sink: sink, report: report}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return report, err
		}
		key, _ := keyToken.(string)
		if err := parser.readKey(decoder, key); err != nil {
			if errors.Is(err, ErrStop) {
				return report, nil
			}
			return report, fmt.Errorf("%s: %w", key, err)
		}
	}
	if parser.kind == "" {
		return report, ErrNotLocationHistory
	}
	if snapshotSink, wants := sink.(SnapshotSink); wants && parser.kind == locationstore.ImportKindGoogleTimeline && parser.snapshot.CoversFrom != 0 {
		if err := snapshotSink.Snapshot(parser.snapshot); err != nil {
			return report, err
		}
	}
	return report, nil
}

type parser struct {
	sink   Sink
	report Report
	kind   string
	// snapshot is what an on-device export covers, gathered as it is read.
	snapshot locationstore.ImportSnapshot
}

// begin sets the file's kind the first time a known key is met.
func (p *parser) begin(kind string) error {
	if p.kind == "" {
		p.kind = kind
		return p.sink.Begin(kind)
	}
	if p.kind != kind {
		return fmt.Errorf("the file mixes formats: %s and %s", p.kind, kind)
	}
	return nil
}

func (p *parser) readKey(decoder *json.Decoder, key string) error {
	switch key {
	case "semanticSegments":
		if err := p.begin(locationstore.ImportKindGoogleTimeline); err != nil {
			return err
		}
		var segments []deviceSegment
		if err := eachElement(decoder, func() error {
			var segment deviceSegment
			if err := decoder.Decode(&segment); err != nil {
				return err
			}
			segments = append(segments, segment)
			return nil
		}); err != nil {
			return err
		}
		return p.deviceSegments(segments)
	case "rawSignals":
		if err := p.begin(locationstore.ImportKindGoogleTimeline); err != nil {
			return err
		}
		return eachElement(decoder, func() error {
			var signal deviceRawSignal
			if err := decoder.Decode(&signal); err != nil {
				return err
			}
			return p.deviceRawSignal(signal)
		})
	case "userLocationProfile":
		if err := p.begin(locationstore.ImportKindGoogleTimeline); err != nil {
			return err
		}
		var profile deviceProfile
		if err := decoder.Decode(&profile); err != nil {
			return err
		}
		return p.deviceProfile(profile)
	case "locations":
		if err := p.begin(locationstore.ImportKindGoogleRecords); err != nil {
			return err
		}
		return eachElement(decoder, func() error {
			var record legacyRecord
			if err := decoder.Decode(&record); err != nil {
				return err
			}
			return p.legacyRecord(record)
		})
	case "timelineObjects":
		if err := p.begin(locationstore.ImportKindGoogleSemantic); err != nil {
			return err
		}
		return eachElement(decoder, func() error {
			var object legacyTimelineObject
			if err := decoder.Decode(&object); err != nil {
				return err
			}
			return p.legacyTimelineObject(object)
		})
	default:
		var skipped json.RawMessage
		if err := decoder.Decode(&skipped); err != nil {
			return err
		}
		p.report["top-level key "+strconv.Quote(key)+" not read"]++
		return nil
	}
}

// eachElement reads a JSON array one element at a time.
func eachElement(decoder *json.Decoder, read func() error) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return nil
	}
	if token != json.Delim('[') {
		return fmt.Errorf("expected an array, found %v", token)
	}
	for decoder.More() {
		if err := read(); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// ---- values ----

// parseLatLng reads the on-device export's coordinates: "47.6°, -122.3°",
// "geo:47.6,-122.3" or "47.6, -122.3".
func parseLatLng(text string) (float64, float64, bool) {
	text = strings.TrimPrefix(strings.TrimSpace(text), "geo:")
	text = strings.ReplaceAll(text, "°", "")
	latitudeText, longitudeText, found := strings.Cut(text, ",")
	if !found {
		return 0, 0, false
	}
	latitude, errLatitude := strconv.ParseFloat(strings.TrimSpace(latitudeText), 64)
	longitude, errLongitude := strconv.ParseFloat(strings.TrimSpace(longitudeText), 64)
	if errLatitude != nil || errLongitude != nil || math.Abs(latitude) > 90 || math.Abs(longitude) > 180 {
		return 0, 0, false
	}
	return latitude, longitude, true
}

// parseTime reads an RFC 3339 time, with or without fractions of a second.
func parseTime(text string) (int64, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text))
	if err != nil {
		return 0, false
	}
	return parsed.Unix(), true
}

// parseLegacyTime reads legacy Takeout's timestamp (RFC 3339) or, in older
// files, timestampMs (epoch milliseconds as a string).
func parseLegacyTime(timestamp, timestampMs string) (int64, bool) {
	if timestamp != "" {
		return parseTime(timestamp)
	}
	if timestampMs != "" {
		milliseconds, err := strconv.ParseInt(strings.TrimSpace(timestampMs), 10, 64)
		if err != nil || milliseconds <= 0 {
			return 0, false
		}
		return milliseconds / 1000, true
	}
	return 0, false
}

// fromE7 turns legacy Takeout's degrees times 10^7 into degrees. Some exports
// wrote negative values as unsigned 32-bit numbers; those are turned back.
func fromE7(latitudeE7, longitudeE7 int64) (float64, float64, bool) {
	if latitudeE7 > 900000000 {
		latitudeE7 -= 4294967296
	}
	if longitudeE7 > 1800000000 {
		longitudeE7 -= 4294967296
	}
	latitude, longitude := float64(latitudeE7)/1e7, float64(longitudeE7)/1e7
	if math.Abs(latitude) > 90 || math.Abs(longitude) > 180 {
		return 0, 0, false
	}
	return latitude, longitude, true
}

func pointer[T any](value T) *T { return &value }

// probability turns a number into 0..1; percent says it is 0..100.
func probability(value *float64, percent bool) *float64 {
	if value == nil {
		return nil
	}
	result := *value
	if percent {
		result /= 100
	}
	if result < 0 || result > 1 {
		return nil
	}
	return &result
}

func sourceRef(startedAt, endedAt int64) string { return fmt.Sprintf("%d-%d", startedAt, endedAt) }

// ---- on-device export ----

type deviceLatLng struct {
	LatLng string `json:"latLng"`
}

type deviceSegment struct {
	StartTime string `json:"startTime"`
	EndTime   string `json:"endTime"`
	Visit     *struct {
		HierarchyLevel int      `json:"hierarchyLevel"`
		Probability    *float64 `json:"probability"`
		TopCandidate   struct {
			PlaceID       string       `json:"placeId"`
			SemanticType  string       `json:"semanticType"`
			Probability   *float64     `json:"probability"`
			PlaceLocation deviceLatLng `json:"placeLocation"`
		} `json:"topCandidate"`
	} `json:"visit"`
	Activity *struct {
		Start          deviceLatLng `json:"start"`
		End            deviceLatLng `json:"end"`
		DistanceMeters *float64     `json:"distanceMeters"`
		Probability    *float64     `json:"probability"`
		TopCandidate   struct {
			Type        string   `json:"type"`
			Probability *float64 `json:"probability"`
		} `json:"topCandidate"`
	} `json:"activity"`
	TimelinePath []struct {
		Point string `json:"point"`
		Time  string `json:"time"`
	} `json:"timelinePath"`
	TimelineMemory json.RawMessage `json:"timelineMemory"`
}

type timedPoint struct {
	timestamp           int64
	latitude, longitude float64
}

func (p *parser) deviceSegments(segments []deviceSegment) error {
	// Every timelinePath point, in time order: each is a raw point, and those
	// inside an activity's time are its path.
	var path []timedPoint
	for _, segment := range segments {
		for _, point := range segment.TimelinePath {
			timestamp, okTime := parseTime(point.Time)
			latitude, longitude, okPlace := parseLatLng(point.Point)
			if !okTime || !okPlace {
				p.report["timelinePath points with no readable time or place"]++
				continue
			}
			path = append(path, timedPoint{timestamp, latitude, longitude})
		}
	}
	sort.SliceStable(path, func(a, b int) bool { return path[a].timestamp < path[b].timestamp })
	for _, point := range path {
		if err := p.sink.Point(locationstore.ImportPoint{Timestamp: point.timestamp, Latitude: point.latitude, Longitude: point.longitude,
			Source: locationstore.PointSourceGoogleTimelinePath}); err != nil {
			return err
		}
	}
	for _, segment := range segments {
		if startedAt, ok := parseTime(segment.StartTime); ok && (p.snapshot.CoversFrom == 0 || startedAt < p.snapshot.CoversFrom) {
			p.snapshot.CoversFrom = startedAt
		}
		if endedAt, ok := parseTime(segment.EndTime); ok && endedAt > p.snapshot.CoversTo {
			p.snapshot.CoversTo = endedAt
		}
	}
	for _, segment := range segments {
		switch {
		case segment.Visit != nil:
			if err := p.deviceVisit(segment); err != nil {
				return err
			}
		case segment.Activity != nil:
			if err := p.deviceActivity(segment, path); err != nil {
				return err
			}
		case segment.TimelinePath != nil:
		case segment.TimelineMemory != nil:
			p.report["timelineMemory segments (trip summaries) not read"]++
		default:
			p.report["segments of an unknown kind"]++
		}
	}
	return nil
}

func (p *parser) deviceVisit(segment deviceSegment) error {
	visit := segment.Visit
	if visit.HierarchyLevel != 0 {
		p.report["visits inside another visit (hierarchyLevel above 0) not read"]++
		return nil
	}
	startedAt, okStart := parseTime(segment.StartTime)
	endedAt, okEnd := parseTime(segment.EndTime)
	if !okStart || !okEnd || endedAt < startedAt {
		p.report["visits with no readable time"]++
		return nil
	}
	candidate := visit.TopCandidate
	record := locationstore.ImportVisit{Source: locationstore.SourceGoogleTimeline, SourceRef: sourceRef(startedAt, endedAt),
		StartedAt: startedAt, EndedAt: endedAt, Probability: probability(candidate.Probability, false),
		SourceSemanticType: candidate.SemanticType}
	if latitude, longitude, ok := parseLatLng(candidate.PlaceLocation.LatLng); ok {
		record.Latitude, record.Longitude = pointer(latitude), pointer(longitude)
		if candidate.PlaceID != "" {
			if err := p.sink.Place(locationstore.ImportPlace{GooglePlaceID: candidate.PlaceID, Latitude: latitude, Longitude: longitude,
				Kind: locationstore.PlaceKindForGoogleSemanticType(candidate.SemanticType, false), Source: locationstore.SourceGoogleTimeline}); err != nil {
				return err
			}
			record.GooglePlaceID = candidate.PlaceID
		}
	}
	return p.sink.Visit(record)
}

func (p *parser) deviceActivity(segment deviceSegment, path []timedPoint) error {
	activity := segment.Activity
	startedAt, okStart := parseTime(segment.StartTime)
	endedAt, okEnd := parseTime(segment.EndTime)
	if !okStart || !okEnd || endedAt < startedAt {
		p.report["activities with no readable time"]++
		return nil
	}
	trip := locationstore.ImportTrip{Source: locationstore.SourceGoogleTimeline, SourceRef: sourceRef(startedAt, endedAt),
		StartedAt: startedAt, EndedAt: endedAt, Mode: locationstore.ModeForGoogleActivityType(activity.TopCandidate.Type),
		SourceMode: activity.TopCandidate.Type, DistanceMeters: activity.DistanceMeters,
		Probability: probability(activity.TopCandidate.Probability, false)}
	if latitude, longitude, ok := parseLatLng(activity.Start.LatLng); ok {
		trip.StartLatitude, trip.StartLongitude = pointer(latitude), pointer(longitude)
	}
	if latitude, longitude, ok := parseLatLng(activity.End.LatLng); ok {
		trip.EndLatitude, trip.EndLongitude = pointer(latitude), pointer(longitude)
	}
	first := sort.Search(len(path), func(index int) bool { return path[index].timestamp >= startedAt })
	for index := first; index < len(path) && path[index].timestamp <= endedAt; index++ {
		trip.Path = append(trip.Path, locationstore.PathPoint{Latitude: path[index].latitude, Longitude: path[index].longitude,
			Timestamp: pointer(path[index].timestamp)})
	}
	return p.sink.Trip(trip)
}

type deviceRawSignal struct {
	Position *struct {
		LatLngUpper    string   `json:"LatLng"`
		LatLng         string   `json:"latLng"`
		AccuracyMeters *float64 `json:"accuracyMeters"`
		Timestamp      string   `json:"timestamp"`
	} `json:"position"`
}

func (p *parser) deviceRawSignal(signal deviceRawSignal) error {
	if signal.Position == nil {
		p.report["raw signals that are not positions (wifi scans, activity records)"]++
		return nil
	}
	text := signal.Position.LatLngUpper
	if text == "" {
		text = signal.Position.LatLng
	}
	latitude, longitude, okPlace := parseLatLng(text)
	timestamp, okTime := parseTime(signal.Position.Timestamp)
	if !okPlace || !okTime {
		p.report["positions with no readable time or place"]++
		return nil
	}
	if p.snapshot.RawSignalsOldestAt == 0 || timestamp < p.snapshot.RawSignalsOldestAt {
		p.snapshot.RawSignalsOldestAt = timestamp
	}
	p.snapshot.RawSignalsNewestAt = max(p.snapshot.RawSignalsNewestAt, timestamp)
	accuracy := signal.Position.AccuracyMeters
	if accuracy != nil && *accuracy < 0 {
		accuracy = nil
	}
	return p.sink.Point(locationstore.ImportPoint{Timestamp: timestamp, Latitude: latitude, Longitude: longitude,
		AccuracyMeters: accuracy, Source: locationstore.PointSourceGoogleTimelineRaw})
}

type deviceProfile struct {
	FrequentPlaces []struct {
		PlaceID       string `json:"placeId"`
		PlaceLocation string `json:"placeLocation"`
		Label         string `json:"label"`
	} `json:"frequentPlaces"`
}

func (p *parser) deviceProfile(profile deviceProfile) error {
	for _, frequent := range profile.FrequentPlaces {
		latitude, longitude, ok := parseLatLng(frequent.PlaceLocation)
		if frequent.PlaceID == "" || !ok {
			p.report["frequent places with no place id or location"]++
			continue
		}
		if err := p.sink.Place(locationstore.ImportPlace{GooglePlaceID: frequent.PlaceID, Latitude: latitude, Longitude: longitude,
			Kind: locationstore.PlaceKindForGoogleSemanticType(frequent.Label, false), Source: locationstore.SourceGoogleTimeline}); err != nil {
			return err
		}
	}
	return nil
}

// ---- legacy Records.json ----

type legacyRecord struct {
	LatitudeE7  *int64   `json:"latitudeE7"`
	LongitudeE7 *int64   `json:"longitudeE7"`
	Accuracy    *float64 `json:"accuracy"`
	Timestamp   string   `json:"timestamp"`
	TimestampMs string   `json:"timestampMs"`
}

func (p *parser) legacyRecord(record legacyRecord) error {
	if record.LatitudeE7 == nil || record.LongitudeE7 == nil {
		p.report["locations with no latitudeE7 or longitudeE7"]++
		return nil
	}
	latitude, longitude, okPlace := fromE7(*record.LatitudeE7, *record.LongitudeE7)
	timestamp, okTime := parseLegacyTime(record.Timestamp, record.TimestampMs)
	if !okPlace || !okTime {
		p.report["locations with no readable time or place"]++
		return nil
	}
	accuracy := record.Accuracy
	if accuracy != nil && *accuracy < 0 {
		accuracy = nil
	}
	return p.sink.Point(locationstore.ImportPoint{Timestamp: timestamp, Latitude: latitude, Longitude: longitude,
		AccuracyMeters: accuracy, Source: locationstore.PointSourceGoogleRecords})
}

// ---- legacy Semantic Location History ----

type legacyLocation struct {
	LatitudeE7         *int64   `json:"latitudeE7"`
	LongitudeE7        *int64   `json:"longitudeE7"`
	PlaceID            string   `json:"placeId"`
	Address            string   `json:"address"`
	Name               string   `json:"name"`
	SemanticType       string   `json:"semanticType"`
	LocationConfidence *float64 `json:"locationConfidence"`
}

func (location legacyLocation) coordinates() (*float64, *float64) {
	if location.LatitudeE7 == nil || location.LongitudeE7 == nil {
		return nil, nil
	}
	latitude, longitude, ok := fromE7(*location.LatitudeE7, *location.LongitudeE7)
	if !ok {
		return nil, nil
	}
	return &latitude, &longitude
}

type legacyDuration struct {
	StartTimestamp   string `json:"startTimestamp"`
	EndTimestamp     string `json:"endTimestamp"`
	StartTimestampMs string `json:"startTimestampMs"`
	EndTimestampMs   string `json:"endTimestampMs"`
}

func (duration legacyDuration) times() (int64, int64, bool) {
	startedAt, okStart := parseLegacyTime(duration.StartTimestamp, duration.StartTimestampMs)
	endedAt, okEnd := parseLegacyTime(duration.EndTimestamp, duration.EndTimestampMs)
	return startedAt, endedAt, okStart && okEnd && endedAt >= startedAt
}

type legacyRawPath struct {
	Points []struct {
		LatE7          *int64   `json:"latE7"`
		LngE7          *int64   `json:"lngE7"`
		AccuracyMeters *float64 `json:"accuracyMeters"`
		Timestamp      string   `json:"timestamp"`
		TimestampMs    string   `json:"timestampMs"`
	} `json:"points"`
}

type legacyTimelineObject struct {
	PlaceVisit *struct {
		Location          legacyLocation `json:"location"`
		Duration          legacyDuration `json:"duration"`
		CenterLatE7       *int64         `json:"centerLatE7"`
		CenterLngE7       *int64         `json:"centerLngE7"`
		SimplifiedRawPath *legacyRawPath `json:"simplifiedRawPath"`
	} `json:"placeVisit"`
	ActivitySegment *struct {
		StartLocation legacyLocation `json:"startLocation"`
		EndLocation   legacyLocation `json:"endLocation"`
		Duration      legacyDuration `json:"duration"`
		Distance      *float64       `json:"distance"`
		ActivityType  string         `json:"activityType"`
		Activities    []struct {
			ActivityType string   `json:"activityType"`
			Probability  *float64 `json:"probability"`
		} `json:"activities"`
		WaypointPath *struct {
			Waypoints []struct {
				LatE7 *int64 `json:"latE7"`
				LngE7 *int64 `json:"lngE7"`
			} `json:"waypoints"`
		} `json:"waypointPath"`
		SimplifiedRawPath *legacyRawPath `json:"simplifiedRawPath"`
	} `json:"activitySegment"`
}

// legacyRawPathPoints sends a simplifiedRawPath's points as raw points and
// answers them as a path.
func (p *parser) legacyRawPathPoints(raw *legacyRawPath) ([]locationstore.PathPoint, error) {
	if raw == nil {
		return nil, nil
	}
	var path []locationstore.PathPoint
	for _, point := range raw.Points {
		if point.LatE7 == nil || point.LngE7 == nil {
			p.report["simplifiedRawPath points with no place"]++
			continue
		}
		latitude, longitude, okPlace := fromE7(*point.LatE7, *point.LngE7)
		timestamp, okTime := parseLegacyTime(point.Timestamp, point.TimestampMs)
		if !okPlace || !okTime {
			p.report["simplifiedRawPath points with no readable time or place"]++
			continue
		}
		accuracy := point.AccuracyMeters
		if accuracy != nil && *accuracy < 0 {
			accuracy = nil
		}
		if err := p.sink.Point(locationstore.ImportPoint{Timestamp: timestamp, Latitude: latitude, Longitude: longitude,
			AccuracyMeters: accuracy, Source: locationstore.PointSourceGoogleSemanticPath}); err != nil {
			return nil, err
		}
		path = append(path, locationstore.PathPoint{Latitude: latitude, Longitude: longitude, Timestamp: pointer(timestamp)})
	}
	return path, nil
}

func (p *parser) legacyTimelineObject(object legacyTimelineObject) error {
	switch {
	case object.PlaceVisit != nil:
		return p.legacyPlaceVisit(object)
	case object.ActivitySegment != nil:
		return p.legacyActivitySegment(object)
	default:
		p.report["timelineObjects that are neither placeVisit nor activitySegment"]++
		return nil
	}
}

func (p *parser) legacyPlaceVisit(object legacyTimelineObject) error {
	visit := object.PlaceVisit
	if _, err := p.legacyRawPathPoints(visit.SimplifiedRawPath); err != nil {
		return err
	}
	startedAt, endedAt, ok := visit.Duration.times()
	if !ok {
		p.report["placeVisits with no readable time"]++
		return nil
	}
	location := visit.Location
	record := locationstore.ImportVisit{Source: locationstore.SourceGoogleSemantic, SourceRef: sourceRef(startedAt, endedAt),
		StartedAt: startedAt, EndedAt: endedAt, Probability: probability(location.LocationConfidence, true),
		SourceSemanticType: location.SemanticType}
	placeLatitude, placeLongitude := location.coordinates()
	record.Latitude, record.Longitude = placeLatitude, placeLongitude
	if visit.CenterLatE7 != nil && visit.CenterLngE7 != nil {
		if latitude, longitude, ok := fromE7(*visit.CenterLatE7, *visit.CenterLngE7); ok {
			record.Latitude, record.Longitude = &latitude, &longitude
		}
	}
	if location.PlaceID != "" && placeLatitude != nil {
		if err := p.sink.Place(locationstore.ImportPlace{GooglePlaceID: location.PlaceID, Name: location.Name, Address: location.Address,
			Latitude: *placeLatitude, Longitude: *placeLongitude,
			Kind:   locationstore.PlaceKindForGoogleSemanticType(location.SemanticType, location.Name != ""),
			Source: locationstore.SourceGoogleSemantic}); err != nil {
			return err
		}
		record.GooglePlaceID = location.PlaceID
	}
	if record.Latitude == nil && record.GooglePlaceID == "" {
		p.report["placeVisits with no place"]++
		return nil
	}
	return p.sink.Visit(record)
}

func (p *parser) legacyActivitySegment(object legacyTimelineObject) error {
	segment := object.ActivitySegment
	rawPath, err := p.legacyRawPathPoints(segment.SimplifiedRawPath)
	if err != nil {
		return err
	}
	startedAt, endedAt, ok := segment.Duration.times()
	if !ok {
		p.report["activitySegments with no readable time"]++
		return nil
	}
	activityType := segment.ActivityType
	if activityType == "" {
		activityType = "UNKNOWN_ACTIVITY_TYPE"
	}
	trip := locationstore.ImportTrip{Source: locationstore.SourceGoogleSemantic, SourceRef: sourceRef(startedAt, endedAt),
		StartedAt: startedAt, EndedAt: endedAt, Mode: locationstore.ModeForGoogleActivityType(activityType), SourceMode: activityType,
		DistanceMeters: segment.Distance, StartGooglePlaceID: segment.StartLocation.PlaceID, EndGooglePlaceID: segment.EndLocation.PlaceID}
	if trip.DistanceMeters != nil && *trip.DistanceMeters < 0 {
		trip.DistanceMeters = nil
	}
	for _, activity := range segment.Activities {
		if activity.ActivityType == activityType {
			trip.Probability = probability(activity.Probability, true)
			break
		}
	}
	trip.StartLatitude, trip.StartLongitude = segment.StartLocation.coordinates()
	trip.EndLatitude, trip.EndLongitude = segment.EndLocation.coordinates()
	// The waypoint path follows the roads; the raw path is what the phone saw.
	// The trip keeps the waypoints when there are any, and the raw path is in
	// the points either way.
	if segment.WaypointPath != nil && len(segment.WaypointPath.Waypoints) > 0 {
		for _, waypoint := range segment.WaypointPath.Waypoints {
			if waypoint.LatE7 == nil || waypoint.LngE7 == nil {
				continue
			}
			if latitude, longitude, ok := fromE7(*waypoint.LatE7, *waypoint.LngE7); ok {
				trip.Path = append(trip.Path, locationstore.PathPoint{Latitude: latitude, Longitude: longitude})
			}
		}
	} else {
		trip.Path = rawPath
	}
	return p.sink.Trip(trip)
}
