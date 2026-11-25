// Package importbatch gathers photos for photo-store's POST /imports: it
// hashes each original, makes its thumbnail, drops a picture seen twice,
// counts what it skipped and why, and posts the photos in batches.
package importbatch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	photostore "github.com/kayushkin/photo-store"
	"github.com/kayushkin/photo-store/thumbnail"
)

// Batch is the photos read so far.
type Batch struct {
	Photos []photostore.ImportPhoto
	// Report counts what was skipped, by reason.
	Report map[string]int
	seen   map[string]bool
}

// New is an empty batch.
func New() *Batch {
	return &Batch{Report: map[string]int{}, seen: map[string]bool{}}
}

// Add reads one original: it is skipped, and counted, when the same picture
// came before or it does not read as an image.
func (in *Batch) Add(original []byte, mediaType, sourceMachine, sourcePath, folder string, takenAt time.Time, dateSource string) {
	digest := sha256.Sum256(original)
	hash := hex.EncodeToString(digest[:])
	if in.seen[hash] {
		in.Report["the same picture again"]++
		return
	}
	thumb, width, height, err := thumbnail.Make(original)
	if err != nil {
		in.Report["not readable as an image"]++
		return
	}
	in.seen[hash] = true
	in.Photos = append(in.Photos, photostore.ImportPhoto{Sha256: hash, SourceMachine: sourceMachine, SourcePath: sourcePath,
		Folder: folder, MediaType: mediaType, TakenAt: takenAt.Unix(), DateSource: dateSource,
		Width: width, Height: height, ThumbnailJPEG: thumb})
}

// Post prints the report and sends the photos to photo-store, 50 at a time.
func (in *Batch) Post(storeURL string, dryRun bool, out io.Writer) error {
	for reason, count := range in.Report {
		fmt.Fprintf(out, "skipped %d: %s\n", count, reason)
	}
	fmt.Fprintf(out, "read %d photos\n", len(in.Photos))
	if dryRun {
		fmt.Fprintln(out, "dry run: nothing posted")
		return nil
	}
	var total photostore.ImportResult
	client := &http.Client{Timeout: 5 * time.Minute}
	for start := 0; start < len(in.Photos); start += 50 {
		batch := in.Photos[start:min(start+50, len(in.Photos))]
		payload, err := json.Marshal(photostore.ImportRequest{Photos: batch})
		if err != nil {
			return err
		}
		response, err := client.Post(storeURL+"/imports", "application/json", bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("photos %d–%d: %w (the batches before it were written)", start+1, start+len(batch), err)
		}
		answer, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("photos %d–%d: photo-store answered %d: %s", start+1, start+len(batch), response.StatusCode, bytes.TrimSpace(answer))
		}
		var result photostore.ImportResult
		if err := json.Unmarshal(answer, &result); err != nil {
			return err
		}
		total.Created += result.Created
		total.Updated += result.Updated
		total.Skipped += result.Skipped
	}
	fmt.Fprintf(out, "imported: created %d, updated %d, unchanged %d\n", total.Created, total.Updated, total.Skipped)
	return nil
}
