package model

import (
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/kanban-store/internal/boundedtext"
)

// WorkState is where a piece of work stands, in words every board shares.
//
// Boards are views, not containers: one card can sit on several boards, and
// each board names its columns its own way ("Waiting on reply", "Blocked").
// A column's work_state maps its name onto this one vocabulary, so a reader
// that spans boards — a project rollup, an orchestrator counting what is
// waiting on someone — can count cards without knowing any board's names.
//
// It is separate from auto_status on purpose. auto_status writes noteboard's
// open/done/archived onto the item when a card lands in the column; work_state
// writes nothing anywhere and is only read. A `done` or `dropped` work_state
// does not imply any auto_status, and setting one never sets the other: doing
// so would make classifying a column silently start editing noteboard items.
// A column that should also close its todos sets both.
type WorkState string

const (
	// WorkStateNotStarted — accepted as work, nobody has begun it.
	WorkStateNotStarted WorkState = "not_started"
	// WorkStateWorking — someone is doing it now.
	WorkStateWorking WorkState = "working"
	// WorkStateWaiting — blocked on a person or on another piece of work.
	WorkStateWaiting WorkState = "waiting"
	// WorkStateDone — finished.
	WorkStateDone WorkState = "done"
	// WorkStateDropped — will not be done.
	WorkStateDropped WorkState = "dropped"
)

// WorkStateMeaning is one entry of GET /api/work-states: a state and what it
// means, so a person mapping a board's columns reads the meaning once, here.
type WorkStateMeaning struct {
	WorkState WorkState `json:"work_state"`
	Meaning   string    `json:"meaning"`
}

// WorkStates is the whole vocabulary, in the order work usually travels it and
// the order GET /api/work-states serves it.
var WorkStates = []WorkStateMeaning{
	{WorkStateNotStarted, "accepted as work, and nobody has begun it"},
	{WorkStateWorking, "someone is doing it now"},
	{WorkStateWaiting, "blocked on a person or on another piece of work"},
	{WorkStateDone, "finished"},
	{WorkStateDropped, "will not be done"},
}

// ValidWorkState reports whether state is in the vocabulary, exactly as
// written: no case folding and no trimming, so what is stored is what was sent.
func ValidWorkState(state WorkState) bool {
	for _, known := range WorkStates {
		if state == known.WorkState {
			return true
		}
	}
	return false
}

// ErrUnknownWorkState names the whole vocabulary, because a caller who guessed
// wrong cannot guess right from a bare rejection.
func ErrUnknownWorkState(raw string) error {
	names := make([]string, 0, len(WorkStates))
	for _, known := range WorkStates {
		names = append(names, string(known.WorkState))
	}
	return fmt.Errorf("unknown work_state %q: use one of %s (GET /api/work-states)", boundedtext.Value(raw), strings.Join(names, ", "))
}

// validateWorkStateField accepts absent (leave it alone), empty (clear it) and
// any state the vocabulary names; anything else is refused naming them all.
func validateWorkStateField(raw *string) error {
	if raw == nil || *raw == "" {
		return nil
	}
	if !ValidWorkState(WorkState(*raw)) {
		return ErrUnknownWorkState(*raw)
	}
	return nil
}

// CardWorkStateSource says which placement a card's shared work_state was read
// from. BoardID, ColumnID and ColumnName are left out when the caller cannot
// view that board: the state is the card's, but the board is not named to
// someone who cannot see it.
type CardWorkStateSource struct {
	BoardID    string    `json:"board_id,omitempty"`
	ColumnID   string    `json:"column_id,omitempty"`
	ColumnName string    `json:"column_name,omitempty"`
	MovedAt    time.Time `json:"moved_at"`
}
