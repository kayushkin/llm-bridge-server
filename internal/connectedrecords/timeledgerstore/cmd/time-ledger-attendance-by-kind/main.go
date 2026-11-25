// Command time-ledger-attendance-by-kind prints how often the operator went to
// each kind of thing in the time ledger: for every kind, how many entries came
// out done, skipped or unknown, with one-off entries counted apart from dates
// of a recurring series. Beside that it prints how many event-store
// suggestions of each category are waiting, so a person deciding how past
// attendance should rank suggestions can see both vocabularies at once.
//
// It only reads. How the rates rank suggestions is the operator's decision
// (time ledger phase 5a); this command shows the numbers that decision rests on.
//
// An entry with several kinds counts once under each. An entry with no kind
// counts under "(no kind)". An entry is a date of a series when the record it
// came from (its live source link) is the source of more than one live entry.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"sort"
	"text/tabwriter"

	timeledgerstore "github.com/kayushkin/llm-bridge-server/internal/connectedrecords/timeledgerstore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/timeledgerstore/internal/storeclient"
)

const (
	pageSize = 1000
	// eventStoreLimit is asked of event-store, which has no paging; a full
	// answer means the list may be cut short, and the command stops.
	eventStoreLimit = 100000
	noKind          = "(no kind)"
)

func main() {
	ledgerURL := flag.String("time-ledger-store-url", "http://127.0.0.1:8344", "where time-ledger-store answers")
	eventStoreURL := flag.String("event-store-url", "http://127.0.0.1:8308", "where event-store answers")
	eventStatus := flag.String("event-status", "candidate", "which event-store suggestions to count per category")
	flag.Parse()

	listed, err := listAllEntries(*ledgerURL)
	if err != nil {
		log.Fatal(err)
	}
	entries := make([]timeledgerstore.TimeEntry, 0, len(listed))
	for _, item := range listed {
		var entry timeledgerstore.TimeEntry
		if err := storeclient.Get(*ledgerURL+"/time-entries/"+url.PathEscape(item.ID), &entry); err != nil {
			log.Fatalf("%s: %v", item.ID, err)
		}
		entries = append(entries, entry)
	}
	categories, err := countEventCategories(*eventStoreURL, *eventStatus)
	if err != nil {
		log.Fatal(err)
	}
	printReport(os.Stdout, countAttendanceByKind(entries), categories, *eventStatus, len(entries))
}

// listAllEntries reads every live entry, a page at a time.
func listAllEntries(ledgerURL string) ([]timeledgerstore.TimeEntry, error) {
	all := []timeledgerstore.TimeEntry{}
	for offset := 0; ; offset += pageSize {
		query := url.Values{"limit": {fmt.Sprint(pageSize)}, "offset": {fmt.Sprint(offset)}}
		var page []timeledgerstore.TimeEntry
		if err := storeclient.Get(ledgerURL+"/time-entries?"+query.Encode(), &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			return all, nil
		}
	}
}

// OutcomeCounts holds how many entries came out each way.
type OutcomeCounts struct {
	Done, Skipped, Unknown int
}

func (counts *OutcomeCounts) add(outcome string) {
	switch outcome {
	case timeledgerstore.OutcomeDone:
		counts.Done++
	case timeledgerstore.OutcomeSkipped:
		counts.Skipped++
	case timeledgerstore.OutcomeUnknown:
		counts.Unknown++
	default:
		log.Fatalf("outcome %q is not one of %v", outcome, timeledgerstore.Outcomes)
	}
}

// KindAttendance is one kind's counts, one-off entries apart from series dates.
type KindAttendance struct {
	Kind        string
	OneOff      OutcomeCounts
	SeriesDates OutcomeCounts
}

// countAttendanceByKind counts each entry's outcome under each of its kinds.
func countAttendanceByKind(entries []timeledgerstore.TimeEntry) []KindAttendance {
	entriesPerSource := map[string]int{}
	for _, entry := range entries {
		for _, source := range liveSources(entry) {
			entriesPerSource[source]++
		}
	}
	byKind := map[string]*KindAttendance{}
	for _, entry := range entries {
		isSeriesDate := false
		for _, source := range liveSources(entry) {
			if entriesPerSource[source] > 1 {
				isSeriesDate = true
			}
		}
		kinds := []string{}
		for _, kind := range entry.Kinds {
			kinds = append(kinds, kind.Name)
		}
		if len(kinds) == 0 {
			kinds = []string{noKind}
		}
		for _, kind := range kinds {
			attendance := byKind[kind]
			if attendance == nil {
				attendance = &KindAttendance{Kind: kind}
				byKind[kind] = attendance
			}
			if isSeriesDate {
				attendance.SeriesDates.add(entry.Outcome)
			} else {
				attendance.OneOff.add(entry.Outcome)
			}
		}
	}
	result := make([]KindAttendance, 0, len(byKind))
	for _, attendance := range byKind {
		result = append(result, *attendance)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Kind < result[j].Kind })
	return result
}

// liveSources names each record an entry came from, as "link_type ref".
func liveSources(entry timeledgerstore.TimeEntry) []string {
	sources := []string{}
	for _, link := range entry.Links {
		if link.RemovedAt == nil && link.BearsOn == timeledgerstore.BearsOnSource {
			sources = append(sources, link.LinkType+" "+link.Ref)
		}
	}
	return sources
}

// countEventCategories counts event-store suggestions of one status per category.
func countEventCategories(eventStoreURL, status string) (map[string]int, error) {
	var answer struct {
		Events []struct {
			Category string `json:"category"`
		} `json:"events"`
	}
	query := url.Values{"status": {status}, "limit": {fmt.Sprint(eventStoreLimit)}}
	if err := storeclient.Get(eventStoreURL+"/events?"+query.Encode(), &answer); err != nil {
		return nil, err
	}
	if len(answer.Events) >= eventStoreLimit {
		return nil, fmt.Errorf("event-store returned %d events, the most asked for; the count would be short", len(answer.Events))
	}
	counts := map[string]int{}
	for _, event := range answer.Events {
		counts[event.Category]++
	}
	return counts, nil
}

func printReport(out io.Writer, kinds []KindAttendance, categories map[string]int, eventStatus string, entryCount int) {
	fmt.Fprintf(out, "%d live time entries\n\n", entryCount)
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(table, "kind\tone-off done\tskipped\tunknown\tseries dates done\tskipped\tunknown\t")
	for _, kind := range kinds {
		fmt.Fprintf(table, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t\n", kind.Kind,
			kind.OneOff.Done, kind.OneOff.Skipped, kind.OneOff.Unknown,
			kind.SeriesDates.Done, kind.SeriesDates.Skipped, kind.SeriesDates.Unknown)
	}
	table.Flush()

	names := make([]string, 0, len(categories))
	for name := range categories {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if categories[names[i]] != categories[names[j]] {
			return categories[names[i]] > categories[names[j]]
		}
		return names[i] < names[j]
	})
	fmt.Fprintf(out, "\nevent-store suggestions with status %q, per category\n", eventStatus)
	table = tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	for _, name := range names {
		shown := name
		if shown == "" {
			shown = "(no category)"
		}
		fmt.Fprintf(table, "%s\t%d\t\n", shown, categories[name])
	}
	table.Flush()
}
