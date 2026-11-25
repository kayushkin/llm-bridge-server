// Command location-suggest-place-names suggests names for the places the
// user goes to that have none, offline. It reads OpenStreetMap extracts on
// disk (Geofabrik's regional .osm.pbf files) for the named features and the
// addresses near each unnamed place, and the calendar through dash for
// events whose street address, found in the extracts, lies at one. It sends
// what it found to location-store (POST /place-suggestions/proposals), which
// decides the suggestions and keeps them as proposed; nothing is named until
// a person or agent accepts one (POST /place-suggestions/{id}/accept). No
// coordinate or address ever goes to an online service.
//
//	location-suggest-place-names [-extract <file or directory>]… [-limit 30] [-from 2025-01-01] [-to 2026-01-01] [-dry-run]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	locationstore "github.com/kayushkin/location-store"
	"github.com/kayushkin/location-store/internal/dashcalendar"
	"github.com/kayushkin/location-store/internal/geonames"
	"github.com/kayushkin/location-store/internal/osmextract"
	"github.com/kayushkin/location-store/internal/storeclient"
)

const (
	// searchMeters is how far around a place features, the nearest address
	// and calendar addresses are looked for; the store decides which are
	// near enough to suggest.
	searchMeters = 150
	// featuresSent is how many of the nearest features are sent per place.
	featuresSent = 10
	// addressAgreeMeters is how near each other the addresses a location
	// matches must lie for the match to stand.
	addressAgreeMeters = 150
)

type extractList []string

func (list *extractList) String() string         { return strings.Join(*list, ",") }
func (list *extractList) Set(value string) error { *list = append(*list, value); return nil }

func main() {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var extracts extractList
	flag.Var(&extracts, "extract", "an OpenStreetMap extract (.osm.pbf), or a directory of them; repeat for several. Default: the "+
		"LOCATION_STORE_OSM_EXTRACTS setting")
	limit := flag.Int("limit", 30, "how many unnamed places, most days visited first")
	from := flag.String("from", today.AddDate(-2, 0, 0).Format(time.DateOnly), "first day of calendar events to read (YYYY-MM-DD, UTC)")
	to := flag.String("to", today.AddDate(0, 0, 1).Format(time.DateOnly), "day after the last day of calendar events to read")
	dryRun := flag.Bool("dry-run", false, "print what it would suggest, store nothing")
	flag.Parse()
	settings, err := locationstore.NewPlaceNameSuggestingSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if len(extracts) == 0 {
		extracts = settings.StringList(locationstore.SettingOpenStreetMapExtracts)
	}
	files, err := extractFiles(extracts)
	if err != nil {
		log.Fatal(err)
	}
	fromTime, err := time.Parse(time.DateOnly, *from)
	if err != nil {
		log.Fatalf("-from: %v", err)
	}
	toTime, err := time.Parse(time.DateOnly, *to)
	if err != nil {
		log.Fatalf("-to: %v", err)
	}

	client := storeclient.New(settings.String(locationstore.SettingLocationStoreURL))
	var unnamed []locationstore.UnnamedPlace
	if err := client.Do(http.MethodGet, "/unnamed-places?"+url.Values{"limit": {fmt.Sprint(*limit)}}.Encode(), nil, &unnamed); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d unnamed places, home left out\n", len(unnamed))

	started := time.Now()
	extract, err := osmextract.Read(context.Background(), files)
	if err != nil {
		log.Fatal(err)
	}
	var sources []string
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			log.Fatal(err)
		}
		sources = append(sources, fmt.Sprintf("OpenStreetMap %s of %s", filepath.Base(file), info.ModTime().UTC().Format(time.DateOnly)))
	}
	fmt.Printf("read %d named features and %d addresses from %d extract(s) in %s\n", len(extract.Features), len(extract.Addresses),
		len(files), time.Since(started).Round(time.Second))

	dash := dashcalendar.New(settings.String(locationstore.SettingDashURL), settings.String(locationstore.SettingDashToken))
	geocoded, read, err := geocodeCalendar(dash, extract, fromTime, toTime)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("calendar: %d events with a location from %s to %s, %d found by street address\n", read, *from, *to, len(geocoded))

	request := locationstore.PlaceSuggestionProposals{DryRun: *dryRun, ProposedBy: locationstore.SetByPlaceNameSuggesting, Sources: sources}
	for _, one := range unnamed {
		place := one.Place
		evidence := locationstore.PlaceProposalEvidence{PlaceID: place.ID, NearbyPOIs: []locationstore.OSMFeature{},
			CalendarEvents: []locationstore.GeocodedCalendarEvent{}}
		for position, feature := range extract.FeaturesNear(place.Latitude, place.Longitude, searchMeters) {
			if position == featuresSent {
				break
			}
			evidence.NearbyPOIs = append(evidence.NearbyPOIs, locationstore.OSMFeature{OSMRef: feature.Ref, Name: feature.Name, Tags: feature.Tags,
				Latitude: feature.Latitude, Longitude: feature.Longitude, Address: feature.Address.Line(), DistanceMeters: math.Round(feature.DistanceMeters)})
		}
		if address, distance, ok := extract.NearestAddress(place.Latitude, place.Longitude, searchMeters); ok {
			evidence.NearestAddress = wireAddress(address, distance)
		}
		for _, event := range geocoded {
			distance := geonames.DistanceMeters(place.Latitude, place.Longitude, event.Address.Latitude, event.Address.Longitude)
			if distance <= searchMeters {
				event.DistanceMeters = math.Round(distance)
				event.Address.DistanceMeters = event.DistanceMeters
				evidence.CalendarEvents = append(evidence.CalendarEvents, event)
			}
		}
		request.Places = append(request.Places, evidence)
	}
	var answer locationstore.PlaceSuggesting
	if err := client.Do(http.MethodPost, "/place-suggestions/proposals", request, &answer); err != nil {
		log.Fatal(err)
	}
	if answer.DryRun {
		fmt.Println("dry run: nothing stored")
	}
	fmt.Printf("suggestions: %s\n\n", countsLabel(answer.Actions))
	patterns := map[string]locationstore.PlaceVisitPattern{}
	for _, one := range unnamed {
		patterns[one.Place.ID] = one.Pattern
	}
	for _, forPlace := range answer.Places {
		printPlace(forPlace, patterns[forPlace.Place.ID])
	}
}

// extractFiles turns the files and directories named into the .osm.pbf
// files to read, in order.
func extractFiles(named []string) ([]string, error) {
	var files []string
	for _, path := range named {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("extract %s: %w (download a regional .osm.pbf, such as Geofabrik's, there)", path, err)
		}
		if !info.IsDir() {
			files = append(files, path)
			continue
		}
		found, err := filepath.Glob(filepath.Join(path, "*.osm.pbf"))
		if err != nil {
			return nil, err
		}
		sort.Strings(found)
		files = append(files, found...)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .osm.pbf extract in %s", strings.Join(named, ", "))
	}
	return files, nil
}

func wireAddress(address osmextract.Address, distance float64) *locationstore.OSMAddress {
	return &locationstore.OSMAddress{OSMRef: address.Ref, Address: address.Line(), Latitude: address.Latitude, Longitude: address.Longitude,
		DistanceMeters: math.Round(distance)}
}

// geocodeCalendar reads every calendar's events and finds each one whose
// location holds a street address in the extracts. It answers those, and how
// many events had a location at all.
func geocodeCalendar(dash *dashcalendar.Client, extract *osmextract.Extract, from, to time.Time) ([]locationstore.GeocodedCalendarEvent, int, error) {
	calendars, err := dash.Calendars()
	if err != nil {
		return nil, 0, err
	}
	var found []locationstore.GeocodedCalendarEvent
	read := 0
	for position := range calendars {
		calendar := &calendars[position]
		events, err := dash.Events(*calendar, from, to)
		if err != nil {
			return nil, 0, err
		}
		for _, event := range events {
			if event.Status == "cancelled" || !locationstore.EventLocationNamesSomewhere(event) {
				continue
			}
			read++
			match, ok := extract.Geocode(event.Location, addressAgreeMeters)
			if !ok {
				continue
			}
			startedAt, endedAt, _, err := locationstore.GoogleEventTimes(event, calendar)
			if err != nil {
				return nil, 0, fmt.Errorf("calendar %s: %w", calendar.ID, err)
			}
			found = append(found, locationstore.GeocodedCalendarEvent{CalendarID: calendar.ID, EventID: event.ID, RecurringEventID: event.RecurringEventID,
				Summary: event.Summary, Location: event.Location, StartedAt: startedAt, EndedAt: endedAt, Venue: match.Venue,
				Address: *wireAddress(match.Address, 0)})
		}
	}
	return found, read, nil
}

func printPlace(forPlace locationstore.PlaceSuggestionsForPlace, pattern locationstore.PlaceVisitPattern) {
	place := forPlace.Place
	town := ""
	if place.Locality != nil {
		town = place.Locality.City
	}
	fmt.Printf("%s (%s, %s): %d days, %d visits, %.1f h, median %.0f min; usually %s (%d), arriving %02d:00 (%d) %s; %s to %s\n",
		place.ID, town, place.Kind, pattern.Days, pattern.Visits, pattern.Hours, pattern.MedianVisitMinutes, pattern.UsualWeekday,
		pattern.UsualWeekdayVisits, pattern.UsualArrivalHour, pattern.UsualArrivalHourVisits, pattern.TimeZone,
		time.Unix(pattern.FirstVisitAt, 0).UTC().Format(time.DateOnly), time.Unix(pattern.LastVisitAt, 0).UTC().Format(time.DateOnly))
	if len(forPlace.Suggestions) == 0 {
		fmt.Println("    nothing to suggest")
		fmt.Println()
		return
	}
	evidence := forPlace.Suggestions[0].Evidence
	for _, feature := range evidence.NearbyPOIs {
		var tags []string
		for key, value := range feature.Tags {
			if key != "name" {
				tags = append(tags, key+"="+value)
			}
		}
		sort.Strings(tags)
		fmt.Printf("    poi      %4.0f m  %-40q %s  %s\n", feature.DistanceMeters, feature.Name, feature.OSMRef, strings.Join(tags, " "))
	}
	if evidence.NearestAddress != nil {
		fmt.Printf("    address  %4.0f m  %s  %s\n", evidence.NearestAddress.DistanceMeters, evidence.NearestAddress.Address, evidence.NearestAddress.OSMRef)
	}
	for _, event := range evidence.CalendarEvents {
		fmt.Printf("    calendar %4.0f m  %s %q at %q (visit overlapped: %t)\n", event.DistanceMeters,
			time.Unix(event.StartedAt, 0).UTC().Format("2006-01-02 15:04Z"), event.Summary, event.Location, event.VisitOverlapped)
	}
	for _, suggestion := range forPlace.Suggestions {
		id := suggestion.ID
		if id == "" {
			id = "(new)"
		}
		fmt.Printf("    → %s %s %q [%s] %s\n", id, suggestion.Status, suggestion.Name, suggestion.Source, suggestion.Reason)
	}
	fmt.Println()
}

func countsLabel(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	var keys []string
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", key, counts[key]))
	}
	return strings.Join(parts, ", ")
}
