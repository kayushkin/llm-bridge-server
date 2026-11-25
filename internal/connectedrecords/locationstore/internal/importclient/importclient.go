// Package importclient sends one import to location-store: it starts the
// import by the file's hash, posts records in batches, and finishes it. A dry
// run counts the records and posts nothing.
package importclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	locationstore "github.com/kayushkin/location-store"
)

// ErrAlreadyImported is Begin's answer when the store finished this same file
// before: there is nothing to send.
var ErrAlreadyImported = errors.New("this file was imported before")

// BatchSize is how many records go in one POST.
const BatchSize = 2000

// Client is one import in progress.
type Client struct {
	StoreURL      string
	Sha256        string
	SourceMachine string
	SourcePath    string
	DryRun        bool
	// Reapply sends a file the store finished before once more, so the store
	// learns which records the file holds.
	Reapply bool
	// snapshot, when the file is a full snapshot of a span, goes with Finish.
	snapshot *locationstore.ImportSnapshot

	http     *http.Client
	importID string
	batch    locationstore.ImportBatch
	pending  int
	// sentPlaces is each place already sent as it was sent, so a place a
	// thousand visits name goes once, and again only when it says more.
	sentPlaces map[string]locationstore.ImportPlace
	// Read counts the records read, by kind.
	Read map[string]int
	// Written sums what the store did.
	Written locationstore.ImportCounts
}

// New makes a client for one import.
func New(storeURL, sha256, sourceMachine, sourcePath string, dryRun bool) *Client {
	return &Client{StoreURL: strings.TrimRight(storeURL, "/"), Sha256: sha256, SourceMachine: sourceMachine, SourcePath: sourcePath,
		DryRun: dryRun, http: &http.Client{Timeout: 10 * time.Minute}, sentPlaces: map[string]locationstore.ImportPlace{},
		Read: map[string]int{}}
}

// HashFile is the sha256 of a file, which an import is found again by.
func HashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// ImportID is the store's id for this import, once begun.
func (c *Client) ImportID() string { return c.importID }

func (c *Client) post(path string, body, answer any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	response, err := c.http.Post(c.StoreURL+path, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: location-store answered %d: %s", path, response.StatusCode, bytes.TrimSpace(content))
	}
	return json.Unmarshal(content, answer)
}

// Begin starts the import, or finds the one already started for this file.
func (c *Client) Begin(kind string) error {
	if c.DryRun {
		c.importID = "dry-run"
		return nil
	}
	var started locationstore.ImportStarted
	if err := c.post("/imports", locationstore.ImportStart{Kind: kind, Sha256: c.Sha256, SourceMachine: c.SourceMachine,
		SourcePath: c.SourcePath, Reapply: c.Reapply}, &started); err != nil {
		return err
	}
	c.importID = started.Import.ID
	if started.AlreadyFinished {
		return fmt.Errorf("%w as %s", ErrAlreadyImported, started.Import.ID)
	}
	return nil
}

func (c *Client) added() error {
	c.pending++
	if c.pending >= BatchSize {
		return c.Flush()
	}
	return nil
}

// Place queues a place, unless the same place was sent already.
func (c *Client) Place(place locationstore.ImportPlace) error {
	if sent, found := c.sentPlaces[place.GooglePlaceID]; found && sent == place {
		return nil
	}
	c.sentPlaces[place.GooglePlaceID] = place
	c.Read["places"]++
	c.batch.Places = append(c.batch.Places, place)
	return c.added()
}

// Visit queues a visit.
func (c *Client) Visit(visit locationstore.ImportVisit) error {
	c.Read["visits"]++
	c.batch.Visits = append(c.batch.Visits, visit)
	return c.added()
}

// Trip queues a trip; its path counts toward the batch's size.
func (c *Client) Trip(trip locationstore.ImportTrip) error {
	c.Read["trips"]++
	c.batch.Trips = append(c.batch.Trips, trip)
	c.pending += len(trip.Path)
	return c.added()
}

// Point queues a raw point.
func (c *Client) Point(point locationstore.ImportPoint) error {
	c.Read["points"]++
	c.batch.Points = append(c.batch.Points, point)
	return c.added()
}

// Snapshot keeps the span a full-snapshot export covers, for Finish.
func (c *Client) Snapshot(snapshot locationstore.ImportSnapshot) error {
	c.snapshot = &snapshot
	return nil
}

// SnapshotRead is what Snapshot kept; nil for a file that is not a snapshot.
func (c *Client) SnapshotRead() *locationstore.ImportSnapshot { return c.snapshot }

// Flush posts what is queued.
func (c *Client) Flush() error {
	if c.pending == 0 {
		return nil
	}
	batch := c.batch
	c.batch, c.pending = locationstore.ImportBatch{}, 0
	if c.DryRun {
		return nil
	}
	if c.importID == "" {
		return errors.New("records came before the import began")
	}
	var counts locationstore.ImportCounts
	if err := c.post("/imports/"+c.importID+"/batches", batch, &counts); err != nil {
		return fmt.Errorf("import %s: %w (the batches before it were written; run the same command again to resume)", c.importID, err)
	}
	c.Written.PointsAdded += counts.PointsAdded
	c.Written.PointsUnchanged += counts.PointsUnchanged
	c.Written.PlacesCreated += counts.PlacesCreated
	c.Written.PlacesUpdated += counts.PlacesUpdated
	c.Written.VisitsCreated += counts.VisitsCreated
	c.Written.VisitsUpdated += counts.VisitsUpdated
	c.Written.TripsCreated += counts.TripsCreated
	c.Written.TripsUpdated += counts.TripsUpdated
	c.Written.RecordsUnchanged += counts.RecordsUnchanged
	return nil
}

// Finish posts what is left and marks the import finished.
func (c *Client) Finish() (*locationstore.Import, error) {
	if err := c.Flush(); err != nil {
		return nil, err
	}
	if c.DryRun {
		return nil, nil
	}
	var record locationstore.Import
	if err := c.post("/imports/"+c.importID+"/finish", locationstore.ImportFinish{Snapshot: c.snapshot}, &record); err != nil {
		return nil, err
	}
	return &record, nil
}
