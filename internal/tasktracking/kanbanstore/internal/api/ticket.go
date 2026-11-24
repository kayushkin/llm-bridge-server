package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/kayushkin/kanban-store/internal/boundedtext"
	"github.com/kayushkin/kanban-store/internal/config"
	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/principalstore"
)

// The ticket routes: the two facts a card cannot hold, and the lifecycle it
// does not store. See internal/model/ticket.go for why there is no status
// field here.

// ticketChannels serves the channel vocabulary, so no caller builds a picker
// out of whichever channels happen to have been used so far.
func (a *API) ticketChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, model.TicketChannels)
}

// ticketLifecycleStates serves the lifecycle vocabulary. A column is
// classified with one of these; a ticket reads its state from its column.
func (a *API) ticketLifecycleStates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, model.TicketLifecycleStates)
}

// cardTicket is GET | PUT | DELETE /api/cards/{id}/ticket.
func (a *API) cardTicket(w http.ResponseWriter, r *http.Request, cardID string) {
	switch r.Method {
	case http.MethodGet:
		view, err := a.ticketView(cardID, principalBoardAccessFrom(r))
		if errors.Is(err, db.ErrNotFound) {
			writeError(w, 404, "card "+boundedtext.Value(cardID)+" is not a ticket")
			return
		}
		if err != nil {
			writeTicketError(w, err)
			return
		}
		writeJSON(w, 200, view)
	case http.MethodPut:
		a.putCardTicket(w, r, cardID)
	case http.MethodDelete:
		if err := a.store.DeleteTicket(cardID); err != nil {
			mapDBErr(w, err)
			return
		}
		// The card stays exactly where it is: "this is not a ticket" is not
		// "this is not work".
		writeJSON(w, 200, map[string]string{"status": "ok"})
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (a *API) putCardTicket(w http.ResponseWriter, r *http.Request, cardID string) {
	var request model.TicketWriteRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	channel, known := model.NormalizeTicketChannel(request.Channel)
	if !known {
		writeError(w, 400, model.ErrUnknownTicketChannel(request.Channel).Error())
		return
	}
	if !principalIDShape.MatchString(request.RequesterPrincipalID) {
		writeError(w, 400, fmt.Sprintf("requester_principal_id must match ^%s$ (for example principal_000010), got %q",
			config.PrincipalIDPattern, boundedtext.Value(request.RequesterPrincipalID)))
		return
	}
	requester, err := a.principals.Get(request.RequesterPrincipalID)
	if errors.Is(err, principalstore.ErrNotFound) {
		writeError(w, 400, fmt.Sprintf("requester_principal_id %s does not exist in principal-store", boundedtext.Value(request.RequesterPrincipalID)))
		return
	}
	if err != nil {
		// The check failed, so the write does not happen: a ticket pointing at
		// a requester nobody confirmed is the silently wrong row this check
		// exists to prevent.
		writeError(w, 502, "principal-store check failed: "+err.Error())
		return
	}
	if requester.Disabled() {
		writeError(w, 400, fmt.Sprintf("requester_principal_id %s is disabled in principal-store", boundedtext.Value(request.RequesterPrincipalID)))
		return
	}
	// The requester is the outside party. A human or a group here would mean a
	// member of staff had been recorded as the person who asked, and every
	// later "who reported this?" would answer with a colleague.
	if requester.Kind != principalstore.KindContact {
		writeError(w, 400, fmt.Sprintf(
			"requester_principal_id %s is a %q principal; a requester is a %q — resolve the address with principal-store's POST /contacts/resolve and use the id it answers",
			request.RequesterPrincipalID, requester.Kind, principalstore.KindContact))
		return
	}

	if (request.SourceEntityType == "") != (request.SourceEntityRef == "") {
		writeError(w, 400, "source_entity_type and source_entity_ref go together: send both or neither")
		return
	}
	if request.SourceEntityType != "" {
		known, err := a.knownEntityType(r.Context(), request.SourceEntityType)
		if err != nil {
			writeError(w, 502, err.Error())
			return
		}
		if !known {
			writeError(w, 400, fmt.Sprintf("source_entity_type %q is not a reference type (GET /api/entity-types, which is reference-store's list)", request.SourceEntityType))
			return
		}
	}

	_, err = a.store.UpsertTicket(cardID, request.RequesterPrincipalID, channel, request.SourceEntityType, request.SourceEntityRef)
	if errors.Is(err, db.ErrTicketSourceConflict) {
		stored, readErr := a.store.GetTicket(cardID)
		if readErr != nil {
			writeError(w, 500, readErr.Error())
			return
		}
		writeError(w, 409, fmt.Sprintf("card %s is already a ticket from %s %s; a ticket's source is written once",
			cardID, stored.SourceEntityType, stored.SourceEntityRef))
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	view, err := a.ticketView(cardID, principalBoardAccessFrom(r))
	if err != nil {
		writeTicketError(w, err)
		return
	}
	writeJSON(w, 200, view)
}

// ticketView assembles the row, the requester's display name and one lifecycle
// per board the card sits on that the caller can view. A nil access is an
// unrestricted caller and sees every board. A state names its board and its
// column, so one on a board the caller cannot view would tell it the card sits
// there and what that board calls its columns.
func (a *API) ticketView(cardID string, access *PrincipalBoardAccess) (*model.TicketView, error) {
	ticket, err := a.store.GetTicket(cardID)
	if err != nil {
		return nil, err
	}
	states, err := a.store.TicketPlacementStates(cardID)
	if err != nil {
		return nil, err
	}
	if access != nil {
		visible := states[:0]
		for _, state := range states {
			if access.LevelOn(state.BoardID) >= BoardAccessView {
				visible = append(visible, state)
			}
		}
		states = visible
	}
	view := &model.TicketView{Ticket: ticket, States: states}
	// The name is for rendering only, and a principal-store that cannot answer
	// must not fail the read: the ticket is still the ticket without it.
	if requester, err := a.principals.Get(ticket.RequesterPrincipalID); err == nil {
		view.RequesterDisplayName = requester.DisplayName
	}
	return view, nil
}

// knownEntityType asks reference-store, which owns the list of types.
func (a *API) knownEntityType(ctx context.Context, entityType string) (bool, error) {
	if a.references == nil {
		return false, fmt.Errorf("kanban-store has no reference-store to check source_entity_type against")
	}
	return a.references.Known(ctx, entityType)
}

const (
	defaultTicketListLimit = 50
	maxTicketListLimit     = 200
)

// listTickets is GET /api/tickets: every ticket the caller can view, newest
// first, across boards. ?channel= narrows to one channel; ?before= (RFC 3339,
// the previous page's next_before) and ?limit= page it.
func (a *API) listTickets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	query := r.URL.Query()
	var filter db.TicketListFilter
	if raw := query.Get("channel"); raw != "" {
		channel, known := model.NormalizeTicketChannel(raw)
		if !known {
			writeError(w, 400, model.ErrUnknownTicketChannel(raw).Error())
			return
		}
		filter.Channel = channel
	}
	if raw := query.Get("before"); raw != "" {
		before, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeError(w, 400, fmt.Sprintf("before must be an RFC 3339 time, got %q", raw))
			return
		}
		filter.Before = &before
	}
	limit := defaultTicketListLimit
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxTicketListLimit {
			writeError(w, 400, fmt.Sprintf("limit must be a whole number from 1 to %d, got %q", maxTicketListLimit, raw))
			return
		}
		limit = parsed
	}

	access := principalBoardAccessFrom(r)
	page := model.TicketList{Tickets: []model.TicketLogEntry{}}
	// Read a batch at a time until the page is full or the tickets run out: a
	// restricted caller may be unable to see most of a batch.
	for {
		batch, err := a.store.ListTickets(filter, limit)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		for _, entry := range batch {
			if len(page.Tickets) == limit {
				// A visible ticket is past the full page, so there is another.
				page.NextBefore = &page.Tickets[limit-1].Ticket.CreatedAt
				writeJSON(w, 200, page)
				return
			}
			created := entry.Ticket.CreatedAt
			filter.Before = &created
			visible, err := a.cardVisibleTo(access, entry.Ticket.CardID)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			if !visible {
				continue
			}
			view, err := a.ticketView(entry.Ticket.CardID, access)
			if errors.Is(err, db.ErrNotFound) {
				continue // deleted since the batch was read
			}
			if err != nil {
				writeTicketError(w, err)
				return
			}
			entry.TicketView = *view
			page.Tickets = append(page.Tickets, entry)
		}
		if len(batch) < limit {
			break
		}
	}
	writeJSON(w, 200, page)
}

func writeTicketError(w http.ResponseWriter, err error) {
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, 404, "not found")
		return
	}
	writeError(w, 500, err.Error())
}
