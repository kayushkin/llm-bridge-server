// Package timeaccounting turns a card's actions into the three numbers a board
// needs: how long the card has been alive, how much of that was time the work
// was actually available to be done, and how that compares to the limit its
// priority sets.
//
// Nothing here is stored. Every figure is computed by walking the card's events
// on demand, which is what keeps the event log the single source of truth — a
// cached total is a second copy of the answer, and a second copy drifts.
package timeaccounting

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/kayushkin/kanban-store/internal/model"
)

// Input is everything a card's figures depend on.
type Input struct {
	Events []model.CardEvent
	Notes  map[string]*model.CardNote
	Level  *model.BoardPriorityLevel
	Hours  *model.BusinessHours
	Now    time.Time
}

// Compute walks the events once and returns both the summary and the timeline.
//
// The rule is the whole design in one sentence: the time between an event and
// the next one belongs to the state that event put the card into. A card with no
// events has no time — it reports zeroes and an empty timeline rather than
// falling back to when its row was written, because a row being written is not
// something that happened to the work.
func Compute(in Input) (*model.CardTimeSummary, []model.TimelineEntry) {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	events := append([]model.CardEvent(nil), in.Events...)
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].OccurredAt.Equal(events[j].OccurredAt) {
			return events[i].OccurredAt.Before(events[j].OccurredAt)
		}
		// Same instant: fall back to when we heard about it, then to the id, so the
		// order is total and two runs of this function never disagree.
		if !events[i].RecordedAt.Equal(events[j].RecordedAt) {
			return events[i].RecordedAt.Before(events[j].RecordedAt)
		}
		return events[i].ID < events[j].ID
	})

	s := &model.CardTimeSummary{AsOf: now, EventCount: len(events)}
	supersededBy := model.SupersededByEventID(events)
	for _, e := range events {
		if e.Kind != model.EventTimeLogged || supersededBy[e.ID] != "" {
			continue
		}
		var detail model.TimeLoggedEventDetail
		// Only kanban-store's time-entries route writes this kind. A detail that
		// does not parse contributes nothing; the time-entries read reports it.
		if err := json.Unmarshal(e.Detail, &detail); err == nil {
			s.LoggedSeconds += detail.Seconds
		}
	}
	if in.Level != nil {
		value := in.Level.PriorityValue
		s.PriorityValue = &value
		s.PriorityLabel = in.Level.Label
		if in.Level.BudgetSeconds != nil {
			budget := *in.Level.BudgetSeconds
			s.BudgetSeconds = &budget
		}
	}
	if len(events) == 0 {
		if in.Hours != nil {
			zero := 0.0
			zeroToo := 0.0
			s.BusinessHoursElapsedSeconds = &zero
			s.BusinessHoursBudgetClockSeconds = &zeroToo
		}
		return s, []model.TimelineEntry{}
	}

	first := events[0].OccurredAt
	last := events[len(events)-1].OccurredAt
	s.FirstEventAt = &first
	s.LastEventAt = &last
	s.ClockState = events[len(events)-1].ClockState

	var businessElapsed, businessBudget float64
	entries := make([]model.TimelineEntry, 0, len(events))
	segments := make([]model.TimeSegment, 0, len(events))

	for i, e := range events {
		end := now
		open := true
		if i+1 < len(events) {
			end = events[i+1].OccurredAt
			open = false
		} else if e.ClockState == model.ClockStopped {
			// The card is finished. Time after the last action is not the card's
			// time, so the final segment has no length at all.
			end = e.OccurredAt
			open = false
		}
		// A backdated event can land before the one recorded ahead of it even after
		// sorting, when two events share an instant. Clamp rather than subtract a
		// negative from the totals.
		if end.Before(e.OccurredAt) {
			end = e.OccurredAt
		}
		seconds := end.Sub(e.OccurredAt).Seconds()

		seg := model.TimeSegment{
			EventID: e.ID, ClockState: e.ClockState,
			From: e.OccurredAt, To: end, Seconds: seconds, Open: open && seconds > 0,
		}
		segments = append(segments, seg)

		s.ElapsedSeconds += seconds
		counts := e.ClockState == model.ClockRunning
		switch e.ClockState {
		case model.ClockRunning:
			s.BudgetClockSeconds += seconds
		case model.ClockPaused:
			s.WaitingSeconds += seconds
		}
		if in.Hours != nil && seconds > 0 {
			b := BusinessSecondsBetween(e.OccurredAt, end, in.Hours)
			businessElapsed += b
			if counts {
				businessBudget += b
			}
		}

		sincePrevious := 0.0
		if i > 0 {
			sincePrevious = e.OccurredAt.Sub(events[i-1].OccurredAt).Seconds()
			if sincePrevious < 0 {
				sincePrevious = 0
			}
		}
		entry := model.TimelineEntry{
			Event:                     e,
			SupersededByEventID:       supersededBy[e.ID],
			SecondsSincePreviousEvent: sincePrevious,
			SegmentSeconds:            seconds,
			SegmentOpen:               seg.Open,
			CountsAgainstBudget:       counts,
			ClockState:                e.ClockState,
		}
		if e.NoteID != "" && in.Notes != nil {
			entry.Note = in.Notes[e.NoteID]
		}
		entries = append(entries, entry)
	}
	s.Segments = segments

	if in.Hours != nil {
		s.BusinessHoursElapsedSeconds = &businessElapsed
		s.BusinessHoursBudgetClockSeconds = &businessBudget
	}
	if s.BudgetSeconds != nil {
		remaining := float64(*s.BudgetSeconds) - s.BudgetClockSeconds
		over := remaining < 0
		s.BudgetRemainingSeconds = &remaining
		s.OverBudget = &over
	}
	return s, entries
}

// BusinessSecondsBetween measures how much of [from, to) falls inside the working
// week, in the board's own zone. It walks day by day in that zone rather than
// doing arithmetic on offsets, so a range spanning a daylight-saving change
// counts the hours that actually existed.
func BusinessSecondsBetween(from, to time.Time, hours *model.BusinessHours) float64 {
	if hours == nil || !to.After(from) {
		return 0
	}
	loc, err := time.LoadLocation(hours.TZID)
	if err != nil {
		// Validation refuses an unknown zone at write time, so reaching this means
		// the zone database lost a name the board was already configured with.
		// Reporting zero is the only honest answer left.
		return 0
	}
	startMinutes, err := model.ParseClockTime(hours.Start)
	if err != nil {
		return 0
	}
	endMinutes, err := model.ParseClockTime(hours.End)
	if err != nil {
		return 0
	}
	days := hours.WeekdaySet()
	if len(days) == 0 {
		return 0
	}
	holidays := hours.HolidaySet()

	total := 0.0
	localFrom := from.In(loc)
	cursor := time.Date(localFrom.Year(), localFrom.Month(), localFrom.Day(), 0, 0, 0, 0, loc)
	for cursor.Before(to) {
		next := cursor.AddDate(0, 0, 1)
		_, working := days[cursor.Weekday()]
		// cursor is midnight in the board's zone, so its date is the date a
		// holiday names.
		if _, holiday := holidays[cursor.Format(model.HolidayDateLayout)]; holiday {
			working = false
		}
		if working {
			windowStart := cursor.Add(time.Duration(startMinutes) * time.Minute)
			windowEnd := cursor.Add(time.Duration(endMinutes) * time.Minute)
			lower := windowStart
			if from.After(lower) {
				lower = from
			}
			upper := windowEnd
			if to.Before(upper) {
				upper = to
			}
			if upper.After(lower) {
				total += upper.Sub(lower).Seconds()
			}
		}
		cursor = next
	}
	return total
}
