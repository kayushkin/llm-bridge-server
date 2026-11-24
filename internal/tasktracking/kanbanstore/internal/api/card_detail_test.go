package api_test

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

func relationsOf(t *testing.T, access model.CallerAccess) []string {
	t.Helper()
	return access.Relations
}

// A client must be able to ask what it may do instead of learning each refusal
// by trying, and must never have to know that can_edit includes can_view.
func TestBoardAccessNamesEveryRelationThatHoldsForTheCaller(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)

	cases := []struct {
		who       map[string]string
		what      string
		relations []string
		unbounded bool
	}{
		{asPrincipal(alice), "an editor", []string{"can_view", "can_edit"}, false},
		{asPrincipal(carol), "a board administrator", []string{"can_view", "can_edit", "can_administer"}, false},
		{asPrincipal(deploymentAdministrator), "a deployment administrator with no grant", []string{"can_view", "can_edit", "can_administer"}, true},
		{asService, "the service token", []string{"can_view", "can_edit", "can_administer"}, true},
	}
	for _, c := range cases {
		w := requestAs(t, h, c.who, "GET", "/api/boards/"+f.supportBoardID+"/access", nil)
		mustStatus(t, w, 200, c.what+" reads its access to Support")
		var access model.CallerAccess
		decodeSuccessfulResponse(t, w, &access)
		if !reflect.DeepEqual(relationsOf(t, access), c.relations) {
			t.Errorf("%s: relations = %v, want %v", c.what, access.Relations, c.relations)
		}
		if access.Unrestricted != c.unbounded {
			t.Errorf("%s: unrestricted = %v, want %v", c.what, access.Unrestricted, c.unbounded)
		}
	}

	// bob holds nothing on Support: asking what he may do there must not
	// confirm the board exists.
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "GET", "/api/boards/"+f.supportBoardID+"/access", nil),
		404, "bob reads his access to a board he cannot see")
	mustStatus(t, requestAs(t, h, asService, "GET", "/api/boards/no-such-board/access", nil),
		404, "access to a board that does not exist")
}

// noteboard checks no caller, so this route is the only read of a card's body
// that does. It has to carry the body, and it has to refuse like any card read.
func TestOneCardIsReadThroughTheGate(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)

	w := requestAs(t, h, asPrincipal(alice), "GET", "/api/cards/"+f.supportCardID, nil)
	mustStatus(t, w, 200, "alice reads a card on her board")
	var detail model.CardDetail
	decodeSuccessfulResponse(t, w, &detail)
	item, isObject := detail.Item.(map[string]any)
	if !isObject || item["title"] != "printer on fire" {
		t.Fatalf("item = %#v, want the noteboard item titled %q", detail.Item, "printer on fire")
	}
	if len(detail.Placements) != 1 || detail.Placements[0].BoardID != f.supportBoardID {
		t.Errorf("placements = %#v, want the one on Support", detail.Placements)
	}
	if want := []string{"can_view", "can_edit"}; !reflect.DeepEqual(detail.Access.Relations, want) {
		t.Errorf("access on her own card = %v, want %v", detail.Access.Relations, want)
	}

	mustStatus(t, requestAs(t, h, asPrincipal(bob), "GET", "/api/cards/"+f.supportCardID, nil),
		404, "bob reads a card on a board he cannot see")
	mustStatus(t, requestAs(t, h, asService, "GET", "/api/cards/no-such-card", nil),
		404, "a card that is on no board")
}

// The shared card sits on Support and on Finance. alice edits Support and
// cannot see Finance, so she may not edit the card — and nothing she reads may
// tell her it is on Finance at all.
func TestACardOnAnUnseenBoardIsViewOnlyAndTheBoardIsNotNamed(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	mustStatus(t, requestAs(t, h, asService, "PUT", "/api/cards/"+f.sharedCardID+"/ticket",
		model.TicketWriteRequest{RequesterPrincipalID: requesterContact, Channel: "email"}), 200, "service files a ticket on the shared card")

	w := requestAs(t, h, asPrincipal(alice), "GET", "/api/cards/"+f.sharedCardID, nil)
	mustStatus(t, w, 200, "alice reads the shared card")
	var detail model.CardDetail
	decodeSuccessfulResponse(t, w, &detail)
	if want := []string{"can_view"}; !reflect.DeepEqual(detail.Access.Relations, want) {
		t.Errorf("access = %v, want %v: the store refuses her edit, so the read must not offer it", detail.Access.Relations, want)
	}
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "PATCH", "/api/cards/"+f.sharedCardID, map[string]any{"title": "x"}),
		403, "the edit the access answer said she may not make")
	for _, placement := range detail.Placements {
		if placement.BoardID == f.financeBoardID {
			t.Errorf("placements name Finance, which alice cannot view")
		}
	}
	if detail.Ticket == nil {
		t.Fatalf("the shared card is a ticket and the read carried none")
	}
	for _, state := range detail.Ticket.States {
		if state.BoardID == f.financeBoardID {
			t.Errorf("the ticket's states name Finance and its column %q, which alice cannot view", state.ColumnName)
		}
	}

	// The same through the ticket route, which named every board before.
	w = requestAs(t, h, asPrincipal(alice), "GET", "/api/cards/"+f.sharedCardID+"/ticket", nil)
	mustStatus(t, w, 200, "alice reads the shared ticket")
	var view model.TicketView
	decodeSuccessfulResponse(t, w, &view)
	if len(view.States) != 1 || view.States[0].BoardID != f.supportBoardID {
		t.Errorf("ticket states = %#v, want only the Support one", view.States)
	}

	// The service token sees both.
	w = requestAs(t, h, asService, "GET", "/api/cards/"+f.sharedCardID+"/ticket", nil)
	mustStatus(t, w, 200, "the service reads the shared ticket")
	decodeSuccessfulResponse(t, w, &view)
	if len(view.States) != 2 {
		t.Errorf("the service sees %d states, want both boards", len(view.States))
	}
}

// Two agents open one ticket. The first saves; the second's save was made
// against a version that is gone. noteboard owns the item and refuses; this
// store has to carry the question there and the answer back, unchanged.
func TestASaveMadeAgainstAnOlderCardIsRefusedWithTheCardAsItIsNow(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)

	w := requestAs(t, h, asPrincipal(alice), "GET", "/api/cards/"+f.supportCardID, nil)
	mustStatus(t, w, 200, "alice opens the card")
	var opened model.CardDetail
	decodeSuccessfulResponse(t, w, &opened)
	versionBothHold := `"` + opened.Item.(map[string]any)["updated_at"].(string) + `"`

	save := func(who, title, ifMatch string) *httptest.ResponseRecorder {
		headers := asPrincipal(who)
		if ifMatch != "" {
			headers["If-Match"] = ifMatch
		}
		return requestAs(t, h, headers, "PATCH", "/api/cards/"+f.supportCardID, map[string]any{"title": title})
	}
	mustStatus(t, save(carol, "carol saved first", versionBothHold), 200, "the first save")

	second := save(alice, "alice saved second", versionBothHold)
	mustStatus(t, second, 412, "the second save, made against the version carol replaced")
	var refusal struct {
		Error   string         `json:"error"`
		Current map[string]any `json:"current"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("412 body is not JSON: %s", second.Body.String())
	}
	if refusal.Current["title"] != "carol saved first" {
		t.Fatalf("412 body = %s, want noteboard's answer carrying the card as carol left it", second.Body.String())
	}

	// Retried against the version the refusal handed back, it is applied; and a
	// save that asks for no check is applied as it always was.
	retryVersion := `"` + refusal.Current["updated_at"].(string) + `"`
	mustStatus(t, save(alice, "alice, having seen carol's", retryVersion), 200, "the retry against the current version")
	mustStatus(t, save(alice, "no precondition", ""), 200, "a save with no If-Match")
}
