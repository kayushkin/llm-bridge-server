// Command photo-import imports a directory prepared on the machine that holds
// the originals: index.jsonl, one line per photo (the fields of ImportPhoto,
// with "thumbnail" naming its JPEG relative to the directory instead of the
// bytes), and the thumbnails themselves.
//
//	photo-import -dir <prepared directory> -dry-run
//
// The import is by the original's sha256, so running it again skips every
// photo whose facts did not change and never touches a description.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	photostore "github.com/kayushkin/photo-store"
)

// indexLine is one line of index.jsonl.
type indexLine struct {
	photostore.ImportPhoto
	Thumbnail string `json:"thumbnail"`
}

func main() {
	dir := flag.String("dir", "", "the directory holding index.jsonl and the thumbnails")
	storeURL := flag.String("store-url", "http://"+photostore.DefaultListenAddress, "photo-store's base URL")
	batchSize := flag.Int("batch", 50, "photos per POST /imports")
	dryRun := flag.Bool("dry-run", false, "read and check everything, post nothing")
	flag.Parse()
	if *dir == "" || *batchSize < 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*dir, *storeURL, *batchSize, *dryRun, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "photo-import: %v\n", err)
		os.Exit(1)
	}
}

func run(dir, storeURL string, batchSize int, dryRun bool, out io.Writer) error {
	file, err := os.Open(filepath.Join(dir, "index.jsonl"))
	if err != nil {
		return err
	}
	defer file.Close()
	var photos []photostore.ImportPhoto
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	line := 0
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var entry indexLine
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&entry); err != nil {
			return fmt.Errorf("index.jsonl line %d: %w", line, err)
		}
		if entry.Thumbnail == "" || strings.Contains(entry.Thumbnail, "..") || filepath.IsAbs(entry.Thumbnail) {
			return fmt.Errorf("index.jsonl line %d: thumbnail must be a path inside the directory, got %q", line, entry.Thumbnail)
		}
		thumbnail, err := os.ReadFile(filepath.Join(dir, entry.Thumbnail))
		if err != nil {
			return fmt.Errorf("index.jsonl line %d: %w", line, err)
		}
		entry.ImportPhoto.ThumbnailJPEG = thumbnail
		photos = append(photos, entry.ImportPhoto)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	fmt.Fprintf(out, "read %d photos from %s\n", len(photos), dir)
	if dryRun {
		fmt.Fprintln(out, "dry run: nothing posted")
		return nil
	}
	var total photostore.ImportResult
	client := &http.Client{Timeout: 5 * time.Minute}
	for start := 0; start < len(photos); start += batchSize {
		batch := photos[start:min(start+batchSize, len(photos))]
		payload, err := json.Marshal(photostore.ImportRequest{Photos: batch})
		if err != nil {
			return err
		}
		response, err := client.Post(storeURL+"/imports", "application/json", bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("photos %d–%d: %w (the batches before it were written)", start+1, start+len(batch), err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("photos %d–%d: photo-store answered %d: %s (the batches before it were written)", start+1, start+len(batch), response.StatusCode, bytes.TrimSpace(body))
		}
		var result photostore.ImportResult
		if err := json.Unmarshal(body, &result); err != nil {
			return err
		}
		total.Created += result.Created
		total.Updated += result.Updated
		total.Skipped += result.Skipped
	}
	fmt.Fprintf(out, "imported: created %d, updated %d, unchanged %d\n", total.Created, total.Updated, total.Skipped)
	return nil
}
