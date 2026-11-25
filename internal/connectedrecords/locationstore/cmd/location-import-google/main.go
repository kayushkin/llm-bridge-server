// Command location-import-google imports Google location history into
// location-store: the on-device Timeline export (Timeline.json), legacy
// Takeout Records.json, or legacy Semantic Location History month files. Give
// it one file, or a directory to import every .json file under it, each as an
// import of its own. Importing the same file again changes nothing; an import
// that stopped part way resumes.
//
//	location-import-google -file <path> [-source-machine <name>] [-dry-run]
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	locationstore "github.com/kayushkin/location-store"
	"github.com/kayushkin/location-store/internal/googletimeline"
	"github.com/kayushkin/location-store/internal/importclient"
)

func main() {
	path := flag.String("file", "", "a Google export file, or a directory of them (required)")
	sourceMachine := flag.String("source-machine", "", "the machine the file was read on, recorded on the import (default: this host's name)")
	dryRun := flag.Bool("dry-run", false, "read and count, post nothing")
	reapply := flag.Bool("reapply", false, "send a file imported before once more, so the store records which visits and trips it holds and reconciles the snapshots")
	flag.Parse()
	if *path == "" {
		flag.Usage()
		os.Exit(2)
	}
	settings, err := locationstore.NewImportGoogleSettingsRegistry(servicesettings.ProcessEnvironment())
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
	storeURL := settings.String(locationstore.SettingLocationStoreURL)

	info, err := os.Stat(*path)
	if err != nil {
		log.Fatal(err)
	}
	if !info.IsDir() {
		if err := importFile(storeURL, *path, machine, *dryRun, *reapply); err != nil {
			log.Fatalf("%s: %v", *path, err)
		}
		return
	}
	var files []string
	err = filepath.WalkDir(*path, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(file), ".json") {
			files = append(files, file)
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
	sort.Strings(files)
	failed := 0
	for _, file := range files {
		err := importFile(storeURL, file, machine, *dryRun, *reapply)
		switch {
		case errors.Is(err, googletimeline.ErrNotLocationHistory):
			fmt.Printf("%s: skipped: not a location history file\n", file)
		case err != nil:
			fmt.Printf("%s: FAILED: %v\n", file, err)
			failed++
		}
	}
	if failed > 0 {
		log.Fatalf("%d of %d files failed", failed, len(files))
	}
}

func importFile(storeURL, path, machine string, dryRun, reapply bool) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	hash, err := importclient.HashFile(absolute)
	if err != nil {
		return err
	}
	file, err := os.Open(absolute)
	if err != nil {
		return err
	}
	defer file.Close()
	client := importclient.New(storeURL, hash, machine, absolute, dryRun)
	client.Reapply = reapply
	sink := &stoppingSink{Client: client}
	report, err := googletimeline.Parse(file, sink)
	if err != nil {
		return err
	}
	if sink.alreadyImported {
		fmt.Printf("%s: already imported as %s; nothing to do\n", path, client.ImportID())
		return nil
	}
	record, err := client.Finish()
	if err != nil {
		return err
	}
	fmt.Printf("%s: read %s\n", path, describeCounts(client.Read))
	for reason, count := range report {
		fmt.Printf("  left out %d: %s\n", count, reason)
	}
	if record == nil {
		if snapshot := client.SnapshotRead(); snapshot != nil {
			fmt.Printf("  a full snapshot of %s to %s (raw positions %s to %s)\n", describeTime(snapshot.CoversFrom),
				describeTime(snapshot.CoversTo), describeTime(snapshot.RawSignalsOldestAt), describeTime(snapshot.RawSignalsNewestAt))
		}
		fmt.Println("  dry run: nothing posted")
		return nil
	}
	written, err := json.Marshal(record.Counts)
	if err != nil {
		return err
	}
	fmt.Printf("  %s finished: %s\n", record.ID, written)
	if record.Snapshot != nil {
		fmt.Printf("  a full snapshot of %s to %s (raw positions %s to %s)\n", describeTime(record.Snapshot.CoversFrom),
			describeTime(record.Snapshot.CoversTo), describeTime(record.Snapshot.RawSignalsOldestAt), describeTime(record.Snapshot.RawSignalsNewestAt))
	}
	if record.SnapshotChanges != nil {
		changes, err := json.Marshal(record.SnapshotChanges)
		if err != nil {
			return err
		}
		fmt.Printf("  snapshots reconciled: %s\n", changes)
	}
	return nil
}

// stoppingSink ends the read quietly when the store already has the file.
type stoppingSink struct {
	*importclient.Client
	alreadyImported bool
}

func (sink *stoppingSink) Begin(kind string) error {
	err := sink.Client.Begin(kind)
	if errors.Is(err, importclient.ErrAlreadyImported) {
		sink.alreadyImported = true
		return googletimeline.ErrStop
	}
	return err
}

func describeCounts(read map[string]int) string {
	if len(read) == 0 {
		return "nothing"
	}
	var parts []string
	for _, kind := range []string{"places", "visits", "trips", "points"} {
		if read[kind] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", read[kind], kind))
		}
	}
	return strings.Join(parts, ", ")
}

func describeTime(at int64) string {
	if at == 0 {
		return "none"
	}
	return time.Unix(at, 0).UTC().Format(time.RFC3339)
}
