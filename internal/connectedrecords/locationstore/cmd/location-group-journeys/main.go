// Command location-group-journeys finds the home base of each stretch of
// time in location-store, from where the nights were spent, and groups each
// span away from it into a journey: a trip of kind journey whose children are
// the drives and flights of the span. The decisions are location-store's
// (POST /journeys/group); this prints them. Running it again after an import
// extends or adds journeys and changes nothing else. It never touches a
// journey made by hand, never moves a trip someone placed by hand, and
// rewrites only titles it wrote.
//
//	location-group-journeys [-dry-run] [-away-km 80] [-minimum-hours 6] [-home-change-days 21] [-return-break-hours 12]
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	locationstore "github.com/kayushkin/location-store"
	"github.com/kayushkin/location-store/internal/storeclient"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "print the home periods and journeys it would write, write nothing")
	awayKilometres := flag.Float64("away-km", locationstore.DefaultAwayDistanceMeters/1000, "how far from home counts as away, in km")
	minimumHours := flag.Float64("minimum-hours", locationstore.DefaultMinimumAwaySeconds/3600, "how long a span away with no night away must last to be a journey, in hours")
	homeChangeDays := flag.Int("home-change-days", locationstore.DefaultHomeChangeDays, "how many days a new night place must lead before it is the home")
	returnBreakHours := flag.Float64("return-break-hours", locationstore.DefaultReturnBreakSeconds/3600, "how long back near home, with no night there, ends a journey, in hours")
	flag.Parse()
	settings, err := locationstore.NewJourneyGroupingSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	client := storeclient.New(settings.String(locationstore.SettingLocationStoreURL))
	request := locationstore.JourneyGroupRequest{DryRun: *dryRun, AwayDistanceMeters: *awayKilometres * 1000,
		MinimumAwaySeconds: int64(*minimumHours * 3600), HomeChangeDays: *homeChangeDays, ReturnBreakSeconds: int64(*returnBreakHours * 3600)}
	var grouping locationstore.JourneyGrouping
	if err := client.Do(http.MethodPost, "/journeys/group", request, &grouping); err != nil {
		log.Fatal(err)
	}
	if grouping.DryRun {
		fmt.Println("dry run: nothing written")
	}
	fmt.Printf("home periods (%d):\n", len(grouping.HomePeriods))
	for _, period := range grouping.HomePeriods {
		place := "no place"
		if period.PlaceID != nil {
			place = *period.PlaceID
		}
		id := period.ID
		if id == "" {
			id = "(new)"
		}
		fmt.Printf("  %s  %s  %s  %s to %s  %d of %d known nights at home\n", id, place, localityLabel(period.Locality),
			day(period.StartedAt), day(period.EndedAt), period.NightsAtHome, period.NightsKnown)
	}
	if len(grouping.HomePlacesMarked) > 0 {
		fmt.Printf("places marked home: %s\n", strings.Join(grouping.HomePlacesMarked, ", "))
	}
	fmt.Printf("journeys (%d):\n", len(grouping.Journeys))
	for _, journey := range grouping.Journeys {
		id := journey.TripID
		if id == "" {
			id = "(new)"
		}
		fmt.Printf("  %-9s %s  %s\n            %s to %s, %d nights, %s, farthest %.0f km, %d trips (%d attached, %d detached)\n", journey.Action, id,
			journey.Title, day(journey.StartedAt), day(journey.EndedAt), journey.Nights, journey.Mode, journey.FarthestMeters/1000,
			len(journey.ChildTripIDs), journey.ChildrenAttached, journey.ChildrenDetached)
		for _, note := range journey.Notes {
			fmt.Printf("            note: %s\n", note)
		}
	}
	if len(grouping.DeletedJourneyIDs) > 0 {
		fmt.Printf("journeys no longer found, deleted (restorable): %s\n", strings.Join(grouping.DeletedJourneyIDs, ", "))
	}
	for _, note := range grouping.Notes {
		fmt.Printf("note: %s\n", note)
	}
}

func day(at int64) string {
	if at == 0 {
		return "now"
	}
	return time.Unix(at, 0).UTC().Format("2006-01-02 15:04Z")
}

func localityLabel(locality *locationstore.Locality) string {
	if locality == nil {
		return "(no town named)"
	}
	return locationstore.LocalityLabel(locality, "")
}
