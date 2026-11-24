package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/kayushkin/kanban-store/internal/boundedtext"
	"github.com/kayushkin/kanban-store/internal/config"
	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/principalstore"
)

// ============================ Card assignments ============================
//
//	/api/cards/:cardID/assignments                — GET
//	/api/cards/:cardID/assignments/:principalID   — PUT (idempotent) | DELETE
//	/api/assignments?principal_id=                — GET (reverse lookup)
//
// An assignment is its own fact about a card — who is on it — and not a card
// link: a link's label is a display name and its uniqueness is per entity ref,
// neither of which is what "assigned" means. Principals are owned by
// principal-store, and this is the one write path in kanban-store that checks
// an id against the service that owns it before storing it; the client's
// package comment says why.

// principalIDShape anchors config.PrincipalIDPattern — the registry's own
// declaration of what a principal id looks like — for the write-path check.
var principalIDShape = regexp.MustCompile(`^(?:` + config.PrincipalIDPattern + `)$`)

func (a *API) cardAssignments(w http.ResponseWriter, r *http.Request, cardID string) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	assignments, err := a.store.ListCardAssignments(cardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	w.Header().Set("ETag", db.AssignmentVersion(assignments))
	writeJSON(w, 200, assignments)
}

func (a *API) cardAssignmentByPrincipal(w http.ResponseWriter, r *http.Request, cardID, principalID string) {
	switch r.Method {
	case http.MethodPut:
		a.assignPrincipal(w, r, cardID, principalID)
	case http.MethodDelete:
		a.unassignPrincipal(w, r, cardID, principalID)
	default:
		writeError(w, 405, "method not allowed")
	}
}

// assignPrincipal is idempotent: 201 with the row when this call created the
// assignment, 200 with the row when it already existed. Only the 201 path is
// an action on the card, so only it lands on the timeline.
func (a *API) assignPrincipal(w http.ResponseWriter, r *http.Request, cardID, principalID string) {
	if !principalIDShape.MatchString(principalID) {
		writeError(w, 400, fmt.Sprintf("principal_id must match ^%s$ (for example principal_000001), got %q", config.PrincipalIDPattern, boundedtext.Value(principalID)))
		return
	}
	principal, err := a.principals.Get(principalID)
	if errors.Is(err, principalstore.ErrNotFound) {
		writeError(w, 400, fmt.Sprintf("principal_id %s does not exist in principal-store", boundedtext.Value(principalID)))
		return
	}
	if err != nil {
		// The check failed, so the write does not happen. Accepting the row
		// because we could not ask would be exactly the silently wrong row the
		// check exists to prevent.
		writeError(w, 502, "principal-store check failed: "+err.Error())
		return
	}
	if principal.Disabled() {
		writeError(w, 400, fmt.Sprintf("principal_id %s is disabled in principal-store", boundedtext.Value(principalID)))
		return
	}
	assignment, created, err := a.store.AssignPrincipalToCardIfVersion(cardID, principalID, actorFrom(r), r.Header.Get("If-Match"))
	if errors.Is(err, db.ErrAssignmentsChanged) {
		writeError(w, 412, err.Error())
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if !created {
		writeJSON(w, 200, assignment)
		return
	}
	// Someone being put on the card is something that happened to it, like an
	// agent being handed it, so it joins the timeline. It says nothing about
	// whether the work is runnable, so it carries the clock state the card was
	// already in: an assignment neither starts nor stops the budget clock.
	state, err := a.clockStateCarriedForward(cardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if err := a.recordEventAndFireMessageTriggers(&model.CardEvent{
		CardID: cardID, Kind: model.EventAssigned, ClockState: state, Actor: actorFrom(r),
		Summary:    principalID,
		Detail:     json.RawMessage(fmt.Sprintf(`{"principal_id":%q}`, principalID)),
		OccurredAt: assignment.CreatedAt,
	}); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, assignment)
}

func (a *API) unassignPrincipal(w http.ResponseWriter, r *http.Request, cardID, principalID string) {
	if err := a.store.UnassignPrincipalFromCard(cardID, principalID); err != nil {
		mapDBErr(w, err)
		return
	}
	state, err := a.clockStateCarriedForward(cardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if err := a.recordEventAndFireMessageTriggers(&model.CardEvent{
		CardID: cardID, Kind: model.EventUnassigned, ClockState: state, Actor: actorFrom(r),
		Summary: principalID,
		Detail:  json.RawMessage(fmt.Sprintf(`{"principal_id":%q}`, principalID)),
	}); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// assignmentsByPrincipal is the reverse lookup: every card a principal is on,
// across every board, oldest assignment first.
func (a *API) assignmentsByPrincipal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	principalID := r.URL.Query().Get("principal_id")
	if principalID == "" {
		writeError(w, 400, "principal_id is required")
		return
	}
	assignments, err := a.store.ListCardAssignmentsByPrincipal(principalID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			mapDBErr(w, err)
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	if access := principalBoardAccessFrom(r); access != nil {
		visible := assignments[:0]
		for _, assignment := range assignments {
			cardVisible, err := a.cardVisibleTo(access, assignment.CardID)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			if cardVisible {
				visible = append(visible, assignment)
			}
		}
		assignments = visible
	}
	writeJSON(w, 200, assignments)
}
