package model

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/kanban-store/internal/boundedtext"
)

// ============================ The budget clock ============================

// ClockState says what the time following an event does to the card's time
// limit. It is stored on the event itself rather than derived at read time from
// the event kind or the column: ladders get re-tuned and columns get
// re-classified, and a timeline has to keep reporting what was true when the
// action actually happened.
type ClockState string

const (
	// ClockRunning — the work is ours to do, so the time after this event counts
	// against the card's limit.
	ClockRunning ClockState = "running"
	// ClockPaused — someone else has the ball: an unanswered email, a blocked
	// dependency, a held card. Time still elapses. It does not count against the
	// limit.
	ClockPaused ClockState = "paused"
	// ClockStopped — the work is over. Nothing accrues after this event, neither
	// elapsed time nor budget.
	ClockStopped ClockState = "stopped"
)

var clockStates = map[ClockState]struct{}{
	ClockRunning: {}, ClockPaused: {}, ClockStopped: {},
}

func ValidClockState(s ClockState) bool {
	_, ok := clockStates[s]
	return ok
}

func ClockStateNames() []string {
	out := make([]string, 0, len(clockStates))
	for s := range clockStates {
		out = append(out, string(s))
	}
	return out
}

// ============================ Event kinds ============================

// EventKind is the action that happened to a card. The set below is what
// kanban-store records for itself; the kind field is deliberately open so a
// caller can record an action this service has never heard of. What an unknown
// kind cannot do is guess its own clock state — see DefaultClockStateForEventKind.
type EventKind string

const (
	EventCardCreated     EventKind = "card_created"
	EventCardAttached    EventKind = "card_attached"
	EventCardDetached    EventKind = "card_detached"
	EventCardMoved       EventKind = "card_moved"
	EventCardHeld        EventKind = "card_held"
	EventCardUnheld      EventKind = "card_unheld"
	EventCardCompleted   EventKind = "card_completed"
	EventEmailReceived   EventKind = "email_received"
	EventAgentDispatched EventKind = "agent_dispatched"
	EventAgentFinished   EventKind = "agent_finished"
	EventNoteAdded       EventKind = "note_added"
	EventWaitingStarted  EventKind = "waiting_started"
	EventWaitingEnded    EventKind = "waiting_ended"
	// EventAssigned and EventUnassigned are a principal being put on the card
	// or taken off it. Like a move they have no kind-level clock state (see the
	// map below): who is on the work says nothing about whether the work is
	// runnable, so the event carries whatever state the card was already in.
	EventAssigned   EventKind = "assigned"
	EventUnassigned EventKind = "unassigned"
	// EventAssignmentSkipped is a card arriving on a board with an assignment
	// pool and being left unassigned: nobody in the pool was available and the
	// board has no default principal. Its detail says why, so the board shows
	// the reason instead of a card that looks forgotten. No clock state of its
	// own, like EventAssigned.
	EventAssignmentSkipped EventKind = "assignment_skipped"
)

// defaultClockStateByEventKind is what each action means for the clock when the
// caller does not say. Reading it: anything that puts work in front of us starts
// the clock, anything that hands the ball to someone else pauses it, and only
// completion stops it.
//
// EventCardMoved is absent on purpose. A move means whatever the destination
// column declares it means, so the state comes from the column and there is no
// kind-level default to fall back on.
//
// EventAssigned and EventUnassigned are absent for the same reason. Assignment
// neither starts nor stops the clock; the handler records them with the state
// the card is already in (its last action, or failing that its column), and a
// caller posting one by hand has to say which state it means.
var defaultClockStateByEventKind = map[EventKind]ClockState{
	EventCardCreated:     ClockRunning,
	EventCardAttached:    ClockRunning,
	EventCardDetached:    ClockStopped,
	EventCardHeld:        ClockPaused,
	EventCardUnheld:      ClockRunning,
	EventCardCompleted:   ClockStopped,
	EventEmailReceived:   ClockRunning,
	EventAgentDispatched: ClockRunning,
	EventAgentFinished:   ClockRunning,
	EventNoteAdded:       ClockRunning,
	EventWaitingStarted:  ClockPaused,
	EventWaitingEnded:    ClockRunning,
}

// DefaultClockStateForEventKind answers what a kind means for the clock, and
// reports ok=false when there is no answer. A caller recording an unknown kind
// must state the clock state explicitly: guessing one would silently invent
// budget figures out of an action this service does not understand.
func DefaultClockStateForEventKind(k EventKind) (ClockState, bool) {
	s, ok := defaultClockStateByEventKind[k]
	return s, ok
}

// ============================ Card events ============================

// CardEvent is one action on one card at one moment. The time between an event
// and the next one is attributed to this event's ClockState — that walk is the
// whole of the time accounting, and it is why there are no running totals stored
// anywhere to drift out of step with the log.
//
// BoardID is empty for actions that are not about a particular board — an email
// arriving concerns the card wherever it sits. A card's timeline on a board is
// its board-specific events merged with its card-wide ones.
type CardEvent struct {
	ID           string     `json:"id"`
	CardID       string     `json:"card_id"`
	BoardID      string     `json:"board_id,omitempty"`
	Kind         EventKind  `json:"kind"`
	ClockState   ClockState `json:"clock_state"`
	Actor        string     `json:"actor,omitempty"`
	Summary      string     `json:"summary,omitempty"`
	FromColumnID string     `json:"from_column_id,omitempty"`
	ToColumnID   string     `json:"to_column_id,omitempty"`
	NoteID       string     `json:"note_id,omitempty"`
	// SupersedesEventID names an earlier event this one replaces. The log stays
	// append-only: the earlier event is never touched, and is read as inactive
	// because this one points at it. At most one event may supersede another.
	// Used by time_logged; see EventTimeLogged.
	SupersedesEventID string          `json:"supersedes_event_id,omitempty"`
	Detail            json.RawMessage `json:"detail,omitempty"`
	OccurredAt        time.Time       `json:"occurred_at"`
	RecordedAt        time.Time       `json:"recorded_at"`
}

// CreateCardEventRequest records an action from outside kanban-store — an email
// classifier attaching mail, an agent reporting that it finished, a human noting
// that a reply went out and the ball is now with the other side.
type CreateCardEventRequest struct {
	Kind       EventKind  `json:"kind"`
	BoardID    string     `json:"board_id,omitempty"`
	ClockState ClockState `json:"clock_state,omitempty"`
	Actor      string     `json:"actor,omitempty"`
	Summary    string     `json:"summary,omitempty"`
	// OccurredAt backdates the event to when the action really happened, which is
	// not always when it is reported: a classifier reads a mailbox on a cadence,
	// so the email arrived before anything here heard about it. Empty means now.
	OccurredAt *time.Time      `json:"occurred_at,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
}

// ResolveClockState settles what the event does to the clock: what the caller
// said, or failing that what the kind means. It refuses rather than guessing
// when neither answers.
func (r *CreateCardEventRequest) ResolveClockState() (ClockState, error) {
	if r.ClockState != "" {
		if !ValidClockState(r.ClockState) {
			return "", fmt.Errorf("clock_state must be one of: %s", strings.Join(ClockStateNames(), ", "))
		}
		return r.ClockState, nil
	}
	if s, ok := DefaultClockStateForEventKind(r.Kind); ok {
		return s, nil
	}
	return "", fmt.Errorf("kind %q has no default clock state, so clock_state is required", boundedtext.Value(string(r.Kind)))
}

func (r *CreateCardEventRequest) Validate() error {
	if r.Kind == "" {
		return fmt.Errorf("kind is required")
	}
	if _, err := r.ResolveClockState(); err != nil {
		return err
	}
	if r.Detail != nil && !json.Valid(r.Detail) {
		return fmt.Errorf("detail is not valid JSON")
	}
	return nil
}

// ============================ Card notes ============================

// CardNote is a status update or a summary written onto a card — an email
// classified and condensed, an agent reporting what it changed, a human leaving
// context for the next person.
//
// It lives here rather than in the noteboard item's body because it is activity,
// not the card's text: notes accumulate, each one is stamped and attributed, and
// adding one is an action on the timeline. The body still says what the work is;
// the notes say what has happened to it.
type CardNote struct {
	ID      string `json:"id"`
	CardID  string `json:"card_id"`
	BoardID string `json:"board_id,omitempty"`
	Kind    string `json:"kind"`
	// Visibility says who the note is for: the people working the card, or the
	// requester as well. See NoteVisibility.
	Visibility NoteVisibility `json:"visibility"`
	Body       string         `json:"body"`
	Actor      string         `json:"actor,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

// NoteVisibility is who a note is written for. It is a fact about the note,
// recorded when it is written, and this store shows every note to every caller
// who can view the card: the people working a ticket need the internal ones
// most. What it decides is what a requester-facing reader — a portal, a reply
// sent by mail — may pass on, and that reader asks for `requester` notes only.
type NoteVisibility string

const (
	// NoteVisibilityInternal is for the people working the card and nobody else.
	NoteVisibilityInternal NoteVisibility = "internal"
	// NoteVisibilityRequester may be shown to the person who asked.
	NoteVisibilityRequester NoteVisibility = "requester"
)

// NoteVisibilities is the vocabulary, served by GET /api/note-visibilities.
var NoteVisibilities = []NoteVisibility{NoteVisibilityInternal, NoteVisibilityRequester}

// DefaultNoteVisibility is what a note written without one gets, and what every
// note written before the field existed is. Internal, because the other
// default would show a requester something by omission.
const DefaultNoteVisibility = NoteVisibilityInternal

// ValidNoteVisibility reports whether v is in the vocabulary.
func ValidNoteVisibility(v NoteVisibility) bool {
	for _, known := range NoteVisibilities {
		if v == known {
			return true
		}
	}
	return false
}

// NoteAddedEventDetail is the detail of a note_added event. The event's summary
// is the note's body, so the timeline carries a note's text; the visibility
// rides beside it so a requester-facing reader of the timeline can drop the
// internal ones without fetching each note.
type NoteAddedEventDetail struct {
	Visibility NoteVisibility `json:"visibility"`
}

type CreateCardNoteRequest struct {
	Body    string `json:"body"`
	BoardID string `json:"board_id,omitempty"`
	// Kind labels what sort of update this is — a free-text label such as
	// "summary" or "status". Empty stores "note".
	Kind  string `json:"kind,omitempty"`
	Actor string `json:"actor,omitempty"`
	// Visibility is `internal` or `requester` (GET /api/note-visibilities).
	// Empty stores `internal`: a note is never shown to a requester because
	// its writer left a field out.
	Visibility NoteVisibility `json:"visibility,omitempty"`
	// ClockState lets a note move the clock, since some updates are exactly the
	// moment the ball changes hands ("replied, waiting on them"). Empty leaves the
	// clock where the note_added default puts it.
	ClockState ClockState `json:"clock_state,omitempty"`
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
}

func (r *CreateCardNoteRequest) Validate() error {
	if strings.TrimSpace(r.Body) == "" {
		return fmt.Errorf("body is required")
	}
	if r.Visibility != "" && !ValidNoteVisibility(r.Visibility) {
		return fmt.Errorf("visibility %q is not one of %v (GET /api/note-visibilities)", boundedtext.Value(string(r.Visibility)), NoteVisibilities)
	}
	if r.ClockState != "" && !ValidClockState(r.ClockState) {
		return fmt.Errorf("clock_state must be one of: %s", strings.Join(ClockStateNames(), ", "))
	}
	return nil
}

// ============================ Priority ladder ============================

// UnsetPriorityValue is the noteboard priority of a card nobody has ranked.
// noteboard defaults the column to 0 and sorts descending, so 0 is the bottom of
// the scale and the value 7,904 of this host's items sit at.
//
// It is reserved: a board's ladder may not define a level at 0. That keeps
// "unranked" and "most urgent" from collapsing into one number, which they would
// if P0 were stored literally as zero.
const UnsetPriorityValue = 0

// BoardPriorityLevel is one rung of a board's ladder: a noteboard priority value,
// the name the board gives it, and how long work at that rung should take.
//
// P0 is the top rung, and the top rung is the one with the HIGHEST PriorityValue
// — noteboard sorts priority descending and every list view in the stack already
// depends on that. The P-number is a label on a rung, not the number in the
// database.
type BoardPriorityLevel struct {
	BoardID       string `json:"board_id"`
	PriorityValue int    `json:"priority_value"`
	Label         string `json:"label"`
	// BudgetSeconds is the time limit for work at this rung, measured on the
	// budget clock. Nil means this rung is named but not timed.
	BudgetSeconds *int `json:"budget_seconds" tstype:"number | null,required"`
	// DefaultAutoHoldAtUSD is the spend ceiling a card is GIVEN when it lands on
	// this rung — a starting value written onto the card's noteboard
	// auto_hold_at_usd, which stays editable per card. It is not enforced from
	// here: the scheduler's spend-ceiling-guard reads the card, never the ladder.
	// Nil means this rung suggests no ceiling. Zero is a real default ("stop
	// before spending a cent"), hence the pointer.
	DefaultAutoHoldAtUSD *float64 `json:"default_auto_hold_at_usd" tstype:"number | null,required"`
}

// PriorityLadder is a board's whole ladder, top rung first. An empty ladder means
// the board ignores priorities: its cards get no rung, no label and no limit.
type PriorityLadder struct {
	BoardID string               `json:"board_id"`
	Levels  []BoardPriorityLevel `json:"levels"`
}

// LevelFor finds the rung a stored priority value sits on, or nil when the card
// is unranked or the board ignores priorities.
func (l *PriorityLadder) LevelFor(priorityValue int) *BoardPriorityLevel {
	if l == nil || priorityValue == UnsetPriorityValue {
		return nil
	}
	for i := range l.Levels {
		if l.Levels[i].PriorityValue == priorityValue {
			return &l.Levels[i]
		}
	}
	return nil
}

// SetPriorityLadderRequest replaces a board's ladder outright. A ladder is one
// object — rungs are only meaningful relative to each other — so there is no
// endpoint for editing a single rung.
type SetPriorityLadderRequest struct {
	Levels []BoardPriorityLevel `json:"levels"`
}

func (r *SetPriorityLadderRequest) Validate() error {
	seenValue := map[int]bool{}
	seenLabel := map[string]bool{}
	for _, lv := range r.Levels {
		if lv.PriorityValue == UnsetPriorityValue {
			return fmt.Errorf("priority_value 0 is reserved for unranked cards and cannot be a level; the top level is the HIGHEST value, not zero")
		}
		if lv.PriorityValue < 0 {
			return fmt.Errorf("priority_value must be positive, got %d", lv.PriorityValue)
		}
		if seenValue[lv.PriorityValue] {
			return fmt.Errorf("duplicate priority_value %d", lv.PriorityValue)
		}
		seenValue[lv.PriorityValue] = true
		if strings.TrimSpace(lv.Label) == "" {
			return fmt.Errorf("label is required for priority_value %d", lv.PriorityValue)
		}
		if seenLabel[lv.Label] {
			return fmt.Errorf("duplicate label %q", boundedtext.Value(lv.Label))
		}
		seenLabel[lv.Label] = true
		if lv.BudgetSeconds != nil && *lv.BudgetSeconds <= 0 {
			return fmt.Errorf("budget_seconds must be positive for %q; omit it for a level with no limit", boundedtext.Value(lv.Label))
		}
		if lv.DefaultAutoHoldAtUSD != nil && (*lv.DefaultAutoHoldAtUSD < 0 || math.IsInf(*lv.DefaultAutoHoldAtUSD, 0) || math.IsNaN(*lv.DefaultAutoHoldAtUSD)) {
			return fmt.Errorf("default_auto_hold_at_usd must be zero or more dollars for %q; omit it for a level with no default", lv.Label)
		}
	}
	return nil
}

// ============================ Business hours ============================

// BusinessHours is a board's working week, used to report elapsed time a second
// way: alongside the plain wall-clock figures, not instead of them.
//
// TZID is mandatory and never defaulted, for the same reason noteboard's
// recurrence rules demand one — an offset is not a zone, and a board whose hours
// were guessed reports numbers nobody can check.
type BusinessHours struct {
	TZID  string   `json:"tzid"`
	Days  []string `json:"days"`
	Start string   `json:"start"`
	End   string   `json:"end"`
	// Holidays are dates, in the board's own zone, on which no business time
	// counts however the week is drawn: "2026-12-25". Stored sorted.
	//
	// On a PATCH, absent and empty differ, as they do for the board's other
	// fields: business_hours sent without holidays keeps the stored ones, so a
	// client that edits only the working week cannot wipe them by not knowing
	// they exist; `"holidays":[]` clears them. Clearing the business hours
	// themselves clears these too, since a holiday is a date in their zone.
	Holidays []string `json:"holidays,omitempty"`
}

// HolidayDateLayout is how a holiday is written: a calendar date with no time
// and no zone, because the zone is the board's.
const HolidayDateLayout = "2006-01-02"

// MaxHolidays bounds the list. Twenty years of a generous calendar fits.
const MaxHolidays = 500

// HolidaySet is the holidays as a set of dates, for the day-by-day walk.
func (b *BusinessHours) HolidaySet() map[string]struct{} {
	out := map[string]struct{}{}
	for _, holiday := range b.Holidays {
		out[holiday] = struct{}{}
	}
	return out
}

var weekdayByCode = map[string]time.Weekday{
	"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday,
}

// WeekdaySet turns the day codes into the weekdays they name.
func (b *BusinessHours) WeekdaySet() map[time.Weekday]struct{} {
	out := map[time.Weekday]struct{}{}
	for _, d := range b.Days {
		if wd, ok := weekdayByCode[strings.ToUpper(strings.TrimSpace(d))]; ok {
			out[wd] = struct{}{}
		}
	}
	return out
}

// ParseClockTime reads an "HH:MM" boundary as minutes past midnight.
func ParseClockTime(v string) (int, error) {
	parts := strings.Split(v, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("time %q must be HH:MM", boundedtext.Value(v))
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("hour in %q must be 00-23", boundedtext.Value(v))
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("minute in %q must be 00-59", boundedtext.Value(v))
	}
	return h*60 + m, nil
}

func (b *BusinessHours) Validate() error {
	if b == nil {
		return nil
	}
	if strings.TrimSpace(b.TZID) == "" {
		return fmt.Errorf("tzid is required: business hours without a zone drift across daylight saving")
	}
	if _, err := time.LoadLocation(b.TZID); err != nil {
		return fmt.Errorf("unknown tzid %q: %w", boundedtext.Value(b.TZID), boundedtext.Error(err))
	}
	if len(b.Days) == 0 {
		return fmt.Errorf("days is required (any of MO TU WE TH FR SA SU)")
	}
	for _, d := range b.Days {
		if _, ok := weekdayByCode[strings.ToUpper(strings.TrimSpace(d))]; !ok {
			return fmt.Errorf("unknown day %q; use MO TU WE TH FR SA SU", boundedtext.Value(d))
		}
	}
	start, err := ParseClockTime(b.Start)
	if err != nil {
		return err
	}
	end, err := ParseClockTime(b.End)
	if err != nil {
		return err
	}
	if end <= start {
		return fmt.Errorf("end (%s) must be after start (%s); overnight business hours are not supported", b.End, b.Start)
	}
	if len(b.Holidays) > MaxHolidays {
		return fmt.Errorf("holidays has %d dates; a board takes at most %d", len(b.Holidays), MaxHolidays)
	}
	seen := map[string]struct{}{}
	for _, holiday := range b.Holidays {
		parsed, err := time.Parse(HolidayDateLayout, holiday)
		// Parse accepts "2026-1-5"; only the canonical form is stored, because
		// the day walk compares strings.
		if err != nil || parsed.Format(HolidayDateLayout) != holiday {
			return fmt.Errorf("holiday %q must be a date written YYYY-MM-DD, in the board's zone", boundedtext.Value(holiday))
		}
		if _, twice := seen[holiday]; twice {
			return fmt.Errorf("holiday %s is listed twice", holiday)
		}
		seen[holiday] = struct{}{}
	}
	return nil
}
