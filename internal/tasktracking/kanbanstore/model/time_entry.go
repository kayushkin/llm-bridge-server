package model

import (
	"fmt"
	"strings"
	"time"
)

// EventTimeLogged is a person recording time they spent on a card: "45 minutes
// on the phone with the carrier". It is an event like any other and sits on the
// timeline with the moves and the notes.
//
// It has no kind-level clock state, like an assignment: saying how long
// something took says nothing about whether the work is runnable now, so the
// event carries the state the card was already in. And it happens when it is
// LOGGED, not when the work was done — the clock is the walk of the events in
// order, and an entry backdated to yesterday would split an interval that was
// already accounted for. When the work was done is TimeLoggedEventDetail.WorkedAt.
//
// It is written only through POST /api/cards/{id}/time-entries, never through
// /events, because an entry may replace another and that has rules.
const EventTimeLogged EventKind = "time_logged"

// MaxTimeEntrySeconds bounds one entry at 31 days. It exists to catch a unit
// mistake — milliseconds sent as seconds — not to say how long work may take.
const MaxTimeEntrySeconds = 31 * 24 * 3600

// TimeLoggedEventDetail is the detail of a time_logged event.
type TimeLoggedEventDetail struct {
	// Seconds is the time spent. Zero only on an entry that replaces another:
	// that is how an entry is withdrawn.
	Seconds int64 `json:"seconds"`
	// WorkerPrincipalID is who did the work, a principal-store human. It is not
	// always the event's actor: a lead may log time for someone on their team.
	WorkerPrincipalID string `json:"worker_principal_id"`
	// WorkedAt is when the work was done, when the person logging it said so.
	WorkedAt *time.Time `json:"worked_at,omitempty"`
}

// CreateTimeEntryRequest is POST /api/cards/{id}/time-entries.
type CreateTimeEntryRequest struct {
	Seconds int64 `json:"seconds"`
	// WorkerPrincipalID defaults to the calling principal. A caller with no
	// principal — an internal service — must name one.
	WorkerPrincipalID string     `json:"worker_principal_id,omitempty"`
	WorkedAt          *time.Time `json:"worked_at,omitempty"`
	// Summary says what the time went on. It is the event's summary.
	Summary string `json:"summary,omitempty"`
	BoardID string `json:"board_id,omitempty"`
	// SupersedesEventID makes this entry a correction of another: the entry it
	// names stops counting and this one counts in its place. Nothing is
	// rewritten — the old entry stays on the timeline exactly as it was logged,
	// and is inactive because this one points at it. An entry can be replaced
	// once; correcting a correction names the newest one.
	SupersedesEventID string `json:"supersedes_event_id,omitempty"`
}

func (r *CreateTimeEntryRequest) Validate(now time.Time) error {
	if r.Seconds < 0 {
		return fmt.Errorf("seconds must not be negative (got %d)", r.Seconds)
	}
	if r.Seconds == 0 && r.SupersedesEventID == "" {
		return fmt.Errorf("seconds is required; zero is only how an entry that replaces another withdraws it")
	}
	if r.Seconds > MaxTimeEntrySeconds {
		return fmt.Errorf("seconds is %d, more than the %d (31 days) one entry takes; check the unit", r.Seconds, MaxTimeEntrySeconds)
	}
	if r.WorkedAt != nil && r.WorkedAt.After(now.Add(5*time.Minute)) {
		return fmt.Errorf("worked_at %s is in the future", r.WorkedAt.UTC().Format(time.RFC3339))
	}
	if r.Summary != strings.TrimSpace(r.Summary) {
		return fmt.Errorf("summary has surrounding whitespace")
	}
	return nil
}

// TimeEntry is one time_logged event read as what it is.
type TimeEntry struct {
	EventID           string     `json:"event_id"`
	CardID            string     `json:"card_id"`
	BoardID           string     `json:"board_id,omitempty"`
	Seconds           int64      `json:"seconds"`
	WorkerPrincipalID string     `json:"worker_principal_id"`
	WorkedAt          *time.Time `json:"worked_at,omitempty"`
	Summary           string     `json:"summary,omitempty"`
	// LoggedBy is the event's actor and LoggedAt its occurred_at.
	LoggedBy string    `json:"logged_by,omitempty"`
	LoggedAt time.Time `json:"logged_at"`
	// SupersedesEventID is the entry this one replaced; SupersededByEventID is
	// the entry that replaced this one. Follow either to read an entry's history.
	SupersedesEventID   string `json:"supersedes_event_id,omitempty"`
	SupersededByEventID string `json:"superseded_by_event_id,omitempty"`
	// Active is false once another entry has replaced this one. Only active
	// entries count.
	Active bool `json:"active"`
}

// CardTimeEntries is GET /api/cards/{id}/time-entries.
type CardTimeEntries struct {
	CardID string `json:"card_id"`
	// ActiveSeconds is the sum over active entries, whatever filter was asked
	// for: it is the card's logged time.
	ActiveSeconds int64       `json:"active_seconds"`
	Entries       []TimeEntry `json:"entries"`
}

// SupersededByEventID maps each event that has been replaced to the event that
// replaced it. It is the whole of "deactivation": nothing is stored on the
// replaced event.
func SupersededByEventID(events []CardEvent) map[string]string {
	supersededBy := map[string]string{}
	for _, event := range events {
		if event.SupersedesEventID != "" {
			supersededBy[event.SupersedesEventID] = event.ID
		}
	}
	return supersededBy
}
