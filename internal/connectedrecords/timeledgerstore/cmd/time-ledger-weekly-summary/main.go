// Command time-ledger-weekly-summary reads one week of the time ledger whole
// and prints the prompt a weekly review would send: every entry that overlaps
// the week, with its kinds, its parent, the outcome the daily review gave it
// and that review's reason, how many records of each link type it rests on,
// and the money linked to it.
//
// It only reads. Where the summary is written (a journal timeline) and where
// its open questions go are the operator's decisions, so until they are made
// the command prints the prompt and its size and stops.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"time"

	timeledgerstore "github.com/kayushkin/llm-bridge-server/internal/connectedrecords/timeledgerstore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/timeledgerstore/internal/storeclient"
)

const pageSize = 1000

func main() {
	ledgerURL := flag.String("time-ledger-store-url", "http://127.0.0.1:8344", "where time-ledger-store answers")
	weekEnding := flag.String("week-ending", "", "the day after the week's last day, YYYY-MM-DD in -time-zone; empty means today")
	timeZoneName := flag.String("time-zone", "America/Los_Angeles", "the zone the week's days are counted in")
	dryRun := flag.Bool("dry-run", false, "print the prompt and its size; required until the write half exists")
	flag.Parse()
	if !*dryRun {
		log.Fatal("only -dry-run exists: where the summary and its questions are written is not decided yet (see the time ledger phase 6 todos)")
	}
	zone, err := time.LoadLocation(*timeZoneName)
	if err != nil {
		log.Fatalf("-time-zone: %v", err)
	}
	from, to, err := weekWindow(*weekEnding, zone, time.Now())
	if err != nil {
		log.Fatal(err)
	}

	listed, err := listEntries(*ledgerURL, from, to)
	if err != nil {
		log.Fatal(err)
	}
	entries := make([]timeledgerstore.TimeEntry, 0, len(listed))
	reviews := map[string]timeledgerstore.Review{}
	for _, item := range listed {
		var entry timeledgerstore.TimeEntry
		if err := storeclient.Get(*ledgerURL+"/time-entries/"+url.PathEscape(item.ID), &entry); err != nil {
			log.Fatalf("%s: %v", item.ID, err)
		}
		if entry.OutcomeReviewID != "" {
			var review timeledgerstore.Review
			if err := storeclient.Get(*ledgerURL+"/reviews/"+url.PathEscape(entry.OutcomeReviewID), &review); err != nil {
				log.Fatalf("%s review %s: %v", entry.ID, entry.OutcomeReviewID, err)
			}
			reviews[entry.ID] = review
		}
		entries = append(entries, entry)
	}

	payload := buildWeekPayload(from, to, zone, entries, reviews)
	prompt, err := json.MarshalIndent(payload, "", " ")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(prompt))
	fmt.Fprintf(os.Stderr, "%d entries, %d bytes of prompt\n", len(entries), len(prompt))
}

// weekWindow gives the seven local days that end before weekEnding (or
// before today, when it is empty), as epoch seconds [from, to).
func weekWindow(weekEnding string, zone *time.Location, now time.Time) (int64, int64, error) {
	var end time.Time
	if weekEnding == "" {
		local := now.In(zone)
		end = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	} else {
		parsed, err := time.ParseInLocation("2006-01-02", weekEnding, zone)
		if err != nil {
			return 0, 0, fmt.Errorf("-week-ending: %w", err)
		}
		end = parsed
	}
	return end.AddDate(0, 0, -7).Unix(), end.Unix(), nil
}

// listEntries reads every entry that overlaps [from, to), a page at a time.
func listEntries(ledgerURL string, from, to int64) ([]timeledgerstore.TimeEntry, error) {
	all := []timeledgerstore.TimeEntry{}
	for offset := 0; ; offset += pageSize {
		query := url.Values{"from": {fmt.Sprint(from)}, "to": {fmt.Sprint(to)}, "limit": {fmt.Sprint(pageSize)}, "offset": {fmt.Sprint(offset)}}
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

// buildWeekPayload turns the week's entries and the reviews that gave them
// their outcomes into what the model reads. Removed links are left out.
// Money is the sum of amount_cents on live transaction links; an entry with
// no such link has no money field, rather than a zero that reads as "free".
func buildWeekPayload(from, to int64, zone *time.Location, entries []timeledgerstore.TimeEntry, reviews map[string]timeledgerstore.Review) map[string]any {
	shown := make([]map[string]any, 0, len(entries))
	weekCents, entriesWithMoney := int64(0), 0
	for _, entry := range entries {
		linkCounts := map[string]int{}
		entryCents, hasMoney := int64(0), false
		for _, link := range entry.Links {
			if link.RemovedAt != nil {
				continue
			}
			linkCounts[link.LinkType]++
			if link.LinkType == "transactions" && link.AmountCents != nil {
				entryCents += *link.AmountCents
				hasMoney = true
			}
		}
		kinds := []string{}
		for _, kind := range entry.Kinds {
			kinds = append(kinds, kind.Name)
		}
		sort.Strings(kinds)
		item := map[string]any{
			"id": entry.ID, "title": entry.Title, "place": entry.Place, "all_day": entry.AllDay,
			"starts": localTime(entry.StartsAt, entry.TimeZone, zone), "ends": localTime(entry.EndsAt, entry.TimeZone, zone),
			"kinds": kinds, "parent_time_entry_id": entry.ParentTimeEntryID,
			"outcome": entry.Outcome, "records_linked": linkCounts,
		}
		if review, ok := reviews[entry.ID]; ok {
			item["outcome_reason"] = review.Reason
			item["outcome_by"] = review.ReviewedBy
		}
		if hasMoney {
			item["amount_cents"] = entryCents
			weekCents += entryCents
			entriesWithMoney++
		}
		shown = append(shown, item)
	}
	return map[string]any{
		"week_from":          time.Unix(from, 0).In(zone).Format("Mon 2006-01-02"),
		"week_to_exclusive":  time.Unix(to, 0).In(zone).Format("Mon 2006-01-02"),
		"time_zone":          zone.String(),
		"entries":            shown,
		"amount_cents_total": weekCents,
		"entries_with_money": entriesWithMoney,
	}
}

// localTime shows a moment in the entry's own zone, or in the week's zone
// when the entry names none or one that does not load.
func localTime(unix int64, entryZoneName string, weekZone *time.Location) string {
	zone := weekZone
	if entryZoneName != "" {
		if loaded, err := time.LoadLocation(entryZoneName); err == nil {
			zone = loaded
		}
	}
	return time.Unix(unix, 0).In(zone).Format("Mon 2006-01-02 15:04 MST")
}
