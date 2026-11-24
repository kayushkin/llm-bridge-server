package tools

import (
	"context"

	"github.com/kayushkin/tool-store/schema"
)

// EndTurn returns a tool that signals the end of a turn.
// The model MUST call this as the last tool in every turn.
// Its presence creates a decision point that encourages batching other work first.
func EndTurn() Impl {
	type input struct {
		Reason string `json:"reason"`
	}
	return Impl{
		Name: "end_turn",
		Description: `Signal that you're done with tool calls for this turn. You MUST call this as the LAST tool in every response.

Before calling end_turn, ask yourself:
- Did I just do a write/edit? Add a build or grep to verify it.
- Did I just read one file? Are there other files I need for the same understanding?
- Could I batch another quick operation into this turn?

Each API round-trip costs tokens. Maximize what you accomplish before ending the turn.`,
		InputSchema: schema.Props([]string{"reason"}, map[string]any{
			"reason": schema.Str("Brief note on what you accomplished this turn"),
		}),
		Run: func(ctx context.Context, raw string) (string, error) {
			in, err := schema.Parse[input](raw)
			if err != nil {
				return "turn ended", nil
			}
			_ = in
			return "turn ended", nil
		},
	}
}
