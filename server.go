package signinstore

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// SessionCookieName is the cookie that carries a session's secret. It is
	// set for the whole domain, HttpOnly, Secure and SameSite=Lax.
	SessionCookieName = "sign_in_session"
	// ServiceTokenHeader carries the service token on the administrator routes.
	ServiceTokenHeader = "X-Sign-In-Store-Service-Token"
	// PrincipalIDHeader names the administrator a service-token call acts for.
	PrincipalIDHeader = "X-Principal-Id"
	// SignedInPrincipalHeader is set on a 200 from GET /session, so a gate
	// such as Caddy's forward_auth can pass who it is on without reading JSON.
	SignedInPrincipalHeader = "X-Signed-In-Principal-Id"

	// linkRequestAnswer is the same whether or not the handle has an account,
	// so the form cannot be used to learn who does.
	linkRequestAnswer = "If that name or number belongs to someone with an account, a sign-in link is on its way to their chat."
	// linkRequestsPerHour bounds link requests per address and per handle.
	linkRequestsPerHour = 5
	defaultListLimit    = 100
	maximumListLimit    = 1000
)

// ServerConfig is what the routes need besides the store and the directory.
type ServerConfig struct {
	ServiceToken  string
	SignInPageURL string
	CookieDomain  string
	Discord       DiscordConfig
}

type server struct {
	store     *Store
	directory *Directory
	config    ServerConfig

	limitMutex   sync.Mutex
	requestTimes map[string][]time.Time
}

// RegisterHandlers mounts every route. It panics on a short service token, at
// boot, because a short token would let a caller guess its way to every
// session.
func RegisterHandlers(mux *http.ServeMux, store *Store, directory *Directory, config ServerConfig) {
	if len(config.ServiceToken) < MinimumServiceTokenLength {
		panic(fmt.Sprintf("sign-in-store: the service token must be at least %d characters", MinimumServiceTokenLength))
	}
	s := &server{store: store, directory: directory, config: config, requestTimes: map[string][]time.Time{}}
	mux.HandleFunc("GET /health", s.health)
	// What a browser reaches, through nginx.
	mux.HandleFunc("POST /link-requests", s.requestLink)
	mux.HandleFunc("POST /redeem", s.redeem)
	mux.HandleFunc("GET /session", s.currentSession)
	mux.HandleFunc("POST /sign-out", s.signOut)
	mux.HandleFunc("GET /discord/start", s.startDiscord)
	mux.HandleFunc("GET /discord/callback", s.finishDiscord)
	// The operator's, through dash with the service token.
	mux.HandleFunc("POST /sign-in-links", s.administrator(s.makeLink))
	mux.HandleFunc("GET /sign-in-links", s.administrator(s.listLinks))
	mux.HandleFunc("GET /sessions", s.administrator(s.listSessions))
	mux.HandleFunc("POST /sessions/{id}/revoke", s.administrator(s.revokeSession))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// decodeBody reads a small JSON body, refusing unknown fields.
func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 8192))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return false
	}
	return true
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// clientAddress is the browser's address: nginx's X-Real-IP when the request
// came from this host, and the connection's address otherwise. A header a
// browser could set itself is never believed.
func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
			return real
		}
	}
	return host
}

// allowRequest limits link requests to linkRequestsPerHour per key.
func (s *server) allowRequest(keys ...string) bool {
	s.limitMutex.Lock()
	defer s.limitMutex.Unlock()
	cutoff := time.Now().Add(-time.Hour)
	for _, key := range keys {
		recent := s.requestTimes[key][:0]
		for _, at := range s.requestTimes[key] {
			if at.After(cutoff) {
				recent = append(recent, at)
			}
		}
		s.requestTimes[key] = recent
		if len(recent) >= linkRequestsPerHour {
			return false
		}
	}
	for _, key := range keys {
		s.requestTimes[key] = append(s.requestTimes[key], time.Now())
	}
	return true
}

// handleKind reads what a person typed: a phone number in international form
// ("+" then digits; spaces, dashes, dots and brackets are ignored), else a
// Discord username. A number without its "+" is not guessed at: people-store
// keeps numbers in international form only.
func handleKind(handle string) (kind, value string) {
	compact := strings.NewReplacer(" ", "", "-", "", ".", "", "(", "", ")", "").Replace(strings.TrimSpace(handle))
	digits := strings.TrimPrefix(compact, "+")
	if strings.HasPrefix(compact, "+") && len(digits) >= 7 && strings.Trim(digits, "0123456789") == "" {
		return SentByPhone, compact
	}
	return SentByDiscord, strings.TrimPrefix(strings.TrimSpace(handle), "@")
}

// chatAccountPrefixesFor says which bridged chat accounts may carry a link
// asked for with a handle of this kind: Discord for a Discord username; SMS,
// WhatsApp or Signal for a phone number.
func chatAccountPrefixesFor(kind string) []string {
	if kind == SentByDiscord {
		return []string{"@discord_"}
	}
	return []string{"@gmessages_", "@whatsapp_", "@signal_"}
}

// POST /link-requests {"handle": "…"}: always the same answer.
func (s *server) requestLink(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Handle string `json:"handle"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Handle) == "" {
		writeError(w, http.StatusBadRequest, "handle_required", `send {"handle": "<Discord username or phone number in international form>"}`)
		return
	}
	kind, value := handleKind(request.Handle)
	if !s.allowRequest("address:"+clientAddress(r), "handle:"+kind+":"+strings.ToLower(value)) {
		writeError(w, http.StatusTooManyRequests, "too_many_requests", "Too many sign-in requests. Try again in an hour.")
		return
	}
	if err := s.sendLinkForHandle(r, kind, value); err != nil {
		// Logged, never shown: the answer must not say whether the handle has an account.
		log.Printf("[link-request] %s %q: %v", kind, value, err)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"message": linkRequestAnswer})
}

func (s *server) sendLinkForHandle(r *http.Request, kind, value string) error {
	person, err := s.directory.PersonOwningHandle(r.Context(), kind, value)
	if err != nil {
		return err
	}
	if person == nil || person.PrincipalID == "" {
		return errors.New("no person with a principal owns this handle")
	}
	principal, err := s.directory.Principal(r.Context(), person.PrincipalID)
	if err != nil {
		return err
	}
	if err := principal.refusalToSignIn(); err != nil {
		return err
	}
	link, secret, err := s.store.MakeSignInLink(principal.ID, kind, "")
	if err != nil {
		return err
	}
	message := "Your sign-in link for kayushkin.com. It works once, within 7 days: " + s.config.SignInPageURL + "#" + secret
	sent, err := s.directory.SendToPersonsChat(r.Context(), person, chatAccountPrefixesFor(kind), message)
	if err != nil {
		return fmt.Errorf("%s made for %s, and sending it failed: %w", link.ID, principal.ID, err)
	}
	if !sent {
		return fmt.Errorf("%s made for %s, and %s has no one-to-one chat on a %s account to send it to", link.ID, principal.ID, person.ID, kind)
	}
	log.Printf("[link-request] sent %s to %s (%s) by %s", link.ID, principal.ID, principal.DisplayName, kind)
	return nil
}

// sessionAnswer is what GET /session and POST /redeem answer: who is signed
// in, and what principal-store says about them now.
type sessionAnswer struct {
	SessionID       string   `json:"session_id"`
	PrincipalID     string   `json:"principal_id"`
	DisplayName     string   `json:"display_name"`
	IsAdministrator bool     `json:"is_administrator"`
	GroupIDs        []string `json:"group_ids"`
	Method          string   `json:"method"`
	ExpiresAt       int64    `json:"expires_at"`
}

func answerFor(session Session, principal Principal) sessionAnswer {
	return sessionAnswer{SessionID: session.ID, PrincipalID: principal.ID, DisplayName: principal.DisplayName, IsAdministrator: principal.IsAdministrator,
		GroupIDs: principal.GroupIDs(), Method: session.Method, ExpiresAt: session.ExpiresAt}
}

func (s *server) setSessionCookie(w http.ResponseWriter, secret string, expiresAt int64) {
	cookie := &http.Cookie{Name: SessionCookieName, Value: secret, Path: "/", Domain: s.config.CookieDomain,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
	if secret == "" {
		cookie.MaxAge = -1
	} else {
		cookie.Expires = time.Unix(expiresAt, 0)
	}
	http.SetCookie(w, cookie)
}

// POST /redeem {"secret": "…"}: spends a link and signs the browser in.
func (s *server) redeem(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Secret string `json:"secret"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if request.Secret == "" {
		writeError(w, http.StatusBadRequest, "secret_required", `send {"secret": "<the part of the link after #>"}`)
		return
	}
	var principal Principal
	session, secret, err := s.store.RedeemSignInLink(request.Secret,
		SessionRequestFacts{UserAgent: r.UserAgent(), RemoteAddress: clientAddress(r)},
		func(principalID string) error {
			var lookupErr error
			principal, lookupErr = s.directory.Principal(r.Context(), principalID)
			if lookupErr != nil {
				return lookupErr
			}
			return principal.refusalToSignIn()
		})
	switch {
	case errors.Is(err, ErrLinkNotUsable):
		writeError(w, http.StatusGone, "link_not_usable", err.Error())
		return
	case errors.Is(err, ErrMayNotSignIn):
		writeError(w, http.StatusForbidden, "may_not_sign_in", "this account cannot sign in any more")
		return
	case err != nil:
		log.Printf("ERROR: [redeem] %v", err)
		writeError(w, http.StatusBadGateway, "could_not_sign_in", err.Error())
		return
	}
	s.setSessionCookie(w, secret, session.ExpiresAt)
	log.Printf("[redeem] %s (%s) signed in as %s through %s", principal.ID, principal.DisplayName, session.ID, session.SignInLinkID)
	writeJSON(w, http.StatusOK, answerFor(session, principal))
}

// liveSessionOf returns the request's session and its principal, or a status
// and code saying why there is none. The principal is read from
// principal-store (cached 30 s), so a disabled person is signed out.
func (s *server) liveSessionOf(r *http.Request) (Session, Principal, int, string) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return Session{}, Principal{}, http.StatusUnauthorized, "not_signed_in"
	}
	session, err := s.store.SessionBySecret(cookie.Value)
	if errors.Is(err, ErrNotFound) {
		return Session{}, Principal{}, http.StatusUnauthorized, "session_unknown"
	}
	if err != nil {
		return Session{}, Principal{}, http.StatusInternalServerError, "store_failed"
	}
	if session.RevokedAt != 0 {
		return Session{}, Principal{}, http.StatusUnauthorized, "session_revoked"
	}
	if s.store.now().Unix() >= session.ExpiresAt {
		return Session{}, Principal{}, http.StatusUnauthorized, "session_expired"
	}
	principal, err := s.directory.Principal(r.Context(), session.PrincipalID)
	if errors.Is(err, ErrNotFound) {
		return Session{}, Principal{}, http.StatusUnauthorized, "principal_unknown"
	}
	if err != nil {
		log.Printf("ERROR: [session] %s: %v", session.ID, err)
		return Session{}, Principal{}, http.StatusBadGateway, "principal_store_unavailable"
	}
	if principal.refusalToSignIn() != nil {
		return Session{}, Principal{}, http.StatusUnauthorized, "principal_may_not_sign_in"
	}
	if err := s.store.TouchSession(session); err != nil {
		log.Printf("ERROR: [session] touch %s: %v", session.ID, err)
	}
	return session, principal, http.StatusOK, ""
}

// GET /session: who the browser's cookie signs in as. Apps and Caddy's
// forward_auth ask it with the browser's cookies.
func (s *server) currentSession(w http.ResponseWriter, r *http.Request) {
	session, principal, status, code := s.liveSessionOf(r)
	if status != http.StatusOK {
		writeError(w, status, code, "no live session")
		return
	}
	w.Header().Set(SignedInPrincipalHeader, principal.ID)
	writeJSON(w, http.StatusOK, answerFor(session, principal))
}

// POST /sign-out: revokes the browser's session and clears the cookie. A
// browser with no live session gets its cookie cleared all the same.
func (s *server) signOut(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(SessionCookieName); err == nil && cookie.Value != "" {
		if session, err := s.store.SessionBySecret(cookie.Value); err == nil && session.RevokedAt == 0 {
			if _, err := s.store.RevokeSession(session.ID, session.PrincipalID, "signed out"); err != nil && !errors.Is(err, ErrAlreadyRevoked) {
				writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
				return
			}
		}
	}
	s.setSessionCookie(w, "", 0)
	writeJSON(w, http.StatusOK, map[string]bool{"signed_out": true})
}

// administrator wraps a route the operator calls through dash: the service
// token, and X-Principal-Id naming an enabled administrator, whose id the
// handler records.
func (s *server) administrator(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		presented := r.Header.Get(ServiceTokenHeader)
		if presented == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(s.config.ServiceToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "service_token_required", "this route takes "+ServiceTokenHeader)
			return
		}
		administratorID := r.Header.Get(PrincipalIDHeader)
		if administratorID == "" {
			writeError(w, http.StatusForbidden, "administrator_required", "send "+PrincipalIDHeader+" naming the administrator this change is recorded as")
			return
		}
		principal, err := s.directory.Principal(r.Context(), administratorID)
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusForbidden, "administrator_required", err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusBadGateway, "principal_store_unavailable", err.Error())
			return
		}
		if !principal.IsAdministrator || principal.DisabledAt != 0 {
			writeError(w, http.StatusForbidden, "administrator_required", administratorID+" is not an enabled administrator")
			return
		}
		next(w, r, administratorID)
	}
}

// POST /sign-in-links {"principal_id": "…"}: the operator makes a link to
// hand over. The answer holds the URL once; only its hash is kept.
func (s *server) makeLink(w http.ResponseWriter, r *http.Request, administratorID string) {
	var request struct {
		PrincipalID string `json:"principal_id"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	principal, err := s.directory.Principal(r.Context(), request.PrincipalID)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusBadRequest, "unknown_principal", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "principal_store_unavailable", err.Error())
		return
	}
	if err := principal.refusalToSignIn(); err != nil {
		writeError(w, http.StatusBadRequest, "may_not_sign_in", err.Error())
		return
	}
	link, secret, err := s.store.MakeSignInLink(principal.ID, SentByOperator, administratorID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	log.Printf("[sign-in-link] %s made %s for %s (%s)", administratorID, link.ID, principal.ID, principal.DisplayName)
	writeJSON(w, http.StatusCreated, map[string]any{"link": link, "url": s.config.SignInPageURL + "#" + secret, "display_name": principal.DisplayName})
}

// listLimit reads ?limit=, 1 to maximumListLimit.
func listLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultListLimit, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > maximumListLimit {
		writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be 1 to %d", maximumListLimit))
		return 0, false
	}
	return limit, true
}

// GET /sign-in-links?principal_id=&limit=
func (s *server) listLinks(w http.ResponseWriter, r *http.Request, _ string) {
	limit, ok := listLimit(w, r)
	if !ok {
		return
	}
	links, err := s.store.ListSignInLinks(r.URL.Query().Get("principal_id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, links)
}

// GET /sessions?principal_id=&limit=
func (s *server) listSessions(w http.ResponseWriter, r *http.Request, _ string) {
	limit, ok := listLimit(w, r)
	if !ok {
		return
	}
	sessions, err := s.store.ListSessions(r.URL.Query().Get("principal_id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}

// POST /sessions/{id}/revoke {"reason": "…"}
func (s *server) revokeSession(w http.ResponseWriter, r *http.Request, administratorID string) {
	var request struct {
		Reason string `json:"reason"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Reason) == "" {
		writeError(w, http.StatusBadRequest, "reason_required", "say why; the reason stays on the row")
		return
	}
	session, err := s.store.RevokeSession(r.PathValue("id"), administratorID, request.Reason)
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "session_not_found", "no session has id "+r.PathValue("id"))
	case errors.Is(err, ErrAlreadyRevoked):
		writeError(w, http.StatusConflict, "session_already_revoked", fmt.Sprintf("%s was revoked by %s: %s", session.ID, session.RevokedBy, session.RevokeReason))
	case err != nil:
		writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
	default:
		writeJSON(w, http.StatusOK, session)
	}
}
