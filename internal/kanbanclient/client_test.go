package kanbanclient

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/kanbanclient/kanbanclienttest"
	kanbanapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/api"
	kanbanmodel "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/model"
)

// item is a noteboard item as kanban-store passes it through.
func item(id string) map[string]any { return map[string]any{"id": id} }

// sessionCards is kanban-store answering the session-cards lookup for one
// session with cards, and with no cards (not a refusal) for any other: an
// unknown session ref is 200 [] on the route, so a lookup aimed at the wrong
// id would read as "no todo" without this distinction in a test.
func sessionCards(sessionID string, cards []kanbanmodel.EntityCardView) (*Client, *kanbanclienttest.Store) {
	store := &kanbanclienttest.Store{EntityCardsAnswer: func(entityType, entityRef string) ([]kanbanmodel.EntityCardView, error) {
		if entityType != "session" || entityRef != sessionID {
			return []kanbanmodel.EntityCardView{}, nil
		}
		return cards, nil
	}}
	return New(store), store
}

func TestLinkedTodoForSessionTakesTheFirstCard(t *testing.T) {
	// kanban-store returns links oldest-first (ListCardsByEntity orders by
	// MIN(created_at), card_id, pinned by TestListCardsByEntityIsOldestLinkFirst),
	// and the first is the todo the session was created for. Re-sorting here
	// would second-guess the store that owns the ordering.
	client, store := sessionCards("br_1", []kanbanmodel.EntityCardView{
		{CardID: "todo-oldest", Item: map[string]any{"id": "todo-oldest", "title": "dispatch"}},
		{CardID: "todo-newer", Item: map[string]any{"id": "todo-newer", "title": "classified work"}},
	})
	got, err := client.LinkedTodoForSession(context.Background(), "br_1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "todo-oldest" {
		t.Errorf("todo = %q, want todo-oldest", got)
	}
	if calls := store.Calls(); len(calls) != 1 || calls[0].About != "session/br_1" {
		t.Errorf("calls = %+v, want one lookup of session br_1", calls)
	}
}

func TestLinkedCardForSessionReadsTheTitleAndBody(t *testing.T) {
	client, _ := sessionCards("br_1", []kanbanmodel.EntityCardView{
		{CardID: "todo-1", Item: map[string]any{"id": "todo-1", "title": "printer jammed", "body": "third floor"}},
	})
	card, err := client.LinkedCardForSession(context.Background(), "br_1")
	if err != nil || card == nil || card.Title != "printer jammed" || card.Body != "third floor" {
		t.Fatalf("card = %+v, %v", card, err)
	}
	client, _ = sessionCards("br_1", []kanbanmodel.EntityCardView{{CardID: "todo-1", Item: map[string]any{"id": "todo-1", "title": 7}}})
	if _, err := client.LinkedCardForSession(context.Background(), "br_1"); err == nil {
		t.Fatal("a title that is not text read as a card")
	}
}

func TestLinkedTodoForSessionSkipsOrphanedLinks(t *testing.T) {
	// A null item is a link that outlived its noteboard item: kanban-store
	// keeps the slot index-aligned and leaves the item nil. Returning its card
	// id would point the signal at a todo no view can render.
	var gone map[string]any
	client, _ := sessionCards("br_1", []kanbanmodel.EntityCardView{
		{CardID: "todo-deleted", Item: gone},
		{CardID: "todo-alive", Item: item("todo-alive")},
	})
	got, err := client.LinkedTodoForSession(context.Background(), "br_1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "todo-alive" {
		t.Errorf("todo = %q, want todo-alive", got)
	}
}

// A row whose card_id is blank is skipped, and the next usable card is
// returned. kanban-store cannot emit this; the good row after the blank one is
// what makes the test discriminating.
func TestLinkedTodoForSessionSkipsRowsWithNoCardID(t *testing.T) {
	client, _ := sessionCards("br_1", []kanbanmodel.EntityCardView{
		{CardID: "", Item: item("blank-card-id")},
		{CardID: "todo-alive", Item: item("todo-alive")},
	})
	got, err := client.LinkedTodoForSession(context.Background(), "br_1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "todo-alive" {
		t.Errorf("todo = %q, want todo-alive", got)
	}
}

func TestLinkedTodoForSessionWithNoLinks(t *testing.T) {
	var gone map[string]any
	for name, cards := range map[string][]kanbanmodel.EntityCardView{
		"empty list":   {},
		"nil list":     nil,
		"all orphaned": {{CardID: "gone", Item: gone}},
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := sessionCards("br_1", cards)
			got, err := client.LinkedTodoForSession(context.Background(), "br_1")
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if got != "" {
				t.Errorf("todo = %q, want empty", got)
			}
		})
	}
}

// A store that refuses must produce an error naming the status, never a
// confident empty string: the caller treats "" as "this session has no todo".
func TestLinkedTodoForSessionErrors(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway} {
		client := New(&kanbanclienttest.Store{EntityCardsAnswer: func(string, string) ([]kanbanmodel.EntityCardView, error) {
			return nil, &kanbanapi.Refusal{Status: status, Message: "noteboard: connection refused"}
		}})
		_, err := client.LinkedTodoForSession(context.Background(), "br_1")
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(status)) {
			t.Errorf("a %d refusal: %v, want an error naming the status", status, err)
		}
	}
	client, _ := sessionCards("br_1", nil)
	if _, err := client.LinkedTodoForSession(context.Background(), ""); err == nil {
		t.Fatal("expected an error for an empty session id")
	}
}

// An id carrying slashes is one entity ref: it is asked for whole, and it can
// never reach another session's cards.
func TestLinkedTodoForSessionAsksForTheWholeIDAndNoOther(t *testing.T) {
	client, store := sessionCards("br_2", []kanbanmodel.EntityCardView{{CardID: "br_2-todo", Item: item("br_2-todo")}})
	got, err := client.LinkedTodoForSession(context.Background(), "br_1/../br_2")
	if err != nil || got != "" {
		t.Fatalf("todo = %q, %v; want none — never another session's card", got, err)
	}
	if calls := store.Calls(); len(calls) != 1 || calls[0].About != "session/br_1/../br_2" {
		t.Errorf("calls = %+v", calls)
	}
}

// Every read this server makes for itself is made as the internal service,
// past every board check. Made as nobody, kanban-store refuses 401, and every
// signal would be minted with no linked todo — a quiet loss, not a loud one.
func TestLookupsAreMadeAsTheInternalService(t *testing.T) {
	client, store := sessionCards("br_1", nil)
	if _, err := client.LinkedTodoForSession(context.Background(), "br_1"); err != nil {
		t.Fatal(err)
	}
	if got := store.Calls()[0].Caller; !got.InternalService || got.PrincipalID != "" {
		t.Fatalf("caller = %+v, want the internal service", got)
	}
}

func TestCardTagsAndItsNotFound(t *testing.T) {
	client := New(&kanbanclienttest.Store{CardTagsAnswer: func(cardID string) ([]string, error) {
		if cardID == "card-1" {
			return []string{"billing"}, nil
		}
		return nil, &kanbanapi.Refusal{Status: http.StatusNotFound, Message: "not found"}
	}})
	if tags, err := client.CardTags(context.Background(), "card-1"); err != nil || len(tags) != 1 || tags[0] != "billing" {
		t.Fatalf("tags = %v, %v", tags, err)
	}
	if _, err := client.CardTags(context.Background(), "card-gone"); !strings.Contains(err.Error(), "404") || !isNotFound(err) {
		t.Fatalf("a card kanban-store does not have: %v, want ErrNotFound", err)
	}
}
