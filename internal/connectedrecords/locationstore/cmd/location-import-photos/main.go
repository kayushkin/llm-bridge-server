// Command location-import-photos makes each photo in photo-store that has GPS
// and a date into a location point: source photo, source_ref the photo's id.
// Only photos whose date can be trusted to be when they were taken are read
// (-date-sources, default exif, filename and google_photos): a photo dated when it was sent
// in a chat or posted online was taken somewhere earlier.
//
// The import is found again by the hash of the points it would write, so
// running it again with nothing new in photo-store changes nothing. A photo
// whose GPS or date changed moves its point.
//
//	location-import-photos [-date-sources exif,filename,google_photos] [-dry-run]
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

	"github.com/kayushkin/llm-bridge/servicesettings"
	locationstore "github.com/kayushkin/location-store"
	"github.com/kayushkin/location-store/internal/importclient"
	photostore "github.com/kayushkin/photo-store"
)

const pageSize = 1000

func main() {
	dateSourcesFlag := flag.String("date-sources", photostore.DateFromExif+","+photostore.DateFromFilename+","+photostore.DateFromGooglePhotos,
		"the photo date sources trusted to say when a photo was taken, comma-separated: "+strings.Join(photostore.DateSources, ", "))
	dryRun := flag.Bool("dry-run", false, "read and count, post nothing")
	flag.Parse()
	settings, err := locationstore.NewImportPhotosSettingsRegistry(servicesettings.ProcessEnvironment())
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

	points, report, err := readPhotoPoints(photoStoreURL, trusted)
	if err != nil {
		log.Fatal(err)
	}
	for reason, count := range report {
		fmt.Printf("left out %d: %s\n", count, reason)
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
		photoStoreURL+"/photos?has_gps=true", *dryRun)
	if len(points) == 0 {
		fmt.Println("no photo has GPS and a trusted date; nothing to import")
		return
	}
	if err := client.Begin(locationstore.ImportKindPhotoStore); errors.Is(err, importclient.ErrAlreadyImported) {
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
	fmt.Printf("read %d photos with GPS and a trusted date\n", len(points))
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

// readPhotoPoints pages through photo-store's photos with GPS, oldest first.
func readPhotoPoints(photoStoreURL string, trusted []string) ([]locationstore.ImportPoint, map[string]int, error) {
	client := &http.Client{Timeout: time.Minute}
	report := map[string]int{}
	points := []locationstore.ImportPoint{}
	for offset := 0; ; offset += pageSize {
		query := url.Values{"has_gps": {"true"}, "limit": {fmt.Sprint(pageSize)}, "offset": {fmt.Sprint(offset)}}
		response, err := client.Get(photoStoreURL + "/photos?" + query.Encode())
		if err != nil {
			return nil, nil, err
		}
		content, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return nil, nil, err
		}
		if response.StatusCode != http.StatusOK {
			return nil, nil, fmt.Errorf("photo-store answered %d: %s", response.StatusCode, strings.TrimSpace(string(content)))
		}
		var photos []photostore.Photo
		if err := json.Unmarshal(content, &photos); err != nil {
			return nil, nil, fmt.Errorf("photo-store's answer: %w", err)
		}
		for _, photo := range photos {
			switch {
			case photo.Latitude == nil || photo.Longitude == nil:
				report["photos with no GPS"]++
			case !slices.Contains(trusted, photo.DateSource) || photo.TakenAt <= 0:
				report[fmt.Sprintf("photos dated by %s, not trusted to say when they were taken", photo.DateSource)]++
			default:
				points = append(points, locationstore.ImportPoint{Timestamp: photo.TakenAt, Latitude: *photo.Latitude,
					Longitude: *photo.Longitude, Source: locationstore.PointSourcePhoto, SourceRef: photo.ID})
			}
		}
		if len(photos) < pageSize {
			return points, report, nil
		}
	}
}
