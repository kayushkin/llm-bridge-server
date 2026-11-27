// Command possession-add-photos brings photos of the owner's things into the
// inventory through llm-bridge-server: it keeps each original in
// possession-store, adds the photo to photo-store upright with its EXIF date
// and place, and queues it for review. The photos are the principal's whose
// token it sends.
//
//	possession-add-photos -queued-by principal_000001 [-folder possessions] [-time-zone America/Los_Angeles] photo.jpg…
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photos"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photos/thumbnail"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func main() {
	queuedBy := flag.String("queued-by", "", "who is adding the photos: a principal_… id (required)")
	folder := flag.String("folder", "possessions", "the photo-store folder the photos go in")
	timeZone := flag.String("time-zone", "", "where the camera was, for a photo whose EXIF date has no offset (an IANA zone such as America/Los_Angeles)")
	flag.Parse()
	if *queuedBy == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: possession-add-photos -queued-by principal_… [-folder …] [-time-zone …] photo…")
		os.Exit(2)
	}
	settings, err := possessions.NewAddPhotosSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		fail(err)
	}
	if err := settings.CheckRequired(); err != nil {
		fail(err)
	}
	var zone *time.Location
	if *timeZone != "" {
		if zone, err = time.LoadLocation(*timeZone); err != nil {
			fail(err)
		}
	}
	adder := adder{
		client:    &http.Client{Timeout: 2 * time.Minute},
		bridgeURL: strings.TrimRight(settings.String(possessions.SettingBridgeURL), "/"),
		token:     settings.String(possessions.SettingPrincipalToken),
		folder:    *folder,
		queuedBy:  *queuedBy,
		zone:      zone,
	}
	failures := 0
	for _, path := range flag.Args() {
		photoID, reviewID, err := adder.add(path)
		if err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			continue
		}
		fmt.Printf("%s\t%s\t%s\n", path, photoID, reviewID)
	}
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "%d of %d photos failed\n", failures, flag.NArg())
		os.Exit(1)
	}
}

type adder struct {
	client    *http.Client
	bridgeURL string
	token     string
	folder    string
	queuedBy  string
	zone      *time.Location
}

func (a adder) add(path string) (string, string, error) {
	extension := strings.ToLower(filepath.Ext(path))
	mediaType, known := possessions.OriginalMediaTypes[extension]
	if !known {
		return "", "", fmt.Errorf("%s files are not read; send JPEG, PNG or WebP (a phone's HEIC photo can be shared as JPEG)", extension)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(original)
	hash := hex.EncodeToString(sum[:])
	facts, err := possessions.ReadExif(original)
	if err != nil {
		return "", "", err
	}
	upright, err := possessions.UprightJPEG(original)
	if err != nil {
		return "", "", err
	}
	thumbnailJPEG, width, height, err := thumbnail.Make(upright)
	if err != nil {
		return "", "", err
	}
	takenAt, dateSource := int64(0), "unknown"
	if taken, ok, err := facts.TakenAt(a.zone); err != nil {
		return "", "", err
	} else if ok {
		takenAt, dateSource = taken.Unix(), "exif"
	} else if info, err := os.Stat(path); err == nil {
		takenAt, dateSource = info.ModTime().Unix(), "mtime"
	}
	kept, err := a.call(http.MethodPut, "/possessions/originals/"+hash+"?"+url.Values{"extension": {extension}, "added_by": {a.queuedBy}}.Encode(), mediaType, original)
	if err != nil {
		return "", "", err
	}
	var where struct {
		SourceMachine string `json:"source_machine"`
		SourcePath    string `json:"source_path"`
	}
	if err := json.Unmarshal(kept, &where); err != nil || where.SourceMachine == "" || where.SourcePath == "" {
		return "", "", fmt.Errorf("possession-store kept the original but did not say where: %s", kept)
	}
	body, err := json.Marshal(photos.ImportRequest{Photos: []photos.ImportPhoto{{
		Sha256: hash, SourceMachine: where.SourceMachine, SourcePath: where.SourcePath, Folder: a.folder, MediaType: mediaType,
		TakenAt: takenAt, DateSource: dateSource, Latitude: facts.Latitude, Longitude: facts.Longitude,
		Width: width, Height: height, ThumbnailJPEG: thumbnailJPEG,
	}}})
	if err != nil {
		return "", "", err
	}
	if _, err := a.call(http.MethodPost, "/photos/imports", "application/json", body); err != nil {
		return "", "", err
	}
	found, err := a.call(http.MethodGet, "/photos/photos?sha256="+url.QueryEscape(hash), "", nil)
	if err != nil {
		return "", "", err
	}
	var photos []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(found, &photos); err != nil || len(photos) != 1 {
		return "", "", fmt.Errorf("photo-store does not answer one photo for sha256 %s: %s", hash, found)
	}
	reviewBody, _ := json.Marshal(map[string]string{"photo_id": photos[0].ID, "queued_by": a.queuedBy, "reading": "inventory"})
	queued, err := a.call(http.MethodPost, "/possessions/photo-reviews", "application/json", reviewBody)
	if err != nil {
		return photos[0].ID, "", err
	}
	var review struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(queued, &review); err != nil {
		return photos[0].ID, "", err
	}
	return photos[0].ID, review.ID, nil
}

func (a adder) call(method, path, contentType string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequest(method, a.bridgeURL+path, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+a.token)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %d %s", method, path, response.StatusCode, bytes.TrimSpace(answer))
	}
	return answer, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "possession-add-photos:", err)
	os.Exit(1)
}
