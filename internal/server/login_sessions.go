package server

// Login sessions: what the login cookie stands for.
//
// A sign-in — demo login or Entra ID — creates a row in login_sessions and
// sets the llm_bridge_principal_session cookie to an opaque random token. The
// row keeps only the token's SHA-256, so the database cannot be used to sign
// in. Every request carrying the cookie is looked up: an unknown token, a
// revoked session and an expired one are each 401 with their own code. So
// logging out, or an administrator revoking a session, ends it for every copy
// of the cookie at once — which the signed cookie this replaced on 2026-10-19
// could not do.
//
// CSRF: each login session has a random CSRF token, handed to the page in the
// login response and in GET /auth/principal. A request authenticated by the
// cookie with any method but GET, HEAD or OPTIONS must carry it in
// X-CSRF-Token, or it is 403. SameSite=Lax alone lets a same-site page, or an
// older browser, send a cookie-bearing POST the person never meant. Requests
// that carry the service token, a session agent token or a session posting
// token are not authenticated by the cookie and are not checked; neither are
// the sign-in routes, which create the session the token belongs to.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// loginSessionCookieName is the cookie that carries a login session's token.
// The name predates server-side sessions; a cookie from before them is simply
// not found and its holder signs in again.
const loginSessionCookieName = "llm_bridge_principal_session"

// loginSessionLifetime is how long a login lasts before its holder must sign
// in again. There is no refresh.
const loginSessionLifetime = 12 * time.Hour

// loginSessionLastSeenResolution is how stale last_seen_at may get before a
// request rewrites it. One page load is dozens of requests, and each would
// otherwise be a database write.
const loginSessionLastSeenResolution = time.Minute

// csrfTokenHeader carries a login session's CSRF token on an unsafe request.
const csrfTokenHeader = "X-CSRF-Token"

// loginSessionRandomBytes is the size of a login session token and of a CSRF
// token before encoding.
const loginSessionRandomBytes = 32

// randomURLSafeToken returns n bytes from crypto/rand, base64url-encoded. A
// failure of the system's random source is not something to work around.
func randomURLSafeToken(n int) (string, error) {
	buffer := make([]byte, n)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("read crypto/rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// sha256Hex is how a token is kept and looked up: never the token itself.
func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// recordLoginSession creates a login session for principalID and returns it
// with the token its cookie must carry. The caller has already decided the
// principal may sign in, and sets the cookie with setLoginSessionCookie once
// anything else it must record about the sign-in is recorded.
func (s *Server) recordLoginSession(r *http.Request, principalID string, method store.LoginSessionMethod, identityBindingID string) (store.LoginSession, string, error) {
	token, err := randomURLSafeToken(loginSessionRandomBytes)
	if err != nil {
		return store.LoginSession{}, "", fmt.Errorf("make a login session token: %w", err)
	}
	csrfToken, err := randomURLSafeToken(loginSessionRandomBytes)
	if err != nil {
		return store.LoginSession{}, "", fmt.Errorf("make a CSRF token: %w", err)
	}
	now := s.loginClock().UTC().Truncate(time.Second)
	session := store.LoginSession{
		TokenHash:         sha256Hex(token),
		CSRFToken:         csrfToken,
		PrincipalID:       principalID,
		Method:            method,
		IdentityBindingID: identityBindingID,
		CreatedAt:         now,
		ExpiresAt:         now.Add(loginSessionLifetime),
		LastSeenAt:        now,
		UserAgent:         r.UserAgent(),
		RemoteAddress:     r.RemoteAddr,
	}
	if err := s.store.InsertLoginSession(&session); err != nil {
		return store.LoginSession{}, "", err
	}
	return session, token, nil
}

// setLoginSessionCookie hands the browser a login session's token.
func setLoginSessionCookie(w http.ResponseWriter, r *http.Request, session store.LoginSession, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     loginSessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  session.ExpiresAt,
		MaxAge:   int(loginSessionLifetime / time.Second),
		HttpOnly: true,
		Secure:   requestArrivedOverHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// clearLoginSessionCookie tells the browser to drop the login cookie.
func clearLoginSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     loginSessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestArrivedOverHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// liveLoginSessionOfRequest finds the login session the request's cookie
// names and refuses one that is unknown, revoked or expired, each with its own
// code so a page can tell "sign in again" from "you were signed out".
func (s *Server) liveLoginSessionOfRequest(r *http.Request) (store.LoginSession, *requestCredentialError) {
	cookie, err := r.Cookie(loginSessionCookieName)
	if err != nil || cookie.Value == "" {
		return store.LoginSession{}, &requestCredentialError{http.StatusUnauthorized, "not_logged_in", fmt.Sprintf(
			"no %s cookie: sign in with GET /auth/entra/start or POST /auth/demo-login", loginSessionCookieName)}
	}
	session, err := s.store.GetLoginSessionByTokenHash(sha256Hex(cookie.Value))
	if errors.Is(err, store.ErrLoginSessionNotFound) {
		return store.LoginSession{}, &requestCredentialError{http.StatusUnauthorized, "login_session_unknown",
			"the login cookie names no login session this server issued; sign in again"}
	}
	if err != nil {
		return store.LoginSession{}, &requestCredentialError{http.StatusInternalServerError, "login_session_lookup_failed", fmt.Sprintf(
			"could not read the login session: %v", err)}
	}
	now := s.loginClock().UTC()
	if session.RevokedAt != nil {
		return store.LoginSession{}, &requestCredentialError{http.StatusUnauthorized, "login_session_revoked", fmt.Sprintf(
			"login session %s was revoked at %s (%s); sign in again", session.ID, session.RevokedAt.Format(time.RFC3339), session.RevokeReason)}
	}
	if !now.Before(session.ExpiresAt) {
		return store.LoginSession{}, &requestCredentialError{http.StatusUnauthorized, "login_session_expired", fmt.Sprintf(
			"login session %s expired at %s; sign in again", session.ID, session.ExpiresAt.Format(time.RFC3339))}
	}
	if now.Sub(session.LastSeenAt) >= loginSessionLastSeenResolution {
		// last_seen_at is a record for the person reviewing their sessions,
		// not a check, so a failed write is logged and the request goes on.
		if err := s.store.TouchLoginSession(session.ID, now.Truncate(time.Second)); err != nil {
			log.Printf("[login] ERROR: could not record that login session %s was seen: %v", session.ID, err)
		}
	}
	return session, nil
}

// requestMethodIsSafe reports whether a method only reads, by HTTP's own
// definition, and so needs no CSRF token.
func requestMethodIsSafe(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// csrfRefusalOf checks the CSRF token of a request authenticated by session's
// cookie. It returns nil for a safe method or a matching token.
func csrfRefusalOf(r *http.Request, session store.LoginSession) *requestCredentialError {
	if requestMethodIsSafe(r.Method) {
		return nil
	}
	presented := r.Header.Values(csrfTokenHeader)
	if len(presented) == 0 || presented[0] == "" {
		return &requestCredentialError{http.StatusForbidden, "csrf_token_missing", fmt.Sprintf(
			"a %s authenticated by the login cookie must carry the session's CSRF token in %s; the login response and GET /auth/principal give it",
			r.Method, csrfTokenHeader)}
	}
	if len(presented) != 1 || subtle.ConstantTimeCompare([]byte(presented[0]), []byte(session.CSRFToken)) != 1 {
		return &requestCredentialError{http.StatusForbidden, "csrf_token_mismatch", fmt.Sprintf(
			"%s does not match this login session's CSRF token", csrfTokenHeader)}
	}
	return nil
}

// loggedInPrincipalResponse is what the sign-in routes and GET /auth/principal
// answer: who, until when, and the CSRF token the page must send back.
type loggedInPrincipalResponse struct {
	PrincipalID    string                   `json:"principal_id"`
	ExpiresAt      time.Time                `json:"expires_at"`
	LoginSessionID string                   `json:"login_session_id"`
	Method         store.LoginSessionMethod `json:"method"`
	CSRFToken      string                   `json:"csrf_token"`
}

func loggedInPrincipalResponseOf(session store.LoginSession) loggedInPrincipalResponse {
	return loggedInPrincipalResponse{
		PrincipalID:    session.PrincipalID,
		ExpiresAt:      session.ExpiresAt,
		LoginSessionID: session.ID,
		Method:         session.Method,
		CSRFToken:      session.CSRFToken,
	}
}

// handleGetLoggedInPrincipal answers who the cookie says the caller is, and
// the CSRF token a page that reloaded needs.
func (s *Server) handleGetLoggedInPrincipal(w http.ResponseWriter, r *http.Request) {
	session, refusal := s.liveLoginSessionOfRequest(r)
	if refusal != nil {
		refusal.write(w)
		return
	}
	writeJSON(w, loggedInPrincipalResponseOf(session))
}

// handleLogout revokes the caller's login session on the server and clears
// the cookie. A cookie that names no live session has nothing to revoke, and
// the cookie is cleared all the same. A live one needs its CSRF token: logout
// changes state, and a page on another site must not be able to end someone's
// session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	session, refusal := s.liveLoginSessionOfRequest(r)
	switch {
	case refusal == nil:
		if csrfRefusal := csrfRefusalOf(r, session); csrfRefusal != nil {
			csrfRefusal.write(w)
			return
		}
		if _, err := s.store.RevokeLoginSession(session.ID, session.PrincipalID, "logout", s.loginClock()); err != nil &&
			!errors.Is(err, store.ErrLoginSessionAlreadyRevoked) {
			writeJSONError(w, http.StatusInternalServerError, "logout_failed", fmt.Sprintf(
				"could not revoke login session %s, so it is still live: %v", session.ID, err))
			return
		}
	case refusal.status == http.StatusUnauthorized:
		// No cookie, or one naming no live session: nothing to revoke.
	default:
		// The session could not be read, so whether it is live is unknown;
		// saying "logged out" would be a guess.
		refusal.write(w)
		return
	}
	clearLoginSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// loginSessionView is a login session as a list shows it: the row without
// its hashes or CSRF token, its state, and whether it is the one the request
// came in on.
type loginSessionView struct {
	store.LoginSession
	// State is live, expired or revoked.
	State   string `json:"state"`
	Current bool   `json:"current"`
}

func (s *Server) loginSessionViewOf(session store.LoginSession, currentLoginSessionID string) loginSessionView {
	state := "live"
	switch {
	case session.RevokedAt != nil:
		state = "revoked"
	case !s.loginClock().Before(session.ExpiresAt):
		state = "expired"
	}
	return loginSessionView{LoginSession: session, State: state, Current: session.ID == currentLoginSessionID}
}

// handleListLoginSessions lists the caller's own login sessions, newest
// first. An administrator, or the service token, may name another principal
// with ?principal_id=; the service token acting as itself must, since it is
// nobody.
func (s *Server) handleListLoginSessions(w http.ResponseWriter, r *http.Request) {
	caller, callerIsPrincipal := callerOfRequest(r)
	mayReadAnyone := !callerIsPrincipal || caller.isAdministrator
	principalID := r.URL.Query().Get("principal_id")
	switch {
	case principalID == "" && !callerIsPrincipal:
		writeJSONError(w, http.StatusBadRequest, "principal_id_required",
			"the service token acting as itself has no login sessions of its own; name whose to list with ?principal_id=")
		return
	case principalID == "":
		principalID = caller.principalID
	case principalID != caller.principalID && !mayReadAnyone:
		writeJSONError(w, http.StatusForbidden, "not_administrator", fmt.Sprintf(
			"only an administrator may list another principal's login sessions; %s is not one", caller.principalID))
		return
	}
	sessions, err := s.store.ListLoginSessionsOfPrincipal(principalID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "login_sessions_unreadable", err.Error())
		return
	}
	views := make([]loginSessionView, 0, len(sessions))
	for _, session := range sessions {
		views = append(views, s.loginSessionViewOf(session, caller.loginSessionID))
	}
	writeJSON(w, views)
}

type revokeLoginSessionRequest struct {
	Reason string `json:"reason"`
}

// handleRevokeLoginSession ends one login session: the caller's own, or
// anyone's for an administrator. Someone else's session answers 404 to a
// principal who is not an administrator, the same as a missing one. The
// revocation is stamped with who did it, so the service token must act as a
// principal to revoke anything.
func (s *Server) handleRevokeLoginSession(w http.ResponseWriter, r *http.Request) {
	caller, callerIsPrincipal := callerOfRequest(r)
	if !callerIsPrincipal {
		writeJSONError(w, http.StatusForbidden, "revoke_requires_a_principal", fmt.Sprintf(
			"a revocation records who made it; send %s with the service token", principalIdentityHeader))
		return
	}
	var request revokeLoginSessionRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("body must be {\"reason\":\"…\"}: %v", err))
		return
	}
	if request.Reason == "" {
		writeJSONError(w, http.StatusBadRequest, "reason_required", "say why the login session is being revoked; the reason is kept on the record")
		return
	}
	loginSessionID := r.PathValue("id")
	session, err := s.store.GetLoginSession(loginSessionID)
	if errors.Is(err, store.ErrLoginSessionNotFound) || (err == nil && session.PrincipalID != caller.principalID && !caller.isAdministrator) {
		writeJSONError(w, http.StatusNotFound, "login_session_not_found", fmt.Sprintf("no login session %s", loginSessionID))
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "login_session_unreadable", err.Error())
		return
	}
	revoked, err := s.store.RevokeLoginSession(session.ID, caller.principalID, request.Reason, s.loginClock())
	if errors.Is(err, store.ErrLoginSessionAlreadyRevoked) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":         map[string]string{"code": "login_session_already_revoked", "message": fmt.Sprintf("login session %s was already revoked", session.ID)},
			"login_session": s.loginSessionViewOf(revoked, caller.loginSessionID),
		})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "login_session_not_revoked", err.Error())
		return
	}
	writeJSON(w, s.loginSessionViewOf(revoked, caller.loginSessionID))
}
