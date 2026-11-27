package calendar

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	ical "github.com/emersion/go-ical"
)

// ParsedEvent is one event read from an import: a VEVENT of an iCalendar
// file, or a Google Calendar event resource.
type ParsedEvent struct {
	// SourceEventID is Google's event id; an .ics file has none.
	SourceEventID string
	// OnlyStatusKnown marks an event deleted from Google that came back with
	// only its id: the import marks the row it finds cancelled and changes
	// nothing else.
	OnlyStatusKnown  bool
	UID              string
	RecurrenceID     string
	Summary          string
	Description      string
	Location         string
	StartsAt         int64
	EndsAt           int64
	AllDay           bool
	StartDate        string
	EndDate          string
	TimeZone         string
	RecurrenceRule   string
	Status           string
	Transparency     string
	OrganizerEmail   string
	OrganizerName    string
	SourceCreatedAt  int64
	SourceModifiedAt int64
	Sequence         int64
	Attendees        []Attendee
	Raw              string
}

// Attendee is one ATTENDEE of an event.
type Attendee struct {
	Email               string `json:"email"`
	Name                string `json:"name"`
	Role                string `json:"role"`
	ParticipationStatus string `json:"participation_status"`
}

// ParseICS reads every VEVENT of an iCalendar file. A time with no zone of its
// own is read in the calendar's X-WR-TIMEZONE; a file that has such a time and
// names no zone is refused rather than guessed at.
func ParseICS(content []byte) ([]ParsedEvent, error) {
	calendar, err := ical.NewDecoder(bytes.NewReader(content)).Decode()
	if err != nil {
		return nil, fmt.Errorf("read iCalendar: %w", err)
	}
	var floating *time.Location
	calendarZone := ""
	if prop := calendar.Props.Get("X-WR-TIMEZONE"); prop != nil && prop.Value != "" {
		calendarZone = prop.Value
		floating, err = time.LoadLocation(prop.Value)
		if err != nil {
			return nil, fmt.Errorf("X-WR-TIMEZONE %q: %w", prop.Value, err)
		}
	}
	raws := rawEventBlocks(content)
	events := calendar.Events()
	if len(raws) != len(events) {
		return nil, fmt.Errorf("found %d VEVENT blocks in the text but read %d events", len(raws), len(events))
	}
	parsed := make([]ParsedEvent, 0, len(events))
	for index, event := range events {
		one, err := parseEvent(event, floating, calendarZone)
		if err != nil {
			return nil, fmt.Errorf("event %d: %w", index+1, err)
		}
		one.Raw = raws[index]
		parsed = append(parsed, one)
	}
	return parsed, nil
}

// rawEventBlocks returns each VEVENT as the file wrote it, folded lines and
// all, in file order.
func rawEventBlocks(content []byte) []string {
	var blocks []string
	var current strings.Builder
	inside := false
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "BEGIN:VEVENT" {
			inside = true
			current.Reset()
		}
		if inside {
			current.WriteString(line)
			current.WriteString("\r\n")
		}
		if line == "END:VEVENT" && inside {
			inside = false
			blocks = append(blocks, current.String())
		}
	}
	return blocks
}

func text(event ical.Event, name string) string {
	prop := event.Props.Get(name)
	if prop == nil {
		return ""
	}
	value, err := prop.Text()
	if err != nil {
		return prop.Value
	}
	return value
}

func parseEvent(event ical.Event, floating *time.Location, calendarZone string) (ParsedEvent, error) {
	var parsed ParsedEvent
	parsed.UID = text(event, ical.PropUID)
	if parsed.UID == "" {
		return parsed, fmt.Errorf("no UID")
	}
	parsed.Summary = text(event, ical.PropSummary)
	parsed.Description = text(event, ical.PropDescription)
	parsed.Location = text(event, ical.PropLocation)
	parsed.Status = text(event, ical.PropStatus)
	parsed.Transparency = text(event, ical.PropTransparency)
	if prop := event.Props.Get(ical.PropRecurrenceRule); prop != nil {
		parsed.RecurrenceRule = prop.Value
	}
	if prop := event.Props.Get(ical.PropRecurrenceID); prop != nil {
		parsed.RecurrenceID = prop.Value
		if zone := prop.Params.Get(ical.ParamTimezoneID); zone != "" {
			parsed.RecurrenceID = zone + ":" + prop.Value
		}
	}
	if prop := event.Props.Get(ical.PropSequence); prop != nil {
		parsed.Sequence, _ = strconv.ParseInt(prop.Value, 10, 64)
	}
	for _, field := range []struct {
		name string
		into *int64
	}{{ical.PropCreated, &parsed.SourceCreatedAt}, {ical.PropLastModified, &parsed.SourceModifiedAt}} {
		if prop := event.Props.Get(field.name); prop != nil {
			moment, err := prop.DateTime(time.UTC)
			if err != nil {
				return parsed, fmt.Errorf("%s: %w", field.name, err)
			}
			*field.into = moment.Unix()
		}
	}

	start := event.Props.Get(ical.PropDateTimeStart)
	if start == nil {
		return parsed, fmt.Errorf("%s has no DTSTART", parsed.UID)
	}
	end := event.Props.Get(ical.PropDateTimeEnd)
	if start.ValueType() == ical.ValueDate {
		parsed.AllDay = true
		startDay, err := time.Parse("20060102", start.Value)
		if err != nil {
			return parsed, fmt.Errorf("DTSTART %q: %w", start.Value, err)
		}
		parsed.StartDate = startDay.Format("2006-01-02")
		parsed.StartsAt = startDay.Unix()
		if end != nil {
			endDay, err := time.Parse("20060102", end.Value)
			if err != nil {
				return parsed, fmt.Errorf("DTEND %q: %w", end.Value, err)
			}
			parsed.EndDate = endDay.Format("2006-01-02")
			parsed.EndsAt = endDay.Unix()
		}
		return parsed, parseAttendees(event, &parsed)
	}
	startsAt, zone, err := moment(start, floating, calendarZone)
	if err != nil {
		return parsed, fmt.Errorf("DTSTART: %w", err)
	}
	parsed.StartsAt, parsed.TimeZone = startsAt, zone
	if end != nil {
		endsAt, _, err := moment(end, floating, calendarZone)
		if err != nil {
			return parsed, fmt.Errorf("DTEND: %w", err)
		}
		parsed.EndsAt = endsAt
	}
	return parsed, parseAttendees(event, &parsed)
}

// moment reads a date-time and the zone it was written in: its TZID, UTC for
// a Z time, or the calendar's zone for a floating one.
func moment(prop *ical.Prop, floating *time.Location, calendarZone string) (int64, string, error) {
	zone := prop.Params.Get(ical.ParamTimezoneID)
	if zone == "" && strings.HasSuffix(prop.Value, "Z") {
		zone = "UTC"
	}
	if zone == "" {
		if floating == nil {
			return 0, "", fmt.Errorf("%q has no zone and the calendar names none", prop.Value)
		}
		zone = calendarZone
	}
	location := floating
	if location == nil {
		location = time.UTC
	}
	value, err := prop.DateTime(location)
	if err != nil {
		return 0, "", err
	}
	return value.Unix(), zone, nil
}

func mailAddress(value string) string {
	if len(value) >= 7 && strings.EqualFold(value[:7], "mailto:") {
		return strings.ToLower(strings.TrimSpace(value[7:]))
	}
	return strings.TrimSpace(value)
}

func parseAttendees(event ical.Event, parsed *ParsedEvent) error {
	if organizer := event.Props.Get(ical.PropOrganizer); organizer != nil {
		parsed.OrganizerEmail = mailAddress(organizer.Value)
		parsed.OrganizerName = organizer.Params.Get(ical.ParamCommonName)
	}
	for _, prop := range event.Props.Values(ical.PropAttendee) {
		parsed.Attendees = append(parsed.Attendees, Attendee{
			Email:               mailAddress(prop.Value),
			Name:                prop.Params.Get(ical.ParamCommonName),
			Role:                prop.Params.Get(ical.ParamRole),
			ParticipationStatus: prop.Params.Get(ical.ParamParticipationStatus),
		})
	}
	return nil
}
