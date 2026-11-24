package grantstore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
)

// Who may read and change grants.
//
// The caller comes from the request's context, put there by the server this
// store is mounted in once it has verified the request's credential
// (ContextWithCaller). Nothing a client sends — no header, no body field —
// names the caller. Every route except /health, /relations and
// /resource-types needs one:
//
//   - InternalService: the server's own internal callers (reading a session's
//     grants at spawn, kanban-store's board checks through the gateway, an
//     operator's script with the service token). Unrestricted.
//   - PrincipalID: a principal the server verified. It must be one
//     principal-store knows, that acts as a caller (a human, a peer or a
//     service) and is not disabled.
//
// Neither is 401. An **administrator** — principal-store's is_administrator —
// is unrestricted, like the internal service: administering the deployment
// includes handing out and taking back access to it. Any other principal may:
//
//   - create or revoke a board grant only on a board it holds can_administer on,
//     directly or through a group. Every other resource type — tools, skills,
//     agents, instances, machines — is the operator's.
//   - read a grant held by itself, or any grant on a board it administers.
//   - list grants only with principal_id set to itself, or with
//     resource_type=board and resource_id set to a board it administers.
//   - read GET /principals/{id}/effective only for its own id.
//
// A grant a principal may not read is 404, the same as one that does not exist.
// A board administrator can grant can_administer to someone else, and that is
// deliberate: handing a board over is part of administering it.

// Caller is who a request to this store acts as, as the server it is mounted
// in verified it. Exactly one of the two is set.
type Caller struct {
	// InternalService is the server's own unrestricted caller.
	InternalService bool
	// PrincipalID is the verified principal the request acts as.
	PrincipalID string
}

type callerContextKey struct{}

// ContextWithCaller returns ctx carrying caller, for the handlers to read.
// Only the server this store is mounted in calls it, after verifying the
// request's credential.
func ContextWithCaller(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, callerContextKey{}, caller)
}

var principalIDShape = regexp.MustCompile(`^principal_\d{6,}$`)

// grantCaller is who a request acts as. unrestricted is the internal service
// or an administrator.
type grantCaller struct {
	unrestricted bool
	principalID  string
}

// identifyCaller answers the request itself (401 or 502) and returns false
// when the caller cannot be established.
func (h *Handlers) identifyCaller(w http.ResponseWriter, r *http.Request) (grantCaller, bool) {
	caller, carried := r.Context().Value(callerContextKey{}).(Caller)
	if !carried || (!caller.InternalService && caller.PrincipalID == "") {
		writeErr(w, http.StatusUnauthorized, "this request carries no verified caller: grant-store is reached only through the server it is mounted in, as a principal or as its internal service")
		return grantCaller{}, false
	}
	if caller.InternalService {
		if caller.PrincipalID != "" {
			writeErr(w, http.StatusInternalServerError, "the request names both the internal service and principal "+caller.PrincipalID+" as its caller; exactly one is allowed")
			return grantCaller{}, false
		}
		return grantCaller{unrestricted: true}, true
	}
	principalID := caller.PrincipalID
	if !principalIDShape.MatchString(principalID) {
		writeErr(w, http.StatusUnauthorized, fmt.Sprintf("the caller %q is not a principal-store id (principal_000001)", principalID))
		return grantCaller{}, false
	}
	summary, err := h.directory.LookupPrincipal(r.Context(), principalID)
	if errors.Is(err, ErrPrincipalNotFound) {
		writeErr(w, http.StatusUnauthorized, fmt.Sprintf("the caller %s does not exist in principal-store", principalID))
		return grantCaller{}, false
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("could not read %s from principal-store, so the caller is unknown: %v", principalID, err))
		return grantCaller{}, false
	}
	if !summary.ActsAsCaller || summary.DisabledAt != 0 {
		writeErr(w, http.StatusUnauthorized, fmt.Sprintf("the caller %s is a %s principal that is disabled or does not act as a caller, by principal-store's answer; only an active human, peer or service acts", principalID, summary.Kind))
		return grantCaller{}, false
	}
	// An administrator is past every rule below, and is still named in the
	// returned caller so a log or a future audit row says who acted.
	return grantCaller{unrestricted: summary.IsAdministrator, principalID: principalID}, true
}

// callerAdministersBoard reads the caller's effective can_administer grants,
// which include those held through a group.
func (h *Handlers) callerAdministersBoard(r *http.Request, caller grantCaller, boardID string) (bool, error) {
	if caller.unrestricted {
		return true, nil
	}
	grants, err := h.store.Effective(r.Context(), h.directory, caller.principalID, RelationCanAdminister, ResourceTypeBoard)
	if err != nil {
		return false, err
	}
	for _, grant := range grants {
		if grant.ResourceID == boardID {
			return true, nil
		}
	}
	return false, nil
}

// callerMayReadGrant: its own grant, or a grant on a board it administers.
func (h *Handlers) callerMayReadGrant(r *http.Request, caller grantCaller, grant *Grant) (bool, error) {
	if caller.unrestricted || grant.PrincipalID == caller.principalID {
		return true, nil
	}
	if grant.ResourceType != ResourceTypeBoard {
		return false, nil
	}
	return h.callerAdministersBoard(r, caller, grant.ResourceID)
}
