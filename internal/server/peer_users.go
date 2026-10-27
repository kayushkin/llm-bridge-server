package server

// Phase 2 of linked servers (design note d107154a): a connected server says
// which of its users a request is for, and this server acts for that user as
// one of its own principals.
//
// On the calling side, the connection proxy sends the caller's principal id in
// peerUserHeader beside the peer token (peer_connection_proxy.go). On this
// side, an administrator maps (peer principal, remote principal id) to a local
// principal (POST /peer-user-mappings). A request naming a mapped user then:
//
//   - acts as the mapped principal, an administrator only when the peer is
//     one too;
//   - is allowed only what both principals' grants allow (grantSubject,
//     principal_grants.go);
//   - reaches only the sessions that principal started through this peer
//     (sessionScope), plus what both may observe;
//   - is refused routes whose answer this server cannot intersect: the store
//     proxies, operations and login sessions.
//
// An unmapped user is 403 peer_user_not_mapped. A request naming no user acts
// as the peer alone, as before phase 2.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// observeSessionsRelation is grant-store's relation for reading sessions one
// did not start, on an instance or a machine. Holding none allows nothing,
// whatever principal-store says of the kind.
const observeSessionsRelation = "can_observe_sessions_on"

// sessionScope is which sessions a restricted caller reaches: those started
// as principalID, and, for a connected server's user, only those started
// through that server's peer principal.
type sessionScope struct {
	principalID            string
	throughPeerPrincipalID string
}

// sessionScopeRestrictingRequest is principalRestrictingRequest for the
// session routes: the scope a request is narrowed to, and false when it is
// not narrowed — the internal service, an administrator, or a caller whose
// grants let it observe every session on a read-only route.
func sessionScopeRestrictingRequest(r *http.Request) (sessionScope, bool) {
	caller, ok := callerOfRequest(r)
	if !ok || !caller.narrowsToOnePrincipal() || caller.observesEverySession {
		return sessionScope{}, false
	}
	scope := sessionScope{principalID: caller.principalID}
	if caller.peer.actsForRemoteUser() {
		scope.throughPeerPrincipalID = caller.peer.peerPrincipalID
	}
	return scope, true
}

// sessionScopeFilterOfRequest is the two store filter fields a restricted
// request sets, both empty when it is not narrowed.
func sessionScopeFilterOfRequest(r *http.Request) (ownedByPrincipalID, ownedThroughPeerPrincipalID string) {
	scope, restricted := sessionScopeRestrictingRequest(r)
	if !restricted {
		return "", ""
	}
	return scope.principalID, scope.throughPeerPrincipalID
}

// sessionIsInScope reports whether a session is one the scope reaches.
func (s *Server) sessionIsInScope(session *store.Session, scope sessionScope) bool {
	if session == nil || scope.principalID == "" || session.PrincipalID != scope.principalID {
		return false
	}
	if scope.throughPeerPrincipalID == "" {
		return true
	}
	origin, found, err := s.store.SessionPeerOriginOfLineage(session.SessionID)
	return err == nil && found && origin.PeerPrincipalID == scope.throughPeerPrincipalID
}

// sessionIDIsInScope is sessionIsInScope for a session id.
func (s *Server) sessionIDIsInScope(sessionID string, scope sessionScope) bool {
	session, err := s.store.GetSession(sessionID)
	return err == nil && s.sessionIsInScope(session, scope)
}

// routesNeverObserved are read-only by method and still not observation: the
// terminal attach drives the session's pty.
var routesNeverObserved = []string{"GET /sessions/{id}/attach", "GET /sessions/{id}/attach-token"}

// requestOnlyReads reports whether a request can be answered to an observer:
// GET or HEAD on a route that does not drive the session.
func requestOnlyReads(r *http.Request, pattern string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	for _, never := range routesNeverObserved {
		if pattern == never {
			return false
		}
	}
	return true
}

// mayObserveSession reports whether the caller's grants let it read this
// session it does not own: can_observe_sessions_on naming the session's
// instance or its machine, for the caller's principal and, through a peer,
// for the peer too.
func (s *Server) mayObserveSession(r *http.Request, caller requestCaller, sessionID string) (bool, error) {
	session, err := s.store.GetSession(sessionID)
	if err != nil {
		return false, nil
	}
	machineID := ""
	if session.InstanceID != "" {
		if instance, err := s.harnessStore.GetInstanceWithMachine(session.InstanceID); err == nil {
			machineID = instance.MachineID
		}
	}
	subject := grantSubjectOfCaller(caller)
	instances, err := s.grantedResourcesOf(r.Context(), subject, observeSessionsRelation, "instance")
	if err != nil {
		return false, err
	}
	if session.InstanceID != "" && instances.requiresAGrantAndNames(session.InstanceID) {
		return true, nil
	}
	if machineID == "" {
		return false, nil
	}
	machines, err := s.grantedResourcesOf(r.Context(), subject, observeSessionsRelation, "machine")
	if err != nil {
		return false, err
	}
	return machines.requiresAGrantAndNames(machineID), nil
}

// mayObserveEverySession reports whether the caller's grants name every
// instance for observation, which is what lets a list answer every session.
func (s *Server) mayObserveEverySession(r *http.Request, caller requestCaller) (bool, error) {
	granted, err := s.grantedResourcesOf(r.Context(), grantSubjectOfCaller(caller), observeSessionsRelation, "instance")
	if err != nil {
		return false, err
	}
	return granted.requiresAGrantAndNames("*"), nil
}

// routeClassesRefusedToPeerUsers are the classes whose answer comes from a
// store or record this server cannot narrow to the intersection of two
// principals, so a connected server's user is refused them: the store
// proxies act as the principal in another store, and operations and login
// sessions are a principal's own with no peer origin.
var routeClassesRefusedToPeerUsers = []routeAccessClass{
	routePrincipalOrSessionAgentThroughStoreProxy,
	routePrincipalCreatesOperation,
	routePrincipalOwnsOperation,
	routePrincipalSeesOwnOperationsOnly,
	routePrincipalManagesOwnLoginSessions,
}

func routeClassIsRefusedToPeerUsers(class routeAccessClass) bool {
	for _, refused := range routeClassesRefusedToPeerUsers {
		if refused == class {
			return true
		}
	}
	return false
}

// handleListPeerUserMappings: GET /peer-user-mappings[?peer_principal_id=].
func (s *Server) handleListPeerUserMappings(w http.ResponseWriter, r *http.Request) {
	mappings, err := s.store.ListPeerUserMappings(strings.TrimSpace(r.URL.Query().Get("peer_principal_id")))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, mappings)
}

// handleCreatePeerUserMapping: POST /peer-user-mappings. The peer principal
// must be one this server made for a connected server, and the local
// principal one that acts as a caller here; both are checked with
// principal-store, the remote id only stored.
func (s *Server) handleCreatePeerUserMapping(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingPeerChange(w, r)
	if !ok {
		return
	}
	var request msg.PeerUserMappingRequest
	if !decodeAdministrationBody(w, r, &request, "created_by") {
		return
	}
	request.PeerPrincipalID = strings.TrimSpace(request.PeerPrincipalID)
	request.RemotePrincipalID = strings.TrimSpace(request.RemotePrincipalID)
	request.LocalPrincipalID = strings.TrimSpace(request.LocalPrincipalID)
	if request.PeerPrincipalID == "" || request.RemotePrincipalID == "" || request.LocalPrincipalID == "" {
		writeJSONError(w, http.StatusBadRequest, "mapping_incomplete", "peer_principal_id, remote_principal_id and local_principal_id are all required")
		return
	}
	if !s.peerPrincipalIsOneOfOurs(request.PeerPrincipalID) {
		writeJSONError(w, http.StatusBadRequest, "unknown_peer_principal", fmt.Sprintf(
			"%s is not a peer principal this server made for a connected server; GET /peer-connections names them", request.PeerPrincipalID))
		return
	}
	local, err := s.principalLookupCache.lookUp(r.Context(), request.LocalPrincipalID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "unknown_local_principal", fmt.Sprintf("principal-store does not know %s: %v", request.LocalPrincipalID, err))
		return
	}
	if !local.ActsAsCaller || local.Kind == "peer" {
		writeJSONError(w, http.StatusBadRequest, "local_principal_cannot_act", fmt.Sprintf(
			"%s is a %q principal; a connected server's user is mapped to a principal that acts as a caller and is not itself a peer", request.LocalPrincipalID, local.Kind))
		return
	}
	mapping := msg.PeerUserMapping{PeerPrincipalID: request.PeerPrincipalID, RemotePrincipalID: request.RemotePrincipalID,
		LocalPrincipalID: request.LocalPrincipalID, CreatedBy: administratorID}
	if err := s.store.InsertPeerUserMapping(&mapping); errors.Is(err, store.ErrPeerUserMappingExists) {
		writeJSONError(w, http.StatusConflict, "peer_user_mapping_exists", fmt.Sprintf(
			"%s's user %s is already mapped; remove that mapping first", request.PeerPrincipalID, request.RemotePrincipalID))
		return
	} else if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSONStatus(w, http.StatusCreated, mapping)
}

// handleRemovePeerUserMapping: POST /peer-user-mappings/{id}/remove {"reason"}.
func (s *Server) handleRemovePeerUserMapping(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingPeerChange(w, r)
	if !ok {
		return
	}
	var request msg.PeerReasonRequest
	if !decodeAdministrationBody(w, r, &request, "removed_by") {
		return
	}
	if strings.TrimSpace(request.Reason) == "" {
		writeJSONError(w, http.StatusBadRequest, "reason_required", "say why; the reason stays on the row")
		return
	}
	removed, err := s.store.RemovePeerUserMapping(r.PathValue("id"), administratorID, request.Reason)
	if errors.Is(err, store.ErrPeerUserMappingNotFound) {
		writeJSONError(w, http.StatusNotFound, "peer_user_mapping_not_found", fmt.Sprintf("%s is not a live mapping", r.PathValue("id")))
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, removed)
}

// peerPrincipalIsOneOfOurs reports whether a connection here names this peer
// principal as the one it made for the other server.
func (s *Server) peerPrincipalIsOneOfOurs(peerPrincipalID string) bool {
	connections, err := s.store.ListPeerConnections()
	if err != nil {
		return false
	}
	for _, connection := range connections {
		if connection.PeerPrincipalID == peerPrincipalID {
			return true
		}
	}
	return false
}
