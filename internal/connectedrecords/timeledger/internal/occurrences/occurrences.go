// Package occurrences turns a calendar-store event into the dates it happens
// on: one for a single event, each date of a series inside a window less the
// dates the series removed (EXDATE) or that a changed copy of one date stands
// in for.
package occurrences

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

// Event is the part of a calendar-store event this package reads.
type Event struct {
	ID             string `json:"id"`
	UID            string `json:"uid"`
	RecurrenceID   string `json:"recurrence_id"`
	SourceEventID  string `json:"source_event_id"`
	Summary        string `json:"summary"`
	Location       string `json:"location"`
	StartsAt       int64  `json:"starts_at"`
	EndsAt         int64  `json:"ends_at"`
	AllDay         bool   `json:"all_day"`
	StartDate      string `json:"start_date"`
	EndDate        string `json:"end_date"`
	TimeZone       string `json:"time_zone"`
	RecurrenceRule string `json:"recurrence_rule"`
	Status         string `json:"status"`
	Raw            string `json:"raw,omitempty"`
}

// Occurrence is one date an event happens on. Key is the date's original
// start, which names it even when a changed copy moved it: it is the
// occurrence_starts_at a time entry's calendar link carries.
type Occurrence struct {
	EventID   string
	Key       int64
	StartsAt  int64
	EndsAt    int64
	AllDay    bool
	StartDate string
	EndDate   string
	TimeZone  string
	Title     string
	Place     string
	Repeats   bool
}

// Cancelled reports whether the calendar says the event will not happen.
func Cancelled(event Event) bool { return strings.EqualFold(event.Status, "CANCELLED") }

var exdatePattern = regexp.MustCompile(`EXDATE([^:"\r\n]*):([0-9TZ,]+)`)

// ParseCalendarTime reads an iCalendar time value as calendar-store keeps
// recurrence ids: "<TZID>:<value>" or "<value>", the value a date, a UTC time
// ending in Z, or a local time in zone.
func ParseCalendarTime(text string, zone *time.Location) (time.Time, bool, error) {
	if tzid, value, found := strings.Cut(text, ":"); found {
		location, err := time.LoadLocation(tzid)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("time zone %q: %w", tzid, err)
		}
		zone, text = location, value
	}
	switch {
	case len(text) == 8:
		day, err := time.ParseInLocation("20060102", text, zone)
		return day, true, err
	case strings.HasSuffix(text, "Z"):
		moment, err := time.Parse("20060102T150405Z", text)
		return moment, false, err
	default:
		moment, err := time.ParseInLocation("20060102T150405", text, zone)
		return moment, false, err
	}
}

// excludedDates reads every EXDATE in an event's raw text, iCalendar or
// Google's JSON alike, as unix seconds.
func excludedDates(raw string, zone *time.Location) (map[int64]bool, error) {
	excluded := map[int64]bool{}
	for _, match := range exdatePattern.FindAllStringSubmatch(raw, -1) {
		valueZone := zone
		for _, parameter := range strings.Split(strings.TrimPrefix(match[1], ";"), ";") {
			if tzid, found := strings.CutPrefix(parameter, "TZID="); found {
				location, err := time.LoadLocation(tzid)
				if err != nil {
					return nil, fmt.Errorf("EXDATE time zone %q: %w", tzid, err)
				}
				valueZone = location
			}
		}
		for _, value := range strings.Split(match[2], ",") {
			moment, _, err := ParseCalendarTime(value, valueZone)
			if err != nil {
				return nil, fmt.Errorf("EXDATE %q: %w", value, err)
			}
			excluded[moment.Unix()] = true
		}
	}
	return excluded, nil
}

// Expand lists every occurrence of the events that starts in [from, to).
// events must hold every event of the calendar that shares a uid, so a
// series knows the dates its changed copies stand in for. An all-day event
// with no zone (Google writes dates that way) is a date wherever the operator
// is, so it is read in floatingZone, the operator's own zone.
func Expand(events []Event, from, to time.Time, floatingZone string) ([]Occurrence, error) {
	if _, err := time.LoadLocation(floatingZone); err != nil || floatingZone == "" {
		return nil, fmt.Errorf("floating zone %q is not an IANA zone", floatingZone)
	}
	for i := range events {
		if events[i].TimeZone == "" {
			if !events[i].AllDay {
				return nil, fmt.Errorf("%s is a timed event with no time zone", events[i].ID)
			}
			events[i].TimeZone = floatingZone
		}
	}
	overridden := map[string]map[int64]bool{}
	for _, event := range events {
		if event.RecurrenceID == "" {
			continue
		}
		zone, err := zoneOf(event)
		if err != nil {
			return nil, err
		}
		slot, _, err := ParseCalendarTime(event.RecurrenceID, zone)
		if err != nil {
			return nil, fmt.Errorf("%s recurrence id %q: %w", event.ID, event.RecurrenceID, err)
		}
		if overridden[event.UID] == nil {
			overridden[event.UID] = map[int64]bool{}
		}
		overridden[event.UID][slot.Unix()] = true
	}
	occurrences := []Occurrence{}
	for _, event := range events {
		if Cancelled(event) {
			continue
		}
		zone, err := zoneOf(event)
		if err != nil {
			return nil, err
		}
		base := Occurrence{EventID: event.ID, Key: event.StartsAt, StartsAt: event.StartsAt, EndsAt: event.EndsAt, AllDay: event.AllDay,
			StartDate: event.StartDate, EndDate: event.EndDate, TimeZone: event.TimeZone, Title: strings.TrimSpace(event.Summary), Place: event.Location}
		if event.AllDay {
			// calendar-store puts an all-day event at UTC midnight, for
			// ordering only; the day runs midnight to midnight in its zone.
			startDay, err := time.ParseInLocation("2006-01-02", event.StartDate, zone)
			if err != nil {
				return nil, fmt.Errorf("%s start date %q: %w", event.ID, event.StartDate, err)
			}
			endDay, err := time.ParseInLocation("2006-01-02", event.EndDate, zone)
			if err != nil {
				return nil, fmt.Errorf("%s end date %q: %w", event.ID, event.EndDate, err)
			}
			base.Key, base.StartsAt, base.EndsAt = startDay.Unix(), startDay.Unix(), endDay.Unix()
		}
		if event.RecurrenceID != "" {
			slot, _, err := ParseCalendarTime(event.RecurrenceID, zone)
			if err != nil {
				return nil, fmt.Errorf("%s recurrence id %q: %w", event.ID, event.RecurrenceID, err)
			}
			base.Key, base.Repeats = slot.Unix(), true
			if inWindow(base.StartsAt, from, to) {
				occurrences = append(occurrences, base)
			}
			continue
		}
		if event.RecurrenceRule == "" {
			if inWindow(base.StartsAt, from, to) {
				occurrences = append(occurrences, base)
			}
			continue
		}
		expanded, err := expandSeries(event, base, zone, from, to, overridden[event.UID])
		if err != nil {
			return nil, err
		}
		occurrences = append(occurrences, expanded...)
	}
	return occurrences, nil
}

func inWindow(startsAt int64, from, to time.Time) bool {
	return startsAt >= from.Unix() && startsAt < to.Unix()
}

func zoneOf(event Event) (*time.Location, error) {
	zone, err := time.LoadLocation(event.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("%s time zone %q: %w", event.ID, event.TimeZone, err)
	}
	return zone, nil
}

func expandSeries(event Event, base Occurrence, zone *time.Location, from, to time.Time, overridden map[int64]bool) ([]Occurrence, error) {
	start := time.Unix(event.StartsAt, 0).In(zone)
	if event.AllDay {
		day, err := time.ParseInLocation("2006-01-02", event.StartDate, zone)
		if err != nil {
			return nil, fmt.Errorf("%s start date %q: %w", event.ID, event.StartDate, err)
		}
		start = day
	}
	options, err := rrule.StrToROptionInLocation(event.RecurrenceRule, zone)
	if err != nil {
		return nil, fmt.Errorf("%s rule %q: %w", event.ID, event.RecurrenceRule, err)
	}
	options.Dtstart = start
	rule, err := rrule.NewRRule(*options)
	if err != nil {
		return nil, fmt.Errorf("%s rule %q: %w", event.ID, event.RecurrenceRule, err)
	}
	excluded, err := excludedDates(event.Raw, zone)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", event.ID, err)
	}
	duration := event.EndsAt - event.StartsAt
	days := 0
	if event.AllDay {
		startDay, _ := time.Parse("2006-01-02", event.StartDate)
		endDay, err := time.Parse("2006-01-02", event.EndDate)
		if err != nil {
			return nil, fmt.Errorf("%s end date %q: %w", event.ID, event.EndDate, err)
		}
		days = int(endDay.Sub(startDay).Hours() / 24)
	}
	occurrences := []Occurrence{}
	for _, moment := range rule.Between(from.In(zone), to.In(zone), true) {
		slot := moment.Unix()
		if excluded[slot] || overridden[slot] || !inWindow(slot, from, to) {
			continue
		}
		occurrence := base
		occurrence.Key, occurrence.StartsAt, occurrence.Repeats = slot, slot, true
		if event.AllDay {
			occurrence.StartDate = moment.Format("2006-01-02")
			end := moment.AddDate(0, 0, days)
			occurrence.EndDate = end.Format("2006-01-02")
			occurrence.EndsAt = end.Unix()
		} else {
			occurrence.EndsAt = slot + duration
		}
		occurrences = append(occurrences, occurrence)
	}
	return occurrences, nil
}
