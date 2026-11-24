package workgraphstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Ingester moves the hook's spool into the store.
//
// The hook appends; the ingester renames the spool aside, waits for any hook
// that opened it before the rename to finish its one write, reads the whole
// file into one transaction, and deletes it. A spool left aside by a crash is
// read first on the next pass, so nothing is read twice or lost.
type Ingester struct {
	Store     *Store
	SpoolPath string
	RepoStore *RepoStoreClient
	// Interval is the pause between passes.
	Interval time.Duration
	// SettleDelay is how long a renamed spool sits before it is read.
	SettleDelay time.Duration
}

// Run ingests until ctx ends. A failed pass is logged and tried again.
func (ingester *Ingester) Run(ctx context.Context) {
	for {
		if err := ingester.Pass(ctx); err != nil {
			log.Printf("ingest %s: %v", ingester.SpoolPath, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(ingester.Interval):
		}
	}
}

func (ingester *Ingester) asidePath() string { return ingester.SpoolPath + ".ingesting" }

// Pass ingests what the spool holds now.
func (ingester *Ingester) Pass(ctx context.Context) error {
	if _, err := os.Stat(ingester.asidePath()); errors.Is(err, fs.ErrNotExist) {
		if err := os.Rename(ingester.SpoolPath, ingester.asidePath()); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ingester.SettleDelay):
		}
	} else if err != nil {
		return err
	}

	content, err := os.ReadFile(ingester.asidePath())
	if err != nil {
		return err
	}
	var updates []SpooledRefUpdate
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var update SpooledRefUpdate
		if err := json.Unmarshal(line, &update); err != nil {
			// A line the hook did not write whole cannot be repaired; say so
			// and keep the rest.
			log.Printf("ingest: line %d of %s is not a ref update, skipped: %v", lineNumber, ingester.asidePath(), err)
			continue
		}
		updates = append(updates, update)
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	repoStoreIDs, err := ingester.RepoStore.IDsByPath(ctx)
	if err != nil {
		return fmt.Errorf("ask repo-store which repos it knows: %w", err)
	}
	if err := ingester.Store.InsertRefUpdates(updates, repoStoreIDs); err != nil {
		return err
	}
	return os.Remove(ingester.asidePath())
}

// Repo is the part of a repo-store repo this service reads.
type Repo struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
	// GitHubURL is repo-store's github_url: the repo's page on GitHub, or ""
	// when its remote is not there.
	GitHubURL string `json:"github_url,omitempty"`
}

// RepoStoreClient asks repo-store about repos.
type RepoStoreClient struct {
	BaseURL string
	HTTP    *http.Client
}

// Repos lists every repo repo-store knows.
func (client *RepoStoreClient) Repos(ctx context.Context) ([]Repo, error) {
	var repos []Repo
	return repos, client.get(ctx, "/repos", &repos)
}

// Repo returns one repo by id, or an error wrapping ErrNotFound.
func (client *RepoStoreClient) Repo(ctx context.Context, id int64) (Repo, error) {
	var repo Repo
	return repo, client.get(ctx, "/repos/"+strconv.FormatInt(id, 10), &repo)
}

func (client *RepoStoreClient) get(ctx context.Context, path string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.BaseURL+path, nil)
	if err != nil {
		return err
	}
	response, err := client.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("repo-store GET %s: %w", path, ErrNotFound)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("repo-store GET %s: %s", path, response.Status)
	}
	if err := json.NewDecoder(response.Body).Decode(into); err != nil {
		return fmt.Errorf("repo-store GET %s: %w", path, err)
	}
	return nil
}

// IDsByPath maps each repo's path to its id.
func (client *RepoStoreClient) IDsByPath(ctx context.Context) (map[string]int64, error) {
	repos, err := client.Repos(ctx)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]int64, len(repos))
	for _, repo := range repos {
		if repo.Path != "" {
			ids[repo.Path] = repo.ID
		}
	}
	return ids, nil
}
