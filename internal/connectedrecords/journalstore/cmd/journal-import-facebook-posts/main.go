// Command journal-import-facebook-posts imports the user's own posts from a
// Facebook data export (JSON format) into journal-store: one private journal
// entry per post, tagged facebook-post, dated when it was posted.
//
//	journal-import-facebook-posts -activity <export>/your_facebook_activity -self-name 'Me Self' \
//	    -people people.json -source-entry entry_000001 -dry-run
//
// -people maps a name as the export writes it to a people-store id, drawn as
// a chip for someone the post tagged. The source is facebook-posts and source_ref
// is "post/<unix time>/<n>", so running it again skips unchanged posts.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	journalstore "github.com/kayushkin/journal-store"
	"github.com/kayushkin/journal-store/internal/facebookexport"
	"github.com/kayushkin/journal-store/internal/importclient"
)

func main() {
	activityDir := flag.String("activity", "", "the export's your_facebook_activity folder")
	selfName := flag.String("self-name", "", "the user's own name as the export writes it")
	peoplePath := flag.String("people", "", "JSON object mapping names as the export writes them to people-store ids, drawn as chips")
	sourceEntry := flag.String("source-entry", "", "the journal-store entry that describes the export, linked from every post")
	source := flag.String("source", "facebook-posts", "the import source every post is filed under")
	storeURL := flag.String("store-url", "http://"+journalstore.DefaultListenAddress, "journal-store's base URL")
	dryRun := flag.Bool("dry-run", false, "ask the store what it would do and write nothing")
	batchSize := flag.Int("batch", 100, "posts per POST /imports request")
	flag.Parse()
	if *activityDir == "" || *selfName == "" || *batchSize < 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*activityDir, *selfName, *peoplePath, *sourceEntry, *source, *storeURL, *dryRun, *batchSize, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "journal-import-facebook-posts: %v\n", err)
		os.Exit(1)
	}
}

func run(activityDir, selfName, peoplePath, sourceEntry, source, storeURL string, dryRun bool, batchSize int, out io.Writer) error {
	people := map[string]string{}
	if peoplePath != "" {
		raw, err := os.ReadFile(peoplePath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &people); err != nil {
			return fmt.Errorf("-people %s: %w", peoplePath, err)
		}
	}
	posts, err := facebookexport.ReadPosts(filepath.Join(activityDir, facebookexport.PostsFile))
	if err != nil {
		return err
	}
	entries := make([]journalstore.ImportEntry, 0, len(posts))
	atSecond := map[int64]int{}
	for _, post := range posts {
		sameSecond := atSecond[post.Time.Unix()]
		atSecond[post.Time.Unix()]++
		title, body := post.Title(selfName), post.Body(sourceEntry, people)
		entries = append(entries, journalstore.ImportEntry{
			Source: source, SourceRef: facebookexport.SourceRef(post, sameSecond), SourceHash: facebookexport.Hash(title, body),
			Kind: journalstore.KindJournal, Visibility: journalstore.VisibilityPrivate,
			Title: title, Body: body, WrittenAt: post.Time.Unix(),
			Tags: []string{"facebook-post"},
		})
	}
	fmt.Fprintf(out, "read %d posts\n", len(posts))
	total, err := importclient.PostAll(storeURL, entries, batchSize, dryRun, func(journalstore.ImportOutcome) {})
	if err != nil {
		return err
	}
	verb := "imported"
	if dryRun {
		verb = "dry run, nothing written; would have"
	}
	fmt.Fprintf(out, "%s: created %d, updated %d, unchanged %d\n", verb, total.Created, total.Updated, total.Skipped)
	return nil
}
