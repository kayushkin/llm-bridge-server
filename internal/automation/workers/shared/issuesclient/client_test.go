package issuesclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newBoundClient returns a Client already bound to a board and one column, so a
// test can call CreateCard without going through EnsureBoard.
func newBoundClient(baseURL string) *Client {
	c := New(baseURL)
	c.BoardID = "board-1"
	c.ColumnsByName = map[string]string{"In Progress": "col-1"}
	return c
}

// linkRecorder is a kanban-store stand-in that creates cards and accepts or
// refuses each link by entity_type, so a test can fail exactly one link.
type linkRecorder struct {
	newCardID string
	refuse    map[string]bool // entity_type -> refuse it
	attached  []string        // entity_types that were actually accepted
	cardPOSTs int
}

func (lr *linkRecorder) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/boards/board-1/cards", func(w http.ResponseWriter, r *http.Request) {
		lr.cardPOSTs++
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"item": map[string]any{"id": lr.newCardID}})
	})
	mux.HandleFunc("/cards/"+lr.newCardID+"/links", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			EntityType string `json:"entity_type"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if lr.refuse[body.EntityType] {
			http.Error(w, "no such entity type", http.StatusBadRequest)
			return
		}
		lr.attached = append(lr.attached, body.EntityType)
		w.WriteHeader(http.StatusCreated)
	})
	return mux
}

// TestALinkFailureStillReportsTheCardThatExists is the regression for card
// a68ad12c: the session link is the FIRST of the two link specs, so a failure
// there leaves a card on the board with zero links. The caller must be able to
// learn that card's id from the error, because nothing else will ever hold it.
func TestALinkFailureStillReportsTheCardThatExists(t *testing.T) {
	lr := &linkRecorder{newCardID: "card-abc", refuse: map[string]bool{"session": true}}
	srv := httptest.NewServer(lr.handler())
	defer srv.Close()

	id, err := newBoundClient(srv.URL).CreateCard(CreateCardArgs{
		ColumnName: "In Progress", Title: "t",
		SessionID: "autoworker-anthropic-1", Provider: "anthropic",
	})
	if err == nil {
		t.Fatal("a refused link must be an error")
	}
	stranded, ok := StrandedCardID(err)
	if !ok {
		t.Fatalf("StrandedCardID must recognize a half-made card; got err %T: %v", err, err)
	}
	if stranded != "card-abc" {
		t.Errorf("stranded card id = %q, want %q", stranded, "card-abc")
	}
	if id != "card-abc" {
		t.Errorf("CreateCard id = %q, want %q — the id must survive the error too", id, "card-abc")
	}
	// The value is only useful if it names the card. A message that omits the id
	// is the exact failure this card was filed for.
	if !strings.Contains(err.Error(), "card-abc") {
		t.Errorf("error text must name the card, got %q", err.Error())
	}
}

// TestTheFailedLinkIsNamedAndLaterLinksAreNotAttempted pins which link failed,
// so a reader can tell "no session link" (unclosable) from "no provider link"
// (cosmetic). Links attach in order and the first failure stops the loop.
func TestTheFailedLinkIsNamedAndLaterLinksAreNotAttempted(t *testing.T) {
	lr := &linkRecorder{newCardID: "card-xyz", refuse: map[string]bool{"autoworker_provider": true}}
	srv := httptest.NewServer(lr.handler())
	defer srv.Close()

	_, err := newBoundClient(srv.URL).CreateCard(CreateCardArgs{
		ColumnName: "In Progress", Title: "t",
		SessionID: "sess-1", Provider: "anthropic",
	})
	var partial *CardCreatedWithoutAllLinksError
	if !errors.As(err, &partial) {
		t.Fatalf("want *CardCreatedWithoutAllLinksError, got %T: %v", err, err)
	}
	if partial.EntityType != "autoworker_provider" || partial.EntityRef != "anthropic" {
		t.Errorf("failed link = %s/%s, want autoworker_provider/anthropic",
			partial.EntityType, partial.EntityRef)
	}
	// The session link came first and must have gone on, so this card IS
	// resolvable to a session even though CreateCard returned an error.
	if len(lr.attached) != 1 || lr.attached[0] != "session" {
		t.Errorf("attached links = %v, want exactly [session]", lr.attached)
	}
}

// TestACardThatWasNeverCreatedStrandsNothing is the negative half. Without it,
// StrandedCardID could return true for everything and both tests above would
// still pass.
func TestACardThatWasNeverCreatedStrandsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "board is gone", http.StatusInternalServerError)
	}))
	defer srv.Close()

	id, err := newBoundClient(srv.URL).CreateCard(CreateCardArgs{
		ColumnName: "In Progress", Title: "t", SessionID: "sess-1", Provider: "anthropic",
	})
	if err == nil {
		t.Fatal("a 500 on card creation must be an error")
	}
	if stranded, ok := StrandedCardID(err); ok {
		t.Errorf("nothing was created, so nothing is stranded; got %q", stranded)
	}
	if id != "" {
		t.Errorf("id = %q, want empty when no card was created", id)
	}
}

// TestBothLinksAttachedIsNotAnError is the other negative control: the happy
// path must stay quiet, or a caller learns to ignore the new error.
func TestBothLinksAttachedIsNotAnError(t *testing.T) {
	lr := &linkRecorder{newCardID: "card-ok", refuse: map[string]bool{}}
	srv := httptest.NewServer(lr.handler())
	defer srv.Close()

	id, err := newBoundClient(srv.URL).CreateCard(CreateCardArgs{
		ColumnName: "In Progress", Title: "t", SessionID: "sess-1", Provider: "anthropic",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "card-ok" {
		t.Errorf("id = %q, want card-ok", id)
	}
	if len(lr.attached) != 2 {
		t.Errorf("attached = %v, want both links", lr.attached)
	}
}

// TestAnEmptyEntityRefIsSkippedRatherThanFailed pins that a caller passing no
// session id (kanban-classifier does, and attaches its own links afterwards)
// does not get a stranded-card error for a card that is fine.
func TestAnEmptyEntityRefIsSkippedRatherThanFailed(t *testing.T) {
	lr := &linkRecorder{newCardID: "card-nolinks", refuse: map[string]bool{}}
	srv := httptest.NewServer(lr.handler())
	defer srv.Close()

	if _, err := newBoundClient(srv.URL).CreateCard(CreateCardArgs{
		ColumnName: "In Progress", Title: "t", SessionID: "", Provider: "",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lr.attached) != 0 {
		t.Errorf("attached = %v, want none attempted", lr.attached)
	}
}

// TestUnwrapReachesTheTransportError keeps errors.Is usable through the new
// type, so a caller can still test for the underlying cause.
func TestUnwrapReachesTheTransportError(t *testing.T) {
	sentinel := errors.New("connection refused")
	e := &CardCreatedWithoutAllLinksError{CardID: "c", EntityType: "session", EntityRef: "s", Cause: sentinel}
	if !errors.Is(e, sentinel) {
		t.Error("Unwrap must expose the cause")
	}
}
