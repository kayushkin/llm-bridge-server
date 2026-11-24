package api

import (
	"fmt"

	"github.com/kayushkin/kanban-store/internal/model"
)

// A rung of a board's priority ladder may carry default_auto_hold_at_usd: the
// spend ceiling a card is given when it takes that priority. It is a starting
// value, not a rule. kanban-store writes it onto the card's noteboard
// auto_hold_at_usd, where it stays editable per card, and the scheduler enforces
// the card's value without ever reading the ladder.
//
// When it is written:
//   - A card created on a board with a priority and no ceiling of its own.
//   - A card attached to a board while it has a priority and no ceiling.
//   - A priority change through PATCH /api/cards/{id} that does not also set
//     auto_hold_at_usd, when the card has no ceiling or still carries the
//     default of the rung it is leaving. A ceiling someone set by hand to any
//     other number is left alone.
//
// It is never written when the ladder is edited: cards that already exist keep
// what they have. A priority changed in noteboard directly never passes through
// here and gets no default.
//
// A card on several boards takes the LOWEST default any of those boards gives
// its priority, so being on a second board never raises what it may spend.

// lowestDefaultSpendCeilingForPriority returns the lowest
// default_auto_hold_at_usd that any of the boards gives this priority value, or
// nil when none of them has a rung for it with a default.
func (a *API) lowestDefaultSpendCeilingForPriority(boardIDs []string, priorityValue int) (*float64, error) {
	var lowest *float64
	for _, boardID := range boardIDs {
		ladder, err := a.store.GetPriorityLadder(boardID)
		if err != nil {
			return nil, fmt.Errorf("read priority ladder of board %s: %w", boardID, err)
		}
		level := ladder.LevelFor(priorityValue)
		if level == nil || level.DefaultAutoHoldAtUSD == nil {
			continue
		}
		if lowest == nil || *level.DefaultAutoHoldAtUSD < *lowest {
			value := *level.DefaultAutoHoldAtUSD
			lowest = &value
		}
	}
	return lowest, nil
}

// boardIDsOfCard lists every board the card is placed on.
func (a *API) boardIDsOfCard(cardID string) ([]string, error) {
	placements, err := a.store.ListPlacementsByCard(cardID)
	if err != nil {
		return nil, fmt.Errorf("list placements of card %s: %w", cardID, err)
	}
	boardIDs := make([]string, 0, len(placements))
	for _, placement := range placements {
		boardIDs = append(boardIDs, placement.BoardID)
	}
	return boardIDs, nil
}

// spendCeilingOfNoteboardItem reads auto_hold_at_usd off a noteboard item. Nil
// means the card has no ceiling. A value of any other shape is an error, not
// "no ceiling": treating garbage as unlimited would let a default overwrite it.
func spendCeilingOfNoteboardItem(item map[string]any) (*float64, error) {
	raw, present := item["auto_hold_at_usd"]
	if !present || raw == nil {
		return nil, nil
	}
	value, ok := raw.(float64)
	if !ok {
		return nil, fmt.Errorf("noteboard item %v has auto_hold_at_usd of type %T, want a number", item["id"], raw)
	}
	return &value, nil
}

// spendCeilingForPriorityChange decides what a priority PATCH should also write
// to auto_hold_at_usd. It returns nil when the ceiling should stay as it is.
func (a *API) spendCeilingForPriorityChange(cardID string, itemBefore map[string]any, newPriorityValue int) (*float64, error) {
	if newPriorityValue == model.UnsetPriorityValue {
		return nil, nil
	}
	boardIDs, err := a.boardIDsOfCard(cardID)
	if err != nil {
		return nil, err
	}
	newDefault, err := a.lowestDefaultSpendCeilingForPriority(boardIDs, newPriorityValue)
	if err != nil || newDefault == nil {
		return nil, err
	}
	currentCeiling, err := spendCeilingOfNoteboardItem(itemBefore)
	if err != nil {
		return nil, err
	}
	if currentCeiling == nil {
		return newDefault, nil
	}
	oldDefault, err := a.lowestDefaultSpendCeilingForPriority(boardIDs, model.PriorityOfNoteboardItem(itemBefore))
	if err != nil {
		return nil, err
	}
	if oldDefault != nil && *oldDefault == *currentCeiling {
		return newDefault, nil
	}
	return nil, nil
}

// priorityValueOfPatch reads the priority a PATCH body sets. JSON null means
// the card becomes unranked.
func priorityValueOfPatch(raw any) (int, error) {
	switch value := raw.(type) {
	case nil:
		return model.UnsetPriorityValue, nil
	case float64:
		if value != float64(int(value)) {
			return 0, fmt.Errorf("priority must be a whole number, got %v", value)
		}
		return int(value), nil
	}
	return 0, fmt.Errorf("priority must be a number or null, got %T", raw)
}

// applyDefaultSpendCeilingOnAttach gives a card that arrives on a board with a
// priority and no ceiling the lowest default its boards give that priority.
func (a *API) applyDefaultSpendCeilingOnAttach(cardID string, item map[string]any) error {
	priorityValue := model.PriorityOfNoteboardItem(item)
	if priorityValue == model.UnsetPriorityValue {
		return nil
	}
	currentCeiling, err := spendCeilingOfNoteboardItem(item)
	if err != nil || currentCeiling != nil {
		return err
	}
	boardIDs, err := a.boardIDsOfCard(cardID)
	if err != nil {
		return err
	}
	defaultCeiling, err := a.lowestDefaultSpendCeilingForPriority(boardIDs, priorityValue)
	if err != nil || defaultCeiling == nil {
		return err
	}
	if _, err := a.noteboard.PatchItem(cardID, map[string]any{"auto_hold_at_usd": *defaultCeiling}); err != nil {
		return fmt.Errorf("write default spend ceiling to card %s: %w", cardID, err)
	}
	return nil
}
