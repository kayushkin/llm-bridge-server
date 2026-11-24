// Command journal-timeline-calendar-events puts past calendar events on
// journal timelines: every one on the timeline given for the calendar, and
// each again on the timeline of every person calendar-store links it to who
// has one.
//
// calendar-store holds the events and who each was with; this command writes
// timeline items that cite an event by its id (calendar_event_000001). A
// calendar entry is a reminder or a plan, not a record that he went, and the
// items say so.
//
// A repeating event becomes one item that spans its dates, from its first to
// its last when the rule ends, and is written once its first date has passed.
// A changed copy of one date of a series stays in calendar-store only. A
// cancelled event is left out.
//
// Nothing is written twice. A timeline that already has an item citing the
// event, deleted ones included, gets no other: an item the operator removed is
// not put back, and an item written by hand that cites the event wins.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	journalstore "github.com/kayushkin/journal-store"
	"github.com/teambition/rrule-go"
)

var client = &http.Client{Timeout: time.Minute}

// ReminderCaveat ends every item: many calendar entries are reminders of
// things he might go to, so an item says nothing about whether he went.
const ReminderCaveat = "Calendar entries are reminders and plans; this does not say he went."

// calendarPageSize is the most calendar-store's GET /events answers at once.
const calendarPageSize = 1000

type personLink struct {
	PersonID string `json:"person_id"`
}

type calendarEvent struct {
	ID             string       `json:"id"`
	RecurrenceID   string       `json:"recurrence_id"`
	Summary        string       `json:"summary"`
	Location       string       `json:"location"`
	StartsAt       int64        `json:"starts_at"`
	EndsAt         int64        `json:"ends_at"`
	AllDay         bool         `json:"all_day"`
	StartDate      string       `json:"start_date"`
	EndDate        string       `json:"end_date"`
	TimeZone       string       `json:"time_zone"`
	RecurrenceRule string       `json:"recurrence_rule"`
	Status         string       `json:"status"`
	People         []personLink `json:"people"`
}

type timelineItem struct {
	Sources []string `json:"sources"`
}

func getJSON(address string, into any) error {
	response, err := client.Get(address)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s: %s", address, response.Status, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("GET %s: %w", address, err)
	}
	return nil
}

func postJSON(address string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	response, err := client.Post(address, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("POST %s: %s: %s", address, response.Status, strings.TrimSpace(string(answer)))
	}
	return nil
}

// span is where an item sits in time.
type span struct {
	OccurredAt int64
	EndsAt     int64
	Precision  string
	// Repeats describes a series ("weekly, until 11 Dec 2024"), "" otherwise.
	Repeats string
}

var frequencyUnits = map[rrule.Frequency]string{
	rrule.YEARLY: "years", rrule.MONTHLY: "months", rrule.WEEKLY: "weeks",
	rrule.DAILY: "days", rrule.HOURLY: "hours", rrule.MINUTELY: "minutes",
}

var frequencyWords = map[rrule.Frequency]string{
	rrule.YEARLY: "yearly", rrule.MONTHLY: "monthly", rrule.WEEKLY: "weekly",
	rrule.DAILY: "daily", rrule.HOURLY: "hourly", rrule.MINUTELY: "every minute",
}

// spanOf places an event in time, or says it has not happened yet. An
// all-day event's dates are read in zone, the operator's.
func spanOf(event calendarEvent, zone *time.Location, now time.Time) (span, bool, error) {
	var result span
	start := time.Unix(event.StartsAt, 0).In(zone)
	end := time.Unix(event.EndsAt, 0).In(zone)
	result.Precision = "minute"
	if event.AllDay {
		day, err := time.ParseInLocation("2006-01-02", event.StartDate, zone)
		if err != nil {
			return result, false, fmt.Errorf("%s start_date %q: %w", event.ID, event.StartDate, err)
		}
		start, end = day, day
		if event.EndDate != "" {
			last, err := time.ParseInLocation("2006-01-02", event.EndDate, zone)
			if err != nil {
				return result, false, fmt.Errorf("%s end_date %q: %w", event.ID, event.EndDate, err)
			}
			// iCalendar's end date is the day after; one day is a moment.
			if last.Sub(day) > 24*time.Hour {
				end = last.Add(-24 * time.Hour)
			}
		}
		result.Precision = "day"
	}
	if event.EndsAt == 0 && !event.AllDay {
		end = start
	}
	if start.After(now) {
		return result, false, nil
	}
	result.OccurredAt = start.Unix()
	if end.After(start) {
		result.EndsAt = end.Unix()
	}
	if event.RecurrenceRule == "" {
		return result, true, nil
	}
	ruleZone := zone
	if event.TimeZone != "" && !event.AllDay {
		if loaded, err := time.LoadLocation(event.TimeZone); err == nil {
			ruleZone = loaded
		}
	}
	options, err := rrule.StrToROptionInLocation(event.RecurrenceRule, ruleZone)
	if err != nil {
		return result, false, fmt.Errorf("%s rule %q: %w", event.ID, event.RecurrenceRule, err)
	}
	options.Dtstart = start
	rule, err := rrule.NewRRule(*options)
	if err != nil {
		return result, false, fmt.Errorf("%s rule %q: %w", event.ID, event.RecurrenceRule, err)
	}
	word := frequencyWords[options.Freq]
	if options.Interval > 1 {
		word = fmt.Sprintf("every %d %s", options.Interval, frequencyUnits[options.Freq])
	}
	result.EndsAt = 0
	if options.Until.IsZero() && options.Count == 0 {
		result.Repeats = word
		return result, true, nil
	}
	dates := rule.All()
	if len(dates) == 0 {
		return result, false, nil
	}
	last := dates[len(dates)-1]
	if last.After(start) {
		result.EndsAt = last.Unix()
	}
	result.Repeats = fmt.Sprintf("%s, %d dates until %s", word, len(dates), last.In(zone).Format("2 Jan 2006"))
	return result, true, nil
}

func markdownText(text string) string {
	return strings.NewReplacer("[", "(", "]", ")").Replace(text)
}

func titleOf(event calendarEvent) string {
	title := strings.TrimSpace(event.Summary)
	if title == "" {
		title = "Untitled calendar event"
	}
	return title
}

type importer struct {
	journalURL, calendarURL, peopleURL string
	operatorPersonID                   string
	dryRun                             bool

	timelineItems   map[string][]timelineItem
	personNames     map[string]string
	personTimelines map[string][]string
	written         int
}

func (importer *importer) cites(timelineID, ref string) (bool, error) {
	items, ok := importer.timelineItems[timelineID]
	if !ok {
		if err := getJSON(importer.journalURL+"/timelines/"+timelineID+"/items?include_deleted=true", &items); err != nil {
			return false, err
		}
		importer.timelineItems[timelineID] = items
	}
	for _, item := range items {
		for _, source := range item.Sources {
			if source == ref {
				return true, nil
			}
		}
	}
	return false, nil
}

func (importer *importer) personName(personID string) (string, error) {
	if name, ok := importer.personNames[personID]; ok {
		return name, nil
	}
	var person struct {
		DisplayName string `json:"display_name"`
	}
	if err := getJSON(importer.peopleURL+"/persons/"+url.PathEscape(personID), &person); err != nil {
		return "", err
	}
	importer.personNames[personID] = person.DisplayName
	return person.DisplayName, nil
}

func (importer *importer) timelinesAbout(personID string) ([]string, error) {
	if ids, ok := importer.personTimelines[personID]; ok {
		return ids, nil
	}
	var entries []struct {
		ID string `json:"id"`
	}
	if err := getJSON(importer.journalURL+"/entries?kind=timeline&about_ref="+url.QueryEscape(personID), &entries); err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	importer.personTimelines[personID] = ids
	return ids, nil
}

func (importer *importer) write(timelineID string, item journalstore.TimelineItemInput) error {
	importer.written++
	if importer.dryRun {
		log.Printf("would add to %s: %s %s %q", timelineID, time.Unix(item.OccurredAt, 0).UTC().Format(time.RFC3339), item.Sources[0], item.Title)
		return nil
	}
	if err := postJSON(importer.journalURL+"/timelines/"+timelineID+"/items", journalstore.TimelineItemsRequest{Items: []journalstore.TimelineItemInput{item}}); err != nil {
		return err
	}
	importer.timelineItems[timelineID] = append(importer.timelineItems[timelineID], timelineItem{Sources: item.Sources})
	return nil
}

func (importer *importer) place(event calendarEvent, calendarTimelineID string, zone *time.Location, now time.Time) error {
	if event.RecurrenceID != "" || strings.EqualFold(event.Status, "CANCELLED") {
		return nil
	}
	when, happened, err := spanOf(event, zone, now)
	if err != nil || !happened {
		return err
	}
	title := titleOf(event)
	where := ""
	if event.Location != "" {
		where = " at " + markdownText(strings.Join(strings.Fields(event.Location), " "))
	}
	repeats := ""
	if when.Repeats != "" {
		repeats = ", repeating " + when.Repeats
	}
	var people []string
	var others []string
	for _, link := range event.People {
		if link.PersonID == importer.operatorPersonID {
			continue
		}
		name, err := importer.personName(link.PersonID)
		if err != nil {
			return err
		}
		people = append(people, link.PersonID)
		others = append(others, fmt.Sprintf("[%s](ref:%s)", markdownText(name), link.PersonID))
	}
	withWhom := ""
	if len(others) > 0 {
		withWhom = " With " + strings.Join(others, ", ") + "."
	}
	body := fmt.Sprintf("A reminder on his calendar: [%s](ref:%s)%s%s.%s %s",
		markdownText(title), event.ID, where, repeats, withWhom, ReminderCaveat)
	item := journalstore.TimelineItemInput{OccurredAt: when.OccurredAt, EndsAt: when.EndsAt, Precision: when.Precision,
		Title: title, Body: body, Sources: []string{event.ID}}
	targets := []string{calendarTimelineID}
	for _, personID := range people {
		timelineIDs, err := importer.timelinesAbout(personID)
		if err != nil {
			return err
		}
		targets = append(targets, timelineIDs...)
	}
	for _, timelineID := range targets {
		already, err := importer.cites(timelineID, event.ID)
		if err != nil {
			return err
		}
		if already {
			continue
		}
		if err := importer.write(timelineID, item); err != nil {
			return fmt.Errorf("%s: %w", event.ID, err)
		}
	}
	return nil
}

func main() {
	journalURL := flag.String("journal-store-url", "http://"+journalstore.DefaultListenAddress, "journal-store's base URL")
	calendarURL := flag.String("calendar-store-url", "", "calendar-store's base URL (required)")
	peopleURL := flag.String("people-store-url", "", "people-store's base URL, for the names shown on links (required)")
	calendarID := flag.String("calendar", "", "calendar-store's calendar id whose events to place (required)")
	timelineID := flag.String("timeline", "", "the timeline entry every event of the calendar goes on (required)")
	operatorPersonID := flag.String("operator-person-id", "", "the person whose calendar it is; never listed as someone the event was with (required)")
	zoneName := flag.String("time-zone", "", "IANA zone an all-day event's date is read in (required)")
	dryRun := flag.Bool("dry-run", false, "say what would be added and write nothing")
	flag.Parse()
	if *calendarURL == "" || *peopleURL == "" || *calendarID == "" || *timelineID == "" || *operatorPersonID == "" || *zoneName == "" {
		flag.Usage()
		os.Exit(2)
	}
	zone, err := time.LoadLocation(*zoneName)
	if err != nil {
		log.Fatalf("time zone: %v", err)
	}
	importer := &importer{journalURL: *journalURL, calendarURL: *calendarURL, peopleURL: *peopleURL,
		operatorPersonID: *operatorPersonID, dryRun: *dryRun,
		timelineItems: map[string][]timelineItem{}, personNames: map[string]string{}, personTimelines: map[string][]string{}}
	now := time.Now()
	read := 0
	for offset := 0; ; offset += calendarPageSize {
		var page []calendarEvent
		if err := getJSON(fmt.Sprintf("%s/events?calendar_id=%s&limit=%d&offset=%d", *calendarURL, url.QueryEscape(*calendarID), calendarPageSize, offset), &page); err != nil {
			log.Fatal(err)
		}
		for _, event := range page {
			if err := importer.place(event, *timelineID, zone, now); err != nil {
				log.Fatal(err)
			}
		}
		read += len(page)
		if len(page) < calendarPageSize {
			break
		}
	}
	verb := "added"
	if *dryRun {
		verb = "would add"
	}
	log.Printf("%d calendar events read; %s %d timeline items", read, verb, importer.written)
}
