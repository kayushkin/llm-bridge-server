package model

import "time"

// The shapes kanban-store answers time questions in. They live here rather than
// beside the walk that fills them because a board view carries a card's time
// summary inline, and the walk imports these — not the other way round.

// TimeSegment is the stretch between one event and the next, carrying the clock
// state the earlier event put the card into.
type TimeSegment struct {
	EventID    string     `json:"event_id"`
	ClockState ClockState `json:"clock_state"`
	From       time.Time  `json:"from"`
	To         time.Time  `json:"to"`
	Seconds    float64    `json:"seconds"`
	// Open marks the segment that is still running: it ends at the moment the
	// figures were computed, not at an event, so it grows until something happens.
	Open bool `json:"open"`
}

// TimelineEntry is one event with the time either side of it: how long since the
// previous action, and how long the card then spent in the state this action put
// it in.
type TimelineEntry struct {
	Event                     CardEvent  `json:"event"`
	Note                      *CardNote  `json:"note,omitempty"`
	SecondsSincePreviousEvent float64    `json:"seconds_since_previous_event"`
	SegmentSeconds            float64    `json:"segment_seconds"`
	SegmentOpen               bool       `json:"segment_open"`
	CountsAgainstBudget       bool       `json:"counts_against_budget"`
	ClockState                ClockState `json:"clock_state"`
	// SupersededByEventID is set on an event another one has replaced — a time
	// entry that was corrected. It stays on the timeline as it was logged, and
	// this is what says it no longer counts.
	SupersededByEventID string `json:"superseded_by_event_id,omitempty"`
}

// CardTimeSummary is the card's time, answered three ways.
type CardTimeSummary struct {
	AsOf time.Time `json:"as_of"`

	// ClockState is where the card stands right now — the state its most recent
	// action left it in.
	ClockState   ClockState `json:"clock_state"`
	FirstEventAt *time.Time `json:"first_event_at,omitempty"`
	LastEventAt  *time.Time `json:"last_event_at,omitempty"`
	EventCount   int        `json:"event_count"`

	// ElapsedSeconds is wall clock from the first action to now, or to the action
	// that stopped the card. Your 25.5 hours.
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	// BudgetClockSeconds is the part of that the work was actually available to be
	// worked on. Your 1.5 hours. This is what the limit is measured against.
	BudgetClockSeconds float64 `json:"budget_clock_seconds"`
	// WaitingSeconds is the rest: time the ball was with someone else. Your 24.
	WaitingSeconds float64 `json:"waiting_seconds"`
	// LoggedSeconds is the time people have recorded by hand on the card: the
	// sum of its active time entries. It is a different thing from the three
	// figures above, which are read off the clock — those say how long the card
	// took, this says how much work somebody says they did. An entry that has
	// been replaced does not count; its replacement does.
	LoggedSeconds int64 `json:"logged_seconds"`

	// BusinessHoursElapsedSeconds and BusinessHoursBudgetClockSeconds repeat the
	// two figures above counting only the board's working week. Both are absent
	// when the board has no business hours configured — an unconfigured board
	// reports nothing here rather than a guessed nine-to-five.
	BusinessHoursElapsedSeconds     *float64 `json:"business_hours_elapsed_seconds,omitempty"`
	BusinessHoursBudgetClockSeconds *float64 `json:"business_hours_budget_clock_seconds,omitempty"`

	// The rung this card's priority sits on, if its board has a ladder and the
	// card is ranked. PriorityLabel is what the board calls it — "P0".
	PriorityValue *int   `json:"priority_value,omitempty"`
	PriorityLabel string `json:"priority_label,omitempty"`
	// BudgetSeconds is the limit that rung sets. Absent means no limit applies,
	// either because the board ignores priorities, the card is unranked, or the
	// rung is named but untimed — and then Remaining and OverBudget are absent too,
	// because there is nothing to be over.
	BudgetSeconds          *int     `json:"budget_seconds,omitempty"`
	BudgetRemainingSeconds *float64 `json:"budget_remaining_seconds,omitempty"`
	OverBudget             *bool    `json:"over_budget,omitempty"`

	Segments []TimeSegment `json:"segments,omitempty"`
}

// CardTimeline is the answer to "what happened to this card, in what order, and
// how long did each step take".
type CardTimeline struct {
	CardID  string              `json:"card_id"`
	BoardID string              `json:"board_id,omitempty"`
	Summary *CardTimeSummary    `json:"summary" tstype:"CardTimeSummary,required"`
	Entries []TimelineEntry     `json:"entries"`
	Notes   []CardNote          `json:"notes"`
	Level   *BoardPriorityLevel `json:"priority_level,omitempty"`
}
