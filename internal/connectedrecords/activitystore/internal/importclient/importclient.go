// Package importclient sends one import to activity-store: it starts the
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

	activitystore "github.com/kayushkin/activity-store"
)

// ErrAlreadyImported is Begin's answer when the store finished this same file
// before: there is nothing to send.
var ErrAlreadyImported = errors.New("this file was imported before")

// BatchSize is how many records go in one POST.
const BatchSize = 2000

// batchAttachmentBytes flushes a batch early once its attachments reach this
// many bytes, before base64.
const batchAttachmentBytes = 32 << 20

// Client is one import in progress.
type Client struct {
	StoreURL      string
	Sha256        string
	SourceMachine string
	SourcePath    string
	DryRun        bool

	http            *http.Client
	importID        string
	batch           activitystore.ImportBatch
	attachmentBytes int
	// Read counts the records read; Written sums what the store did.
	Read    int
	Written activitystore.ImportCounts
}

// New makes a client for one import.
func New(storeURL, sha256, sourceMachine, sourcePath string, dryRun bool) *Client {
	return &Client{StoreURL: strings.TrimRight(storeURL, "/"), Sha256: sha256, SourceMachine: sourceMachine, SourcePath: sourcePath,
		DryRun: dryRun, http: &http.Client{Timeout: 10 * time.Minute}}
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
		return fmt.Errorf("POST %s: activity-store answered %d: %s", path, response.StatusCode, bytes.TrimSpace(content))
	}
	return json.Unmarshal(content, answer)
}

// Begin starts the import, or finds the one already started for this file.
func (c *Client) Begin(kind string) error {
	if c.DryRun {
		c.importID = "dry-run"
		return nil
	}
	var started activitystore.ImportStarted
	if err := c.post("/imports", activitystore.ImportStart{Kind: kind, Sha256: c.Sha256, SourceMachine: c.SourceMachine,
		SourcePath: c.SourcePath}, &started); err != nil {
		return err
	}
	c.importID = started.Import.ID
	if started.AlreadyFinished {
		return fmt.Errorf("%w as %s", ErrAlreadyImported, started.Import.ID)
	}
	return nil
}

// Add queues one record, and sends the batch when it is full.
func (c *Client) Add(activity activitystore.ImportActivity) error {
	c.Read++
	c.batch.Activities = append(c.batch.Activities, activity)
	for _, attachment := range activity.Attachments {
		c.attachmentBytes += len(attachment.ContentBase64) * 3 / 4
	}
	if len(c.batch.Activities) >= BatchSize || c.attachmentBytes >= batchAttachmentBytes {
		return c.Flush()
	}
	return nil
}

// Flush sends what is queued.
func (c *Client) Flush() error {
	if len(c.batch.Activities) == 0 {
		return nil
	}
	if !c.DryRun {
		var counts activitystore.ImportCounts
		if err := c.post("/imports/"+c.importID+"/batches", c.batch, &counts); err != nil {
			return err
		}
		c.Written.Created += counts.Created
		c.Written.Folded += counts.Folded
		c.Written.Known += counts.Known
		c.Written.AttachmentsStored += counts.AttachmentsStored
	}
	c.batch = activitystore.ImportBatch{}
	c.attachmentBytes = 0
	return nil
}

// Finish sends what is left and stamps the import finished.
func (c *Client) Finish() (activitystore.Import, error) {
	if err := c.Flush(); err != nil {
		return activitystore.Import{}, err
	}
	if c.DryRun {
		return activitystore.Import{ID: c.importID}, nil
	}
	var finished activitystore.Import
	err := c.post("/imports/"+c.importID+"/finish", struct{}{}, &finished)
	return finished, err
}
