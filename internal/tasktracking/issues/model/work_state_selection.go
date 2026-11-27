package model

import "time"

// The rule that picks a card's shared work_state out of its placements, and
// its input. Nothing here goes on the wire, so tygo.yaml leaves this file out
// of the TypeScript render.

// PlacementWorkState is one placement of a card, with what its column means
// and when the card last moved there. It is the input to SharedWorkState.
type PlacementWorkState struct {
	BoardID    string
	ColumnID   string
	ColumnName string
	// WorkState is nil when the column has none.
	WorkState *WorkState
	// MovedAt is the placement's updated_at: set when the card is placed on
	// the board and on every move there, reorders within a column included.
	MovedAt time.Time
}

// SharedWorkState picks a card's one shared state out of its placements: the
// placement moved most recently wins, whichever board it is on, and a tie goes
// to the lower board id so the answer is the same on every read. The winner's
// column decides — if that column has no work_state the card has none, even
// when an older placement's column has one, because the latest move is what
// someone last said about the work.
//
// Returns nil, nil for a card with no placements.
func SharedWorkState(placements []PlacementWorkState) (*WorkState, *CardWorkStateSource) {
	var latest *PlacementWorkState
	for i := range placements {
		candidate := &placements[i]
		if latest == nil || candidate.MovedAt.After(latest.MovedAt) ||
			(candidate.MovedAt.Equal(latest.MovedAt) && candidate.BoardID < latest.BoardID) {
			latest = candidate
		}
	}
	if latest == nil {
		return nil, nil
	}
	return latest.WorkState, &CardWorkStateSource{
		BoardID: latest.BoardID, ColumnID: latest.ColumnID, ColumnName: latest.ColumnName, MovedAt: latest.MovedAt,
	}
}
