package server

// rule-store's reads in this process: the sessions from this server's store,
// and the cards, columns and entity tags from kanban-store's Go methods, as
// the internal service, as the standalone rule-store read them with the two
// service tokens. The session feed rule-store follows stays a request to
// this server's own /session-events, through the HTTP reader it is given,
// and so do the calls a live rule makes, whose method, path and answer
// rule-store records on each run.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/rulestore"
	kanbanapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/api"
)

// RuleStoreSessions is this server's sessions as rule-store reads them: the
// list and one session from the store, as GET /sessions and GET
// /sessions/{id} answer the internal service, and the feed through feed.
func (s *Server) RuleStoreSessions(feed *rulestore.LLMBridge) rulestore.Sessions {
	return ruleStoreSessions{server: s, feed: feed}
}

type ruleStoreSessions struct {
	server *Server
	feed   *rulestore.LLMBridge
}

func (sessions ruleStoreSessions) ListSessions(context.Context) ([]json.RawMessage, error) {
	stored, err := sessions.server.store.ListSessionsPaged(0, 0)
	if err != nil {
		return nil, err
	}
	encoded := make([]json.RawMessage, len(stored))
	for i, session := range stored {
		if encoded[i], err = json.Marshal(session); err != nil {
			return nil, fmt.Errorf("session %s does not encode: %w", session.SessionID, err)
		}
	}
	return encoded, nil
}

// GetSession answers found false where GET /sessions/{id} answers 404: for
// any failure to read the row.
func (sessions ruleStoreSessions) GetSession(_ context.Context, sessionID string) (rulestore.SessionSnapshot, bool, error) {
	session, err := sessions.server.store.GetSession(sessionID)
	if err != nil {
		return rulestore.SessionSnapshot{}, false, nil
	}
	encoded, err := json.Marshal(session)
	if err != nil {
		return rulestore.SessionSnapshot{}, false, fmt.Errorf("session %s does not encode: %w", sessionID, err)
	}
	snapshot, err := rulestore.ParseSessionSnapshot(encoded)
	return snapshot, true, err
}

func (sessions ruleStoreSessions) FollowSessions(ctx context.Context, lastEventID string, handler rulestore.FeedHandler) (string, error) {
	return sessions.feed.FollowSessions(ctx, lastEventID, handler)
}

// RuleStoreKanban is kanban-store as rule-store reads it. It answers once
// EnableKanbanStore has run.
func (s *Server) RuleStoreKanban() rulestore.KanbanFacts {
	return ruleStoreKanban{server: s}
}

type ruleStoreKanban struct {
	server *Server
}

var ruleStoreCaller = kanbanapi.Caller{InternalService: true}

func (kanban ruleStoreKanban) api() (*kanbanapi.API, error) {
	handlers := kanban.server.kanbanStore.Load()
	if handlers == nil || handlers.api == nil {
		return nil, errKanbanStoreOff
	}
	return handlers.api, nil
}

// ruleStoreKanbanAnswer is a kanban-store refusal as rule-store reads it: a
// 404 wraps rulestore.ErrKanbanNotFound, and any other names its status.
func ruleStoreKanbanAnswer(what string, err error) error {
	var refusal *kanbanapi.Refusal
	if errors.As(err, &refusal) {
		if refusal.Status == 404 {
			return fmt.Errorf("%w: %s: %s", rulestore.ErrKanbanNotFound, what, refusal.Message)
		}
		return fmt.Errorf("kanban-store %s: %d %s", what, refusal.Status, refusal.Message)
	}
	return fmt.Errorf("kanban-store %s: %w", what, err)
}

func (kanban ruleStoreKanban) LinkedCardPlacements(_ context.Context, sessionID string) ([]rulestore.CardPlacement, error) {
	api, err := kanban.api()
	if err != nil {
		return nil, err
	}
	linked, err := api.EntityCards(ruleStoreCaller, "session", sessionID)
	if err != nil {
		return nil, ruleStoreKanbanAnswer("the cards of session "+sessionID, err)
	}
	placements := []rulestore.CardPlacement{}
	for _, card := range linked {
		cardPlacements, _, err := api.CardPlacementsAndLinks(ruleStoreCaller, card.CardID)
		// A card that is on no board — linked, but detached from every board
		// — sits in no column, so it has no placement, which is an answer,
		// not a failure.
		var refusal *kanbanapi.Refusal
		if errors.As(err, &refusal) && refusal.Status == 404 {
			continue
		}
		if err != nil {
			return nil, ruleStoreKanbanAnswer("card "+card.CardID, err)
		}
		for _, placement := range cardPlacements {
			placements = append(placements, rulestore.CardPlacement{CardID: card.CardID, BoardID: placement.BoardID, ColumnID: placement.ColumnID})
		}
	}
	return placements, nil
}

func (kanban ruleStoreKanban) SessionTags(_ context.Context, sessionID string) ([]string, error) {
	api, err := kanban.api()
	if err != nil {
		return nil, err
	}
	tags, err := api.EntityTags(ruleStoreCaller, "session", sessionID)
	if err != nil {
		return nil, ruleStoreKanbanAnswer("the tags of session "+sessionID, err)
	}
	names := make([]string, 0, len(tags))
	for _, tag := range tags {
		names = append(names, tag.Tag)
	}
	return names, nil
}

func (kanban ruleStoreKanban) Card(_ context.Context, cardID string) (rulestore.CardDetail, error) {
	api, err := kanban.api()
	if err != nil {
		return rulestore.CardDetail{}, err
	}
	placements, links, err := api.CardPlacementsAndLinks(ruleStoreCaller, cardID)
	if err != nil {
		return rulestore.CardDetail{}, ruleStoreKanbanAnswer("card "+cardID, err)
	}
	card := rulestore.CardDetail{Placements: []rulestore.CardPlacementDetail{}, Links: []rulestore.CardLinkRef{}}
	for _, placement := range placements {
		card.Placements = append(card.Placements, rulestore.CardPlacementDetail{BoardID: placement.BoardID, ColumnID: placement.ColumnID,
			UpdatedAt: placement.UpdatedAt.Format(time.RFC3339Nano)})
	}
	for _, link := range links {
		card.Links = append(card.Links, rulestore.CardLinkRef{EntityType: link.EntityType, EntityRef: link.EntityRef})
	}
	return card, nil
}

// ruleStoreColumnPageSize is how many cards one column read asks for, as
// rule-store paged it over HTTP.
const ruleStoreColumnPageSize = 500

func (kanban ruleStoreKanban) ColumnCards(_ context.Context, boardID, columnID string) ([]rulestore.ColumnArrival, error) {
	api, err := kanban.api()
	if err != nil {
		return nil, err
	}
	arrivals := []rulestore.ColumnArrival{}
	for offset := 0; ; offset += ruleStoreColumnPageSize {
		columnBoardID, total, cards, err := api.ColumnCardsAndLinks(ruleStoreCaller, columnID, ruleStoreColumnPageSize, offset)
		if err != nil {
			return nil, ruleStoreKanbanAnswer("column "+columnID, err)
		}
		if columnBoardID != boardID {
			return nil, fmt.Errorf("column %s is on board %s, not %s", columnID, columnBoardID, boardID)
		}
		for _, card := range cards {
			arrival := rulestore.ColumnArrival{CardID: card.Placement.CardID, ArrivedAt: card.Placement.UpdatedAt.Format(time.RFC3339Nano), SessionIDs: []string{}}
			for _, link := range card.Links {
				if link.EntityType == rulestore.SubjectTypeSession {
					arrival.SessionIDs = append(arrival.SessionIDs, link.EntityRef)
				}
			}
			arrivals = append(arrivals, arrival)
		}
		if len(cards) == 0 || offset+len(cards) >= total {
			return arrivals, nil
		}
	}
}
