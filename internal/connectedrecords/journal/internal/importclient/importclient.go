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

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/journal"
)

// DefaultStoreURL is the journal-store the commands call when no URL flag is
// given: where the standalone store listened, so a command behaves as it did
// before the store moved into llm-bridge-server. Pass the URL flag to call it
// elsewhere.
const DefaultStoreURL = "http://127.0.0.1:8322"

// Post sends one batch. With dryRun the store counts and writes nothing.
func Post(storeURL string, entries []journal.ImportEntry, dryRun bool) (*journal.ImportResult, error) {
	payload, err := json.Marshal(journal.ImportRequest{Entries: entries})
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
	var result journal.ImportResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("journal-store's answer is not an import result: %w", err)
	}
	return &result, nil
}

// PostAll sends entries in batches and adds up the results, printing each
// outcome with print. A failed batch stops the run; say tells the caller
// whether the batches before it were written.
func PostAll(storeURL string, entries []journal.ImportEntry, batchSize int, dryRun bool, print func(journal.ImportOutcome)) (journal.ImportResult, error) {
	var total journal.ImportResult
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
