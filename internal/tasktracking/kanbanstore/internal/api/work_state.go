package api

import (
	"net/http"

	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/noteboard"
)

// workStates serves the shared work-state vocabulary, each state with its
// meaning. A column's work_state must be one of these.
func (a *API) workStates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, model.WorkStates)
}

// sharedWorkStateForCaller applies model.SharedWorkState and then hides the
// source board from a caller who cannot view it. The state itself is the
// card's, the same for every reader; which board it came from is not named to
// someone who cannot see that board, as placements and a ticket's states are
// not.
func sharedWorkStateForCaller(placements []model.PlacementWorkState, access *PrincipalBoardAccess) (*model.WorkState, *model.CardWorkStateSource) {
	state, source := model.SharedWorkState(placements)
	if source != nil && access != nil && access.LevelOn(source.BoardID) < BoardAccessView {
		source = &model.CardWorkStateSource{MovedAt: source.MovedAt}
	}
	return state, source
}

// cardWorkStatesForCaller reads the shared work_state of many cards in one
// query. A card on no board is absent from the map.
func (a *API) cardWorkStatesForCaller(cardIDs []string, access *PrincipalBoardAccess) (map[string]cardWorkState, error) {
	placementsByCard, err := a.store.ListPlacementWorkStatesForCards(cardIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]cardWorkState, len(placementsByCard))
	for cardID, placements := range placementsByCard {
		state, source := sharedWorkStateForCaller(placements, access)
		out[cardID] = cardWorkState{State: state, Source: source}
	}
	return out, nil
}

type cardWorkState struct {
	State  *model.WorkState
	Source *model.CardWorkStateSource
}

// entityCards is GET /api/entities/{type}/{ref}/cards: every card linked to
// the entity, oldest link first, each with its noteboard item and its shared
// work_state — enough for a rollup (a project's cards, counted by state) in
// one call. It costs one query here and one noteboard call, however many cards
// the entity has: the placements come back with the links, so neither the
// visibility check nor the work_state reads per card.
func (a *API) entityCards(w http.ResponseWriter, r *http.Request, entityType, entityRef string) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	linked, err := a.store.ListCardsByEntity(entityType, entityRef)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	access := principalBoardAccessFrom(r)
	visible := linked[:0]
	for _, card := range linked {
		if access == nil || canViewAnyPlacement(access, card.Placements) {
			visible = append(visible, card)
		}
	}
	ids := make([]string, len(visible))
	for i, card := range visible {
		ids[i] = card.CardID
	}
	itemsByID := map[string]noteboard.Item{}
	if len(ids) > 0 {
		// No filter and no sort: noteboard answers every live item named, held
		// ones included, and lists the rest as missing_ids.
		result, err := a.noteboard.QueryItems(noteboard.ItemsQuery{IDs: ids, IncludeItems: true})
		if err != nil {
			writeError(w, 502, err.Error())
			return
		}
		for i, id := range result.IDs {
			itemsByID[id] = result.Items[i]
		}
	}
	out := make([]model.EntityCardView, len(visible))
	for i, card := range visible {
		state, source := sharedWorkStateForCaller(card.Placements, access)
		// A card whose item is gone from noteboard keeps item null, so a
		// caller can spot the orphan.
		out[i] = model.EntityCardView{CardID: card.CardID, Item: itemsByID[card.CardID], WorkState: state, WorkStateSource: source}
	}
	writeJSON(w, 200, out)
}

// canViewAnyPlacement is cardVisibleTo for a card whose placements are
// already read: visible when it sits on at least one board the caller can view.
func canViewAnyPlacement(access *PrincipalBoardAccess, placements []model.PlacementWorkState) bool {
	for _, placement := range placements {
		if access.LevelOn(placement.BoardID) >= BoardAccessView {
			return true
		}
	}
	return false
}
