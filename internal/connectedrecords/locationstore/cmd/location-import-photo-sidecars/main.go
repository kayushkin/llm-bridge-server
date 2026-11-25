// Command location-import-photo-sidecars places photos whose files carry no
// GPS by the location Google Photos kept for them, from their Google Takeout
// sidecars: one point per photo, source google_photos_sidecar, source_ref the
// photo-store photo id, at the photo's taken_at in photo-store.
//
// It reads every file named sidecars.jsonl under -dir. Each line is one
// photo's Takeout sidecar, reduced and keyed by the sha256 of the photo's
// file: {"sha256", "geoData": {"latitude", "longitude", …}, …}. A geoData of
// 0, 0 is Google's word for no location. The photo is found in photo-store by
// the hash (GET /photos?sha256=), and is left out when photo-store already
// has GPS for it (location-import-photos makes that point), when it is not
// in photo-store, or when its date is not trusted to say when it was taken
// (-date-sources, default exif, filename and google_photos, as location-import-photos).
// Google keeps no accuracy for geoData, so the point has none.
//
// The import is found again by the hash of the points it would write, so
// running it again with nothing new changes nothing.
//
//	location-import-photo-sidecars -dir <directory> [-date-sources exif,filename,google_photos] [-dry-run]
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	locationstore "github.com/kayushkin/location-store"
	"github.com/kayushkin/location-store/internal/importclient"
	photostore "github.com/kayushkin/photo-store"
)

// SidecarFileName is the name of the files read under -dir.
const SidecarFileName = "sidecars.jsonl"

// sidecar is the part of a sidecar line this command reads.
type sidecar struct {
	Sha256  string `json:"sha256"`
	GeoData struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"geoData"`
}

func main() {
	directory := flag.String("dir", "", "the directory whose every "+SidecarFileName+" is read, at any depth")
	dateSourcesFlag := flag.String("date-sources", photostore.DateFromExif+","+photostore.DateFromFilename+","+photostore.DateFromGooglePhotos,
		"the photo date sources trusted to say when a photo was taken, comma-separated: "+strings.Join(photostore.DateSources, ", "))
	dryRun := flag.Bool("dry-run", false, "read and count, post nothing")
	flag.Parse()
	if *directory == "" {
		log.Fatal("-dir is required")
	}
	settings, err := locationstore.NewImportPhotoSidecarsSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	trusted := strings.Split(*dateSourcesFlag, ",")
	for _, source := range trusted {
		if !slices.Contains(photostore.DateSources, source) || source == photostore.DateUnknown {
			log.Fatalf("-date-sources: %q is not a photo date source that names a time: use any of %s except %s", source, strings.Join(photostore.DateSources, ", "), photostore.DateUnknown)
		}
	}
	photoStoreURL := strings.TrimRight(settings.String(locationstore.SettingPhotoStoreURL), "/")

	report := map[string]int{}
	located, err := readSidecars(*directory, report)
	if err != nil {
		log.Fatal(err)
	}
	points, err := placePhotos(photoStoreURL, located, trusted, report)
	if err != nil {
		log.Fatal(err)
	}
	for reason, count := range report {
		fmt.Printf("left out %d: %s\n", count, reason)
	}
	if len(points) == 0 {
		fmt.Println("no sidecar places a photo that photo-store has no GPS for; nothing to import")
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
	absolute, err := filepath.Abs(*directory)
	if err != nil {
		log.Fatal(err)
	}
	client := importclient.New(settings.String(locationstore.SettingLocationStoreURL), hex.EncodeToString(digest[:]), machine, absolute, *dryRun)
	if err := client.Begin(locationstore.ImportKindGooglePhotosSidecars); errors.Is(err, importclient.ErrAlreadyImported) {
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
	fmt.Printf("read %d photos placed by their sidecar\n", len(points))
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

// readSidecars reads every sidecar file under directory, in path order, and
// answers the sidecars with a location, one per photo hash, first seen kept.
func readSidecars(directory string, report map[string]int) ([]sidecar, error) {
	files := []string{}
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() == SidecarFileName {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no %s under %s", SidecarFileName, directory)
	}
	slices.Sort(files)
	seen := map[string]sidecar{}
	located := []sidecar{}
	for _, path := range files {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 1<<16), 1<<24)
		for line := 1; scanner.Scan(); line++ {
			var read sidecar
			if err := json.Unmarshal(scanner.Bytes(), &read); err != nil {
				file.Close()
				return nil, fmt.Errorf("%s line %d: %w", path, line, err)
			}
			read.Sha256 = strings.ToLower(strings.TrimSpace(read.Sha256))
			switch {
			case read.Sha256 == "":
				file.Close()
				return nil, fmt.Errorf("%s line %d: no sha256", path, line)
			case read.GeoData.Latitude == 0 && read.GeoData.Longitude == 0:
				report["sidecars with no geoData"]++
				continue
			}
			if earlier, found := seen[read.Sha256]; found {
				if earlier.GeoData != read.GeoData {
					report["sidecars that place a photo already placed by another sidecar, elsewhere (the first is kept)"]++
				} else {
					report["sidecars repeating another for the same photo"]++
				}
				continue
			}
			seen[read.Sha256] = read
			located = append(located, read)
		}
		err = scanner.Err()
		file.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return located, nil
}

// placePhotos finds each sidecar's photo in photo-store and answers a point
// for each photo with no GPS of its own and a trusted date.
func placePhotos(photoStoreURL string, located []sidecar, trusted []string, report map[string]int) ([]locationstore.ImportPoint, error) {
	client := &http.Client{Timeout: time.Minute}
	points := []locationstore.ImportPoint{}
	for _, read := range located {
		response, err := client.Get(photoStoreURL + "/photos?" + url.Values{"sha256": {read.Sha256}}.Encode())
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("photo-store answered %d: %s", response.StatusCode, strings.TrimSpace(string(content)))
		}
		var photos []photostore.Photo
		if err := json.Unmarshal(content, &photos); err != nil {
			return nil, fmt.Errorf("photo-store's answer: %w", err)
		}
		if len(photos) > 1 {
			return nil, fmt.Errorf("photo-store answered %d photos for one sha256: is GET /photos?sha256= deployed?", len(photos))
		}
		if len(photos) == 0 {
			report["sidecars whose photo is not in photo-store"]++
			continue
		}
		photo := photos[0]
		switch {
		case photo.Sha256 != read.Sha256:
			return nil, fmt.Errorf("photo-store answered %s for sha256 %s: is GET /photos?sha256= deployed?", photo.ID, read.Sha256)
		case photo.Latitude != nil && photo.Longitude != nil:
			report["photos photo-store has GPS for already (location-import-photos reads them)"]++
		case !slices.Contains(trusted, photo.DateSource) || photo.TakenAt <= 0:
			report[fmt.Sprintf("photos dated by %s, not trusted to say when they were taken", photo.DateSource)]++
		default:
			points = append(points, locationstore.ImportPoint{Timestamp: photo.TakenAt, Latitude: read.GeoData.Latitude,
				Longitude: read.GeoData.Longitude, Source: locationstore.PointSourceGooglePhotosSidecar, SourceRef: photo.ID})
		}
	}
	return points, nil
}
