// Command time-ledger-import brings into the time ledger what the operator
// planned or did, from the stores that hold it:
//
//   - every journey in location-store, as an entry of kind journey whose
//     source is the trip;
//   - every date of every calendar-store event, as an entry whose source is
//     the event and that date (kind routine for a series);
//   - every Discord event date the operator went to, by discord-signup-store's
//     attendance records, with the record as evidence it was done;
//   - event-store suggestions the operator added to the calendar, as a second
//     source on the entries of the calendar event they became.
//
// An entry already made for a source is brought up to date where a program
// set what changed, never where a model or the operator did. An entry inside a
// journey's span is put inside the journey unless someone placed it.
// Everything it writes it signs as a program.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strings"
	"time"

	timeledgerstore "github.com/kayushkin/time-ledger-store"
	"github.com/kayushkin/time-ledger-store/internal/occurrences"
	"github.com/kayushkin/time-ledger-store/internal/storeclient"
)

const signer = "time-ledger-import"

type importer struct {
	ledgerURL, calendarStoreURL, locationStoreURL, discordStoreURL, eventStoreURL string
	calendarID, operatorDiscordID, timeZone                                       string
	from, to                                                                      time.Time
	dryRun                                                                        bool
	kindIDs                                                                       map[string]string
	created, updated, unchanged, linked                                           int
}

func main() {
	var imp importer
	var fromDay, toDay string
	flag.StringVar(&imp.ledgerURL, "time-ledger-store-url", "http://127.0.0.1:8343", "where time-ledger-store answers")
	flag.StringVar(&imp.calendarStoreURL, "calendar-store-url", "http://127.0.0.1:8342", "where calendar-store answers")
	flag.StringVar(&imp.calendarID, "calendar", "", "the calendar-store calendar to read (calendar_000001); required")
	flag.StringVar(&imp.locationStoreURL, "location-store-url", "http://127.0.0.1:8333", "where location-store answers")
	flag.StringVar(&imp.discordStoreURL, "discord-signup-store-url", "http://127.0.0.1:8312", "where discord-signup-store answers")
	flag.StringVar(&imp.operatorDiscordID, "operator-discord-id", "", "the operator's Discord user id, whose attendance records to read; required")
	flag.StringVar(&imp.eventStoreURL, "event-store-url", "http://127.0.0.1:8308", "where event-store answers")
	flag.StringVar(&imp.timeZone, "time-zone", "", "the IANA zone the window's days are counted in; required")
	flag.StringVar(&fromDay, "from", time.Now().AddDate(0, 0, -60).Format("2006-01-02"), "first day of the window (YYYY-MM-DD, in -time-zone)")
	flag.StringVar(&toDay, "to", time.Now().AddDate(0, 0, 31).Format("2006-01-02"), "day after the last day of the window")
	flag.BoolVar(&imp.dryRun, "dry-run", false, "print what it would write, write nothing")
	flag.Parse()
	if imp.calendarID == "" || imp.operatorDiscordID == "" || imp.timeZone == "" {
		log.Fatal("-calendar, -operator-discord-id and -time-zone are required")
	}
	zone, err := time.LoadLocation(imp.timeZone)
	if err != nil {
		log.Fatalf("-time-zone: %v", err)
	}
	if imp.from, err = time.ParseInLocation("2006-01-02", fromDay, zone); err != nil {
		log.Fatalf("-from: %v", err)
	}
	if imp.to, err = time.ParseInLocation("2006-01-02", toDay, zone); err != nil {
		log.Fatalf("-to: %v", err)
	}
	if err := imp.readKinds(); err != nil {
		log.Fatal(err)
	}
	journeys, err := imp.importJourneys()
	if err != nil {
		log.Fatal(err)
	}
	calendarEvents, calendarEntries, err := imp.importCalendar()
	if err != nil {
		log.Fatal(err)
	}
	discordEntries, err := imp.importDiscordAttendance()
	if err != nil {
		log.Fatal(err)
	}
	if err := imp.linkEventStoreEvents(calendarEvents); err != nil {
		log.Fatal(err)
	}
	if err := imp.placeInsideJourneys(journeys, append(calendarEntries, discordEntries...)); err != nil {
		log.Fatal(err)
	}
	log.Printf("%s to %s: %d entries made, %d brought up to date, %d unchanged, %d links added", fromDay, toDay, imp.created, imp.updated, imp.unchanged, imp.linked)
}

func (imp *importer) readKinds() error {
	var kinds []timeledgerstore.Kind
	if err := storeclient.Get(imp.ledgerURL+"/kinds", &kinds); err != nil {
		return err
	}
	imp.kindIDs = map[string]string{}
	for _, kind := range kinds {
		imp.kindIDs[kind.Name] = kind.ID
	}
	for _, needed := range []string{"journey", "routine"} {
		if imp.kindIDs[needed] == "" {
			return fmt.Errorf("time-ledger-store has no kind %q", needed)
		}
	}
	return nil
}

// entryFor finds the entry a source belongs to, deleted ones too: an entry
// the operator deleted keeps its source, so it is not made again.
func (imp *importer) entryFor(linkType, ref string, occurrence *int64) (*timeledgerstore.TimeEntry, error) {
	query := url.Values{"ref": {ref}}
	if occurrence != nil {
		query.Set("occurrence_starts_at", fmt.Sprint(*occurrence))
	}
	var links []timeledgerstore.Link
	if err := storeclient.Get(imp.ledgerURL+"/links/"+linkType+"?"+query.Encode(), &links); err != nil {
		return nil, err
	}
	for _, link := range links {
		if link.BearsOn != timeledgerstore.BearsOnSource {
			continue
		}
		var entry timeledgerstore.TimeEntry
		if err := storeclient.Get(imp.ledgerURL+"/time-entries/"+link.TimeEntryID, &entry); err != nil {
			return nil, err
		}
		return &entry, nil
	}
	return nil, nil
}

// wanted is what a source says an entry should be.
type wanted struct {
	title              string
	startsAt, endsAt   int64
	allDay             bool
	startDate, endDate string
	timeZone, place    string
	kindNames          []string
	source             timeledgerstore.LinkInput
	evidence           []timeledgerstore.LinkInput
}

// ensure makes the entry a source wants, or brings the one it has up to date
// in the fields a program last set. It answers the entry, or nil when the
// operator deleted it or this is a dry run.
func (imp *importer) ensure(want wanted) (*timeledgerstore.TimeEntry, error) {
	existing, err := imp.entryFor(want.source.LinkType, want.source.Ref, want.source.OccurrenceStartsAt)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		input := timeledgerstore.EntryInput{Title: want.title, StartsAt: want.startsAt, EndsAt: want.endsAt, AllDay: want.allDay, StartDate: want.startDate,
			EndDate: want.endDate, TimeZone: want.timeZone, Place: want.place, Links: append([]timeledgerstore.LinkInput{want.source}, want.evidence...),
			By: signer, ByKind: timeledgerstore.ActorProgram, Reason: want.source.Reason}
		for _, name := range want.kindNames {
			input.KindIDs = append(input.KindIDs, imp.kindIDs[name])
		}
		if imp.dryRun {
			log.Printf("would make %q %s from %s %s", want.title, time.Unix(want.startsAt, 0).Format(time.RFC3339), want.source.LinkType, want.source.Ref)
			imp.created++
			return nil, nil
		}
		var entry timeledgerstore.TimeEntry
		if err := storeclient.Do("POST", imp.ledgerURL+"/time-entries", input, &entry); err != nil {
			return nil, err
		}
		imp.created++
		imp.linked += len(input.Links)
		return &entry, nil
	}
	if existing.DeletedAt != nil {
		imp.unchanged++
		return nil, nil
	}
	change := timeledgerstore.EntryChange{By: signer, ByKind: timeledgerstore.ActorProgram, Reason: "the source changed: " + want.source.LinkType + " " + want.source.Ref}
	changed := false
	if existing.TitleByKind == timeledgerstore.ActorProgram && existing.Title != want.title {
		change.Title, changed = &want.title, true
	}
	if existing.PlaceByKind != timeledgerstore.ActorModel && existing.PlaceByKind != timeledgerstore.ActorOperator && existing.Place != want.place {
		change.Place, changed = &want.place, true
	}
	if existing.SpanByKind == timeledgerstore.ActorProgram && (existing.StartsAt != want.startsAt || existing.EndsAt != want.endsAt ||
		existing.AllDay != want.allDay || existing.StartDate != want.startDate || existing.EndDate != want.endDate || existing.TimeZone != want.timeZone) {
		change.StartsAt, change.EndsAt, change.AllDay, change.StartDate, change.EndDate, change.TimeZone =
			&want.startsAt, &want.endsAt, &want.allDay, &want.startDate, &want.endDate, &want.timeZone
		changed = true
	}
	if changed {
		if imp.dryRun {
			log.Printf("would bring %s %q up to date", existing.ID, want.title)
		} else if err := storeclient.Do("PATCH", imp.ledgerURL+"/time-entries/"+existing.ID, change, existing); err != nil {
			if !storeclient.IsStatus(err, 409) {
				return nil, err
			}
			log.Printf("left %s as it is: %v", existing.ID, err)
		}
		imp.updated++
	} else {
		imp.unchanged++
	}
	for _, link := range want.evidence {
		if err := imp.link(existing.ID, link); err != nil {
			return nil, err
		}
	}
	return existing, nil
}

// link adds a link, leaving alone one the operator took off.
func (imp *importer) link(entryID string, link timeledgerstore.LinkInput) error {
	if imp.dryRun {
		return nil
	}
	body := map[string]any{"ref": link.Ref, "bears_on": link.BearsOn, "reason": link.Reason, "by": signer, "by_kind": timeledgerstore.ActorProgram}
	if link.OccurrenceStartsAt != nil {
		body["occurrence_starts_at"] = *link.OccurrenceStartsAt
	}
	var made timeledgerstore.Link
	err := storeclient.Do("POST", imp.ledgerURL+"/time-entries/"+entryID+"/links/"+link.LinkType, body, &made)
	if storeclient.IsStatus(err, 409) {
		log.Printf("did not link %s %s to %s: %v", link.LinkType, link.Ref, entryID, err)
		return nil
	}
	if err != nil {
		return err
	}
	if made.LinkedAt > 0 && made.LinkedBy == signer {
		imp.linked++
	}
	return nil
}

type trip struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	StartsAt int64  `json:"started_at"`
	EndsAt   int64  `json:"ended_at"`
}

func (imp *importer) importJourneys() ([]timeledgerstore.TimeEntry, error) {
	var trips []trip
	query := url.Values{"kind": {"journey"}, "from": {fmt.Sprint(imp.from.Unix())}, "to": {fmt.Sprint(imp.to.Unix())}}
	if err := storeclient.Get(imp.locationStoreURL+"/trips?"+query.Encode(), &trips); err != nil {
		return nil, err
	}
	entries := []timeledgerstore.TimeEntry{}
	for _, journey := range trips {
		title := journey.Title
		if title == "" {
			title = "Journey " + journey.ID
		}
		entry, err := imp.ensure(wanted{title: title, startsAt: journey.StartsAt, endsAt: journey.EndsAt, timeZone: imp.timeZone,
			kindNames: []string{"journey"},
			source: timeledgerstore.LinkInput{LinkType: "trips", Ref: journey.ID, BearsOn: timeledgerstore.BearsOnSource,
				Reason: "location-store found this journey in the operator's location history"}})
		if err != nil {
			return nil, err
		}
		if entry != nil {
			entries = append(entries, *entry)
		}
	}
	return entries, nil
}

// importCalendar answers every event of the calendar, for event-store's
// links, and the entries made or found for the window's dates.
func (imp *importer) importCalendar() ([]occurrences.Event, []timeledgerstore.TimeEntry, error) {
	events := []occurrences.Event{}
	for offset := 0; ; offset += 1000 {
		var page []occurrences.Event
		query := url.Values{"calendar_id": {imp.calendarID}, "limit": {"1000"}, "offset": {fmt.Sprint(offset)}}
		if err := storeclient.Get(imp.calendarStoreURL+"/events?"+query.Encode(), &page); err != nil {
			return nil, nil, err
		}
		events = append(events, page...)
		if len(page) < 1000 {
			break
		}
	}
	// Only a series needs its raw text, for the dates it removed.
	for i := range events {
		if events[i].RecurrenceRule == "" {
			continue
		}
		var full occurrences.Event
		if err := storeclient.Get(imp.calendarStoreURL+"/events/"+events[i].ID, &full); err != nil {
			return nil, nil, err
		}
		events[i].Raw = full.Raw
	}
	dates, err := occurrences.Expand(events, imp.from, imp.to, imp.timeZone)
	if err != nil {
		return nil, nil, err
	}
	entries := []timeledgerstore.TimeEntry{}
	for _, date := range dates {
		title := date.Title
		if title == "" {
			title = "(untitled calendar event)"
		}
		kinds := []string{}
		if date.Repeats {
			kinds = append(kinds, "routine")
		}
		key := date.Key
		entry, err := imp.ensure(wanted{title: title, startsAt: date.StartsAt, endsAt: date.EndsAt, allDay: date.AllDay, startDate: date.StartDate,
			endDate: date.EndDate, timeZone: date.TimeZone, place: date.Place, kindNames: kinds,
			source: timeledgerstore.LinkInput{LinkType: "calendar_events", Ref: date.EventID, OccurrenceStartsAt: &key, BearsOn: timeledgerstore.BearsOnSource,
				Reason: "on the operator's calendar"}})
		if err != nil {
			return nil, nil, err
		}
		if entry != nil {
			entries = append(entries, *entry)
		}
	}
	return events, entries, nil
}

type attendance struct {
	Ref                string `json:"ref"`
	EventRef           string `json:"event_ref"`
	OccurrenceStartsAt int64  `json:"occurrence_starts_at"`
	Basis              string `json:"basis"`
}

type discordEvent struct {
	Ref      string `json:"ref"`
	Name     string `json:"name"`
	StartsAt int64  `json:"starts_at"`
	EndsAt   int64  `json:"ends_at"`
	TimeZone string `json:"timezone"`
	Location string `json:"location"`
}

func (imp *importer) importDiscordAttendance() ([]timeledgerstore.TimeEntry, error) {
	var answer struct {
		Attendance []attendance `json:"attendance"`
	}
	if err := storeclient.Get(imp.discordStoreURL+"/api/attendance?discord_user_id="+url.QueryEscape(imp.operatorDiscordID), &answer); err != nil {
		return nil, err
	}
	events := map[string]discordEvent{}
	entries := []timeledgerstore.TimeEntry{}
	for _, record := range answer.Attendance {
		event, read := events[record.EventRef]
		if !read {
			if err := storeclient.Get(imp.discordStoreURL+"/api/discord-events/"+record.EventRef, &event); err != nil {
				return nil, err
			}
			events[record.EventRef] = event
		}
		startsAt := record.OccurrenceStartsAt
		if startsAt == 0 {
			startsAt = event.StartsAt
		}
		if startsAt < imp.from.Unix() || startsAt >= imp.to.Unix() {
			continue
		}
		// An event with no end is kept as an instant: the ledger does not
		// guess how long it ran.
		endsAt := startsAt
		if event.EndsAt > event.StartsAt {
			endsAt = startsAt + event.EndsAt - event.StartsAt
		}
		timeZone := event.TimeZone
		if timeZone == "" {
			timeZone = imp.timeZone
		}
		entry, err := imp.ensure(wanted{title: event.Name, startsAt: startsAt, endsAt: endsAt, timeZone: timeZone, place: event.Location,
			source: timeledgerstore.LinkInput{LinkType: "discord_events", Ref: record.EventRef, OccurrenceStartsAt: &startsAt, BearsOn: timeledgerstore.BearsOnSource,
				Reason: "discord-signup-store records the operator going"},
			evidence: []timeledgerstore.LinkInput{{LinkType: "discord_attendances", Ref: record.Ref, BearsOn: timeledgerstore.BearsOnDone,
				Reason: "discord-signup-store's attendance record, basis " + record.Basis}}})
		if err != nil {
			return nil, err
		}
		if entry != nil {
			entries = append(entries, *entry)
		}
	}
	return entries, nil
}

type eventStoreEvent struct {
	ID              int64  `json:"id"`
	Title           string `json:"title"`
	CalendarEventID string `json:"calendar_event_id"`
}

// linkEventStoreEvents adds each added suggestion as a source of the entries
// of the calendar event it became. event-store names that event by Google's
// id, which calendar-store keeps as source_event_id.
func (imp *importer) linkEventStoreEvents(calendarEvents []occurrences.Event) error {
	var answer struct {
		Events []eventStoreEvent `json:"events"`
	}
	if err := storeclient.Get(imp.eventStoreURL+"/events?status=added&limit=1000", &answer); err != nil {
		return err
	}
	byGoogleID := map[string]string{}
	for _, event := range calendarEvents {
		if event.SourceEventID != "" && event.RecurrenceID == "" {
			byGoogleID[event.SourceEventID] = event.ID
		}
	}
	for _, suggestion := range answer.Events {
		calendarEventID := byGoogleID[suggestion.CalendarEventID]
		if suggestion.CalendarEventID == "" || calendarEventID == "" {
			continue
		}
		var links []timeledgerstore.Link
		if err := storeclient.Get(imp.ledgerURL+"/links/calendar_events?ref="+calendarEventID, &links); err != nil {
			return err
		}
		for _, link := range links {
			if link.BearsOn != timeledgerstore.BearsOnSource {
				continue
			}
			if err := imp.link(link.TimeEntryID, timeledgerstore.LinkInput{LinkType: "event_store_events", Ref: fmt.Sprint(suggestion.ID),
				BearsOn: timeledgerstore.BearsOnSource, Reason: "event-store suggested it and the operator added it to the calendar"}); err != nil {
				return err
			}
		}
	}
	return nil
}

// placeInsideJourneys puts each entry with no parent, or one a program chose,
// inside the shortest journey whose span holds it.
func (imp *importer) placeInsideJourneys(journeys, entries []timeledgerstore.TimeEntry) error {
	sort.Slice(journeys, func(i, j int) bool {
		return journeys[i].EndsAt-journeys[i].StartsAt < journeys[j].EndsAt-journeys[j].StartsAt
	})
	for _, entry := range entries {
		if entry.ParentSetByKind == timeledgerstore.ActorModel || entry.ParentSetByKind == timeledgerstore.ActorOperator {
			continue
		}
		parent := ""
		for _, journey := range journeys {
			if journey.ID != entry.ID && entry.StartsAt >= journey.StartsAt && entry.EndsAt <= journey.EndsAt {
				parent = journey.ID
				break
			}
		}
		if parent == entry.ParentTimeEntryID || imp.dryRun {
			continue
		}
		change := timeledgerstore.EntryChange{ParentTimeEntryID: &parent, By: signer, ByKind: timeledgerstore.ActorProgram,
			Reason: "it lies inside this journey"}
		if parent == "" {
			change.Reason = "it no longer lies inside a journey"
		}
		if err := storeclient.Do("PATCH", imp.ledgerURL+"/time-entries/"+entry.ID, change, nil); err != nil {
			if !storeclient.IsStatus(err, 409) {
				return err
			}
			log.Printf("left %s where it is: %v", entry.ID, strings.TrimSpace(err.Error()))
		}
	}
	return nil
}
