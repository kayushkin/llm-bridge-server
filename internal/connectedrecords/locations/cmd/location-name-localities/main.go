// Command location-name-localities names the town of every place, visit and
// trip end in location-store whose locality is empty, offline. The first run
// downloads GeoNames' public list of towns of 1000 people or more into
// LOCATION_STORE_GEONAMES_DIRECTORY, which must be the directory
// llm-bridge-server's location_store.geonames_directory names, since the
// store reads the files from there; the download asks for the public files
// and sends nothing about anyone. The towns are then looked up
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

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/locations"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/locations/internal/geonames"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/locations/internal/storeclient"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "count what would be named, write nothing")
	downloadAgain := flag.Bool("download-again", false, "fetch the GeoNames files again even when they are there (the store reads them once per start)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `usage: location-name-localities [-dry-run] [-download-again]

LOCATION_STORE_GEONAMES_DIRECTORY (required) must name the directory
llm-bridge-server's location_store.geonames_directory names: the GeoNames
files are downloaded there, and the store reads them from there.
LOCATION_STORE_URL is where location-store answers.

`)
		flag.PrintDefaults()
	}
	flag.Parse()
	settings, err := locations.NewLocalityNamingSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	client := storeclient.New(settings.String(locations.SettingLocationStoreURL))

	// The files must land where the store reads them: the directory the
	// server's location_store.geonames_directory names, which
	// LOCATION_STORE_GEONAMES_DIRECTORY must name too.
	dir := settings.String(locations.SettingGeonamesDirectory)
	if *downloadAgain || !geonames.Present(dir) {
		fmt.Printf("downloading the GeoNames towns to %s…\n", dir)
		if err := geonames.Download(dir); err != nil {
			log.Fatal(err)
		}
	}

	var fill locations.LocalityFill
	if err := client.Do(http.MethodPost, "/localities/fill", locations.LocalityFillRequest{DryRun: *dryRun}, &fill); err != nil {
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
