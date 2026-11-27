// fakeIssueStore stands in for kanban-store's Go surface (issueStore) in
// tests: each method answers from the function a test
// sets, or refuses 501 when none is set, and every call's caller is recorded.

package server

import (
	"net/http"
	"net/url"
	"sync"

	issuesapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/api"
	issuesmodel "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/model"
	"github.com/kayushkin/llm-bridge/msg"
)

// fakeIssueStoreCall is one call the fake was asked: the method, the caller and the record
// it was about (an entity "type/ref", a card id or a board id).
type fakeIssueStoreCall struct {
	Method string
	Caller issuesapi.Caller
	About  string
}

// fakeIssueStore answers each method with its function.
type fakeIssueStore struct {
	EntityCardsAnswer            func(entityType, entityRef string) ([]issuesmodel.EntityCardView, error)
	CardLinksAnswer              func(cardID string) ([]issuesmodel.CardLink, error)
	RecordCardEventAnswer        func(cardID string, request issuesmodel.CreateCardEventRequest) (*issuesmodel.CardEvent, error)
	CardTagsAnswer               func(cardID string) ([]string, error)
	BoardEffectiveDefaultsAnswer func(boardID string, tags []string) (*issuesmodel.EffectiveDefaults, error)
	CardEffectiveDefaultsAnswer  func(boardID, cardID string) (*issuesmodel.EffectiveDefaults, error)
	BoardClassificationAnswer    func(caller issuesapi.Caller, boardID string) (*msg.BoardClassification, error)
	BoardCardPageAnswer          func(caller issuesapi.Caller, boardID string, query url.Values) (*issuesmodel.BoardCardPage, error)
	PublishAnswer                func(publication msg.BoardClassificationDecisionPublication) (*issuesmodel.ClassificationDecision, bool, error)

	mutex sync.Mutex
	calls []fakeIssueStoreCall
}

// Calls is every call the fake was asked, in order.
func (store *fakeIssueStore) Calls() []fakeIssueStoreCall {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	return append([]fakeIssueStoreCall(nil), store.calls...)
}

func (store *fakeIssueStore) record(method string, caller issuesapi.Caller, about string) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.calls = append(store.calls, fakeIssueStoreCall{Method: method, Caller: caller, About: about})
}

var notAnswered = &issuesapi.Refusal{Status: http.StatusNotImplemented, Message: "the test set no answer for this call"}

func (store *fakeIssueStore) EntityCards(caller issuesapi.Caller, entityType, entityRef string) ([]issuesmodel.EntityCardView, error) {
	store.record("EntityCards", caller, entityType+"/"+entityRef)
	if store.EntityCardsAnswer == nil {
		return nil, notAnswered
	}
	return store.EntityCardsAnswer(entityType, entityRef)
}

func (store *fakeIssueStore) CardLinks(caller issuesapi.Caller, cardID string) ([]issuesmodel.CardLink, error) {
	store.record("CardLinks", caller, cardID)
	if store.CardLinksAnswer == nil {
		return nil, notAnswered
	}
	return store.CardLinksAnswer(cardID)
}

func (store *fakeIssueStore) RecordCardEvent(caller issuesapi.Caller, cardID string, request issuesmodel.CreateCardEventRequest) (*issuesmodel.CardEvent, error) {
	store.record("RecordCardEvent", caller, cardID)
	if store.RecordCardEventAnswer == nil {
		return nil, notAnswered
	}
	return store.RecordCardEventAnswer(cardID, request)
}

func (store *fakeIssueStore) CardTags(caller issuesapi.Caller, cardID string) ([]string, error) {
	store.record("CardTags", caller, cardID)
	if store.CardTagsAnswer == nil {
		return nil, notAnswered
	}
	return store.CardTagsAnswer(cardID)
}

func (store *fakeIssueStore) BoardEffectiveDefaults(caller issuesapi.Caller, boardID string, tags []string) (*issuesmodel.EffectiveDefaults, error) {
	store.record("BoardEffectiveDefaults", caller, boardID)
	if store.BoardEffectiveDefaultsAnswer == nil {
		return nil, notAnswered
	}
	return store.BoardEffectiveDefaultsAnswer(boardID, tags)
}

func (store *fakeIssueStore) CardEffectiveDefaults(caller issuesapi.Caller, boardID, cardID string) (*issuesmodel.EffectiveDefaults, error) {
	store.record("CardEffectiveDefaults", caller, boardID)
	if store.CardEffectiveDefaultsAnswer == nil {
		return nil, notAnswered
	}
	return store.CardEffectiveDefaultsAnswer(boardID, cardID)
}

func (store *fakeIssueStore) BoardClassification(caller issuesapi.Caller, boardID string) (*msg.BoardClassification, error) {
	store.record("BoardClassification", caller, boardID)
	if store.BoardClassificationAnswer == nil {
		return nil, notAnswered
	}
	return store.BoardClassificationAnswer(caller, boardID)
}

func (store *fakeIssueStore) BoardCardPage(caller issuesapi.Caller, boardID string, query url.Values) (*issuesmodel.BoardCardPage, error) {
	store.record("BoardCardPage", caller, boardID)
	if store.BoardCardPageAnswer == nil {
		return nil, notAnswered
	}
	return store.BoardCardPageAnswer(caller, boardID, query)
}

func (store *fakeIssueStore) PublishClassificationDecision(caller issuesapi.Caller, publication msg.BoardClassificationDecisionPublication) (*issuesmodel.ClassificationDecision, bool, error) {
	store.record("PublishClassificationDecision", caller, publication.BoardID)
	if store.PublishAnswer == nil {
		return nil, false, notAnswered
	}
	return store.PublishAnswer(publication)
}
