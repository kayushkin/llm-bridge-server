package server

// Demo login and the identity-carrying kanban proxy.
//
// This is a stand-in for real login, for a product deployed separately from
// this host with its own frontend. A caller names a principal-store principal
// and is signed in as it — no password, no second factor — and every request it
// then sends through /kanban/ reaches kanban-store carrying that principal's id
// in X-Principal-Id, which kanban-store trusts and filters boards by.
//
// Because kanban-store trusts the header, two things make this safe only as a
// demo, and only behind this proxy:
//
//   - The header is set here and nowhere else. Every client-sent
//     X-Principal-Id and X-Kanban-Store-Service-Token is deleted before the
//     request is forwarded, so a client cannot name itself or claim the
//     unrestricted service identity.
//   - kanban-store must not be reachable by users except through this proxy;
//     a user who can reach it directly can send any X-Principal-Id they like.
//
// All of it is registered only when LLMBRIDGE_DEMO_LOGIN=enabled (see
// config.DemoLoginEnabled); otherwise none of these routes exist.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
)

// demoLoginCookieName is the cookie that carries a signed principal session.
const demoLoginCookieName = "llm_bridge_principal_session"

// demoLoginSessionLifetime is how long a demo login lasts before the caller
// must log in again. There is no refresh.
const demoLoginSessionLifetime = 12 * time.Hour

// principalSessionCookieCodec signs and verifies the demo login cookie.
//
// The cookie value is `<principal id>.<expiry epoch seconds>.<signature>`,
// where the signature is base64url(HMAC-SHA256(key, "<principal id>.<expiry>")).
// A principal id is refused at login unless it is made only of letters, digits
// and underscores, so the "." separators are unambiguous.
type principalSessionCookieCodec struct {
	signingKey []byte
	now        func() time.Time
}

// principalSession is what a verified cookie says: who, and until when.
type principalSession struct {
	PrincipalID string
	ExpiresAt   time.Time
}

func (codec *principalSessionCookieCodec) signature(signedPortion string) string {
	mac := hmac.New(sha256.New, codec.signingKey)
	mac.Write([]byte(signedPortion))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (codec *principalSessionCookieCodec) encode(session principalSession) string {
	signedPortion := session.PrincipalID + "." + strconv.FormatInt(session.ExpiresAt.Unix(), 10)
	return signedPortion + "." + codec.signature(signedPortion)
}

// decode verifies a cookie value. Every failure — wrong shape, bad signature,
// expired — is an error; there is no partially-trusted result.
func (codec *principalSessionCookieCodec) decode(cookieValue string) (principalSession, error) {
	parts := strings.Split(cookieValue, ".")
	if len(parts) != 3 {
		return principalSession{}, errors.New("malformed session cookie")
	}
	principalID, expiryText, presentedSignature := parts[0], parts[1], parts[2]
	expectedSignature := codec.signature(principalID + "." + expiryText)
	if !hmac.Equal([]byte(presentedSignature), []byte(expectedSignature)) {
		return principalSession{}, errors.New("session cookie signature does not verify")
	}
	if !isWellFormedPrincipalID(principalID) {
		return principalSession{}, errors.New("session cookie names a malformed principal id")
	}
	expirySeconds, err := strconv.ParseInt(expiryText, 10, 64)
	if err != nil {
		return principalSession{}, errors.New("session cookie expiry is not a number")
	}
	expiresAt := time.Unix(expirySeconds, 0).UTC()
	if !codec.now().Before(expiresAt) {
		return principalSession{}, errors.New("session cookie has expired")
	}
	return principalSession{PrincipalID: principalID, ExpiresAt: expiresAt}, nil
}

// isWellFormedPrincipalID accepts principal-store's id shape (principal_000001):
// the principal_ prefix followed by letters, digits and underscores only.
func isWellFormedPrincipalID(principalID string) bool {
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

// registerDemoLoginRoutes mounts the demo login routes and the kanban-store
// and grant-store proxies.
// Called from routes() only when demo login is enabled.
func (s *Server) registerDemoLoginRoutes() {
	s.mux.HandleFunc("POST /auth/demo-login", s.handleDemoLogin)
	s.mux.HandleFunc("GET /auth/principal", s.handleGetLoggedInPrincipal)
	s.mux.HandleFunc("POST /auth/logout", s.handleDemoLogout)
	s.mux.HandleFunc(kanbanProxyMountPrefix+"/", s.handleKanbanStoreProxyAsPrincipal)
	s.mux.HandleFunc(grantStoreProxyMountPrefix+"/", s.handleGrantStoreProxyAsPrincipal)
}

type demoLoginRequest struct {
	PrincipalID string `json:"principal_id"`
}

type loggedInPrincipalResponse struct {
	PrincipalID string    `json:"principal_id"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// handleDemoLogin signs the caller in as a human, active principal that
// principal-store has.
func (s *Server) handleDemoLogin(w http.ResponseWriter, r *http.Request) {
	var request demoLoginRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("body must be {\"principal_id\":\"principal_000001\"}: %v", err))
		return
	}
	if !isWellFormedPrincipalID(request.PrincipalID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_principal_id", fmt.Sprintf(
			"principal_id %q is not a principal-store id (principal_000001); a name or email is not accepted, because neither is unique", request.PrincipalID))
		return
	}
	principal, err := s.principalClient.Get(r.Context(), request.PrincipalID)
	if err != nil {
		if errors.Is(err, principalclient.ErrNotFound) {
			writeJSONError(w, http.StatusBadRequest, "unknown_principal", fmt.Sprintf(
				"principal_id %s does not exist in principal-store: %v", request.PrincipalID, err))
			return
		}
		writeJSONError(w, http.StatusBadGateway, "principal_store_unavailable", fmt.Sprintf(
			"could not confirm principal_id %s with principal-store, so no login was issued: %v", request.PrincipalID, err))
		return
	}
	if principal.Kind != "human" {
		writeJSONError(w, http.StatusBadRequest, "principal_not_human", fmt.Sprintf(
			"principal_id %s is a %q principal; only a human principal can log in — a group is a set of people, not someone who acts", request.PrincipalID, principal.Kind))
		return
	}
	if principal.DisabledAt != 0 {
		writeJSONError(w, http.StatusBadRequest, "principal_disabled", fmt.Sprintf(
			"principal_id %s was disabled in principal-store at %s and cannot log in",
			request.PrincipalID, time.Unix(principal.DisabledAt, 0).UTC().Format(time.RFC3339)))
		return
	}

	session := principalSession{
		PrincipalID: principal.ID,
		ExpiresAt:   s.principalSessionCookieCodec.now().Add(demoLoginSessionLifetime).UTC().Truncate(time.Second),
	}
	http.SetCookie(w, &http.Cookie{
		Name:     demoLoginCookieName,
		Value:    s.principalSessionCookieCodec.encode(session),
		Path:     "/",
		Expires:  session.ExpiresAt,
		MaxAge:   int(demoLoginSessionLifetime / time.Second),
		HttpOnly: true,
		Secure:   requestArrivedOverHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, loggedInPrincipalResponse{PrincipalID: session.PrincipalID, ExpiresAt: session.ExpiresAt})
}

// requestArrivedOverHTTPS reports whether the cookie can be marked Secure: the
// request reached this process over TLS, or a TLS-terminating proxy in front
// of it says so. Marking it Secure on a plain-HTTP deployment would make the
// browser drop it and every login silently fail.
func requestArrivedOverHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// verifiedPrincipalSession reads and verifies the login cookie on r.
func (s *Server) verifiedPrincipalSession(r *http.Request) (principalSession, error) {
	cookie, err := r.Cookie(demoLoginCookieName)
	if err != nil {
		return principalSession{}, fmt.Errorf("no %s cookie: log in with POST /auth/demo-login", demoLoginCookieName)
	}
	return s.principalSessionCookieCodec.decode(cookie.Value)
}

// handleGetLoggedInPrincipal answers who the cookie says the caller is.
func (s *Server) handleGetLoggedInPrincipal(w http.ResponseWriter, r *http.Request) {
	session, err := s.verifiedPrincipalSession(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "not_logged_in", err.Error())
		return
	}
	writeJSON(w, loggedInPrincipalResponse{PrincipalID: session.PrincipalID, ExpiresAt: session.ExpiresAt})
}

// handleDemoLogout clears the login cookie. The cookie is stateless, so a copy
// captured before logout stays valid until it expires; this is a demo.
func (s *Server) handleDemoLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     demoLoginCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestArrivedOverHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// removeCookieFromRequestHeader rewrites the Cookie header without the named
// cookie, keeping every other cookie as sent. The login cookie is this
// gateway's credential and kanban-store has no use for it.
func removeCookieFromRequestHeader(header http.Header, cookieName string) {
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
