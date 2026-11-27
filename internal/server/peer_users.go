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
//   - is allowed only what both principals' grants allow (GrantSubject,
//     principal_grants.go);
//   - reaches only the sessions that principal started through this peer
//     (SessionScope), plus what both may observe;
//   - is refused routes whose answer this server cannot intersect: the store
//     proxies, operations and login sessions.
//
// An unmapped user is 403 peer_user_not_mapped. A request naming no user acts
// as the peer alone, as before phase 2.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/access/grantcheck"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// observeSessionsRelation is grant-store's relation for reading sessions one
// did not start, on an instance or a machine. Holding none allows nothing,
// whatever principal-store says of the kind.
const observeSessionsRelation = "can_observe_sessions_on"

// callThroughRelation is grant-store's relation for listing a peer connection
// and calling the connected server through it.
const callThroughRelation = "can_call_through"

// sessionIsInScope reports whether a session is one the scope reaches.
func (s *Server) sessionIsInScope(session *store.Session, scope requestcaller.SessionScope) bool {
	if session == nil || scope.PrincipalID == "" || session.PrincipalID != scope.PrincipalID {
		return false
	}
	if scope.ThroughPeerPrincipalID == "" {
		return true
	}
	origin, found, err := s.store.SessionPeerOriginOfLineage(session.SessionID)
	return err == nil && found && origin.PeerPrincipalID == scope.ThroughPeerPrincipalID
}

// sessionIDIsInScope is sessionIsInScope for a session id.
func (s *Server) sessionIDIsInScope(sessionID string, scope requestcaller.SessionScope) bool {
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

// sessionReachedByCaller reports whether a caller who is not an
// administrator reaches one session on a session route: the session is in
// its scope, it started the session as another principal and may still drive
// it, or, on a route that only reads, its grants let it observe the session.
// A session it does not reach answers as a missing one.
func (s *Server) sessionReachedByCaller(ctx context.Context, caller requestcaller.Caller, sessionID string, onlyReads bool) (bool, error) {
	if s.sessionIDIsInScope(sessionID, requestcaller.SessionScopeOfCaller(caller)) {
		return true, nil
	}
	// A service that started this session as another principal drives it
	// while it holds can_start_sessions_as on that one.
	drives, err := s.callerStartedSessionIDAndMayDriveIt(ctx, caller, sessionID)
	if err != nil || drives {
		return drives, err
	}
	if !onlyReads {
		return false, nil
	}
	return s.mayObserveSession(ctx, caller, sessionID)
}

// mayObserveSession reports whether the caller's grants let it read this
// session it does not own: can_observe_sessions_on naming the session's
// instance or its machine, for the caller's principal and, through a peer,
// for the peer too.
func (s *Server) mayObserveSession(ctx context.Context, caller requestcaller.Caller, sessionID string) (bool, error) {
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
	subject := grantcheck.SubjectOfCaller(caller)
	instances, err := s.grantChecker.GrantedResourcesOf(ctx, subject, observeSessionsRelation, "instance")
	if err != nil {
		return false, err
	}
	if session.InstanceID != "" && instances.RequiresAGrantAndNames(session.InstanceID) {
		return true, nil
	}
	if machineID == "" {
		return false, nil
	}
	machines, err := s.grantChecker.GrantedResourcesOf(ctx, subject, observeSessionsRelation, "machine")
	if err != nil {
		return false, err
	}
	return machines.RequiresAGrantAndNames(machineID), nil
}

// mayObserveEverySession reports whether the caller's grants name every
// instance for observation, which is what lets a list answer every session.
func (s *Server) mayObserveEverySession(r *http.Request, caller requestcaller.Caller) (bool, error) {
	granted, err := s.grantChecker.GrantedResourcesOf(r.Context(), grantcheck.SubjectOfCaller(caller), observeSessionsRelation, "instance")
	if err != nil {
		return false, err
	}
	return granted.RequiresAGrantAndNames("*"), nil
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
		httpjson.WriteJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	httpjson.WriteJSON(w, mappings)
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
		httpjson.WriteJSONError(w, http.StatusBadRequest, "mapping_incomplete", "peer_principal_id, remote_principal_id and local_principal_id are all required")
		return
	}
	if !s.peerPrincipalIsOneOfOurs(request.PeerPrincipalID) {
		httpjson.WriteJSONError(w, http.StatusBadRequest, "unknown_peer_principal", fmt.Sprintf(
			"%s is not a peer principal this server made for a connected server; GET /peer-connections names them", request.PeerPrincipalID))
		return
	}
	local, err := s.principalLookupCache.LookUp(r.Context(), request.LocalPrincipalID)
	if err != nil {
		httpjson.WriteJSONError(w, http.StatusBadRequest, "unknown_local_principal", fmt.Sprintf("principal-store does not know %s: %v", request.LocalPrincipalID, err))
		return
	}
	if !local.ActsAsCaller || local.Kind == "peer" {
		httpjson.WriteJSONError(w, http.StatusBadRequest, "local_principal_cannot_act", fmt.Sprintf(
			"%s is a %q principal; a connected server's user is mapped to a principal that acts as a caller and is not itself a peer", request.LocalPrincipalID, local.Kind))
		return
	}
	mapping := msg.PeerUserMapping{PeerPrincipalID: request.PeerPrincipalID, RemotePrincipalID: request.RemotePrincipalID,
		LocalPrincipalID: request.LocalPrincipalID, CreatedBy: administratorID}
	if err := s.store.InsertPeerUserMapping(&mapping); errors.Is(err, store.ErrPeerUserMappingExists) {
		httpjson.WriteJSONError(w, http.StatusConflict, "peer_user_mapping_exists", fmt.Sprintf(
			"%s's user %s is already mapped; remove that mapping first", request.PeerPrincipalID, request.RemotePrincipalID))
		return
	} else if err != nil {
		httpjson.WriteJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
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
		httpjson.WriteJSONError(w, http.StatusBadRequest, "reason_required", "say why; the reason stays on the row")
		return
	}
	removed, err := s.store.RemovePeerUserMapping(r.PathValue("id"), administratorID, request.Reason)
	if errors.Is(err, store.ErrPeerUserMappingNotFound) {
		httpjson.WriteJSONError(w, http.StatusNotFound, "peer_user_mapping_not_found", fmt.Sprintf("%s is not a live mapping", r.PathValue("id")))
		return
	}
	if err != nil {
		httpjson.WriteJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	if ended := s.peerRequestsInFlight.cancelMapping(removed.ID); ended > 0 {
		log.Printf("[peers] user mapping %s was removed; ended %d request(s) still acting through it", removed.ID, ended)
	}
	httpjson.WriteJSON(w, removed)
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
