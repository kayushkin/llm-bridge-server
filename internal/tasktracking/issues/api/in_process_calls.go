package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/db"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/model"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/noteboard"
	"github.com/kayushkin/llm-bridge/msg"
)

// The reads and writes the server this store is mounted in makes of it, as
// Go calls. Each one is the code its route runs, and each holds the caller to
// the rule the route table holds a request to (authorizeRequest), so a call
// is refused exactly as the route would refuse it: a board or card the caller
// may not view is a 404 Refusal, one it may view but not change a 403.

// Refusal is kanban-store saying no to a Go call: the status its route
// answers and the message it writes.
type Refusal struct {
	Status  int
	Message string
	// body is what the route writes when it is not {"error": Message}: a
	// classification refusal's code, or noteboard's own answer as it came.
	body any
	raw  []byte
}

func (refusal *Refusal) Error() string {
	return fmt.Sprintf("status %d: %s", refusal.Status, refusal.Message)
}

// refusalFrom is an access refusal as a Go error.
func refusalFrom(refusal *accessRefusal) *Refusal {
	return &Refusal{Status: refusal.status, Message: refusal.message}
}

// writeRefusal answers err as its route does: a Refusal with its status and
// body, anything else 500.
func writeRefusal(w http.ResponseWriter, err error) {
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch {
	case refusal.raw != nil:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(refusal.Status)
		_, _ = w.Write(refusal.raw)
	case refusal.body != nil:
		writeJSON(w, refusal.Status, refusal.body)
	default:
		writeError(w, refusal.Status, refusal.Message)
	}
}

// dbRefusal is a database error as the routes answer it (mapDBErr).
func dbRefusal(err error) *Refusal {
	if errors.Is(err, db.ErrBoardChanged) {
		return &Refusal{Status: http.StatusConflict, Message: err.Error()}
	}
	if errors.Is(err, db.ErrNotFound) {
		return &Refusal{Status: http.StatusNotFound, Message: "not found"}
	}
	return &Refusal{Status: http.StatusInternalServerError, Message: err.Error()}
}

// authorizeCall is the gate for a Go call, as principalGate is for a
// request: the internal service and an administrator pass unrestricted (nil
// access), and any other principal is read from principal-store and
// grant-store and held to rule, the route table's rule for what it asks. A
// nil rule is a route the handler filters itself.
func (a *API) authorizeCall(caller Caller, rule func(*PrincipalBoardAccess) (*accessRefusal, error)) (*PrincipalBoardAccess, error) {
	if a.principalEnforcement == nil {
		return nil, &Refusal{Status: http.StatusInternalServerError, Message: "kanban-store has no grant-store configured, so no request can be authorized"}
	}
	if caller.InternalService {
		return nil, nil
	}
	if caller.PrincipalID == "" {
		return nil, &Refusal{Status: http.StatusUnauthorized, Message: "no caller: this store answers a principal or the internal service, as the server it is mounted in verified them"}
	}
	access, administrator, refusal := a.boardAccessOfPrincipal(caller.PrincipalID)
	if refusal != nil {
		return nil, refusalFrom(refusal)
	}
	if administrator {
		return nil, nil
	}
	if rule != nil {
		refusal, err := rule(access)
		if err != nil {
			return nil, err
		}
		if refusal != nil {
			return nil, refusalFrom(refusal)
		}
	}
	return access, nil
}

func (a *API) viewOnBoard(boardID, what string) func(*PrincipalBoardAccess) (*accessRefusal, error) {
	return func(access *PrincipalBoardAccess) (*accessRefusal, error) {
		return requireOnBoard(access, boardID, BoardAccessView, what), nil
	}
}

func (a *API) onCard(cardID string, needed BoardAccessLevel, what string) func(*PrincipalBoardAccess) (*accessRefusal, error) {
	return func(access *PrincipalBoardAccess) (*accessRefusal, error) {
		return a.requireOnCard(access, cardID, needed, what)
	}
}

// EntityCards is GET /api/entities/{type}/{ref}/cards: every card linked to
// the entity that the caller may see, oldest link first.
func (a *API) EntityCards(caller Caller, entityType, entityRef string) ([]model.EntityCardView, error) {
	access, err := a.authorizeCall(caller, nil)
	if err != nil {
		return nil, err
	}
	return a.entityCardsFor(access, entityType, entityRef)
}

// CardLinks is GET /api/cards/{id}/links.
func (a *API) CardLinks(caller Caller, cardID string) ([]model.CardLink, error) {
	if _, err := a.authorizeCall(caller, a.onCard(cardID, BoardAccessView, "GET /api/cards/"+cardID+"/links")); err != nil {
		return nil, err
	}
	return a.cardLinksOf(cardID)
}

// RecordCardEvent is POST /api/cards/{id}/events.
func (a *API) RecordCardEvent(caller Caller, cardID string, request model.CreateCardEventRequest) (*model.CardEvent, error) {
	access, err := a.authorizeCall(caller, a.onCard(cardID, BoardAccessEdit, "POST /api/cards/"+cardID+"/events"))
	if err != nil {
		return nil, err
	}
	actor := ""
	if access != nil {
		actor = access.PrincipalID
	}
	return a.recordCardEvent(cardID, request, actor)
}

// CardTags is the tags of a card, as GET /api/cards/{id} answers them on its
// noteboard item. A noteboard item on no board is not a card (404), and a
// card whose noteboard item is gone has no tags to read (404 too).
func (a *API) CardTags(caller Caller, cardID string) ([]string, error) {
	if _, err := a.authorizeCall(caller, a.onCard(cardID, BoardAccessView, "GET /api/cards/"+cardID)); err != nil {
		return nil, err
	}
	placements, err := a.store.ListPlacementsByCard(cardID)
	if err != nil {
		return nil, &Refusal{Status: http.StatusInternalServerError, Message: err.Error()}
	}
	if len(placements) == 0 {
		return nil, &Refusal{Status: http.StatusNotFound, Message: "not found"}
	}
	item, err := a.noteboard.GetItem(cardID)
	if noteboard.IsNotFound(err) {
		return nil, &Refusal{Status: http.StatusNotFound, Message: "card " + cardID + " has no noteboard item"}
	}
	if err != nil {
		return nil, &Refusal{Status: http.StatusBadGateway, Message: err.Error()}
	}
	tags, err := tagsOfNoteboardItem(item)
	if err != nil {
		return nil, &Refusal{Status: http.StatusBadGateway, Message: err.Error()}
	}
	return tags, nil
}

// BoardEffectiveDefaults is GET /api/boards/{id}/effective-defaults?tag=….
func (a *API) BoardEffectiveDefaults(caller Caller, boardID string, tags []string) (*model.EffectiveDefaults, error) {
	if _, err := a.authorizeCall(caller, a.viewOnBoard(boardID, "GET /api/boards/"+boardID+"/effective-defaults")); err != nil {
		return nil, err
	}
	resolved, err := a.effectiveDefaultsFor(boardID, tags)
	if err != nil {
		return nil, dbRefusal(err)
	}
	return resolved, nil
}

// CardEffectiveDefaults is GET /api/boards/{id}/cards/{card_id}/effective-defaults.
func (a *API) CardEffectiveDefaults(caller Caller, boardID, cardID string) (*model.EffectiveDefaults, error) {
	if _, err := a.authorizeCall(caller, a.viewOnBoard(boardID, "GET /api/boards/"+boardID+"/cards/"+cardID+"/effective-defaults")); err != nil {
		return nil, err
	}
	return a.cardEffectiveDefaultsFor(boardID, cardID)
}

// BoardClassification is GET /api/boards/{id}/classification.
func (a *API) BoardClassification(caller Caller, boardID string) (*msg.BoardClassification, error) {
	access, err := a.authorizeCall(caller, a.viewOnBoard(boardID, "GET /api/boards/"+boardID+"/classification"))
	if err != nil {
		return nil, err
	}
	return a.boardClassificationFor(access, boardID)
}

// BoardCardPage is GET /api/boards/{id}/card-page, with query its query
// string's values.
func (a *API) BoardCardPage(caller Caller, boardID string, query url.Values) (*model.BoardCardPage, error) {
	access, err := a.authorizeCall(caller, a.viewOnBoard(boardID, "GET /api/boards/"+boardID+"/card-page"))
	if err != nil {
		return nil, err
	}
	return a.boardCardPageFor(access, boardID, query)
}

// PublishClassificationDecision is POST /api/boards/{id}/classification/decisions,
// the classifier's alone: only the internal service may publish. created is
// false when the same decision was published before.
func (a *API) PublishClassificationDecision(caller Caller, publication msg.BoardClassificationDecisionPublication) (*model.ClassificationDecision, bool, error) {
	if !caller.InternalService {
		// The route table's answer for a principal: unseen is 404, seen 403.
		// An administrator passes the gate and is refused by the handler.
		if _, err := a.authorizeCall(caller, func(access *PrincipalBoardAccess) (*accessRefusal, error) {
			if access.LevelOn(publication.BoardID) < BoardAccessView {
				return refuseUnseen, nil
			}
			return nil, nil
		}); err != nil {
			return nil, false, err
		}
		return nil, false, &Refusal{Status: http.StatusForbidden, Message: "decisions are published by the classifier, as the internal service",
			body: map[string]string{"code": "service_token_required", "error": "decisions are published by the classifier, as the internal service"}}
	}
	return a.publishClassificationDecisionFor(publication.BoardID, publication)
}

// Board is GET /api/boards/{id}: the board's own record.
func (a *API) Board(caller Caller, boardID string) (*model.Board, error) {
	if _, err := a.authorizeCall(caller, a.viewOnBoard(boardID, "GET /api/boards/"+boardID)); err != nil {
		return nil, err
	}
	board, err := a.store.GetBoard(boardID)
	if err != nil {
		return nil, dbRefusal(err)
	}
	return board, nil
}

// EntityTags is GET /api/entities/{type}/{ref}/tags. Entity tags are one tag
// space shared by every board and every kind of entity, so a principal is
// refused them, as the route refuses one.
func (a *API) EntityTags(caller Caller, entityType, entityRef string) ([]model.EntityTag, error) {
	if _, err := a.authorizeCall(caller, func(*PrincipalBoardAccess) (*accessRefusal, error) {
		return &accessRefusal{status: http.StatusForbidden, message: "entity tags are not available under principal enforcement"}, nil
	}); err != nil {
		return nil, err
	}
	tags, err := a.store.ListEntityTags(entityType, entityRef)
	if err != nil {
		return nil, &Refusal{Status: http.StatusInternalServerError, Message: err.Error()}
	}
	return tags, nil
}

// CardPlacementsAndLinks is the part of GET /api/cards/{id} that says where a
// card sits and what it links: the placements the caller may see, and every
// link. A noteboard item on no board is not a card (404).
func (a *API) CardPlacementsAndLinks(caller Caller, cardID string) ([]*model.Placement, []model.CardLink, error) {
	access, err := a.authorizeCall(caller, a.onCard(cardID, BoardAccessView, "GET /api/cards/"+cardID))
	if err != nil {
		return nil, nil, err
	}
	placements, err := a.store.ListPlacementsByCard(cardID)
	if err != nil {
		return nil, nil, &Refusal{Status: http.StatusInternalServerError, Message: err.Error()}
	}
	if len(placements) == 0 {
		return nil, nil, &Refusal{Status: http.StatusNotFound, Message: "not found"}
	}
	visible := []*model.Placement{}
	for _, placement := range placements {
		if access == nil || access.LevelOn(placement.BoardID) >= BoardAccessView {
			visible = append(visible, placement)
		}
	}
	links, err := a.store.ListCardLinks(cardID)
	if err != nil {
		return nil, nil, &Refusal{Status: http.StatusInternalServerError, Message: err.Error()}
	}
	return visible, links, nil
}

// ColumnCard is one card of a column page: where it sits and what it links.
type ColumnCard struct {
	Placement *model.Placement
	Links     []model.CardLink
}

// ColumnCardsAndLinks is one page of a column's cards in stored order, as GET
// /api/columns/{id}/cards?limit=&offset= pages them with no filter: each
// card's placement and links, the column's board and how many cards the
// column holds.
func (a *API) ColumnCardsAndLinks(caller Caller, columnID string, limit, offset int) (string, int, []ColumnCard, error) {
	column, err := a.store.GetColumn(columnID)
	if err != nil {
		return "", 0, nil, dbRefusal(err)
	}
	if _, err := a.authorizeCall(caller, a.viewOnBoard(column.BoardID, "GET /api/columns/"+columnID+"/cards")); err != nil {
		return "", 0, nil, err
	}
	if offset < 0 {
		offset = 0
	}
	placements, _, total, err := a.columnPage(columnID, db.CardFilter{}, cardContentQuery{}, limit, offset)
	if err != nil {
		return "", 0, nil, cardReadRefusal(err)
	}
	ids := make([]string, len(placements))
	for i, placement := range placements {
		ids[i] = placement.CardID
	}
	linksByCard, err := a.store.ListCardLinksForCards(ids)
	if err != nil {
		return "", 0, nil, &Refusal{Status: http.StatusInternalServerError, Message: err.Error()}
	}
	cards := make([]ColumnCard, len(placements))
	for i, placement := range placements {
		cards[i] = ColumnCard{Placement: placement, Links: linksByCard[placement.CardID]}
	}
	return column.BoardID, total, cards, nil
}
