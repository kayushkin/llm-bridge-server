// Command location-segment-phone-points makes the points a phone sent live
// (POST /points/live) into visits and the movements between them. The
// decisions are location-store's (POST /phone-points/segment); this prints
// them. Running it again rewrites only what earlier runs made, and never a
// visit or movement someone edited or deleted.
//
//	location-segment-phone-points [-dry-run] [-days 3] [-all]
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	locationstore "github.com/kayushkin/location-store"
	"github.com/kayushkin/location-store/internal/storeclient"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "print what it would write, write nothing")
	days := flag.Int("days", 3, "read the phone points of the last this many days")
	all := flag.Bool("all", false, "print unchanged records too, not only changes")
	flag.Parse()
	if *days < 1 {
		log.Fatal("-days must be at least 1")
	}
	settings, err := locationstore.NewPhoneSegmentingSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	client := storeclient.New(settings.String(locationstore.SettingLocationStoreURL))
	to := time.Now().Add(time.Hour).Unix()
	request := locationstore.PhoneSegmentRequest{DryRun: *dryRun, From: to - int64(*days)*86400 - 3600, To: to}
	var segmentation locationstore.PhoneSegmentation
	if err := client.Do(http.MethodPost, "/phone-points/segment", request, &segmentation); err != nil {
		log.Fatal(err)
	}
	if segmentation.DryRun {
		fmt.Println("dry run: nothing written")
	}
	for _, action := range segmentation.Actions {
		if action.Action == "unchanged" && !*all {
			continue
		}
		detail := ""
		if action.PlaceID != nil {
			detail = " at " + *action.PlaceID
		}
		if action.Mode != "" {
			detail = fmt.Sprintf(" %s %.1f km", action.Mode, *action.DistanceMeters/1000)
		}
		if action.Note != "" {
			detail += " (" + action.Note + ")"
		}
		fmt.Printf("  %-17s %-5s %-12s %s to %s, %d points%s\n", action.Action, action.EntityType, action.ID,
			clock(action.StartedAt), clock(action.EndedAt), action.Points, detail)
	}
	actions := make([]string, 0, len(segmentation.ActionCounts))
	for action := range segmentation.ActionCounts {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	fmt.Printf("read %d phone points from %s (%d too rough); %d places made", segmentation.PointsRead,
		clock(segmentation.From), segmentation.PointsTooRough, len(segmentation.PlacesCreated))
	for _, action := range actions {
		fmt.Printf(", %d %s", segmentation.ActionCounts[action], action)
	}
	fmt.Println()
}

func clock(epochSeconds int64) string {
	return time.Unix(epochSeconds, 0).UTC().Format("2006-01-02 15:04Z")
}
