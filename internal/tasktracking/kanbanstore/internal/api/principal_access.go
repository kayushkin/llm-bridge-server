package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/kayushkin/kanban-store/internal/boundedtext"
	"github.com/kayushkin/kanban-store/internal/config"
	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/grantstore"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/principalstore"
)

// Who may see and change which board.
//
// There is no off switch. Every request is one of two kinds:
//
//   - An internal service (grant-store checking a board exists, a dispatcher,
//     the classifier) sends X-Kanban-Store-Service-Token. A matching token is
//     unrestricted.
//   - Anything else must carry X-Principal-Id, set by the gateway from a
//     verified login. The principal's board grants are read from grant-store on
//     every request, and the request is allowed, filtered or refused by them.
//
// A request with neither is 401. The header is trusted as sent, so users must
// never reach this store except through a gateway that removes any
// X-Principal-Id or service token the client sent and sets its own.
//
// An **administrator** — principal-store's is_administrator on a human — is
// unrestricted, like the service token: every board, every card, every list,
// whether or not anything was granted. It is read from principal-store on each
// request, so a demotion bites at once.
//
// Access is per board. A card is visible when it sits on at least one board the
// principal can view, and changeable only when the principal can edit every
// board it sits on — a card's content is one noteboard item shared by all its
// boards, so an editor on one board must not rewrite a card on a board they
// cannot see. can_administer includes can_edit, which includes can_view; that
// ordering is kanban-store's rule, and grant-store stores the three as
// independent tuples.
//
// Every route has an explicit rule in authorizeRequest. A route without one is
// refused, so a route added later is closed until someone decides who may use
// it.

const (
	PrincipalIDHeader  = "X-Principal-Id"
	ServiceTokenHeader = "X-Kanban-Store-Service-Token"
)

// BoardAccessLevel orders what a principal may do on one board.
type BoardAccessLevel int

const (
	BoardAccessNone BoardAccessLevel = iota
	BoardAccessView
	BoardAccessEdit
	BoardAccessAdminister
)

var boardAccessLevelOfRelation = map[string]BoardAccessLevel{
	grantstore.RelationCanView:       BoardAccessView,
	grantstore.RelationCanEdit:       BoardAccessEdit,
	grantstore.RelationCanAdminister: BoardAccessAdminister,
}

// PrincipalEnforcement is what every API needs to answer a request at all.
type PrincipalEnforcement struct {
	// ServiceToken is compared in constant time. It must be at least 32
	// characters: a short or empty token would match requests that should not
	// be unrestricted.
	ServiceToken string
	Grants       *grantstore.Client
}

// SetPrincipalEnforcement gives the API its service token and grant-store
// client. It panics on a short token or a nil client, at boot, because either
// would leave the store open while its log says it is checking callers.
func (a *API) SetPrincipalEnforcement(enforcement PrincipalEnforcement) {
	if len(enforcement.ServiceToken) < config.MinimumServiceTokenLength {
		panic(fmt.Sprintf("kanban-store: the service token must be at least %d characters, or requests that omit the header would be unrestricted", config.MinimumServiceTokenLength))
	}
	if enforcement.Grants == nil {
		panic("kanban-store: a grant-store client is required; board access is read from it on every request")
	}
	a.principalEnforcement = &enforcement
}

// PrincipalBoardAccess is one request's principal and what it may do on each
// board. A nil *PrincipalBoardAccess in a request context means unrestricted:
// an internal service sent the service token, or the principal is an
// administrator.
type PrincipalBoardAccess struct {
	PrincipalID string
	levels      map[string]BoardAccessLevel
}

// LevelOn is the principal's access to one board.
func (access *PrincipalBoardAccess) LevelOn(boardID string) BoardAccessLevel {
	return access.levels[boardID]
}

// ViewableBoardIDs is every board the principal can at least view.
func (access *PrincipalBoardAccess) ViewableBoardIDs() map[string]bool {
	viewable := map[string]bool{}
	for boardID, level := range access.levels {
		if level >= BoardAccessView {
			viewable[boardID] = true
		}
	}
	return viewable
}

// relationsHeldAt is every board relation that holds at a level, weakest first:
// the inclusion rule, applied once here so that no client re-implements it.
func relationsHeldAt(level BoardAccessLevel) []string {
	relations := []string{}
	for relation, levelOfRelation := range boardAccessLevelOfRelation {
		if levelOfRelation <= level {
			relations = append(relations, relation)
		}
	}
	sort.Slice(relations, func(i, j int) bool {
		return boardAccessLevelOfRelation[relations[i]] < boardAccessLevelOfRelation[relations[j]]
	})
	return relations
}

// callerAccessAt is the wire answer for a request whose caller holds level.
// A nil access is the service token or an administrator, for whom level is
// not consulted.
func callerAccessAt(r *http.Request, access *PrincipalBoardAccess, level BoardAccessLevel) model.CallerAccess {
	if access == nil {
		return model.CallerAccess{
			PrincipalID:  r.Header.Get(PrincipalIDHeader),
			Unrestricted: true,
			Relations:    relationsHeldAt(BoardAccessAdminister),
		}
	}
	return model.CallerAccess{PrincipalID: access.PrincipalID, Relations: relationsHeldAt(level)}
}

// levelOnCard is what a principal may do to a card, by the same rule
// requireOnCard refuses with: viewing needs view on any board the card sits
// on, and anything more needs it on every one of them.
func levelOnCard(access *PrincipalBoardAccess, boardIDsOfCard []string) BoardAccessLevel {
	seen := false
	lowest := BoardAccessAdminister
	for _, boardID := range boardIDsOfCard {
		level := access.LevelOn(boardID)
		if level >= BoardAccessView {
			seen = true
		}
		if level < lowest {
			lowest = level
		}
	}
	if !seen {
		return BoardAccessNone
	}
	if lowest < BoardAccessView {
		return BoardAccessView
	}
	return lowest
}

type principalBoardAccessContextKey struct{}

type serviceCallerContextKey struct{}

// calledWithServiceToken reports whether the request carried the service
// token. An administrator is unrestricted too, but is a person: a route only
// an internal service may use asks this, not whether access is nil.
func calledWithServiceToken(r *http.Request) bool {
	service, _ := r.Context().Value(serviceCallerContextKey{}).(bool)
	return service
}

// principalBoardAccessFrom returns the request's access, nil when unrestricted.
func principalBoardAccessFrom(r *http.Request) *PrincipalBoardAccess {
	access, _ := r.Context().Value(principalBoardAccessContextKey{}).(*PrincipalBoardAccess)
	return access
}

// principalGate wraps the router when enforcement is on.
func (a *API) principalGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enforcement := a.principalEnforcement
		if enforcement == nil {
			// Nothing may answer without a configured token: a nil enforcement
			// here means the binary was wired wrong, and serving would serve
			// every board to anyone.
			writeError(w, http.StatusInternalServerError, "kanban-store has no service token configured, so no request can be authorized")
			return
		}
		if r.Method == http.MethodOptions || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		if token := r.Header.Get(ServiceTokenHeader); token != "" {
			if subtle.ConstantTimeCompare([]byte(token), []byte(enforcement.ServiceToken)) != 1 {
				writeError(w, http.StatusUnauthorized, ServiceTokenHeader+" does not match this store's service token")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), serviceCallerContextKey{}, true)))
			return
		}
		principalID := r.Header.Get(PrincipalIDHeader)
		if principalID == "" {
			writeError(w, http.StatusUnauthorized, "principal enforcement is on: send "+PrincipalIDHeader+" (set by the gateway from a login) or "+ServiceTokenHeader)
			return
		}
		access, administrator, refusal := a.boardAccessOfPrincipal(principalID)
		if refusal != nil {
			writeError(w, refusal.status, refusal.message)
			return
		}
		if administrator {
			next.ServeHTTP(w, r)
			return
		}
		refusal, err := a.authorizeRequest(r, access)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if refusal != nil {
			writeError(w, refusal.status, refusal.message)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalBoardAccessContextKey{}, access)))
	})
}

// boardAccessOfPrincipal reads what a principal may do on every board: from
// principal-store whether it exists, is enabled and is an administrator, and
// from grant-store its board grants. The gate asks it for the caller; a route
// that acts for someone other than its caller asks it for them. An unknown or
// disabled principal is refused 401, and an owner that cannot answer 502: a
// guess in either direction would be wrong.
func (a *API) boardAccessOfPrincipal(principalID string) (access *PrincipalBoardAccess, administrator bool, refusal *accessRefusal) {
	if !principalIDShape.MatchString(principalID) {
		return nil, false, &accessRefusal{http.StatusUnauthorized, fmt.Sprintf("%s %q is not a principal-store id (principal_000001)", PrincipalIDHeader, boundedtext.Value(principalID))}
	}
	// An administrator is past every rule; principal-store owns that fact.
	principal, err := a.principals.Get(principalID)
	if errors.Is(err, principalstore.ErrNotFound) {
		return nil, false, &accessRefusal{http.StatusUnauthorized, fmt.Sprintf("%s %s does not exist in principal-store", PrincipalIDHeader, boundedtext.Value(principalID))}
	}
	if err != nil {
		return nil, false, &accessRefusal{http.StatusBadGateway, "could not read the principal, so access is unknown and nothing is served: " + err.Error()}
	}
	if principal.Disabled() {
		return nil, false, &accessRefusal{http.StatusUnauthorized, fmt.Sprintf("%s %s is disabled in principal-store", PrincipalIDHeader, boundedtext.Value(principalID))}
	}
	if principal.IsAdministrator {
		return nil, true, nil
	}
	grants, err := a.principalEnforcement.Grants.EffectiveBoardGrants(principalID)
	if errors.Is(err, grantstore.ErrPrincipalNotFound) {
		return nil, false, &accessRefusal{http.StatusUnauthorized, err.Error()}
	}
	if err != nil {
		return nil, false, &accessRefusal{http.StatusBadGateway, "could not read board grants, so access is unknown and nothing is served: " + err.Error()}
	}
	access = &PrincipalBoardAccess{PrincipalID: principalID, levels: map[string]BoardAccessLevel{}}
	for _, grant := range grants {
		level, known := boardAccessLevelOfRelation[grant.Relation]
		if !known {
			// grant-store only returns relations it serves for boards; one this
			// store does not know grants nothing here rather than guessing.
			log.Printf("principal access: %s holds %q on board %s, which kanban-store does not enforce; ignored", principalID, grant.Relation, grant.BoardID)
			continue
		}
		if level > access.levels[grant.BoardID] {
			access.levels[grant.BoardID] = level
		}
	}
	return access, false, nil
}

type accessRefusal struct {
	status  int
	message string
}

// refuseUnseen is the answer for a board or card the principal cannot view: the
// same 404 as one that does not exist, so ids cannot be probed.
var refuseUnseen = &accessRefusal{status: http.StatusNotFound, message: "not found"}

func refuseBelow(needed BoardAccessLevel, what string) *accessRefusal {
	names := map[BoardAccessLevel]string{
		BoardAccessView: grantstore.RelationCanView, BoardAccessEdit: grantstore.RelationCanEdit, BoardAccessAdminister: grantstore.RelationCanAdminister,
	}
	return &accessRefusal{status: http.StatusForbidden, message: fmt.Sprintf("%s needs %s", what, names[needed])}
}

// requireOnBoard checks one board: unseen is 404, seen but short is 403.
func requireOnBoard(access *PrincipalBoardAccess, boardID string, needed BoardAccessLevel, what string) *accessRefusal {
	level := access.LevelOn(boardID)
	if level < BoardAccessView {
		return refuseUnseen
	}
	if level < needed {
		return refuseBelow(needed, what)
	}
	return nil
}

// requireOnCard checks a card by the boards it sits on: viewing needs view on
// any of them, anything more needs that level on all of them. A card on no board
// is not visible to a principal at all.
func (a *API) requireOnCard(access *PrincipalBoardAccess, cardID string, needed BoardAccessLevel, what string) (*accessRefusal, error) {
	boardIDs, err := a.boardIDsOfCard(cardID)
	if err != nil {
		return nil, err
	}
	level := levelOnCard(access, boardIDs)
	if level < BoardAccessView {
		return refuseUnseen, nil
	}
	if level < needed {
		return refuseBelow(needed, what+" (on every board the card sits on)"), nil
	}
	return nil, nil
}

// authorizeRequest is the route table. It returns a refusal, or nil to let the
// handler run, in which case list handlers filter by the access in the context.
func (a *API) authorizeRequest(r *http.Request, access *PrincipalBoardAccess) (*accessRefusal, error) {
	path := r.URL.Path
	reading := r.Method == http.MethodGet
	segments := func(prefix string) []string {
		return strings.Split(strings.TrimPrefix(path, prefix), "/")
	}
	switch {
	case path == SettingsPath:
		// Addresses, paths and which secrets are set: the operator's, so the
		// service token or an administrator, both of whom are past this table.
		return &accessRefusal{status: http.StatusForbidden, message: "settings are read with " + ServiceTokenHeader + " or by an administrator"}, nil

	case path == "/api/entity-types", path == "/api/message-trigger-options",
		path == "/api/ticket-channels", path == "/api/ticket-lifecycle-states", path == "/api/note-visibilities",
		path == "/api/assignment-strategies", path == "/api/work-states":
		// Vocabularies: what a channel or a lifecycle state may be. They name
		// no board and hold no card content.
		return nil, nil

	case path == "/api/boards":
		// GET is filtered by the handler; POST makes the creator its administrator.
		return nil, nil

	case strings.HasPrefix(path, "/api/boards/"):
		parts := segments("/api/boards/")
		boardID := parts[0]
		needed := BoardAccessAdminister
		switch {
		case len(parts) == 1:
			if reading {
				needed = BoardAccessView
			}
		case parts[1] == "cards":
			needed = BoardAccessEdit
			if reading {
				needed = BoardAccessView
			}
			if len(parts) >= 3 && r.Method == http.MethodPut {
				// Attaching an existing item: it must already be a card this
				// principal can see, or any noteboard item could be pulled onto a
				// board and read through it.
				if refusal := requireOnBoard(access, boardID, BoardAccessEdit, "attaching a card to a board"); refusal != nil {
					return refusal, nil
				}
				return a.requireOnCard(access, parts[2], BoardAccessView, "attaching a card")
			}
		case parts[1] == "card-page", parts[1] == "columns", parts[1] == "priority-levels", parts[1] == "tag-rules", parts[1] == "effective-defaults":
			if reading {
				needed = BoardAccessView
			}
		case parts[1] == "access":
			// What the caller itself may do here. It names nobody else's grants.
			if reading {
				needed = BoardAccessView
			}
		case parts[1] == "message-triggers", parts[1] == "message-deliveries":
			// Triggers name who gets texted and what they are sent.
			needed = BoardAccessAdminister
		case parts[1] == "classification":
			// Reading decisions and labels: can_view. A review sets a card's
			// labels: can_edit. The taxonomy and policy: can_administer.
			// Publishing a decision is the classifier's, with the service token,
			// so no principal may do it however much it holds.
			switch {
			case reading:
				needed = BoardAccessView
			case r.Method == http.MethodPost && len(parts) == 5 && parts[2] == "decisions" && parts[4] == "reviews":
				needed = BoardAccessEdit
			case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "decisions":
				if access.LevelOn(boardID) < BoardAccessView {
					return refuseUnseen, nil
				}
				return &accessRefusal{status: http.StatusForbidden, message: "decisions are published by the classifier with " + ServiceTokenHeader}, nil
			}
		default:
			return refuseUnseen, nil
		}
		return requireOnBoard(access, boardID, needed, r.Method+" "+path), nil

	case strings.HasPrefix(path, "/api/columns/"):
		parts := segments("/api/columns/")
		column, err := a.store.GetColumn(parts[0])
		if errors.Is(err, db.ErrNotFound) {
			return refuseUnseen, nil
		}
		if err != nil {
			return nil, err
		}
		needed := BoardAccessAdminister
		if reading {
			needed = BoardAccessView
		}
		return requireOnBoard(access, column.BoardID, needed, r.Method+" "+path), nil

	case strings.HasPrefix(path, "/api/cards/"):
		parts := segments("/api/cards/")
		cardID := parts[0]
		if boardID := r.URL.Query().Get("board_id"); boardID != "" {
			if refusal := requireOnBoard(access, boardID, BoardAccessView, "board_id"); refusal != nil {
				return refusal, nil
			}
		}
		needed := BoardAccessEdit
		if reading {
			needed = BoardAccessView
		}
		return a.requireOnCard(access, cardID, needed, r.Method+" "+path)

	case strings.HasPrefix(path, "/api/links/"):
		return a.requireOnCardOf(access, a.store.CardIDOfLink, segments("/api/links/")[0], "deleting a link")

	case strings.HasPrefix(path, "/api/notes/"):
		return a.requireOnCardOf(access, a.store.CardIDOfNote, segments("/api/notes/")[0], "deleting a note")

	case strings.HasPrefix(path, "/api/message-triggers/"):
		trigger, err := a.store.GetMessageTrigger(segments("/api/message-triggers/")[0])
		if errors.Is(err, db.ErrNotFound) {
			return refuseUnseen, nil
		}
		if err != nil {
			return nil, err
		}
		return requireOnBoard(access, trigger.BoardID, BoardAccessAdminister, "a message trigger"), nil

	case strings.HasPrefix(path, "/api/entities/"):
		parts := segments("/api/entities/")
		if len(parts) == 3 && parts[2] == "cards" && reading {
			return nil, nil // filtered by the handler
		}
		// Entity tags are one tag space shared by every board and every kind of
		// entity; nothing yet says which principal may read or write them.
		return &accessRefusal{status: http.StatusForbidden, message: "entity tags are not available under principal enforcement"}, nil

	case path == "/api/tags":
		return &accessRefusal{status: http.StatusForbidden, message: "the tag listing spans every board and is not available under principal enforcement"}, nil

	case path == "/api/assignments", path == "/api/search", path == "/api/tickets":
		return nil, nil // filtered by the handler

	case path == "/api/bulk-card-commands":
		// Names no card of its own. Each card in the request is judged by the
		// single-card rule above, as this caller, before anything is done to it.
		return nil, nil
	}
	return &accessRefusal{status: http.StatusForbidden, message: "no access rule for " + r.Method + " " + path + " under principal enforcement"}, nil
}

func (a *API) requireOnCardOf(access *PrincipalBoardAccess, cardIDOf func(string) (string, error), id, what string) (*accessRefusal, error) {
	cardID, err := cardIDOf(id)
	if errors.Is(err, db.ErrNotFound) {
		return refuseUnseen, nil
	}
	if err != nil {
		return nil, err
	}
	return a.requireOnCard(access, cardID, BoardAccessEdit, what)
}

// cardVisibleTo reports whether an unrestricted request, or a principal that can
// view at least one of the card's boards, may see the card. List handlers use it.
func (a *API) cardVisibleTo(access *PrincipalBoardAccess, cardID string) (bool, error) {
	if access == nil {
		return true, nil
	}
	boardIDs, err := a.boardIDsOfCard(cardID)
	if err != nil {
		return false, err
	}
	for _, boardID := range boardIDs {
		if access.LevelOn(boardID) >= BoardAccessView {
			return true, nil
		}
	}
	return false, nil
}
