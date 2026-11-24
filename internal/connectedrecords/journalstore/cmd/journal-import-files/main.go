// Command journal-import-files imports a directory of exported notes into
// journal-store as private notes, one entry per file.
//
//	journal-import-files -time-zone Europe/Lisbon -dry-run <export directory>
//	journal-import-files -time-zone Europe/Lisbon <export directory>
//
// Each .txt, .md, .docx (and .pdf, when pdftotext is installed) becomes kind
// note, visibility private, source samsung-notes, source_ref the path under
// the directory, title the note's title from a Samsung Notes file name or
// else its first line or file name, tags ["samsung-notes"], and written_at
// the time in a Samsung Notes file name, or else the first date in its first
// lines, or else the file's modification time. Running it again skips every file whose bytes did not
// change and updates the ones that did. -dry-run asks the store what it would
// do and writes nothing.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	journalstore "github.com/kayushkin/journal-store"
	"github.com/kayushkin/journal-store/internal/fileimport"
	"github.com/kayushkin/journal-store/internal/importclient"
)

func main() {
	storeURL := flag.String("store-url", "http://"+journalstore.DefaultListenAddress, "journal-store's base URL")
	source := flag.String("source", "samsung-notes", "the source every entry is imported under; also its one tag")
	dryRun := flag.Bool("dry-run", false, "print what would be imported and ask the store what it would do, writing nothing")
	batchSize := flag.Int("batch", 100, "entries per POST /imports request")
	timeZone := flag.String("time-zone", "Local", "IANA time zone the notes were written in (Europe/Lisbon); a time in a file name or a date in the text is read in it")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [flags] <directory>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 || *batchSize < 1 {
		flag.Usage()
		os.Exit(2)
	}
	location, err := time.LoadLocation(*timeZone)
	if err != nil {
		fmt.Fprintf(os.Stderr, "journal-import-files: -time-zone: %v\n", err)
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *storeURL, *source, *dryRun, *batchSize, location, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "journal-import-files: %v\n", err)
		os.Exit(1)
	}
}

func run(directory, storeURL, source string, dryRun bool, batchSize int, location *time.Location, out io.Writer) error {
	info, err := os.Stat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", directory)
	}
	files, skipped, err := fileimport.Scan(directory, location)
	if err != nil {
		return err
	}
	for _, skip := range skipped {
		fmt.Fprintf(out, "skip   %s (%s)\n", skip.RelativePath, skip.Reason)
	}
	entries := make([]journalstore.ImportEntry, 0, len(files))
	for _, file := range files {
		fmt.Fprintf(out, "read   %s  %s (%s)  %q  %d bytes\n",
			file.RelativePath, file.WrittenAt.Format("2006-01-02 15:04"), file.DateSource, file.Title, len(file.Body))
		entries = append(entries, journalstore.ImportEntry{
			Source: source, SourceRef: file.RelativePath, SourceHash: file.Hash,
			Kind: journalstore.KindNote, Visibility: journalstore.VisibilityPrivate,
			Title: file.Title, Body: file.Body, WrittenAt: file.WrittenAt.Unix(), Tags: []string{source},
		})
	}
	if len(entries) == 0 {
		fmt.Fprintf(out, "nothing to import: %d files skipped\n", len(skipped))
		return nil
	}

	total, err := importclient.PostAll(storeURL, entries, batchSize, dryRun, func(outcome journalstore.ImportOutcome) {
		fmt.Fprintf(out, "%-7s %s → %s\n", outcome.Outcome, outcome.SourceRef, outcome.EntryID)
	})
	if err != nil {
		return err
	}
	verb := "imported"
	if dryRun {
		verb = "dry run, nothing written; would have"
	}
	fmt.Fprintf(out, "%s: created %d, updated %d, unchanged %d; %d files not read\n",
		verb, total.Created, total.Updated, total.Skipped, len(skipped))
	return nil
}
