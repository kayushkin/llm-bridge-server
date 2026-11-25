// Package kanbanclienttest is a stand-in for kanban-store's Go surface
// (kanbanclient.Store) in tests: each method answers from the function a test
// sets, or refuses 501 when none is set, and every call's caller is recorded.
package kanbanclienttest

import (
	"net/http"
	"net/url"
	"sync"

	kanbanapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/api"
	kanbanmodel "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/model"
	"github.com/kayushkin/llm-bridge/msg"
)

// Call is one call the fake was asked: the method, the caller and the record
// it was about (an entity "type/ref", a card id or a board id).
type Call struct {
	Method string
	Caller kanbanapi.Caller
	About  string
}

// Store answers each method with its function.
type Store struct {
	EntityCardsAnswer            func(entityType, entityRef string) ([]kanbanmodel.EntityCardView, error)
	CardLinksAnswer              func(cardID string) ([]kanbanmodel.CardLink, error)
	RecordCardEventAnswer        func(cardID string, request kanbanmodel.CreateCardEventRequest) (*kanbanmodel.CardEvent, error)
	CardTagsAnswer               func(cardID string) ([]string, error)
	BoardEffectiveDefaultsAnswer func(boardID string, tags []string) (*kanbanmodel.EffectiveDefaults, error)
	CardEffectiveDefaultsAnswer  func(boardID, cardID string) (*kanbanmodel.EffectiveDefaults, error)
	BoardClassificationAnswer    func(caller kanbanapi.Caller, boardID string) (*msg.BoardClassification, error)
	BoardCardPageAnswer          func(caller kanbanapi.Caller, boardID string, query url.Values) (*kanbanmodel.BoardCardPage, error)
	PublishAnswer                func(publication msg.BoardClassificationDecisionPublication) (*kanbanmodel.ClassificationDecision, bool, error)

	mutex sync.Mutex
	calls []Call
}

// Calls is every call the fake was asked, in order.
func (store *Store) Calls() []Call {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	return append([]Call(nil), store.calls...)
}

func (store *Store) record(method string, caller kanbanapi.Caller, about string) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.calls = append(store.calls, Call{Method: method, Caller: caller, About: about})
}

var notAnswered = &kanbanapi.Refusal{Status: http.StatusNotImplemented, Message: "the test set no answer for this call"}

func (store *Store) EntityCards(caller kanbanapi.Caller, entityType, entityRef string) ([]kanbanmodel.EntityCardView, error) {
	store.record("EntityCards", caller, entityType+"/"+entityRef)
	if store.EntityCardsAnswer == nil {
		return nil, notAnswered
	}
	return store.EntityCardsAnswer(entityType, entityRef)
}

func (store *Store) CardLinks(caller kanbanapi.Caller, cardID string) ([]kanbanmodel.CardLink, error) {
	store.record("CardLinks", caller, cardID)
	if store.CardLinksAnswer == nil {
		return nil, notAnswered
	}
	return store.CardLinksAnswer(cardID)
}

func (store *Store) RecordCardEvent(caller kanbanapi.Caller, cardID string, request kanbanmodel.CreateCardEventRequest) (*kanbanmodel.CardEvent, error) {
	store.record("RecordCardEvent", caller, cardID)
	if store.RecordCardEventAnswer == nil {
		return nil, notAnswered
	}
	return store.RecordCardEventAnswer(cardID, request)
}

func (store *Store) CardTags(caller kanbanapi.Caller, cardID string) ([]string, error) {
	store.record("CardTags", caller, cardID)
	if store.CardTagsAnswer == nil {
		return nil, notAnswered
	}
	return store.CardTagsAnswer(cardID)
}

func (store *Store) BoardEffectiveDefaults(caller kanbanapi.Caller, boardID string, tags []string) (*kanbanmodel.EffectiveDefaults, error) {
	store.record("BoardEffectiveDefaults", caller, boardID)
	if store.BoardEffectiveDefaultsAnswer == nil {
		return nil, notAnswered
	}
	return store.BoardEffectiveDefaultsAnswer(boardID, tags)
}

func (store *Store) CardEffectiveDefaults(caller kanbanapi.Caller, boardID, cardID string) (*kanbanmodel.EffectiveDefaults, error) {
	store.record("CardEffectiveDefaults", caller, boardID)
	if store.CardEffectiveDefaultsAnswer == nil {
		return nil, notAnswered
	}
	return store.CardEffectiveDefaultsAnswer(boardID, cardID)
}

func (store *Store) BoardClassification(caller kanbanapi.Caller, boardID string) (*msg.BoardClassification, error) {
	store.record("BoardClassification", caller, boardID)
	if store.BoardClassificationAnswer == nil {
		return nil, notAnswered
	}
	return store.BoardClassificationAnswer(caller, boardID)
}

func (store *Store) BoardCardPage(caller kanbanapi.Caller, boardID string, query url.Values) (*kanbanmodel.BoardCardPage, error) {
	store.record("BoardCardPage", caller, boardID)
	if store.BoardCardPageAnswer == nil {
		return nil, notAnswered
	}
	return store.BoardCardPageAnswer(caller, boardID, query)
}

func (store *Store) PublishClassificationDecision(caller kanbanapi.Caller, publication msg.BoardClassificationDecisionPublication) (*kanbanmodel.ClassificationDecision, bool, error) {
	store.record("PublishClassificationDecision", caller, publication.BoardID)
	if store.PublishAnswer == nil {
		return nil, false, notAnswered
	}
	return store.PublishAnswer(publication)
}
