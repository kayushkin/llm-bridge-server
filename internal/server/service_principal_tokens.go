package server

// Service principal tokens: how a program on this host calls this server as a
// principal of its own (principal-store's kind service), held to the route
// rules and to its grants like a person signed in as it, instead of with the
// unrestricted service token.
//
// An administrator issues one with POST /service-principal-tokens for a
// principal whose kind principal-store says holds_service_principal_tokens.
// The token is answered once and kept only as its SHA-256. The program sends it
// as "Authorization: Bearer llm-bridge-service-principal-token-v1.…" on any
// route; principalOfRequest resolves it to its principal, and from there it is
// a principal like any other. It is never accepted through the public reverse
// proxy: it is for this host's own programs.
//
// A service may also start sessions that run as another principal, when it
// holds can_start_sessions_as on that principal (handleCreateSession). The
// session's starter is recorded in session_starters, and the starter may drive
// it while the grant stands (callerStartedSessionAndMayDriveIt). PUT
// /sessions/{id}/starter records one for a session created before starters
// were recorded.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// servicePrincipalTokenPrefix starts every service principal token, so a
// bearer is told apart from a session agent token and a posting token before
// anything is looked up.
const servicePrincipalTokenPrefix = "llm-bridge-service-principal-token-v1"

// startSessionsAsRelation is grant-store's relation for starting sessions that
// run as another principal. Holding none allows nothing, whatever the kind.
const startSessionsAsRelation = "can_start_sessions_as"

func isServicePrincipalToken(token string) bool {
	return strings.HasPrefix(token, servicePrincipalTokenPrefix+".")
}

func hashOfServicePrincipalToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func mintServicePrincipalToken() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("read random bytes for a service principal token: %w", err)
	}
	return servicePrincipalTokenPrefix + "." + base64.RawURLEncoding.EncodeToString(random), nil
}

// principalOfServicePrincipalToken verifies a presented token and returns the
// principal it acts as.
func (s *Server) principalOfServicePrincipalToken(ctx context.Context, r *http.Request, token string) (string, *requestCredentialError) {
	if requestCameThroughPublicProxy(r) {
		return "", &requestCredentialError{http.StatusForbidden, "service_principal_token_not_public",
			"a service principal token is accepted only from this host, never through the public proxy"}
	}
	record, err := s.store.ServicePrincipalTokenByHash(hashOfServicePrincipalToken(token))
	if errors.Is(err, store.ErrServicePrincipalTokenNotFound) {
		return "", &requestCredentialError{http.StatusUnauthorized, "invalid_service_principal_token",
			"no service principal token matches the one presented"}
	}
	if err != nil {
		return "", &requestCredentialError{http.StatusInternalServerError, "store_failed", err.Error()}
	}
	if record.RevokedAt != nil {
		return "", &requestCredentialError{http.StatusUnauthorized, "service_principal_token_revoked", fmt.Sprintf(
			"service principal token %s was revoked at %s by %s: %s", record.ID, record.RevokedAt.Format(time.RFC3339), record.RevokedBy, record.RevokeReason)}
	}
	principal, credentialError := s.principalThatHoldsServicePrincipalTokens(ctx, record.PrincipalID)
	if credentialError != nil {
		return "", credentialError
	}
	if err := s.store.TouchServicePrincipalToken(record.ID, time.Now()); err != nil {
		log.Printf("[service-principal-token] ERROR record use of %s: %v", record.ID, err)
	}
	return principal.ID, nil
}

// principalThatHoldsServicePrincipalTokens reads a principal and refuses one
// whose kind principal-store says may not hold a service principal token.
func (s *Server) principalThatHoldsServicePrincipalTokens(ctx context.Context, principalID string) (principalclient.Principal, *requestCredentialError) {
	principal, err := s.principalLookupCache.lookUp(ctx, principalID)
	if errors.Is(err, principalclient.ErrNotFound) {
		return principalclient.Principal{}, &requestCredentialError{http.StatusBadRequest, "unknown_principal", fmt.Sprintf(
			"principal %s is not in principal-store", principalID)}
	}
	if err != nil {
		return principalclient.Principal{}, &requestCredentialError{http.StatusBadGateway, "principal_store_unavailable", err.Error()}
	}
	if !principal.HoldsServicePrincipalTokens {
		return principalclient.Principal{}, &requestCredentialError{http.StatusForbidden, "principal_does_not_hold_service_principal_tokens", fmt.Sprintf(
			"principal %s is a %s, and principal-store says only a kind with holds_service_principal_tokens (a service) may hold a service principal token", principalID, principal.Kind)}
	}
	return principal, nil
}

// administratorMakingServicePrincipalChange returns the principal a token or
// starter change is recorded as.
func administratorMakingServicePrincipalChange(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID, identified := principalIdentityOfRequest(r)
	if !identified {
		writeJSONError(w, http.StatusForbidden, "service_principal_change_requires_a_principal", fmt.Sprintf(
			"a change to service principal tokens or session starters records which administrator made it; send %s with the service token", principalIdentityHeader))
		return "", false
	}
	return principalID, true
}

func decodeStrictJSONBody(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return false
	}
	return true
}

// issuedServicePrincipalToken is the answer to POST /service-principal-tokens:
// the token, shown this once, and its record.
type issuedServicePrincipalToken struct {
	Token  string                      `json:"token"`
	Record store.ServicePrincipalToken `json:"record"`
}

// handleIssueServicePrincipalToken: POST /service-principal-tokens
// {"principal_id","description"}.
func (s *Server) handleIssueServicePrincipalToken(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingServicePrincipalChange(w, r)
	if !ok {
		return
	}
	var request struct {
		PrincipalID string `json:"principal_id"`
		Description string `json:"description"`
	}
	if !decodeStrictJSONBody(w, r, &request) {
		return
	}
	request.Description = strings.TrimSpace(request.Description)
	if !strings.HasPrefix(request.PrincipalID, "principal_") || request.Description == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"principal_id (a principal-store id, principal_000001) and description (what will hold the token) are both required")
		return
	}
	principal, credentialError := s.principalThatHoldsServicePrincipalTokens(r.Context(), request.PrincipalID)
	if credentialError != nil {
		credentialError.write(w)
		return
	}
	if principal.DisabledAt != 0 {
		writeJSONError(w, http.StatusBadRequest, "principal_disabled", fmt.Sprintf("principal %s is disabled", principal.ID))
		return
	}
	token, err := mintServicePrincipalToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "token_not_minted", err.Error())
		return
	}
	record := store.ServicePrincipalToken{PrincipalID: principal.ID, Description: request.Description, CreatedBy: administratorID}
	if err := s.store.InsertServicePrincipalToken(&record, hashOfServicePrincipalToken(token)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	log.Printf("[service-principal-token] %s issued %s for %s (%s)", administratorID, record.ID, record.PrincipalID, record.Description)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, issuedServicePrincipalToken{Token: token, Record: record})
}

// handleListServicePrincipalTokens: GET /service-principal-tokens.
func (s *Server) handleListServicePrincipalTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.ListServicePrincipalTokens()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, tokens)
}

// handleRevokeServicePrincipalToken: POST /service-principal-tokens/{id}/revoke
// {"reason"}.
func (s *Server) handleRevokeServicePrincipalToken(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingServicePrincipalChange(w, r)
	if !ok {
		return
	}
	var request struct {
		Reason string `json:"reason"`
	}
	if !decodeStrictJSONBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Reason) == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "reason is required: it says why the token stopped working")
		return
	}
	record, err := s.store.RevokeServicePrincipalToken(r.PathValue("id"), administratorID, strings.TrimSpace(request.Reason))
	switch {
	case errors.Is(err, store.ErrServicePrincipalTokenNotFound):
		writeJSONError(w, http.StatusNotFound, "service_principal_token_not_found", "no service principal token has id "+r.PathValue("id"))
		return
	case errors.Is(err, store.ErrServicePrincipalTokenAlreadyRevoked):
		writeJSONError(w, http.StatusConflict, "service_principal_token_already_revoked", fmt.Sprintf("%s was revoked at %s", record.ID, record.RevokedAt.Format(time.RFC3339)))
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	log.Printf("[service-principal-token] %s revoked %s for %s: %s", administratorID, record.ID, record.PrincipalID, record.RevokeReason)
	writeJSON(w, record)
}

// mayStartSessionsAs reports whether the subject holds can_start_sessions_as
// on principalID. Holding none allows nothing, whatever the kind.
func (s *Server) mayStartSessionsAs(ctx context.Context, subject grantSubject, principalID string) (bool, error) {
	granted, err := s.grantedResourcesOf(ctx, subject, startSessionsAsRelation, "principal")
	if err != nil {
		return false, err
	}
	return granted.requiresAGrantAndNames(principalID), nil
}

// callerStartedSessionAndMayDriveIt reports whether the caller started this
// session as its principal and still holds can_start_sessions_as on it.
func (s *Server) callerStartedSessionAndMayDriveIt(ctx context.Context, caller requestCaller, session *store.Session) (bool, error) {
	if session == nil || caller.principalID == "" || session.PrincipalID == "" || caller.peer != nil {
		return false, nil
	}
	starter, found, err := s.store.SessionStarterOf(session.SessionID)
	if err != nil {
		return false, fmt.Errorf("read who started session %s: %w", session.SessionID, err)
	}
	if !found || starter.PrincipalID != caller.principalID {
		return false, nil
	}
	return s.mayStartSessionsAs(ctx, grantSubjectOfCaller(caller), session.PrincipalID)
}

// callerStartedSessionIDAndMayDriveIt is callerStartedSessionAndMayDriveIt
// for a session id; a session this server does not have is false.
func (s *Server) callerStartedSessionIDAndMayDriveIt(ctx context.Context, caller requestCaller, sessionID string) (bool, error) {
	session, err := s.store.GetSession(sessionID)
	if err != nil {
		return false, nil
	}
	return s.callerStartedSessionAndMayDriveIt(ctx, caller, session)
}

// handleRecordSessionStarter: PUT /sessions/{id}/starter {"principal_id"}, for
// a session created before starters were recorded. The starter must hold
// can_start_sessions_as on the session's principal now, or the record would
// give it nothing.
func (s *Server) handleRecordSessionStarter(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingServicePrincipalChange(w, r)
	if !ok {
		return
	}
	var request struct {
		PrincipalID string `json:"principal_id"`
	}
	if !decodeStrictJSONBody(w, r, &request) {
		return
	}
	session, err := s.store.GetSession(r.PathValue("id"))
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if session.PrincipalID == "" || request.PrincipalID == "" || request.PrincipalID == session.PrincipalID {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf(
			"a starter is another principal than the one session %s runs as (%q); principal_id %q is not one", session.SessionID, session.PrincipalID, request.PrincipalID))
		return
	}
	if existing, found, err := s.store.SessionStarterOf(session.SessionID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	} else if found {
		writeJSONError(w, http.StatusConflict, "session_starter_already_recorded", fmt.Sprintf(
			"session %s's starter is already recorded as %s", session.SessionID, existing.PrincipalID))
		return
	}
	allowed, err := s.mayStartSessionsAs(r.Context(), grantSubject{principalID: request.PrincipalID}, session.PrincipalID)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "grant_store_unavailable", err.Error())
		return
	}
	if !allowed {
		writeJSONError(w, http.StatusConflict, "not_granted", fmt.Sprintf(
			"%s holds no %s grant on %s, so recording it as session %s's starter would let it do nothing", request.PrincipalID, startSessionsAsRelation, session.PrincipalID, session.SessionID))
		return
	}
	starter := store.SessionStarter{SessionID: session.SessionID, PrincipalID: request.PrincipalID, RecordedBy: administratorID, Backfilled: true}
	if err := s.store.RecordSessionStarter(starter); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	recorded, _, err := s.store.SessionStarterOf(session.SessionID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	log.Printf("[service-principal-token] %s recorded %s as the starter of session %s (runs as %s)", administratorID, request.PrincipalID, session.SessionID, session.PrincipalID)
	writeJSON(w, recorded)
}

// handleSelf: GET /self. Who the request acts as, so a service learns its own
// principal id from its token rather than from a setting beside it.
func (s *Server) handleSelf(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOfRequest(r)
	if !ok || caller.principalID == "" {
		writeJSON(w, msg.CallerSelf{InternalService: true})
		return
	}
	writeJSON(w, msg.CallerSelf{PrincipalID: caller.principalID, IsAdministrator: caller.isAdministrator})
}
