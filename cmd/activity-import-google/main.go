// Command activity-import-google imports what a Google Takeout export says the
// user did online into activity-store: every My Activity page
// (My Activity/<Product>/MyActivity.html, with the voice recordings and
// pictures its cards name), YouTube's watch and search history, Chrome's
// History.json and Chrome's bookmarks. Each file is an import of its own,
// found again by its hash, so importing the same export again changes
// nothing, and an import that stopped part way resumes.
//
//	activity-import-google -dir <takeout dir> [-source-machine <name>] [-dry-run]
package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	activitystore "github.com/kayushkin/activity-store"
	"github.com/kayushkin/activity-store/internal/importclient"
	"github.com/kayushkin/activity-store/internal/takeout"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// youTubeProduct is what YouTube's own history is filed under, as My
// Activity names the product.
const youTubeProduct = "YouTube"

type importer struct {
	storeURL, machine string
	dryRun            bool
	// named is every attachment file a card named, so the files no card
	// names can be reported as left out.
	named map[string]bool
}

func main() {
	directory := flag.String("dir", "", "the Takeout directory: the one holding My Activity, Chrome and YouTube and YouTube Music (required)")
	sourceMachine := flag.String("source-machine", "", "the machine the export was read on, recorded on each import (default: this host's name)")
	dryRun := flag.Bool("dry-run", false, "read and count, post nothing")
	flag.Parse()
	if *directory == "" {
		flag.Usage()
		os.Exit(2)
	}
	settings, err := activitystore.NewImportGoogleSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	machine := *sourceMachine
	if machine == "" {
		if machine, err = os.Hostname(); err != nil {
			log.Fatalf("no -source-machine and no host name: %v", err)
		}
	}
	root, err := filepath.Abs(*directory)
	if err != nil {
		log.Fatal(err)
	}
	run := &importer{storeURL: settings.String(activitystore.SettingActivityStoreURL), machine: machine, dryRun: *dryRun, named: map[string]bool{}}
	found := 0

	myActivity := filepath.Join(root, "My Activity")
	if entries, err := os.ReadDir(myActivity); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			pages, err := filepath.Glob(filepath.Join(myActivity, entry.Name(), "*.html"))
			if err != nil {
				log.Fatal(err)
			}
			sort.Strings(pages)
			for _, page := range pages {
				found++
				if err := run.myActivityPage(page, activitystore.ImportKindMyActivityHTML,
					takeout.MyActivityPage{Product: entry.Name(), Source: activitystore.SourceGoogleMyActivity}); err != nil {
					log.Fatalf("%s: %v", page, err)
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Fatal(err)
	}

	youTubePages, err := filepath.Glob(filepath.Join(root, "YouTube and YouTube Music", "history", "*.html"))
	if err != nil {
		log.Fatal(err)
	}
	sort.Strings(youTubePages)
	for _, page := range youTubePages {
		found++
		if err := run.myActivityPage(page, activitystore.ImportKindYouTubeHistoryHTML,
			takeout.MyActivityPage{Product: youTubeProduct, Source: activitystore.SourceYouTubeHistory}); err != nil {
			log.Fatalf("%s: %v", page, err)
		}
	}

	history := filepath.Join(root, "Chrome", "History.json")
	if _, err := os.Stat(history); err == nil {
		found++
		if err := run.chromeHistory(history); err != nil {
			log.Fatalf("%s: %v", history, err)
		}
	}
	bookmarks := filepath.Join(root, "Chrome", "Bookmarks.html")
	if _, err := os.Stat(bookmarks); err == nil {
		found++
		if err := run.chromeBookmarks(bookmarks); err != nil {
			log.Fatalf("%s: %v", bookmarks, err)
		}
	}
	if found == 0 {
		log.Fatalf("%s holds no My Activity pages, YouTube history, Chrome/History.json or Chrome/Bookmarks.html", root)
	}
	run.reportUnnamedFiles(myActivity)
}

// send posts one file's records as one import.
func (run *importer) send(path, kind string, records []takeout.Record, attach func(takeout.Record) ([]activitystore.ImportAttachment, error)) error {
	sha, err := importclient.HashFile(path)
	if err != nil {
		return err
	}
	client := importclient.New(run.storeURL, sha, run.machine, path, run.dryRun)
	if err := client.Begin(kind); err != nil {
		if errors.Is(err, importclient.ErrAlreadyImported) {
			fmt.Printf("%s: %v; nothing to send\n", path, err)
			return nil
		}
		return err
	}
	actions := map[string]int{}
	var first, last int64
	for _, record := range records {
		activity := record.Activity
		if attach != nil {
			if activity.Attachments, err = attach(record); err != nil {
				return err
			}
		}
		actions[activity.Action]++
		if first == 0 || activity.OccurredAt < first {
			first = activity.OccurredAt
		}
		if activity.OccurredAt > last {
			last = activity.OccurredAt
		}
		if err := client.Add(activity); err != nil {
			return err
		}
	}
	finished, err := client.Finish()
	if err != nil {
		return err
	}
	span := "no records"
	if len(records) > 0 {
		span = time.Unix(first, 0).UTC().Format("2006-01-02") + " to " + time.Unix(last, 0).UTC().Format("2006-01-02")
	}
	if run.dryRun {
		fmt.Printf("%s: would send %d records (%s), %s\n", path, client.Read, span, describeCounts(actions))
		return nil
	}
	fmt.Printf("%s: %s sent %d records (%s): %d created, %d folded into activities already held, %d known, %d attachments stored\n",
		path, finished.ID, client.Read, span, client.Written.Created, client.Written.Folded, client.Written.Known, client.Written.AttachmentsStored)
	return nil
}

func describeCounts(counts map[string]int) string {
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return counts[names[i]] > counts[names[j]] })
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s %d", name, counts[name]))
	}
	return strings.Join(parts, ", ")
}

func (run *importer) myActivityPage(path, kind string, about takeout.MyActivityPage) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	records, err := takeout.ReadMyActivity(file, about)
	file.Close()
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	for _, record := range records {
		for _, name := range record.AttachmentFiles {
			run.named[filepath.Join(directory, name)] = true
		}
	}
	return run.send(path, kind, records, func(record takeout.Record) ([]activitystore.ImportAttachment, error) {
		var attachments []activitystore.ImportAttachment
		for _, name := range record.AttachmentFiles {
			if name != filepath.Base(name) || name == ".." {
				return nil, fmt.Errorf("a card names the file %q, which is not beside the page", name)
			}
			filePath := filepath.Join(directory, name)
			content, err := os.ReadFile(filePath)
			if err != nil {
				return nil, fmt.Errorf("a card names %s: %w", name, err)
			}
			contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
			if contentType == "" {
				return nil, fmt.Errorf("a card names %s, whose type this importer cannot tell from its extension", name)
			}
			attachments = append(attachments, activitystore.ImportAttachment{FileName: name, ContentType: contentType,
				ContentBase64: base64.StdEncoding.EncodeToString(content)})
		}
		return attachments, nil
	})
}

func (run *importer) chromeHistory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	records, err := takeout.ReadChromeHistory(file)
	file.Close()
	if err != nil {
		return err
	}
	return run.send(path, activitystore.ImportKindChromeHistoryJSON, records, nil)
}

func (run *importer) chromeBookmarks(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	read, err := takeout.ReadChromeBookmarks(file)
	file.Close()
	if err != nil {
		return err
	}
	if read.Undated > 0 {
		fmt.Printf("%s: %d bookmarks have no date added and are left out\n", path, read.Undated)
	}
	return run.send(path, activitystore.ImportKindChromeBookmarksHTML, read.Records, nil)
}

// reportUnnamedFiles names the files beside the My Activity pages that no
// card names: they belong to no activity, so they are not imported.
func (run *importer) reportUnnamedFiles(myActivity string) {
	files, err := filepath.Glob(filepath.Join(myActivity, "*", "*"))
	if err != nil {
		log.Fatal(err)
	}
	var unnamed []string
	for _, path := range files {
		if strings.EqualFold(filepath.Ext(path), ".html") || run.named[path] {
			continue
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			unnamed = append(unnamed, path)
		}
	}
	if len(unnamed) > 0 {
		fmt.Printf("%d files beside the My Activity pages are named by no card and were left out:\n", len(unnamed))
		for _, path := range unnamed {
			fmt.Printf("    %s\n", path)
		}
	}
}
