package signinstore

import (
	"context"
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

// HandlersConfig is what the routes need besides the store and the directory.
type HandlersConfig struct {
	// SignInPageURL is the page a link opens: a link is this URL, "#" and the
	// link's secret.
	SignInPageURL string
	// CookieDomain is the domain the session cookie is set for, with every
	// name under it.
	CookieDomain string
	Discord      DiscordConfig
}

// Handlers answers sign-in-store's routes. The server that mounts them
// decides who reaches each one; the browser routes check their own cookie or
// secret, and the administrator routes read the administrator from the
// request context (ContextWithAdministrator), never from a header.
type Handlers struct {
	store     *Store
	directory *Directory
	config    HandlersConfig

	limitMutex   sync.Mutex
	requestTimes map[string][]time.Time
}

// NewHandlers returns the routes over store and directory.
func NewHandlers(store *Store, directory *Directory, config HandlersConfig) *Handlers {
	return &Handlers{store: store, directory: directory, config: config, requestTimes: map[string][]time.Time{}}
}

type administratorContextKey struct{}

// ContextWithAdministrator says which principal an administrator route acts
// for. The server that mounts the routes sets it from a credential it has
// verified; the route checks once more with principal-store that the
// principal is an enabled administrator, and records the id on what it does.
func ContextWithAdministrator(ctx context.Context, principalID string) context.Context {
	return context.WithValue(ctx, administratorContextKey{}, principalID)
}

func administratorOf(ctx context.Context) string {
	principalID, _ := ctx.Value(administratorContextKey{}).(string)
	return principalID
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

// Health is GET /health.
func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
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
func (h *Handlers) allowRequest(keys ...string) bool {
	h.limitMutex.Lock()
	defer h.limitMutex.Unlock()
	cutoff := time.Now().Add(-time.Hour)
	for _, key := range keys {
		recent := h.requestTimes[key][:0]
		for _, at := range h.requestTimes[key] {
			if at.After(cutoff) {
				recent = append(recent, at)
			}
		}
		h.requestTimes[key] = recent
		if len(recent) >= linkRequestsPerHour {
			return false
		}
	}
	for _, key := range keys {
		h.requestTimes[key] = append(h.requestTimes[key], time.Now())
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

// RequestLink is POST /link-requests {"handle": "…"}: always the same answer.
func (h *Handlers) RequestLink(w http.ResponseWriter, r *http.Request) {
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
	if !h.allowRequest("address:"+clientAddress(r), "handle:"+kind+":"+strings.ToLower(value)) {
		writeError(w, http.StatusTooManyRequests, "too_many_requests", "Too many sign-in requests. Try again in an hour.")
		return
	}
	if err := h.sendLinkForHandle(r, kind, value); err != nil {
		// Logged, never shown: the answer must not say whether the handle has an account.
		log.Printf("[link-request] %s %q: %v", kind, value, err)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"message": linkRequestAnswer})
}

func (h *Handlers) sendLinkForHandle(r *http.Request, kind, value string) error {
	person, err := h.directory.PersonOwningHandle(r.Context(), kind, value)
	if err != nil {
		return err
	}
	if person == nil || person.PrincipalID == "" {
		return errors.New("no person with a principal owns this handle")
	}
	principal, err := h.directory.Principal(r.Context(), person.PrincipalID)
	if err != nil {
		return err
	}
	if err := principal.refusalToSignIn(); err != nil {
		return err
	}
	link, secret, err := h.store.MakeSignInLink(r.Context(), principal.ID, kind, "")
	if err != nil {
		return err
	}
	message := "Your sign-in link. It works once, within 7 days: " + h.config.SignInPageURL + "#" + secret
	sent, err := h.directory.SendToPersonsChat(r.Context(), person, chatAccountPrefixesFor(kind), message)
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

func (h *Handlers) setSessionCookie(w http.ResponseWriter, secret string, expiresAt int64) {
	cookie := &http.Cookie{Name: SessionCookieName, Value: secret, Path: "/", Domain: h.config.CookieDomain,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
	if secret == "" {
		cookie.MaxAge = -1
	} else {
		cookie.Expires = time.Unix(expiresAt, 0)
	}
	http.SetCookie(w, cookie)
}

// Redeem is POST /redeem {"secret": "…"}: spends a link and signs the browser in.
func (h *Handlers) Redeem(w http.ResponseWriter, r *http.Request) {
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
	session, secret, err := h.store.RedeemSignInLink(r.Context(), request.Secret,
		SessionRequestFacts{UserAgent: r.UserAgent(), RemoteAddress: clientAddress(r)},
		func(principalID string) error {
			var lookupErr error
			principal, lookupErr = h.directory.Principal(r.Context(), principalID)
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
	h.setSessionCookie(w, secret, session.ExpiresAt)
	log.Printf("[redeem] %s (%s) signed in as %s through %s", principal.ID, principal.DisplayName, session.ID, session.SignInLinkID)
	writeJSON(w, http.StatusOK, answerFor(session, principal))
}

// liveSessionOf returns the request's session and its principal, or a status
// and code saying why there is none. The principal is read from
// principal-store (cached 30 s), so a disabled person is signed out.
func (h *Handlers) liveSessionOf(r *http.Request) (Session, Principal, int, string) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return Session{}, Principal{}, http.StatusUnauthorized, "not_signed_in"
	}
	session, err := h.store.SessionBySecret(r.Context(), cookie.Value)
	if errors.Is(err, ErrNotFound) {
		return Session{}, Principal{}, http.StatusUnauthorized, "session_unknown"
	}
	if err != nil {
		return Session{}, Principal{}, http.StatusInternalServerError, "store_failed"
	}
	if session.RevokedAt != 0 {
		return Session{}, Principal{}, http.StatusUnauthorized, "session_revoked"
	}
	if h.store.now().Unix() >= session.ExpiresAt {
		return Session{}, Principal{}, http.StatusUnauthorized, "session_expired"
	}
	principal, err := h.directory.Principal(r.Context(), session.PrincipalID)
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
	if err := h.store.TouchSession(r.Context(), session); err != nil {
		log.Printf("ERROR: [session] touch %s: %v", session.ID, err)
	}
	return session, principal, http.StatusOK, ""
}

// CurrentSession is GET /session: who the browser's cookie signs in as. Apps and Caddy's
// forward_auth ask it with the browser's cookies.
func (h *Handlers) CurrentSession(w http.ResponseWriter, r *http.Request) {
	session, principal, status, code := h.liveSessionOf(r)
	if status != http.StatusOK {
		writeError(w, status, code, "no live session")
		return
	}
	w.Header().Set(SignedInPrincipalHeader, principal.ID)
	writeJSON(w, http.StatusOK, answerFor(session, principal))
}

// SignOut is POST /sign-out: revokes the browser's session and clears the cookie. A
// browser with no live session gets its cookie cleared all the same.
func (h *Handlers) SignOut(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(SessionCookieName); err == nil && cookie.Value != "" {
		if session, err := h.store.SessionBySecret(r.Context(), cookie.Value); err == nil && session.RevokedAt == 0 {
			if _, err := h.store.RevokeSession(r.Context(), session.ID, session.PrincipalID, "signed out"); err != nil && !errors.Is(err, ErrAlreadyRevoked) {
				writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
				return
			}
		}
	}
	h.setSessionCookie(w, "", 0)
	writeJSON(w, http.StatusOK, map[string]bool{"signed_out": true})
}

// asAdministrator runs next for the administrator the request context names,
// after checking with principal-store that it is an enabled administrator.
// No administrator in the context is 403: the change has nobody to be
// recorded as.
func (h *Handlers) asAdministrator(w http.ResponseWriter, r *http.Request, next func(http.ResponseWriter, *http.Request, string)) {
	administratorID := administratorOf(r.Context())
	if administratorID == "" {
		writeError(w, http.StatusForbidden, "administrator_required", "this route acts for an administrator, and the request names none")
		return
	}
	principal, err := h.directory.Principal(r.Context(), administratorID)
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

// MakeLink is POST /sign-in-links, an administrator route.
func (h *Handlers) MakeLink(w http.ResponseWriter, r *http.Request) {
	h.asAdministrator(w, r, h.makeLink)
}

// ListLinks is GET /sign-in-links, an administrator route.
func (h *Handlers) ListLinks(w http.ResponseWriter, r *http.Request) {
	h.asAdministrator(w, r, h.listLinks)
}

// ListSessions is GET /sessions, an administrator route.
func (h *Handlers) ListSessions(w http.ResponseWriter, r *http.Request) {
	h.asAdministrator(w, r, h.listSessions)
}

// RevokeSession is POST /sessions/{id}/revoke, an administrator route.
func (h *Handlers) RevokeSession(w http.ResponseWriter, r *http.Request) {
	h.asAdministrator(w, r, h.revokeSession)
}

// SendDiscordMessage is POST /principals/{id}/discord-messages, an
// administrator route.
func (h *Handlers) SendDiscordMessage(w http.ResponseWriter, r *http.Request) {
	h.asAdministrator(w, r, h.sendDiscordMessage)
}

// POST /sign-in-links {"principal_id": "…"}: the operator makes a link to
// hand over. The answer holds the URL once; only its hash is kept.
// POST /principals/{id}/discord-messages {"body": "…"}: send a message to the
// one-to-one Discord chat of the person pointing at this principal, through
// multichat — the same way a sign-in link goes out, so from the operator's own
// Discord account. A site uses it to pass on something one member sent
// another; the body must say who it is from. 404 when no person points at the
// principal, 409 when they have no one-to-one Discord chat here.
func (h *Handlers) sendDiscordMessage(w http.ResponseWriter, r *http.Request, administratorID string) {
	principalID := r.PathValue("id")
	var request struct {
		Body string `json:"body"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Body) == "" {
		writeError(w, http.StatusBadRequest, "body_required", `send {"body": "<the message>"}`)
		return
	}
	if _, err := h.directory.Principal(r.Context(), principalID); errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown_principal", err.Error())
		return
	} else if err != nil {
		writeError(w, http.StatusBadGateway, "principal_store_unavailable", err.Error())
		return
	}
	person, err := h.directory.PersonPointingAtPrincipal(r.Context(), principalID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "people_store_unavailable", err.Error())
		return
	}
	if person == nil {
		writeError(w, http.StatusNotFound, "no_person", "no person in people-store points at "+principalID)
		return
	}
	sent, err := h.directory.SendToPersonsChat(r.Context(), person, chatAccountPrefixesFor(SentByDiscord), request.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "send_failed", err.Error())
		return
	}
	if !sent {
		writeError(w, http.StatusConflict, "no_chat", person.ID+" has no one-to-one Discord chat to send to")
		return
	}
	log.Printf("[discord-message] %s sent a message to %s (%s)", administratorID, principalID, person.ID)
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "person_id": person.ID})
}

func (h *Handlers) makeLink(w http.ResponseWriter, r *http.Request, administratorID string) {
	var request struct {
		PrincipalID string `json:"principal_id"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	principal, err := h.directory.Principal(r.Context(), request.PrincipalID)
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
	link, secret, err := h.store.MakeSignInLink(r.Context(), principal.ID, SentByOperator, administratorID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	log.Printf("[sign-in-link] %s made %s for %s (%s)", administratorID, link.ID, principal.ID, principal.DisplayName)
	writeJSON(w, http.StatusCreated, map[string]any{"link": link, "url": h.config.SignInPageURL + "#" + secret, "display_name": principal.DisplayName})
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
func (h *Handlers) listLinks(w http.ResponseWriter, r *http.Request, _ string) {
	limit, ok := listLimit(w, r)
	if !ok {
		return
	}
	links, err := h.store.ListSignInLinks(r.Context(), r.URL.Query().Get("principal_id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, links)
}

// GET /sessions?principal_id=&limit=
func (h *Handlers) listSessions(w http.ResponseWriter, r *http.Request, _ string) {
	limit, ok := listLimit(w, r)
	if !ok {
		return
	}
	sessions, err := h.store.ListSessions(r.Context(), r.URL.Query().Get("principal_id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}

// POST /sessions/{id}/revoke {"reason": "…"}
func (h *Handlers) revokeSession(w http.ResponseWriter, r *http.Request, administratorID string) {
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
	session, err := h.store.RevokeSession(r.Context(), r.PathValue("id"), administratorID, request.Reason)
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
