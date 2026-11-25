// Command message-link-journal-transcripts records, on each conversation day,
// which journal-store entry holds that day's transcript. An importer names the
// transcript by its journal source and source_ref; this reads journal-store's
// transcript entries for each source given and posts their ids.
//
//	message-link-journal-transcripts -source skype -source discord-package -source facebook-messenger
//
// The addresses are JOURNAL_STORE_URL and MESSAGE_STORE_URL.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	messagestore "github.com/kayushkin/message-store"
)

const importerName = "message-link-journal-transcripts"

// pageSize is the most entries journal-store answers in one page.
const pageSize = 500

// linkBatchSize is how many links go in one POST.
const linkBatchSize = 2000

type sources []string

func (s *sources) String() string     { return strings.Join(*s, ",") }
func (s *sources) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	var journalSources sources
	flag.Var(&journalSources, "source", "a journal-store transcript source to link (repeatable): skype, discord-package, facebook-messenger, android-messages, flip3-sms…")
	dryRun := flag.Bool("dry-run", false, "read the transcripts and count them; post nothing")
	showUnmatched := flag.Int("show-unmatched", 10, "print up to this many unmatched source_refs per source")
	flag.Parse()
	if len(journalSources) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	settings, err := messagestore.NewImporterSettingsRegistry(importerName, servicesettings.ProcessEnvironment())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", importerName, err)
		os.Exit(1)
	}
	linker := &linker{journalURL: strings.TrimRight(settings.String(messagestore.SettingJournalStoreURL), "/"),
		storeURL: strings.TrimRight(settings.String(messagestore.SettingStoreURL), "/"), http: &http.Client{Timeout: 5 * time.Minute}}
	failed := false
	for _, source := range journalSources {
		if err := linker.link(source, *dryRun, *showUnmatched, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %s: %v\n", importerName, source, err)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

type linker struct {
	journalURL, storeURL string
	http                 *http.Client
}

type journalEntry struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	SourceRef string `json:"source_ref"`
}

// transcripts reads every live transcript entry journal-store holds under
// source, page by page.
func (l *linker) transcripts(source string) ([]messagestore.JournalEntryLink, error) {
	var links []messagestore.JournalEntryLink
	seen := map[string]bool{}
	for offset := 0; ; offset += pageSize {
		query := url.Values{"kind": {"transcript"}, "source": {source}, "limit": {fmt.Sprint(pageSize)}, "offset": {fmt.Sprint(offset)}}
		var page []journalEntry
		if err := l.call(http.MethodGet, l.journalURL+"/entries?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, entry := range page {
			if entry.Source != source || entry.SourceRef == "" {
				return nil, fmt.Errorf("journal-store answered entry %s with source %q, source_ref %q for source %q", entry.ID, entry.Source, entry.SourceRef, source)
			}
			if seen[entry.ID] {
				return nil, fmt.Errorf("entry %s came twice: journal-store changed while it was read; run again", entry.ID)
			}
			seen[entry.ID] = true
			links = append(links, messagestore.JournalEntryLink{JournalSource: source, JournalSourceRef: entry.SourceRef, JournalEntryID: entry.ID})
		}
		if len(page) < pageSize {
			return links, nil
		}
	}
}

func (l *linker) link(source string, dryRun bool, showUnmatched int, out io.Writer) error {
	links, err := l.transcripts(source)
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintf(out, "%s: %d transcripts; dry run, nothing posted\n", source, len(links))
		return nil
	}
	result := messagestore.JournalEntryLinkResult{Unmatched: []messagestore.JournalEntryLink{}}
	for start := 0; start < len(links); start += linkBatchSize {
		var answer messagestore.JournalEntryLinkResult
		if err := l.call(http.MethodPost, l.storeURL+"/conversation-days/journal-entries", links[start:min(start+linkBatchSize, len(links))], &answer); err != nil {
			return err
		}
		result.Linked += answer.Linked
		result.Unmatched = append(result.Unmatched, answer.Unmatched...)
	}
	fmt.Fprintf(out, "%s: %d transcripts, %d linked, %d unmatched\n", source, len(links), result.Linked, len(result.Unmatched))
	for _, unmatched := range result.Unmatched[:min(showUnmatched, len(result.Unmatched))] {
		fmt.Fprintf(out, "  unmatched %s %s\n", unmatched.JournalEntryID, unmatched.JournalSourceRef)
	}
	return nil
}

func (l *linker) call(method, address string, body, answer any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequest(method, address, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := l.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, address, response.Status, strings.TrimSpace(string(content)))
	}
	return json.Unmarshal(content, answer)
}
