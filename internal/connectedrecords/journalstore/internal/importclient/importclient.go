// Package importclient posts entries to journal-store's POST /imports in
// batches, for the import commands.
package importclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	journalstore "github.com/kayushkin/journal-store"
)

// Post sends one batch. With dryRun the store counts and writes nothing.
func Post(storeURL string, entries []journalstore.ImportEntry, dryRun bool) (*journalstore.ImportResult, error) {
	payload, err := json.Marshal(journalstore.ImportRequest{Entries: entries})
	if err != nil {
		return nil, err
	}
	url := storeURL + "/imports"
	if dryRun {
		url += "?dry_run=true"
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	response, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("journal-store answered %d: %s", response.StatusCode, bytes.TrimSpace(body))
	}
	var result journalstore.ImportResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("journal-store's answer is not an import result: %w", err)
	}
	return &result, nil
}

// PostAll sends entries in batches and adds up the results, printing each
// outcome with print. A failed batch stops the run; say tells the caller
// whether the batches before it were written.
func PostAll(storeURL string, entries []journalstore.ImportEntry, batchSize int, dryRun bool, print func(journalstore.ImportOutcome)) (journalstore.ImportResult, error) {
	var total journalstore.ImportResult
	for start := 0; start < len(entries); start += batchSize {
		batch := entries[start:min(start+batchSize, len(entries))]
		result, err := Post(storeURL, batch, dryRun)
		if err != nil {
			return total, fmt.Errorf("entries %d–%d of %d: %w (the batches before it were %s)",
				start+1, start+len(batch), len(entries), err, map[bool]string{true: "not written: this is a dry run", false: "written"}[dryRun])
		}
		for _, outcome := range result.Outcomes {
			print(outcome)
		}
		total.Created += result.Created
		total.Updated += result.Updated
		total.Skipped += result.Skipped
	}
	return total, nil
}
