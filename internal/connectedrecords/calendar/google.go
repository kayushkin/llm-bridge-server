package calendar

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Import formats: how the body of an import was written.
const (
	ImportFormatICS             = "ics"
	ImportFormatGoogleEventJSON = "google-event-json"
)

// googleEvent is the part of a Google Calendar v3 event resource the columns
// hold. The whole resource is kept as the event's raw.
type googleEvent struct {
	ID                string      `json:"id"`
	ICalUID           string      `json:"iCalUID"`
	Status            string      `json:"status"`
	Summary           string      `json:"summary"`
	Description       string      `json:"description"`
	Location          string      `json:"location"`
	Transparency      string      `json:"transparency"`
	RecurringEventID  string      `json:"recurringEventId"`
	OriginalStartTime *googleTime `json:"originalStartTime"`
	Start             *googleTime `json:"start"`
	End               *googleTime `json:"end"`
	Recurrence        []string    `json:"recurrence"`
	Organizer         *struct {
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
	} `json:"organizer"`
	Attendees []struct {
		Email          string `json:"email"`
		DisplayName    string `json:"displayName"`
		ResponseStatus string `json:"responseStatus"`
		Optional       bool   `json:"optional"`
	} `json:"attendees"`
	Created  string `json:"created"`
	Updated  string `json:"updated"`
	Sequence int64  `json:"sequence"`
}

type googleTime struct {
	Date     string `json:"date"`
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

// googleParticipationStatuses maps Google's responseStatus to the iCalendar
// PARTSTAT an .ics export writes for it, so both imports fill the column alike.
var googleParticipationStatuses = map[string]string{
	"needsAction": "NEEDS-ACTION",
	"accepted":    "ACCEPTED",
	"declined":    "DECLINED",
	"tentative":   "TENTATIVE",
}

// ParseGoogleEvents reads a JSON array of Google Calendar v3 event resources,
// as Google lists them with singleEvents=false: a series once, with its
// RRULE, and each changed or cancelled date as its own event with an
// originalStartTime. Columns are filled the way ParseICS fills them from
// Google's .ics export of the same event, so the two find each other by
// (uid, recurrence_id): uid is iCalUID, and recurrence_id is written as the
// export writes RECURRENCE-ID.
//
// Google promises only the id of an event deleted from the calendar. Such an
// event comes back with OnlyStatusKnown set: the store marks a row it already
// holds cancelled and keeps the rest of it.
func ParseGoogleEvents(content []byte) ([]ParsedEvent, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(content, &raws); err != nil {
		return nil, fmt.Errorf("read Google events: the body is not a JSON array: %w", err)
	}
	parsed := make([]ParsedEvent, 0, len(raws))
	for index, raw := range raws {
		var event googleEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, fmt.Errorf("event %d: %w", index+1, err)
		}
		one, err := parseGoogleEvent(event)
		if err != nil {
			return nil, fmt.Errorf("event %d (%s): %w", index+1, event.ID, err)
		}
		one.Raw = string(raw)
		parsed = append(parsed, one)
	}
	return parsed, nil
}

func parseGoogleEvent(event googleEvent) (ParsedEvent, error) {
	var parsed ParsedEvent
	if event.ID == "" {
		return parsed, fmt.Errorf("no id")
	}
	parsed.SourceEventID = event.ID
	parsed.UID = event.ICalUID
	parsed.Status = strings.ToUpper(event.Status)
	cancelled := parsed.Status == "CANCELLED"
	if event.Start == nil && event.OriginalStartTime == nil {
		if !cancelled {
			return parsed, fmt.Errorf("no start")
		}
		parsed.OnlyStatusKnown = true
		return parsed, nil
	}
	if parsed.UID == "" {
		return parsed, fmt.Errorf("no iCalUID")
	}
	parsed.Summary = event.Summary
	parsed.Description = event.Description
	parsed.Location = event.Location
	// Google leaves transparency out when it is opaque, its documented default;
	// the export writes TRANSP:OPAQUE.
	parsed.Transparency = strings.ToUpper(event.Transparency)
	if parsed.Transparency == "" {
		parsed.Transparency = "OPAQUE"
	}
	parsed.Sequence = event.Sequence
	for _, field := range []struct {
		name, value string
		into        *int64
	}{{"created", event.Created, &parsed.SourceCreatedAt}, {"updated", event.Updated, &parsed.SourceModifiedAt}} {
		if field.value == "" {
			continue
		}
		moment, err := time.Parse(time.RFC3339Nano, field.value)
		if err != nil {
			return parsed, fmt.Errorf("%s %q: %w", field.name, field.value, err)
		}
		*field.into = moment.Unix()
	}
	for _, line := range event.Recurrence {
		if rule, ok := strings.CutPrefix(line, "RRULE:"); ok {
			parsed.RecurrenceRule = rule
			break
		}
	}
	if event.OriginalStartTime != nil {
		recurrenceID, err := googleRecurrenceID(*event.OriginalStartTime)
		if err != nil {
			return parsed, fmt.Errorf("originalStartTime: %w", err)
		}
		parsed.RecurrenceID = recurrenceID
	}

	// A cancelled date of a series carries only the start it replaces.
	start, end := event.Start, event.End
	if start == nil {
		start, end = event.OriginalStartTime, nil
	}
	if start.Date != "" {
		startDay, err := time.Parse("2006-01-02", start.Date)
		if err != nil {
			return parsed, fmt.Errorf("start.date %q: %w", start.Date, err)
		}
		parsed.AllDay = true
		parsed.StartDate = start.Date
		parsed.StartsAt = startDay.Unix()
		if end != nil {
			endDay, err := time.Parse("2006-01-02", end.Date)
			if err != nil {
				return parsed, fmt.Errorf("end.date %q: %w", end.Date, err)
			}
			parsed.EndDate = end.Date
			parsed.EndsAt = endDay.Unix()
		}
	} else {
		startsAt, err := time.Parse(time.RFC3339, start.DateTime)
		if err != nil {
			return parsed, fmt.Errorf("start.dateTime %q: %w", start.DateTime, err)
		}
		parsed.StartsAt = startsAt.Unix()
		parsed.TimeZone = start.TimeZone
		if end != nil {
			endsAt, err := time.Parse(time.RFC3339, end.DateTime)
			if err != nil {
				return parsed, fmt.Errorf("end.dateTime %q: %w", end.DateTime, err)
			}
			parsed.EndsAt = endsAt.Unix()
		}
	}

	if event.Organizer != nil {
		parsed.OrganizerEmail = strings.ToLower(strings.TrimSpace(event.Organizer.Email))
		parsed.OrganizerName = event.Organizer.DisplayName
	}
	for _, attendee := range event.Attendees {
		status, known := googleParticipationStatuses[attendee.ResponseStatus]
		if !known {
			return parsed, fmt.Errorf("attendee %s: responseStatus %q is not one Google documents", attendee.Email, attendee.ResponseStatus)
		}
		role := "REQ-PARTICIPANT"
		if attendee.Optional {
			role = "OPT-PARTICIPANT"
		}
		parsed.Attendees = append(parsed.Attendees, Attendee{
			Email: strings.ToLower(strings.TrimSpace(attendee.Email)), Name: attendee.DisplayName,
			Role: role, ParticipationStatus: status,
		})
	}
	return parsed, nil
}

// googleRecurrenceID writes an originalStartTime as Google's export writes
// RECURRENCE-ID, which ParseICS stores as "<TZID>:<local time>", a UTC time
// with Z, or a date.
func googleRecurrenceID(original googleTime) (string, error) {
	if original.Date != "" {
		day, err := time.Parse("2006-01-02", original.Date)
		if err != nil {
			return "", err
		}
		return day.Format("20060102"), nil
	}
	moment, err := time.Parse(time.RFC3339, original.DateTime)
	if err != nil {
		return "", err
	}
	if original.TimeZone == "" {
		return moment.UTC().Format("20060102T150405Z"), nil
	}
	zone, err := time.LoadLocation(original.TimeZone)
	if err != nil {
		return "", fmt.Errorf("timeZone %q: %w", original.TimeZone, err)
	}
	return original.TimeZone + ":" + moment.In(zone).Format("20060102T150405"), nil
}
