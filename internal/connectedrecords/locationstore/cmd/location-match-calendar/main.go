// Command location-match-calendar ties Google Calendar events to where the
// user was for them: a visit, a flight, a journey. It reads the calendars
// and their events from dash, the one way to Google Calendar, and hands them
// to location-store, which decides (POST /calendar/match); this prints what
// it did. Running it again over the same window changes nothing; a link a
// person made or deleted stays as they left it.
//
// Run it after an import has been named and grouped: location-import-google,
// then location-name-localities, then location-group-journeys, then this.
//
//	location-match-calendar [-from 2026-01-01] [-to 2026-02-01] [-calendar <id>,…] [-dry-run] [-all]
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	locationstore "github.com/kayushkin/location-store"
	"github.com/kayushkin/location-store/internal/dashcalendar"
	"github.com/kayushkin/location-store/internal/storeclient"
)

func main() {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	from := flag.String("from", today.AddDate(0, 0, -90).Format(time.DateOnly), "first day of events to match (YYYY-MM-DD, UTC)")
	to := flag.String("to", today.AddDate(0, 0, 1).Format(time.DateOnly), "day after the last day of events to match (YYYY-MM-DD, UTC)")
	calendarList := flag.String("calendar", "", "comma-separated calendar ids; empty reads every calendar dash lists")
	dryRun := flag.Bool("dry-run", false, "print the links it would write, write nothing")
	all := flag.Bool("all", false, "print unchanged links too, not only changes")
	flag.Parse()
	settings, err := locationstore.NewCalendarMatchingSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	fromTime, err := time.Parse(time.DateOnly, *from)
	if err != nil {
		log.Fatalf("-from: %v", err)
	}
	toTime, err := time.Parse(time.DateOnly, *to)
	if err != nil {
		log.Fatalf("-to: %v", err)
	}
	dash := dashcalendar.New(settings.String(locationstore.SettingDashURL), settings.String(locationstore.SettingDashToken))
	calendars, err := dash.Calendars()
	if err != nil {
		log.Fatal(err)
	}
	wanted := map[string]bool{}
	for _, id := range strings.Split(*calendarList, ",") {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = true
		}
	}
	request := locationstore.CalendarMatchRequest{DryRun: *dryRun, From: fromTime.Unix(), To: toTime.Unix()}
	for _, calendar := range calendars {
		if len(wanted) > 0 && !wanted[calendar.ID] {
			continue
		}
		delete(wanted, calendar.ID)
		events, err := dash.Events(calendar, fromTime, toTime)
		if err != nil {
			log.Fatal(err)
		}
		request.Calendars = append(request.Calendars, locationstore.CalendarEvents{Calendar: calendar, Events: events})
		fmt.Printf("calendar %s (%s, %s): %d events\n", calendar.ID, calendarName(calendar), calendar.AccessRole, len(events))
	}
	for id := range wanted {
		log.Fatalf("-calendar %s: dash lists no such calendar", id)
	}

	client := storeclient.New(settings.String(locationstore.SettingLocationStoreURL))
	var matching locationstore.CalendarMatching
	if err := client.Do(http.MethodPost, "/calendar/match", request, &matching); err != nil {
		log.Fatal(err)
	}
	if matching.DryRun {
		fmt.Println("dry run: nothing written")
	}
	fmt.Printf("%d events from %s to %s\n", matching.EventsRead, *from, *to)
	fmt.Printf("links by rule: %s\n", countsLabel(matching.LinksByRule))
	fmt.Printf("actions: %s\n", countsLabel(matching.Actions))
	fmt.Printf("unlinked: %s\n", countsLabel(matching.Unlinked))
	for _, planned := range matching.Links {
		if planned.Action == locationstore.CalendarLinkUnchanged && !*all {
			continue
		}
		link := planned.Link
		id := link.ID
		if id == "" {
			id = "(new)"
		}
		fmt.Printf("  %-12s %s  %s  %q → %s %s  [%s %.1f]\n               %s\n", planned.Action, id,
			time.Unix(link.StartedAt, 0).UTC().Format("2006-01-02 15:04Z"), link.Summary, link.TargetType, link.TargetID,
			link.Rule, link.Confidence, link.Reason)
	}
	for _, series := range matching.SeriesPlaces {
		place := "no spot"
		if series.PlaceID != nil {
			place = fmt.Sprintf("%s (%s) [%s]", *series.PlaceID, series.PlaceName, strings.Join(series.SpotPlaceIDs, " "))
		}
		id := series.ID
		if id == "" {
			id = "(dry run)"
		}
		fmt.Printf("  series %s %q → %s: %d of %d times out, of %d times; %s, confidence %.2f\n", id, series.Summary, place,
			series.InstancesAtPlace, series.InstancesOut, series.Instances, series.Outcome, series.Confidence)
	}
	for _, note := range matching.Notes {
		fmt.Printf("note: %s\n", note)
	}
}

func calendarName(calendar locationstore.GoogleCalendar) string {
	if calendar.SummaryOverride != "" {
		return calendar.SummaryOverride
	}
	return calendar.Summary
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
