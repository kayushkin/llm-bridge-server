package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

// ============================ Stub llm-bridge-server ============================

// fakeLLMBridgeServer stands in for the two routes the board-settings write
// path reads: GET /agents (a list with integer ids and renameable slugs) and
// GET /instances/{id}. It answers a missing instance the way the real server
// does — 404 with "instance not found" — and nothing else.
type fakeLLMBridgeServer struct{}

const (
	knownAgentID         = "17"
	knownAgentSlug       = "agent-dashboard"
	unknownAgentID       = "9999"
	knownInstanceID      = "inst-test"
	otherKnownInstanceID = "inst-other"
	unknownInstanceID    = "inst-nope"
)

func newFakeLLMBridgeServer() *fakeLLMBridgeServer { return &fakeLLMBridgeServer{} }

func (f *fakeLLMBridgeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []map[string]any{
			{"id": 17, "slug": knownAgentSlug, "display_name": "Aengus"},
			{"id": 12, "slug": "argraphments", "display_name": "Ogham"},
		})
	})
	mux.HandleFunc("GET /instances/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id != knownInstanceID && id != otherKnownInstanceID {
			w.WriteHeader(404)
			w.Write([]byte("instance not found"))
			return
		}
		writeJSON(w, 200, map[string]any{"id": id, "harness_type": "claude_code"})
	})
	return mux
}

// fakeBundleStore stands in for bundle-store's GET /bundles/{id}: integer ids,
// 400 "invalid id" for anything else, 404 for an integer it does not have.
type fakeBundleStore struct{}

const (
	knownBundleID   = "6"
	knownBundleName = "docker"
	unknownBundleID = "9999"
)

func newFakeBundleStore() *fakeBundleStore { return &fakeBundleStore{} }

func (f *fakeBundleStore) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bundles/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := strconv.Atoi(id); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid id"})
			return
		}
		if id != knownBundleID {
			writeJSON(w, 404, map[string]string{"error": "bundle not found"})
			return
		}
		writeJSON(w, 200, map[string]any{"id": 6, "name": knownBundleName, "display_name": "Docker"})
	})
	return mux
}

func str(s string) *string { return &s }

func getBoard(t *testing.T, h http.Handler, id string) model.Board {
	t.Helper()
	w := do(t, h, "GET", "/api/boards/"+id, nil)
	if w.Code != 200 {
		t.Fatalf("get board: %d %s", w.Code, w.Body.String())
	}
	var b model.Board
	decodeSuccessfulResponse(t, w, &b)
	return b
}

func patchBoard(t *testing.T, h http.Handler, id string, req model.UpdateBoardRequest) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, "PATCH", "/api/boards/"+id, req)
}

// ============================ Settings round trip ============================

func TestBoardSettingsRoundTripAndClear(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Settings")

	w := patchBoard(t, h, boardID, model.UpdateBoardRequest{
		DefaultPrincipalID: str(activePrincipal),
		DefaultAgentID:     str(knownAgentID),
		DefaultInstanceID:  str(knownInstanceID),
		DefaultBundleID:    str(knownBundleID),
		Classifier:         &model.ClassifierConfig{Vocabulary: "work", MailAccountIDs: []string{"demo-work"}, HoldNewCards: true},
	})
	if w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	b := getBoard(t, h, boardID)
	if b.DefaultPrincipalID != activePrincipal || b.DefaultAgentID != knownAgentID || b.DefaultInstanceID != knownInstanceID || b.DefaultBundleID != knownBundleID {
		t.Fatalf("defaults did not round-trip: %+v", b)
	}
	if b.Classifier == nil || b.Classifier.Vocabulary != "work" || len(b.Classifier.MailAccountIDs) != 1 || b.Classifier.MailAccountIDs[0] != "demo-work" || !b.Classifier.HoldNewCards {
		t.Fatalf("classifier did not round-trip: %+v", b.Classifier)
	}

	// A PATCH that says nothing about the settings leaves them alone.
	if w := patchBoard(t, h, boardID, model.UpdateBoardRequest{Name: str("Renamed")}); w.Code != 200 {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	if b := getBoard(t, h, boardID); b.DefaultAgentID != knownAgentID || b.Classifier == nil {
		t.Fatalf("an unrelated PATCH wiped settings: %+v", b)
	}

	// An empty string clears an id; an empty object clears the classifier.
	w = patchBoard(t, h, boardID, model.UpdateBoardRequest{
		DefaultPrincipalID: str(""), DefaultAgentID: str(""), DefaultInstanceID: str(""), DefaultBundleID: str(""),
		Classifier: &model.ClassifierConfig{},
	})
	if w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	b = getBoard(t, h, boardID)
	if b.DefaultPrincipalID != "" || b.DefaultAgentID != "" || b.DefaultInstanceID != "" || b.DefaultBundleID != "" || b.Classifier != nil {
		t.Fatalf("clear did not clear: %+v", b)
	}
	// And a cleared setting is absent on the wire, not an empty string.
	raw := do(t, h, "GET", "/api/boards/"+boardID, nil).Body.String()
	for _, key := range []string{"default_principal_id", "default_agent_id", "default_instance_id", "default_bundle_id", "classifier"} {
		if strings.Contains(raw, key) {
			t.Fatalf("cleared %s still on the wire: %s", key, raw)
		}
	}
}

func TestBoardSettingsListedWithTheBoard(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Listed")
	patchBoard(t, h, boardID, model.UpdateBoardRequest{DefaultInstanceID: str(knownInstanceID)})
	w := do(t, h, "GET", "/api/boards", nil)
	var boards []model.Board
	decodeSuccessfulResponse(t, w, &boards)
	if len(boards) != 1 || boards[0].DefaultInstanceID != knownInstanceID {
		t.Fatalf("list does not carry settings: %+v", boards)
	}
}

// ============================ Owner checks ============================

func TestBoardSettingsRefuseAnUnknownOrDisabledPrincipal(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Owners")
	for _, principal := range []string{unknownPrincipal, disabledPrincipal, "not-an-id"} {
		w := patchBoard(t, h, boardID, model.UpdateBoardRequest{DefaultPrincipalID: str(principal)})
		if w.Code != 400 {
			t.Fatalf("principal %q: want 400, got %d %s", principal, w.Code, w.Body.String())
		}
	}
	if b := getBoard(t, h, boardID); b.DefaultPrincipalID != "" {
		t.Fatalf("a refused principal was written: %+v", b)
	}
}

func TestBoardSettingsRefuseAnUnknownAgentOrASlug(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Owners")
	for _, agent := range []string{unknownAgentID, knownAgentSlug} {
		w := patchBoard(t, h, boardID, model.UpdateBoardRequest{DefaultAgentID: str(agent)})
		if w.Code != 400 {
			t.Fatalf("agent %q: want 400, got %d %s", agent, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "numeric id") {
			t.Fatalf("the refusal should tell the caller to send the numeric id: %s", w.Body.String())
		}
	}
	w := patchBoard(t, h, boardID, model.UpdateBoardRequest{DefaultInstanceID: str(unknownInstanceID)})
	if w.Code != 400 {
		t.Fatalf("instance: want 400, got %d %s", w.Code, w.Body.String())
	}
	for _, bundle := range []string{unknownBundleID, knownBundleName} {
		w := patchBoard(t, h, boardID, model.UpdateBoardRequest{DefaultBundleID: str(bundle)})
		if w.Code != 400 {
			t.Fatalf("bundle %q: want 400, got %d %s", bundle, w.Code, w.Body.String())
		}
	}
	if b := getBoard(t, h, boardID); b.DefaultAgentID != "" || b.DefaultInstanceID != "" || b.DefaultBundleID != "" {
		t.Fatalf("a refused id was written: %+v", b)
	}
}

func TestBoardSettingsWithAnOwnerDownAre502AndWriteNothing(t *testing.T) {
	principals := httptest.NewServer(newFakePrincipalStore().handler())
	defer principals.Close()
	closedPort := httptest.NewServer(http.NotFoundHandler())
	closedPort.Close()

	h, _, _, cleanup := setupWithOwners(t, principals.URL, closedPort.URL, closedPort.URL)
	defer cleanup()
	boardID := mkBoard(t, h, "Down")
	for _, req := range []model.UpdateBoardRequest{
		{DefaultAgentID: str(knownAgentID)},
		{DefaultInstanceID: str(knownInstanceID)},
		{DefaultBundleID: str(knownBundleID)},
	} {
		w := patchBoard(t, h, boardID, req)
		if w.Code != 502 {
			t.Fatalf("owner down: want 502, got %d %s", w.Code, w.Body.String())
		}
	}
	// The principal owner is up, so a principal alone still writes.
	if w := patchBoard(t, h, boardID, model.UpdateBoardRequest{DefaultPrincipalID: str(activePrincipal)}); w.Code != 200 {
		t.Fatalf("principal with bridge down: %d %s", w.Code, w.Body.String())
	}
	b := getBoard(t, h, boardID)
	if b.DefaultAgentID != "" || b.DefaultInstanceID != "" || b.DefaultPrincipalID != activePrincipal {
		t.Fatalf("unexpected state after owner-down PATCHes: %+v", b)
	}
}

func TestBoardSettingsRefuseAHalfClassifier(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Half")
	for _, cc := range []model.ClassifierConfig{
		{Vocabulary: "work"},
		{MailAccountIDs: []string{"demo-work"}},
		{HoldNewCards: true},
	} {
		w := patchBoard(t, h, boardID, model.UpdateBoardRequest{Classifier: &cc})
		if w.Code != 400 {
			t.Fatalf("classifier %+v: want 400, got %d %s", cc, w.Code, w.Body.String())
		}
	}
}

// ============================ Default assignee ============================

func TestACardCreatedOnTheBoardTakesItsDefaultAssignee(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Defaults")
	columnID := mkColumn(t, h, boardID, "Todo", "")
	patchBoard(t, h, boardID, model.UpdateBoardRequest{DefaultPrincipalID: str(activePrincipal)})

	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards?actor=email-classifier", model.CreateCardRequest{Title: "new mail", ColumnID: columnID})
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)
	if len(cv.Assignments) != 1 || cv.Assignments[0].PrincipalID != activePrincipal {
		t.Fatalf("created card should carry the default assignee: %+v", cv.Assignments)
	}
	if cv.Assignments[0].AssignedBy != "email-classifier" {
		t.Fatalf("assigned_by should be the creating actor, got %q", cv.Assignments[0].AssignedBy)
	}
	events := eventsOfKind(t, h, cv.Placement.CardID, model.EventAssigned)
	if len(events) != 1 {
		t.Fatalf("want one assigned event, got %d", len(events))
	}
	var detail map[string]any
	json.Unmarshal(events[0].Detail, &detail)
	if detail["source"] != "board_default" {
		t.Fatalf("the event should say the assignment came from the board default: %s", string(events[0].Detail))
	}
}

func TestAttachingAnAlreadyAssignedCardKeepsItsAssignee(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	// Card born on a board with no default, assigned by hand to principal 1.
	sourceBoard := mkBoard(t, h, "Source")
	sourceColumn := mkColumn(t, h, sourceBoard, "Todo", "")
	w := do(t, h, "POST", "/api/boards/"+sourceBoard+"/cards", model.CreateCardRequest{Title: "owned", ColumnID: sourceColumn})
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)
	cardID := cv.Placement.CardID
	if len(cv.Assignments) != 0 {
		t.Fatalf("a board with no default should create unassigned cards: %+v", cv.Assignments)
	}
	if w := do(t, h, "PUT", "/api/cards/"+cardID+"/assignments/"+activePrincipal, nil); w.Code != 201 {
		t.Fatalf("assign: %d %s", w.Code, w.Body.String())
	}

	// A second board whose default is a different principal.
	targetBoard := mkBoard(t, h, "Target")
	targetColumn := mkColumn(t, h, targetBoard, "Inbox", "")
	// principal_000003 is unknown to the fake store, so use the one it knows
	// and make the point with the count instead: attaching must not add a row.
	patchBoard(t, h, targetBoard, model.UpdateBoardRequest{DefaultPrincipalID: str(activePrincipal)})
	if w := do(t, h, "PUT", "/api/boards/"+targetBoard+"/cards/"+cardID, model.AttachCardRequest{ColumnID: targetColumn}); w.Code != 201 {
		t.Fatalf("attach: %d %s", w.Code, w.Body.String())
	}
	w = do(t, h, "GET", "/api/cards/"+cardID+"/assignments", nil)
	var assignments []model.CardAssignment
	decodeSuccessfulResponse(t, w, &assignments)
	if len(assignments) != 1 {
		t.Fatalf("attach should not add an assignee to an assigned card: %+v", assignments)
	}
	if got := eventsOfKind(t, h, cardID, model.EventAssigned); len(got) != 1 {
		t.Fatalf("want the one hand-made assigned event, got %d", len(got))
	}
}

func TestAttachingAnUnassignedCardTakesTheBoardDefault(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	sourceBoard := mkBoard(t, h, "Source")
	sourceColumn := mkColumn(t, h, sourceBoard, "Todo", "")
	w := do(t, h, "POST", "/api/boards/"+sourceBoard+"/cards", model.CreateCardRequest{Title: "loose", ColumnID: sourceColumn})
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)
	targetBoard := mkBoard(t, h, "Target")
	targetColumn := mkColumn(t, h, targetBoard, "Inbox", "")
	patchBoard(t, h, targetBoard, model.UpdateBoardRequest{DefaultPrincipalID: str(activePrincipal)})
	if w := do(t, h, "PUT", "/api/boards/"+targetBoard+"/cards/"+cv.Placement.CardID, model.AttachCardRequest{ColumnID: targetColumn}); w.Code != 201 {
		t.Fatalf("attach: %d %s", w.Code, w.Body.String())
	}
	w = do(t, h, "GET", "/api/cards/"+cv.Placement.CardID+"/assignments", nil)
	var assignments []model.CardAssignment
	decodeSuccessfulResponse(t, w, &assignments)
	if len(assignments) != 1 || assignments[0].PrincipalID != activePrincipal {
		t.Fatalf("attach should hand an unassigned card to the board default: %+v", assignments)
	}
}

func TestADefaultAssigneeDisabledSinceRefusesTheCardBeforeNoteboardIsTouched(t *testing.T) {
	principals := newFakePrincipalStore()
	principalServer := httptest.NewServer(principals.handler())
	defer principalServer.Close()
	bridge := httptest.NewServer(newFakeLLMBridgeServer().handler())
	defer bridge.Close()
	bundles := httptest.NewServer(newFakeBundleStore().handler())
	defer bundles.Close()
	h, nb, _, cleanup := setupWithOwners(t, principalServer.URL, bridge.URL, bundles.URL)
	defer cleanup()

	boardID := mkBoard(t, h, "Stale")
	columnID := mkColumn(t, h, boardID, "Todo", "")
	if w := patchBoard(t, h, boardID, model.UpdateBoardRequest{DefaultPrincipalID: str(activePrincipal)}); w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	// The person leaves. principal-store never deletes, it disables.
	principals.disabledAtByID[activePrincipal] = 1_757_000_000

	itemsBefore := len(nb.items)
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "orphan?", ColumnID: columnID})
	if w.Code != 400 {
		t.Fatalf("want 400 naming the board's stale default, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "default_principal_id") {
		t.Fatalf("the refusal should point at the setting to fix: %s", w.Body.String())
	}
	if len(nb.items) != itemsBefore {
		t.Fatalf("a refused card must not leave a noteboard item behind: %d → %d", itemsBefore, len(nb.items))
	}
}

func TestTheClassifiersOrganizationMustBeAnActiveGroup(t *testing.T) {
	principals := newFakePrincipalStore()
	principalServer := httptest.NewServer(principals.handler())
	defer principalServer.Close()
	bridge := httptest.NewServer(newFakeLLMBridgeServer().handler())
	defer bridge.Close()
	bundles := httptest.NewServer(newFakeBundleStore().handler())
	defer bundles.Close()
	h, _, _, cleanup := setupWithOwners(t, principalServer.URL, bridge.URL, bundles.URL)
	defer cleanup()
	boardID := mkBoard(t, h, "Mail")
	classifier := func(organizationID string) model.UpdateBoardRequest {
		return model.UpdateBoardRequest{Classifier: &model.ClassifierConfig{Vocabulary: "personal", MailAccountIDs: []string{"gmail-personal"}, OrganizationID: organizationID}}
	}
	for _, refused := range []struct{ id, says string }{
		{activePrincipal, "is a human"},
		{requesterContact, "is a contact"},
		{unknownPrincipal, "does not exist"},
		{"org-7", "must match"},
	} {
		w := patchBoard(t, h, boardID, classifier(refused.id))
		if w.Code != 400 || !strings.Contains(w.Body.String(), refused.says) || !strings.Contains(w.Body.String(), "classifier.organization_id") {
			t.Errorf("%s: %d %s", refused.id, w.Code, w.Body.String())
		}
	}
	if w := patchBoard(t, h, boardID, classifier(organizationGroup)); w.Code != 200 {
		t.Fatalf("a group: %d %s", w.Code, w.Body.String())
	}
	w := do(t, h, "GET", "/api/boards/"+boardID, nil)
	var board model.Board
	decodeSuccessfulResponse(t, w, &board)
	if board.Classifier == nil || board.Classifier.OrganizationID != organizationGroup {
		t.Fatalf("read back %+v", board.Classifier)
	}
	principals.disabledAtByID[organizationGroup] = 1_757_000_000
	if w := patchBoard(t, h, boardID, classifier(organizationGroup)); w.Code != 400 || !strings.Contains(w.Body.String(), "disabled") {
		t.Fatalf("a disabled group: %d %s", w.Code, w.Body.String())
	}
}
