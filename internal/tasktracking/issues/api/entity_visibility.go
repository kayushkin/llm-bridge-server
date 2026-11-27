package api

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrPrincipalRefused is a principal whose board access cannot be read
// because principal-store or grant-store refuses it: unknown, disabled, or
// not a principal id at all.
var ErrPrincipalRefused = errors.New("kanban-store: the principal is refused")

// EntitiesVisibleToPrincipal answers, for each ref in order, whether the
// principal may view at least one card that links an entity of entityType by
// that ref — the rule every card read here applies. An administrator sees
// every linked entity. It is for the stores beside this one in the server,
// such as mailstack, whose messages a person may read through a ticket.
//
// A principal principal-store does not know or has disabled wraps
// ErrPrincipalRefused; a store that could not answer is any other error, so a
// failure is never read as "sees nothing".
func (a *API) EntitiesVisibleToPrincipal(principalID, entityType string, refs []string) ([]bool, error) {
	if a.principalEnforcement == nil {
		return nil, errors.New("kanban-store has no grant-store configured, so no principal's access can be read")
	}
	access, administrator, refusal := a.boardAccessOfPrincipal(principalID)
	if refusal != nil {
		if refusal.status == http.StatusUnauthorized {
			return nil, fmt.Errorf("%w: %s", ErrPrincipalRefused, refusal.message)
		}
		return nil, errors.New(refusal.message)
	}
	visible := make([]bool, len(refs))
	for index, ref := range refs {
		linked, err := a.store.ListCardsByEntity(entityType, ref)
		if err != nil {
			return nil, fmt.Errorf("kanban-store: the cards linking %s %s: %w", entityType, ref, err)
		}
		for _, card := range linked {
			if administrator {
				visible[index] = true
				break
			}
			seen, err := a.cardVisibleTo(access, card.CardID)
			if err != nil {
				return nil, fmt.Errorf("kanban-store: whether %s may view card %s: %w", principalID, card.CardID, err)
			}
			if seen {
				visible[index] = true
				break
			}
		}
	}
	return visible, nil
}
