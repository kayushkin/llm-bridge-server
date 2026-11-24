package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/kanban-store/internal/bundlestore"
	"github.com/kayushkin/kanban-store/internal/config"
	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/filestore"
	"github.com/kayushkin/kanban-store/internal/llmbridge"
	"github.com/kayushkin/kanban-store/internal/messaging"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/noteboard"
	"github.com/kayushkin/kanban-store/internal/principalstore"
	"github.com/kayushkin/kanban-store/internal/referencestore"
	"github.com/kayushkin/kanban-store/internal/timeaccounting"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

type API struct {
	store     *db.Store
	noteboard *noteboard.Client
	// principals is consulted on exactly one write: assigning a principal to a
	// card. See internal/principalstore for why this otherwise-dumb store checks
	// that one reference.
	principals *principalstore.Client
	// references is reference-store, which owns the list of reference types.
	references *referencestore.Client
	// bridge is consulted on one write: setting a board's default agent or
	// default instance. See internal/llmbridge for why.
	bridge *llmbridge.Client
	// bundles is consulted on one write: setting a board's default bundle.
	bundles *bundlestore.Client
	// messages fires the board's message triggers after a card event is
	// recorded. It has no sender until SetMessageSender is called.
	messages *messaging.Dispatcher
	// messageTriggersInFlight counts dispatches started by fireMessageTriggers
	// that have not returned; WaitForMessageTriggers waits on it.
	messageTriggersInFlight sync.WaitGroup
	// principalEnforcement is nil unless SetPrincipalEnforcement was called;
	// see principal_access.go.
	principalEnforcement *PrincipalEnforcement
	// files is file-store, which owns uploaded bytes. Nil until SetFileStore is
	// called, and then every attachment route answers 503; see attachments.go.
	files *filestore.Client
	// settings is what GET /settings describes: every environment variable the
	// process reads, as config.SettingDefinitions declares them.
	settings *servicesettings.Registry
}

// New panics on a nil settings registry, at boot: the registry is an argument
// rather than a setter so that no command can serve boards and forget
// GET /settings.
func New(store *db.Store, nb *noteboard.Client, principals *principalstore.Client, bridge *llmbridge.Client, bundles *bundlestore.Client, settings *servicesettings.Registry) *API {
	if settings == nil {
		panic("kanban-store: a settings registry is required; GET /settings describes the service from it")
	}
	return &API{store: store, noteboard: nb, principals: principals, bridge: bridge, bundles: bundles, messages: messaging.NewDispatcher(store, nb), settings: settings}
}

// SettingsPath is where servicesettings.Handler is mounted.
const SettingsPath = "/settings"

func (a *API) Handler() http.Handler {
	return cors(a.principalGate(a.routes()))
}

// routes is every route with no gate in front of it. Handler puts the gate
// there; the bulk card route runs single-card requests through these same
// handlers after applying the gate's rules to each one itself.
func (a *API) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", a.health)

	// what the process was started with; the service token or an administrator.
	// Nothing is Editable, so PUT /settings/{key} is not mounted.
	mux.Handle(SettingsPath, servicesettings.Handler(a.settings, SettingsPath))

	// boards
	mux.HandleFunc("/api/boards", a.boards)
	mux.HandleFunc("/api/boards/", a.boardsTree)

	// message triggers (single-trigger ops; a board's list is under /api/boards/:id)
	mux.HandleFunc("/api/message-triggers/", a.messageTriggerByID)
	mux.HandleFunc("/api/message-trigger-options", a.messageTriggerOptions)

	// columns (single-resource ops, and a column's cards a page at a time)
	mux.HandleFunc("/api/columns/", a.columnScoped)

	// cards (single-resource ops, including move + delete)
	mux.HandleFunc("/api/cards/", a.cardScoped)

	// links (delete by link id)
	mux.HandleFunc("/api/links/", a.linkByID)

	// card notes (delete by note id)
	mux.HandleFunc("/api/notes/", a.notesByID)

	// reverse lookups by entity
	mux.HandleFunc("/api/entities/", a.entityScoped)

	// reverse lookup by principal: every card someone is assigned to
	mux.HandleFunc("/api/assignments", a.assignmentsByPrincipal)

	// tickets across every board, newest first; and the ticket vocabularies:
	// what a channel and a lifecycle state may be
	mux.HandleFunc("/api/tickets", a.listTickets)
	mux.HandleFunc("/api/ticket-channels", a.ticketChannels)
	mux.HandleFunc("/api/ticket-lifecycle-states", a.ticketLifecycleStates)

	// note vocabulary: who a note may be written for
	mux.HandleFunc("/api/note-visibilities", a.noteVisibilities)

	// work-state vocabulary: the states every board's columns map onto
	mux.HandleFunc("/api/work-states", a.workStates)

	// assignment pool vocabulary: how a board's pool chooses among its members
	mux.HandleFunc("/api/assignment-strategies", a.assignmentStrategies)

	// entity-type registry & cross-entity tag listing
	mux.HandleFunc("/api/entity-types", a.entityTypes)
	mux.HandleFunc("/api/tags", a.allTags)

	// search delegate
	mux.HandleFunc("/api/search", a.search)

	// one command applied to many cards, each through its single-card route
	mux.HandleFunc("/api/bulk-card-commands", a.bulkCardCommands)

	return mux
}

func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Expose-Headers", "ETag")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, If-Match, "+PrincipalIDHeader)
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func mapDBErr(w http.ResponseWriter, err error) {
	if errors.Is(err, db.ErrBoardChanged) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, 404, "not found")
		return
	}
	writeError(w, 500, err.Error())
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	counts, err := a.store.Counts()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "counts": counts})
}

// ============================ Boards ============================

func (a *API) boards(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		includeArchived := r.URL.Query().Get("include_archived") == "true"
		boards, err := a.store.ListBoards(includeArchived)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if access := principalBoardAccessFrom(r); access != nil {
			viewable := access.ViewableBoardIDs()
			visible := boards[:0]
			for _, board := range boards {
				if viewable[board.ID] {
					visible = append(visible, board)
				}
			}
			boards = visible
		}
		writeJSON(w, 200, boards)
	case "POST":
		var req model.CreateBoardRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		b, err := a.store.CreateBoard(&req)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if access := principalBoardAccessFrom(r); access != nil {
			// A principal who creates a board administers it. Without that grant
			// the board would be invisible to the one who made it, so a failed
			// grant undoes the board rather than leaving it orphaned.
			if err := a.principalEnforcement.Grants.GrantBoardAdministration(access.PrincipalID, b.ID); err != nil {
				if deleteErr := a.store.DeleteBoard(b.ID); deleteErr != nil {
					writeError(w, 500, fmt.Sprintf("board %s was created, granting its creator can_administer failed (%v), and deleting it again also failed: %v", b.ID, err, deleteErr))
					return
				}
				writeError(w, 502, "board not created: could not give its creator can_administer: "+err.Error())
				return
			}
		}
		writeJSON(w, 201, b)
	default:
		writeError(w, 405, "method not allowed")
	}
}

// boardsTree handles /api/boards/:id and its sub-resources.
//
//	/api/boards/:id
//	/api/boards/:id/columns
//	/api/boards/:id/columns/reorder
//	/api/boards/:id/cards
//	/api/boards/:id/cards/:cardID   (PUT to attach existing card)
func (a *API) boardsTree(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/boards/")
	if rest == "" {
		writeError(w, 400, "missing board id")
		return
	}
	parts := strings.Split(rest, "/")
	boardID := parts[0]

	if len(parts) == 1 {
		a.boardByID(w, r, boardID)
		return
	}
	switch parts[1] {
	case "card-page":
		if len(parts) == 2 {
			a.boardCardPage(w, r, boardID)
			return
		}
	case "columns":
		if len(parts) == 2 {
			a.boardColumns(w, r, boardID)
			return
		}
		if len(parts) == 3 && parts[2] == "reorder" {
			a.reorderColumns(w, r, boardID)
			return
		}
	case "cards":
		if len(parts) == 2 {
			a.boardCards(w, r, boardID)
			return
		}
		if len(parts) == 3 {
			a.boardCardByID(w, r, boardID, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "effective-defaults" {
			a.cardEffectiveDefaults(w, r, boardID, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "classification" {
			a.cardClassificationLabels(w, r, boardID, parts[2])
			return
		}
	case "priority-levels":
		if len(parts) == 2 {
			a.boardPriorityLevels(w, r, boardID)
			return
		}
	case "tag-rules":
		if len(parts) == 2 {
			a.boardTagRules(w, r, boardID)
			return
		}
	case "effective-defaults":
		if len(parts) == 2 {
			a.boardEffectiveDefaults(w, r, boardID)
			return
		}
	case "message-triggers":
		if len(parts) == 2 {
			a.boardMessageTriggers(w, r, boardID)
			return
		}
	case "message-deliveries":
		if len(parts) == 2 {
			a.boardMessageDeliveries(w, r, boardID)
			return
		}
	case "access":
		if len(parts) == 2 {
			a.boardCallerAccess(w, r, boardID)
			return
		}
	case "classification":
		a.boardClassificationRoutes(w, r, boardID, parts[2:])
		return
	}
	writeError(w, 404, "not found")
}

// boardCallerAccess answers what the caller of this request may do on a board,
// so a client can offer only the commands the store would accept.
func (a *API) boardCallerAccess(w http.ResponseWriter, r *http.Request, boardID string) {
	if r.Method != "GET" {
		writeError(w, 405, "method not allowed")
		return
	}
	if _, err := a.store.GetBoard(boardID); err != nil {
		mapDBErr(w, err)
		return
	}
	access := principalBoardAccessFrom(r)
	level := BoardAccessAdminister
	if access != nil {
		level = access.LevelOn(boardID)
	}
	writeJSON(w, 200, callerAccessAt(r, access, level))
}

func (a *API) boardByID(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case "GET":
		b, err := a.store.GetBoard(id)
		if err != nil {
			mapDBErr(w, err)
			return
		}
		w.Header().Set("ETag", strconv.Quote(b.UpdatedAt.Format(time.RFC3339Nano)))
		writeJSON(w, 200, b)
	case "PATCH":
		var req model.UpdateBoardRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := a.checkBoardSettings(&req); err != nil {
			writeSettingsCheckFailure(w, err)
			return
		}
		var expected *time.Time
		if ifMatch := r.Header.Get("If-Match"); ifMatch != "" && ifMatch != "*" {
			version, err := strconv.Unquote(ifMatch)
			if err != nil {
				writeError(w, 400, "If-Match must be the board's quoted ETag or *")
				return
			}
			parsed, err := time.Parse(time.RFC3339Nano, version)
			if err != nil {
				writeError(w, 400, "If-Match must be the board's quoted ETag or *")
				return
			}
			expected = &parsed
		}
		b, err := a.store.UpdateBoardIfVersion(id, &req, expected)
		if errors.Is(err, db.ErrBoardChanged) && r.Header.Get("If-Match") != "" {
			writeError(w, http.StatusPreconditionFailed, err.Error())
			return
		}
		if err != nil {
			mapDBErr(w, err)
			return
		}
		w.Header().Set("ETag", strconv.Quote(b.UpdatedAt.Format(time.RFC3339Nano)))
		writeJSON(w, 200, b)
	case "DELETE":
		if err := a.store.DeleteBoard(id); err != nil {
			mapDBErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	default:
		writeError(w, 405, "method not allowed")
	}
}

// ============================ Columns ============================

func (a *API) boardColumns(w http.ResponseWriter, r *http.Request, boardID string) {
	switch r.Method {
	case "GET":
		cols, err := a.store.ListColumns(boardID)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, cols)
	case "POST":
		var req model.CreateColumnRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		c, err := a.store.CreateColumn(boardID, &req)
		if err != nil {
			mapDBErr(w, err)
			return
		}
		writeJSON(w, 201, c)
	default:
		writeError(w, 405, "method not allowed")
	}
}

// reorderColumns accepts {"columns":[{"id":"...","position":1.0}, ...]}.
func (a *API) reorderColumns(w http.ResponseWriter, r *http.Request, boardID string) {
	if r.Method != "POST" {
		writeError(w, 405, "method not allowed")
		return
	}
	var req struct {
		Columns []struct {
			ID       string  `json:"id"`
			Position float64 `json:"position"`
		} `json:"columns"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid JSON")
		return
	}
	for _, c := range req.Columns {
		if _, err := a.store.UpdateColumn(c.ID, &model.UpdateColumnRequest{Position: &c.Position}); err != nil {
			mapDBErr(w, err)
			return
		}
	}
	cols, err := a.store.ListColumns(boardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, cols)
}

// columnScoped routes /api/columns/:id and /api/columns/:id/cards.
func (a *API) columnScoped(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/columns/")
	if rest == "" {
		writeError(w, 400, "missing column id")
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "cards" {
		a.columnCards(w, r, parts[0])
		return
	}
	if len(parts) != 1 {
		writeError(w, 404, "not found")
		return
	}
	a.columnByID(w, r, parts[0])
}

func (a *API) columnByID(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case "GET":
		c, err := a.store.GetColumn(id)
		if err != nil {
			mapDBErr(w, err)
			return
		}
		writeJSON(w, 200, c)
	case "PATCH":
		var req model.UpdateColumnRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if req.AutoStatus != nil && *req.AutoStatus != "" && *req.AutoStatus != "open" && *req.AutoStatus != "done" && *req.AutoStatus != "archived" {
			writeError(w, 400, "auto_status must be one of: open, done, archived")
			return
		}
		c, err := a.store.UpdateColumn(id, &req)
		if err != nil {
			mapDBErr(w, err)
			return
		}
		writeJSON(w, 200, c)
	case "DELETE":
		if err := a.store.DeleteColumn(id); err != nil {
			mapDBErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	default:
		writeError(w, 405, "method not allowed")
	}
}

// ============================ Cards (board-scoped) ============================

func (a *API) boardCards(w http.ResponseWriter, r *http.Request, boardID string) {
	switch r.Method {
	case "GET":
		// A board with 6,466 cards on it answered 12 MB per read, on a page that
		// polls every fifteen seconds. limit caps each column; the column's own
		// endpoint fetches the rest a page at a time.
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil {
			limit = 0
		}
		filter, err := cardFilterFromQuery(r)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		content, err := cardContentQueryFromRequest(r)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		view, err := a.assembleBoardView(boardID, limit, filter, content, principalBoardAccessFrom(r))
		if err != nil {
			if content.isEmpty() {
				mapDBErr(w, err)
			} else {
				writeCardReadError(w, err)
			}
			return
		}
		writeJSON(w, 200, view)
	case "POST":
		a.createCardOnBoard(w, r, boardID)
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (a *API) boardCardByID(w http.ResponseWriter, r *http.Request, boardID, cardID string) {
	switch r.Method {
	case "PUT":
		// Attach an existing noteboard item to this board.
		var req model.AttachCardRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		// Verify the card actually exists in noteboard, and read its tags: a
		// tag rule may name who the card goes to on this board.
		item, err := a.noteboard.GetItem(cardID)
		if err != nil {
			writeNoteboardItemError(w, err)
			return
		}
		cardTags, err := tagsOfNoteboardItem(item)
		if err != nil {
			writeError(w, 502, err.Error())
			return
		}
		if err := a.checkWIP(req.ColumnID); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		arrivalAssignee, err := a.assigneeOnArrival(boardID, cardTags)
		if err != nil {
			writeSettingsCheckFailure(w, err)
			return
		}
		p, err := a.store.AttachCard(boardID, cardID, &req)
		if err != nil {
			mapDBErr(w, err)
			return
		}
		if err := a.recordEventAndFireMessageTriggers(&model.CardEvent{
			CardID: cardID, BoardID: boardID, Kind: model.EventCardAttached,
			ClockState: a.clockStateForColumn(cardID, boardID, req.ColumnID),
			Actor:      actorFrom(r), ToColumnID: req.ColumnID,
		}); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if _, err := a.applyAssigneeOnArrival(cardID, boardID, arrivalAssignee, actorFrom(r)); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if err := a.applyDefaultSpendCeilingOnAttach(cardID, item); err != nil {
			writeError(w, 502, err.Error())
			return
		}
		writeJSON(w, 201, p)
	case "DELETE":
		if err := a.store.DetachCard(boardID, cardID); err != nil {
			mapDBErr(w, err)
			return
		}
		// The card left this board, so its clock on this board stops. The log stays:
		// a card that was here and went is part of what happened.
		if err := a.recordEventAndFireMessageTriggers(&model.CardEvent{
			CardID: cardID, BoardID: boardID, Kind: model.EventCardDetached,
			ClockState: model.ClockStopped, Actor: actorFrom(r),
		}); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (a *API) createCardOnBoard(w http.ResponseWriter, r *http.Request, boardID string) {
	var req model.CreateCardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid JSON")
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	col, err := a.store.GetColumn(req.ColumnID)
	if err != nil {
		mapDBErr(w, err)
		return
	}
	if col.BoardID != boardID {
		writeError(w, 400, "column does not belong to this board")
		return
	}
	if err := a.checkWIP(req.ColumnID); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	arrivalAssignee, err := a.assigneeOnArrival(boardID, req.Tags)
	if err != nil {
		writeSettingsCheckFailure(w, err)
		return
	}

	// A priority with no ceiling of its own takes its rung's default ceiling.
	if req.Priority != nil && req.AutoHoldAtUSD == nil {
		defaultCeiling, err := a.lowestDefaultSpendCeilingForPriority([]string{boardID}, *req.Priority)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		req.AutoHoldAtUSD = defaultCeiling
	}

	// Create the noteboard item first — it's the source of truth.
	item, err := a.noteboard.CreateItem(noteboard.CreateItemPayload{
		Type:          "todo",
		Title:         req.Title,
		Body:          req.Body,
		Tags:          req.Tags,
		Priority:      req.Priority,
		ListID:        req.ListID,
		DueAt:         req.DueAt,
		ParentID:      req.ParentID,
		Hold:          req.Hold,
		HoldReason:    req.HoldReason,
		AutoHoldAtUSD: req.AutoHoldAtUSD,
	})
	if err != nil {
		writeError(w, 502, "noteboard create failed: "+err.Error())
		return
	}
	cardID, _ := item["id"].(string)
	if cardID == "" {
		writeError(w, 502, "noteboard returned no id")
		return
	}
	// Then attach.
	p, err := a.store.AttachCard(boardID, cardID, &model.AttachCardRequest{
		ColumnID: req.ColumnID,
		Position: req.Position,
	})
	// If the destination column has auto_status, apply it now so card creation
	// is symmetric with MoveCard (otherwise classifier-created cards in Done
	// stay status=open in noteboard).
	//
	// Non-fatal, because the card and its placement both exist by this point —
	// but NOT unreported. The failure leaves the item reading "open" in a Done
	// column, which is the exact state this write exists to prevent, and "open"
	// is also what a column with no auto_status returns, so a silent failure is
	// indistinguishable from nothing having been asked for. noteboard is the
	// source of truth agents read to decide what work is still open, so a
	// dropped write here hands already-finished work back to the queue.
	var autoStatusApplied, autoStatusError string
	if col.AutoStatus != nil && *col.AutoStatus != "" {
		patched, perr := a.noteboard.PatchItem(cardID, map[string]any{"status": *col.AutoStatus})
		if perr != nil {
			autoStatusError = perr.Error()
		} else {
			item = patched
			autoStatusApplied = *col.AutoStatus
		}
	}
	if err != nil {
		// Best-effort cleanup: the noteboard item now exists with no placement.
		// Leaving it in place is the safer default — user can find it on /notes.
		writeError(w, 500, err.Error())
		return
	}
	// The card's clock starts here. A card created straight into a column that
	// stops the clock (a classifier filing already-handled mail under "No action")
	// is born finished, and says so.
	if err := a.recordEventAndFireMessageTriggers(&model.CardEvent{
		CardID: cardID, BoardID: boardID, Kind: model.EventCardCreated,
		ClockState: a.clockStateForColumn(cardID, boardID, req.ColumnID),
		Actor:      actorFrom(r), Summary: req.Title, ToColumnID: req.ColumnID,
	}); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	assignments, err := a.applyAssigneeOnArrival(cardID, boardID, arrivalAssignee, actorFrom(r))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	workStates, err := a.cardWorkStatesForCaller([]string{cardID}, principalBoardAccessFrom(r))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, model.CardView{
		Placement: p, Item: item, Assignments: assignments,
		WorkState: workStates[cardID].State, WorkStateSource: workStates[cardID].Source,
		AutoStatusApplied: autoStatusApplied, AutoStatusError: autoStatusError,
	})
}

// checkWIP returns an error if attaching another card would exceed the column's wip_limit.
func (a *API) checkWIP(columnID string) error {
	col, err := a.store.GetColumn(columnID)
	if err != nil {
		return err
	}
	if col.WIPLimit == nil {
		return nil
	}
	n, err := a.store.CountColumnCards(columnID)
	if err != nil {
		return err
	}
	if n >= *col.WIPLimit {
		return errors.New("WIP limit reached for column")
	}
	return nil
}

// ============================ Cards (single-resource) ============================
//
//	/api/cards/:cardID                — PATCH (forward to noteboard) | DELETE (archive)
//	/api/cards/:cardID/move           — POST  (move within/across columns)
//	/api/cards/:cardID/placements     — GET   (all boards this card is on)
//	/api/cards/:cardID/links          — GET, POST
//	/api/cards/:cardID/events         — GET, POST  (the action log)
//	/api/cards/:cardID/notes          — GET, POST  (status updates and summaries)
//	/api/cards/:cardID/timeline       — GET   (events + notes + the time they took)
//	/api/cards/:cardID/assignments    — GET   (who is on the card)
//	/api/cards/:cardID/assignments/:principalID — PUT (idempotent) | DELETE
func (a *API) cardScoped(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/cards/")
	if rest == "" {
		writeError(w, 400, "missing card id")
		return
	}
	parts := strings.Split(rest, "/")
	cardID := parts[0]

	if len(parts) == 1 {
		a.cardByID(w, r, cardID)
		return
	}
	switch parts[1] {
	case "move":
		a.moveCard(w, r, cardID)
		return
	case "placements":
		a.cardPlacements(w, r, cardID)
		return
	case "links":
		a.cardLinks(w, r, cardID)
		return
	case "hold":
		a.holdCard(w, r, cardID, true)
		return
	case "unhold":
		a.holdCard(w, r, cardID, false)
		return
	case "events":
		a.cardEvents(w, r, cardID)
		return
	case "notes":
		a.cardNotes(w, r, cardID)
		return
	case "timeline":
		a.cardTimeline(w, r, cardID)
		return
	case "ticket":
		a.cardTicket(w, r, cardID)
		return
	case "time-entries":
		if len(parts) == 2 {
			a.cardTimeEntries(w, r, cardID)
			return
		}
	case "attachments":
		if len(parts) == 2 {
			a.cardAttachments(w, r, cardID)
			return
		}
		if len(parts) == 3 && parts[2] != "" {
			a.cardAttachmentByFile(w, r, cardID, parts[2])
			return
		}
		if len(parts) == 4 && parts[2] != "" && parts[3] == "content" {
			a.cardAttachmentContent(w, r, cardID, parts[2])
			return
		}
	case "assignments":
		if len(parts) == 2 {
			a.cardAssignments(w, r, cardID)
			return
		}
		if len(parts) == 3 && parts[2] != "" {
			a.cardAssignmentByPrincipal(w, r, cardID, parts[2])
			return
		}
	}
	writeError(w, 404, "not found")
}

// holdCard is the stop/play button. The hold lives on the noteboard item, not on
// this board — which is what lets a card be paused in ANY column instead of only
// by being dragged into a designated gate column, and what makes the pause bind
// on the noteboard-discovery path that never looks at a board at all.
func (a *API) holdCard(w http.ResponseWriter, r *http.Request, cardID string, hold bool) {
	if r.Method != "POST" {
		writeError(w, 405, "method not allowed")
		return
	}
	var item noteboard.Item
	var err error
	holdReason := ""
	if hold {
		var req struct {
			Reason string `json:"reason"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		holdReason = req.Reason
		item, err = a.noteboard.HoldItem(cardID, req.Reason)
	} else {
		item, err = a.noteboard.UnholdItem(cardID)
	}
	if err != nil {
		writeNoteboardItemError(w, err)
		return
	}
	// Pressing stop parks the work wherever it sits, so the clock pauses on every
	// board at once — which is why this event carries no board.
	kind, state := model.EventCardUnheld, model.ClockRunning
	if hold {
		kind, state = model.EventCardHeld, model.ClockPaused
	}
	if err := a.recordEventAndFireMessageTriggers(&model.CardEvent{
		CardID: cardID, Kind: kind, ClockState: state, Actor: actorFrom(r), Summary: holdReason,
	}); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, item)
}

// writeNoteboardItemError answers a failed noteboard call about one card: 404
// when noteboard says the item does not exist, 502 when noteboard could not be
// asked or failed. A caller told 502 retries; a caller told 404 stops.
func writeNoteboardItemError(w http.ResponseWriter, err error) {
	if noteboard.IsNotFound(err) {
		writeError(w, 404, err.Error())
		return
	}
	writeError(w, 502, err.Error())
}

func (a *API) cardByID(w http.ResponseWriter, r *http.Request, cardID string) {
	switch r.Method {
	case "GET":
		a.cardDetail(w, r, cardID)
	case "PATCH":
		// Forward arbitrary patch to noteboard so callers can edit title/body/tags/etc.
		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		// A priority change may carry the new rung's default spend ceiling with it,
		// in the same noteboard write. A patch that names a ceiling decides it.
		if rawPriority, setsPriority := patch["priority"]; setsPriority {
			if _, setsCeiling := patch["auto_hold_at_usd"]; !setsCeiling {
				newPriorityValue, err := priorityValueOfPatch(rawPriority)
				if err != nil {
					writeError(w, 400, err.Error())
					return
				}
				itemBefore, err := a.noteboard.GetItem(cardID)
				if err != nil {
					writeNoteboardItemError(w, err)
					return
				}
				defaultCeiling, err := a.spendCeilingForPriorityChange(cardID, itemBefore, newPriorityValue)
				if err != nil {
					writeError(w, 502, err.Error())
					return
				}
				if defaultCeiling != nil {
					patch["auto_hold_at_usd"] = *defaultCeiling
				}
			}
		}
		// If-Match goes to noteboard as sent: it owns the item and its version,
		// and the check has to happen where the write does.
		item, err := a.noteboard.PatchItemIfMatch(cardID, patch, r.Header.Get("If-Match"))
		var changed *noteboard.ItemChangedError
		if errors.As(err, &changed) {
			// Someone else saved first. noteboard's answer carries the item as it
			// is now, and is relayed unchanged.
			w.Header().Set("Content-Type", "application/json")
			if changed.ETag != "" {
				w.Header().Set("ETag", changed.ETag)
			}
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write(changed.Body)
			return
		}
		if err != nil {
			writeNoteboardItemError(w, err)
			return
		}
		// Completing a card stops its clock, however the completion was expressed —
		// dragged into a Done column, or patched straight to done from anywhere.
		if status, ok := patch["status"].(string); ok && (status == "done" || status == "archived") {
			if err := a.recordEventAndFireMessageTriggers(&model.CardEvent{
				CardID: cardID, Kind: model.EventCardCompleted, ClockState: model.ClockStopped,
				Actor: actorFrom(r), Summary: status,
			}); err != nil {
				writeError(w, 500, err.Error())
				return
			}
		}
		writeJSON(w, 200, item)
	case "DELETE":
		hard := r.URL.Query().Get("hard") == "true"
		if hard {
			// A purged card's files go with it, first: they are what is left
			// of what people sent, and a card that is gone can no longer say
			// who may read them. If they cannot be destroyed the card stays.
			if status, err := a.purgeAttachmentsOfCard(cardID); err != nil {
				writeError(w, status, err.Error())
				return
			}
			// The text each classification decision was given is the card's
			// text; it goes with the card, and the digest stays.
			if err := a.store.RemoveClassificationSourcesOfCard(cardID); err != nil {
				writeError(w, 500, "the card's classification sources could not be removed, so the card stays: "+err.Error())
				return
			}
		}
		if err := a.noteboard.DeleteItem(cardID, hard); err != nil {
			writeNoteboardItemError(w, err)
			return
		}
		// On hard delete the item is gone for good, so drop its placements too.
		// On a reversible delete the placements stay, because the delete itself
		// is undoable and restoring the item has to restore it onto its boards.
		// Until then the card still occupies its column with a null item — the
		// same shape as an orphan — so a frontend must render that case.
		if hard {
			_ = a.store.DetachCardEverywhere(cardID)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	default:
		writeError(w, 405, "method not allowed")
	}
}

// cardDetail reads one card through the gate: its noteboard item, the
// placements the caller can see, its links, assignments and ticket, and what
// the caller may do to it. noteboard itself checks no caller, so a client that
// must not see every item reads a card's body here.
func (a *API) cardDetail(w http.ResponseWriter, r *http.Request, cardID string) {
	placements, err := a.store.ListPlacementsByCard(cardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if len(placements) == 0 {
		// A noteboard item on no board is not a card, and answering it here
		// would make this route a way to read any note by id.
		writeError(w, 404, "not found")
		return
	}
	access := principalBoardAccessFrom(r)
	level := BoardAccessAdminister
	if access != nil {
		boardIDs := make([]string, 0, len(placements))
		for _, placement := range placements {
			boardIDs = append(boardIDs, placement.BoardID)
		}
		level = levelOnCard(access, boardIDs)
	}
	item, err := a.noteboard.GetItem(cardID)
	if err != nil && !noteboard.IsNotFound(err) {
		writeError(w, 502, err.Error())
		return
	}
	detail := model.CardDetail{CardID: cardID, Placements: []*model.Placement{}, Access: callerAccessAt(r, access, level)}
	if item != nil {
		detail.Item = item
	}
	for _, placement := range placements {
		if access == nil || access.LevelOn(placement.BoardID) >= BoardAccessView {
			detail.Placements = append(detail.Placements, placement)
		}
	}
	workStates, err := a.cardWorkStatesForCaller([]string{cardID}, access)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	detail.WorkState, detail.WorkStateSource = workStates[cardID].State, workStates[cardID].Source
	if detail.Links, err = a.store.ListCardLinks(cardID); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if detail.Assignments, err = a.store.ListCardAssignments(cardID); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	ticket, err := a.ticketView(cardID, access)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		writeError(w, 500, err.Error())
		return
	}
	if err == nil {
		detail.Ticket = ticket
	}
	writeJSON(w, 200, detail)
}

func (a *API) moveCard(w http.ResponseWriter, r *http.Request, cardID string) {
	if r.Method != "POST" {
		writeError(w, 405, "method not allowed")
		return
	}
	var req model.MoveCardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid JSON")
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	// WIP check only when crossing into a new column.
	current, err := a.store.GetPlacement(cardID, req.BoardID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		writeError(w, 500, err.Error())
		return
	}
	if current == nil || current.ColumnID != req.ColumnID {
		if err := a.checkWIP(req.ColumnID); err != nil {
			writeError(w, 409, err.Error())
			return
		}
	}
	p, err := a.store.MoveCard(cardID, &req)
	if err != nil {
		mapDBErr(w, err)
		return
	}
	// Auto-status: if the destination column has auto_status set, PATCH the
	// noteboard item to match. Failure here is non-fatal (the move succeeded
	// in kanban-store) but logged in the response.
	col, _ := a.store.GetColumn(req.ColumnID)
	fromColumn := ""
	if current != nil {
		fromColumn = current.ColumnID
	}
	if err := a.recordEventAndFireMessageTriggers(&model.CardEvent{
		CardID: cardID, BoardID: req.BoardID, Kind: model.EventCardMoved,
		ClockState:   a.clockStateForColumn(cardID, req.BoardID, req.ColumnID),
		Actor:        actorFrom(r),
		FromColumnID: fromColumn, ToColumnID: req.ColumnID,
	}); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	resp := map[string]any{"placement": p}
	if col != nil && col.AutoStatus != nil && *col.AutoStatus != "" {
		if _, err := a.noteboard.PatchItem(cardID, map[string]any{"status": *col.AutoStatus}); err != nil {
			resp["auto_status_error"] = err.Error()
		} else {
			resp["auto_status_applied"] = *col.AutoStatus
		}
	}
	writeJSON(w, 200, resp)
}

func (a *API) cardPlacements(w http.ResponseWriter, r *http.Request, cardID string) {
	if r.Method != "GET" {
		writeError(w, 405, "method not allowed")
		return
	}
	ps, err := a.store.ListPlacementsByCard(cardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if access := principalBoardAccessFrom(r); access != nil {
		visible := ps[:0]
		for _, placement := range ps {
			if access.LevelOn(placement.BoardID) >= BoardAccessView {
				visible = append(visible, placement)
			}
		}
		ps = visible
	}
	writeJSON(w, 200, ps)
}

// ============================ Card links ============================

func (a *API) cardLinks(w http.ResponseWriter, r *http.Request, cardID string) {
	switch r.Method {
	case "GET":
		ls, err := a.store.ListCardLinks(cardID)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, ls)
	case "POST":
		var req model.CreateCardLinkRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		// Mail arriving and an agent being handed the work are things that happened
		// to the card, so they join its timeline. Every other kind of link is a fact
		// about the card rather than an event, and is not logged.
		kind, state, isAction := clockStateForEntityLink(req.EntityType)
		if req.ClockState != "" {
			if !model.ValidClockState(req.ClockState) {
				writeError(w, 400, "clock_state must be one of: "+strings.Join(model.ClockStateNames(), ", "))
				return
			}
			state = req.ClockState
		}
		if (req.OccurredAt != nil || req.ClockState != "") && !isAction {
			// Refused rather than ignored. A caller that backdates a repo link has
			// misunderstood what the link is, and answering 201 would let it believe
			// it had moved something on the timeline.
			writeError(w, 400, fmt.Sprintf(
				"occurred_at and clock_state are only meaningful for a link that records an action; %q records a fact about the card and is never logged",
				req.EntityType))
			return
		}
		l, err := a.store.CreateCardLink(cardID, &req)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if isAction {
			// The link's CreatedAt is when we recorded it; occurred_at is when it
			// happened. They differ for anything filed off a backlog.
			occurred := l.CreatedAt
			if req.OccurredAt != nil {
				occurred = *req.OccurredAt
			}
			if err := a.recordEventAndFireMessageTriggers(&model.CardEvent{
				CardID: cardID, Kind: kind, ClockState: state, Actor: actorFrom(r),
				Summary: l.Label, OccurredAt: occurred,
				Detail: json.RawMessage(fmt.Sprintf(`{"entity_type":%q,"entity_ref":%q}`, req.EntityType, req.EntityRef)),
			}); err != nil {
				writeError(w, 500, err.Error())
				return
			}
		}
		writeJSON(w, 201, l)
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (a *API) linkByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/links/")
	if id == "" {
		writeError(w, 400, "missing link id")
		return
	}
	if r.Method != "DELETE" {
		writeError(w, 405, "method not allowed")
		return
	}
	if err := a.store.DeleteCardLink(id); err != nil {
		mapDBErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// ============================ Entities ============================
//
//	/api/entities/:type/:ref/cards
//	/api/entities/:type/:ref/tags
//	/api/entities/:type/:ref/tags/:tag
func (a *API) entityScoped(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/entities/")
	parts := strings.Split(rest, "/")
	if len(parts) < 3 {
		writeError(w, 400, "expected /api/entities/:type/:ref/...")
		return
	}
	etype, eref, sub := parts[0], parts[1], parts[2]

	switch sub {
	case "cards":
		if r.Method != "GET" {
			writeError(w, 405, "method not allowed")
			return
		}
		a.entityCards(w, r, etype, eref)
	case "tags":
		if len(parts) == 4 {
			a.entityTagOne(w, r, etype, eref, parts[3])
			return
		}
		a.entityTagsList(w, r, etype, eref)
	default:
		writeError(w, 404, "not found")
	}
}

func (a *API) entityTagsList(w http.ResponseWriter, r *http.Request, etype, eref string) {
	switch r.Method {
	case "GET":
		ts, err := a.store.ListEntityTags(etype, eref)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, ts)
	case "POST":
		var req model.CreateEntityTagRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		t, err := a.store.AddEntityTag(etype, eref, req.Tag)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, t)
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (a *API) entityTagOne(w http.ResponseWriter, r *http.Request, etype, eref, tag string) {
	if r.Method != "DELETE" {
		writeError(w, 405, "method not allowed")
		return
	}
	if err := a.store.DeleteEntityTag(etype, eref, tag); err != nil {
		mapDBErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// entityTypes serves reference-store's list of reference types unchanged, for
// the clients that have always read it here.
func (a *API) entityTypes(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeError(w, 405, "method not allowed")
		return
	}
	if a.references == nil {
		writeError(w, 503, "kanban-store has no reference-store to read the list of types from")
		return
	}
	rows, err := a.references.Types(r.Context())
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, rows)
}

// SetReferenceStore gives the API reference-store, which owns the list of
// reference types.
func (a *API) SetReferenceStore(references *referencestore.Client) { a.references = references }

func (a *API) allTags(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tag := q.Get("tag")
	etype := q.Get("entity_type")
	if tag != "" {
		results, err := a.store.FindEntitiesByTag(tag, etype)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, results)
		return
	}
	tags, err := a.store.ListAllEntityTags()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, tags)
}

// ============================ Search ============================

// search delegates to noteboard FTS, then optionally filters to items present
// on a board (?board_id=) or in any kanban placement (?on_board=true).
func (a *API) search(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeError(w, 405, "method not allowed")
		return
	}
	q := r.URL.Query()
	query := q.Get("q")
	if query == "" {
		writeError(w, 400, "q is required")
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	items, err := a.noteboard.Search(query, limit)
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	boardID := q.Get("board_id")
	onBoard := q.Get("on_board") == "true"
	access := principalBoardAccessFrom(r)
	if access != nil {
		// A principal searches only cards on boards it can view, so the filter
		// below always runs; a named board it cannot view matches nothing.
		onBoard = true
	}
	if boardID == "" && !onBoard {
		writeJSON(w, 200, items)
		return
	}

	// Filter: keep only items that have at least one placement (optionally on the named board).
	out := items[:0]
	for _, it := range items {
		id, _ := it["id"].(string)
		if id == "" {
			continue
		}
		ps, err := a.store.ListPlacementsByCard(id)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		match := false
		for _, p := range ps {
			if access != nil && access.LevelOn(p.BoardID) < BoardAccessView {
				continue
			}
			if boardID != "" && p.BoardID == boardID {
				match = true
				break
			}
			if boardID == "" && onBoard {
				match = true
				break
			}
		}
		if match {
			out = append(out, it)
		}
	}
	writeJSON(w, 200, out)
}

// ============================ Board view assembly ============================

// assembleBoardView builds the board. limit caps how many cards each column
// carries, in stored order; 0 means all of them.
//
// ⚠️ Paging is in STORED order, which is not the order the board displays. What
// a client sorts by — priority, due date, title — lives in noteboard, so sorting
// the whole board server-side would mean fetching every item on it, which is the
// cost paging exists to avoid. A client showing a page therefore sorts what it
// has, and has to say so.
// cardFilterFromQuery reads the filter a board or column read was asked for.
// A value that cannot mean anything is a 400 naming what would: a filter that
// was silently dropped answers every card, which reads as "all of these match".
func cardFilterFromQuery(r *http.Request) (db.CardFilter, error) {
	query := r.URL.Query()
	filter := db.CardFilter{
		AssigneePrincipalID:  query.Get("assignee"),
		RequesterPrincipalID: query.Get("requester"),
	}
	for name, principalID := range map[string]string{"assignee": filter.AssigneePrincipalID, "requester": filter.RequesterPrincipalID} {
		if principalID != "" && !principalIDShape.MatchString(principalID) {
			return db.CardFilter{}, fmt.Errorf("%s must be a principal-store id matching ^%s$ (for example principal_000010), got %q", name, config.PrincipalIDPattern, principalID)
		}
	}
	for name, target := range map[string]*bool{"unassigned": &filter.Unassigned, "tickets_only": &filter.TicketsOnly} {
		switch value := query.Get(name); value {
		case "", "false":
		case "true":
			*target = true
		default:
			return db.CardFilter{}, fmt.Errorf("%s must be true or false, got %q", name, value)
		}
	}
	if filter.Unassigned && filter.AssigneePrincipalID != "" {
		return db.CardFilter{}, fmt.Errorf("assignee and unassigned=true cannot both be asked for: no card matches both")
	}
	if rawChannel := query.Get("channel"); rawChannel != "" {
		channel, known := model.NormalizeTicketChannel(rawChannel)
		if !known {
			return db.CardFilter{}, model.ErrUnknownTicketChannel(rawChannel)
		}
		filter.Channel = channel
	}
	return filter, nil
}

func (a *API) assembleBoardView(boardID string, limit int, filter db.CardFilter, content cardContentQuery, access *PrincipalBoardAccess) (*model.BoardView, error) {
	b, err := a.store.GetBoard(boardID)
	if err != nil {
		return nil, err
	}
	cols, err := a.store.ListColumns(boardID)
	if err != nil {
		return nil, err
	}
	// Per column, so a limit means "this many of each" rather than a slice of one
	// arbitrary column, and so the count a client needs for "show more" is exact.
	// columnPage cuts each page after every filter, this store's and noteboard's.
	var placements []*model.Placement
	var items []noteboard.Item
	totals := map[string]int{}
	for _, c := range cols {
		page, pageItems, total, err := a.columnPage(c.ID, filter, content, limit, 0)
		if err != nil {
			return nil, err
		}
		totals[c.ID] = total
		placements = append(placements, page...)
		items = append(items, pageItems...)
	}
	ladder, err := a.store.GetPriorityLadder(boardID)
	if err != nil {
		return nil, err
	}
	cards, err := a.assembleCards(b, placements, items, ladder, access)
	if err != nil {
		return nil, err
	}
	byCol := map[string][]model.CardView{}
	var orphans []model.CardView
	for i, card := range cards {
		if items[i] == nil {
			orphans = append(orphans, card)
			continue
		}
		byCol[card.Placement.ColumnID] = append(byCol[card.Placement.ColumnID], card)
	}
	colViews := make([]model.ColumnView, len(cols))
	for i, c := range cols {
		colViews[i] = model.ColumnView{Column: c, Cards: byCol[c.ID], Total: totals[c.ID]}
	}
	return &model.BoardView{Board: b, Columns: colViews, Orphans: orphans, PriorityLadder: ladder}, nil
}

// assembleCards joins only the selected page, keeping content untouched.
func (a *API) assembleCards(b *model.Board, placements []*model.Placement, items []noteboard.Item, ladder *model.PriorityLadder, access *PrincipalBoardAccess) ([]model.CardView, error) {
	ids := make([]string, len(placements))
	for i, p := range placements {
		ids[i] = p.CardID
	}
	// Events and links for exactly the cards on screen, one query each. The links
	// used to be one query per card, which on the largest board here is 6,466 of
	// them for a single read.
	eventsByCard, err := a.store.ListCardEventsForCards(ids, b.ID)
	if err != nil {
		return nil, err
	}
	linksByCard, err := a.store.ListCardLinksForCards(ids)
	if err != nil {
		return nil, err
	}
	assignmentsByCard, err := a.store.ListCardAssignmentsForCards(ids)
	if err != nil {
		return nil, err
	}
	// One query for every ticket on screen, like the links and assignments
	// above: a board of 6,000 cards must not become 6,000 reads.
	ticketsByCard, err := a.store.ListTicketsForCards(ids)
	if err != nil {
		return nil, err
	}
	// A card's shared state can come from another board, so this reads every
	// placement of the cards on screen — still one query.
	workStatesByCard, err := a.cardWorkStatesForCaller(ids, access)
	if err != nil {
		return nil, err
	}
	labelsByCard, err := a.store.ClassificationLabelsForCards(b.ID, ids)
	if err != nil {
		return nil, err
	}
	asOf := time.Now().UTC()

	cards := make([]model.CardView, 0, len(placements))
	for i, p := range placements {
		cv := model.CardView{
			Placement: p, Item: items[i], Links: linksByCard[p.CardID], Assignments: assignmentsByCard[p.CardID],
			Ticket: ticketsByCard[p.CardID], Classification: labelsByCard[p.CardID],
			WorkState: workStatesByCard[p.CardID].State, WorkStateSource: workStatesByCard[p.CardID].Source,
		}
		summary, _ := timeaccounting.Compute(timeaccounting.Input{
			Events: eventsByCard[p.CardID],
			Level:  ladder.LevelFor(model.PriorityOfNoteboardItem(items[i])),
			Hours:  b.BusinessHours,
			Now:    asOf,
		})
		// The segments are the timeline's working, and the timeline endpoint is
		// where they belong. A board asks how much time, not which stretches of it.
		summary.Segments = nil
		cv.Time = summary
		cards = append(cards, cv)
	}
	return cards, nil
}
