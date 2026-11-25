// Command location-name-localities names the town of every place, visit and
// trip end in location-store whose locality is empty, offline. The first run
// downloads GeoNames' public list of towns of 1000 people or more into the
// service's data directory (<data dir>/geonames); the download asks for the
// public files and sends nothing about anyone. The towns are then looked up
// inside location-store: no coordinate leaves the host. A locality already set
// is never changed.
//
//	location-name-localities [-dry-run] [-download-again]
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/kayushkin/llm-bridge/servicesettings"
	locationstore "github.com/kayushkin/location-store"
	"github.com/kayushkin/location-store/internal/geonames"
	"github.com/kayushkin/location-store/internal/storeclient"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "count what would be named, write nothing")
	downloadAgain := flag.Bool("download-again", false, "fetch the GeoNames files again even when they are there (the service reads them once per start)")
	flag.Parse()
	settings, err := locationstore.NewLocalityNamingSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	client := storeclient.New(settings.String(locationstore.SettingLocationStoreURL))

	// The service says where its data is, so the files land where it reads.
	var served struct {
		Settings []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"settings"`
	}
	if err := client.Do(http.MethodGet, "/settings", nil, &served); err != nil {
		log.Fatal(err)
	}
	dataDir := ""
	for _, setting := range served.Settings {
		if setting.Key == locationstore.SettingDataDirectory {
			dataDir = setting.Value
		}
	}
	if dataDir == "" {
		log.Fatalf("location-store's GET /settings names no %s", locationstore.SettingDataDirectory)
	}
	dir := locationstore.GeonamesDirIn(dataDir)
	if *downloadAgain || !geonames.Present(dir) {
		fmt.Printf("downloading the GeoNames towns to %s…\n", dir)
		if err := geonames.Download(dir); err != nil {
			log.Fatal(err)
		}
	}

	var fill locationstore.LocalityFill
	if err := client.Do(http.MethodPost, "/localities/fill", locationstore.LocalityFillRequest{DryRun: *dryRun}, &fill); err != nil {
		log.Fatal(err)
	}
	verb := "named"
	if fill.DryRun {
		verb = "would name"
	}
	fmt.Printf("%s %d places, %d visits, %d trip starts and %d trip ends, in %d towns (of %d GeoNames towns)\n", verb,
		fill.Places, fill.Visits, fill.TripStarts, fill.TripEnds, fill.DistinctTowns, fill.GeonamesTownsIn)
	fmt.Printf("already named: %d; no coordinate: %d; no town within two degrees: %d\n", fill.AlreadyNamed, fill.NoCoordinate, fill.NoTownNearby)
}
