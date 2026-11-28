package login

// Demo login and the identity-carrying store proxies.
//
// ⚠️ Demo login is a stand-in for real login. A caller names a principal-store
// principal and is signed in as it — no password, no second factor — and every
// request it then sends through /issues/ reaches kanban-store carrying that
// principal's id in X-Principal-Id, which kanban-store trusts and filters
// boards by. Entra ID sign-in (entra_sign_in.go) is the real login; a
// deployment that uses it switches demo login off with
// LLMBRIDGE_DEMO_LOGIN_ENABLED=false. While it is on, anyone who can reach
// POST /auth/demo-login can sign in as anybody.
//
// Either way the result is a login session (login_sessions.go): a row and an
// opaque cookie, revocable on the server.
//
// Because kanban-store trusts the header, two things make the proxy safe:
//
//   - The header is set here and nowhere else. Every client-sent
//     X-Principal-Id and X-Kanban-Store-Service-Token is deleted before the
//     request is forwarded, so a client cannot name itself or claim the
//     unrestricted service identity.
//   - kanban-store must not be reachable by users except through this proxy;
//     a user who can reach it directly can send any X-Principal-Id they like.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// IsWellFormedPrincipalID accepts principal-store's id shape (principal_000001):
// the principal_ prefix followed by letters, digits and underscores only.
func IsWellFormedPrincipalID(principalID string) bool {
	const prefix = "principal_"
	if !strings.HasPrefix(principalID, prefix) || len(principalID) == len(prefix) {
		return false
	}
	for _, character := range principalID {
		isLetter := (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z')
		isDigit := character >= '0' && character <= '9'
		if !isLetter && !isDigit && character != '_' {
			return false
		}
	}
	return true
}

type demoLoginRequest struct {
	PrincipalID string `json:"principal_id"`
}

// HandleDemoLogin signs the caller in as a human, active principal that
// principal-store has, unless demo login is switched off.
func (s *Service) HandleDemoLogin(w http.ResponseWriter, r *http.Request) {
	if !s.Cfg.DemoLoginEnabled {
		httpjson.WriteJSONError(w, http.StatusNotFound, "demo_login_disabled",
			"demo login is switched off on this server (LLMBRIDGE_DEMO_LOGIN_ENABLED=false); sign in with GET /auth/entra/start")
		return
	}
	var request demoLoginRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		httpjson.WriteJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("body must be {\"principal_id\":\"principal_000001\"}: %v", err))
		return
	}
	if !IsWellFormedPrincipalID(request.PrincipalID) {
		httpjson.WriteJSONError(w, http.StatusBadRequest, "invalid_principal_id", fmt.Sprintf(
			"principal_id %q is not a principal-store id (principal_000001); a name or email is not accepted, because neither is unique", request.PrincipalID))
		return
	}
	principal, err := s.PrincipalStore().Get(r.Context(), request.PrincipalID, false)
	if err != nil {
		if errors.Is(err, principals.ErrNotFound) {
			httpjson.WriteJSONError(w, http.StatusBadRequest, "unknown_principal", fmt.Sprintf(
				"principal_id %s does not exist in principal-store: %v", request.PrincipalID, err))
			return
		}
		httpjson.WriteJSONError(w, http.StatusBadGateway, "principal_store_unavailable", fmt.Sprintf(
			"could not confirm principal_id %s with principal-store, so no login was issued: %v", request.PrincipalID, err))
		return
	}
	if principal.Kind != "human" {
		httpjson.WriteJSONError(w, http.StatusBadRequest, "principal_not_human", fmt.Sprintf(
			"principal_id %s is a %q principal; only a human principal can log in — a group is a set of people, not someone who acts", request.PrincipalID, principal.Kind))
		return
	}
	if principal.DisabledAt != 0 {
		httpjson.WriteJSONError(w, http.StatusBadRequest, "principal_disabled", fmt.Sprintf(
			"principal_id %s was disabled in principal-store at %s and cannot log in",
			request.PrincipalID, time.Unix(principal.DisabledAt, 0).UTC().Format(time.RFC3339)))
		return
	}

	session, token, err := s.recordLoginSession(r, principal.ID, store.LoginSessionMethodDemo, "")
	if err != nil {
		httpjson.WriteJSONError(w, http.StatusInternalServerError, "login_session_not_recorded", err.Error())
		return
	}
	setLoginSessionCookie(w, r, session, token)
	httpjson.WriteJSON(w, loggedInPrincipalResponseOf(session))
}

// requestArrivedOverHTTPS reports whether the cookie can be marked Secure: the
// request reached this process over TLS, or a TLS-terminating proxy in front
// of it says so. Marking it Secure on a plain-HTTP deployment would make the
// browser drop it and every login silently fail.
func requestArrivedOverHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// RemoveCookieFromRequestHeader rewrites the Cookie header without the named
// cookie, keeping every other cookie as sent. The login cookie is this
// gateway's credential and kanban-store has no use for it.
func RemoveCookieFromRequestHeader(header http.Header, cookieName string) {
	cookieLines := header.Values("Cookie")
	if len(cookieLines) == 0 {
		return
	}
	var keptCookies []string
	for _, line := range cookieLines {
		for _, pair := range strings.Split(line, ";") {
			trimmed := strings.TrimSpace(pair)
			if trimmed == "" {
				continue
			}
			name, _, _ := strings.Cut(trimmed, "=")
			if strings.TrimSpace(name) == cookieName {
				continue
			}
			keptCookies = append(keptCookies, trimmed)
		}
	}
	header.Del("Cookie")
	if len(keptCookies) > 0 {
		header.Set("Cookie", strings.Join(keptCookies, "; "))
	}
}
