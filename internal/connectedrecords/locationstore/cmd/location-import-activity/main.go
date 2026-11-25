// Command location-import-activity makes each activity in activity-store that
// says where the user was into a location point: source google_activity,
// source_ref the activity's id, at the activity's time. activity-store keeps
// no accuracy, so the point has none.
//
// An activity's coordinates come from the first Google "Locations" line that
// gives them, and that line says how Google knew. Only the bases in -bases
// are read (default "From your device"): "From your places (Home)" is the
// saved home address, and "Based on your past activity" is a guess, neither a
// fix of where the user was at that time.
//
// activity-store has no filter for activities with coordinates, so every
// live activity is read, a page at a time, and the rest are left out. The
// import is found again by the hash of the points it would write, so
// running it again with nothing new changes nothing.
//
//	location-import-activity [-bases "From your device"] [-dry-run]
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	activitystore "github.com/kayushkin/activity-store"
	"github.com/kayushkin/llm-bridge/servicesettings"
	locationstore "github.com/kayushkin/location-store"
	"github.com/kayushkin/location-store/internal/importclient"
)

// pageSize is activity-store's largest page.
const pageSize = 1000

// DefaultBasis is the basis Google gives a location its device measured.
const DefaultBasis = "From your device"

func main() {
	basesFlag := flag.String("bases", DefaultBasis,
		"how Google knew the location, as the activity's Locations line says it, comma-separated: only these are read")
	dryRun := flag.Bool("dry-run", false, "read and count, post nothing")
	flag.Parse()
	settings, err := locationstore.NewImportActivitySettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	trustedBases := []string{}
	for _, basis := range strings.Split(*basesFlag, ",") {
		if basis = strings.TrimSpace(basis); basis != "" {
			trustedBases = append(trustedBases, basis)
		}
	}
	if len(trustedBases) == 0 {
		log.Fatal("-bases names no basis")
	}
	activityStoreURL := strings.TrimRight(settings.String(locationstore.SettingActivityStoreURL), "/")

	points, report, err := readActivityPoints(activityStoreURL, trustedBases)
	if err != nil {
		log.Fatal(err)
	}
	for reason, count := range report {
		fmt.Printf("left out %d: %s\n", count, reason)
	}
	if len(points) == 0 {
		fmt.Println("no activity has coordinates from a trusted basis; nothing to import")
		return
	}
	encoded, err := json.Marshal(points)
	if err != nil {
		log.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	machine, err := os.Hostname()
	if err != nil {
		log.Fatal(err)
	}
	client := importclient.New(settings.String(locationstore.SettingLocationStoreURL), hex.EncodeToString(digest[:]), machine,
		activityStoreURL+"/activities", *dryRun)
	if err := client.Begin(locationstore.ImportKindActivityStore); errors.Is(err, importclient.ErrAlreadyImported) {
		fmt.Printf("%v; nothing to do\n", err)
		return
	} else if err != nil {
		log.Fatal(err)
	}
	for _, point := range points {
		if err := client.Point(point); err != nil {
			log.Fatal(err)
		}
	}
	record, err := client.Finish()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("read %d activities with coordinates from a trusted basis\n", len(points))
	if record == nil {
		fmt.Println("dry run: nothing posted")
		return
	}
	written, err := json.Marshal(record.Counts)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s finished: %s\n", record.ID, written)
}

// readActivityPoints pages through activity-store's live activities, oldest
// first, and keeps those with coordinates from a trusted basis.
func readActivityPoints(activityStoreURL string, trustedBases []string) ([]locationstore.ImportPoint, map[string]int, error) {
	client := &http.Client{Timeout: time.Minute}
	report := map[string]int{}
	points := []locationstore.ImportPoint{}
	for offset := 0; ; offset += pageSize {
		query := url.Values{"order": {"asc"}, "limit": {fmt.Sprint(pageSize)}, "offset": {fmt.Sprint(offset)}}
		response, err := client.Get(activityStoreURL + "/activities?" + query.Encode())
		if err != nil {
			return nil, nil, err
		}
		content, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return nil, nil, err
		}
		if response.StatusCode != http.StatusOK {
			return nil, nil, fmt.Errorf("activity-store answered %d: %s", response.StatusCode, strings.TrimSpace(string(content)))
		}
		var page activitystore.ActivityPage
		if err := json.Unmarshal(content, &page); err != nil {
			return nil, nil, fmt.Errorf("activity-store's answer: %w", err)
		}
		for _, activity := range page.Activities {
			point, reason := pointOf(activity, trustedBases)
			if reason != "" {
				report[reason]++
				continue
			}
			points = append(points, point)
		}
		if len(page.Activities) < pageSize {
			return points, report, nil
		}
	}
}

// pointOf is the point an activity says the user was at, or the reason it
// gives none. Activities with no coordinates are the great majority and are
// not reported.
func pointOf(activity activitystore.Activity, trustedBases []string) (locationstore.ImportPoint, string) {
	if activity.Latitude == nil || activity.Longitude == nil {
		return locationstore.ImportPoint{}, "activities with no coordinates"
	}
	// activity-store takes the coordinates from the first Locations line
	// that gives them; that line's basis says how Google knew.
	basis := ""
	for _, location := range activity.Details.Locations {
		if location.Latitude != nil && location.Longitude != nil {
			basis = location.Basis
			break
		}
	}
	if !slices.Contains(trustedBases, basis) {
		return locationstore.ImportPoint{}, fmt.Sprintf("activities located %q, not a trusted basis", basis)
	}
	if activity.OccurredAt <= 0 {
		return locationstore.ImportPoint{}, "activities with no time"
	}
	return locationstore.ImportPoint{Timestamp: activity.OccurredAt, Latitude: *activity.Latitude, Longitude: *activity.Longitude,
		Source: locationstore.PointSourceGoogleActivity, SourceRef: activity.ID}, ""
}
